import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/community-backflow-automation.yml"
PR_CHECK = ROOT / ".github/workflows/pr-check.yml"


class CommunityBackflowAutomationWorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = WORKFLOW.read_text(encoding="utf-8")
        cls.pr_check = PR_CHECK.read_text(encoding="utf-8")

    def test_events_use_default_branch_trusted_code(self):
        self.assertIn("workflow_run:", self.workflow)
        self.assertIn('workflows: ["PR Check"]', self.workflow)
        self.assertIn("pull_request_target:", self.workflow)
        self.assertIn("ref: ${{ github.event.repository.default_branch }}", self.workflow)
        self.assertIn("persist-credentials: false", self.workflow)
        self.assertNotIn("github.event.pull_request.head.sha", self.workflow)
        self.assertNotIn("github.event.workflow_run.head_sha }}", self.workflow)

    def test_permissions_and_explicit_opt_in_are_declared(self):
        for permission in (
            "checks: read",
            "contents: read",
            "issues: write",
            "pull-requests: write",
        ):
            self.assertIn(permission, self.workflow)
        self.assertNotIn("actions: read", self.workflow)
        self.assertNotIn("contents: write", self.workflow)
        self.assertIn(
            "COMMUNITY_BACKFLOW_AUTOMATION_ENABLED: "
            "${{ vars.COMMUNITY_BACKFLOW_AUTOMATION_ENABLED }}",
            self.workflow,
        )
        self.assertIn("--event-file \"$GITHUB_EVENT_PATH\"", self.workflow)

    def test_pr_check_routes_and_runs_automation_contract_tests(self):
        self.assertIn(
            "'.github/workflows/community-backflow-automation.yml'",
            self.pr_check,
        )
        self.assertGreaterEqual(
            self.pr_check.count("scripts/tests/community-backflow-automation.test.mjs"),
            2,
        )


if __name__ == "__main__":
    unittest.main()
