import json
import re
import tomllib
import unittest
from fnmatch import fnmatchcase
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PR_WORKFLOW = ROOT / ".github/workflows/pr-check.yml"
RELEASE_WORKFLOW = ROOT / ".github/workflows/release-packages.yml"
MAC_BUILD_WORKFLOW = ROOT / ".github/workflows/mac-build.yml"
PUBLIC_SUBMODULE_VERIFIER = ROOT / "scripts/verify-public-submodule.sh"
TAURI_MANIFEST = ROOT / "pinvou3-app/src-tauri/Cargo.toml"


def _extract_quoted_paths(block):
    """提取 YAML 块中 `- 'path'` 形式的路径条目（保持文本序）。"""
    paths = []
    for line in block.splitlines():
        stripped = line.strip()
        if stripped.startswith("- '") and stripped.endswith("'"):
            paths.append(stripped[3:-1])
    return paths


def _without_yaml_comments(block):
    return "\n".join(
        line for line in block.splitlines() if not line.lstrip().startswith("#")
    )


def _is_covered_by_trigger(entry, trigger_paths):
    """entry 被 trigger path 覆盖:完全相同,或 trigger 是其上层 `/**` 目录 glob。

    与 dorny/paths-filter 的 some-with-excludes 语义对齐:至少一条正向
    pattern 命中,且没有任何 `!` 排除条目命中(排除优先于命中)。忽略
    排除条目会让路由锁在 filter 组新增排除时仍虚报覆盖(fail-open)。
    """

    def _matches(pattern):
        return entry == pattern or (
            pattern.endswith("/**") and entry.startswith(pattern[:-2])
        )

    if not any(_matches(p) for p in trigger_paths if not p.startswith("!")):
        return False
    excludes = [p[1:] for p in trigger_paths if p.startswith("!")]
    return not any(_matches(p) for p in excludes)


def _extract_contract_read_targets(contract_test):
    """从 multiagent_plan_normalize.test.mjs 源码派生全部 src-tauri 读取目标。

    `read('src-tauri', ...)` 产出单文件目标,`path.join(here, '..', 'src-tauri',
    ...)` 产出 readdirSync 整目录拼接的 `/**` 目标。单引号与双引号形式都被
    接受:此前正则只匹配单引号,双引号的 `read("src-tauri", ...)` 会整体绕过
    路由锁(fail-open,经变异验证)。
    """
    targets = []
    for args in re.findall(
        r"read\(['\"]src-tauri['\"],\s*([^)]*)\)", contract_test
    ):
        parts = re.findall(r"['\"]([^'\"]*)['\"]", args)
        if not parts:
            raise AssertionError(f"无法解析的契约测试 read 目标: {args}")
        targets.append("pinvou3-app/src-tauri/" + "/".join(parts))
    for args in re.findall(
        r"path\.join\(here, ['\"]\.\.['\"], ['\"]src-tauri['\"],\s*([^)]*)\)",
        contract_test,
    ):
        parts = re.findall(r"['\"]([^'\"]*)['\"]", args)
        targets.append("pinvou3-app/src-tauri/" + "/".join(parts) + "/**")
    return targets


def _matches_paths_filter(path, patterns):
    """Model paths-filter v4 some-with-excludes routing for policy examples."""
    included = any(
        fnmatchcase(path, pattern)
        for pattern in patterns
        if not pattern.startswith("!")
    )
    excluded = any(
        fnmatchcase(path, pattern[1:])
        for pattern in patterns
        if pattern.startswith("!")
    )
    return included and not excluded


def _evaluate_github_condition(
    expression,
    *,
    event_name,
    draft,
    mode,
    rust_full=False,
    rust_code=False,
    cli_rust=False,
    full_label=False,
):
    """Replay the checked-in GitHub expression for a deterministic truth table."""

    replacements = {
        "!cancelled()": "True",
        "github.event_name": repr(event_name),
        "github.event.pull_request.draft": repr(draft),
        "needs.changes.outputs.backflow_mode": repr(mode),
        "needs.changes.outputs.rust_full": repr(str(rust_full).lower()),
        "needs.changes.outputs.rust_code": repr(str(rust_code).lower()),
        "needs.changes.outputs.cli_rust": repr(str(cli_rust).lower()),
        "contains(github.event.pull_request.labels.*.name, 'ci:full-rust')": repr(
            full_label
        ),
    }
    evaluated = expression
    for source, value in replacements.items():
        evaluated = evaluated.replace(source, value)
    evaluated = evaluated.replace("&&", " and ").replace("||", " or ")
    evaluated = re.sub(r"\btrue\b", "True", evaluated, flags=re.IGNORECASE)
    evaluated = re.sub(r"\bfalse\b", "False", evaluated, flags=re.IGNORECASE)
    evaluated = " ".join(evaluated.split())
    return bool(eval(evaluated, {"__builtins__": {}}, {}))


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

    def test_pr_verifier_checks_registered_history_and_keeps_release_closure(self):
        verifier = PUBLIC_SUBMODULE_VERIFIER.read_text(encoding="utf-8")
        verifier_gate = self.pr_workflow.split(
            "- name: 公开底座 gitlink 可达性", maxsplit=1
        )[1].split("- name: 初始化公共底座 submodule", maxsplit=1)[0]
        self.assertIn("./scripts/verify-public-submodule.sh --registered-snapshot", verifier_gate)
        self.assertNotIn("--allow-registered-candidate", verifier_gate)
        self.assertNotIn("LOCAL_SECURITY_HEAD", verifier)
        self.assertIn('[[ "$tag_target" != "$gitlink" ]]', verifier)
        self.assertIn('PINVOU_CODEWHALE_BRANCH="pinvou3-clean"', verifier)
        self.assertIn('PINVOU_CODEWHALE_TAG="pinvou-v0.9.12-r3"', verifier)
        self.assertIn('[[ "$branch_target" != "$gitlink" ]]', verifier)
        self.assertIn('[[ "$branch_target" != "$tag_target" ]]', verifier)
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
        self.assertIn("Merge Queue diff-selected browser smoke", frontend)
        self.assertIn("github.event.merge_group.base_sha", frontend)
        self.assertIn("github.event.merge_group.head_sha", frontend)
        self.assertEqual(frontend.count("select-frontend-smokes.mjs"), 2)
        self.assertNotIn("npm run test:browser-smoke", frontend)
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

    def test_approval_center_browser_regression_is_registered(self):
        package = json.loads((ROOT / "pinvou3-app/package.json").read_text(encoding="utf-8"))
        finance = package["scripts"]["test:account-platform-finance-ui"]
        self.assertIn("node tests/account_platform_approval_center_smoke.mjs", finance)
        selector = (ROOT / "scripts/select-frontend-smokes.mjs").read_text(encoding="utf-8")
        self.assertIn('command("npm", "test:account-platform-finance-ui")', selector)

    def test_organization_member_smokes_resolve_to_real_browser_files(self):
        package = json.loads((ROOT / "pinvou3-app/package.json").read_text(encoding="utf-8"))
        selector = (ROOT / "scripts/select-frontend-smokes.mjs").read_text(encoding="utf-8")
        for feature, filename in (("organization", "account_platform_organization_ui_smoke.mjs"), ("member-units", "account_platform_member_units_ui_smoke.mjs")):
            command = f"test:account-platform-{feature}-ui"
            self.assertEqual(package["scripts"][command], f"node tests/{filename}")
            self.assertTrue((ROOT / "pinvou3-app/tests" / filename).is_file())
            self.assertGreaterEqual(selector.count(f'command("npm", "{command}")'), 2)

    def test_registration_bonus_smokes_resolve_to_both_compiled_browser_suites(self):
        package = json.loads((ROOT / "pinvou3-app/package.json").read_text(encoding="utf-8"))
        command = "test:account-platform-registration-bonus-ui"
        files = ("admin_registration_bonus_ui_smoke.mjs", "admin_registration_bonus_precision_ui_smoke.mjs")
        self.assertEqual(package["scripts"][command], " && ".join(f"node tests/{name}" for name in files))
        for name in files:
            self.assertTrue((ROOT / "pinvou3-app/tests" / name).is_file())
        selector = (ROOT / "scripts/select-frontend-smokes.mjs").read_text(encoding="utf-8")
        self.assertGreaterEqual(selector.count(f'command("npm", "{command}")'), 2)

    def test_closure_smokes_include_production_recovery_and_original_contract(self):
        package = json.loads((ROOT / "pinvou3-app/package.json").read_text(encoding="utf-8"))
        command = "test:account-platform-closure-ui"
        files = ("account_platform_closure_ui_smoke.mjs", "account_platform_closure_production_smoke.mjs")
        self.assertEqual(package["scripts"][command], " && ".join(f"node tests/{name}" for name in files))
        for name in files:
            self.assertTrue((ROOT / "pinvou3-app/tests" / name).is_file())
        selector = (ROOT / "scripts/select-frontend-smokes.mjs").read_text(encoding="utf-8")
        self.assertGreaterEqual(selector.count(f'command("npm", "{command}")'), 2)

    def test_embedded_account_portal_assets_are_rebuilt_and_checked(self):
        changes = _without_yaml_comments(
            self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
                "\n  fast-gate:", maxsplit=1
            )[0]
        )
        paths = _extract_quoted_paths(
            changes.split("            frontend:", maxsplit=1)[1].split(
                "            relay:", maxsplit=1
            )[0]
        )
        self.assertTrue(_matches_paths_filter("account-platform/internal/portal/static/admin.html", paths))
        self.assertTrue(_matches_paths_filter("account-platform/internal/portal/static/assets/admin.js", paths))
        frontend = self.pr_workflow.split("\n  frontend-test:", maxsplit=1)[1].split(
            "\n  relay-test:", maxsplit=1
        )[0]
        gate = frontend.split("      - name: Verify embedded Account Portal assets are current", maxsplit=1)[1].split(
            "      - name:", maxsplit=1
        )[0]
        self.assertNotIn("if:", gate)
        self.assertIn("npm run build:account-portal", gate)
        self.assertIn("git diff --exit-code -- ../account-platform/internal/portal/static", gate)
        self.assertIn("git ls-files --others --exclude-standard", gate)
        self.assertIn('test -z "$(git ls-files', gate)

    def test_cross_language_contract_rust_sources_route_to_frontend_test(self):
        # multiagent_plan_normalize.test.mjs reads the Rust sources below for
        # cross-language contract pins (swarm contract text, same-snapshot
        # invariant, edit-resend replay, roster caps). If they are absent from
        # the frontend path filter, a Rust-only PR silently skips that node
        # gate — the same structural blind spot the static-analysis configs
        # above guard against.
        changes = _without_yaml_comments(
            self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
                "\n  fast-gate:", maxsplit=1
            )[0]
        )
        frontend_paths = changes.split(
            "            frontend:", maxsplit=1
        )[1].split("            relay:", maxsplit=1)[0]
        for path in (
            "pinvou3-app/src-tauri/src/features/assistant/**",
            "pinvou3-app/src-tauri/src/features/multiagent/transcripts.rs",
            "pinvou3-app/src-tauri/src/features/remote_control/manager/**",
            "pinvou3-app/src-tauri/src/features/sessions/**",
            "pinvou3-app/src-tauri/src/features/personas/mod.rs",
            "pinvou3-app/src-tauri/src/features/files/file_ingest.rs",
            "pinvou3-app/src-tauri/src/app/commands/multiagent.rs",
            "pinvou3-app/src-tauri/src/app/commands/chat.rs",
            "pinvou3-app/src-tauri/src/app/commands/memory.rs",
            "pinvou3-app/src-tauri/src/app/commands/interaction.rs",
            "pinvou3-app/src-tauri/src/app/commands/remote_control.rs",
            "pinvou3-app/src-tauri/src/app/commands/personas.rs",
            "pinvou3-app/src-tauri/src/lib.rs",
        ):
            self.assertIn(
                f"- '{path}'",
                frontend_paths,
                f"跨语言契约测试读取的 Rust 源 {path} 不在 frontend filter 中,Rust-only PR 会静默跳过该 node 门禁",
            )

    def test_cross_language_contract_reads_fully_routed(self):
        # 上面的静态清单会随 .mjs 演进漂移:这里从
        # multiagent_plan_normalize.test.mjs 本身派生它读取的全部 src-tauri
        # 目标(单文件 read(...) 与 readdirSync 整目录拼接,单/双引号形式
        # 均归一化匹配),逐一断言 frontend filter 覆盖。给契约测试新增
        # Rust read 而不路由、或把已路由文件挪走,都会在这里失败(本套件
        # 在 fast-gate 每个 PR 必跑)。
        changes = _without_yaml_comments(
            self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
                "\n  fast-gate:", maxsplit=1
            )[0]
        )
        frontend_entries = _extract_quoted_paths(
            changes.split("            frontend:", maxsplit=1)[1].split(
                "            relay:", maxsplit=1
            )[0]
        )
        contract_test = (
            ROOT / "pinvou3-app/tests/multiagent_plan_normalize.test.mjs"
        ).read_text(encoding="utf-8")

        targets = _extract_contract_read_targets(contract_test)

        self.assertTrue(
            targets,
            "未能从 multiagent_plan_normalize.test.mjs 解析出 src-tauri 读取目标",
        )
        for target in targets:
            self.assertTrue(
                _is_covered_by_trigger(target, frontend_entries),
                f"跨语言契约测试读取的 {target} 未被 frontend filter 覆盖,"
                "Rust-only 改动会静默跳过该 node 门禁",
            )

    def test_contract_target_derivation_covers_double_quoted_reads(self):
        # 回归锁:派生正则此前只匹配单引号形式,双引号的
        # `read("src-tauri", ...)` 会整体绕过上面的路由锁(经变异验证)。
        # 对派生函数喂最小 fixture:双引号的单文件与整目录目标都必须被
        # 解析出来,且不被缺少该条目的 frontend filter 覆盖——即未来出现
        # 未路由的双引号读取时,路由锁必定失败而不是静默通过。
        fixture = (
            "const direct = read(\"src-tauri\", \"src\", \"features\", "
            "\"future_feature\", \"mod.rs\");\n"
            "const dir = path.join(here, \"..\", \"src-tauri\", \"src\", "
            "\"features\", \"future_module\");\n"
        )
        targets = _extract_contract_read_targets(fixture)
        self.assertIn(
            "pinvou3-app/src-tauri/src/features/future_feature/mod.rs",
            targets,
            "双引号 read 目标必须被派生出来(单引号正则 fail-open 回归)",
        )
        self.assertIn(
            "pinvou3-app/src-tauri/src/features/future_module/**",
            targets,
            "双引号 path.join 整目录目标必须被派生出来",
        )
        unrouted_filter = ["pinvou3-app/src-tauri/src/lib.rs"]
        for target in targets:
            self.assertFalse(
                _is_covered_by_trigger(target, unrouted_filter),
                f"未路由的双引号读取 {target} 不应被无关 filter 覆盖",
            )

    def test_trigger_coverage_respects_exclusions(self):
        # dorny/paths-filter(some-with-excludes)的语义是"至少一条正向
        # pattern 命中且没有任何 `!` 排除条目命中"。此前
        # _is_covered_by_trigger 忽略排除条目:frontend 组若新增排除,
        # 路由锁会虚报覆盖而 CI 实际不触发该门禁。
        positive = ["pinvou3-app/src-tauri/src/features/**"]
        self.assertTrue(
            _is_covered_by_trigger(
                "pinvou3-app/src-tauri/src/features/assistant/engine.rs",
                positive,
            )
        )
        with_exclude = positive + [
            "!pinvou3-app/src-tauri/src/features/assistant/**"
        ]
        self.assertFalse(
            _is_covered_by_trigger(
                "pinvou3-app/src-tauri/src/features/assistant/engine.rs",
                with_exclude,
            ),
            "正向命中但被 `!` 排除条目命中的路径不得视为覆盖",
        )
        # 排除只作用于其命中范围,同组其余路径仍被覆盖。
        self.assertTrue(
            _is_covered_by_trigger(
                "pinvou3-app/src-tauri/src/features/sessions/mode_state.rs",
                with_exclude,
            )
        )
        # 精确条目形式的排除同样生效。
        exact_exclude = [
            "pinvou3-app/src-tauri/src/lib.rs",
            "!pinvou3-app/src-tauri/src/lib.rs",
        ]
        self.assertFalse(
            _is_covered_by_trigger(
                "pinvou3-app/src-tauri/src/lib.rs", exact_exclude
            )
        )
        # 只有排除条目、没有任何正向 pattern 时不得视为覆盖。
        self.assertFalse(
            _is_covered_by_trigger(
                "pinvou3-app/src-tauri/src/lib.rs",
                ["!pinvou3-app/src-tauri/src/lib.rs"],
            )
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
            "rust_full",
            "cli_rust",
            "knowledge_rust",
            "knowledge_dependencies",
            "release_contract",
            "frontend",
            "relay",
            "team_relay_hub",
            "update_release_console",
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
        for output, fallback in (
            ("backflow_mode", "full"),
            ("pure_release_backflow", "false"),
        ):
            self.assertIn(f"{output}:", changes)
            self.assertIn(f"|| '{fallback}'", changes)

        required_gate = self.workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertIn("Merge Queue 基础检查失败", required_gate)
        for job in (
            "team-relay-hub-test",
            "update-release-console-test",
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
        self.assertRegex(hub_job, r"image: mirror\.gcr\.io/library/postgres@sha256:[a-f0-9]{64}")
        self.assertIn("TEAM_RELAY_TEST_DATABASE_URL", hub_job)
        self.assertIn("npm --prefix team-relay-hub ci", hub_job)
        self.assertIn("npm --prefix team-relay-hub test", hub_job)
        self.assertIn(
            "npm --prefix team-relay-hub audit "
            "--registry=https://registry.npmjs.org --audit-level=high",
            hub_job,
        )

        console_job = self.workflow.split(
            "\n  update-release-console-test:", maxsplit=1
        )[1].split("\n  release-contract-test:", maxsplit=1)[0]
        self.assertIn(
            "needs.changes.outputs.update_release_console == 'true'",
            console_job,
        )
        self.assertIn("npm --prefix update-release-console ci", console_job)
        self.assertIn("npm --prefix update-release-console test", console_job)
        self.assertIn('node-version: ["20", "24"]', console_job)
        self.assertIn("node-version: ${{ matrix.node-version }}", console_job)
        self.assertIn(
            "npm --prefix update-release-console audit "
            "--registry=https://registry.npmjs.org --audit-level=high",
            console_job,
        )
        self.assertIn(
            'docker build --build-arg NODE_IMAGE="$PINVOU_UPDATE_NODE_IMAGE" '
            "--file update-release-console/Dockerfile "
            "update-release-console",
            console_job,
        )
        self.assertIn(
            "docker compose --file update-release-console/deploy/compose.yaml "
            "config --quiet",
            console_job,
        )
        self.assertRegex(console_job, r"mirror\.gcr\.io/library/nginx@sha256:[a-f0-9]{64} nginx -t")
        self.assertIn("/var/www/pinvou3/latest.json", console_job)
        self.assertIn(
            "docker compose --file update-release-console/deploy/compose.yaml "
            "up --detach --build",
            console_job,
        )
        self.assertIn("release-console/healthz", console_job)
        self.assertIn("release-console/api/status", console_job)
        self.assertIn(
            "docker compose --file update-release-console/deploy/compose.yaml "
            "down --volumes --remove-orphans",
            console_job,
        )
        self.assertIn("always() && matrix.node-version == '20'", console_job)

    def test_trusted_backflow_has_parallel_three_platform_light_gate(self):
        portability = self.workflow.split(
            "\n  backflow-portability:", maxsplit=1
        )[1].split("\n  fast-gate:", maxsplit=1)[0]
        self.assertIn("needs.changes.outputs.trusted_backflow == 'true'", portability)
        self.assertIn("needs.changes.outputs.backflow_infra == 'true'", portability)
        self.assertIn("github.event_name == 'pull_request'", portability)
        self.assertIn("github.event_name == 'merge_group'", portability)
        self.assertIn("os: [ubuntu-22.04, windows-latest, macos-15]", portability)
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
        # The -D-warnings hard gate must stay in this job (single shared
        # cache); rust-lint must not compile the workspace a second time.
        self.assertIn(
            "cargo clippy --manifest-path pinvou-knowledge/Cargo.toml --lib --bins --no-deps --features server -- -D warnings",
            knowledge,
        )
        self.assertNotIn("cargo clippy pinvou-knowledge", self.pr_workflow)
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

    def test_fast_gate_actionlint_is_pinned_and_checksum_verified(self):
        fast_gate = self.pr_workflow.split("\n  fast-gate:", maxsplit=1)[1].split(
            "\n  frontend-test:", maxsplit=1
        )[0]
        step = fast_gate.split(
            "- name: workflow lint (actionlint)", maxsplit=1
        )[1].split("\n      - name:", maxsplit=1)[0]
        # The release artifact is fetched from the pinned tag and verified
        # against the release checksums.txt digest. Executing an installer
        # fetched from a mutable ref (e.g. raw.githubusercontent .../main/)
        # would let third-party code drift under a green gate.
        self.assertIn(
            "https://github.com/rhysd/actionlint/releases/download/v1.7.12/actionlint_1.7.12_linux_amd64.tar.gz",
            step,
        )
        self.assertIn(
            "8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8  actionlint.tar.gz",
            step,
        )
        self.assertIn("| sha256sum --check -", step)
        self.assertNotIn("download-actionlint.bash", step)
    def test_cli_crate_has_its_own_required_gate(self):
        changes = _without_yaml_comments(
            self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
                "\n  fast-gate:", maxsplit=1
            )[0]
        )
        self.assertIn("cli_rust:", changes)
        cli_paths = changes.split("            cli_rust:", maxsplit=1)[1].split(
            "            knowledge_rust:", maxsplit=1
        )[0]
        self.assertIn(
            "- 'pinvou-cli/**/*.rs'",
            cli_paths,
            "cli_rust must match the real crate directory (pinvou-cli)",
        )
        self.assertIn("- 'pinvou-cli/**/Cargo.toml'", cli_paths)
        self.assertIn("- 'CodeWhale'", cli_paths)
        # The CLI path-depends on the app crate, so the leaf features that
        # rust_full exempts still gate through the CLI suite (a change confined
        # to features/feedback or features/personas would otherwise run NO rust
        # gate at all).
        self.assertIn("- 'pinvou3-app/src-tauri/src/features/feedback/**'", cli_paths)
        self.assertIn("- 'pinvou3-app/src-tauri/src/features/personas/**'", cli_paths)

        cli_test = _without_yaml_comments(
            self.pr_workflow.split("\n  cli-test:", maxsplit=1)[1].split(
                "\n  windows-rust-test:", maxsplit=1
            )[0]
        )
        self.assertIn("needs.changes.outputs.cli_rust == 'true'", cli_test)
        self.assertIn(
            "github.event.pull_request.draft == false",
            cli_test,
            "draft PRs must skip the heavy CLI leg like the other rust jobs",
        )
        self.assertIn("- name: Set up zram and swap", cli_test)
        self.assertIn("scripts/ci-memory-setup.sh", cli_test)
        self.assertIn(
            "cargo fmt --all --check --manifest-path pinvou-cli/Cargo.toml",
            cli_test,
            "pinvou-cli is a virtual workspace: plain --manifest-path fmt fails "
            "with 'Failed to find targets', --all is required",
        )
        self.assertIn(
            "cargo test --manifest-path pinvou-cli/Cargo.toml --locked --no-fail-fast",
            cli_test,
        )
        self.assertIn(
            "cargo test -p adapter-gaia --features test-support --locked --no-fail-fast",
            cli_test,
            "dataset_contract is required-features-gated and silently skipped by "
            "the workspace run; the gaia timeout pins live there",
        )
        self.assertIn("cache-targets: false", cli_test)

        required_gate = self.pr_workflow.split(
            "\n  required-gate:", maxsplit=1
        )[1]
        self.assertIn("- cli-test", required_gate)
        self.assertIn('"cli-test:$CLI_TEST_RESULT"', required_gate)

    def test_benchmark_jobs_stay_out_of_product_pr_workflow(self):
        self.assertNotIn("\n  benchmark-contract:", self.pr_workflow)
        self.assertNotIn("\n  benchmark-test:", self.pr_workflow)
        changes = self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  fast-gate:", maxsplit=1
        )[0]
        self.assertNotIn("benchmark:", changes)
        self.assertNotIn("benchmark_cli:", changes)
        self.assertNotIn("benchmark_headless:", changes)
        self.assertNotIn("benchmark_codewhale:", changes)

    def test_full_rust_filter_fails_closed_with_stable_module_boundaries(self):
        changes = _without_yaml_comments(
            self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
                "\n  fast-gate:", maxsplit=1
            )[0]
        )
        rust_full = changes.split("            rust_full:", maxsplit=1)[1].split(
            "            knowledge_rust:", maxsplit=1
        )[0]
        rust_full_paths = _extract_quoted_paths(rust_full)
        self.assertIn(
            "predicate-quantifier: some-with-excludes",
            changes,
        )
        self.assertIn("pinvou3-app/src-tauri/**/*.rs", rust_full_paths)

        low_risk_boundaries = (
            "!pinvou3-app/src-tauri/src/features/feedback/**",
            "!pinvou3-app/src-tauri/src/features/personas/**",
            "!pinvou3-app/src-tauri/src/features/pet/**",
        )
        for boundary in low_risk_boundaries:
            self.assertIn(boundary, rust_full_paths)

        high_risk_examples = (
            "pinvou3-app/src-tauri/src/app/commands/chat.rs",
            "pinvou3-app/src-tauri/src/app/commands/interaction.rs",
            "pinvou3-app/src-tauri/src/app/commands/settings.rs",
            "pinvou3-app/src-tauri/src/features/knowledge/mod.rs",
            "pinvou3-app/src-tauri/src/features/review/mod.rs",
            "pinvou3-app/src-tauri/src/features/runtime_bundle/platform/mod.rs",
            "pinvou3-app/src-tauri/src/features/voice/voice_asr.rs",
            "pinvou3-app/src-tauri/src/features/updater/mod.rs",
            "pinvou3-app/src-tauri/src/features/future_feature/mod.rs",
            "pinvou3-app/src-tauri/src/features/assistant/product_runtime/headless_bridge_contract_tests.rs",
        )
        for path in high_risk_examples:
            self.assertTrue(
                _matches_paths_filter(path, rust_full_paths),
                f"unclassified/high-risk Rust path must run full tests: {path}",
            )

        low_risk_examples = (
            "pinvou3-app/src-tauri/src/features/feedback/mod.rs",
            "pinvou3-app/src-tauri/src/features/personas/mod.rs",
            "pinvou3-app/src-tauri/src/features/pet/platform/detach.rs",
        )
        for path in low_risk_examples:
            self.assertFalse(
                _matches_paths_filter(path, rust_full_paths),
                f"documented low-risk leaf should use the fast route: {path}",
            )

        for workflow_path in (
            ".github/workflows/pr-check.yml",
            ".github/workflows/mac-build.yml",
        ):
            self.assertNotIn(
                workflow_path,
                rust_full_paths,
                "workflow policy changes must not link the full application tests",
            )

        literal_feature_files = [
            path
            for path in rust_full_paths
            if "/src/features/" in path
            and path.endswith(".rs")
            and "*" not in path
        ]
        self.assertEqual(
            literal_feature_files,
            [],
            "rust_full must not enumerate internal feature files",
        )

    def test_rust_modes_run_combined_full_regression_only_for_high_risk(self):
        self.assertIn("merge_group:", self.pr_workflow)
        self.assertIn("ci:full-rust", self.pr_workflow)
        rust_lint = self.pr_workflow.split(
            "\n  rust-lint:", maxsplit=1
        )[1].split("\n  rust-test:", maxsplit=1)[0]
        self.assertIn("RUN_HEAVY_RUST_CHECKS", rust_lint)
        self.assertIn("github.event.pull_request.draft == false", rust_lint)
        self.assertIn("needs.changes.outputs.rust_dependencies == 'true'", rust_lint)

        rust_test = self.workflow.split("\n  rust-test:", maxsplit=1)[1].split(
            "\n  windows-rust-test:", maxsplit=1
        )[0]
        self.assertRegex(
            rust_test,
            r"github\.event_name == 'merge_group'\s*&&\s*"
            r"needs\.changes\.outputs\.rust_full == 'true'",
        )
        self.assertIn(
            "needs.changes.outputs.rust_full == 'true'",
            rust_test,
        )
        self.assertIn(
            "needs.changes.outputs.rust_code == 'true'",
            rust_test,
        )
        self.assertIn(
            "needs.changes.outputs.trusted_backflow != 'true'",
            rust_test,
        )
        self.assertIn("github.event.pull_request.draft == false", rust_test)
        self.assertIn(
            "contains(github.event.pull_request.labels.*.name, 'ci:full-rust')",
            rust_test,
        )
        # Main is a cumulative compile verification and must not depend on
        # adjacent diff paths.
        self.assertIn(
            "github.event_name == 'push' ||",
            rust_test,
        )
        self.assertIn(
            "cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml --no-default-features --lib -- --test-threads=1",
            rust_test,
        )
        # push(main) 只编译暖 cache,不执行测试:MQ 已对同一组合树跑过全量测试;
        # push 恢复暖 cache 后跑全量测试曾连续触发 hosted runner 失联(见 workflow 注释)。
        self.assertIn(
            "- name: cargo test --lib（含 strict_mode 回归；真 bge-m3/vLLM 测试已 #[ignore]）\n"
            "        if: ${{ github.event_name != 'push' }}",
            rust_test,
        )
        self.assertIn(
            "- name: cargo test --lib --no-run (push main 仅编译暖 cache)\n"
            "        if: ${{ github.event_name == 'push' }}",
            rust_test,
        )
        # hosted runner 失联防护(私有仓规格实测为 2-core/7.8GiB RAM,2026-09
        # 探针):编译与执行拆成独立 step(失联后日志全丢,按 step 状态定位阶段),
        # CI 关 DWARF 缩小测试二进制降低链接内存峰值。有效内存由 job 开头的
        # zram 扩容 step(scripts/ci-memory-setup.sh,zram→盘上 swap→zswap→
        # 镜像自带 swap 逐层降级)提供;盘上 swapfile 8G 必建(zram 池 70%
        # 封顶后唯一无界溢出层,/mnt 为 tmpfs 或空间不足时护栏跳过,见
        # ci-memory-setup.sh 头注释);看门狗已删除。
        self.assertIn(
            "- name: cargo test --lib --no-run（编译链接测试二进制）\n"
            "        if: ${{ github.event_name != 'push' }}",
            rust_test,
        )
        self.assertIn(
            "cargo test --test behavior_telemetry (standalone memory-bounded harness)",
            rust_test,
        )
        self.assertIn("--test behavior_telemetry", rust_test)
        self.assertIn("--features behavior-telemetry-test", rust_test)
        self.assertEqual(rust_test.count("--no-default-features"), 4)
        self.assertNotIn(
            "cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib",
            rust_test,
        )
        self.assertIn(
            'sudo --preserve-env=GITHUB_ACTIONS,'
            'PINVOU3_CI_DISABLE_ZRAM bash "${{ github.workspace }}'
            '/scripts/ci-memory-setup.sh"',
            rust_test,
        )
        self.assertNotIn("ci-memguard", self.pr_workflow)
        self.assertNotIn("fallocate -l 3G", rust_test)
        self.assertIn('CARGO_PROFILE_DEV_CODEGEN_UNITS: "16"', rust_test)
        self.assertIn('CARGO_PROFILE_DEV_DEBUG: "0"', rust_test)
        self.assertIn("timeout-minutes: 120", rust_test)
        self.assertIn(
            'RUSTFLAGS: "-C link-arg=-fuse-ld=lld '
            '-C link-arg=-Wl,--thinlto-jobs=1 '
            '-C link-arg=-Wl,--threads=1"',
            rust_test,
        )

    def test_all_linux_jobs_enlarge_runner_memory(self):
        # Every ubuntu-* job must run the zram/swap memory setup right after
        # checkout; Windows/macOS jobs are out of scope (hosted images there
        # have different memory characteristics).
        # Only split at 2-space-indented `key:` lines (job/trigger boundaries),
        # not at deeper indentation.
        setup_step = "- name: Set up zram and swap"
        # rust-lint 例外:该 job 在 Swatinem/rust-cache 恢复阶段与扩容后的
        # 内存配置组合曾稳定杀死 runner(2026-09-12 前后 5/5 复现;后定性为
        # 时间窗环境事故,但 fmt/clippy 非内存密集,保守维持豁免)。
        excluded_jobs = {"rust-lint"}
        blocks = re.split(
            r"\n  (?=[A-Za-z0-9_-]+:\s*$)", self.pr_workflow, flags=re.MULTILINE
        )
        linux_jobs = [
            block
            for block in blocks
            if re.search(r"^    runs-on: ubuntu", block, flags=re.MULTILINE)
            and block.strip().split(":", maxsplit=1)[0] not in excluded_jobs
        ]
        self.assertGreaterEqual(len(linux_jobs), 10)
        for job in linux_jobs:
            job_name = job.strip().split(":", maxsplit=1)[0]
            self.assertIn(
                setup_step,
                job,
                f"ubuntu job '{job_name}' must run scripts/ci-memory-setup.sh",
            )
            self.assertIn(
                '"${{ github.workspace }}/scripts/ci-memory-setup.sh"',
                job,
                f"ubuntu job '{job_name}' must invoke scripts/ci-memory-setup.sh"
                " via an absolute path (some jobs set a run working-directory)",
            )
            # 内核态挂起无法被脚本内部的用户态 timeout 中断,workflow 侧的
            # timeout 240 + 非致命包装是唯一防线(脚本头注释所声称的最后
            # 一道防线)。漏写包装的 job 挂死时会烧满整个 job 时限,守卫
            # 在此强制每个调用点结构一致。
            self.assertIn(
                'run: timeout --kill-after=15 240 sudo --preserve-env='
                'GITHUB_ACTIONS,PINVOU3_CI_DISABLE_ZRAM'
                ' bash "${{ github.workspace }}/scripts/ci-memory-setup.sh"',
                job,
                f"ubuntu job '{job_name}' must hard-cap ci-memory-setup with"
                " 'timeout --kill-after=15 240' (userspace hang backstop) and"
                " pass GITHUB_ACTIONS and the PINVOU3_CI_DISABLE_ZRAM emergency"
                " switch through sudo (sudo env_reset strips them, silently"
                " disabling the script's ::warning annotations and the standing"
                " zram opt-out)",
            )
            self.assertIn(
                '|| echo "::warning::ci-memory-setup',
                job,
                f"ubuntu job '{job_name}' must keep ci-memory-setup non-fatal"
                " (degrade to stock runner memory with a ::warning)",
            )
        # 透传与非致命包装契约必须覆盖所有 workflow 的调用点,不只
        # pr-check:其余文件任何一处缩水,都会让注解/应急开关在该 workflow
        # 静默失效(2026-09-18 复审:缩水 release-packages 调用点时,仅扫
        # pr-check 的守卫 27 项全绿)。
        workflows_dir = ROOT / ".github/workflows"
        workflow_paths = sorted(workflows_dir.glob("*.yml")) + sorted(
            workflows_dir.glob("*.yaml")
        )
        call_line = (
            "run: timeout --kill-after=15 240 sudo --preserve-env="
            "GITHUB_ACTIONS,PINVOU3_CI_DISABLE_ZRAM"
            ' bash "${{ github.workspace }}/scripts/ci-memory-setup.sh"'
        )
        total_call_sites = 0
        for workflow_path in workflow_paths:
            text = workflow_path.read_text(encoding="utf-8")
            call_sites = text.count(
                'bash "${{ github.workspace }}/scripts/ci-memory-setup.sh"'
            )
            if call_sites == 0:
                continue
            total_call_sites += call_sites
            self.assertEqual(
                text.count(call_line),
                call_sites,
                f"{workflow_path.name}: every ci-memory-setup call site must"
                " hard-cap with 'timeout --kill-after=15 240' and pass"
                " GITHUB_ACTIONS and the PINVOU3_CI_* emergency switches"
                " through sudo (sudo env_reset strips them, silently"
                " disabling ::warning annotations and the standing opt-out)",
            )
            self.assertEqual(
                text.count('|| echo "::warning::ci-memory-setup'),
                call_sites,
                f"{workflow_path.name}: every ci-memory-setup call site must"
                " stay non-fatal (degrade to stock runner memory with a"
                " ::warning)",
            )
        self.assertGreaterEqual(
            total_call_sites,
            19,
            "ci-memory-setup call sites across workflows must not shrink"
            " without updating this contract",
        )

    def test_memory_setup_script_pins_annotations_and_honest_degradation(self):
        # #1149 复审跟进:此前该脚本只有 workflow 调用结构被钉住,脚本内容
        # 零覆盖——「声称保留实已删除」这类误导文案的回归现有测试抓不住。
        # 这里钉住三类锚点:zswap 关闭(压缩在内存中的层只允许存在一份)、
        # ::warning:: 注解及其单行约束、诚实失败文案。
        source = (ROOT / "scripts" / "ci-memory-setup.sh").read_text(encoding="utf-8")
        # zram 激活时必须显式关闭 zswap,否则存量 Ubuntu 内核默认开启的
        # zswap 会在 zram 前对每个换出页双重压缩。
        self.assertIn("echo 0 >/sys/module/zswap/params/enabled", source)
        # Actions 下降级告警必须是步骤注解,且注解载荷必须折叠换行与 CR
        # (workflow command 是单行协议,换行处会被截断;runner 的 .NET 行
        # 读取器同样把孤立 CR 当行终止,可伪造第二条 workflow command)。
        self.assertIn('echo "::warning::[memory-setup] ${*//[', source)
        self.assertIn("${*//[$'\\r\\n']/ }", source)
        # 预期可恢复的慢路径(modprobe 首失败、镜像 swap 预激活、镜像 swap
        # as-is 失败后的重试、全层失败后的镜像 swapfile 兜底重试)只进日志,
        # 不得注解化,否则每个 job 都会常驻黄标稀释真告警。函数体全文钉死:
        # 改回 ::warning:: 注解即红。
        self.assertIn(
            'warn_recoverable() { echo "[memory-setup] WARNING: $*" >&2; }',
            source,
        )
        self.assertIn(
            'warn_recoverable "modprobe zram failed${modprobe_err:+: ${modprobe_err}};',
            source,
        )
        self.assertIn(
            'warn_recoverable "no swap active; activating the image swapfile'
            ' before the slow module install"',
            source,
        )
        self.assertIn(
            'warn_recoverable "swapon ${cand} failed as is; trying chmod 600'
            " + mkswap + swapon once\"",
            source,
        )
        self.assertIn(
            'warn_recoverable "no active swap after all layers; trying the'
            ' image-provided swapfile"',
            source,
        )
        # 磁盘 swap 重建失败必须携带诊断,且 removed_note 只允许出现在 rm
        # 实际成功的分支内(裸 `|| removed_note=""` 会被后续无条件赋值覆盖
        # 成死代码);不得再出现「声称保留既有 swap 配置」的旧误导文案。
        self.assertIn(
            "if rm -f /mnt/swapfile; then\n"
            '        removed_note=" (the previous /mnt/swapfile was removed)"\n'
            "      fi",
            source,
        )
        self.assertNotIn("keeping the existing swap configuration", source)

    def test_memory_setup_disk_swap_is_mandatory(self):
        # 2026-09-19 拍板:磁盘 swap 必建,不再是 PINVOU3_CI_ENABLE_DISK_SWAP
        # opt-in(zram 池 70% 封顶后,8G /mnt swapfile 是唯一无界溢出层)。
        # 开关残留即红;主流程必须无条件调用(顶层零缩进),8G 定值一并钉死。
        source = (ROOT / "scripts" / "ci-memory-setup.sh").read_text(encoding="utf-8")
        self.assertNotIn("PINVOU3_CI_ENABLE_DISK_SWAP", source)
        self.assertIn("DISK_SWAP_SIZE_KIB=$((8 * 1024 * 1024))", source)
        self.assertIn(
            'log "provisioning the mandatory /mnt disk swap"\nsetup_disk_swap',
            source,
        )

    def test_fetch_connectors_verifies_checksums_with_loud_diagnostics(self):
        # #1155 复审跟进:fetch-connectors.sh 的 verify_file 在 set -e 下被
        # 三处调用点裸调(check_only 门禁、下载复核、二进制落位前),失配若
        # 零输出退出,门禁只剩 "exit code 1"。诊断内建在 verify_file,任何
        # 调用点都无法静默化;下载前的探测调用自行 >/dev/null 2>&1 抑制。
        source = (ROOT / "scripts" / "fetch-connectors.sh").read_text(
            encoding="utf-8"
        )
        # 失配必须携带期望/实际值(文件缺失/不可读单列,不可读属 I/O 故障
        # 而非哈希差异),并显式 return 1。
        self.assertIn(
            'echo "sha256 校验失败: $1（期望 $2，实际 $(compute_sha256 "$1")）"',
            source,
        )
        self.assertIn('echo "sha256 校验失败: $1（文件缺失，期望 $2）"', source)
        self.assertIn('echo "sha256 校验失败: $1（文件不可读，期望 $2）"', source)
        self.assertIn('[[ "$(compute_sha256 "$1")" == "$2" ]]', source)
        self.assertIn("return 1", source)
        # 三处裸调 + 下载前探测调用,校验点不得悄悄减少。
        self.assertEqual(
            source.count("verify_file "),
            4,
            "fetch-connectors.sh must keep verifying: check_only gate,"
            " pre-download probe, post-download recheck, and pre-install"
            " binary check",
        )


    def test_release_linux_build_jobs_prepare_disk_and_prune_apt(self):
        # 发布构建 job(单盘 hosted runner,x64 开机仅 ~13-14G 可用)必须在
        # 工具链/缓存/依赖落盘之前清理未使用预装 SDK,并在装系统依赖后
        # autoremove + clean;否则冷编译曾把盘写满(ENOSPC,2026-09-13 起)。
        blocks = re.split(
            r"\n  (?=[A-Za-z0-9_-]+:\s*$)", self.release_workflow, flags=re.MULTILINE
        )
        for job_id in ("build-linux-x64", "build-linux-arm64"):
            job = next(
                (b for b in blocks if b.strip().startswith(f"{job_id}:")), None
            )
            self.assertIsNotNone(job, f"release job '{job_id}' not found")
            self.assertIn(
                "python3 scripts/ci-rust-disk.py",
                job,
                f"'{job_id}' must run the disk preparation script",
            )
            self.assertIn(
                "--min-free-gib 24",
                job,
                f"'{job_id}' must fail fast below 24 GiB free",
            )
            # 磁盘准备必须发生在 setup-node 之前(aggressive 档会删
            # /opt/hostedtoolcache,删后 setup-node 会重新下载 Node)。
            self.assertLess(
                job.index("python3 scripts/ci-rust-disk.py"),
                job.index("uses: actions/setup-node"),
                f"'{job_id}' must prepare disk before actions/setup-node",
            )
            self.assertIn("sudo apt-get autoremove -y --purge", job)
            self.assertIn("sudo apt-get clean", job)
        # x64 镜像预装最重(开机可用最少),用 aggressive 档;arm64 镜像轻量
        # 且要保住 setup-node 的工具缓存,只用 base 档。
        x64 = next(b for b in blocks if b.strip().startswith("build-linux-x64:"))
        arm64 = next(b for b in blocks if b.strip().startswith("build-linux-arm64:"))
        self.assertIn("--aggressive", x64)
        self.assertNotIn("--aggressive", arm64)

    def test_audited_redundant_apt_packages_stay_pruned(self):
        # 2026-09-15 探针逐包审计结论(单包模拟安装,安装集合不变即冗余):
        # libgtk-3-dev/libsoup-3.0-dev/libx11-dev/libxi-dev/libxtst-dev 均被
        # libwebkit2gtk-4.1-dev 硬依赖链传递安装,librsvg2-dev 构建不需要
        # (Cargo 无 rsvg crate)。任何 workflow 的安装清单回填它们都会拖慢
        # 依赖安装并占用单盘 runner 的构建磁盘。守卫剥注释后全文扫描:审计
        # 注释允许提及这些名字,但非注释文本出现即拒绝(含多行续行)。
        redundant = (
            "libgtk-3-dev",
            "libsoup-3.0-dev",
            "librsvg2-dev",
            "libx11-dev",
            "libxi-dev",
            "libxtst-dev",
        )
        workflows = sorted(
            list((ROOT / ".github/workflows").glob("*.yml"))
            + list((ROOT / ".github/workflows").glob("*.yaml"))
        )
        self.assertTrue(workflows, "no workflow files found under .github/workflows")
        for workflow in workflows:
            text = _without_yaml_comments(workflow.read_text(encoding="utf-8"))
            for package in redundant:
                self.assertNotIn(
                    package,
                    text,
                    f"{workflow.name}: 审计判定的冗余 apt 包 '{package}' 不得回填"
                    "(由 libwebkit2gtk-4.1-dev 传递安装或构建不需要)",
                )

    def test_all_linux_jobs_pin_the_release_runner_image(self):
        # 镜像版本绝不漂移(单一镜像公约):全部 workflow 的 Linux runner 必须
        # 与 release 构建同基线(ubuntu-22.04 / ubuntu-22.04-arm)。发布二进制
        # 链接构建机的 glibc,测试/校验必须跑在发布同款系统上;镜像升级必须
        # 全仓一次性协调进行(连同 glibc 下限守卫),禁止 ubuntu-latest 等
        # 滚动镜像或个别 job 单独换版。剥掉 YAML 注释再扫:注释里的版本
        # 记述(如迁移说明)不应触发守卫,守卫只管真实 runs-on。
        allowed = {"ubuntu-22.04", "ubuntu-22.04-arm"}
        workflows = sorted(
            list((ROOT / ".github/workflows").glob("*.yml"))
            + list((ROOT / ".github/workflows").glob("*.yaml"))
        )
        self.assertTrue(workflows, "no workflow files found under .github/workflows")
        for workflow in workflows:
            text = _without_yaml_comments(workflow.read_text(encoding="utf-8"))
            self.assertNotIn(
                "ubuntu-latest",
                text,
                f"{workflow.name}: ubuntu-latest 是滚动镜像,违反单一镜像公约,"
                "必须钉到与 release 一致的 ubuntu-22.04",
            )
            for image in sorted(set(re.findall(r"ubuntu-\d+\.\d+(?:-arm)?", text))):
                self.assertIn(
                    image,
                    allowed,
                    f"{workflow.name}: Linux 镜像 '{image}' 偏离发布基线,"
                    "镜像升级必须全仓一次性进行",
                )

    def test_windows_rust_test_parallel_phases_preserve_routing_and_coverage(self):
        # Both matrix legs retain the exact cumulative/high-risk route; splitting
        # compile graphs must not turn either leg into an optional check.
        windows_rust_test = _without_yaml_comments(
            self.pr_workflow.split("\n  windows-rust-test:", maxsplit=1)[1].split(
                "\n  windows-rustup-repair-test:", maxsplit=1
            )[0]
        )
        self.assertIn(
            "github.event_name == 'push' ||", windows_rust_test
        )
        self.assertIn(
            "needs.changes.outputs.rust_full == 'true'",
            windows_rust_test,
        )
        self.assertIn(
            "needs.changes.outputs.backflow_mode == 'windows_full'",
            windows_rust_test,
        )
        self.assertIn(
            "needs.changes.outputs.backflow_mode == 'ordinary'",
            windows_rust_test,
        )
        self.assertRegex(
            windows_rust_test,
            r"github\.event_name == 'merge_group'\s*&&\s*"
            r"needs\.changes\.outputs\.backflow_mode == 'full'",
        )
        self.assertIn(
            "contains(github.event.pull_request.labels.*.name, 'ci:full-rust')",
            windows_rust_test,
        )
        self.assertIn(
            "github.event.pull_request.draft == false", windows_rust_test
        )
        self.assertIn("name: windows-rust-test (${{ matrix.phase }})", windows_rust_test)
        self.assertIn("fail-fast: false", windows_rust_test)
        self.assertIn("max-parallel: 2", windows_rust_test)
        self.assertIn("phase: [all-targets-check, regression]", windows_rust_test)

        self.assertIn(
            "- name: Windows Rust 全目标检查\n"
            "        if: ${{ matrix.phase == 'all-targets-check' }}",
            windows_rust_test,
        )
        self.assertIn("--all-targets --features dev-tools", windows_rust_test)
        self.assertIn(
            "- name: Windows Rust 单元测试链接检查\n"
            "        if: ${{ matrix.phase == 'regression' }}",
            windows_rust_test,
        )
        self.assertIn("--lib --no-run --message-format=json", windows_rust_test)

        # Both isolated legs restore one established namespace, but only the
        # regression/main leg may save. This avoids a second cache writer/key.
        self.assertIn("- name: Cargo cache", windows_rust_test)
        self.assertIn("shared-key: windows-rust-test", windows_rust_test)
        self.assertIn(
            "save-if: ${{ matrix.phase == 'regression' && "
            "github.ref == 'refs/heads/main' }}",
            windows_rust_test,
        )
        self.assertEqual(windows_rust_test.count("shared-key:"), 1)
        self.assertNotIn(
            "if: ${{ matrix.phase == 'regression' }}\n"
            "        uses: Swatinem/rust-cache",
            windows_rust_test,
        )
        self.assertIn("WINDOWS_RUST_CACHE", windows_rust_test)
        self.assertIn("WINDOWS_RUST_TIMING", windows_rust_test)

        # The static updater contract is dependency-free and uses the runner's
        # preinstalled Node. Rust checks still must not install frontend dependencies.
        self.assertNotIn("actions/setup-node", windows_rust_test)
        self.assertNotIn("npm --prefix pinvou3-app ci", windows_rust_test)
        self.assertIn(
            "npm --prefix pinvou3-app run test:windows-static-updater",
            windows_rust_test,
        )

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
        # Cut at the next job boundary: the round-17 macos-rust-check leg
        # legitimately runs `cargo test` (computer_use unit tests) and sits
        # between the regression step and the previous extraction boundary —
        # this assertion only guards the Windows regression against
        # re-invoking cargo, which would re-link the exe and lose the
        # embedded Common-Controls manifest.
        regression = regression.split("\n  macos-rust-check:", maxsplit=1)[0]
        self.assertIn('"$test_exe" "$filter" --test-threads=1', regression)
        self.assertNotIn("cargo test", regression)
        self.assertIn(
            "'connector_introspection_guard_matches_complete_names_only'",
            regression,
            "Windows must execute the PowerShell connector-introspection hook regression",
        )
        for filter_name in (
            "platform::filesystem::tests::windows_",
            "platform::paths::tests::managed_python_",
            "artifact_read_recovers_an_occupied_1177_layout_after_release",
            "artifact_writes_never_expose_partial_utf8_to_concurrent_readers",
            "artifact_public_read_",
            "features::memory::tests::topic_migration_",
            "features::updater::platform::windows::tests::",
        ):
            self.assertEqual(regression.count(filter_name), 1)
        self.assertIn("running [1-9][0-9]* tests?", regression)

        required_gate = self.pr_workflow.split(
            "\n  required-gate:", maxsplit=1
        )[1]
        self.assertIn("- windows-rust-test", required_gate)
        self.assertIn("WINDOWS_RUST_RESULT", required_gate)
        self.assertIn('"windows-rust-test:$WINDOWS_RUST_RESULT"', required_gate)

    def test_windows_rust_split_matches_the_declared_feature_graph(self):
        with TAURI_MANIFEST.open("rb") as manifest_file:
            manifest = tomllib.load(manifest_file)
        features = manifest["features"]
        self.assertEqual(features["default"], ["local-embed"])
        self.assertEqual(features["dev-tools"], [])
        dump_prompt = next(
            binary for binary in manifest["bin"]
            if binary["name"] == "dump_system_prompt"
        )
        self.assertEqual(dump_prompt["required-features"], ["dev-tools"])

        windows_rust_test = self.pr_workflow.split(
            "\n  windows-rust-test:", maxsplit=1
        )[1].split("\n  windows-rustup-repair-test:", maxsplit=1)[0]
        self.assertIn("--all-targets --features dev-tools", windows_rust_test)
        self.assertIn("--lib --no-run --message-format=json", windows_rust_test)
        self.assertNotIn("--lib --no-run --features dev-tools", windows_rust_test)

    def test_windows_release_powershell_scripts_route_to_native_static_contract(self):
        changes = self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  backflow-portability:", maxsplit=1
        )[0]
        route = changes.split("            windows_static_updater:", maxsplit=1)[1].split(
            "            backflow_infra:", maxsplit=1
        )[0]
        paths = _extract_quoted_paths(route)
        for script in (
            "scripts/prepare-windows-release.ps1",
            "scripts/verify-windows-release-online.ps1",
        ):
            self.assertTrue(_matches_paths_filter(script, paths), script)

        job = self.pr_workflow.split(
            "\n  windows-static-updater-contract:", maxsplit=1
        )[1].split("\n  windows-rust-test:", maxsplit=1)[0]
        self.assertIn("runs-on: windows-latest", job)
        self.assertIn("needs.changes.outputs.windows_static_updater == 'true'", job)
        self.assertIn("npm --prefix pinvou3-app run test:windows-static-updater", job)
        required_gate = self.pr_workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertIn("- windows-static-updater-contract", required_gate)
        self.assertIn(
            '"windows-static-updater-contract:$WINDOWS_STATIC_UPDATER_RESULT"',
            required_gate,
        )

    def test_windows_policy_condition_truth_table_and_required_result(self):
        windows_job = self.pr_workflow.split(
            "\n  windows-rust-test:", maxsplit=1
        )[1].split("\n  windows-rustup-repair-test:", maxsplit=1)[0]
        job_expression = re.search(
            r"\n    if: >-\n      \$\{\{(.*?)\n      \}\}",
            windows_job,
            flags=re.DOTALL,
        ).group(1)

        required_gate = self.pr_workflow.split("\n  required-gate:", maxsplit=1)[1]
        required_expression = re.search(
            r"WINDOWS_RUST_REQUIRED_BY_POLICY: \$\{\{ (.*?) \}\}",
            required_gate,
        ).group(1)

        cases = (
            # policy/API/diff failure is full even for a paths-filter-low-risk diff
            ("policy failure ready", "pull_request", False, "full", False, True, False, True, True),
            ("policy failure draft", "pull_request", True, "full", False, True, False, False, False),
            ("incomplete queue entry policy failure", "merge_group", False, "full", False, False, False, True, True),
            ("truncated queue labels policy failure", "merge_group", False, "full", True, True, False, True, True),
            # A provable mixed/non-trusted group keeps ordinary MQ behavior; each PR
            # already took its own ready-stage Windows route.
            ("mixed cumulative group", "merge_group", False, "ordinary", True, True, False, False, False),
            ("non-main low-risk PR", "pull_request", False, "ordinary", False, True, False, False, False),
            ("ordinary high-risk PR", "pull_request", False, "ordinary", True, True, False, True, False),
            ("ci full label", "pull_request", False, "ordinary", False, True, True, True, False),
            ("Windows-native non-Rust", "pull_request", False, "windows_full", False, False, False, True, True),
            ("portable Rust ready", "pull_request", False, "portable_rust", True, True, False, False, False),
            ("pure release ready", "pull_request", False, "release", True, True, False, False, False),
            ("main cumulative", "push", False, "full", False, False, False, True, False),
        )
        for (
            name,
            event_name,
            draft,
            mode,
            rust_full,
            rust_code,
            full_label,
            expected_job,
            expected_policy_required,
        ) in cases:
            with self.subTest(name=name):
                values = dict(
                    event_name=event_name,
                    draft=draft,
                    mode=mode,
                    rust_full=rust_full,
                    rust_code=rust_code,
                    full_label=full_label,
                )
                self.assertEqual(
                    _evaluate_github_condition(job_expression, **values),
                    expected_job,
                )
                policy_required = _evaluate_github_condition(
                    required_expression, **values
                )
                self.assertEqual(policy_required, expected_policy_required)
                # Replay the required-gate shell contract: a policy-required
                # Windows leg accepts only success; without the requirement the
                # per-item loop accepts success|skipped and rejects the rest.
                accepted = [
                    result
                    for result in ("success", "skipped", "failure", "cancelled")
                    if result == "success"
                    or (result == "skipped" and not policy_required)
                ]
                self.assertEqual(
                    accepted,
                    ["success"] if policy_required else ["success", "skipped"],
                )

    def test_pure_release_backflow_runs_contract_only_and_skipped_jobs_are_accepted(self):
        for start, end in (
            ("frontend-test", "relay-test"),
            ("knowledge-rust", "rust-lint"),
            ("rust-lint", "rust-test"),
            ("windows-rustup-repair-test", "windows-codex-runtime-test"),
            ("windows-codex-runtime-test", "windows-runtime-test"),
            ("windows-runtime-test", "macos-codex-runtime-test"),
            ("macos-codex-runtime-test", "required-gate"),
        ):
            job = self.pr_workflow.split(f"\n  {start}:", maxsplit=1)[1].split(
                f"\n  {end}:", maxsplit=1
            )[0]
            self.assertIn("needs.changes.outputs.pure_release_backflow != 'true'", job)

        release_contract = self.pr_workflow.split(
            "\n  release-contract-test:", maxsplit=1
        )[1].split("\n  knowledge-rust:", maxsplit=1)[0]
        self.assertNotIn("pure_release_backflow", release_contract)

        required_gate = self.pr_workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertIn("success|skipped", required_gate)
        self.assertIn("WINDOWS_RUST_REQUIRED_BY_POLICY", required_gate)
        self.assertIn('"$WINDOWS_RUST_RESULT" != success', required_gate)
        self.assertIn("回流策略 $BACKFLOW_MODE 要求完整 Windows Rust", required_gate)
    def test_macos_rust_check_is_wired_into_required_gate(self):
        # Review finding: the new native macOS leg must satisfy the same
        # three-wiring rule as windows-rust-test (needs entry, env backfill,
        # summary-loop entry) -- otherwise removing the job keeps CI green
        # while the gate silently degrades.
        job_body = self.pr_workflow.split("\n  macos-rust-check:", maxsplit=1)[1]
        job = re.split(r"\n  [a-zA-Z]", job_body, maxsplit=1)[0]
        self.assertIn("needs: changes", job)
        self.assertIn("MACOS_RUST_CHECK_RESULT", self.pr_workflow)
        required_gate = self.pr_workflow.split(
            "\n  required-gate:", maxsplit=1
        )[1]
        self.assertIn("- macos-rust-check", required_gate)
        self.assertIn("MACOS_RUST_CHECK_RESULT", required_gate)
        self.assertIn('"macos-rust-check:$MACOS_RUST_CHECK_RESULT"', required_gate)

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

    def test_windows_python_dependency_contract_runs_in_required_native_job(self):
        changes = self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  fast-gate:", maxsplit=1
        )[0]
        # Anchor on the 12-space-indented dorny filter key (not the 8-space outputs
        # mapping) and capture only the entry lines of that one filter group, so
        # moving the ps1 route into another filter fails this assertion.
        windows_codex_filter = re.search(
            r"\n            windows_codex:\n((?:              .*(?:\n|$))+)",
            changes,
        ).group(1)
        self.assertIn(
            "windows_python_dependency_contract.ps1",
            windows_codex_filter,
        )

        windows_job = self.pr_workflow.split(
            "\n  windows-codex-runtime-test:", maxsplit=1
        )[1].split("\n  macos-codex-runtime-test:", maxsplit=1)[0]
        self.assertIn(
            "npm --prefix pinvou3-app run test:windows-runtime",
            windows_job,
        )

        required_gate = self.pr_workflow.split(
            "\n  required-gate:", maxsplit=1
        )[1]
        self.assertIn("- windows-codex-runtime-test", required_gate)

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
        # tauri_effective_config.test.js parses this script's --target dispatch to
        # pin the cross-build Node architecture mapping, and it runs inside
        # release-contract-test; without this entry a PR touching only the bridge
        # script would silently skip that contract gate.
        self.assertIn(
            "- 'pinvou3-app/scripts/prepare-codex-bridge-runtime.sh'",
            release_contract_paths,
        )
        # Keep the l1 section boundary load-bearing: if the split anchor stops
        # matching, this slice would silently grow and make the assertions above
        # vacuous instead of exposing the workflow drift.
        self.assertNotIn(
            "- 'pinvou3-app/src-tauri/tests/l1_dialog_harness.rs'",
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
        self.assertIn("os: [macos-15, ubuntu-22.04, windows-latest]", smoke)


if __name__ == "__main__":
    unittest.main()
