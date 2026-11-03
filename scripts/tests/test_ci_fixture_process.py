"""Real OS process-tree regressions; every child is created by this test."""
import ctypes
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

SUPERVISOR = Path(__file__).resolve().parents[1] / 'ci-fixture-process.py'


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
    def invoke(self, source, directory, name='receipt'):
        receipt = Path(directory) / (name + '.json')
        p = subprocess.run([sys.executable, str(SUPERVISOR), '--receipt', str(receipt), '--cleanup-seconds', '0.2', '--', sys.executable, '-c', source], capture_output=True, text=True, timeout=15)
        return p, json.loads(receipt.read_text()), receipt

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
