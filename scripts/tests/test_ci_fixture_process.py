"""Real OS process-tree regressions; every child is created by this test."""
import ctypes
from contextlib import contextmanager
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


@contextmanager
def held_process(pid):
    """Keep the observed identity alive; never re-open a possibly reused PID."""
    if sys.platform == 'linux':
        import select
        descriptor = os.pidfd_open(pid)
        try:
            yield lambda: not bool(select.select([descriptor], [], [], 0)[0])
        finally:
            os.close(descriptor)
        return
    from ctypes import wintypes as w
    kernel = ctypes.WinDLL('kernel32', use_last_error=True)
    kernel.OpenProcess.argtypes, kernel.OpenProcess.restype = [w.DWORD, w.BOOL, w.DWORD], w.HANDLE
    kernel.WaitForSingleObject.argtypes, kernel.WaitForSingleObject.restype = [w.HANDLE, w.DWORD], w.DWORD
    kernel.CloseHandle.argtypes = [w.HANDLE]
    handle = kernel.OpenProcess(0x100000, False, pid)
    if not handle:
        raise ctypes.WinError(ctypes.get_last_error())
    try:
        yield lambda: kernel.WaitForSingleObject(handle, 0) == 0x102
    finally:
        kernel.CloseHandle(handle)


@unittest.skipUnless(sys.platform in ('win32', 'linux'), 'process scope supports CI operating systems')
class ProcessScopeTests(unittest.TestCase):
    def test_diagnostic_delivery_failure_cannot_replace_execution_or_cleanup(self):
        with patch.object(WORKSPACE, 'diagnostics', True), patch('builtins.print', side_effect=OSError('closed diagnostics')):
            WORKSPACE.diagnostic_phase('observe_process_tree')

    @unittest.skipUnless(shutil.which('node'), 'Node creates the detached test descendant')
    def test_optional_root_evidence_preserves_original_cancellation_verdict(self):
        # The separate interpreter alone owns the observed command and its
        # descendants. A metadata barrier triggers cancellation only after the
        # real root's nonzero exit has been observed through its held identity.
        source = r'''
import importlib.util,json,os,signal,sys,threading,time
spec=importlib.util.spec_from_file_location('owned',sys.argv[1])
owned=importlib.util.module_from_spec(spec);spec.loader.exec_module(owned)
evidence={};errors=[]
def cancel_after_exit():
    until=time.monotonic()+10
    while 'exit_code' not in evidence:
        if time.monotonic()>=until: errors.append('exit evidence missing');owned.cancelled=signal.SIGTERM;return
        time.sleep(.005)
    owned.cancelled=signal.SIGTERM
watcher=threading.Thread(target=cancel_after_exit);watcher.start()
script="require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{detached:true,stdio:'ignore'}).unref();process.exitCode=23"
run=owned.windows_run if sys.platform=='win32' else owned.linux_run
result=run([sys.argv[2],'-e',script],os.environ.copy(),.2,exit_evidence=evidence)
watcher.join(11)
assert not watcher.is_alive() and not errors,(errors,evidence)
assert result[0]==143 and result[2] is True and isinstance(result[1],bool),result
assert evidence=={'started':True,'exit_code':23},evidence
print('original-cancel-verdict-preserved-with-root23')
'''
        p = subprocess.run([sys.executable, '-X', 'utf8', '-c', source, str(SUPERVISOR), shutil.which('node')],
                           capture_output=True, text=True, timeout=20)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(p.stdout.strip(), 'original-cancel-verdict-preserved-with-root23')

    def test_exit_evidence_rejects_callback_mappings_before_start(self):
        class CallbackMapping(dict):
            def clear(self):
                raise AssertionError('a callback mapping must never execute')
        for run in (WORKSPACE.linux_run, WORKSPACE.windows_run):
            with self.subTest(run=run.__name__), self.assertRaises(TypeError):
                run(['never-launch'], {}, .1, exit_evidence=CallbackMapping())

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
            "import os,pathlib,sys,faulthandler;"
            "faulthandler.dump_traceback_later(3,repeat=False);"
            "root=pathlib.Path(os.environ['PINVOU_CI_FIXTURE_ROOT']);"
            "deep=root.joinpath(*(['deep-segment-0123456789']*12));"
            "raw='\\\\\\\\?\\\\'+str(deep);"
            "print('fixture_child_phase=mkdir',file=sys.stderr,flush=True);"
            "os.makedirs(raw);"
            "print('fixture_child_phase=write',file=sys.stderr,flush=True);"
            "pathlib.Path(raw,'proof.txt').write_text('owned');"
            "print('fixture_child_phase=written',file=sys.stderr,flush=True);"
            "print(len(str(deep)),flush=True);"
            "faulthandler.cancel_dump_traceback_later()"
        )
        # Bounded stress, not retries: every iteration must satisfy the same
        # 15-second limit and confirmed process/workspace retirement.
        for iteration in range(100):
            with tempfile.TemporaryDirectory() as directory:
                p, receipt, _ = self.invoke(source, directory)
                self.assertEqual(p.returncode, 0, p.stderr)
                self.assertGreater(int(p.stdout.strip()), 260)
                self.assertEqual(receipt['state'], 'released')
                self.assertFalse(receipt['cleanup_failed'])
                self.assertFalse(Path(receipt['workspace']['path']).exists())
            if (iteration + 1) % 20 == 0:
                print(f'fixture_stress_completed={iteration + 1}/100', file=sys.stderr, flush=True)

    @unittest.skipUnless(shutil.which('node'), 'Node is required for the actual JavaScript signal hook')
    def test_failed_fixture_signal_keeps_receipt_and_supervisor_retires_owned_descendant(self):
        node = shutil.which('node')
        fixture = (SUPERVISOR.parents[1] / 'pinvou3-app/tests/helpers/fixture-scope.mjs').as_uri()
        script = """
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { existsSync, writeFileSync } from 'node:fs';
const { fixtureScope } = await import(FIXTURE);
fixtureScope.register({kind:'probe',id:'signal-owned',close(){throw new Error('primary fixture close failure')},remove(){throw new Error('unconfirmed resource must not be removed')}});
const child = spawn(process.execPath,['-e',"process.send({pid:process.pid});setInterval(()=>{},1000)"],{detached:true,stdio:['ignore','ignore','ignore','ipc']});
const [ready] = await once(child,'message');
console.log('OWNED_CHILD_PID='+ready.pid);
writeFileSync(READY, String(ready.pid));
child.disconnect(); child.unref();
setInterval(()=>{},1000);
while (!existsSync(RELEASE)) await new Promise(resolve => setTimeout(resolve, 5));
if (process.platform === 'win32') process.emit('SIGTERM');
else process.kill(process.pid,'SIGTERM');
""".replace('FIXTURE', json.dumps(fixture))
        with tempfile.TemporaryDirectory() as directory:
            ready, release = Path(directory) / 'child-ready', Path(directory) / 'child-release'
            script = script.replace('READY', json.dumps(str(ready))).replace('RELEASE', json.dumps(str(release)))
            source = 'import subprocess,sys;sys.exit(subprocess.run(' + repr([node, '--input-type=module', '--eval', script]) + ').returncode)'
            receipt_path = Path(directory) / 'receipt.json'
            p = subprocess.Popen([sys.executable, str(SUPERVISOR), '--receipt', str(receipt_path),
                '--cleanup-seconds', '0.2', '--', sys.executable, '-c', source],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                deadline = time.monotonic() + 15
                while not ready.is_file() and p.poll() is None and time.monotonic() < deadline:
                    time.sleep(.005)
                self.assertTrue(ready.is_file(), 'the owned descendant never reached the identity barrier')
                pid = int(ready.read_text())
                with held_process(pid) as child_alive:
                    self.assertTrue(child_alive())
                    release.touch()
                    stdout, stderr = p.communicate(timeout=max(.01, deadline - time.monotonic()))
                    self.assertEqual(p.returncode, 1, stderr)
                    self.assertFalse(child_alive(), 'the exact held descendant survived cleanup')
            finally:
                release.touch()
                if p.poll() is None:
                    p.terminate()
                p.communicate(timeout=10)
            self.assertIn('OWNED_CHILD_PID='+str(pid), stdout)
            self.assertIn('primary fixture close failure', stderr)
            receipt = json.loads(receipt_path.read_text())
            self.assertTrue(receipt['cleanup_failed'], 'the surviving descendant must require supervisor intervention')
            self.assertEqual(receipt['state'], 'released')
            ledger = receipt_path.parent / (receipt_path.stem + '-resources') / 'signal-owned.json'
            self.assertEqual(json.loads(ledger.read_text())['state'], 'unconfirmed')

    def invoke(self, source, directory, name='receipt'):
        receipt = Path(directory) / (name + '.json')
        try:
            p = subprocess.run([sys.executable, str(SUPERVISOR), '--diagnostics', '--receipt', str(receipt), '--cleanup-seconds', '0.2', '--', sys.executable, '-c', source], capture_output=True, text=True, timeout=15)
        except subprocess.TimeoutExpired as error:
            evidence = error.stderr or b''
            if isinstance(evidence, bytes):
                evidence = evidence.decode('utf-8', errors='replace')
            # TimeoutExpired's normal traceback drops the captured diagnostic
            # stderr. Preserve bounded stage/stack evidence without printing argv.
            raise AssertionError('supervisor exceeded unchanged 15-second limit; stage/stack evidence:\n' + evidence[-16000:]) from None
        return p, json.loads(receipt.read_text()), receipt

    def test_success_preserves_output_and_confirms_tree_exit(self):
        with tempfile.TemporaryDirectory() as directory:
            p, receipt, _ = self.invoke("print('fixture actually executed')", directory)
            self.assertEqual(p.returncode, 0, p.stderr)
            self.assertIn('fixture actually executed', p.stdout)
            self.assertEqual(receipt['state'], 'released')
            self.assertFalse(receipt['cleanup_failed'])

    def test_timeout_keeps_captured_stage_evidence_without_command_arguments(self):
        error = subprocess.TimeoutExpired(['private-command-argument'], 15, stderr=b'fixture_phase=retire_workspace')
        with tempfile.TemporaryDirectory() as directory, patch.object(subprocess, 'run', side_effect=error):
            with self.assertRaises(AssertionError) as caught:
                self.invoke('never runs', directory)
        self.assertIn('fixture_phase=retire_workspace', str(caught.exception))
        self.assertNotIn('private-command-argument', str(caught.exception))
        self.assertTrue(caught.exception.__suppress_context__)

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
            release = Path(directory) / 'other-release'
            # Keep the first scope alive until the ownership assertion, rather
            # than assuming a second Windows process scope starts within 2s.
            source = ("import os,time,pathlib\nprint(os.getpid(),flush=True)\n"
                      f"release=pathlib.Path({str(release)!r})\n"
                      "while not release.exists(): time.sleep(0.025)\n")
            other = subprocess.Popen([sys.executable, str(SUPERVISOR), '--receipt', str(receipt), '--', sys.executable, '-c', source], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                pid = int(other.stdout.readline())
                self.assertTrue(alive(pid))
                p, result, _ = self.invoke("print('independent')", directory, 'separate')
                self.assertEqual(p.returncode, 0, p.stderr)
                self.assertTrue(alive(pid), 'another scope killed the first scope')
                self.assertFalse(result['cleanup_failed'])
                release.touch()
                self.assertEqual(other.wait(timeout=10), 0)
            finally:
                release.touch()
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
