import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/pr-check.yml"


class MergeQueuePolicyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = WORKFLOW.read_text(encoding="utf-8")

    def _job(self, name, next_name):
        return self.workflow.split(f"\n  {name}:", maxsplit=1)[1].split(
            f"\n  {next_name}:", maxsplit=1
        )[0]

    def test_merge_queue_reuses_pr_required_gate(self):
        self.assertIn("merge_group:", self.workflow)
        for job, next_job in (
            ("commit-message", "version-consistency"),
            ("version-consistency", "changes"),
            ("changes", "fast-gate"),
            ("fast-gate", "frontend-test"),
        ):
            self.assertNotIn(
                "github.event_name == 'merge_group'",
                self._job(job, next_job),
                f"{job} 不应在 Merge Queue 重复运行",
            )

        required_gate = self.workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertIn("github.event_name == 'merge_group'", required_gate)
        self.assertIn("完整门禁已在 PR 入队前验证", required_gate)
        self.assertIn('if [[ "$GITHUB_EVENT_NAME" == "merge_group" ]]', required_gate)

    def test_backflow_rust_changes_defer_full_test_to_main_push(self):
        rust_test = self._job("rust-test", "windows-rust-test")
        self.assertIn("needs.changes.outputs.rust == 'true'", rust_test)
        self.assertIn(
            "github.event.pull_request.head.repo.full_name != github.repository",
            rust_test,
        )
        self.assertIn(
            "startsWith(github.head_ref, 'backflow/community-') == false",
            rust_test,
        )
        self.assertIn("push main:始终完整跑", rust_test)
        self.assertIn("只保留最新 main 的一轮验证", rust_test)

        concurrency = self.workflow.split("\nconcurrency:", maxsplit=1)[1].split(
            "\njobs:", maxsplit=1
        )[0]
        self.assertIn(
            "group: pr-check-${{ github.event.pull_request.number || github.ref }}",
            concurrency,
        )
        self.assertIn("cancel-in-progress: true", concurrency)

        rust_lint = self._job("rust-lint", "rust-test")
        self.assertNotIn("backflow/community-", rust_lint)
        self.assertNotIn("edited", self.workflow)


if __name__ == "__main__":
    unittest.main()
