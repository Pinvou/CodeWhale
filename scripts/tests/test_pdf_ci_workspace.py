"""Execute the workflow's PDF step twice against a persistent runner temp root."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]
FAKE_PYTHON = '''\
import json, os, pathlib, shutil, sys
args = sys.argv[1:]
if args[:2] == ['-m', 'venv']:
    dest = pathlib.Path(args[2]) / 'bin' / 'python'
    dest.parent.mkdir(parents=True)
    shutil.copy2(__file__, dest)
elif args[:2] == ['-m', 'pip']:
    if os.environ.get('FAIL_PIP'):
        sys.exit(19)
elif any(arg.endswith('fetch_pdf_gate_wheels.py') for arg in args):
    pathlib.Path(args[args.index('--output') + 1]).mkdir(exist_ok=False)
elif args[:1] == ['-']:
    launcher = pathlib.Path(args[1])
    launcher.write_text('controlled child launcher')
elif any(arg.endswith('run_pdf_gate.py') for arg in args):
    assert '--strict' in args
    audit = pathlib.Path(os.environ['EVIDENCE_PDF_AUDIT_DIR'])
    child = pathlib.Path(os.environ['EVIDENCE_PDF_TEST_PYTHON'])
    assert audit.is_dir() and child.is_file() and audit.parent == child.parent
    assert os.environ['EVIDENCE_PDF_REQUIRE_SYMLINK'] == '1'
    with open(os.environ['PDF_WORKSPACE_PROOF'], 'a') as output:
        output.write(json.dumps({'root': str(audit.parent)}) + '\\n')
else:
    raise AssertionError('unexpected interpreter invocation')
'''


@unittest.skipUnless(sys.platform == 'linux', 'executes the Linux-only PDF workflow step')
class PDFWorkspaceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='pdf-ci-workspace-')
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.runner = self.base / 'persistent runner temp'
        self.runner.mkdir()
        self.proof = self.base / 'proof.jsonl'
        # The old fixed directories deliberately survive both success and error.
        for name in ['pdf-test-lxml-wheels', 'evidence-pdf-venv']:
            folder = self.runner / name
            folder.mkdir()
            (folder / 'foreign-data').write_text('preserve')
        self.python = self.base / 'fake-python'
        self.python.write_text('#!' + sys.executable + '\n' + FAKE_PYTHON)
        self.python.chmod(0o700)
        workflow = (ROOT / '.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        block = workflow.split('      - name: Real PDF validator regression\n', 1)[1]
        block = block.split('\n      - name:', 1)[0].split('        run: |\n', 1)[1]
        self.script = textwrap.dedent(block).replace(
            '${{ steps.evidence_pdf_python.outputs.python-path }}', str(self.python)
        )

    def run_step(self, fail=False):
        env = dict(os.environ, RUNNER_TEMP=str(self.runner), PDF_WORKSPACE_PROOF=str(self.proof))
        if fail:
            env['FAIL_PIP'] = '1'
        return subprocess.run(['bash', '-c', self.script], cwd=ROOT, env=env,
                              capture_output=True, text=True, timeout=30)

    def assert_preserved(self):
        self.assertEqual(sorted(p.name for p in self.runner.iterdir()),
                         ['evidence-pdf-venv', 'pdf-test-lxml-wheels'])
        for folder in self.runner.iterdir():
            self.assertEqual((folder / 'foreign-data').read_text(), 'preserve')

    def test_successive_jobs_get_new_audit_and_venv_without_removing_old_data(self):
        for _ in range(2):
            result = self.run_step()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_preserved()
        roots = [json.loads(line)['root'] for line in self.proof.read_text().splitlines()]
        self.assertEqual(len(set(roots)), 2)
        self.assertTrue(all(Path(root).parent == self.runner and not Path(root).exists() for root in roots))

    def test_failed_install_removes_only_its_owned_workspace(self):
        result = self.run_step(fail=True)
        self.assertEqual(result.returncode, 19, result.stderr)
        self.assertFalse(self.proof.exists())
        self.assert_preserved()


if __name__ == '__main__':
    unittest.main()
