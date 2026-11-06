"""Exercise the quota runner's actual subprocess/JSON gate, not the Lua policy."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

SOURCE = Path(__file__).resolve().parents[1] / "account-platform-auth-quota-lua-check.py"
SPEC = importlib.util.spec_from_file_location("quota_lua_check", SOURCE)
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class QuotaLuaRunnerTests(unittest.TestCase):
    def test_group_wait_ignores_zombies_and_unrelated_sessions(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def stat(pid, state, group, session):
                folder = root / str(pid)
                folder.mkdir(exist_ok=True)
                (folder / "stat").write_text(f"{pid} (worker ) name) {state} 1 {group} {session} 0\n")
            stat(1, "Z", 123, 123)
            stat(2, "S", 123, 456)
            stat(3, "S", 456, 123)
            self.assertFalse(CHECK._owned_group_has_live_process(123, root))
            stat(4, "S", 123, 123)
            self.assertTrue(CHECK._owned_group_has_live_process(123, root))
            stat(4, "X", 123, 123)
            self.assertFalse(CHECK._owned_group_has_live_process(123, root))

    def test_live_owned_group_cannot_outwait_the_cleanup_budget(self):
        with patch.object(CHECK, "_owned_group_has_live_process", return_value=True), \
                patch.object(CHECK.time, "monotonic", return_value=11), \
                patch.object(CHECK.time, "sleep") as sleep:
            with self.assertRaisesRegex(ValueError, "cleanup deadline"):
                CHECK._wait_owned_group(123, deadline=10)
            sleep.assert_not_called()

    @unittest.skipUnless(sys.platform.startswith("linux"), "owned process-group regression requires Linux")
    def test_cleanup_waits_for_signalled_descendant_after_parent_exit(self):
        with tempfile.TemporaryDirectory(prefix="quota delayed group ") as directory:
            ready = Path(directory) / "child.ready"
            pid_file = Path(directory) / "child.pid"
            child = "from pathlib import Path; import os,time; Path(" + repr(str(pid_file)) + ").write_text(str(os.getpid())); Path(" + repr(str(ready)) + ").write_text('ready'); time.sleep(30)"
            parent = "import subprocess,sys,time; subprocess.Popen([sys.executable,'-c'," + repr(child) + "]); time.sleep(30)"
            process = subprocess.Popen([sys.executable, "-c", parent], start_new_session=True,
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            kill_group = os.killpg
            timer = None
            try:
                deadline = time.monotonic() + 5
                while not ready.exists():
                    if time.monotonic() >= deadline:
                        self.fail("owned descendant fixture did not become ready")
                    time.sleep(0.01)
                pid = int(pid_file.read_text())

                def finish_group():
                    try:
                        kill_group(process.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass

                def parent_exits_first(group, sig):
                    nonlocal timer
                    self.assertEqual(group, process.pid)
                    self.assertEqual(sig, signal.SIGKILL)
                    # Model asynchronous group death: the direct parent exits
                    # immediately, but its owned descendant exits a little later.
                    os.kill(process.pid, sig)
                    timer = threading.Timer(0.15, finish_group)
                    timer.start()

                with patch.object(CHECK.os, "killpg", side_effect=parent_exits_first):
                    CHECK.stop_owned_process(process)
                self.assertFalse(self.child_running(pid), "cleanup returned before the signalled descendant exited")
            finally:
                try:
                    kill_group(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait(timeout=10)
                if timer is not None:
                    timer.join(timeout=2)

    def child_running(self, pid):
        if os.name == "nt":
            import ctypes
            from ctypes import wintypes
            kernel = ctypes.WinDLL("kernel32", use_last_error=True)
            kernel.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
            kernel.OpenProcess.restype = wintypes.HANDLE
            kernel.WaitForSingleObject.argtypes = [wintypes.HANDLE, wintypes.DWORD]
            kernel.CloseHandle.argtypes = [wintypes.HANDLE]
            handle = kernel.OpenProcess(0x100000, False, pid)  # SYNCHRONIZE
            if not handle:
                self.assertIn(ctypes.get_last_error(), [87, 1168], "cannot inspect our fixture child's exit")
                return False
            try:
                status = kernel.WaitForSingleObject(handle, 0)
                self.assertIn(status, [0, 258])
                return status == 258
            finally:
                kernel.CloseHandle(handle)
        state = Path("/proc") / str(pid) / "stat"
        # An exited child may briefly remain a zombie until its new parent reaps it.
        return state.exists() and state.read_text().split(") ", 1)[1].split()[0] not in {"Z", "X"}

    def invoke(self, mode="pass", *, lua_missing=False, timeout=5):
        with tempfile.TemporaryDirectory(prefix="quota runner fixture ") as directory:
            fixture = Path(directory) / "go events.py"
            events = []
            for name in sorted(CHECK.REQUIRED):
                if name in CHECK.LUA_REQUIRED and mode != "no-lua":
                    events.append({"Action": "output", "Package": CHECK.PACKAGE, "Test": name, "Output": "AUTH_QUOTA_ACTUAL_LUA executions=1\n"})
                events.append({"Action": "pass", "Package": CHECK.PACKAGE, "Test": name})
            events.append({"Action": "pass", "Package": CHECK.PACKAGE})
            if mode == "zero":
                events = [{"Action": "pass", "Package": CHECK.PACKAGE}]
            elif mode == "missing":
                events = [e for e in events if e.get("Test") != "TestTokenQuotaLegacyCountersRepairOnceAndExpireThroughHTTP"]
            elif mode == "skip":
                events.append({"Action": "skip", "Package": CHECK.PACKAGE, "Test": "TestTokenQuotaDeviceBoundaryAndHTTPConsumer/subcase"})
            elif mode == "fail":
                events.append({"Action": "fail", "Package": CHECK.PACKAGE, "Test": "TestTokenQuotaDeviceBoundaryAndHTTPConsumer"})
            elif mode == "duplicate":
                events.append({"Action": "pass", "Package": CHECK.PACKAGE, "Test": "TestTokenQuotaDeviceBoundaryAndHTTPConsumer"})
            payload = "\n".join(json.dumps(event) for event in events) + "\n"
            if mode == "invalid-json":
                payload = "{broken\n"
            child_pid, child_ready = Path(directory) / "child.pid", Path(directory) / "child.ready"
            script = "import os, sys, time, subprocess\nfrom pathlib import Path\n"
            script += "assert 'PINVOU_TEST_AUTH_QUOTA_LUA_COMMAND' in os.environ\n"
            script += "assert '-timeout=90s' in sys.argv and '-run=^TestTokenQuota' in sys.argv\n"
            timeout_mode = mode in {"timeout", "timeout-diagnostics", "timeout-oversize", "timeout-invalid-utf8"}
            if timeout_mode:
                child = "from pathlib import Path; import time; Path(" + repr(str(child_ready)) + ").write_text('ready'); time.sleep(30)"
                script += "child = subprocess.Popen([sys.executable, '-c', " + repr(child) + "], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)\n"
                script += "Path(" + repr(str(child_pid)) + ").write_text(str(child.pid))\n"
                script += "while not Path(" + repr(str(child_ready)) + ").exists(): time.sleep(0.01)\n"
                if mode == "timeout-diagnostics":
                    script += "sys.stdout.write(" + repr(payload) + "); sys.stdout.flush()\n"
                    script += "sys.stderr.write('QUOTA_FIXTURE_DIAGNOSTIC_BEFORE_TIMEOUT\\n'); sys.stderr.flush()\n"
                elif mode == "timeout-oversize":
                    script += "sys.stderr.write('x' * ((1 << 20) + 1)); sys.stderr.flush()\n"
                elif mode == "timeout-invalid-utf8":
                    script += "os.write(1, b'\\xff'); os.write(2, b'QUOTA_FIXTURE_TRUNCATED_UTF8\\n')\n"
                script += "time.sleep(30)\n"
            if mode == "invalid-utf8":
                invalid_output = payload.encode("utf-8") + b'{"Action":"output","Output":"\xff"}\n'
                script += "os.write(1, " + repr(invalid_output) + ")\n"
            else:
                script += "sys.stdout.write(" + repr(payload) + ")\n"
            script += "raise SystemExit(" + ("2" if mode == "nonzero" else "0") + ")\n"
            fixture.write_text(script, encoding="utf-8")
            lua = str(Path(directory) / "missing-lua") if lua_missing else sys.executable
            stdout, stderr = io.StringIO(), io.StringIO()
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                code = CHECK.main([
                    "--lua-command-json", json.dumps([lua]),
                    "--go-command-json", json.dumps([sys.executable, "-B", str(fixture)]),
                    "--timeout", str(timeout),
                ])
            if timeout_mode:
                self.assertTrue(child_ready.exists(), "timeout negative must reach a running descendant")
                pid = int(child_pid.read_text())
                try:
                    self.assertFalse(self.child_running(pid), "owned Lua-like descendant survived the runner timeout")
                finally:
                    # A broken cleanup must not leak the negative-control child.
                    if self.child_running(pid):
                        if os.name == "nt":
                            subprocess.run(["taskkill", "/PID", str(pid), "/T", "/F"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10, check=False)
                        else:
                            os.kill(pid, signal.SIGKILL)
                if mode == "timeout-diagnostics":
                    self.assertIn('"Action": "pass"', stdout.getvalue(), "timeout discarded the Go output")
                elif mode == "timeout-oversize":
                    self.assertEqual(stdout.getvalue(), "", "oversized diagnostics were published")
                elif mode == "timeout-invalid-utf8":
                    self.assertIn("\ufffd", stdout.getvalue(), "partial UTF-8 discarded timeout diagnostics")
            return code, stderr.getvalue()

    def test_complete_subprocess_evidence_passes(self):
        code, message = self.invoke()
        self.assertEqual(code, 0, message)
        self.assertIn("AUTH_QUOTA_PRODUCTION_LUA_PASS", message)

    def test_rejects_missing_zero_skip_fail_duplicate_and_model_only(self):
        for mode in ["zero", "missing", "skip", "fail", "duplicate", "no-lua", "nonzero", "invalid-json"]:
            with self.subTest(mode=mode):
                code, message = self.invoke(mode)
                self.assertEqual(code, 1, message)
                self.assertIn("AUTH_QUOTA_PRODUCTION_LUA_FAIL", message)

    def test_missing_cli_fails_before_go_execution(self):
        code, message = self.invoke(lua_missing=True)
        self.assertEqual(code, 1)
        self.assertIn("required test executable is unavailable", message)

    def test_timeout_terminates_owned_process(self):
        started = time.monotonic()
        code, message = self.invoke("timeout", timeout=1)
        self.assertEqual(code, 1)
        self.assertIn("process deadline", message)
        self.assertLess(time.monotonic() - started, 5)

    def test_timeout_preserves_diagnostics_without_accepting_pass_events(self):
        code, message = self.invoke("timeout-diagnostics", timeout=1)
        self.assertEqual(code, 1)
        self.assertIn("process deadline", message)
        self.assertIn("QUOTA_FIXTURE_DIAGNOSTIC_BEFORE_TIMEOUT", message)
        self.assertNotIn("AUTH_QUOTA_PRODUCTION_LUA_PASS", message)

    def test_timeout_diagnostics_keep_the_output_bound(self):
        code, message = self.invoke("timeout-oversize", timeout=1)
        self.assertEqual(code, 1)
        self.assertIn("test output exceeded its bound", message)
        self.assertNotIn("xxxxxxxx", message)

    def test_timeout_keeps_partial_utf8_diagnostics(self):
        code, message = self.invoke("timeout-invalid-utf8", timeout=1)
        self.assertEqual(code, 1)
        self.assertIn("process deadline", message)
        self.assertIn("QUOTA_FIXTURE_TRUNCATED_UTF8", message)

    def test_successful_exit_still_requires_strict_utf8(self):
        code, message = self.invoke("invalid-utf8")
        self.assertEqual(code, 1)
        self.assertIn("AUTH_QUOTA_PRODUCTION_LUA_FAIL", message)


if __name__ == "__main__":
    unittest.main()
