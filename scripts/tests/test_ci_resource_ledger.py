import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('resource_ledger', ROOT / 'scripts/ci-resource-ledger.py')
ledger = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ledger)


class LedgerTests(unittest.TestCase):
    def test_unconfirmed_missing_or_malformed_is_not_clean(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / 'owned.json'
            for value in [dict(schema=1, kind='database', state='created'), dict(schema=1, kind='process', state='released', cleanup_failed=True), dict(schema=2, kind='database', state='released')]:
                path.write_text(json.dumps(value))
                self.assertFalse(ledger.inspect([root])['clean'])
            path.write_text('{')
            self.assertFalse(ledger.inspect([root])['clean'])
            self.assertFalse(ledger.inspect([root / 'absent'])['clean'])

    def test_all_resource_receipts_are_checked_including_nested_process_scopes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            child = root / 'child'
            child.mkdir()
            (root / 'database.json').write_text(json.dumps(dict(schema=1, kind='database', state='released')))
            (child / 'browser.json').write_text(json.dumps(dict(schema=1, kind='browser', state='released')))
            self.assertEqual(ledger.inspect([root]), dict(schema=1, receipts=2, failures=[], clean=True))


if __name__ == '__main__':
    unittest.main()
