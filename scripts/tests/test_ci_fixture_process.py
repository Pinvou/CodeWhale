"""Real OS process-tree regressions; every child is created by this test."""
import ctypes
import importlib.util
import json
import os
import shutil
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

SUPERVISOR = Path(__file__).resolve().parents[1] / 'ci-fixture-process.py'
spec = importlib.util.spec_from_file_location('fixture_workspace', SUPERVISOR)
WORKSPACE = importlib.util.module_from_spec(spec)
spec.loader.exec_module(WORKSPACE)


def alive(pid):
    if sys.platform == 'linux':
        try:
            return Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()[0] != 'Z'
        except FileNotFoundError:
            return False
    from ctypes import wintypes as w
    kernel = ctypes.WinDLL('kernel32', use_last_error=True)
    kernel.OpenProcess.argtypes, kernel.OpenProcess.restype = [w.DWORD, w.BOOL, w.DWORD], w.HANDLE
    kernel.WaitForSingleObject.argtypes, kernel.WaitForSingleObject.restype = [w.HANDLE, w.DWORD], w.DWORD
    kernel.CloseHandle.argtypes = [w.HANDLE]
    handle = kernel.OpenProcess(0x100000, False, pid)
    if not handle:
        return False
    try:
        return kernel.WaitForSingleObject(handle, 0) == 0x102
    finally:
        kernel.CloseHandle(handle)


@unittest.skipUnless(sys.platform in ('win32', 'linux'), 'process scope supports CI operating systems')
class ProcessScopeTests(unittest.TestCase):
    def test_windows_namespace_preserves_drive_unc_and_extended_paths(self):
        for plain, expected in (
            (r'C:\owned\path', '\\\\?\\C:\\owned\\path'),
            (r'\\server\share\owned', '\\\\?\\UNC\\server\\share\\owned'),
            ('\\\\?\\C:\\owned\\path', '\\\\?\\C:\\owned\\path'),
            ('\\\\?\\UNC\\server\\share\\owned', '\\\\?\\UNC\\server\\share\\owned'),
        ):
            with self.subTest(path=plain):
                self.assertEqual(WORKSPACE.windows_extended_path(plain), expected)
        with self.assertRaises(ValueError):
            WORKSPACE.windows_extended_path('relative')

    def test_workspace_identity_mismatch_never_deletes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)/'owned'; root.mkdir()
            (root/'sentinel').write_text('retained')
            identity = root.stat()
            with patch.object(WORKSPACE.shutil, 'rmtree') as remove:
                with self.assertRaises(RuntimeError):
                    WORKSPACE.remove_owned_workspace(root, (identity.st_dev, identity.st_ino+1))
                remove.assert_not_called()
            self.assertEqual((root/'sentinel').read_text(), 'retained')

    @unittest.skipUnless(sys.platform == 'win32', 'actual Windows deep-path retirement')
    def test_supervisor_retires_deep_owned_workspace_after_process_exit(self):
        source = (
            "import os,pathlib;"
            "root=pathlib.Path(os.environ['PINVOU_CI_FIXTURE_ROOT']);"
            "deep=root.joinpath(*(['deep-segment-0123456789']*12));"
            "raw='\\\\\\\\?\\\\'+str(deep);"
            "os.makedirs(raw);"
            "pathlib.Path(raw,'proof.txt').write_text('owned');"
            "print(len(str(deep)),flush=True)"
        )
        with tempfile.TemporaryDirectory() as directory:
            p, receipt, _ = self.invoke(source, directory)
            self.assertEqual(p.returncode, 0, p.stderr)
            self.assertGreater(int(p.stdout.strip()), 260)
            self.assertEqual(receipt['state'], 'released')
            self.assertFalse(receipt['cleanup_failed'])
            self.assertFalse(Path(receipt['workspace']['path']).exists())

    @unittest.skipUnless(shutil.which('node'), 'Node is required for the actual JavaScript signal hook')
    def test_failed_fixture_signal_keeps_receipt_and_supervisor_retires_owned_descendant(self):
        node = shutil.which('node')
        fixture = (SUPERVISOR.parents[1] / 'pinvou3-app/tests/helpers/fixture-scope.mjs').as_uri()
        script = """
import { spawn } from 'node:child_process';
import { once } from 'node:events';
const { fixtureScope } = await import(FIXTURE);
fixtureScope.register({kind:'probe',id:'signal-owned',close(){throw new Error('primary fixture close failure')},remove(){throw new Error('unconfirmed resource must not be removed')}});
const child = spawn(process.execPath,['-e',"process.send({pid:process.pid});setInterval(()=>{},1000)"],{detached:true,stdio:['ignore','ignore','ignore','ipc']});
const [ready] = await once(child,'message');
console.log('OWNED_CHILD_PID='+ready.pid);
child.disconnect(); child.unref();
setInterval(()=>{},1000);
if (process.platform === 'win32') process.emit('SIGTERM');
else process.kill(process.pid,'SIGTERM');
""".replace('FIXTURE', json.dumps(fixture))
        source = 'import subprocess,sys;sys.exit(subprocess.run(' + repr([node, '--input-type=module', '--eval', script]) + ').returncode)'
        with tempfile.TemporaryDirectory() as directory:
            p, receipt, receipt_path = self.invoke(source, directory)
            self.assertEqual(p.returncode, 1, p.stderr)
            self.assertIn('primary fixture close failure', p.stderr)
            self.assertTrue(receipt['cleanup_failed'], 'the surviving descendant must require supervisor intervention')
            self.assertEqual(receipt['state'], 'released')
            ledger = receipt_path.parent / (receipt_path.stem + '-resources') / 'signal-owned.json'
            self.assertEqual(json.loads(ledger.read_text())['state'], 'unconfirmed')
            pid = int(p.stdout.split('OWNED_CHILD_PID=')[1].splitlines()[0])
            self.assertFalse(alive(pid))

    def invoke(self, source, directory, name='receipt', extra=()):
        receipt = Path(directory) / (name + '.json')
        p = subprocess.run([sys.executable, str(SUPERVISOR), '--receipt', str(receipt), '--cleanup-seconds', '0.2', *extra, '--', sys.executable, '-c', source], capture_output=True, text=True, timeout=15)
        return p, json.loads(receipt.read_text()), receipt

    @unittest.skipUnless(sys.platform == 'win32', 'short Windows staging workspace')
    def test_short_scope_retires_files_even_when_child_skips_finally(self):
        parent = next(p for p in SUPERVISOR.parents if p.parent != p and len(str(p))+10 <= 35 and not any(c.isspace() for c in str(p)))
        source = "import os,pathlib; r=pathlib.Path(os.environ['PINVOU_CI_FIXTURE_ROOT']); assert len(str(r))<=35; (r/'fixture').mkdir(); (r/'fixture'/'proof').write_text('owned'); (r/'w123456.owner.json').write_text('owned clone marker'); os._exit(9)"
        with tempfile.TemporaryDirectory() as directory:
            # Keep an external short scope's registration in the enclosing job
            # ledger if the test process is forcibly cancelled before teardown.
            directory = os.environ.get('PINVOU_CI_RESOURCE_LEDGER', directory)
            p, receipt, _ = self.invoke(source, directory, name='short-'+str(time.monotonic_ns()), extra=('--short-workspace-parent', str(parent)))
            self.assertEqual(p.returncode, 9, p.stderr)
            self.assertEqual(receipt['exit_code'], 9)
            self.assertEqual(receipt['state'], 'released')
            self.assertFalse(receipt['cleanup_failed'])
            self.assertFalse(Path(receipt['workspace']['path']).exists())

    @unittest.skipUnless(sys.platform == 'win32', 'short Windows staging workspace')
    def test_short_scope_refuses_drive_root_before_creating_resources(self):
        with tempfile.TemporaryDirectory() as directory:
            receipt = Path(directory)/'refused.json'
            p = subprocess.run([sys.executable, str(SUPERVISOR), '--receipt', str(receipt), '--short-workspace-parent', Path(directory).anchor, '--', sys.executable, '-c', "raise SystemExit('must not run')"], capture_output=True, text=True, timeout=15)
            self.assertNotEqual(p.returncode, 0)
            self.assertIn('short workspace parent', p.stderr)
            self.assertFalse(receipt.exists())

    @unittest.skipUnless(sys.platform == 'win32', 'short Windows staging workspace')
    def test_short_scope_registration_failure_retires_owned_directory(self):
        parent = next(p for p in SUPERVISOR.parents if p.parent != p and len(str(p))+10 <= 35 and not any(c.isspace() for c in str(p)))
        original = WORKSPACE.atomic_receipt
        def write(path, value):
            if value['state'] == 'registered':
                raise OSError('controlled receipt registration failure')
            original(path, value)
        with tempfile.TemporaryDirectory() as directory:
            receipt = Path(directory)/'registration.json'
            with patch.object(WORKSPACE, 'atomic_receipt', side_effect=write), patch.object(WORKSPACE, 'windows_run') as run, patch.object(WORKSPACE.signal, 'signal'):
                with self.assertRaisesRegex(OSError, 'controlled receipt'):
                    WORKSPACE.main(['--receipt', str(receipt), '--short-workspace-parent', str(parent), '--', sys.executable, '-c', 'pass'])
            run.assert_not_called()
            value = json.loads(receipt.read_text())
            self.assertEqual(value['state'], 'released')
            self.assertTrue(value['cleanup_failed'], 'setup failure must not become a successful test')
            self.assertFalse(Path(value['workspace']['path']).exists())

    def test_success_preserves_output_and_confirms_tree_exit(self):
        with tempfile.TemporaryDirectory() as directory:
            p, receipt, _ = self.invoke("print('fixture actually executed')", directory)
            self.assertEqual(p.returncode, 0, p.stderr)
            self.assertIn('fixture actually executed', p.stdout)
            self.assertEqual(receipt['state'], 'released')
            self.assertFalse(receipt['cleanup_failed'])

    @unittest.skipUnless(sys.platform == 'linux', 'AF_UNIX Chrome path limit is Linux-specific')
    def test_long_runner_temp_cannot_overflow_chrome_singleton_socket(self):
        with tempfile.TemporaryDirectory() as directory:
            long_temp = Path(directory) / ('runner-' + 'x' * 100)
            long_temp.mkdir()
            source = "import os,socket,tempfile,pathlib; root=pathlib.Path(tempfile.mkdtemp(prefix='com.google.Chrome.')); p=root/'SingletonSocket'; s=socket.socket(socket.AF_UNIX); s.bind(str(p)); s.close(); p.unlink(); root.rmdir(); print(os.environ['TMPDIR'])"
            with patch.dict(os.environ, TMPDIR=str(long_temp), TMP=str(long_temp), TEMP=str(long_temp)):
                p, receipt, _ = self.invoke(source, directory)
            self.assertEqual(p.returncode, 0, p.stderr)
            self.assertTrue(p.stdout.strip().startswith('/tmp/pinvou-ci-fixture-'))
            self.assertEqual(receipt['state'], 'released')
            self.assertFalse(receipt['cleanup_failed'])

    def test_business_failure_keeps_original_exit_status(self):
        with tempfile.TemporaryDirectory() as directory:
            p, receipt, _ = self.invoke("raise SystemExit(7)", directory)
            self.assertEqual(p.returncode, 7, p.stderr)
            self.assertEqual(receipt['exit_code'], 7)
            self.assertFalse(receipt['cleanup_failed'])

    def test_detached_grandchild_is_retired_and_zero_exit_cannot_hide_leak(self):
        with tempfile.TemporaryDirectory() as directory:
            source = "import subprocess,sys; p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(20)'],start_new_session=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL);print(p.pid,flush=True)"
            p, receipt, _ = self.invoke(source, directory)
            self.assertEqual(p.returncode, 86, p.stderr)
            self.assertTrue(receipt['cleanup_failed'])
            self.assertEqual(receipt['state'], 'released')
            self.assertFalse(alive(int(p.stdout.strip())))

    def test_two_scopes_do_not_retire_each_others_children(self):
        with tempfile.TemporaryDirectory() as directory:
            receipt = Path(directory) / 'other.json'
            other = subprocess.Popen([sys.executable, str(SUPERVISOR), '--receipt', str(receipt), '--', sys.executable, '-c', "import os,time;print(os.getpid(),flush=True);time.sleep(2)"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                pid = int(other.stdout.readline())
                self.assertTrue(alive(pid))
                p, result, _ = self.invoke("print('independent')", directory, 'separate')
                self.assertEqual(p.returncode, 0, p.stderr)
                self.assertTrue(alive(pid), 'another scope killed the first scope')
                self.assertFalse(result['cleanup_failed'])
                self.assertEqual(other.wait(timeout=10), 0)
            finally:
                if other.poll() is None:
                    other.terminate()
                    other.wait(timeout=10)
                other.stdout.close()
                other.stderr.close()

    def test_controller_termination_cannot_leave_its_child_alive(self):
        with tempfile.TemporaryDirectory() as directory:
            receipt = Path(directory) / 'cancelled.json'
            p = subprocess.Popen([sys.executable, str(SUPERVISOR), '--receipt', str(receipt), '--cleanup-seconds', '0.2', '--', sys.executable, '-c', "import os,time;print(os.getpid(),flush=True);time.sleep(20)"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                pid = int(p.stdout.readline())
                p.terminate()
                p.wait(timeout=10)
                deadline = time.monotonic() + 5
                while alive(pid) and time.monotonic() < deadline:
                    time.sleep(0.025)
                self.assertFalse(alive(pid), 'controller cancellation left a child')
                self.assertNotEqual(p.returncode, 0)
            finally:
                if p.poll() is None:
                    p.kill()
                    p.wait(timeout=10)
                p.stdout.close()
                p.stderr.close()

    @unittest.skipUnless(sys.platform == 'linux', 'Windows containment retires the kernel Job Object on controller death')
    def test_sigkill_receipt_recovery_is_bound_to_scope_uid_cgroup_and_pidfd(self):
        spec = importlib.util.spec_from_file_location('fixture_recover', SUPERVISOR.with_name('ci-fixture-recover.py'))
        recovery = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(recovery)
        with tempfile.TemporaryDirectory() as directory:
            receipt = Path(directory) / 'killed.json'
            p = subprocess.Popen([sys.executable, str(SUPERVISOR), '--receipt', str(receipt), '--', sys.executable, '-c', "import os,time;print(os.getpid(),flush=True);time.sleep(20)"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            value = None
            try:
                pid = int(p.stdout.readline())
                p.kill()
                p.wait(timeout=10)
                value = json.loads(receipt.read_text())
                wrong = {**value, 'scope': {**value['scope'], 'uid': value['scope']['uid'] + 1}}
                with self.assertRaises(ValueError): recovery.recover(wrong, grace=1)
                self.assertTrue(alive(pid), 'mismatched ownership triggered termination')
                result = recovery.recover(value, grace=1)
                self.assertFalse(alive(pid))
                self.assertEqual(result['state'], 'released')
                self.assertTrue(result['cleanup_failed'], 'recovery must not turn the failed run green')
                self.assertFalse(Path(value['workspace']['path']).exists())
            finally:
                if p.poll() is None:
                    p.terminate()
                    p.wait(timeout=10)
                if value:
                    recovery.recover(value, grace=1)
                p.stdout.close()
                p.stderr.close()


if __name__ == '__main__':
    unittest.main()
