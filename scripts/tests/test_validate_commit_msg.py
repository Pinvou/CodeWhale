from __future__ import annotations

import importlib.util
import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "validate-commit-msg.py"
WORKFLOW = Path(__file__).resolve().parents[2] / ".github/workflows/pr-check.yml"
SPEC = importlib.util.spec_from_file_location("validate_commit_msg", SCRIPT)
assert SPEC and SPEC.loader
validate_commit_msg = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(validate_commit_msg)


class ValidateCommitTextTests(unittest.TestCase):
    def test_github_pr_suffix_is_not_counted_towards_description_limit(self) -> None:
        description = "修" * 50

        self.assertEqual(
            validate_commit_msg.validate_text(
                f"fix: {description} (#191)",
                "squash commit",
            ),
            [],
        )

    def test_description_over_limit_is_rejected_before_github_pr_suffix(self) -> None:
        description = "修" * 51

        errors = validate_commit_msg.validate_text(
            f"fix: {description} (#191)",
            "squash commit",
        )

        self.assertEqual(len(errors), 1)
        self.assertIn("description must be 50 characters or fewer, got 51", errors[0])

    def test_only_terminal_github_pr_suffix_is_excluded(self) -> None:
        description = f"{'修' * 50} (#191) 后续"

        errors = validate_commit_msg.validate_text(
            f"fix: {description}",
            "regular commit",
        )

        self.assertEqual(len(errors), 1)
        self.assertIn(
            f"description must be 50 characters or fewer, got {len(description)}",
            errors[0],
        )

    def test_github_pr_suffix_does_not_hide_forbidden_punctuation(self) -> None:
        errors = validate_commit_msg.validate_text(
            "fix: 修复权限声明。 (#191)",
            "squash commit",
        )

        self.assertEqual(len(errors), 1)
        self.assertIn("description must not end with punctuation", errors[0])

    def test_github_pr_suffix_does_not_hide_vague_description(self) -> None:
        errors = validate_commit_msg.validate_text(
            "fix: 修改代码 (#191)",
            "squash commit",
        )

        self.assertEqual(len(errors), 1)
        self.assertIn("description is too vague: 修改代码", errors[0])

    def test_empty_description_after_pr_suffix_fails_cleanly(self) -> None:
        # `fix:  (#1)`: the GitHub suffix stripper leaves an empty description;
        # the guard must report a validation error, not crash with IndexError
        # (the pre-#1518 behavior that broke the commit-message CI leg).
        errors = validate_commit_msg.validate_text(
            "fix:  (#1)",
            "squash commit",
        )

        self.assertNotEqual(errors, [])
        self.assertIn("description must use Chinese", errors[0])

    def test_subject_only_file_accepts_pr_title_without_signoff(self) -> None:
        with patch.object(Path, "read_text", return_value="ci(pr): 修复标题校验"):
            errors = validate_commit_msg.validate_file(
                Path("pr-title"),
                require_signoff=False,
            )

        self.assertEqual(errors, [])


class ValidateCommitRangeTests(unittest.TestCase):
    @staticmethod
    def commit_record(
        commit: str,
        parents: str,
        author_name: str,
        author_email: str,
        message: str,
    ) -> str:
        return "\0".join(
            (commit, parents, author_name, author_email, message)
        ) + "\x1e"

    def test_range_grandfathers_legacy_history_and_skips_merge_subject(self) -> None:
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[True, True]
            ),
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "0123456789abcdef",
                "parent-one parent-two",
                "Test User",
                "test@example.com",
                "Merge branch 'main'",
            )

            self.assertEqual(validate_commit_msg.validate_range("base", "head"), [])

        git_mock.assert_called_once_with(
            "log",
            "--format=%H%x00%P%x00%an%x00%ae%x00%B%x1e",
            "base..head",
            "--not",
            validate_commit_msg.LEGACY_HISTORY_CUTOFF,
        )

    def test_range_skips_cutoff_exclusion_when_cutoff_missing(self) -> None:
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[True, False]
            ),
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "0123456789abcdef",
                "parent",
                "Test User",
                "test@example.com",
                "fix: 修复提交门禁范围\n\nSigned-off-by: Test User <test@example.com>",
            )

            self.assertEqual(validate_commit_msg.validate_range("base", "head"), [])

        git_mock.assert_called_once_with(
            "log",
            "--format=%H%x00%P%x00%an%x00%ae%x00%B%x1e",
            "base..head",
        )

    def test_missing_base_validates_head_alone_in_shallow_clone(self) -> None:
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[False, False]
            ),
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "0123456789abcdef",
                "parent",
                "Test User",
                "test@example.com",
                "fix: 修复提交门禁范围\n\nSigned-off-by: Test User <test@example.com>",
            )

            self.assertEqual(
                validate_commit_msg.validate_range("missing-base", "head"), []
            )

        # `head^!` selects head and excludes its parents: a bare `head` would
        # walk every reachable ancestor and mis-flag old nonconforming
        # commits the legacy cutoff would have grandfathered (the round-5
        # review regression for the shallow-clone fallback).
        git_mock.assert_called_once_with(
            "log",
            "--format=%H%x00%P%x00%an%x00%ae%x00%B%x1e",
            "head^!",
        )

    def test_missing_base_with_present_cutoff_still_excludes_cutoff(self) -> None:
        # Shallow-clone combo (F, T): base absent but the legacy cutoff object
        # present — validate `head` alone while still excluding the cutoff.
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[False, True]
            ),
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "0123456789abcdef",
                "parent",
                "Test User",
                "test@example.com",
                "fix: 修复提交门禁范围\n\nSigned-off-by: Test User <test@example.com>",
            )

            self.assertEqual(
                validate_commit_msg.validate_range("missing-base", "head"), []
            )

        git_mock.assert_called_once_with(
            "log",
            "--format=%H%x00%P%x00%an%x00%ae%x00%B%x1e",
            "head^!",
            "--not",
            validate_commit_msg.LEGACY_HISTORY_CUTOFF,
        )

    def test_range_still_rejects_new_nonconforming_commit(self) -> None:
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[True, True]
            ),
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "fedcba9876543210",
                "parent",
                "Test User",
                "test@example.com",
                "fix: english only\n\nSigned-off-by: Test User <test@example.com>",
            )

            errors = validate_commit_msg.validate_range("base", "head")

        self.assertTrue(errors)
        self.assertIn("description must use Chinese", errors[0])

    def test_shallow_boundary_merge_keeps_exemption(self) -> None:
        # Round-12 review: at a shallow boundary git grafts the boundary
        # commit's parents away — `git log --format=%P` reports NO parents
        # even for a merge (verified on a depth-1 clone of a merge head,
        # while `git cat-file` shows both), so without the raw-object check
        # a merge head loses its merge exemption and a GitHub auto-merge
        # commit's subject/DCO gets validated. The raw parent check only
        # arms in a shallow clone (.git/shallow), so full clones never pay
        # the extra plumbing calls.
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[False, False]
            ),
            patch.object(
                validate_commit_msg, "_in_shallow_clone", return_value=True
            ),
            patch.object(
                validate_commit_msg, "_raw_parents", return_value=["p1", "p2"]
            ) as raw_parents,
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "0123456789abcdef",
                "",  # grafted away at the shallow boundary
                "GitHub",
                "noreply@github.com",
                "Merge pull request #1 from branch",
            )

            self.assertEqual(
                validate_commit_msg.validate_range("missing-base", "head"), []
            )
        raw_parents.assert_called_once_with("0123456789abcdef")

    def test_shallow_boundary_non_merge_still_validated(self) -> None:
        # The raw-object check must not become a blanket exemption: a
        # shallow-boundary single-parent commit (raw object agrees with %P)
        # is still validated, and the raw check runs exactly once.
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[False, False]
            ),
            patch.object(
                validate_commit_msg, "_in_shallow_clone", return_value=True
            ),
            patch.object(
                validate_commit_msg, "_raw_parents", return_value=["p1"]
            ),
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "fedcba9876543210",
                "parent",
                "Test User",
                "test@example.com",
                "fix: 修复提交门禁范围",
            )

            errors = validate_commit_msg.validate_range("missing-base", "head")

        self.assertEqual(len(errors), 1)
        self.assertIn("missing Signed-off-by", errors[0])

    def test_range_rejects_missing_signoff(self) -> None:
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[True, True]
            ),
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "fedcba9876543210",
                "parent",
                "Test User",
                "test@example.com",
                "fix: 修复提交门禁范围",
            )

            errors = validate_commit_msg.validate_range("base", "head")

        self.assertEqual(len(errors), 1)
        self.assertIn("missing Signed-off-by", errors[0])

    def test_range_rejects_signoff_from_another_identity(self) -> None:
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[True, True]
            ),
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "fedcba9876543210",
                "parent",
                "Test User",
                "test@example.com",
                "fix: 修复提交门禁范围\n\n"
                "Signed-off-by: Another User <another@example.com>",
            )

            errors = validate_commit_msg.validate_range("base", "head")

        self.assertEqual(len(errors), 1)
        self.assertIn("must match commit author", errors[0])

    def test_range_accepts_rewritten_squash_author_when_enabled(self) -> None:
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[True, True]
            ),
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "fedcba9876543210",
                "parent",
                "GitHub Squash Author",
                "rewritten@example.com",
                "fix: 修复提交门禁范围\n\n"
                "Signed-off-by: Original Author <original@example.com>",
            )

            errors = validate_commit_msg.validate_range(
                "base",
                "head",
                require_matching_signoff=False,
            )

        self.assertEqual(errors, [])

    def test_range_still_requires_signoff_when_author_matching_is_disabled(self) -> None:
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[True, True]
            ),
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "fedcba9876543210",
                "parent",
                "GitHub Squash Author",
                "rewritten@example.com",
                "fix: 修复提交门禁范围",
            )

            errors = validate_commit_msg.validate_range(
                "base",
                "head",
                require_matching_signoff=False,
            )

        self.assertEqual(len(errors), 1)
        self.assertIn("missing Signed-off-by", errors[0])

    def test_range_exempts_trusted_dependabot_identity(self) -> None:
        with (
            patch.object(
                validate_commit_msg, "git_commit_exists", side_effect=[True, True]
            ),
            patch.object(validate_commit_msg, "git") as git_mock,
        ):
            git_mock.return_value = self.commit_record(
                "fedcba9876543210",
                "parent",
                "dependabot[bot]",
                "49699333+dependabot[bot]@users.noreply.github.com",
                "fix: 修复依赖版本",
            )

            self.assertEqual(validate_commit_msg.validate_range("base", "head"), [])


class HistoricalSquashSignoffTests(unittest.TestCase):
    commit_record = staticmethod(ValidateCommitRangeTests.commit_record)
    message = "fix: 修复历史提交签署校验\n\nSigned-off-by: z27014 <zhuo.wenpei@h3c.com>"

    def check_record(self, sha: str, *, author=None, message=None, **kwargs):
        author = author or validate_commit_msg.HISTORICAL_SQUASH_AUTHOR
        record = self.commit_record(
            sha, "parent", *author, self.message if message is None else message
        )
        with patch.object(validate_commit_msg, "git", return_value=record):
            return validate_commit_msg.validate_range("base", "head", **kwargs)

    def test_registry_is_bounded_to_19_full_object_ids_with_source_prs(self):
        registry = validate_commit_msg.HISTORICAL_SQUASH_SIGNOFFS
        self.assertEqual(len(registry), 19)
        self.assertEqual(len(set(registry.values())), 19)
        for sha, pr in registry.items():
            self.assertRegex(sha, re.compile(r"^[0-9a-f]{40}$"))
            self.assertIsInstance(pr, int)

    def test_all_registered_objects_still_require_original_signer_and_title(self):
        for sha in validate_commit_msg.HISTORICAL_SQUASH_SIGNOFFS:
            with self.subTest(sha=sha):
                self.assertEqual(self.check_record(sha), [])
                for message in (
                    "fix: 修复历史提交签署校验",
                    self.message.replace("z27014", "wrong-signer"),
                    self.message.replace("zhuo.wenpei@h3c.com", "other@example.com"),
                    self.message.replace("修复历史提交签署校验", "english only"),
                ):
                    self.assertTrue(self.check_record(sha, message=message))
                self.assertTrue(self.check_record(
                    sha, message="fix: 修复历史提交签署校验",
                    require_matching_signoff=False,
                ))
                self.assertTrue(self.check_record(
                    sha, message=self.message.replace("z27014", "wrong-signer"),
                    require_matching_signoff=False,
                ))

    def test_registered_author_anomalies_fail_even_if_self_signed_or_bot(self):
        for sha in validate_commit_msg.HISTORICAL_SQUASH_SIGNOFFS:
            for author in (
                validate_commit_msg.HISTORICAL_SQUASH_SIGNER,
                ("other", "other@example.com"),
                ("dependabot[bot]", "49699333+dependabot[bot]@users.noreply.github.com"),
            ):
                with self.subTest(sha=sha, author=author):
                    self.assertTrue(self.check_record(sha, author=author))
                    self.assertTrue(self.check_record(
                        sha, author=author,
                        message=f"fix: 修复历史提交签署校验\n\nSigned-off-by: {author[0]} <{author[1]}>",
                    ))

    def test_unknown_and_abbreviated_object_ids_do_not_create_identity_alias(self):
        for sha in validate_commit_msg.HISTORICAL_SQUASH_SIGNOFFS:
            with self.subTest(sha=sha):
                self.assertTrue(self.check_record(sha[:12]))
                self.assertTrue(self.check_record(sha + "0"))
        self.assertTrue(self.check_record("0" * 40))
        self.assertEqual(self.check_record(
            "0" * 40, author=validate_commit_msg.HISTORICAL_SQUASH_SIGNER
        ), [])

    def test_real_git_recreated_commit_does_not_inherit_historical_exception(self):
        # A real object ID changes when any message/content/parent changes. No
        # network or repository history is needed, so this also runs in CI clones.
        with tempfile.TemporaryDirectory() as directory:
            env = dict(os.environ, GIT_AUTHOR_NAME="zhuowp",
                       GIT_AUTHOR_EMAIL="zwp-1989@qq.com",
                       GIT_COMMITTER_NAME="Fixture", GIT_COMMITTER_EMAIL="fixture@example.com",
                       GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)

            def git(*args, input=None):
                return subprocess.check_output(
                    ("git", "-C", directory, *args), input=input,
                    text=True, encoding="utf-8", env=env,
                ).strip()

            git("init", "--quiet")
            tree = git("hash-object", "-t", "tree", "--stdin", input="")
            base = git("commit-tree", tree, input="fixture root")
            original = git("commit-tree", tree, "-p", base, input=self.message)
            recreated = git("commit-tree", tree, "-p", base, input=self.message + "\n\n新提交")
            self.assertNotEqual(original, recreated)
            with (
                patch.object(validate_commit_msg, "git", side_effect=git),
                patch.object(validate_commit_msg, "LEGACY_HISTORY_CUTOFF", base),
                patch.object(validate_commit_msg, "HISTORICAL_SQUASH_SIGNOFFS", {original: 1829}),
            ):
                self.assertEqual(validate_commit_msg.validate_range(base, original), [])
                errors = validate_commit_msg.validate_range(base, recreated)
                self.assertEqual(len(errors), 1)
                self.assertIn("must match commit author", errors[0])


class CommitMessageWorkflowTests(unittest.TestCase):
    def test_main_push_allows_squash_author_rewrite_only_after_pr_validation(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")

        self.assertIn(
            'python3 scripts/validate-commit-msg.py --range '
            '"${{ github.event.pull_request.base.sha }}" '
            '"${{ github.event.pull_request.head.sha }}"',
            workflow,
        )
        self.assertIn(
            'python3 scripts/validate-commit-msg.py --range "$before" '
            '"${{ github.sha }}" --allow-signoff-author-mismatch',
            workflow,
        )
        self.assertNotIn(
            '"${{ github.event.pull_request.head.sha }}" '
            "--allow-signoff-author-mismatch",
            workflow,
        )


if __name__ == "__main__":
    unittest.main()
