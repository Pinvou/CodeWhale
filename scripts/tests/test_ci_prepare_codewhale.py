"""Real Git histories/checkouts; no mocked git commands or public downloads."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
BASH = os.environ.get("PINVOU_TEST_BASH") or shutil.which("bash")


class PrepareCodeWhale(unittest.TestCase):
    def git(self, where, *args):
        return subprocess.check_output(["git", "-C", str(where), *args], text=True, stderr=subprocess.PIPE).strip()

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="cw-ci-prepare-")
        self.addCleanup(self.temporary.cleanup)
        base = Path(self.temporary.name)
        self.source, self.root = base / "public", base / "workspace"
        self.source.mkdir()
        self.root.mkdir()
        for folder in (self.source, self.root):
            self.git(folder, "init", "-q")
            self.git(folder, "config", "user.name", "CI fixture")
            self.git(folder, "config", "user.email", "fixture@example.invalid")
        (self.source / "tracked").write_text("first\n")
        self.git(self.source, "add", "tracked")
        self.git(self.source, "commit", "-qm", "fixture first")
        self.ancestor = self.git(self.source, "rev-parse", "HEAD")
        (self.source / "tracked").write_text("registered\n")
        self.git(self.source, "commit", "-qam", "fixture registered")
        self.head = self.git(self.source, "rev-parse", "HEAD")
        (self.root / "scripts").mkdir()
        shutil.copyfile(ROOT / "scripts/ci-prepare-codewhale.sh", self.root / "scripts/ci-prepare-codewhale.sh")
        (self.root / ".gitmodules").write_text('[submodule "CodeWhale"]\n\tpath = CodeWhale\n\turl = https://github.com/Pinvou/CodeWhale.git\n')
        self.git(self.root, "add", ".gitmodules")
        self.git(self.root, "update-index", "--add", "--cacheinfo", "160000," + self.head + ",CodeWhale")
        self.environment = dict(os.environ, GITHUB_ACTIONS="true", GITHUB_WORKSPACE=self.root.as_posix(),
                                GIT_CONFIG_COUNT="2", GIT_CONFIG_KEY_0="url." + self.source.as_uri() + ".insteadOf",
                                GIT_CONFIG_VALUE_0="https://github.com/Pinvou/CodeWhale.git",
                                GIT_CONFIG_KEY_1="protocol.file.allow", GIT_CONFIG_VALUE_1="always",
                                GIT_TERMINAL_PROMPT="0")
        self.checkout = self.root / "CodeWhale"

    def run_prepare(self, *, history=True, **changes):
        self.assertTrue(BASH, "Git Bash or Bash is required to run the CI checkout contract")
        arguments = [BASH, "scripts/ci-prepare-codewhale.sh"] + (["--history"] if history else [])
        return subprocess.run(arguments, cwd=self.root,
                              env={**self.environment, **changes}, text=True, stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, timeout=60)

    def clone_shallow(self):
        subprocess.run(["git", "clone", "-q", "--depth=1", self.source.as_uri(), str(self.checkout)], check=True)
        self.assertEqual(self.git(self.checkout, "rev-parse", "--is-shallow-repository"), "true")

    def assert_ready(self, result):
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("CODEWHALE_CI_READY head=" + self.head, result.stdout)
        self.assertEqual(self.git(self.checkout, "rev-parse", "HEAD"), self.head)
        self.assertEqual(self.git(self.checkout, "rev-parse", "--is-shallow-repository"), "false")
        self.assertEqual(self.git(self.checkout, "rev-list", "--count", self.ancestor + "..HEAD"), "1")
        self.assertEqual(self.git(self.checkout, "status", "--porcelain"), "")
        self.assertEqual((self.checkout / "tracked").read_text(), "registered\n")

    def test_initializes_exact_indexed_commit(self):
        self.assert_ready(self.run_prepare())

    def test_restores_dirty_shallow_checkout_and_removes_untracked_files(self):
        self.clone_shallow()
        (self.checkout / "tracked").write_text("stale runner content\n")
        (self.checkout / "untracked").write_text("stale generated file\n")
        self.assert_ready(self.run_prepare())
        self.assertFalse((self.checkout / "untracked").exists())

    def test_refuses_local_or_other_workspace_before_mutating(self):
        self.clone_shallow()
        (self.checkout / "tracked").write_text("preserve local work\n")
        for changes in ({"GITHUB_ACTIONS": "false"}, {"GITHUB_WORKSPACE": self.source.as_posix()}):
            result = self.run_prepare(**changes)
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertEqual((self.checkout / "tracked").read_text(), "preserve local work\n")
            self.assertEqual(self.git(self.checkout, "rev-parse", "--is-shallow-repository"), "true")

    def test_refuses_wrong_public_url_before_mutating(self):
        self.clone_shallow()
        self.git(self.root, "config", "-f", ".gitmodules", "submodule.CodeWhale.url", "https://example.invalid/other.git")
        result = self.run_prepare()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(self.git(self.checkout, "rev-parse", "--is-shallow-repository"), "true")

    def test_tree_only_preparation_does_not_download_unneeded_history(self):
        self.clone_shallow()
        (self.checkout / "tracked").write_text("stale\n")
        result = self.run_prepare(history=False)
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(self.git(self.checkout, "rev-parse", "--is-shallow-repository"), "true")
        self.assertEqual((self.checkout / "tracked").read_text(), "registered\n")

    def test_explicit_checkout_overrides_stale_local_update_strategy(self):
        self.clone_shallow()
        self.git(self.root, "config", "submodule.CodeWhale.update", "none")
        (self.checkout / "tracked").write_text("stale runner content\n")
        self.assert_ready(self.run_prepare())

    def assert_foreign_child_preserved(self, metadata):
        self.clone_shallow()
        shutil.move(str(self.checkout / ".git"), metadata)
        (self.checkout / ".git").write_text("gitdir: " + metadata.as_posix() + "\n")
        self.git(self.checkout, "config", "core.worktree", self.checkout.as_posix())
        (self.checkout / "staged-sentinel").write_text("preserve this stage\n")
        self.git(self.checkout, "add", "staged-sentinel")
        before = {name: (metadata / name).read_bytes() for name in ("HEAD", "index", "config")}
        result = self.run_prepare()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("Mutable Git metadata", result.stdout)
        self.assertEqual(before, {name: (metadata / name).read_bytes() for name in before})
        self.assertIn("staged-sentinel", self.git(self.checkout, "diff", "--cached", "--name-only"))

    def test_refuses_foreign_child_gitdir_before_any_metadata_changes(self):
        self.assert_foreign_child_preserved(self.root.parent / "foreign-child.git")

    def test_refuses_foreign_child_gitdir_inside_the_workspace(self):
        self.assert_foreign_child_preserved(self.root / "other-repository.git")

    def assert_foreign_parent_preserved(self, metadata):
        shutil.move(str(self.root / ".git"), metadata)
        (self.root / ".git").write_text("gitdir: " + metadata.as_posix() + "\n")
        self.git(self.root, "config", "core.worktree", self.root.as_posix())
        before = {name: (metadata / name).read_bytes() for name in ("HEAD", "index", "config")}
        result = self.run_prepare()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("Mutable Git metadata", result.stdout)
        self.assertEqual(before, {name: (metadata / name).read_bytes() for name in before})
        self.assertFalse(self.checkout.exists())

    def test_refuses_foreign_parent_gitdir_before_sync(self):
        self.assert_foreign_parent_preserved(self.root.parent / "foreign-parent.git")

    def test_refuses_foreign_parent_gitdir_inside_the_workspace(self):
        self.assert_foreign_parent_preserved(self.root / "other-parent.git")

    def test_refuses_foreign_common_directory_before_any_metadata_changes(self):
        self.clone_shallow()
        self.git(self.checkout, "config", "core.worktree", self.checkout.as_posix())
        metadata = self.checkout / ".git"
        foreign = self.root.parent / "foreign-common.git"
        shutil.copytree(metadata, foreign)
        (metadata / "commondir").write_text(foreign.as_posix() + "\n")
        before = {(folder, name): (folder / name).read_bytes()
                  for folder in (metadata, foreign) for name in ("HEAD", "index", "config")}
        result = self.run_prepare()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("Mutable Git metadata", result.stdout)
        self.assertEqual(before, {(folder, name): (folder / name).read_bytes() for folder, name in before})

    def test_refuses_mutable_index_override_before_touching_it(self):
        foreign = self.root.parent / "external-index"
        parent_index = self.root / ".git/index"
        original_parent_index = parent_index.read_bytes()
        (self.root / "index-sentinel").write_text("preserve this stage\n")
        self.git(self.root, "add", "index-sentinel")
        original = parent_index.read_bytes()
        foreign.write_bytes(original)
        parent_index.write_bytes(original_parent_index)
        result = self.run_prepare(GIT_INDEX_FILE=foreign.as_posix())
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("Repository path overrides", result.stdout)
        self.assertEqual(foreign.read_bytes(), original)
        self.assertFalse(self.checkout.exists())

    def test_allows_immutable_shared_object_alternates(self):
        self.git(self.root, "clone", "-q", "--shared", self.source.as_posix(), self.checkout.as_posix())
        alternates = self.checkout / ".git/objects/info/alternates"
        self.assertTrue(alternates.is_file())
        before = {name: (self.source / ".git" / name).read_bytes() for name in ("HEAD", "index", "config")}
        self.assert_ready(self.run_prepare())
        self.assertEqual(before, {name: (self.source / ".git" / name).read_bytes() for name in before})

    def test_reuses_owned_module_after_removing_working_tree(self):
        self.assert_ready(self.run_prepare())
        shutil.rmtree(self.checkout)
        self.assert_ready(self.run_prepare())


if __name__ == "__main__":
    unittest.main()
