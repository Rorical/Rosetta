import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch

import audit


class AuditLifecycleTest(unittest.TestCase):
    def test_only_closed_policy_bound_runs_publish_once(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            (root / 'runs').mkdir()
            for name in ('closed', 'active', 'no-policy'):
                run = root / 'runs' / name
                run.mkdir()
                (run / 'closed.json').write_text('{}')
                for file in ('replay.jsonl.gz', 'observations.jsonl', 'candidates.jsonl'):
                    (run / file).write_bytes(b'')
                if name != 'no-policy':
                    (run / 'search.json').write_text('{}')
            (root / 'current-run').write_text(str(root / 'runs' / 'active'))

            def execute(command, **_):
                pathlib.Path(command[command.index('-out') + 1]).write_text('{"type":"summary"}\n')

            with patch('audit.subprocess.run', side_effect=execute) as process:
                self.assertEqual(audit.audit_completed(root, pathlib.Path('/bin/routeaudit')), 1)
                self.assertEqual(audit.audit_completed(root, pathlib.Path('/bin/routeaudit')), 0)
                self.assertEqual(process.call_count, 1)
            self.assertEqual(json.loads((root / 'runs' / 'closed' / 'opportunity-audit.jsonl').read_text()),
                             {'type': 'summary'})
            self.assertFalse((root / 'runs' / 'active' / 'opportunity-audit.jsonl').exists())

    def test_failed_capture_is_not_retried_or_published(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            run = root / 'runs' / 'closed'
            run.mkdir(parents=True)
            for name in ('closed.json', 'search.json', 'replay.jsonl', 'observations.jsonl', 'candidates.jsonl'):
                (run / name).write_text('{}')
            with patch('audit.subprocess.run', side_effect=OSError('failed')) as process:
                audit.audit_completed(root, pathlib.Path('/bin/routeaudit'))
                audit.audit_completed(root, pathlib.Path('/bin/routeaudit'))
                self.assertEqual(process.call_count, 1)
            self.assertTrue((run / 'opportunity-audit-error.json').exists())
            self.assertFalse((run / 'opportunity-audit.jsonl').exists())
            self.assertFalse((run / 'opportunity-audit.jsonl.next').exists())


    def test_execution_journal_is_frozen_without_partial_tail(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            run = root / 'runs' / 'closed'
            run.mkdir(parents=True)
            for name in ('closed.json', 'search.json', 'replay.jsonl', 'observations.jsonl', 'candidates.jsonl'):
                (run / name).write_text('{}')
            journal = root / 'execution.jsonl'
            journal.write_bytes(b'{"type":"session"}\n{"type":"execution"}\n{"type":')

            def execute(command, **_):
                frozen = pathlib.Path(command[command.index('-execution-paper') + 1])
                self.assertEqual(frozen.read_bytes(), b'{"type":"session"}\n{"type":"execution"}\n')
                with journal.open('ab') as stream:
                    stream.write(b'"execution"}\n')
                self.assertEqual(frozen.read_bytes(), b'{"type":"session"}\n{"type":"execution"}\n')
                pathlib.Path(command[command.index('-out') + 1]).write_text('{"type":"summary"}\n')

            with patch('audit.subprocess.run', side_effect=execute):
                self.assertEqual(audit.audit_completed(root, pathlib.Path('/bin/routeaudit'), execution_paper=journal), 1)
            self.assertTrue((run / 'opportunity-audit.jsonl').exists())
            self.assertFalse((run / 'execution-paper-audit.jsonl.next').exists())


if __name__ == '__main__':
    unittest.main()
