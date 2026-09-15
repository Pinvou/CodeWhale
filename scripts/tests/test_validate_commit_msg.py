from __future__ import annotations

import importlib.util
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
        with patch.object(validate_commit_msg, "git") as git_mock:
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

    def test_range_still_rejects_new_nonconforming_commit(self) -> None:
        with patch.object(validate_commit_msg, "git") as git_mock:
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

    def test_range_rejects_missing_signoff(self) -> None:
        with patch.object(validate_commit_msg, "git") as git_mock:
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
        with patch.object(validate_commit_msg, "git") as git_mock:
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
        with patch.object(validate_commit_msg, "git") as git_mock:
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
        with patch.object(validate_commit_msg, "git") as git_mock:
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
        with patch.object(validate_commit_msg, "git") as git_mock:
            git_mock.return_value = self.commit_record(
                "fedcba9876543210",
                "parent",
                "dependabot[bot]",
                "49699333+dependabot[bot]@users.noreply.github.com",
                "fix: 修复依赖版本",
            )

            self.assertEqual(validate_commit_msg.validate_range("base", "head"), [])


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
