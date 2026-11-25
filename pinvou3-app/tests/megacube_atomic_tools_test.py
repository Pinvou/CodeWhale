#!/usr/bin/env python3
"""Offline behavioral coverage: real MCP, durable workers, HTTP ranges and artifacts."""
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import signal
import stat
import sys
import tempfile
import threading
import time
import unittest
import urllib.parse

from megacube_mcp_smoke import McpClient

DATA = b"GGUF" + bytes(range(256)) * 8192
DIGEST = hashlib.sha256(DATA).hexdigest()
REVISION = "a" * 40


class Fixture(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    ranges = []
    resolved_paths = []
    stream_bad = False
    ignore_range = False
    changed_etag = False
    short_budget_exhausted = False

    def log_message(self, *args):
        pass

    def respond(self, data, status=200, headers=None):
        self.send_response(status)
        self.send_header("Content-Length", str(len(data)))
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def json(self, data, status=200):
        self.respond(json.dumps(data).encode(), status, {"Content-Type": "application/json"})

    def do_HEAD(self):
        self.send_response(200)
        self.send_header("Content-Length", str(len(DATA)))
        self.end_headers()

    def do_GET(self):
        path = urllib.parse.urlsplit(self.path).path
        if path == "/api/models":
            return self.json([{"id": "test/GGUF", "lastModified": "2026-10-16T00:00:00Z", "downloads": 9}])
        if path == "/api/models/test/GGUF/revision/main":
            return self.json({"sha": REVISION, "private": False, "gated": False, "siblings": [
                {"rfilename": "weights/model.gguf", "lfs": {"size": len(DATA), "sha256": DIGEST}},
                {"rfilename": "config.json", "size": 2}]})
        if path == "/api/models/test/Bad/revision/main":
            return self.json({"sha": REVISION, "siblings": [{"rfilename": "../escape"}]})
        if path == "/test/GGUF/raw/%s/config.json" % REVISION:
            return self.json({"architectures": ["UnknownArchitecture"], "quantization_config": {"quant_method": "q4"}})
        if path == "/api/models/test/GGUF":
            return self.json({"siblings": [{"rfilename": "model.gguf"}], "gated": False})
        if path == "/v1/models":
            return self.json({"data": [{"id": "fixture-model"}]})
        if path == "/fail":
            return self.json({"error": "not found"}, 404)
        if "/resolve/" in path:
            Fixture.resolved_paths.append(path)
        if path.endswith("config.json"):
            return self.respond(b"{}")
        if path in ("/file", "/slow", "/ignore-range", "/bad-range") or "/resolve/" in path:
            start = 0
            requested = self.headers.get("Range")
            if requested:
                Fixture.ranges.append((path, requested, self.headers.get("If-Range")))
                start = int(requested.removeprefix("bytes=").removesuffix("-"))
            if path == "/ignore-range" or Fixture.ignore_range:
                start = 0
            if start >= len(DATA):
                return self.respond(b"", 416)
            self.send_response(206 if start else 200)
            self.send_header("Content-Length", str(len(DATA) - start))
            self.send_header("ETag", '"changed"' if Fixture.changed_etag else '"fixture-v1"')
            if start:
                self.send_header("Content-Range", "bytes %d-%d/%d" % (
                    0 if path == "/bad-range" else start, len(DATA) - 1, len(DATA)))
            self.end_headers()
            try:
                for offset in range(start, len(DATA), 8192):
                    self.wfile.write(DATA[offset:offset + 8192])
                    self.wfile.flush()
                    if path == "/slow":
                        time.sleep(0.01)
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        return self.respond(b"", 404)

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if Fixture.short_budget_exhausted and request.get("max_tokens", 0) < 128 and not request.get("stream"):
            return self.json({"choices": [{"finish_reason": "length", "message": {"content": None}}]})
        if request.get("stream"):
            if Fixture.stream_bad:
                return self.respond(b'data: {"error":{"message":"bad"}}\n\ndata: [DONE]\n\n')
            return self.respond(b'data: {"choices":[{"delta":{"content":"OK"}}]}\n\ndata: [DONE]\n\n',
                                headers={"Content-Type": "text/event-stream"})
        return self.json({"choices": [{"message": {"content": "OK"}}]})


class AtomicToolsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.http = ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
        threading.Thread(target=cls.http.serve_forever, daemon=True).start()
        cls.base = "http://127.0.0.1:%d" % cls.http.server_port

    @classmethod
    def tearDownClass(cls):
        cls.http.shutdown()
        cls.http.server_close()

    def setUp(self):
        Fixture.ignore_range = False
        Fixture.changed_etag = False
        Fixture.short_budget_exhausted = False
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name)
        self.dest = self.path / "outside-store"
        self.dest.mkdir()
        self.env = dict(os.environ, PINVOU3_HOME=str(self.path / "home"),
                        CMSD_SOCKET=str(self.path / "absent.sock"), PINVOU_HF_ENDPOINT=self.base,
                        PYTHONDONTWRITEBYTECODE="1")
        self.client = McpClient(self.env)
        self.tasks = []

    def close_client(self):
        self.client.close()
        self.client.proc.stdout.close()
        self.client.proc.stderr.close()

    def tearDown(self):
        for task_id in self.tasks:
            result = self.client.tool("cube_get_task", {"task_id": task_id})
            if result.get("data", {}).get("status") not in ("succeeded", "failed", "cancelled", "interrupted"):
                self.client.tool("cube_cancel_task", {"task_id": task_id})
                self.wait(task_id)
        self.close_client()
        self.tmp.cleanup()

    def tool(self, tool_name, **args):
        result = self.client.tool(tool_name, args)
        if tool_name != "cube_get_task":
            self.assertNotIn("error_code", result.get("data") or {}, result)
        return result["data"]

    def start(self, path="/file", name="weights/model.gguf", digest=DIGEST):
        item = {"url": self.base + path, "path": name, "size_bytes": len(DATA)}
        if digest is not None:
            item["sha256"] = digest
        data = self.tool("cube_fetch_files", directory=str(self.dest), files=[item])
        self.tasks.append(data["task_id"])
        return data["task_id"]

    def wait(self, task_id, statuses=("succeeded", "failed", "cancelled", "interrupted")):
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            data = self.tool("cube_get_task", task_id=task_id)
            if data["status"] in statuses:
                return data
            time.sleep(0.03)
        self.fail("task did not reach %s: %s" % (statuses, data))

    def cancel_after_progress(self, task_id):
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            data = self.tool("cube_get_task", task_id=task_id)
            if data.get("progress_done_bytes", 0) >= 65536:
                break
            time.sleep(0.05)
        self.assertGreater(data["progress_done_bytes"], 0)
        self.tool("cube_cancel_task", task_id=task_id)
        self.assertEqual(self.wait(task_id)["status"], "cancelled")

    def test_standard_rejection_is_not_installation_rejection(self):
        data = self.tool("cube_check_model_repo", repo_id="test/GGUF")
        self.assertFalse(data["standard_install_supported"])
        facts = self.tool("cube_inspect_source", source="hf", repo_id="test/GGUF")
        self.assertEqual(facts["config"]["architectures"], ["UnknownArchitecture"])
        self.assertEqual(facts["resolved_revision"], REVISION)
        task_id = self.start()
        self.assertEqual(self.wait(task_id)["status"], "succeeded")

    def test_search_and_environment_independent_of_cmsd(self):
        data = self.tool("cube_search_models", query="GGUF")
        self.assertEqual(data["models"][0]["id"], "test/GGUF")
        env = self.tool("cube_inspect_environment", directory=str(self.dest))
        self.assertGreater(env["disk_free_bytes"], 0)
        self.assertIn("machine", env)
        self.assertFalse(self.tool("cube_get_status")["cmsd_available"])

    def test_repo_pins_commit_and_preserves_nested_paths(self):
        Fixture.resolved_paths.clear()
        data = self.tool("cube_fetch_repo", repo_id="test/GGUF", directory=str(self.dest), include=["*.gguf"])
        self.tasks.append(data["task_id"])
        result = self.wait(data["task_id"])
        self.assertEqual(result["status"], "succeeded", result)
        self.assertEqual((self.dest / "weights/model.gguf").read_bytes(), DATA)
        self.assertTrue(all(REVISION in p for p in Fixture.resolved_paths))
        self.assertFalse((self.dest / "config.json").exists())

    def test_download_survives_mcp_restart(self):
        task_id = self.start("/slow")
        self.wait(task_id, ("running",))
        self.close_client()
        self.client = McpClient(self.env)
        recent = self.tool("cube_get_task")["tasks"]
        self.assertIn(task_id, [t["id"] for t in recent])
        self.assertEqual(self.wait(task_id)["status"], "succeeded")
        self.assertEqual((self.dest / "weights/model.gguf").read_bytes(), DATA)

    def test_cancel_resume_uses_verified_range(self):
        Fixture.ranges.clear()
        task_id = self.start("/slow")
        self.cancel_after_progress(task_id)
        self.assertFalse((self.dest / "weights/model.gguf").exists())
        self.tool("cube_resume_task", task_id=task_id)
        result = self.wait(task_id)
        self.assertEqual(result["status"], "succeeded", result)
        self.assertTrue(any(p == "/slow" and int(r[6:-1]) > 0 for p, r, _ in Fixture.ranges))
        self.assertEqual((self.dest / "weights/model.gguf").read_bytes(), DATA)

    def test_worker_interruption_can_resume(self):
        task_id = self.start("/slow")
        data = self.wait(task_id, ("running",))
        os.kill(data["pid"], signal.SIGKILL)  # Only the test-created transfer worker.
        self.wait(task_id, ("interrupted",))
        self.tool("cube_resume_task", task_id=task_id)
        self.assertEqual(self.wait(task_id)["status"], "succeeded")

    def test_server_ignoring_range_restarts_without_appending(self):
        task_id = self.start("/slow")
        self.cancel_after_progress(task_id)
        Fixture.ignore_range = True
        self.tool("cube_resume_task", task_id=task_id)
        result = self.wait(task_id)
        self.assertEqual(result["status"], "succeeded", result)
        self.assertEqual((self.dest / "weights/model.gguf").read_bytes(), DATA)

    def test_changed_source_without_expected_hash_does_not_resume(self):
        task_id = self.start("/slow", digest=None)
        self.cancel_after_progress(task_id)
        Fixture.changed_etag = True
        self.tool("cube_resume_task", task_id=task_id)
        result = self.wait(task_id)
        self.assertEqual(result["error_code"], "SOURCE_CHANGED", result)
        self.assertFalse((self.dest / "weights/model.gguf").exists())

    def test_multi_file_resume_reuses_only_its_own_computed_files(self):
        data = self.tool("cube_fetch_files", directory=str(self.dest), files=[
            {"path": "first.gguf", "url": self.base + "/file"},
            {"path": "second.gguf", "url": self.base + "/slow"},
        ])
        task_id = data["task_id"]
        self.tasks.append(task_id)
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            state = self.tool("cube_get_task", task_id=task_id)
            if state.get("files_completed") == 1 and state.get("current_file") == "second.gguf":
                break
            time.sleep(0.03)
        self.assertEqual(state["files_completed"], 1)
        self.tool("cube_cancel_task", task_id=task_id)
        self.assertEqual(self.wait(task_id)["status"], "cancelled")
        self.tool("cube_resume_task", task_id=task_id)
        result = self.wait(task_id)
        self.assertEqual(result["status"], "succeeded", result)
        self.assertTrue(all(a["verification"] == "computed_only" for a in result["artifacts"]))

    def test_manifest_matches_live_tool_discovery_and_optional_task_id(self):
        manifest = json.loads((Path(__file__).resolve().parents[1] / "resources/mcp-servers/megacube-admin/manifest.json").read_text())
        tools = self.client.call("tools/list")["result"]["tools"]
        self.assertEqual(set(manifest["mcp_tools"]), {"mcp_megacube_admin_" + t["name"] for t in tools})
        task = next(t for t in tools if t["name"] == "cube_get_task")
        self.assertNotIn("task_id", task["inputSchema"]["required"])

    def test_source_file_listing_is_paginated(self):
        first = self.tool("cube_inspect_source", source="hf", repo_id="test/GGUF", files_limit=1)
        self.assertEqual(first["files_total"], 2)
        self.assertEqual(first["files_next_offset"], 1)
        second = self.tool("cube_inspect_source", source="hf", repo_id="test/GGUF", files_offset=1, files_limit=1)
        self.assertNotEqual(first["files"][0]["path"], second["files"][0]["path"])

    def test_http_failures_hide_signed_query_and_do_not_publish(self):
        task_id = self.start("/fail?signature=do-not-echo")
        result = self.wait(task_id)
        self.assertEqual(result["error_code"], "HTTP_404", result)
        self.assertNotIn("do-not-echo", json.dumps(result))
        self.assertFalse((self.dest / "weights/model.gguf").exists())

    def test_hash_mismatch_not_published_and_existing_file_not_overwritten(self):
        task_id = self.start(digest="b" * 64)
        self.assertEqual(self.wait(task_id)["error_code"], "HASH_MISMATCH")
        self.assertFalse((self.dest / "weights/model.gguf").exists())
        target = self.dest / "existing.gguf"
        target.write_bytes(b"user data")
        second = self.start(name="existing.gguf")
        self.assertEqual(self.wait(second)["error_code"], "EXISTING_FILE_CONFLICT")
        self.assertEqual(target.read_bytes(), b"user data")

    def test_computed_digest_not_claimed_as_publisher_verification(self):
        task_id = self.start(digest=None)
        result = self.wait(task_id)
        self.assertEqual(result["status"], "succeeded", result)
        self.assertEqual(result["artifacts"][0]["verification"], "computed_only")

    def test_traversal_and_symlinks_never_escape_destination(self):
        for name in ("../escape", "/absolute", "nested/../../escape"):
            result = self.client.tool("cube_fetch_files", {"directory": str(self.dest), "files": [{"path": name, "url": self.base + "/file"}]})
            self.assertEqual(result["data"]["error_code"], "INVALID_FILENAME")
        (self.dest / "linked").symlink_to(self.path, target_is_directory=True)
        result = self.client.tool("cube_fetch_files", {"directory": str(self.dest), "files": [{"path": "linked/escape", "url": self.base + "/file"}]})
        self.assertEqual(result["data"]["error_code"], "UNSAFE_PATH")
        result = self.client.tool("cube_inspect_source", {"source": "hf", "repo_id": "test/Bad"})
        self.assertEqual(result["data"]["error_code"], "INVALID_FILENAME")
        self.assertFalse((self.path / "escape").exists())

    def test_private_request_and_redacted_public_url(self):
        task_id = self.start("/file?signature=test-secret")
        result = self.wait(task_id)
        self.assertNotIn("test-secret", json.dumps(result))
        request = self.path / "home/megacube/installations/tasks" / task_id / "request.json"
        self.assertEqual(stat.S_IMODE(request.stat().st_mode), 0o600)
        self.assertIn("test-secret", request.read_text())

    def test_artifact_check_verify_register_and_rediscover(self):
        (self.dest / "model.gguf").write_bytes(DATA)
        (self.dest / "INSTALLATION.md").write_text("Start: isolated engine. Stop: recorded command.")
        inspection = self.tool("cube_validate_artifact", directory=str(self.dest))
        self.assertFalse(inspection["inference_verified"])
        verification = self.tool("cube_verify_artifacts", directory=str(self.dest), files=[{"path": "model.gguf", "sha256": DIGEST}])
        self.tasks.append(verification["task_id"])
        self.assertEqual(self.wait(verification["task_id"])["status"], "succeeded")
        record = self.tool("cube_register_artifact", directory=str(self.dest), verification_task_id=verification["task_id"], instructions_file="INSTALLATION.md")
        self.assertFalse(record["cmsd_managed"])
        self.close_client()
        self.client = McpClient(self.env)
        records = self.tool("cube_list_models")["external_artifacts"]
        self.assertEqual(records[0]["id"], record["id"])
        self.assertEqual(records[0]["runtime_status"], "unknown")
        self.assertEqual(records[0]["instructions_file"], "INSTALLATION.md")

    def test_runtime_checks_and_evidence_cannot_be_fabricated(self):
        Fixture.stream_bad = False
        result = self.tool("cube_validate_runtime", directory=str(self.dest), base_url=self.base + "/v1", model="fixture-model")
        self.assertTrue(result["passed"], result)
        self.assertIn("tool_calling", result["untested"])
        record = self.tool("cube_register_artifact", directory=str(self.dest), runtime_validation_id=result["id"])
        self.assertEqual(record["runtime_validation_id"], result["id"])
        Fixture.stream_bad = True
        failed = self.tool("cube_validate_runtime", directory=str(self.dest), base_url=self.base + "/v1", model="fixture-model")
        self.assertFalse(failed["passed"])
        Fixture.stream_bad = False
        invalid = self.client.tool("cube_register_artifact", {"directory": str(self.dest), "runtime_validation_id": "validation-" + "0" * 32})
        self.assertIn("error_code", invalid["data"])

    def test_registration_update_preserves_success_evidence_and_exposes_failed_recheck(self):
        (self.dest / "INSTALLATION.md").write_text("Start and stop instructions; optional management is pending.")
        (self.dest / "model.gguf").write_bytes(DATA)
        task = self.tool("cube_verify_artifacts", directory=str(self.dest),
                         files=[{"path": "model.gguf", "sha256": DIGEST}])
        self.tasks.append(task["task_id"])
        self.assertEqual(self.wait(task["task_id"])["status"], "succeeded")
        runtime = self.tool("cube_validate_runtime", directory=str(self.dest), base_url=self.base + "/v1", model="fixture-model")
        first = self.tool("cube_register_artifact", directory=str(self.dest), source_url=self.base + "/model",
                          instructions_file="INSTALLATION.md", verification_task_id=task["task_id"], runtime_validation_id=runtime["id"])
        updated = self.tool("cube_register_artifact", directory=str(self.dest), name="Installed fixture")
        self.assertEqual(updated["created_at"], first["created_at"])
        self.assertEqual(updated["verification_task_id"], task["task_id"])
        self.assertEqual(updated["runtime_validation_id"], runtime["id"])
        self.assertEqual(updated["file_evidence"]["expected_sha256_files"], 1)
        self.assertEqual(updated["runtime_evidence"]["status"], "passed")
        self.assertEqual(updated["instructions_file"], "INSTALLATION.md")
        self.assertEqual(updated["source_url"], self.base + "/model")
        # Simulate an installed pre-upgrade record, which had receipt ids but
        # no history/summary fields. A new failed check must retain its id too.
        record_path = self.path / "home/megacube/installations/artifacts" / (first["id"] + ".json")
        legacy = json.loads(record_path.read_text())
        for key in ("evidence_history", "file_evidence", "runtime_evidence", "created_at"):
            legacy.pop(key, None)
        record_path.write_text(json.dumps(legacy))
        Fixture.stream_bad = True
        try:
            failed = self.tool("cube_validate_runtime", directory=str(self.dest), base_url=self.base + "/v1", model="fixture-model")
        finally:
            Fixture.stream_bad = False
        self.tool("cube_register_artifact", directory=str(self.dest), runtime_validation_id=failed["id"])
        self.close_client()
        self.client = McpClient(self.env)
        record = self.tool("cube_list_models")["external_artifacts"][0]
        self.assertEqual(record["runtime_status"], "unknown")
        self.assertEqual(record["runtime_evidence"]["status"], "failed")
        self.assertEqual(record["evidence_history"]["runtime_validations"], [runtime["id"], failed["id"]])
        self.assertEqual(record["file_evidence"]["status"], "succeeded")
        self.assertEqual(record["management_status"], "external_record_only")
        self.assertFalse(record["cmsd_managed"])

    def test_verify_missing_file_does_not_start_task(self):
        result = self.client.tool("cube_verify_artifacts", {"directory": str(self.dest), "files": [{"path": "missing.gguf", "sha256": DIGEST}]})
        self.assertEqual(result["data"]["error_code"], "MISSING_FILE")

    def test_runtime_budget_exhaustion_is_inconclusive_and_can_be_retested(self):
        Fixture.short_budget_exhausted = True
        short = self.tool("cube_validate_runtime", directory=str(self.dest), base_url=self.base + "/v1", model="fixture-model", max_tokens=32)
        self.assertFalse(short["passed"])
        self.assertEqual(short["status"], "inconclusive")
        chat = next(c for c in short["checks"] if c["capability"] == "chat")
        self.assertEqual(chat["reason"], "token_budget_exhausted")
        record = self.tool("cube_register_artifact", directory=str(self.dest), runtime_validation_id=short["id"])
        self.assertEqual(record["runtime_evidence"]["status"], "inconclusive")
        default = self.tool("cube_validate_runtime", directory=str(self.dest), base_url=self.base + "/v1", model="fixture-model", test_stream=False)
        self.assertTrue(default["passed"])
        self.assertEqual(default["max_tokens"], 128)
        self.assertIn("stream", default["untested"])
        invalid = self.client.tool("cube_validate_runtime", {"directory": str(self.dest), "base_url": self.base + "/v1", "model": "fixture-model", "max_tokens": 1000000})
        self.assertEqual(invalid["data"]["error_code"], "INVALID_TOKEN_BUDGET")


if __name__ == "__main__":
    unittest.main(verbosity=2)
