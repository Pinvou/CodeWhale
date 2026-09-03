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
        self.assertIn("npm run test:browser-smoke", frontend)
        self.assertEqual(frontend.count("npm run test:markdown"), 0)

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
            "team_relay_hub",
            "account_platform",
            "acp_runtime",
            "windows_codex",
            "windows_runtime",
            "windows_rustup_repair",
            "backflow_infra",
        ):
            self.assertIn(
                f"{output}: ${{{{ steps.filter.outputs.{output} }}}}",
                changes,
            )

        required_gate = self.workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertIn("Merge Queue 基础检查失败", required_gate)
        for job in (
            "team-relay-hub-test",
            "account-platform-test",
            "backflow-portability",
            "release-contract-test",
            "windows-runtime-test",
            "windows-rustup-repair-test",
        ):
            self.assertIn(f"- {job}", required_gate)
            self.assertIn(f'"{job}:$', required_gate)

        hub_job = self.workflow.split(
            "\n  team-relay-hub-test:", maxsplit=1
        )[1].split("\n  release-contract-test:", maxsplit=1)[0]
        self.assertIn("needs.changes.outputs.team_relay_hub == 'true'", hub_job)
        self.assertIn("postgres:16-alpine", hub_job)
        self.assertIn("TEAM_RELAY_TEST_DATABASE_URL", hub_job)
        self.assertIn("npm --prefix team-relay-hub ci", hub_job)
        self.assertIn("npm --prefix team-relay-hub test", hub_job)
        self.assertIn(
            "npm --prefix team-relay-hub audit "
            "--registry=https://registry.npmjs.org --audit-level=high",
            hub_job,
        )

        account_job = self.workflow.split(
            "\n  account-platform-test:", maxsplit=1
        )[1].split("\n  release-contract-test:", maxsplit=1)[0]
        self.assertIn("needs.changes.outputs.account_platform == 'true'", account_job)
        self.assertIn("postgres:17-alpine", account_job)
        self.assertIn("go test -short -race ./...", account_job)
        self.assertIn("PINVOU_TEST_TOKEN_PAGINATION_GATE=1 go test", account_job)
        self.assertIn("PINVOU_TEST_TOKEN_DATABASE_URL", account_job)
        self.assertIn("buf lint account-platform/contracts/proto", account_job)
        self.assertIn("account_platform_test_${domain//-/_}", account_job)
        self.assertIn("token-portal", account_job)
        self.assertIn("account-event-relay", account_job)
        self.assertIn("alpine/helm:4.2.3", account_job)
        self.assertIn("govulncheck@v1.6.0", account_job)
        self.assertIn("docker compose", account_job)

    def test_trusted_backflow_has_parallel_three_platform_light_gate(self):
        portability = self.workflow.split(
            "\n  backflow-portability:", maxsplit=1
        )[1].split("\n  fast-gate:", maxsplit=1)[0]
        self.assertIn("needs.changes.outputs.trusted_backflow == 'true'", portability)
        self.assertIn("needs.changes.outputs.backflow_infra == 'true'", portability)
        self.assertIn("os: [ubuntu-latest, windows-latest, macos-15]", portability)
        self.assertIn("runs-on: ${{ matrix.os }}", portability)
        self.assertIn("fail-fast: false", portability)
        self.assertIn("scripts/community-backflow.mjs preflight", portability)
        self.assertIn("scripts/community-backflow.mjs verify", portability)
        self.assertIn(
            "github.event_name == 'pull_request' && needs.changes.outputs.trusted_backflow == 'true'",
            portability,
        )
        self.assertNotIn("npm ci", portability)
        self.assertNotIn("cargo ", portability)
        self.assertNotIn("submodule update", portability)

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
