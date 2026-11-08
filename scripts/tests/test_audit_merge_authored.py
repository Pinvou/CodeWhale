import contextlib
import importlib.util
import io
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location(
    "audit_merge_authored", Path(__file__).resolve().parents[1] / "audit-merge-authored.py")
AUDIT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(AUDIT)


def original_report(base, git):
    """Compatibility oracle: the previous all-path, per-parent algorithm."""
    def tokens(parent, merge):
        result = {}
        for name in git("diff", "--name-only", parent, merge).splitlines():
            values = set()
            for line in git("diff", "--unified=0", parent, merge, "--", name).splitlines():
                if line.startswith("+") and not line.startswith("+++"):
                    values.update(AUDIT.TOKEN.findall(line))
            result[name] = values
        return result

    merges = git("rev-list", "--merges", f"{base}..HEAD").split()
    if not merges:
        print(f"no merge commits under {base}..HEAD")
        return
    findings = 0
    for merge in merges:
        left = tokens(git("rev-parse", f"{merge}^1").strip(), merge)
        right = tokens(git("rev-parse", f"{merge}^2").strip(), merge)
        for name in sorted(set(left) & set(right)):
            authored = left[name] & right[name]
            if authored:
                findings += len(authored)
                print(f"{merge[:9]} {name}: {', '.join(sorted(authored))}")
    if not findings:
        print(f"no merge-authored identifiers under {base}..HEAD")


class MergeAuthoredTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.command("init", "--quiet")
        self.command("config", "user.name", "Audit Fixture")
        self.command("config", "user.email", "fixture@example.invalid")
        self.command("config", "commit.gpgsign", "false")

    def command(self, *args, data=None):
        return subprocess.run(
            ["git", *args], cwd=self.root, input=data, capture_output=True,
            text=True, encoding="utf-8", check=True).stdout

    def commit(self, files, parents=()):
        # Build Git objects directly so quoted names also work on Windows.
        entries = []
        for name, content in sorted(files.items()):
            blob = self.command("hash-object", "-w", "--stdin", data=content).strip()
            entries.append(f"100644 blob {blob}\t{name}\0")
        tree = self.command("mktree", "-z", data="".join(entries)).strip()
        args = ["commit-tree", tree]
        for parent in parents:
            args.extend(["-p", parent])
        return self.command(*args, data="Controlled audit fixture\n").strip()

    def fixture(self, disjoint=False):
        original = {"shared.txt": "InitialIdentifier\n", "original.txt": "RenameContent\n"}
        base = self.commit(original)
        left_files = {**original, "left.txt": "LeftOnlyIdentifier\n"}
        right_files = {**original, "right.txt": "RightOnlyIdentifier\n"}
        if not disjoint:
            for files, value in ((left_files, "LeftParentIdentifier"),
                                 (right_files, "RightParentIdentifier")):
                files["shared.txt"] = value + "\n"
                files['quoted"name.txt'] = value + "\n"
                files["日本語.txt"] = value + "\n"
            right_files.pop("original.txt")
            right_files["renamed.txt"] = "RenameContent\n"
        left = self.commit(left_files, (base,))
        right = self.commit(right_files, (base,))
        if not disjoint:
            self.assertIn("R100\toriginal.txt\trenamed.txt", self.command(
                "diff", "--name-status", "--find-renames", base, right))
        combined = {**left_files, **right_files}
        if not disjoint:
            combined.pop("original.txt")
            combined["renamed.txt"] = "RenameContent\nRenameAuthoredIdentifier\n"
            for name in ("shared.txt", 'quoted"name.txt', "日本語.txt"):
                combined[name] = "ResolutionAuthoredIdentifier\n"
        merge = self.commit(combined, (left, right))
        self.command("update-ref", "HEAD", merge)
        return base

    def compare(self, base):
        old_calls, new_calls = [], []

        def counted(calls):
            def run(*args):
                calls.append(args)
                return self.command(*args)
            return run

        before, after = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(before):
            original_report(base, counted(old_calls))
        with patch.object(AUDIT, "BASE", base), patch.object(AUDIT, "git", counted(new_calls)):
            with contextlib.redirect_stdout(after):
                AUDIT.main()
        self.assertEqual(before.getvalue(), after.getvalue())
        self.assertLess(len(new_calls), len(old_calls))
        return after.getvalue(), new_calls

    def test_real_git_rename_quoted_unicode_and_authored_tokens_are_unchanged(self):
        base = self.fixture()
        for quoted_paths in ("true", "false"):
            with self.subTest(quote_path=quoted_paths):
                self.command("config", "core.quotePath", quoted_paths)
                output, calls = self.compare(base)
                self.assertIn("shared.txt: ResolutionAuthoredIdentifier", output)
                self.assertIn("renamed.txt: RenameAuthoredIdentifier", output)
                self.assertFalse(any(args[-1] in ("left.txt", "right.txt")
                                     for args in calls if "--unified=0" in args))
                if quoted_paths == "false":
                    self.assertIn("日本語.txt: ResolutionAuthoredIdentifier", output)

    def test_empty_path_intersection_never_runs_per_path_diffs(self):
        base = self.fixture(disjoint=True)
        output, calls = self.compare(base)
        self.assertEqual(f"no merge-authored identifiers under {base}..HEAD\n", output)
        self.assertFalse(any("--unified=0" in args for args in calls))

    def test_no_merges_preserves_message_and_single_git_call(self):
        base = self.commit({"file.txt": "InitialIdentifier\n"})
        self.command("update-ref", "HEAD", base)
        output = io.StringIO()
        with patch.object(AUDIT, "BASE", base), patch.object(AUDIT, "git", wraps=self.command) as git:
            with contextlib.redirect_stdout(output):
                AUDIT.main()
        self.assertEqual(f"no merge commits under {base}..HEAD\n", output.getvalue())
        self.assertEqual(1, git.call_count)


if __name__ == "__main__":
    unittest.main()
