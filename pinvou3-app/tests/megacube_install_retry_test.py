#!/usr/bin/env python3
"""Real stdio MCP against an isolated Unix fixture; not model inference."""
import json
import os
from pathlib import Path
import socketserver
import tempfile
import threading
import unittest

from megacube_mcp_smoke import McpClient


class StoreRetryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        home = Path(self.tmp.name)
        (home / "model-store").mkdir()
        self.calls = []
        self.capable = True
        self.reject_retry = False
        self.observation = "pending"
        self.log_value = None
        fixture = self

        class Handler(socketserver.StreamRequestHandler):
            def handle(self):
                _, path, _ = self.rfile.readline().decode().split()
                length = 0
                while True:
                    line = self.rfile.readline()
                    if line == b"\r\n":
                        break
                    key, value = line.decode().split(":", 1)
                    if key.lower() == "content-length":
                        length = int(value)
                body = json.loads(self.rfile.read(length))
                fixture.calls.append((path, body))
                method = path.rsplit("/", 1)[-1]
                if method == "health":
                    value = {"capabilities": ["model.install_retry.v1"] if fixture.capable else []}
                elif method == "library":
                    value = {"models": [{"private": "must-not-be-returned"}],
                             "incompleteInstallations": [{"runId": "failed-run", "kind": "download", "canPreview": True}],
                             "incompleteError": None}
                elif method == "incomplete_preview":
                    value = {"runId": body["run_id"], "previewSha256": "a" * 64, "kind": "download", "modelWritesGranted": False}
                elif method == "incomplete_retry":
                    value = {"run_id": "cmsd-test-task", "cmsd_task_id": "test-task"}
                elif method == "log":
                    value = fixture.log_value or {"lines": [], "next": body["from"], "done": False, "backend_pending": fixture.observation == "pending", "unconfirmed": fixture.observation == "unknown"}
                elif method == "cancel":
                    value = {"ok": True, "status": "cancelling"}
                else:
                    raise AssertionError(method)
                code = 409 if method == "incomplete_retry" and fixture.reject_retry else 200
                payload = json.dumps({"error": {"message": "installation_retry_capability_required"}} if code == 409 else {"result": value}).encode()
                self.wfile.write(f"HTTP/1.1 {code} Fixture\r\nContent-Length: {len(payload)}\r\nConnection: close\r\n\r\n".encode() + payload)

        self.server = socketserver.ThreadingUnixStreamServer(str(home / "model-store/service.sock"), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.client = McpClient({**os.environ, "PINVOU3_HOME": str(home), "CMSD_SOCKET": str(home / "absent.sock"), "PYTHONDONTWRITEBYTECODE": "1"})
        self.addCleanup(self.client.close)

    def test_discovery_exposes_only_reviewed_recovery_arguments(self):
        tools = self.client.call("tools/list")["result"]["tools"]
        retry = next(t for t in tools if t["name"] == "cube_retry_install")
        self.assertEqual(set(retry["inputSchema"]["properties"]), {"run_id", "expected_preview_sha256"})
        self.assertFalse(retry["inputSchema"]["additionalProperties"])

    def test_failed_no_db_index_and_preview_do_not_execute(self):
        listed = self.client.tool("cube_list_incomplete_installs")
        self.assertEqual(listed["data"]["installations"][0]["runId"], "failed-run")
        self.assertNotIn("must-not-be-returned", json.dumps(listed))
        preview = self.client.tool("cube_preview_install_retry", {"run_id": "failed-run"})
        self.assertEqual(preview["data"]["kind"], "download")
        self.assertFalse(preview["data"]["modelWritesGranted"])
        self.assertFalse(any(path.endswith("incomplete_retry") for path, _ in self.calls))

    def test_missing_cmsd_is_unconfirmed_and_points_to_the_store_index(self):
        result = self.client.tool("cube_get_task")
        self.assertEqual(result["data"]["cmsd_task_observation"], "unconfirmed")
        self.assertIn("cube_list_incomplete_installs", result["next"])

    def test_retry_reuses_ui_path_and_can_observe_and_cancel_before_child_exists(self):
        accepted = self.client.tool("cube_retry_install", {"run_id": "failed-run", "expected_preview_sha256": "a" * 64})
        task_id = accepted["data"]["task_id"]
        self.assertEqual(task_id, "store:cmsd-test-task")
        self.assertIn(("/v1/ui/incomplete_retry", {"run_id": "failed-run", "expected_sha256": "a" * 64}), self.calls)
        task = self.client.tool("cube_get_task", {"task_id": task_id, "wait_seconds": 0})
        self.assertEqual(task["data"]["status"], "running")
        self.assertTrue(task["data"]["backend_pending"])
        self.observation = "unknown"
        self.assertEqual(self.client.tool("cube_get_task", {"task_id": task_id})["data"]["status"], "unknown")
        cancelled = self.client.tool("cube_cancel_task", {"task_id": task_id})
        self.assertEqual(cancelled["data"]["status"], "cancelling")
        self.assertIn(("/v1/ui/cancel", {"run_id": "cmsd-test-task"}), self.calls)

    def test_server_tail_preserves_absolute_total_and_explicit_preview_truncation(self):
        self.log_value = {"from": 9992, "lines": ["x" * 1000] * 8, "next": 10000,
                          "next_byte": 0, "total": 10000, "truncated_lines": [9993],
                          "done": True, "ok": False, "error": "original error", "error_truncated": True}
        result = self.client.tool("cube_get_task", {"task_id": "store:old-run", "wait_seconds": 0})
        data = result["data"]
        self.assertEqual(data["events_offset"], 9992)
        self.assertEqual(data["events_total"], 10000)
        self.assertIsNone(data["events_next_offset"])
        self.assertTrue(data["events"][1]["truncated"])
        self.assertTrue(data["error_truncated"])
        self.assertEqual(data["status"], "failed")
        request = next(body for path, body in self.calls if path.endswith("/log"))
        self.assertEqual((request["tail"], request["limit"], request["preview_chars"]), (8, 8, 1000))

    def test_explicit_page_advances_absolute_cursor_even_for_a_terminal_task(self):
        self.log_value = {"from": 10, "lines": ["a", "b"], "next": 12, "next_byte": 0,
                          "total": 100, "done": True, "ok": True, "result_truncated": True}
        result = self.client.tool("cube_get_task", {"task_id": "store:old-run", "events_offset": 10, "events_limit": 2, "wait_seconds": 0})
        data = result["data"]
        self.assertEqual((data["events_offset"], data["events_next_offset"], data["events_total"]), (10, 12, 100))
        self.assertTrue(data["result_available"])
        request = next(body for path, body in self.calls if path.endswith("/log"))
        self.assertNotIn("tail", request)

    def test_missing_capability_never_falls_back_to_install(self):
        self.capable = False
        result = self.client.tool("cube_retry_install", {"run_id": "failed-run", "expected_preview_sha256": "a" * 64})
        self.assertEqual(result["data"]["error_code"], "STORE_RETRY_CAPABILITY_REQUIRED")
        self.assertEqual([path for path, _ in self.calls], ["/v1/ui/health"])

    def test_rejected_admission_is_returned_without_resubmission(self):
        self.reject_retry = True
        result = self.client.tool("cube_retry_install", {"run_id": "failed-run", "expected_preview_sha256": "a" * 64})
        self.assertEqual(result["data"]["error_code"], "STORE_REQUEST_REJECTED")
        self.assertEqual(sum(path.endswith("incomplete_retry") for path, _ in self.calls), 1)

    def test_owner_confirmed_paths_and_invalid_digest_are_rejected_before_io(self):
        for args in ({"run_id": "../outside", "expected_preview_sha256": "a" * 64},
                     {"run_id": "failed-run", "expected_preview_sha256": "b"},
                     {"run_id": "failed-run", "expected_preview_sha256": "a" * 64, "confirmed": True}):
            self.assertIn("error_code", self.client.tool("cube_retry_install", args)["data"])
        self.assertEqual(self.calls, [])


if __name__ == "__main__":
    unittest.main()
