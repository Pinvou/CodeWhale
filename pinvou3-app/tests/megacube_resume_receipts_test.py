#!/usr/bin/env python3
"""Offline production-worker regression. No sockets, subprocesses or real HTTP."""
import hashlib
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

MODULES = Path(__file__).resolve().parents[1] / "src-tauri/resources/common/bundle/mcp-servers"
sys.path.insert(0, str(MODULES))
from megacube_tools import state, transfers


class Response(io.BytesIO):
    status = 200

    def __init__(self, body):
        super().__init__(body)
        self.headers = {"Content-Length": str(len(body)), "ETag": '"v1"'}


class ResumeReceipts(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="r40-offline-")
        self.root = Path(self.temp.name)
        self.env = patch.dict(os.environ, {"PINVOU3_HOME": str(self.root / "profile")})
        self.env.start()
        self.destination = self.root / "files"
        self.destination.mkdir()
        self.first = self.destination / "one.bin"
        self.original = b"already downloaded"
        self.first.write_bytes(self.original)
        self.task_id = "fetch-" + "1" * 32
        self.job = state.private_dir(transfers.job_dir(self.task_id))
        self.receipt = self.job / "resume-artifacts.json"
        self.old = {"path": str(self.first), "size_bytes": len(self.original),
                    "sha256": hashlib.sha256(self.original).hexdigest(), "verification": "computed_only"}
        self.request = {"directory": str(self.destination), "credential_env": None,
                        "files": [{"path": "one.bin", "url": "https://example.invalid/one", "sha256": None,
                                   "size_bytes": len(self.original)},
                                  {"path": "two.bin", "url": "https://example.invalid/two", "sha256": None,
                                   "size_bytes": 1}]}
        self.initial = {"id": self.task_id, "task_id": self.task_id, "kind": "fetch", "status": "failed",
                        "directory": str(self.destination), "created_at": time.time(), "events": [],
                        "artifacts": [self.old], "files_completed": 1, "file_count": 2}
        state.write_json(self.job / "request.json", self.request)
        state.write_json(self.job / "state.json", self.initial)
        self.spawns = []
        self.spawn = patch.object(transfers, "spawn", lambda job: self.spawns.append(str(job)))
        self.spawn.start()
        self.network = patch.object(transfers, "open_url", side_effect=AssertionError("real HTTP forbidden"))
        self.network.start()

    def tearDown(self):
        self.network.stop()
        self.spawn.stop()
        self.env.stop()
        self.temp.cleanup()

    def saved(self):
        return state.read_json(self.job / "state.json")

    def resume(self):
        transfers.resume_task({"task_id": self.task_id})

    def cancel_queued(self):
        self.resume()
        result = transfers.cancel_task(self.task_id)
        self.assertTrue(result["data"]["cancel_requested"])
        transfers.run_worker(self.job)
        self.assertEqual(self.saved()["status"], "cancelled")

    def finish(self):
        self.resume()
        with patch.object(transfers, "open_url", return_value=Response(b"2")) as network:
            transfers.run_worker(self.job)
        result = self.saved()
        self.assertEqual(result["status"], "succeeded", result)
        self.assertEqual(result["files_completed"], 2)
        self.assertEqual(result["progress_done_bytes"], len(self.original) + 1)
        self.assertEqual(len(result["artifacts"]), 2)
        self.assertEqual(len({a["path"] for a in result["artifacts"]}), 2)
        self.assertEqual(network.call_count, 1)
        self.assertEqual(self.first.read_bytes(), self.original)
        return result

    def test_queued_cancel_keeps_computed_digest_and_resumes(self):
        self.cancel_queued()
        self.assertEqual(self.saved()["artifacts"], [])
        self.assertEqual(self.saved()["files_completed"], 0)
        self.finish()

    def test_cancel_during_existing_file_hash_keeps_reuse_evidence(self):
        self.resume()
        def interrupted(path, cancel=None):
            state.write_json(self.job / "cancel", {})
            cancel()
        with patch.object(transfers, "sha256_file", side_effect=interrupted):
            transfers.run_worker(self.job)
        self.assertEqual(self.saved()["status"], "cancelled")
        self.assertEqual(self.saved()["artifacts"], [])
        self.finish()

    def test_repeated_cancelled_resumes_have_no_duplicate_results_or_counts(self):
        for _ in range(3):
            self.cancel_queued()
        self.finish()

    def test_corruption_is_rejected_without_overwrite_and_can_be_repaired(self):
        self.cancel_queued()
        damaged = b"X" * len(self.original)
        self.first.write_bytes(damaged)
        self.resume()
        transfers.run_worker(self.job)
        self.assertEqual(self.saved().get("error_code"), "EXISTING_FILE_CONFLICT")
        self.assertEqual(self.first.read_bytes(), damaged)
        self.first.write_bytes(self.original)
        self.finish()

    def test_receipt_save_failure_precedes_clearing_old_public_artifacts(self):
        self.resume()
        real_write = transfers.write_json
        def fail(path, value):
            if Path(path).name == "resume-artifacts.json":
                raise OSError(28, "fixture full disk")
            return real_write(path, value)
        with patch.object(transfers, "write_json", side_effect=fail):
            transfers.run_worker(self.job)
        result = self.saved()
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["artifacts"], [self.old])
        self.finish()

    def test_current_verified_digest_supersedes_older_private_receipt(self):
        state.write_json(self.receipt, {"version": 1, "artifacts": [{**self.old, "sha256": "0" * 64}]})
        self.finish()

    def test_each_retry_rehashes_existing_files(self):
        self.cancel_queued()
        calls = []
        real_hash = transfers.sha256_file
        def record(path, cancel=None):
            calls.append(str(path))
            return real_hash(path, cancel)
        with patch.object(transfers, "sha256_file", side_effect=record):
            self.finish()
        self.assertIn(str(self.first), calls)

    def test_public_artifacts_are_current_attempt_and_private_cache_is_not_published(self):
        self.cancel_queued()
        self.assertEqual(self.saved()["artifacts"], [])
        public = transfers.task_status(self.task_id)
        self.assertNotIn("resume_artifacts", public)
        self.assertNotIn("resume-artifacts", json.dumps(public))
        self.assertTrue(self.receipt.is_file())
        self.finish()

    def test_publish_then_interrupted_state_save_still_has_verifiable_digest(self):
        self.first.unlink()
        self.request["files"] = self.request["files"][:1]
        self.request["files"][0]["size_bytes"] = 1
        self.initial["artifacts"] = []
        self.initial["file_count"] = 1
        state.write_json(self.job / "request.json", self.request)
        state.write_json(self.job / "state.json", self.initial)
        self.resume()
        real_link = transfers.os.link
        class SimulatedDeath(BaseException):
            pass
        def publish(*args, **kwargs):
            real_link(*args, **kwargs)
            raise SimulatedDeath()
        with patch.object(transfers, "open_url", return_value=Response(b"1")), patch.object(transfers.os, "link", side_effect=publish):
            with self.assertRaises(SimulatedDeath):
                transfers.run_worker(self.job)
        self.assertEqual(self.first.read_bytes(), b"1")
        # Simulate a later process observing the terminated worker.
        result = self.saved()
        result.update(status="interrupted", pid=None)
        state.write_json(self.job / "state.json", result)
        self.resume()
        transfers.run_worker(self.job)
        self.assertEqual(self.saved()["status"], "succeeded", self.saved())
        self.assertEqual(len(self.saved()["artifacts"]), 1)

    def test_verify_cancel_preserves_existing_all_or_nothing_public_result_semantics(self):
        self.request.update(operation="verify", files=[{"path": "one.bin", "sha256": None, "size_bytes": len(self.original)}])
        self.initial.update(kind="verify", artifacts=[])
        state.write_json(self.job / "request.json", self.request)
        state.write_json(self.job / "state.json", self.initial)
        self.cancel_queued()
        self.assertEqual(self.saved()["artifacts"], [])
        self.assertFalse(self.receipt.exists())
        self.resume()
        transfers.run_worker(self.job)
        self.assertEqual(self.saved()["status"], "succeeded")
        self.assertEqual(self.saved()["files_completed"], 1)
        self.assertEqual(len(self.saved()["artifacts"]), 1)
        self.assertFalse(self.receipt.exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
