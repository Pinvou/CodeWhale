import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PR_WORKFLOW = ROOT / ".github/workflows/pr-check.yml"


class CiGatePolicyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = PR_WORKFLOW.read_text(encoding="utf-8")

    def test_pr_modes_and_stacked_pr_triggers_are_explicit(self):
        trigger = self.workflow.split("\non:", maxsplit=1)[1].split(
            "\npermissions:", maxsplit=1
        )[0]
        pull_request_trigger = trigger.split("\n  pull_request:", maxsplit=1)[
            1
        ].split("\n  merge_group:", maxsplit=1)[0]
        active_trigger = "\n".join(
            line
            for line in pull_request_trigger.splitlines()
            if not line.lstrip().startswith("#")
        )
        self.assertNotIn("branches:", active_trigger)
        self.assertIn("ready_for_review", pull_request_trigger)
        self.assertIn("converted_to_draft", pull_request_trigger)

        frontend = self.workflow.split("\n  frontend-test:", maxsplit=1)[1].split(
            "\n  relay-test:", maxsplit=1
        )[0]
        self.assertIn("github.event.pull_request.draft == false", frontend)
        self.assertIn("Ready PR 定向浏览器 smoke", frontend)
        self.assertIn("Merge Queue 完整浏览器 smoke", frontend)
        self.assertIn("select-frontend-smokes.mjs", frontend)

    def test_merge_queue_uses_path_filtering_and_product_gates(self):
        changes = self.workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  fast-gate:", maxsplit=1
        )[0]
        self.assertIn("uses: dorny/paths-filter@v4", changes)
        self.assertIn("github.event_name == 'merge_group'", changes)
        for output in (
            "rust_code",
            "rust_dependencies",
            "release_contract",
            "frontend",
            "relay",
            "acp_runtime",
            "windows_codex",
            "windows_runtime",
            "windows_rustup_repair",
        ):
            self.assertIn(
                f"{output}: ${{{{ steps.filter.outputs.{output} }}}}",
                changes,
            )

        required_gate = self.workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertIn("Merge Queue 基础检查失败", required_gate)
        for job in (
            "release-contract-test",
            "windows-runtime-test",
            "windows-rustup-repair-test",
        ):
            self.assertIn(f"- {job}", required_gate)
            self.assertIn(f'"{job}:$', required_gate)

    def test_rust_modes_preserve_fast_drafts_and_queue_validation(self):
        rust_lint = self.workflow.split("\n  rust-lint:", maxsplit=1)[1].split(
            "\n  rust-test:", maxsplit=1
        )[0]
        self.assertIn("RUN_HEAVY_RUST_CHECKS", rust_lint)
        self.assertIn("github.event.pull_request.draft == false", rust_lint)
        self.assertIn("needs.changes.outputs.rust_dependencies == 'true'", rust_lint)

        rust_test = self.workflow.split("\n  rust-test:", maxsplit=1)[1].split(
            "\n  windows-rust-test:", maxsplit=1
        )[0]
        self.assertIn("github.event_name == 'merge_group'", rust_test)
        self.assertIn("needs.changes.outputs.rust_code == 'true'", rust_test)
        self.assertIn("github.event.pull_request.draft == false", rust_test)
        self.assertIn(
            "contains(github.event.pull_request.labels.*.name, 'ci:full-rust')",
            rust_test,
        )
        self.assertIn("github.event_name == 'push'", rust_test)

    def test_release_and_platform_contracts_skip_drafts_but_run_in_queue(self):
        release_contract = self.workflow.split(
            "\n  release-contract-test:", maxsplit=1
        )[1].split("\n  rust-lint:", maxsplit=1)[0]
        self.assertIn("needs.changes.outputs.release_contract == 'true'", release_contract)
        self.assertIn("github.event.pull_request.draft == false", release_contract)
        self.assertNotIn("github.event_name != 'merge_group'", release_contract)

        for start, end in (
            ("windows-codex-runtime-test", "windows-runtime-test"),
            ("windows-runtime-test", "macos-codex-runtime-test"),
            ("macos-codex-runtime-test", "required-gate"),
        ):
            block = self.workflow.split(f"\n  {start}:", maxsplit=1)[1].split(
                f"\n  {end}:", maxsplit=1
            )[0]
            self.assertIn("github.event.pull_request.draft == false", block)


if __name__ == "__main__":
    unittest.main()
