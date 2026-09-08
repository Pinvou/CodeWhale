from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "account-platform-kms-monitor.py"
SPEC = importlib.util.spec_from_file_location("kms_monitor", SCRIPT)
MON = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MON)


class KMSMonitorTests(unittest.TestCase):
    def test_certificate_disk_and_time_thresholds(self):
        now = time.time()
        self.assertEqual(MON.certificate_status(now + 15*86400, now), "ok")
        self.assertEqual(MON.certificate_status(now + 13*86400, now), "critical")
        self.assertEqual(MON.certificate_status(now - 1, now), "critical")
        self.assertEqual(MON.capacity_status(100 << 30, 30 << 30, 100, 20), "ok")
        self.assertEqual(MON.capacity_status(100 << 30, 15 << 30, 100, 20), "warning")
        self.assertEqual(MON.capacity_status(100 << 30, 5 << 30, 100, 20), "critical")
        self.assertEqual(MON.capacity_status(100 << 30, 30 << 30, 100, 9), "warning")
        for delta, expected in ((0, "ok"), (-80*60, "ok"), (-91*60, "critical"), (301, "critical")):
            stamp = datetime.fromtimestamp(now + delta, timezone.utc).isoformat()
            self.assertEqual(MON.age_status(stamp, now), expected)
        with self.assertRaises(ValueError):
            MON.age_status("2026-01-01T00:00:00", now)

    def test_snapshot_integrity_staleness_and_incomplete_attempt(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(MON, "private", side_effect=lambda path, **kwargs: path.stat()):
            root = Path(directory)
            now = time.time()
            snapshot = root / "snapshot-20260908T010000Z-0123456789abcdef"
            snapshot.mkdir()
            cipher = b"test-only-encrypted-content-placeholder"
            (snapshot / "snapshot.age").write_bytes(cipher)
            data = {"version": 1, "captured_at": datetime.fromtimestamp(now, timezone.utc).isoformat(),
                    "ciphertext_bytes": len(cipher), "ciphertext_sha256": hashlib.sha256(cipher).hexdigest()}
            (snapshot / "SNAPSHOT_READY").write_text(json.dumps(data))
            self.assertEqual(MON.check_snapshot(root, now), "ok")
            self.assertEqual(MON.check_snapshot(root, now + 91*60), "critical")
            incomplete = root / "snapshot-20260908T000000Z-0123456789abcdef"
            incomplete.mkdir()
            os.utime(incomplete, (now-1000, now-1000))
            self.assertEqual(MON.check_snapshot(root, now), "warning")
            (snapshot / "snapshot.age").write_bytes(b"corrupt")
            with self.assertRaises(ValueError):
                MON.check_snapshot(root, now)

    def test_all_failures_are_fixed_checks_without_exception_content(self):
        message = "sensitive-response-must-not-be-published"
        with patch.object(MON, "check_tls", side_effect=ValueError(message)), \
             patch.object(MON, "check_agents", return_value="ok"), \
             patch.object(MON, "check_backup_unit", return_value="ok"), \
             patch.object(MON, "check_snapshot", return_value="ok"), \
             patch.object(MON, "check_disk", return_value="warning"), \
             patch.object(MON, "check_audit", return_value="ok"):
            data = MON.collect(Path("/example"), "https://example.invalid", "test", time.time())
        self.assertEqual(data["status"], "attention_required")
        self.assertEqual(data["checks"]["kms_tls_health_and_certificate"], "critical")
        self.assertEqual(data["checks"]["disk_and_inodes"], "warning")
        self.assertNotIn(message, json.dumps(data))
        self.assertEqual(data["notification_delivery"], "local_status_only")

    def test_monitor_uses_lease_only_and_no_privileged_mutations(self):
        with patch.object(MON, "run_command", return_value=b"") as command:
            self.assertEqual(MON.check_agents(Path("/example"), "https://example.invalid", "test"), "ok")
        args = command.call_args.args[0]
        self.assertIn("--lease-only", args)
        self.assertNotIn("--require-renewal", args)

    def test_backup_failure_or_disabled_timer_is_not_healthy(self):
        with patch.object(MON, "run_command", side_effect=[b"active", b"enabled", b"Result=exit-code\nExecMainStatus=1\n"]):
            with self.assertRaises(ValueError):
                MON.check_backup_unit()
        with patch.object(MON, "run_command", side_effect=ValueError("timer inactive")):
            with self.assertRaises(ValueError):
                MON.check_backup_unit()

    @unittest.skipUnless(os.name == "posix" and hasattr(os, "geteuid") and os.geteuid() == 0,
                         "requires Linux root file permission proof")
    def test_atomic_private_status_replaces_only_its_owned_file(self):
        with tempfile.TemporaryDirectory(prefix="pinvou-monitor-test-") as directory:
            root = Path(directory).resolve()
            os.chmod(root, 0o700)
            MON.publish(root, {"status": "ok"})
            MON.publish(root, {"status": "attention_required"})
            self.assertEqual(json.loads((root / "status.json").read_text())["status"], "attention_required")
            self.assertEqual((root / "status.json").stat().st_mode & 0o777, 0o600)
            self.assertEqual([p.name for p in root.iterdir()], ["status.json"])


if __name__ == "__main__":
    unittest.main()
