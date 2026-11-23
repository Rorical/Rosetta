"""Maintain completed paper runs without touching the active capture."""
import argparse
import contextlib
import datetime
import fcntl
import gzip
import hashlib
import http.client
import json
import os
import pathlib
import shutil
import socket
import time


def atomic_write(path, content):
    temporary = path.with_name(path.name + '.next')
    with temporary.open('wb') as stream:
        os.chmod(temporary, 0o600)
        stream.write(content)
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(path)
    descriptor = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def current_run(root):
    pointer = root / 'current-run'
    if not pointer.exists():
        return None
    run = pathlib.Path(pointer.read_text().strip())
    if not run.is_absolute():
        run = root / run
    run = run.resolve()
    if run.parent != (root / 'runs').resolve():
        raise ValueError('current run is outside the runs directory')
    return run


@contextlib.contextmanager
def locked(root):
    # finish (after the process exits) and rotate (while it runs) both close
    # runs and replace resume.json; one at a time.
    with (root / 'maintain.lock').open('a') as stream:
        fcntl.flock(stream, fcntl.LOCK_EX)
        yield


def resume_block(root):
    try:
        return json.loads((root / 'resume.json').read_bytes())['block']['number']
    except (OSError, ValueError, KeyError, TypeError):
        return 0


def close_run(root, run, marker, keep_newer_resume):
    final = run / 'final-pools.json'
    if final.exists():
        try:
            content = final.read_bytes()
            snapshot = json.loads(content)
            block = snapshot['block']
            if snapshot['version'] not in (1, 2, 3, 4) or not snapshot['pools'] or block['number'] <= 0 or len(block['hash']) != 66:
                raise ValueError('invalid pool snapshot identity')
            # A rotated run closed late must not move the resume point back.
            if not keep_newer_resume or block['number'] > resume_block(root):
                atomic_write(root / 'resume.json', content)
            marker['checkpoint'] = block
            marker['checkpoint_sha256'] = hashlib.sha256(content).hexdigest()
        except (ValueError, KeyError, TypeError) as error:
            marker['checkpoint_error'] = str(error)
    atomic_write(run / 'closed.json', (json.dumps(marker) + '\n').encode())
    print(json.dumps({'finished': run.name, **marker}))


def unclosed_runs(root, active):
    return sorted(run for run in (root / 'runs').iterdir()
                  if run.is_dir() and run.resolve() != active and not (run / 'closed.json').exists())


def finish(root):
    with locked(root):
        run = current_run(root)
        # With the process gone no run has a writer left, including one whose
        # rotation never reached the rotate step.
        for rotated in unclosed_runs(root, run):
            close_run(root, rotated, {'closed_at': time.time(), 'closed_by': 'finish'}, True)
        if run is None:
            return
        marker = {'closed_at': time.time(), 'service_result': os.getenv('SERVICE_RESULT', 'manual'),
                  'exit_code': os.getenv('EXIT_CODE'), 'exit_status': os.getenv('EXIT_STATUS')}
        close_run(root, run, marker, False)


class UnixConnection(http.client.HTTPConnection):
    def __init__(self, path, timeout):
        super().__init__('localhost', timeout=timeout)
        self.socket_path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(str(self.socket_path))


def request_rotation(socket_path, timeout):
    connection = UnixConnection(socket_path, timeout)
    try:
        connection.request('POST', '/rotate')
        response = connection.getresponse()
        body = response.read()
    finally:
        connection.close()
    if response.status != 200:
        raise RuntimeError('rotation failed (%d): %s' % (response.status, body.decode(errors='replace').strip()))
    return json.loads(body)


def rotate(root, socket_path, timeout):
    """Ask the running process to start its next run, then close the old one.

    The process answers once the closed run's files are synced and its
    final-pools.json is in place, which it writes last. Runs without that file
    may still be closing and are left for a later rotate or finish.
    """
    result = request_rotation(socket_path, timeout)
    with locked(root):
        for run in unclosed_runs(root, current_run(root)):
            if (run / 'final-pools.json').exists():
                close_run(root, run, {'closed_at': time.time(), 'closed_by': 'rotate'}, True)
    print(json.dumps({'rotated': pathlib.Path(result['closed']).name, 'run': pathlib.Path(result['run']).name,
                      'run_id': result['run_id']}))


def digest_stream(stream):
    digest = hashlib.sha256()
    for chunk in iter(lambda: stream.read(1 << 20), b''):
        digest.update(chunk)
    return digest.hexdigest()


def archive(root, max_bytes, keep_hours):
    active = current_run(root)
    # Only a stopped process can create closed.json; never rotate an open writer.
    completed = [run for run in (root / 'runs').iterdir()
                 if run.is_dir() and run.resolve() != active and (run / 'closed.json').exists()]
    completed.sort(key=lambda run: json.loads((run / 'closed.json').read_text())['closed_at'])
    for run in completed:
        source = run / 'replay.jsonl'
        if not source.exists():
            continue
        target = run / 'replay.jsonl.gz'
        temporary = run / 'replay.jsonl.gz.next'
        with source.open('rb') as stream:
            original_hash = digest_stream(stream)
        with source.open('rb') as stream, temporary.open('wb') as output:
            os.chmod(temporary, 0o600)
            with gzip.GzipFile(filename='', mode='wb', fileobj=output, compresslevel=1, mtime=0) as compressed:
                shutil.copyfileobj(stream, compressed, 1 << 20)
            output.flush()
            os.fsync(output.fileno())
        with gzip.open(temporary, 'rb') as stream:
            if digest_stream(stream) != original_hash:
                raise ValueError('compressed replay checksum mismatch')
        temporary.replace(target)
        atomic_write(run / 'archive.json', (json.dumps({'sha256': original_hash,
                     'original_bytes': source.stat().st_size, 'compressed_bytes': target.stat().st_size}) + '\n').encode())
        source.unlink()
        print(json.dumps({'archived': run.name, 'bytes': target.stat().st_size}))
    archives = [run / 'replay.jsonl.gz' for run in completed if (run / 'replay.jsonl.gz').exists()]
    total = sum(path.stat().st_size for path in archives)
    for path in archives:
        if total <= max_bytes:
            break
        marker = json.loads((path.parent / 'closed.json').read_text())
        if time.time() - marker['closed_at'] < keep_hours * 3600:
            continue
        size = path.stat().st_size
        atomic_write(path.parent / 'replay-pruned.json', (json.dumps({
            'pruned_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'compressed_bytes': size, 'reason': 'completed replay storage budget'}) + '\n').encode())
        path.unlink()
        total -= size
        print(json.dumps({'pruned_replay': path.parent.name, 'bytes': size}))
    print(json.dumps({'retained_replay_bytes': total, 'budget_bytes': max_bytes}))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=['finish', 'archive', 'rotate'])
    parser.add_argument('root', type=pathlib.Path)
    parser.add_argument('--socket', type=pathlib.Path,
                        help='control socket for rotate; defaults to ROOT/control/ops.sock')
    parser.add_argument('--rotate-timeout', type=float, default=90)
    parser.add_argument('--max-replay-bytes', type=int, default=8 << 30)
    parser.add_argument('--keep-hours', type=float, default=24)
    args = parser.parse_args()
    if args.max_replay_bytes < 0 or args.keep_hours < 0:
        parser.error('storage budget and retention hours must be non-negative')
    if args.action == 'finish':
        finish(args.root)
    elif args.action == 'rotate':
        rotate(args.root, args.socket or args.root / 'control' / 'ops.sock', args.rotate_timeout)
    else:
        archive(args.root, args.max_replay_bytes, args.keep_hours)


if __name__ == '__main__':
    main()
