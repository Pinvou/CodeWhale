"""Independent record provenance must not overwrite ambiguous local history."""
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src-tauri/resources/common/bundle/mcp-servers"))
from megacube_tools.state import private_dir, remember_local_device, write_json


class RecordOriginTest(unittest.TestCase):
    def test_local_observation_has_its_own_receipt_and_no_download_origin(self):
        with tempfile.TemporaryDirectory() as home, patch.dict(os.environ, PINVOU3_HOME=home):
            remember_local_device()
            base = Path(home) / "megacube"
            self.assertTrue((base / "local-device-observed-v1").is_file())
            self.assertFalse((base / "installations").exists())

    def test_new_generic_task_root_has_origin_but_no_local_receipt(self):
        with tempfile.TemporaryDirectory() as home, patch.dict(os.environ, PINVOU3_HOME=home):
            base = Path(home) / "megacube/installations"
            write_json(base / "tasks/test/record.json", {"status": "downloaded"})
            self.assertEqual((base / ".independent-records-v1").read_bytes(),
                             b"pinvou-independent-installation-records-v1\n")
            self.assertFalse((base.parent / "local-device-observed-v1").exists())
            self.assertFalse((base / "ownership").exists())

    def test_existing_empty_or_populated_roots_are_never_relabelled(self):
        for populated in (False, True):
            with tempfile.TemporaryDirectory() as home, patch.dict(os.environ, PINVOU3_HOME=home):
                base = Path(home) / "megacube/installations"
                base.mkdir(parents=True)
                if populated:
                    (base / "ownership").mkdir()
                    (base / "ownership/card").write_text("cmsd\n")
                private_dir(base / "tasks/new")
                self.assertFalse((base / ".independent-records-v1").exists())

    def test_repeated_writes_preserve_corrupt_origin_for_conservative_review(self):
        with tempfile.TemporaryDirectory() as home, patch.dict(os.environ, PINVOU3_HOME=home):
            base = Path(home) / "megacube/installations"
            private_dir(base / "tasks/one")
            (base / ".independent-records-v1").write_text("unconfirmed")
            private_dir(base / "artifacts")
            self.assertEqual((base / ".independent-records-v1").read_text(), "unconfirmed")

    def test_uncertain_origin_publication_is_not_later_relabelled_as_confirmed(self):
        with tempfile.TemporaryDirectory() as home, patch.dict(os.environ, PINVOU3_HOME=home):
            base = Path(home) / "megacube/installations"
            with patch("os.fsync", side_effect=OSError("injected fsync failure")):
                with self.assertRaises(OSError):
                    private_dir(base / "tasks/new")
            self.assertTrue((base / ".origin-pending").exists())
            private_dir(base / "tasks/retry")
            self.assertTrue((base / ".origin-pending").exists())


if __name__ == "__main__":
    unittest.main()
