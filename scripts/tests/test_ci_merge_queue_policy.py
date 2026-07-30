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


if __name__ == "__main__":
    unittest.main()
