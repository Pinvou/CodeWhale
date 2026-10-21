#!/usr/bin/env python3
"""Exercise generated download scripts and model-facing task boundaries offline."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest

from megacube_mcp_smoke import FakeCmsd, McpClient, SERVER

spec = importlib.util.spec_from_file_location("megacube_workflow", SERVER)
server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(server)


class LocalReuseTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.weights = b"fixture weights for import" * 300
        self.digest = hashlib.sha256(self.weights).hexdigest()
        self.card = self.root / "card"
        self.card.mkdir()
        self.models = self.root / "models"
        self.home = self.root / "home"
        self.home.mkdir()
        # Exercise the actual script; speed up only its heartbeat timer and
        # prohibit accidental CLI/network fallback in this offline fixture.
        setup = self.root / "bash-env"
        setup.write_text("sleep() { command sleep 0.01; }\n")
        commands = self.root / "commands"
        commands.mkdir()
        for name in ("python3", "curl", "wget", "hf"):
            path = commands / name
            path.write_text("#!/bin/sh\necho unexpected-network-fallback >&2\nexit 97\n")
            path.chmod(0o755)
        self.env = {**os.environ, "HOME": str(self.home), "BASH_ENV": str(setup),
                    "XDG_DATA_HOME": str(self.root / "data"),
                    "PATH": str(commands) + os.pathsep + os.environ["PATH"],
                    "LMM_MODELS_DIR": str(self.models), "LMM_IMPORT_DIRS": "", "LMM_IMPORT_CANDIDATES": ""}

    def render(self):
        self.filename = "model.safetensors"
        files = server._render_sglang_card_files({
            "repo_id": "test/fixture-NVFP4", "model_name": "fixture", "weights_bytes": len(self.weights),
            "lfs_files": [{"name": self.filename, "size": len(self.weights), "sha256": self.digest}],
            "small_files": ["config.json"], "quantization": "NVFP4"}, "fixture")
        for name, data in files.items():
            path = self.card / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
            if name.endswith(".sh"):
                path.chmod(0o755)
        self.destination = self.models / "fixture-NVFP4-test"

    def candidate(self, name, corrupted=False):
        directory = self.root / name
        directory.mkdir()
        (directory / self.filename).write_bytes(b"x" * len(self.weights) if corrupted else self.weights)
        (directory / "config.json").write_text("{}")
        (directory / ".cache").mkdir()
        (directory / ".cache" / "unrelated").write_text("do not import")
        return directory

    def run_download(self):
        return subprocess.run(["bash", str(self.card / "download_model.sh")], env=self.env,
                              capture_output=True, text=True, timeout=15)

    def test_host_candidate_with_spaces_is_verified_and_copied_without_moving_source(self):
        self.render()
        candidate = self.candidate("已有 权重")
        self.env["LMM_IMPORT_CANDIDATES"] = str(candidate)
        result = self.run_download()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.destination / self.filename).read_bytes(), self.weights)
        self.assertEqual((candidate / self.filename).read_bytes(), self.weights)
        self.assertFalse((self.destination / ".cache").exists())
        self.assertFalse((self.card / ".venv").exists())
        meta = json.loads((self.card / "card.json").read_text())
        self.assertIn("vendor/start.sh", meta["package_files"])

    def test_corrupt_same_size_candidate_is_skipped_before_copy(self):
        self.render()
        bad = self.candidate("bad", corrupted=True)
        good = self.candidate("good")
        self.env["LMM_IMPORT_CANDIDATES"] = str(bad) + "\n" + str(good)
        result = self.run_download()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.destination / self.filename).read_bytes(), self.weights)
        self.assertEqual((bad / self.filename).read_bytes(), b"x" * len(self.weights))
        self.assertEqual((good / self.filename).read_bytes(), self.weights)

    def test_existing_different_target_is_preserved(self):
        self.render()
        self.env["LMM_IMPORT_CANDIDATES"] = str(self.candidate("good"))
        self.destination.mkdir(parents=True)
        (self.destination / self.filename).write_bytes(b"existing user file")
        result = self.run_download()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.destination / self.filename).read_bytes(), b"existing user file")

    def test_failed_copy_does_not_publish_partial_weight_and_can_be_retried(self):
        self.render()
        candidate = self.candidate("good")
        self.env["LMM_IMPORT_CANDIDATES"] = str(candidate)
        command = self.root / "commands" / "cp"
        command.write_text('#!/bin/bash\nprintf partial > "${@: -1}"\nexit 7\n')
        command.chmod(0o755)
        failed = self.run_download()
        self.assertNotEqual(failed.returncode, 0)
        self.assertFalse((self.destination / self.filename).exists())
        self.assertFalse(list(self.destination.glob("*.lmm-import.*")))
        command.unlink()
        success = self.run_download()
        self.assertEqual(success.returncode, 0, success.stdout + success.stderr)
        self.assertEqual((self.destination / self.filename).read_bytes(), self.weights)
        self.assertEqual((candidate / self.filename).read_bytes(), self.weights)



class TaskDeliveryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.cmsd = FakeCmsd(str(self.root / "cmsd.sock"))
        self.cmsd.start()
        deadline = time.monotonic() + 3
        while not (self.root / "cmsd.sock").exists() and time.monotonic() < deadline:
            time.sleep(.01)
        self.client = McpClient({**os.environ, "PINVOU3_HOME": str(self.root / "home"),
                                 "CMSD_SOCKET": self.cmsd.sock_path, "MEGACUBE_CARDS_DIR": str(self.root / "cards")})

    def tearDown(self):
        self.client.close()
        self.client.proc.stdout.close()
        self.client.proc.stderr.close()
        self.cmsd.stop()
        self.cmsd.join(timeout=2)
        self.tmp.cleanup()

    def test_poll_is_bounded_and_explicit_pages_keep_failure_and_progress(self):
        self.cmsd.tasks["task-long"] = {
            "id": "task-long", "kind": "install", "card": "fixture", "status": "failed",
            "error": "package write permission denied", "created_at": 1,
            "progress_done_bytes": 1024, "progress_total_bytes": 1024,
            "last_event": {"stage": "store", "message": "x" * 100000},
            "events": [{"stage": "store", "message": str(i) + ":" + "x" * 3000} for i in range(500)]}
        result = self.client.tool("cube_get_task", {"task_id": "task-long", "wait_seconds": 0})
        data = result["data"]
        self.assertEqual(data["status"], "failed")
        self.assertIn("permission denied", result["summary"])
        self.assertNotIn("下载中", result["summary"])
        self.assertEqual(len(data["events"]), 8)
        self.assertEqual(data["events_offset"], 492)
        self.assertEqual(data["events_total"], 500)
        self.assertTrue(data["last_event"]["message_truncated"])
        self.assertLess(len(json.dumps(result)), 13000)
        first = self.client.tool("cube_get_task", {"task_id": "task-long", "events_offset": 0,
                                                  "events_limit": 3, "wait_seconds": 0})["data"]
        self.assertEqual(first["events_next_offset"], 3)
        self.assertTrue(first["events"][0]["message"].startswith("0:"))
        invalid = self.client.tool("cube_get_task", {"task_id": "task-long", "events_limit": 0, "wait_seconds": 0})
        self.assertEqual(invalid["data"]["error_code"], "INVALID_PAGE")
        recent = self.client.tool("cube_get_task")["data"]["tasks"]
        self.assertNotIn("events", recent[0])

    def test_model_summary_does_not_turn_discovery_or_cleanup_reservation_into_control(self):
        self.cmsd.models = [
            {"card":"owned","managed":True,"status":"stopped"},
            {"card":"adoptable","managed":False,"can_adopt":True,"can_reserve_uninstall":True,"status":"stopped"},
            {"card":"partial","managed":False,"can_adopt":False,"can_reserve_uninstall":True,"status":"not_deployed"},
        ]
        result=self.client.tool("cube_list_models")
        self.assertIn("1 个已托管、2 个仅发现",result["summary"])
        self.assertIn("只读观察",result["summary"])
        self.assertIn("显式启用",result["summary"])
        self.assertFalse(result["data"]["models"][1]["managed"])
        self.assertIn("不授启停权",result["next"])
        self.assertEqual(self.cmsd.model_actions,[])

    def test_successful_task_ack_with_null_error_still_returns_accepted_task(self):
        result=server._task_reply({"task_id":"accepted-task","error":None},"启动模型")
        self.assertIn("已下发",result["summary"])
        self.assertEqual(result["data"]["task_id"],"accepted-task")

    def test_mcp_management_request_records_device_awareness_without_claiming_ownership(self):
        result=self.client.tool("cube_start_model",{"card":"testcard-1b-nvfp4-tachyon"})
        self.assertIn("task_id",result["data"])
        records=self.root/"home/megacube/installations"
        receipt=self.root/"home/megacube/local-device-observed-v1"
        self.assertEqual(json.loads(receipt.read_text()), {"source":"cmsd","model_ownership":False})
        self.assertFalse(records.exists())
        self.assertFalse((records/"ownership/testcard-1b-nvfp4-tachyon").exists())

    def test_failed_local_observation_receipt_prevents_management_post(self):
        (self.root/"home/megacube/local-device-observed-v1").mkdir(parents=True)
        result=self.client.tool("cube_start_model",{"card":"testcard-1b-nvfp4-tachyon"})
        self.assertEqual(result["data"]["error_code"], "DEVICE_RECORD_FAILED")
        self.assertEqual(self.cmsd.model_actions, [])

    def test_success_guides_verification_and_local_card_reports_execution_effects(self):
        self.cmsd.tasks["task-done"] = {"id": "task-done", "kind": "install", "card": "fixture", "status": "succeeded"}
        done = self.client.tool("cube_get_task", {"task_id": "task-done"})
        self.assertIn("验证", done["next"])
        card = self.root / "cards" / "fixture"
        card.mkdir(parents=True)
        for name in ("install_engine", "download_model", "deploy", "enable", "stop", "uninstall"):
            (card / (name + ".sh")).write_text("#!/bin/sh\nexit 0\n")
        result = self.client.tool("cube_install_local_card", {"card_id": "fixture"})
        self.assertEqual(self.cmsd.installs, ["fixture"])
        self.assertEqual(result["data"]["execution_effects"], ["install_engine", "download_or_reuse_weights", "deploy"])
        self.assertEqual(result["data"]["management_status"], "not_confirmed_by_installation")

    def test_local_tachyon_card_never_reaches_the_management_install_api(self):
        card = self.root / "cards" / "native"
        card.mkdir(parents=True)
        for name in ("install_engine", "download_model", "deploy", "enable", "stop", "uninstall"):
            (card / (name + ".sh")).write_text("#!/bin/sh\nexit 0\n")
        for metadata, script in (({"engine_name": "Tachyon"}, ""),
                                 ({"engine_name": "sglang"}, "ENGINE=tachyon-serve\n"),
                                 (None, "ARTIFACT=weights.tachyon\n")):
            with self.subTest(metadata=metadata, script=script):
                (card / "card.json").unlink(missing_ok=True)
                if metadata is not None:
                    (card / "card.json").write_text(json.dumps(metadata))
                (card / "common.sh").write_text(script)
                result = self.client.tool("cube_install_local_card", {"card_id": "native"})
                self.assertEqual((result.get("data") or {}).get("error_code"), "TACHYON_STORE_REQUIRED")
                self.assertIn("H3C", result["summary"])
                self.assertIn("card_id", result["next"])
                self.assertEqual(self.cmsd.installs, [])



if __name__ == "__main__":
    unittest.main(verbosity=2)
