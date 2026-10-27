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
import time
import unittest

SOURCE = Path(__file__).resolve().parents[1] / "account-platform-auth-quota-lua-check.py"
SPEC = importlib.util.spec_from_file_location("quota_lua_check", SOURCE)
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class QuotaLuaRunnerTests(unittest.TestCase):
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
            if mode == "timeout":
                child = "from pathlib import Path; import time; Path(" + repr(str(child_ready)) + ").write_text('ready'); time.sleep(30)"
                script += "child = subprocess.Popen([sys.executable, '-c', " + repr(child) + "], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)\n"
                script += "Path(" + repr(str(child_pid)) + ").write_text(str(child.pid))\n"
                script += "while not Path(" + repr(str(child_ready)) + ").exists(): time.sleep(0.01)\n"
                script += "time.sleep(30)\n"
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
            if mode == "timeout":
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


if __name__ == "__main__":
    unittest.main()
