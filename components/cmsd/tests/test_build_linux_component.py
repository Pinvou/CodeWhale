"""Source provenance boundaries, using disposable Git repositories only."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location(
    "build_linux_component", Path(__file__).resolve().parents[1] / "scripts/build-linux-component.py")
BUILD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BUILD)


class SourceIdentityTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.repository = Path(self.temporary.name)
        self.root = self.repository / "components/cmsd"
        (self.root / "src").mkdir(parents=True)
        (self.root / "src/main.rs").write_text("fn main() {}\n")
        (self.root / ".gitignore").write_text("/target\n")
        self.git("init", "-q")
        self.git("add", ".")
        self.commit()

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.repository, text=True).strip()

    def commit(self):
        self.git("-c", "user.name=Component fixture", "-c", "user.email=fixture@example.invalid",
                 "-c", "core.hooksPath=/dev/null", "commit", "-qm", "fixture")

    def test_clean_identity_is_owning_repository_head(self):
        identity = BUILD.source_identity(self.root)
        self.assertEqual(identity["build_revision"], self.git("rev-parse", "HEAD"))
        self.assertFalse(identity["dirty"])
        self.assertEqual(len(identity["source_sha256"]), 64)

    def test_modified_and_staged_sources_require_explicit_development(self):
        file = self.root / "src/main.rs"
        file.write_text("fn main() { println!(\"changed\"); }\n")
        for staged in (False, True):
            if staged:
                self.git("add", ".")
            with self.assertRaisesRegex(ValueError, "committed and unchanged"):
                BUILD.source_identity(self.root)
            identity = BUILD.source_identity(self.root, allow_dirty=True)
            self.assertTrue(identity["dirty"])
            self.assertTrue(identity["build_revision"].startswith("development:"))

    def test_ignored_untracked_source_cannot_masquerade_as_committed(self):
        # Even a global/local ignore entry cannot hide a Cargo input from identity checks.
        (self.repository / ".git/info/exclude").write_text("hidden.rs\n")
        (self.root / "src/hidden.rs").write_text("pub fn hidden() {}\n")
        with self.assertRaises(ValueError):
            BUILD.source_identity(self.root)

    def test_assume_unchanged_cannot_hide_modified_source(self):
        self.git("update-index", "--assume-unchanged", "components/cmsd/src/main.rs")
        (self.root / "src/main.rs").write_text("fn main() { panic!(\"changed\"); }\n")
        self.assertEqual(self.git("status", "--porcelain"), "")
        with self.assertRaises(ValueError):
            BUILD.source_identity(self.root)

    def test_uncommitted_import_cannot_use_parent_head(self):
        self.git("rm", "--cached", "-r", "components/cmsd")
        with self.assertRaises(ValueError):
            BUILD.source_identity(self.root)

    def test_content_change_alters_development_identity(self):
        file = self.root / "src/main.rs"
        file.write_text("one")
        before = BUILD.source_identity(self.root, True)
        file.write_text("two")
        after = BUILD.source_identity(self.root, True)
        self.assertNotEqual(before["build_revision"], after["build_revision"])

    def test_generated_output_and_unrelated_changes_do_not_dirty_component(self):
        before = BUILD.source_identity(self.root)
        (self.root / "target").mkdir()
        (self.root / "target/generated").write_text("output")
        (self.repository / "unrelated.txt").write_text("other work")
        self.assertEqual(before, BUILD.source_identity(self.root))

    def test_missing_tracked_source_rejected(self):
        (self.root / "src/main.rs").unlink()
        with self.assertRaises(ValueError):
            BUILD.source_identity(self.root)

    def test_external_source_symlink_rejected(self):
        (self.root / "src/external.rs").symlink_to(self.repository / "outside.rs")
        with self.assertRaisesRegex(ValueError, "symlinks"):
            BUILD.source_identity(self.root, True)

    def test_old_binary_or_wrong_platform_rejected(self):
        valid = {"component": "cmsd", "build_revision": "current", "os": "linux",
                 "architecture": "aarch64"}
        for changes in ({"build_revision": "old"}, {"architecture": "x86_64"}, {"os": "darwin"}):
            with patch.object(BUILD.subprocess, "check_output", return_value=json.dumps(valid | changes)):
                with self.assertRaisesRegex(ValueError, "identity/platform"):
                    BUILD.validate_binary_info(Path("/fixture/cmsd"), "current", "aarch64")
        with patch.object(BUILD.subprocess, "check_output", return_value=json.dumps(valid)):
            BUILD.validate_binary_info(Path("/fixture/cmsd"), "current", "aarch64")


if __name__ == "__main__":
    unittest.main()
