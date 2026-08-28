import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PR_WORKFLOW = ROOT / ".github/workflows/pr-check.yml"
RELEASE_WORKFLOW = ROOT / ".github/workflows/release-packages.yml"
MAC_BUILD_WORKFLOW = ROOT / ".github/workflows/mac-build.yml"
PUBLIC_SUBMODULE_VERIFIER = ROOT / "scripts/verify-public-submodule.sh"


def _without_yaml_comments(block):
    return "\n".join(
        line for line in block.splitlines() if not line.lstrip().startswith("#")
    )


class CiGatePolicyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = PR_WORKFLOW.read_text(encoding="utf-8")
        cls.pr_workflow = cls.workflow
        cls.release_workflow = RELEASE_WORKFLOW.read_text(encoding="utf-8")

    def test_release_workflow_does_not_reference_retired_web_template(self):
        for retired_reference in (
            "test:web-template-packaging",
            "prepare:web-template",
            "resources/common/web-template",
            "网页模板发布前冒烟",
        ):
            self.assertNotIn(
                retired_reference,
                self.release_workflow,
                f"发布流程仍引用已退役网页模板: {retired_reference}",
            )

    def test_pr_submodule_verifier_strictly_matches_the_published_tag(self):
        verifier = PUBLIC_SUBMODULE_VERIFIER.read_text(encoding="utf-8")
        verifier_gate = self.pr_workflow.split(
            "- name: 公开底座 gitlink 可达性", maxsplit=1
        )[1].split("- name: 初始化公共底座 submodule", maxsplit=1)[0]
        self.assertIn("./scripts/verify-public-submodule.sh", verifier_gate)
        self.assertNotIn("--allow-registered-candidate", verifier_gate)
        self.assertNotIn("LOCAL_SECURITY_HEAD", verifier)
        self.assertIn('[[ "$tag_target" != "$gitlink" ]]', verifier)
        self.assertIn('PINVOU_CODEWHALE_TAG="pinvou-v0.9.5-r11"', verifier)
        self.assertIn("unknown argument", verifier)

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

    def test_static_analysis_gate_configs_route_to_frontend_test(self):
        # 静态分析门禁只在 frontend-test 内运行，其配置文件必须进入
        # frontend 路径过滤，否则 config-only PR 会静默跳过消费它的门禁。
        changes = _without_yaml_comments(
            self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
                "\n  fast-gate:", maxsplit=1
            )[0]
        )
        frontend_paths = changes.split(
            "            frontend:", maxsplit=1
        )[1].split("            relay:", maxsplit=1)[0]
        for path in (
            "pinvou3-app/.oxlintrc.json",
            "pinvou3-app/biome.jsonc",
            "pinvou3-app/knip.json",
            "pinvou3-app/jsconfig.json",
            "pinvou3-app/eslint.config.mjs",
            "pinvou3-app/scripts/audit-compat.mjs",
        ):
            self.assertIn(
                f"- '{path}'",
                frontend_paths,
                f"静态门禁配置 {path} 不在 frontend filter 中",
            )

    def test_merge_queue_uses_path_filtering_and_product_gates(self):
        changes = self.workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  fast-gate:", maxsplit=1
        )[0]
        self.assertIn("uses: dorny/paths-filter@v4", changes)
        self.assertIn("github.event_name == 'merge_group'", changes)
        for output in (
            "rust_code",
            "rust_dependencies",
            "knowledge_rust",
            "knowledge_dependencies",
            "release_contract",
            "frontend",
            "relay",
            "team_relay_hub",
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

    def test_trusted_backflow_has_parallel_three_platform_light_gate(self):
        portability = self.workflow.split(
            "\n  backflow-portability:", maxsplit=1
        )[1].split("\n  fast-gate:", maxsplit=1)[0]
        self.assertIn("needs.changes.outputs.trusted_backflow == 'true'", portability)
        self.assertIn("needs.changes.outputs.backflow_infra == 'true'", portability)
        self.assertIn("github.event_name == 'pull_request'", portability)
        self.assertIn("github.event_name == 'merge_group'", portability)
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

    def test_windows_runtime_gate_tracks_python_mcp_manifests(self):
        changes = self.workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  fast-gate:", maxsplit=1
        )[0]
        windows_runtime = changes.split("\n            windows_runtime:", maxsplit=1)[
            1
        ].split("\n            windows_rustup_repair:", maxsplit=1)[0]
        self.assertIn(
            "pinvou3-app/resources/mcp-servers/**/manifest.json",
            windows_runtime,
        )

    def test_standalone_knowledge_crate_has_its_own_required_gate(self):
        changes = _without_yaml_comments(
            self.workflow.split("\n  changes:", maxsplit=1)[1].split(
                "\n  fast-gate:", maxsplit=1
            )[0]
        )
        self.assertIn("knowledge_rust:", changes)
        self.assertIn("knowledge_dependencies:", changes)
        knowledge_paths = changes.split(
            "            knowledge_rust:", maxsplit=1
        )[1].split("            knowledge_dependencies:", maxsplit=1)[0]
        self.assertIn("- 'pinvou-knowledge/**/*.rs'", knowledge_paths)
        self.assertIn("- 'pinvou-knowledge/deploy/**'", knowledge_paths)

        knowledge = _without_yaml_comments(
            self.workflow.split("\n  knowledge-rust:", maxsplit=1)[1].split(
                "\n  rust-lint:", maxsplit=1
            )[0]
        )
        self.assertIn("needs.changes.outputs.knowledge_rust == 'true'", knowledge)
        self.assertIn(
            "cargo fmt --manifest-path pinvou-knowledge/Cargo.toml -- --check",
            knowledge,
        )
        self.assertIn(
            "cargo clippy --manifest-path pinvou-knowledge/Cargo.toml --all-targets --all-features --no-deps",
            knowledge,
        )
        self.assertIn(
            "cargo test --manifest-path pinvou-knowledge/Cargo.toml --all-features",
            knowledge,
        )
        self.assertIn("bash -n pinvou-knowledge/deploy/install.sh", knowledge)
        self.assertIn("needs.changes.outputs.knowledge_dependencies == 'true'", knowledge)
        self.assertIn("--manifest-path pinvou-knowledge/Cargo.toml", knowledge)

        required_gate = self.workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertIn("- knowledge-rust", required_gate)
        self.assertIn('"knowledge-rust:$KNOWLEDGE_RUST_RESULT"', required_gate)

    def test_benchmark_jobs_are_hard_disabled_and_cannot_be_enabled_by_label(self):
        changes = self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  fast-gate:", maxsplit=1
        )[0]
        self.assertIn("benchmark:", changes)
        self.assertIn("benchmark_cli:", changes)
        self.assertIn("benchmark_headless:", changes)
        self.assertIn("benchmark_codewhale:", changes)
        self.assertIn("- 'pinvou-cli/**'", changes)
        self.assertIn(
            "- 'pinvou3-app/src-tauri/src/features/assistant/product_runtime/headless_bridge.rs'",
            changes,
        )

        contract = self.pr_workflow.split("\n  benchmark-contract:", maxsplit=1)[1].split(
            "\n  benchmark-test:", maxsplit=1
        )[0]
        contract_header = contract.split("\n    runs-on:", maxsplit=1)[0]
        self.assertIn("if: ${{ false }}", contract_header)
        self.assertNotIn("ci:full-benchmark", contract_header)
        self.assertNotIn("github.event_name", contract_header)
        self.assertNotIn("needs.changes.outputs.benchmark == 'true'", contract_header)
        self.assertIn("runs-on: ubuntu-latest", contract)
        self.assertIn(
            "RUSTC_WRAPPER: ${{ github.workspace }}/pinvou3-app/src-tauri/scripts/rustc-stack-wrapper",
            contract,
        )
        self.assertIn(
            "cargo test --manifest-path pinvou-cli/Cargo.toml --all-features", contract
        )
        for package in (
            "agent-backend-api",
            "benchmark-core",
            "adapter-smoke",
            "adapter-gaia",
        ):
            self.assertIn(f"-p {package}", contract)
        self.assertIn(
            "-p pinvou-cli --no-default-features", contract
        )
        self.assertIn(
            "cargo check --manifest-path pinvou3-app/src-tauri/Cargo.toml", contract
        )
        self.assertIn("--features benchmark-hooks --test headless_bridge_contract", contract)
        self.assertIn("--features benchmark-hooks --lib", contract)
        self.assertIn(
            "features::assistant::product_runtime::headless_bridge::", contract
        )
        self.assertIn("-- --test-threads=1", contract)
        self.assertIn("needs.changes.outputs.benchmark_codewhale == 'true'", contract)

        benchmark = self.pr_workflow.split("\n  benchmark-test:", maxsplit=1)[1].split(
            "\n  required-gate:", maxsplit=1
        )[0]
        benchmark_header = benchmark.split("\n    runs-on:", maxsplit=1)[0]
        self.assertIn("cargo test --manifest-path pinvou-cli/Cargo.toml --all-features", benchmark)
        self.assertIn(
            "cargo test --manifest-path pinvou-cli/Cargo.toml --no-default-features", benchmark
        )
        self.assertIn("--features benchmark-hooks", benchmark)
        self.assertIn("cargo test --manifest-path CodeWhale/Cargo.toml", benchmark)
        self.assertIn("-p codewhale-tui --lib --locked forkguard_", benchmark)
        self.assertIn("            CodeWhale", benchmark)
        self.assertIn("if: ${{ false }}", benchmark_header)
        self.assertNotIn("ci:full-benchmark", benchmark_header)
        self.assertNotIn("github.event_name", benchmark_header)
        self.assertNotIn("github.event_name == 'merge_group'", benchmark)
        self.assertNotIn("github.event_name == 'push'", benchmark)
        self.assertNotIn("needs.changes.outputs.benchmark", benchmark_header)
        self.assertNotIn("\n        if:", benchmark)

        benchmark_filter = changes.split("benchmark:", 1)[1].split(
            "release_contract:", 1
        )[0]
        self.assertNotIn(".github/workflows/pr-check.yml", benchmark_filter)

        required_gate = self.pr_workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertIn("- benchmark-contract", required_gate)
        self.assertIn("BENCHMARK_CONTRACT_RESULT", required_gate)
        self.assertIn('"benchmark-contract:$BENCHMARK_CONTRACT_RESULT"', required_gate)
        self.assertNotIn("- benchmark-test", required_gate)
        self.assertNotIn("BENCHMARK_TEST_RESULT", required_gate)
        self.assertNotIn('"benchmark-test:$BENCHMARK_TEST_RESULT"', required_gate)

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
        self.assertNotIn(
            "github.event_name == 'push' || needs.changes.outputs.rust_code == 'true'",
            rust_test,
        )
        self.assertIn("timeout-minutes: 120", rust_test)
        self.assertIn(
            'RUSTFLAGS: "-C link-arg=-fuse-ld=lld '
            '-C link-arg=-Wl,--thinlto-jobs=1 '
            '-C link-arg=-Wl,--threads=1"',
            rust_test,
        )

    def test_windows_rust_test_runs_on_every_retained_main_push(self):
        # concurrency 可能用后续非 Rust pending 替换含 Rust 变更的 pending；Windows
        # 累计验证不得依赖单次 push 的相邻路径 diff。
        windows_rust_test = _without_yaml_comments(
            self.workflow.split("\n  windows-rust-test:", maxsplit=1)[1].split(
                "\n  windows-rustup-repair-test:", maxsplit=1
            )[0]
        )
        self.assertIn("github.event_name == 'push'", windows_rust_test)
        self.assertNotIn("needs.changes.outputs.rust_code == 'true'", windows_rust_test)
        self.assertNotIn("github.event_name == 'merge_group'", windows_rust_test)
        self.assertNotIn("github.event.pull_request.draft == false", windows_rust_test)

        self.assertIn(
            "defaults:\n      run:\n        shell: bash",
            windows_rust_test,
        )
        self.assertIn("Windows 测试 exe 嵌入 Common-Controls v6 清单", windows_rust_test)
        self.assertIn('"-outputresource:$($testExe.FullName);#1"', windows_rust_test)
        self.assertIn('"PINVOU3_TEST_EXE=$testExe" | Out-File', windows_rust_test)
        self.assertIn('test_exe="$(cygpath -u "$PINVOU3_TEST_EXE")"', windows_rust_test)
        regression = windows_rust_test.split(
            "- name: Windows 原子替换状态机回归", maxsplit=1
        )[1]
        self.assertIn('"$test_exe" "$filter" --test-threads=1', regression)
        self.assertNotIn("cargo test", regression)

        required_gate = self.pr_workflow.split(
            "\n  required-gate:", maxsplit=1
        )[1]
        self.assertIn("- windows-rust-test", required_gate)
        self.assertIn("WINDOWS_RUST_RESULT", required_gate)
        self.assertIn('"windows-rust-test:$WINDOWS_RUST_RESULT"', required_gate)

    def test_windows_browser_wrapper_lifecycle_runs_in_required_native_job(self):
        changes = self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  fast-gate:", maxsplit=1
        )[0]
        windows_codex_filter = changes.split("windows_codex:", maxsplit=1)[1]
        self.assertIn(
            "resources/common/bundle/mcp-servers/browser-*",
            windows_codex_filter,
        )
        self.assertIn(
            "browser_wrapper_windows_lifecycle.test.mjs",
            windows_codex_filter,
        )
        self.assertIn("browser_wrapper_lazy.test.mjs", windows_codex_filter)

        windows_job = self.pr_workflow.split(
            "\n  windows-codex-runtime-test:", maxsplit=1
        )[1].split("\n  macos-codex-runtime-test:", maxsplit=1)[0]
        self.assertIn("needs.changes.outputs.windows_codex == 'true'", windows_job)
        self.assertIn("runs-on: windows-latest", windows_job)
        self.assertIn("Windows browser wrapper lifecycle regression", windows_job)
        self.assertIn(
            "node --test pinvou3-app/tests/browser_wrapper_windows_lifecycle.test.mjs",
            windows_job,
        )
        self.assertIn(
            "node pinvou3-app/tests/browser_wrapper_lazy.test.mjs",
            windows_job,
        )
        self.assertIn('PINVOU3_TEST_BROWSER_NO_HOST: "1"', windows_job)

        required_gate = self.pr_workflow.split(
            "\n  required-gate:", maxsplit=1
        )[1]
        self.assertIn("- windows-codex-runtime-test", required_gate)
        self.assertIn("WINDOWS_CODEX_RESULT", required_gate)

    def test_release_and_platform_contracts_skip_drafts_but_run_in_queue(self):
        changes = _without_yaml_comments(
            self.workflow.split("\n  changes:", maxsplit=1)[1].split(
                "\n  fast-gate:", maxsplit=1
            )[0]
        )
        release_contract_paths = changes.split(
            "            release_contract:", maxsplit=1
        )[1].split("            l1:", maxsplit=1)[0]
        self.assertIn("- 'pinvou3-app/src-tauri/resources/**'", release_contract_paths)
        self.assertIn(
            "- 'pinvou3-app/tests/knowledge_host_packaging.test.mjs'",
            release_contract_paths,
        )

        release_contract = self.workflow.split(
            "\n  release-contract-test:", maxsplit=1
        )[1].split("\n  knowledge-rust:", maxsplit=1)[0]
        self.assertIn("needs.changes.outputs.release_contract == 'true'", release_contract)
        self.assertIn("github.event.pull_request.draft == false", release_contract)
        self.assertNotIn("github.event_name != 'merge_group'", release_contract)
        self.assertIn(
            "npm --prefix pinvou3-app run test:knowledge-host-packaging",
            release_contract,
        )

        for start, end in (
            ("windows-codex-runtime-test", "windows-runtime-test"),
            ("windows-runtime-test", "macos-codex-runtime-test"),
            ("macos-codex-runtime-test", "required-gate"),
        ):
            block = self.workflow.split(f"\n  {start}:", maxsplit=1)[1].split(
                f"\n  {end}:", maxsplit=1
            )[0]
            self.assertIn("github.event.pull_request.draft == false", block)

    def test_pure_frontend_changes_do_not_trigger_macos_rust_build(self):
        # Pure-frontend paths must not enter the mac-build trigger set (avoids
        # needless native builds); but package.json/package-lock.json changes
        # must trigger (the lockfile affects the build).
        # Folded in from scripts/tests/test_ci_trigger_routing_policy.py to
        # remove the duplicated parsing of the same mac-build.yml trigger
        # block across two files.
        trigger = MAC_BUILD_WORKFLOW.read_text(encoding="utf-8").split(
            "\non:", maxsplit=1
        )[1].split("\npermissions:", maxsplit=1)[0]

        self.assertIn("'pinvou3-app/src-tauri/**'", trigger)
        self.assertNotIn("'pinvou3-app/src/**'", trigger)
        self.assertIn("'pinvou3-app/package.json'", trigger)
        self.assertIn("'pinvou3-app/package-lock.json'", trigger)

    def test_wrapper_smoke_routes_merge_groups_before_platform_matrix(self):
        # rustc-wrapper-smoke must first pass the paths-filter gate before
        # entering the three-platform matrix, so wrapper-unrelated PRs do not
        # run the full three-platform smoke.
        # Folded in from scripts/tests/test_ci_trigger_routing_policy.py.
        workflow = (
            ROOT / ".github/workflows/rustc-wrapper-smoke.yml"
        ).read_text(encoding="utf-8")
        trigger = workflow.split("\non:", maxsplit=1)[1].split(
            "\npermissions:", maxsplit=1
        )[0]
        pull_request = trigger.split("\n  pull_request:", maxsplit=1)[1].split(
            "\n  merge_group:", maxsplit=1
        )[0]
        push = trigger.split("\n  push:", maxsplit=1)[1]
        changes = workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  smoke:", maxsplit=1
        )[0]
        smoke = workflow.split("\n  smoke:", maxsplit=1)[1]

        self.assertIn("merge_group:", trigger)
        self.assertIn("push:", trigger)
        self.assertIn("paths:", trigger)
        workflow_path = "'.github/workflows/rustc-wrapper-smoke.yml'"
        self.assertIn(workflow_path, pull_request)
        self.assertIn(workflow_path, push)
        self.assertIn(workflow_path, changes)
        self.assertIn("uses: dorny/paths-filter@v4", changes)
        self.assertIn("wrapper: ${{ steps.filter.outputs.wrapper }}", changes)
        self.assertIn("needs: changes", smoke)
        self.assertIn("if: ${{ needs.changes.outputs.wrapper == 'true' }}", smoke)
        self.assertIn("os: [macos-15, ubuntu-latest, windows-latest]", smoke)


if __name__ == "__main__":
    unittest.main()
