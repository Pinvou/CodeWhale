"""The real Windows atomic entry must retain tests and fail on missing receipts."""
import importlib.util
# Load ctypes before the ABI negative control changes struct.calcsize.
import ctypes
from contextlib import redirect_stderr, redirect_stdout
import io
import json
from pathlib import Path
import re
import struct
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / '.github/workflows/pr-check.yml'
SPEC = importlib.util.spec_from_file_location('atomic_resource_ledger', ROOT / 'scripts/ci-resource-ledger.py')
LEDGER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(LEDGER)


def windows_step(name):
    workflow = WORKFLOW.read_text(encoding='utf-8')
    job = re.search(r'^  windows-rust-test:\n(.*?)(?=^  [\w-]+:|\Z)', workflow, re.M | re.S)[1]
    return next(block for block in job.split('\n      - name: ')[1:] if block.splitlines()[0] == name)


def run_body(step):
    body = step.split('        run: |\n', 1)[1]
    lines = []
    for line in body.splitlines():
        if not line.strip():
            lines.append('')
        elif line.startswith('          '):
            lines.append(line[10:])
        else:
            break
    return '\n'.join(lines) + '\n'


class WindowsAtomicResourceTests(unittest.TestCase):
    def inventory_check(self, root):
        body = run_body(windows_step('Confirm Windows atomic resource retirement'))
        code = re.search(r'''"\$python_exe" - [^\n]* <<'PY'\n(.*?)^PY$''', body, re.M | re.S)[1]
        with mock.patch.object(sys, 'argv', ['-', str(root)]):
            exec(compile(code, '<Windows atomic receipt inventory>', 'exec'), {})

    def test_real_atomic_entry_keeps_patched_exe_filters_and_nonzero_execution(self):
        body = run_body(windows_step('Windows 原子替换状态机回归'))
        self.assertIn('set -euo pipefail', body)
        self.assertIn('test_exe="$(cygpath -u "$PINVOU3_TEST_EXE")"', body)
        self.assertIn('test_exe_windows="$(cygpath -w "$test_exe")"', body)
        self.assertIn("'platform::process::tests::'", body)
        self.assertIn("'windows_attachment_runtime_stays_security_gated'", body)
        command = '"$python_exe" ../../scripts/ci-fixture-process.py --receipt "$receipt" -- "$test_exe_windows" "$filter" --test-threads=1 2>&1 | tee "$report"'
        self.assertIn(command, body)
        self.assertLess(body.index("printf 'atomic-%s.json"), body.index(command))
        self.assertIn("grep -qE 'running [1-9][0-9]* tests?' \"$report\"", body)
        self.assertNotRegex(body, re.compile(r'^\s*"\$test_exe" "\$filter"', re.M))
        initialization = run_body(windows_step('Initialize Windows atomic resource ledger'))
        provider = windows_step('Windows atomic fixture Python')
        self.assertNotIn('actions/setup-python', provider)
        self.assertIn("sys.platform != 'win32'", run_body(provider))
        binding = 'python_exe="$(cygpath -u "$PINVOU_WINDOWS_ATOMIC_PYTHON")"'
        retirement = run_body(windows_step('Confirm Windows atomic resource retirement'))
        for consumer in (initialization, body, retirement):
            self.assertIn(binding, consumer)
        self.assertIn('"$python_exe" scripts/ci-resource-ledger.py --init', initialization)
        self.assertIn('"$python_exe" scripts/ci-resource-ledger.py --directory "$PINVOU_CI_RESOURCE_LEDGER"', retirement)

    def selector_code(self):
        body = run_body(windows_step('Windows atomic fixture Python'))
        return body.split('<<\'PY\' >> "$GITHUB_ENV"\n', 1)[1].rsplit('\nPY\n', 1)[0]

    @unittest.skipUnless(sys.platform == 'win32', 'Native interpreter selection is validated on Windows')
    def test_native_selector_exports_the_verified_interpreter(self):
        output = io.StringIO()
        with redirect_stdout(output), redirect_stderr(io.StringIO()):
            exec(compile(self.selector_code(), '<Windows atomic Python selector>', 'exec'), {})
        self.assertEqual(output.getvalue(), 'PINVOU_WINDOWS_ATOMIC_PYTHON=' + sys.executable + '\n')

    def test_selector_rejects_incompatible_identity_before_export(self):
        cases = (
            (sys, 'platform', 'linux'),
            (sys, 'version_info', (3, 11, 99)),
            (struct, 'calcsize', lambda _format: 4),
            (sys, 'executable', 'relative-python.exe'),
        )
        for owner, name, value in cases:
            with self.subTest(field=name), mock.patch.object(sys, 'platform', 'win32'):
                output = io.StringIO()
                with mock.patch.object(owner, name, value), redirect_stdout(output):
                    with self.assertRaises(SystemExit):
                        exec(compile(self.selector_code(), '<Windows atomic Python selector>', 'exec'), {})
                self.assertEqual(output.getvalue(), '')

    def test_actual_inventory_reader_rejects_missing_empty_duplicate_or_unowned_names(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.assertRaisesRegex(SystemExit, 'unavailable'):
                self.inventory_check(root)
            inventory = root / 'atomic-expected.txt'
            for contents in ('', 'atomic-1.json\natomic-1.json\n', '../other.json\n'):
                inventory.write_text(contents, encoding='utf-8')
                with self.subTest(contents=contents), self.assertRaisesRegex(SystemExit, 'invalid or empty'):
                    self.inventory_check(root)
            inventory.write_text('atomic-1.json\n', encoding='utf-8')
            with self.assertRaisesRegex(SystemExit, 'receipt missing'):
                self.inventory_check(root)

    def test_existing_ledger_reader_keeps_unconfirmed_or_dirty_receipts_failed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'atomic-expected.txt').write_text('atomic-1.json\n', encoding='utf-8')
            receipt = root / 'atomic-1.json'
            for state, dirty, clean in [('registered', True, False), ('released', True, False), ('released', False, True)]:
                with self.subTest(state=state, dirty=dirty):
                    receipt.write_text(json.dumps({'schema': 1, 'kind': 'process', 'id': 'synthetic',
                                                   'state': state, 'cleanup_failed': dirty}), encoding='utf-8')
                    self.inventory_check(root)
                    self.assertEqual(LEDGER.inspect([root])['clean'], clean)

    def test_retirement_is_always_mandatory_while_upload_remains_diagnostic(self):
        check = windows_step('Confirm Windows atomic resource retirement')
        self.assertIn('always()', check)
        self.assertIn("matrix.phase == 'regression'", check)
        self.assertNotIn('continue-on-error', check)
        body = run_body(check)
        self.assertIn('"$python_exe" scripts/ci-resource-ledger.py --directory "$PINVOU_CI_RESOURCE_LEDGER"', body)
        self.assertNotIn('|| true', body)
        upload = windows_step('Windows atomic lifecycle receipts')
        self.assertIn('timeout-minutes: 3', upload)
        self.assertIn('continue-on-error: true', upload)
        self.assertIn('/**/*.json', upload)
        self.assertIn('/atomic-expected.txt', upload)
        self.assertIn('retention-days: 7', upload)


if __name__ == '__main__':
    unittest.main()
