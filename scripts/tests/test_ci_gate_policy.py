import re
import tomllib
import unittest
from fnmatch import fnmatchcase
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PR_WORKFLOW = ROOT / ".github/workflows/pr-check.yml"
RELEASE_WORKFLOW = ROOT / ".github/workflows/release-packages.yml"
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

    def test_pr_submodule_verifier_strictly_matches_the_published_tag(self):
        verifier = PUBLIC_SUBMODULE_VERIFIER.read_text(encoding="utf-8")
        verifier_gate = self.pr_workflow.split(
            "- name: 公开底座 gitlink 可达性", maxsplit=1
        )[1].split("- name: 初始化公共底座 submodule", maxsplit=1)[0]
        self.assertIn("./scripts/verify-public-submodule.sh", verifier_gate)
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
            "bundle_chain",
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
        self.assertIn("postgres:16-alpine", hub_job)
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
            "docker build --file update-release-console/Dockerfile "
            "update-release-console",
            console_job,
        )
        self.assertIn(
            "docker compose --file update-release-console/deploy/compose.yaml "
            "config --quiet",
            console_job,
        )
        self.assertIn("nginx:1.27-alpine nginx -t", console_job)
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
            "cargo clippy --manifest-path pinvou-knowledge/Cargo.toml --lib --bins --no-deps --features server --locked -- -D warnings",
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

    def test_windows_rust_test_parallel_phases_preserve_routing_and_coverage(self):
        # The all-target check and the linked regressions run as two matrix
        # legs of one required job. Splitting the compile graphs must not
        # turn either leg into an optional check, add a cache writer, or drop
        # a step from its leg.
        windows_rust_test = _without_yaml_comments(
            self.pr_workflow.split("\n  windows-rust-test:", maxsplit=1)[1].split(
                "\n  macos-rust-check:", maxsplit=1
            )[0]
        )
        self.assertIn("name: windows-rust-test (${{ matrix.phase }})", windows_rust_test)
        self.assertIn("fail-fast: false", windows_rust_test)
        self.assertIn("max-parallel: 2", windows_rust_test)
        self.assertIn("phase: [all-targets-check, regression]", windows_rust_test)
        # Routing is job level, so it applies to both legs unchanged.
        job_if = windows_rust_test.split("\n    if: >-", 1)[1].split("\n    strategy:", 1)[0]
        self.assertNotIn("matrix.phase", job_if)

        steps = re.split(r"\n      - name: ", windows_rust_test)[1:]
        phase_of = {}
        for step in steps:
            name = step.split("\n", 1)[0].strip()
            match = re.search(r"\n        if: \$\{\{ matrix\.phase == '([a-z-]+)' \}\}", step)
            phase_of[name] = match.group(1) if match else None
        expected = {
            "Windows Rust 全目标检查": "all-targets-check",
            "pinvou-cli Windows compile check": "all-targets-check",
            "Windows Rust 单元测试链接检查": "regression",
            "Windows 测试 exe 嵌入 Common-Controls v6 清单": "regression",
            "Windows 测试二进制导入诊断": "regression",
            "CodeWhale Windows PowerShell regressions": "regression",
            "Windows 原子替换状态机回归": "regression",
            # Shared setup runs on both legs.
            "初始化公共底座 submodule": None,
            "Cargo cache": None,
            "Windows Rust cache baseline diagnostics": None,
        }
        for name, phase in expected.items():
            with self.subTest(step=name):
                self.assertIn(name, phase_of)
                self.assertEqual(phase_of[name], phase)
        self.assertIn("--all-targets --features dev-tools", windows_rust_test)
        self.assertIn("--lib --no-run --locked --message-format=json", windows_rust_test)

        # Both legs restore one established namespace; only regression on
        # main may save, so there is no second writer or new key.
        self.assertEqual(windows_rust_test.count("shared-key:"), 1)
        self.assertIn("shared-key: windows-rust-test", windows_rust_test)
        self.assertIn(
            "save-if: ${{ matrix.phase == 'regression' && "
            "github.ref == 'refs/heads/main' }}",
            windows_rust_test,
        )
        self.assertIn("WINDOWS_RUST_CACHE", windows_rust_test)
        self.assertIn("WINDOWS_RUST_TIMING", windows_rust_test)
        self.assertNotIn("actions/setup-node", windows_rust_test)

    def test_memory_setup_disk_swap_is_mandatory(self):
        # 2026-09-19 拍板:磁盘 swap 必建,不再是 PINVOU3_CI_ENABLE_DISK_SWAP
        # opt-in(zram 池 70% 封顶后,8G /mnt swapfile 是唯一无界溢出层)。
        # 开关残留即红;主流程必须无条件调用(顶层零缩进),8G 定值一并钉死。
        source = (ROOT / "scripts" / "ci-memory-setup.sh").read_text(encoding="utf-8")
        self.assertNotIn("PINVOU3_CI_ENABLE_DISK_SWAP", source)
        self.assertIn("DISK_SWAP_SIZE_KIB=$((8 * 1024 * 1024))", source)
        self.assertIn("DISK_SWAP_SIZE_KIB=$((16 * 1024 * 1024))", source)
        self.assertIn(
            "swapon --show=NAME --noheadings 2>/dev/null | grep -q '/dev/zram'",
            source,
        )
        self.assertIn(
            'warn "apt-get install ${modules_pkg} failed once; retrying"',
            source,
        )
        self.assertIn(
            'warn "apt-get install ${modules_pkg} failed again; giving up on zram layers"',
            source,
        )
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
        self.assertIn("--lib --no-run --locked --message-format=json", windows_rust_test)

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
        self.assertIn("--lib --no-run --locked --message-format=json", windows_rust_test)
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
        # The VC++ temp-preflight pins inside windows_runtime_packaging_contract.test.js
        # target src-tauri/packaging/windows/nsis/vcredist-temp-preflight.ps1, so edits
        # to that file must trigger the only job that runs the pins.
        self.assertIn(
            "pinvou3-app/src-tauri/packaging/windows/nsis/**",
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

    def test_windows_rustup_repair_runs_in_required_native_job(self):
        changes = self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  fast-gate:", maxsplit=1
        )[0]
        self.assertIn(
            "windows_rustup_repair: ${{ steps.filter.outputs.windows_rustup_repair }}",
            changes,
        )
        repair_filter = re.search(
            r"\n            windows_rustup_repair:\n((?:              .*(?:\n|$))+)",
            changes,
        ).group(1)
        for trigger in (
            ".github/workflows/pr-check.yml",
            "pinvou3-app/scripts/ci/**",
            "pinvou3-app/scripts/tauri/build.js",
            "pinvou3-app/tests/windows_rustup_repair_smoke.ps1",
            "pinvou3-app/tests/windows_rust_toolchain_contract.test.js",
            "pinvou3-app/src-tauri/rust-toolchain.toml",
            "pinvou3-app/package.json",
        ):
            self.assertIn(trigger, repair_filter)

        job_body = self.pr_workflow.split(
            "\n  windows-rustup-repair-test:", maxsplit=1
        )[1]
        job = re.split(r"\n  [a-zA-Z]", job_body, maxsplit=1)[0]
        self.assertIn("needs: changes", job)
        self.assertIn(
            "needs.changes.outputs.windows_rustup_repair == 'true'", job
        )
        self.assertIn("runs-on: windows-latest", job)
        self.assertIn("npm --prefix pinvou3-app run test:windows-rustup-repair", job)
        # scripts/ci/** is in no node-test filter, so this job is the only
        # gate that pins ensure-rust-toolchain.ps1 through the node contract
        # test. Dropping the step would silently unpin the repair engine.
        self.assertIn(
            "node --test pinvou3-app/tests/windows_rust_toolchain_contract.test.js",
            job,
        )
        # The contract test must run before the smoke: it fails in seconds on
        # engine drift, while the smoke pays a real toolchain download first.
        self.assertLess(
            job.index(
                "node --test pinvou3-app/tests/windows_rust_toolchain_contract.test.js"
            ),
            job.index("npm --prefix pinvou3-app run test:windows-rustup-repair"),
        )

        # Same three-wiring rule as the other native legs: needs entry, env
        # backfill, and summary-loop entry.
        required_gate = self.pr_workflow.split(
            "\n  required-gate:", maxsplit=1
        )[1]
        self.assertIn("- windows-rustup-repair-test", required_gate)
        self.assertIn(
            "WINDOWS_RUSTUP_REPAIR_RESULT: "
            "${{ needs.windows-rustup-repair-test.result }}",
            required_gate,
        )
        self.assertIn(
            '"windows-rustup-repair-test:$WINDOWS_RUSTUP_REPAIR_RESULT"',
            required_gate,
        )

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
            self.assertIn(
                "github.event.pull_request.draft == false",
                block,
                f"{start} 必须只在非 draft 的 Ready PR 上运行",
            )


    def test_mac_bundle_smoke_is_gated_on_bundle_chain_paths(self):
        # mac-build.yml's bundle_chain filter (before its 2026-10 fold into
        # pr-check's macos-rust-check) decided when to append the universal
        # bundle smoke. After the fold the filter moved into pr-check's
        # changes job, and the smoke step runs only on push with a
        # bundle_chain hit; this test pins: the filter entries are complete
        # (one missing = packaging-chain changes silently skip the smoke),
        # the smoke step consumes that output, and the PR/Queue side does not
        # run it (the PR side gets the lightweight contract from
        # release-contract-test, and a VERSION bump gets full dmg builds from
        # release-packages). VERSION is deliberately absent from the filter:
        # a real version-sync commit always touches
        # tauri.conf.json/package.json, entering bundle_chain through them.
        changes = _without_yaml_comments(
            self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
                "\n  fast-gate:", maxsplit=1
            )[0]
        )
        self.assertIn("bundle_chain:", changes)
        bundle_chain_paths = _extract_quoted_paths(
            changes.split("            bundle_chain:", maxsplit=1)[1]
        )
        self.assertTrue(bundle_chain_paths, "bundle_chain parsed to an empty path list")
        for entry in (
            "pinvou3-app/scripts/tauri/**",
            "pinvou3-app/src-tauri/tauri.conf.json",
            "pinvou3-app/src-tauri/config/**",
            "pinvou3-app/src-tauri/packaging/**",
            "pinvou3-app/src-tauri/resources/**",
            "pinvou3-app/package.json",
            "pinvou3-app/package-lock.json",
            "pinvou3-app/vite.config.mjs",
        ):
            self.assertIn(
                entry,
                bundle_chain_paths,
                f"bundle_chain is missing the packaging-chain entry; this path "
                f"changing would silently skip the bundle smoke: {entry}",
            )

        macos_job = self.pr_workflow.split(
            "\n  macos-rust-check:", maxsplit=1
        )[1].split("\n  windows-codex-runtime-test:", maxsplit=1)[0]
        smoke_step = _without_yaml_comments(
            macos_job.split(
                "- name: Tauri bundle smoke", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn(
            "github.event_name == 'push' && needs.changes.outputs.bundle_chain == 'true'",
            smoke_step,
            "the bundle smoke must run only on push and only for packaging-chain changes",
        )
        self.assertIn(
            "node scripts/tauri/build.js build --target universal-apple-darwin",
            smoke_step,
        )
        self.assertNotIn(
            "continue-on-error",
            smoke_step,
            "the universal bundle smoke must be able to fail main "
            "(a broken macos packaging chain must be red)",
        )

    def test_macos_rust_check_routes_by_rust_filters_not_frontend_paths(self):
        # PR/MQ-side routing: the macos job's if must not consume the
        # frontend/pet outputs (it is a rust gate; frontend changes are
        # validated by frontend-test). On main push the folded mac legs run
        # path-independently (the cumulative main-push contract,
        # frontend-only pushes included), so this pin is about PR routing,
        # not push. The bundle_chain filter must also not treat
        # pure-frontend src/** as a packaging-chain change — but
        # package.json/package-lock.json must stay bundle_chain triggers (the
        # lockfile affects the build). Formerly pinned the standalone
        # mac-build.yml trigger; the workflow was folded into pr-check's
        # macos-rust-check in 2026-10.
        macos_job = self.pr_workflow.split(
            "\n  macos-rust-check:", maxsplit=1
        )[1].split("\n  windows-codex-runtime-test:", maxsplit=1)[0]
        job_if = macos_job.split("\n    if: >-", maxsplit=1)[1].split(
            "\n    runs-on:", maxsplit=1
        )[0]
        for output in ("frontend", "pet"):
            self.assertNotIn(
                f"needs.changes.outputs.{output}",
                job_if,
                f"macos-rust-check is a rust gate; {output} changes must not trigger it",
            )
        # Positive shape: push must stay unconditional and the PR branch must
        # keep the rust-filter routing. Without this, collapsing the whole if
        # to a bare push check deletes PR-side macOS coverage (the only leg
        # that runs macOS-only unit tests on PRs) while every negative pin
        # and step-level pin stays green.
        self.assertIn(
            "github.event_name == 'push' ||",
            job_if,
            "a main push must enter this job unconditionally "
            "(the path-independent cumulative coverage contract)",
        )
        self.assertIn(
            "needs.changes.outputs.rust_full == 'true'",
            job_if,
            "the PR side must keep the rust-filter routing (high-risk draft/ready paths)",
        )

        changes = _without_yaml_comments(
            self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
                "\n  fast-gate:", maxsplit=1
            )[0]
        )
        bundle_tail = changes.split("            bundle_chain:", maxsplit=1)[1]
        # bundle_chain is the last filter group and the extraction below has
        # no lower bound: a group appended after it would leak into the
        # extracted paths (the exact-entry pins would keep passing until a
        # negation false-fails). Pin the boundary instead.
        leaked_groups = [
            line
            for line in bundle_tail.splitlines()[1:]
            if re.fullmatch(r"            [A-Za-z0-9_-]+:", line)
        ]
        self.assertEqual(
            [], leaked_groups,
            "a new filter group after bundle_chain must re-bound this extraction",
        )
        bundle_chain_paths = _extract_quoted_paths(bundle_tail)
        self.assertNotIn(
            "pinvou3-app/src/**",
            bundle_chain_paths,
            "pure-frontend src/** must not enter bundle_chain (avoids needless native builds)",
        )
        self.assertIn("pinvou3-app/package.json", bundle_chain_paths)
        self.assertIn("pinvou3-app/package-lock.json", bundle_chain_paths)

    def test_macos_native_regression_legs_survived_the_mac_build_fold(self):
        # mac-build.yml was deleted in 2026-10 and its push-only coverage was
        # folded into macos-rust-check. Its old failure mode was silent: the
        # workflow kept "running" (cancel-failing) for weeks with zero
        # coverage. Pin each folded leg so removing one cannot go unnoticed.
        macos_job = self.pr_workflow.split(
            "\n  macos-rust-check:", maxsplit=1
        )[1].split("\n  windows-codex-runtime-test:", maxsplit=1)[0]
        # mac-build died inside its own 50-minute cap; the fold replaces it
        # with 180. Reverting to the default (360) or deleting the cap burns
        # a hung runner for hours — the exact waste this PR removes.
        self.assertIn("timeout-minutes: 180", macos_job)

        # Full native lib regression: push-only, serial threads, locked.
        # Comment-stripped: an if-line deleted but kept "alive" in a YAML
        # comment must not satisfy the gate.
        test_step = _without_yaml_comments(
            macos_job.split(
                "- name: macOS full lib tests", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn(
            "if: ${{ github.event_name == 'push' }}", test_step
        )
        self.assertIn(
            "cargo test --lib --features benchmark-hooks --locked -- --test-threads=1",
            test_step,
        )
        # Same silent-no-op guard as the PR leg's computer_use run: a
        # filterless suite that somehow runs zero tests must fail the push.
        self.assertIn("running [1-9][0-9]* tests?", test_step)

        # The universal build needs both darwin targets installed; dropping
        # this step resurfaces as MODULE/target errors on the first
        # bundle_chain push, weeks after the fold (same class as the npm
        # provisioning miss the review caught).
        targets_step = _without_yaml_comments(
            macos_job.split(
                "- name: Install both targets (universal bundle smoke, push only)",
                maxsplit=1,
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn("github.event_name == 'push'", targets_step)
        self.assertIn(
            "rustup target add aarch64-apple-darwin x86_64-apple-darwin",
            targets_step,
        )

        # The release-fast compile smoke and the verify script stay push-only.
        release_fast = _without_yaml_comments(
            macos_job.split(
                "- name: Cargo build (release-fast", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn("github.event_name == 'push'", release_fast)
        self.assertIn(
            "cargo build --profile release-fast --target aarch64-apple-darwin --lib",
            release_fast,
        )
        verify_step = _without_yaml_comments(
            macos_job.split(
                "- name: Verify script", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn("always() && github.event_name == 'push'", verify_step)
        self.assertIn("./scripts/run-mac-verify.sh --skip-test", verify_step)

        # The computer_use filtered run yields to the full leg on push (the
        # full suite executes the same tests) and keeps guarding PR legs.
        computer_use = _without_yaml_comments(
            macos_job.split(
                "- name: macOS computer_use unit tests", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn("github.event_name != 'push'", computer_use)

        # The bundle smoke consumes node_modules (build.js resolves
        # @tauri-apps/cli from it and the beforeBuildCommand runs vite), so the
        # push-gated Node.js setup + npm ci steps must survive alongside it.
        # The 2026-10 review round caught the fold initially dropping them:
        # the first bundle_chain push would have died MODULE_NOT_FOUND while
        # every lock test stayed green.
        node_step = _without_yaml_comments(
            macos_job.split(
                "- name: Node.js (universal bundle smoke, push only)", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn(
            "github.event_name == 'push' && needs.changes.outputs.bundle_chain == 'true'",
            node_step,
            "node setup is only consumed by the bundle_chain-gated smoke; "
            "gating it the same way stops paying npm ci on unrelated pushes",
        )
        self.assertIn("node-version: '24'", node_step)
        self.assertIn(
            "cache-dependency-path: pinvou3-app/package-lock.json", node_step
        )
        npm_step = _without_yaml_comments(
            macos_job.split(
                "- name: Install frontend deps (universal bundle smoke, push only)",
                maxsplit=1,
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn(
            "github.event_name == 'push' && needs.changes.outputs.bundle_chain == 'true'",
            npm_step,
        )
        self.assertIn("npm ci", npm_step)
        # The dual rustup targets feed the same universal build (lipo needs
        # both arches); they are smoke-only provisioning too, so they carry
        # the identical gate — otherwise unrelated pushes pay rustup while
        # node/npm skip.
        dual_target_step = macos_job.split(
            "- name: Install both targets (universal bundle smoke, push only)",
            maxsplit=1,
        )[1].split("\n      - name:", maxsplit=1)[0]
        self.assertIn(
            "github.event_name == 'push' && needs.changes.outputs.bundle_chain == 'true'",
            dual_target_step,
        )
        self.assertIn(
            "rustup target add aarch64-apple-darwin x86_64-apple-darwin",
            dual_target_step,
        )
        # Provisioning must precede the smoke step in the job body.
        self.assertLess(
            macos_job.index("- name: Node.js (universal bundle smoke, push only)"),
            macos_job.index("- name: Tauri bundle smoke"),
        )

        # Deployment-target parity with the release job.
        self.assertIn('MACOSX_DEPLOYMENT_TARGET: "11.0"', macos_job)

    def test_macos_lld_linker_wiring_is_pinned(self):
        # The mac leg's lld linker wiring is easy to lose silently: a dropped
        # env line, a job-level leak into the release legs, or a replaced
        # RUSTFLAGS write all fail no build. The probe cannot self-validate
        # these static pieces, so pin them:
        # - the linker env applied per step to exactly the three
        #   dev-profile test legs (the linux leg pins its own
        #   RUSTFLAGS/DEV_DEBUG exactly; mirror that here),
        # - the job-level env staying free of the linker: the folded
        #   push-only release-fast/bundle legs ship artifacts and are
        #   outside the validated scope, so they must keep default Apple
        #   ld linking,
        # - the probe emitting a step output instead of GITHUB_ENV, so
        #   nothing downstream inherits the probed linker by default,
        # - no global RUSTFLAGS write: the obsolete strip workaround is
        #   removed with the pinned toolchain fix, and a global write could
        #   re-propagate the probe's flags onto the release legs,
        # - the probe staying before the cache step, so a toolchain without
        #   a usable lld fails in seconds ahead of any cache restore or
        #   build work (cargo fingerprints inside target/, not the cache
        #   key, keep the lld-built artifacts consistent).
        macos_job = self.pr_workflow.split(
            "\n  macos-rust-check:", maxsplit=1
        )[1].split("\n  windows-codex-runtime-test:", maxsplit=1)[0]

        # No linker env at job level (comments excluded: they document
        # the scoping and legitimately name the variables).
        job_env = _without_yaml_comments(
            macos_job.split("\n    env:", maxsplit=1)[1].split(
                "\n    steps:", maxsplit=1
            )[0]
        )
        self.assertNotIn("CARGO_PROFILE_DEV_LTO", job_env)
        self.assertNotIn("RUSTFLAGS", job_env)

        # Per-step application to the three dev-profile test legs, identical
        # across them so target/debug artifacts stay incrementally reusable.
        test_leg_names = (
            "- name: macOS Rust all-targets check",
            "- name: macOS computer_use unit tests",
            "- name: macOS full lib tests (native regression, push only)",
        )
        for step_name in test_leg_names:
            body = macos_job.split(step_name, maxsplit=1)[1].split(
                "\n      - name:", maxsplit=1
            )[0]
            self.assertIn('CARGO_PROFILE_DEV_LTO: "thin"', body, step_name)
            self.assertIn(
                "RUSTFLAGS: ${{ steps.mac_lld_probe.outputs.flags }}",
                body,
                step_name,
            )

        # The probe emits a step output, not GITHUB_ENV: nothing between the
        # probe and the consuming test steps — nor the release legs below —
        # may inherit the probed linker silently.
        probe_step = macos_job.split(
            "- name: Probe and export the macOS lld link flags", maxsplit=1
        )[1].split("\n      - name:", maxsplit=1)[0]
        self.assertIn("id: mac_lld_probe", probe_step)
        self.assertIn('echo "flags=$flags" >> "$GITHUB_OUTPUT"', probe_step)
        self.assertIn('echo "probed RUSTFLAGS=$flags"', probe_step)
        self.assertNotIn("$GITHUB_ENV", probe_step)
        self.assertLess(
            macos_job.index("- name: Probe and export the macOS lld link flags"),
            macos_job.index("uses: Swatinem/rust-cache@v2"),
        )

        # Release builds use the profile's strip policy now that the pinned
        # toolchain includes the Mach-O fix. No step may export RUSTFLAGS
        # globally and accidentally carry the dev linker into those builds.
        self.assertNotIn("- name: macOS 27+ strip workaround", macos_job)
        for line in _without_yaml_comments(macos_job).splitlines():
            if "GITHUB_ENV" in line:
                self.assertNotIn("RUSTFLAGS", line)

        # The push-only release legs carry no linker env of their own.
        for step_name in (
            "- name: Cargo build (release-fast",
            "- name: Tauri bundle smoke",
        ):
            body = macos_job.split(step_name, maxsplit=1)[1].split(
                "\n      - name:", maxsplit=1
            )[0]
            self.assertNotIn("RUSTFLAGS", body, step_name)
            self.assertNotIn("CARGO_PROFILE_DEV_LTO", body, step_name)

    def test_main_rust_caches_save_on_failure_and_rust_test_keeps_targets(self):
        # cache-on-failure keeps one failed main run from stranding a
        # namespace cold — mac-build's exact death loop (evicted once, then
        # save-on-success-only kept it cold forever). Saves stay main-only
        # (rust-cache's post step also requires save-if), so PR runs never
        # write caches. rust-test's target cache stays re-enabled: the
        # 2026-08-29/30 restored-target runner deaths were root-caused to
        # runner RAM and absorbed by the zram+swap layers (maintainer
        # ruling, recorded at the cache step) — dropping targets again is a
        # policy change, not cleanup, and must revisit this pin.
        for job_name, end_marker in (
            ("rust-test", "\n  cli-test:"),
            ("windows-rust-test", "\n  macos-rust-check:"),
            ("macos-rust-check", "\n  windows-codex-runtime-test:"),
        ):
            job = self.pr_workflow.split(
                f"\n  {job_name}:", maxsplit=1
            )[1].split(end_marker, maxsplit=1)[0]
            # Official's rust-test job restores the lint warm cache
            # (rust-lint-v2, save-if: false) before its own Cargo test cache
            # step; anchor on the step name so the lint restore is not
            # mistaken for the save-on-failure cache.
            job_scope = (
                job.split("- name: Cargo test cache", maxsplit=1)[1]
                if job_name == "rust-test"
                else job
            )
            cache_step = _without_yaml_comments(
                job_scope.split("uses: Swatinem/rust-cache@v2", maxsplit=1)[1].split(
                    "\n      - name:", maxsplit=1
                )[0]
            )
            self.assertIn("cache-on-failure: true", cache_step, job_name)
            self.assertIn("refs/heads/main", cache_step, job_name)
        rust_test_job = self.pr_workflow.split(
            "\n  rust-test:", maxsplit=1
        )[1].split("\n  cli-test:", maxsplit=1)[0]
        rust_test_cache = _without_yaml_comments(
            rust_test_job.split("- name: Cargo test cache", maxsplit=1)[1].split(
                "uses: Swatinem/rust-cache@v2", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn("shared-key: rust-test-v2", rust_test_cache)
        # The re-enable is the absence of the old cache-targets: false
        # (rust-cache's default keeps target artifacts). Comments stripped:
        # the decision record above mentions the old value historically.
        self.assertNotIn("cache-targets:", rust_test_cache)

    def test_quota_and_toolchain_hardening_stays_pinned(self):
        # 2026-10 audit hardening. Each item is a silent policy change (quota
        # weight or toolchain drift) if removed, with no runtime error to
        # expose it — so each is pinned:
        # - CARGO_INCREMENTAL=0 repo-wide: incremental artifacts are pure
        #   cache-bloat on ephemeral runners under the 10GB quota.
        # - knowledge-rust drops its target cache (workspace compiles cold
        #   inside its 30-minute cap); only ~/.cargo is cached.
        # - The CodeWhale Windows regression resolves its toolchain via the
        #   same rust-toolchain.toml-derived resolution as every other leg
        #   (RUSTUP_TOOLCHAIN beats the directory override file).
        # - Both Cargo.lock drift guards fail loud when a cargo invocation
        #   regenerated a lockfile instead of honoring it (--locked parity).
        self.assertIn('CARGO_INCREMENTAL: "0"', self.pr_workflow)

        knowledge_job = self.pr_workflow.split(
            "\n  knowledge-rust:", maxsplit=1
        )[1].split("\n  rust-lint:", maxsplit=1)[0]
        knowledge_cache = _without_yaml_comments(
            knowledge_job.split(
                "uses: Swatinem/rust-cache@v2", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn("cache-targets: false", knowledge_cache)

        windows_rust_job = self.pr_workflow.split(
            "\n  windows-rust-test:", maxsplit=1
        )[1].split("\n  macos-rust-check:", maxsplit=1)[0]
        self.assertIn(
            "RUSTUP_TOOLCHAIN: ${{ steps.pinned_toolchain.outputs.version }}",
            _without_yaml_comments(windows_rust_job),
        )

        for job_name, end_marker in (
            ("knowledge-rust", "\n  rust-lint:"),
            ("rust-lint", "\n  rust-test:"),
        ):
            job = self.pr_workflow.split(
                f"\n  {job_name}:", maxsplit=1
            )[1].split(end_marker, maxsplit=1)[0]
            self.assertIn(
                "Cargo.lock drift guard", job, job_name,
            )
            self.assertIn(
                "git diff --exit-code -- '**/Cargo.lock'", job, job_name,
            )

    def test_connector_darwin_x64_executes_on_intel(self):
        # darwin-x64 verifies the pinned x86_64 connector CLIs by EXECUTING
        # them. On the arm64 image that step was skipped (can_run: false),
        # leaving sha256/file checks only — a hash-matching but broken binary
        # shipped green. macos-15-intel is the Intel image (sunset ~2027 with
        # the macos-15 generation; the re-homing note lives in the matrix).
        workflow = (
            ROOT / ".github/workflows/connector-verify.yml"
        ).read_text(encoding="utf-8")
        darwin_x64 = workflow.split(
            "- platform: darwin-x64", maxsplit=1
        )[1].split("- platform:", maxsplit=1)[0]
        self.assertIn("runs-on: macos-15-intel", darwin_x64)
        self.assertIn("can_run: true", darwin_x64)
        # The arm64 leg keeps executing on the arm64 image (no coverage lost).
        darwin_arm64 = workflow.split(
            "- platform: darwin-arm64", maxsplit=1
        )[1].split("- platform:", maxsplit=1)[0]
        self.assertIn("runs-on: macos-15", darwin_arm64)
        self.assertIn("can_run: true", darwin_arm64)

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
        # The 2026-10 concurrency fix: only PR runs cancel each other; queue
        # entries and main pushes must never cancel (queue entries carry the
        # required-check contexts the merge queue waits on). Both jobs get
        # explicit caps instead of the 360-minute workflow default.
        self.assertIn(
            "cancel-in-progress: ${{ github.event_name == 'pull_request' }}",
            workflow,
        )
        self.assertIn("timeout-minutes: 10", changes)
        self.assertIn("timeout-minutes: 20", smoke)


if __name__ == "__main__":
    unittest.main()
