import contextlib
import gzip
import http.server
import importlib.util
import io
import json
import pathlib
import shutil
import socketserver
import tempfile
import threading
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('maintain', pathlib.Path(__file__).with_name('maintain.py'))
manage = importlib.util.module_from_spec(spec)
spec.loader.exec_module(manage)


class MaintenanceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        (self.root / 'runs').mkdir()
        self.run = self.root / 'runs' / '001'
        self.run.mkdir()
        (self.root / 'current-run').write_text(str(self.run))

    def quiet(self, function, *args):
        with contextlib.redirect_stdout(io.StringIO()):
            function(*args)

    def test_checkpoint_survives_missing_or_invalid_next_snapshot(self):
        content = json.dumps({'version': 1, 'block': {'number': 1, 'hash': '0x' + '01' * 32}, 'pools': [{}]}).encode()
        final = self.run / 'final-pools.json'
        final.write_bytes(content)
        self.quiet(manage.finish, self.root)
        self.assertEqual((self.root / 'resume.json').read_bytes(), content)
        final.write_text('{')
        self.quiet(manage.finish, self.root)
        self.assertEqual((self.root / 'resume.json').read_bytes(), content)
        self.assertIn('checkpoint_error', json.loads((self.run / 'closed.json').read_text()))
        final.unlink()
        self.quiet(manage.finish, self.root)
        self.assertEqual((self.root / 'resume.json').read_bytes(), content)

    def test_archive_skips_active_and_keeps_candidates_and_checkpoint(self):
        replay = (b'{"type":"block"}\n' * 1000) + b'partial-crash-line'
        (self.run / 'replay.jsonl').write_bytes(replay)
        (self.run / 'closed.json').write_text('{"closed_at":0}')
        self.quiet(manage.archive, self.root, 0, 0)
        self.assertTrue((self.run / 'replay.jsonl').exists())
        next_run = self.root / 'runs' / '002'
        next_run.mkdir()
        (self.root / 'current-run').write_text(str(next_run))
        (self.run / 'candidates.jsonl').write_text('candidate')
        self.quiet(manage.archive, self.root, 1 << 20, 0)
        self.assertFalse((self.run / 'replay.jsonl').exists())
        with gzip.open(self.run / 'replay.jsonl.gz', 'rb') as stream:
            self.assertEqual(stream.read(), replay)
        self.assertEqual((self.run / 'replay.jsonl.gz').stat().st_mode & 0o777, 0o600)
        self.quiet(manage.archive, self.root, 0, 0)
        self.assertFalse((self.run / 'replay.jsonl.gz').exists())
        self.assertEqual((self.run / 'candidates.jsonl').read_text(), 'candidate')
        self.assertTrue((self.run / 'replay-pruned.json').exists())
        self.assertTrue((self.run / 'archive.json').exists())

    def test_promotes_supported_snapshot_versions_and_preserves_oracle(self):
        final = self.run / 'final-pools.json'
        for version in (1, 2, 3, 4):
            with self.subTest(version=version):
                snapshot = {'version': version, 'block': {'number': version, 'hash': '0x' + '01' * 32},
                            'pools': [{}]}
                if version == 4:
                    snapshot['pools'][0]['slipstream'] = {
                        'Valid': True, 'Block': version, 'Observations': [{'Timestamp': 100}]}
                content = json.dumps(snapshot).encode()
                final.write_bytes(content)
                self.quiet(manage.finish, self.root)
                self.assertEqual((self.root / 'resume.json').read_bytes(), content)
                self.assertNotIn('checkpoint_error', json.loads((self.run / 'closed.json').read_text()))
        final.write_text(json.dumps({**snapshot, 'version': 5}))
        self.quiet(manage.finish, self.root)
        self.assertEqual((self.root / 'resume.json').read_bytes(), content)
        self.assertIn('checkpoint_error', json.loads((self.run / 'closed.json').read_text()))

    def test_archive_retains_recent_evidence_even_above_budget(self):
        self.quiet(manage.finish, self.root)
        (self.run / 'replay.jsonl').write_text('evidence\n')
        next_run = self.root / 'runs' / '002'
        next_run.mkdir()
        (self.root / 'current-run').write_text(str(next_run))
        self.quiet(manage.archive, self.root, 0, 24)
        self.assertTrue((self.run / 'replay.jsonl.gz').exists())

    def test_corrupted_archive_does_not_replace_original(self):
        self.quiet(manage.finish, self.root)
        source = self.run / 'replay.jsonl'
        source.write_text('evidence\n')
        next_run = self.root / 'runs' / '002'
        next_run.mkdir()
        (self.root / 'current-run').write_text(str(next_run))
        with mock.patch.object(manage.gzip, 'open', return_value=io.BytesIO(b'corrupted')):
            with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
                self.quiet(manage.archive, self.root, 0, 0)
        self.assertEqual(source.read_text(), 'evidence\n')
        self.assertFalse((self.run / 'replay.jsonl.gz').exists())

    def snapshot(self, number):
        return json.dumps({'version': 4, 'block': {'number': number, 'hash': '0x' + '01' * 32}, 'pools': [{}]}).encode()

    def fake_bot(self, status, rotate):
        """Serve POST /rotate on a socket the way the bot does."""
        directory = tempfile.mkdtemp(dir='/tmp')  # sun_path is short on macOS
        self.addCleanup(shutil.rmtree, directory)
        path = pathlib.Path(directory) / 'ops.sock'

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                body = json.dumps(rotate()).encode() if status == 200 else b'refused'
                self.send_response(status)
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        server = socketserver.UnixStreamServer(str(path), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return path

    def test_rotate_closes_the_rotated_run_while_the_next_one_runs(self):
        next_run = self.root / 'runs' / '002'

        def rotate():
            # The bot moves the marker, closes the old files, then publishes the snapshot.
            next_run.mkdir()
            (self.root / 'current-run').write_text(str(next_run) + '\n')
            (self.run / 'final-pools.json').write_bytes(self.snapshot(9))
            return {'closed': str(self.run), 'run': str(next_run), 'run_id': '2-1'}

        (self.root / 'resume.json').write_bytes(self.snapshot(5))
        self.quiet(manage.rotate, self.root, self.fake_bot(200, rotate), 5)
        closed = json.loads((self.run / 'closed.json').read_text())
        self.assertEqual((closed['closed_by'], closed['checkpoint']['number']), ('rotate', 9))
        self.assertEqual((self.root / 'resume.json').read_bytes(), self.snapshot(9))
        self.assertFalse((next_run / 'closed.json').exists())

        # Shutdown later closes the current run as before.
        (next_run / 'final-pools.json').write_bytes(self.snapshot(12))
        self.quiet(manage.finish, self.root)
        self.assertEqual((self.root / 'resume.json').read_bytes(), self.snapshot(12))
        self.assertNotIn('closed_by', json.loads((next_run / 'closed.json').read_text()))

    def test_refused_rotation_closes_nothing(self):
        with self.assertRaisesRegex(RuntimeError, 'refused'):
            self.quiet(manage.rotate, self.root, self.fake_bot(500, None), 5)
        self.assertFalse((self.run / 'closed.json').exists())

    def test_finish_closes_rotated_runs_without_moving_resume_back(self):
        # A rotation whose rotate step never ran, then a newer checkpoint.
        (self.run / 'final-pools.json').write_bytes(self.snapshot(7))
        unfinished = self.root / 'runs' / '002'
        unfinished.mkdir()
        current = self.root / 'runs' / '003'
        current.mkdir()
        (self.root / 'current-run').write_text(str(current))
        (self.root / 'resume.json').write_bytes(self.snapshot(8))
        self.quiet(manage.finish, self.root)
        self.assertEqual((self.root / 'resume.json').read_bytes(), self.snapshot(8))
        self.assertEqual(json.loads((self.run / 'closed.json').read_text())['checkpoint']['number'], 7)
        self.assertNotIn('checkpoint', json.loads((unfinished / 'closed.json').read_text()))
        self.assertTrue((current / 'closed.json').exists())

    def test_relative_run_pointer_and_atomic_replace_failure(self):
        (self.root / 'current-run').write_text('runs/001')
        self.assertEqual(manage.current_run(self.root), self.run.resolve())
        target = self.root / 'resume.json'
        target.write_bytes(b'previous checkpoint')
        with mock.patch.object(pathlib.Path, 'replace', side_effect=OSError('failed rename')):
            with self.assertRaises(OSError):
                manage.atomic_write(target, b'next checkpoint')
        self.assertEqual(target.read_bytes(), b'previous checkpoint')


if __name__ == '__main__':
    unittest.main()
