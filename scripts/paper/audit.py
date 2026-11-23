"""Audit closed paper captures using their saved search policy and local data."""
import argparse
import json
import os
import pathlib
import subprocess
import time

from maintain import atomic_write, current_run


def capture_file(run, name):
    for suffix in ('.jsonl', '.jsonl.gz'):
        path = run / (name + suffix)
        if path.exists():
            return path
    return None


def freeze_execution_journal(source, target):
    # Bound the read to the initial size. A growing writer cannot keep the audit
    # reading forever, and an unfinished final append is never parsed as JSON.
    with source.open('rb') as incoming, target.open('wb') as outgoing:
        os.chmod(target, 0o600)
        remaining = os.fstat(incoming.fileno()).st_size
        while remaining:
            line = incoming.readline(remaining)
            remaining -= len(line)
            if not line or not line.endswith(b'\n'):
                break
            outgoing.write(line)
        outgoing.flush()
        os.fsync(outgoing.fileno())


def audit_completed(root, binary, inventory=None, max_cases=500, max_runs=1,
                    execution_paper=None):
    active = current_run(root)
    completed = sorted(run for run in (root / 'runs').iterdir()
                       if run.is_dir() and run.resolve() != active
                       and (run / 'closed.json').exists()
                       and (run / 'search.json').exists()
                       and not (run / 'opportunity-audit.jsonl').exists()
                       and not (run / 'opportunity-audit-error.json').exists())
    attempts = 0
    for run in completed:
        replay = capture_file(run, 'replay')
        paper = capture_file(run, 'observations')
        candidates = capture_file(run, 'candidates')
        if replay is None or paper is None or candidates is None:
            continue
        if attempts >= max_runs:
            break
        attempts += 1
        frozen = run / 'execution-paper-audit.jsonl.next'
        temporary = run / 'opportunity-audit.jsonl.next'
        temporary.unlink(missing_ok=True)  # An interrupted unpublished audit.
        command = [str(binary), '-auto', '-capture', str(replay), '-paper', str(paper),
                   '-candidates', str(candidates), '-search-config', str(run / 'search.json'),
                   '-max-cases', str(max_cases), '-out', str(temporary)]
        if inventory is not None:
            command.extend(['-inventory', str(inventory)])
        if (run / 'research.json').exists():
            command.extend(['-research-config', str(run / 'research.json')])
        try:
            if execution_paper is not None:
                freeze_execution_journal(execution_paper, frozen)
                command.extend(['-execution-paper', str(frozen)])
            subprocess.run(command, check=True, timeout=1500, stdout=subprocess.DEVNULL,
                           stderr=subprocess.PIPE)
            # Flush and publish only a complete successful report. The helper
            # synchronizes the containing directory as well as the file.
            atomic_write(run / 'opportunity-audit.jsonl', temporary.read_bytes())
            print(json.dumps({'audited': run.name}))
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired, OSError) as error:
            # Closed inputs do not change; avoid retrying a bad capture every
            # timer tick. Removing this marker explicitly schedules a retry.
            atomic_write(run / 'opportunity-audit-error.json', (json.dumps({
                'failed_at': time.time(), 'error_type': type(error).__name__,
                'exit_code': getattr(error, 'returncode', None)}) + '\n').encode())
            print(json.dumps({'audit_failed': run.name, 'error_type': type(error).__name__}))
        finally:
            temporary.unlink(missing_ok=True)
            frozen.unlink(missing_ok=True)
    return attempts


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('root', type=pathlib.Path)
    parser.add_argument('--routeaudit', required=True, type=pathlib.Path)
    parser.add_argument('--inventory', type=pathlib.Path)
    parser.add_argument('--execution-paper', type=pathlib.Path,
                        help='optional append-only executor JSONL journal; frozen per audit')
    parser.add_argument('--max-cases', type=int, default=500)
    parser.add_argument('--max-runs', type=int, default=1)
    args = parser.parse_args()
    if args.max_cases < 1 or args.max_runs < 1:
        parser.error('case and run limits must be positive')
    audit_completed(args.root, args.routeaudit, args.inventory, args.max_cases, args.max_runs,
                    args.execution_paper)


if __name__ == '__main__':
    main()
