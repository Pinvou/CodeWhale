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

    def test_merge_queue_runs_the_layered_product_gate(self):
        self.assertIn("merge_group:", self.workflow)
        changes = self._job("changes", "fast-gate")
        self.assertIn("github.event_name == 'merge_group'", changes)
        self.assertIn("uses: dorny/paths-filter@v4", changes)

        required_gate = self.workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertIn("github.event_name == 'merge_group'", required_gate)
        self.assertIn("Merge Queue 基础检查失败", required_gate)
        self.assertNotIn("完整门禁已在 PR 入队前验证", required_gate)
        self.assertIn("backflow-portability", required_gate)

    def test_backflow_portability_rechecks_merge_group_on_three_platforms(self):
        portability = self._job("backflow-portability", "fast-gate")
        self.assertIn("os: [ubuntu-latest, windows-latest, macos-15]", portability)
        self.assertIn("github.event_name == 'pull_request'", portability)
        self.assertIn("github.event_name == 'merge_group'", portability)
        self.assertIn("github.event.merge_group.base_sha", portability)
        self.assertIn("github.event.merge_group.head_sha", portability)

    def test_trusted_backflow_uses_diff_aware_rust_policy(self):
        changes = self._job("changes", "fast-gate")
        self.assertIn("trusted_backflow:", changes)
        self.assertIn("backflow_mode:", changes)
        self.assertIn("pure_release_backflow:", changes)
        self.assertIn("python3 scripts/ci-rust-policy.py", changes)
        self.assertIn("GITHUB_TOKEN: ${{ github.token }}", changes)

        rust_test = self._job("rust-test", "windows-rust-test")
        self.assertIn(
            "needs.changes.outputs.trusted_backflow != 'true'", rust_test
        )
        self.assertIn("github.event_name == 'merge_group'", rust_test)
        self.assertIn(
            "contains(github.event.pull_request.labels.*.name, 'ci:full-rust')",
            rust_test,
        )

        rust_lint = self._job("rust-lint", "rust-test")
        self.assertIn("pure_release_backflow != 'true'", rust_lint)

        windows_rust_test = self._job("windows-rust-test", "windows-rustup-repair-test")
        self.assertIn("needs.changes.outputs.backflow_mode == 'windows_full'", windows_rust_test)
        self.assertIn("needs.changes.outputs.backflow_mode == 'full'", windows_rust_test)
        self.assertIn("needs.changes.outputs.backflow_mode == 'ordinary'", windows_rust_test)
        self.assertIn("needs.changes.outputs.rust_full == 'true'", windows_rust_test)

    def test_running_main_cache_warmer_and_cumulative_checks_are_preserved(self):
        concurrency = self.workflow.split("\nconcurrency:", maxsplit=1)[1].split(
            "\njobs:", maxsplit=1
        )[0]
        self.assertIn(
            "group: pr-check-${{ github.event.pull_request.number || github.ref }}",
            concurrency,
        )
        self.assertIn(
            "cancel-in-progress: ${{ github.event_name != 'push' }}", concurrency
        )

        rust_test = self._job("rust-test", "windows-rust-test")
        self.assertIn("github.event_name == 'push' ||", rust_test)
        self.assertIn("save-if: ${{ github.ref == 'refs/heads/main' }}", rust_test)

        windows_rust_test = self._job("windows-rust-test", "windows-rustup-repair-test")
        self.assertIn("github.event_name == 'push' ||", windows_rust_test)
        self.assertIn("github.event_name == 'pull_request'", windows_rust_test)
        self.assertIn("github.event.pull_request.draft == false", windows_rust_test)
        self.assertIn("needs.changes.outputs.rust_full == 'true'", windows_rust_test)
        self.assertIn("needs.changes.outputs.rust_code == 'true'", windows_rust_test)
        self.assertIn("github.event_name == 'merge_group'", windows_rust_test)
        self.assertIn("needs.changes.outputs.backflow_mode == 'full'", windows_rust_test)


if __name__ == "__main__":
    unittest.main()
