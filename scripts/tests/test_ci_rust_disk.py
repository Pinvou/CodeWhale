import importlib.util
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("ci_rust_disk", ROOT / "scripts/ci-rust-disk.py")
DISK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DISK)


class RustDiskTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.workspace = self.root / "checkout"
        workflow = self.workspace / ".github/workflows/pr-check.yml"
        workflow.parent.mkdir(parents=True)
        workflow.touch()
        self.sdk = self.root / "sdk"
        self.sdk.mkdir()
        (self.sdk / "unused.bin").write_bytes(b"sdk")
        self.keep = self.workspace / "target/dependency.rlib"
        self.keep.parent.mkdir()
        self.keep.write_bytes(b"keep")
        self.enterContext(patch.object(DISK.sys, "platform", "linux"))
        self.enterContext(patch.dict(os.environ, {"RUNNER_ENVIRONMENT": "github-hosted"}))
        self.enterContext(patch.object(DISK, "SDK_PATHS", (self.sdk, self.root / "missing-sdk")))
        self.enterContext(patch.object(DISK.shutil, "disk_usage", return_value=SimpleNamespace(free=DISK.MIN_FREE_BYTES)))

    def test_only_allowlisted_sdks_are_removed_and_missing_paths_are_ok(self):
        DISK.prepare_disk(self.workspace)
        self.assertFalse(self.sdk.exists())
        self.assertEqual(self.keep.read_bytes(), b"keep")
        # Idempotent: absent SDKs must not fail a repeated preparation.
        DISK.prepare_disk(self.workspace)

    def test_rejects_self_hosted_or_non_linux_before_deleting(self):
        for platform, environment in (("linux", "self-hosted"), ("win32", "github-hosted")):
            with self.subTest(platform=platform, environment=environment):
                with patch.object(DISK.sys, "platform", platform), patch.dict(os.environ, {"RUNNER_ENVIRONMENT": environment}):
                    with self.assertRaisesRegex(RuntimeError, "GitHub-hosted Linux"):
                        DISK.prepare_disk(self.workspace)
                self.assertTrue(self.sdk.exists())

    def test_rejects_workspace_overlap_before_any_deletion(self):
        for unsafe in (self.workspace, self.workspace / "target", self.root):
            with self.subTest(unsafe=unsafe), patch.object(DISK, "SDK_PATHS", (self.sdk, unsafe)):
                with self.assertRaisesRegex(RuntimeError, "overlapping"):
                    DISK.prepare_disk(self.workspace)
                self.assertTrue(self.sdk.exists())

    def test_rejects_redirected_path_before_any_deletion(self):
        original_resolve = Path.resolve

        def redirected_resolve(path, *args, **kwargs):
            return self.workspace if path == self.sdk else original_resolve(path)

        with patch.object(Path, "resolve", redirected_resolve):
            with self.assertRaisesRegex(RuntimeError, "redirected"):
                DISK.prepare_disk(self.workspace)
        self.assertTrue(self.sdk.exists())

    def test_rejects_non_directory_target_before_any_deletion(self):
        unexpected = self.root / "file"
        unexpected.touch()
        with patch.object(DISK, "SDK_PATHS", (self.sdk, unexpected)):
            with self.assertRaisesRegex(RuntimeError, "unexpected"):
                DISK.prepare_disk(self.workspace)
        self.assertTrue(self.sdk.exists())

    def test_rejects_low_disk_after_cleanup(self):
        with patch.object(DISK.shutil, "disk_usage", return_value=SimpleNamespace(free=DISK.MIN_FREE_BYTES - 1)):
            with self.assertRaisesRegex(RuntimeError, "at least 12 GiB"):
                DISK.prepare_disk(self.workspace)
        self.assertEqual(self.keep.read_bytes(), b"keep")

    def test_main_fails_without_workspace(self):
        with patch.dict(os.environ, {"GITHUB_WORKSPACE": ""}):
            self.assertEqual(DISK.main(), 1)
        self.assertTrue(self.sdk.exists())

    def test_cleanup_errors_are_not_ignored(self):
        with patch.dict(os.environ, {"GITHUB_WORKSPACE": str(self.workspace)}):
            with patch.object(DISK.shutil, "rmtree", side_effect=PermissionError("denied")):
                self.assertEqual(DISK.main(), 1)

    def test_workflow_prepares_disk_after_swap_and_before_both_harnesses(self):
        workflow = (ROOT / ".github/workflows/pr-check.yml").read_text(encoding="utf-8")
        job = workflow.split("\n  rust-test:", 1)[1].split("\n  windows-rust-test:", 1)[0]
        prepare = job.index("python3 scripts/ci-rust-disk.py")
        self.assertLess(job.index("Set up zram and swap"), prepare)
        self.assertLess(job.index("- name: Cargo test cache"), prepare)
        self.assertLess(prepare, job.index("- name: cargo test --lib --no-run"))
        self.assertLess(prepare, job.index("- name: cargo test --test behavior_telemetry"))
        self.assertIn('RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT"', job)
        self.assertNotIn("continue-on-error", job)


if __name__ == "__main__":
    unittest.main()
