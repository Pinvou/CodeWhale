import assert from "node:assert/strict";
import { readFile, mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { execFile } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import { renderReleasePathWorkflows } from "../../scripts/generate-remote-control-release-paths.mjs";
import { test } from "node:test";
import { minimatch } from "minimatch";
import { parse } from "yaml";
import {
  changedPathsRequireRemoteControlRelease, isRemoteControlReleasePath,
  REMOTE_CONTROL_RELEASE_EXACT_PATHS, REMOTE_CONTROL_RELEASE_PREFIXES,
} from "../../scripts/remote-control-release-path-policy.mjs";

function releaseContractPatterns(source) {
  const workflow = parse(source);
  const filterStep = workflow?.jobs?.changes?.steps?.find((step) => step?.id === "filter");
  assert.equal(filterStep?.uses, "dorny/paths-filter@v4", "changes.filter must remain the dorny path router");
  assert.equal(typeof filterStep?.with?.filters, "string", "dorny filter map must be YAML source");
  const filters = parse(filterStep.with.filters);
  assert.ok(Array.isArray(filters?.release_contract), "release_contract filter missing");
  assert.ok(filters.release_contract.every((pattern) => typeof pattern === "string" && pattern.length > 0), "release_contract filter contains a non-string pattern");
  return filters.release_contract;
}

function workflowTriggerPathPatterns(source, eventName) {
  const workflow = parse(source);
  // YAML 1.1 parsers may expose the plain `on` key as boolean true. The
  // repository parser is YAML 1.2, but keep the assertion diagnostic explicit.
  const triggers = workflow?.on ?? workflow?.true ?? workflow?.[true];
  const patterns = triggers?.[eventName]?.paths;
  assert.ok(Array.isArray(patterns), `release workflow ${eventName}.paths missing`);
  assert.ok(patterns.every((pattern) => typeof pattern === "string" && pattern.length > 0), `release workflow ${eventName}.paths contains a non-string pattern`);
  return patterns;
}

function globMatches(pattern, path) {
  // dorny/paths-filter delegates glob evaluation to picomatch. Minimatch's
  // slash-aware `*`/`**` behavior is equivalent for the deliberately simple
  // positive patterns in this release policy and avoids a permissive regex
  // approximation in the contract test itself.
  return minimatch(path, pattern, { dot: true, nocase: false, nonegate: true });
}

function samplesForPrefix(prefix) {
  if (prefix.endsWith("/")) return [`${prefix}direct.ext`, `${prefix}deep/direct.ext`, `${prefix}deep/nested/file.ext`];
  return [`${prefix}direct.ext`, `${prefix}scope/direct.ext`, `${prefix}scope/deep/nested.ext`];
}

const criticalPaths = [
  "pinvou3-app/src-tauri/src/app/commands/desktop_remote_action.rs",
  "pinvou3-app/src-tauri/src/app/commands/desktop_remote_capabilities.rs",
  "pinvou3-app/src-tauri/src/app/commands/desktop_remote_model.rs",
  "pinvou3-app/src-tauri/src/app/commands/desktop_remote_proxy.rs",
  "pinvou3-app/src-tauri/src/app/commands/desktop_remote_task.rs",
  "pinvou3-app/src-tauri/src/app/commands/remote_control.rs",
  "pinvou3-app/src-tauri/src/app/commands/mod.rs",
  "pinvou3-app/src-tauri/src/features/remote_control/controller/task.rs",
  "pinvou3-app/src-tauri/src/platform/capabilities.rs",
  "pinvou3-app/src-tauri/build.rs",
  "pinvou3-app/src-tauri/build_support/desktop_controller.rs",
  "pinvou3-app/src-tauri/capabilities/default.json",
  "pinvou3-app/src-tauri/src/app/desktop_remote_composition.rs",
  "pinvou3-app/src-tauri/src/features/sessions/remote_proxy/mod.rs",
  "pinvou3-app/src-tauri/src/features/sessions/application/ports.rs",
  "pinvou3-app/src-tauri/src/features/codex_acp/runtime.rs",
  "pinvou3-app/src-tauri/src/features/files/attachment_upload.rs",
  "pinvou3-app/scripts/codex-bridge-runtime/package.json",
  "pinvou3-app/scripts/codex-bridge-runtime/package-lock.json",
  "pinvou3-app/scripts/tauri/codex-bridge.js",
  "pinvou3-app/scripts/prepare-codex-bridge-runtime.sh",
  "pinvou3-app/src-tauri/src/features/assistant/runtime_model.rs",
  "pinvou3-app/src-tauri/src/core/panic_scope.rs",
  "pinvou3-app/src-tauri/src/core/panic_hooks.rs",
  "pinvou3-app/src-tauri/src/features/assistant/image_capability.rs",
  "pinvou3-app/src-tauri/src/platform/prefs/model.rs",
  "pinvou3-app/src-tauri/src/platform/credential_store.rs",
  "pinvou3-app/src-tauri/src/features/assistant/turn_shell_tasks.rs",
  "pinvou3-app/src-tauri/Cargo.lock",
  "pinvou3-app/scripts/tauri/startup-window-config.js",
  "pinvou3-app/scripts/tauri/require-wrapper.js",
  "pinvou3-app/scripts/tauri/npm-invocation.js",
  "remote-control-relay/server.js",
  "pinvou3-app/src-tauri/packaging/windows/nsis/scripts/stage-resources.ps1",
  "pinvou3-app/src/features/remote-control/ControlledDevicePanel.jsx",
  "pinvou3-app/src/features/chat/ChatView.jsx",
  "pinvou3-app/src/features/settings/SettingsView.jsx",
  "pinvou3-app/src/app/main.jsx",
  "pinvou3-app/src/components/Toggle.jsx",
  "pinvou3-app/src/platform/tauri/bridge/desktop-remote-control.js",
  "pinvou3-app/tests/remote_control_feature_gate_contract.test.mjs",
  "pinvou3-app/tests/remote_control_real_ui_smoke_contract.test.mjs",
  "remote-control-relay/docs/schemas/remote-control-release-evidence.v1.schema.json",
  "remote-control-relay/scripts/controller-release-gate.mjs",
  "remote-control-relay/scripts/validate-controller-release-evidence.mjs",
  "remote-control-relay/scripts/release-evidence-artifacts.mjs",
  "remote-control-relay/scripts/verify-github-workflow-run.cjs",
  "remote-control-relay/test/controller-release-evidence.test.js",
  "remote-control-relay/test/release-evidence-artifacts.test.js",
  "remote-control-relay/test/controller-release-source-binding.test.js",
  "remote-control-relay/test/installer-spdx.test.js",
  "scripts/create-installer-spdx.mjs",
  "scripts/run-windows-remote-control-e2e.ps1",
  "scripts/windows-e2e-orchestrator-helpers.ps1",
  "scripts/ci-fixture-process.py",
  "scripts/tests/test_ci_fixture_process.py",
  "remote-control-relay/test/windows-release-owned-service.test.js",
  "remote-control-relay/test/fixtures/windows-owned-driver.cs",
  "scripts/release-owned-process.py",
  "scripts/windows-owned-process.cs",
  "remote-control-relay/scripts/release-process.mjs",
  "remote-control-relay/test/release-process.test.js",
  ".github/workflows/windows-remote-control-build-evidence.yml",
  ".github/workflows/windows-remote-control-evidence.yml",
  ".github/workflows/release-packages.yml",
  ".github/workflows/pr-check.yml",
];

test("a change to one remote dispatch or authority entry routes all release checks", async () => {
  const pr = await readFile(new URL("../../.github/workflows/pr-check.yml", import.meta.url), "utf8");
  const release = await readFile(new URL("../../.github/workflows/release-packages.yml", import.meta.url), "utf8");
  const routes = [
    ["PR release_contract", releaseContractPatterns(pr)],
    ["release push", workflowTriggerPathPatterns(release, "push")],
    ["release pull_request", workflowTriggerPathPatterns(release, "pull_request")],
  ];
  // Derive the cases from the production dispatch and composition boundaries,
  // independently of the policy list so a missing entry fails this test.
  for (const path of [
    "pinvou3-app/src-tauri/src/lib.rs",
    "pinvou3-app/src-tauri/src/features/sessions/application/dispatch_remote.rs",
    "pinvou3-app/src-tauri/src/features/sessions/application/project_remote.rs",
    "pinvou3-app/src-tauri/src/features/sessions/application/ports.rs",
    "pinvou3-app/src-tauri/src/features/sessions/application/submit_controlled.rs",
    "pinvou3-app/src-tauri/src/features/assistant/remote_turn.rs",
    "pinvou3-app/src-tauri/src/features/codex_acp/mod.rs",
    "pinvou3-app/src-tauri/src/features/codex_acp/runtime.rs",
    "pinvou3-app/src-tauri/src/features/codex_acp/operation_gate.rs",
    "pinvou3-app/src-tauri/src/features/codex_acp/remote_ownership.rs",
    "pinvou3-app/src-tauri/src/features/codex_acp/remote_turn.rs",
    "pinvou3-app/src-tauri/src/features/codex_acp/events.rs",
    "pinvou3-app/src-tauri/src/features/codex_acp/lifecycle.rs",
    "pinvou3-app/src-tauri/src/features/codex_acp/install.rs",
    "pinvou3-app/src-tauri/src/features/assistant/engine_pool.rs",
    "pinvou3-app/src-tauri/src/features/assistant/engine.rs",
    "pinvou3-app/src-tauri/src/features/assistant/skill_materialization.rs",
    "pinvou3-app/src-tauri/src/features/assistant/platform/bridge.rs",
    "pinvou3-app/src-tauri/src/features/assistant/forwarder.rs",
    "pinvou3-app/src-tauri/src/features/codex_acp/stall.rs",
    "pinvou3-app/src-tauri/src/platform/prefs/mod.rs",
    "pinvou3-app/src-tauri/src/features/sessions/validators.rs",
    "pinvou3-app/src-tauri/src/features/sessions/store.rs",
    "pinvou3-app/src-tauri/src/features/sessions/mode_state.rs",
    "pinvou3-app/src-tauri/src/core/mode_state.rs",
    "pinvou3-app/src-tauri/src/features/sessions/retention.rs",
    "pinvou3-app/src-tauri/src/features/files/ingest_core.rs",
    "pinvou3-app/src-tauri/src/features/files/attachment_upload.rs",
    "pinvou3-app/scripts/codex-bridge-runtime/package.json",
    "pinvou3-app/scripts/codex-bridge-runtime/package-lock.json",
    "pinvou3-app/scripts/tauri/codex-bridge.js",
    "pinvou3-app/scripts/prepare-codex-bridge-runtime.sh",
    "pinvou3-app/src-tauri/src/features/assistant/runtime_model.rs",
    "pinvou3-app/src-tauri/src/core/panic_scope.rs",
    "pinvou3-app/src-tauri/src/core/panic_hooks.rs",
    "pinvou3-app/src-tauri/src/features/assistant/image_capability.rs",
    "pinvou3-app/src-tauri/src/platform/prefs/model.rs",
    "pinvou3-app/src-tauri/src/platform/credential_store.rs",
    "pinvou3-app/src-tauri/src/features/assistant/turn_shell_tasks.rs",
    "pinvou3-app/src-tauri/Cargo.lock",
    "pinvou3-app/scripts/tauri/startup-window-config.js",
    "pinvou3-app/scripts/tauri/require-wrapper.js",
    "pinvou3-app/scripts/tauri/npm-invocation.js",
    "pinvou3-app/src-tauri/src/features/codex_acp/attachments.rs",
    "pinvou3-app/src-tauri/src/features/assistant/attachments.rs",
    "pinvou3-app/src-tauri/src/features/files/published_attachment.rs",
    "pinvou3-app/src-tauri/src/features/files/limits.rs",
    "pinvou3-app/src-tauri/src/features/files/platform/model_reference.rs",
    "pinvou3-app/src-tauri/src/platform/filesystem.rs",
    "pinvou3-app/src-tauri/src/platform/filesystem/linux_publication.rs",
    "pinvou3-app/src-tauri/src/features/files/ingest_deps.rs",
    "pinvou3-app/src-tauri/src/features/files/platform/mod.rs",
    "pinvou3-app/src-tauri/src/features/files/platform/office_stream.rs",
    "pinvou3-app/src-tauri/src/features/files/platform/office_stream.py",
    "pinvou3-app/src-tauri/src/features/files/ingest_diagnostics.rs",
    "pinvou3-app/src-tauri/src/core/model_wire.rs",
    "pinvou3-app/src-tauri/src/features/sessions/mod.rs",
    "pinvou3-app/src-tauri/src/features/files/ingest_office.rs",
    "pinvou3-app/src-tauri/src/features/files/ingest_email.rs",
    "pinvou3-app/src-tauri/src/features/files/ingest_archive.rs",
    "pinvou3-app/src-tauri/src/features/files/platform/archive_entry.rs",
    "pinvou3-app/src-tauri/src/features/files/visual_preview.rs",
    "pinvou3-app/src/index.html",
    "pinvou3-app/vite.config.mjs",
    "pinvou3-app/src/platform/tauri/bridge/sessions.js",
    "pinvou3-app/src/platform/tauri/bridge/chat-events.js",
    "pinvou3-app/src/shared/bridge-shared-helpers.js",
    "pinvou3-app/src/platform/tauri/bridge/chat.js",
    "pinvou3-app/src/platform/tauri/bridge/interaction.js",
    "pinvou3-app/src-tauri/src/platform/paths.rs",
    "pinvou3-app/src-tauri/src/platform/process.rs",
    "pinvou3-app/src-tauri/src/features/files/text_decode.rs",
    "pinvou3-app/src-tauri/src/platform/os/windows/windows_path.rs",
    "pinvou3-app/src-tauri/src/platform/os/windows/windows_system.rs",
    "pinvou3-app/src-tauri/src/platform/os/posix.rs",
    "pinvou3-app/src-tauri/src/platform/os/linux/linux_system.rs",
    "pinvou3-app/src-tauri/src/features/files/spreadsheet_structure.rs",
    "pinvou3-app/scripts/vite-runtime-assets.mjs",
    "pinvou3-app/src-tauri/tauri.conf.json",
    "pinvou3-app/src-tauri/Cargo.toml",
    "pinvou3-app/src-tauri/src/main.rs",
    "pinvou3-app/scripts/tauri/effective-config.js",
    "pinvou3-app/src-tauri/src/features/files/ingest_pdf.rs",
    "pinvou3-app/src-tauri/src/features/files/file_ingest.rs",
    "pinvou3-app/src-tauri/src/platform/os/linux/linux_path.rs",
    "pinvou3-app/src-tauri/src/platform/os/interface/system.rs",
    "pinvou3-app/src-tauri/src/platform/os/interface/path.rs",
    "pinvou3-app/src/shared/chunked-file-upload.js",
    "pinvou3-app/src-tauri/src/platform/os/mod.rs",
    "pinvou3-app/src-tauri/src/platform/os/interface/mod.rs",
    "pinvou3-app/src-tauri/src/platform/os/linux/mod.rs",
    "pinvou3-app/src-tauri/src/platform/os/windows/mod.rs",
    "pinvou3-app/src-tauri/src/features/assistant/platform/mod.rs",
    "pinvou3-app/src-tauri/src/features/sessions/application/mod.rs",
    "pinvou3-app/src-tauri/src/features/assistant/mod.rs",
    "pinvou3-app/src-tauri/src/features/files/mod.rs",
    "pinvou3-app/src-tauri/src/app/mod.rs",
    "pinvou3-app/src-tauri/src/platform/mod.rs",
    "pinvou3-app/src-tauri/src/core/mod.rs",
    "pinvou3-app/src-tauri/src/features/mod.rs",
    "pinvou3-app/scripts/tauri/build.js",
    "pinvou3-app/scripts/tauri/platform-config.js",
  ]) {
    assert.equal(changedPathsRequireRemoteControlRelease(`${path}\n`), true, `release diff misses ${path}`);
    for (const [route, patterns] of routes) {
      assert.ok(patterns.some((pattern) => globMatches(pattern, path)), `${route} misses ${path}`);
    }
  }
  for (const path of [
    "pinvou3-app/src-tauri/src/features/assistant/multiagent_regression_tests.rs",
    "pinvou3-app/src-tauri/src/features/codex_acp/login.rs",
    "pinvou3-app/src-tauri/src/features/sessions/application/admission.rs",
  ]) {
    assert.equal(changedPathsRequireRemoteControlRelease(`${path}\n`), false, `release diff overmatches ${path}`);
    for (const [route, patterns] of routes) {
      assert.ok(!patterns.some((pattern) => globMatches(pattern, path)), `${route} overmatches ${path}`);
    }
  }
});

test("single release path policy covers every remote-control security boundary", () => {
  for (const path of criticalPaths) assert.equal(isRemoteControlReleasePath(path), true, path);
  assert.equal(changedPathsRequireRemoteControlRelease(`docs/unrelated.md\n${criticalPaths[0]}\n`), true);
  assert.equal(changedPathsRequireRemoteControlRelease("docs/unrelated.md\nREADME.md\n"), false);
});

test("official desktop releases enable Windows and Linux and keep macOS disabled without repository variables", async () => {
  const source = await readFile(new URL("../../.github/workflows/release-packages.yml", import.meta.url), "utf8");
  const { jobs } = parse(source);
  const flag = "PINVOU_DESKTOP_CONTROLLER_ENABLED";
  const gateJobName = "remote-control-release-gate";
  const pinnedCompileJobs = {
    "build-windows-x64": "1",
    "build-linux-x64": "1",
    "build-linux-arm64": "1",
    "build-linux-loongarch64": "1",
    "build-macos-universal": "0",
  };
  for (const [name, value] of Object.entries({ [gateJobName]: "1", ...pinnedCompileJobs })) {
    const job = jobs[name];
    assert.equal(job?.env?.[flag], value, `${name} must explicitly select its build capability`);
    for (const step of job.steps) {
      assert.ok(step.env?.[flag] === undefined || step.env[flag] === value,
        `${name}: ${step.name || step.uses} must not override the platform policy`);
      // 评审 round-3 P2-1：钉扎 job 的 run 文本不得出现 flag 字面量——
      // `FLAG=0 node …` 前缀、`export FLAG=…` 与 GITHUB_ENV 注入都属
      // 行内改写，job 级 env 钉扎对此无感知；开关只允许经钉死的 job env
      // 选择。
      assert.doesNotMatch(step.run ?? "", new RegExp(flag),
        `${name}: ${step.name} must not touch ${flag} inside run text; `
          + "the capability is selected only by the pinned job env");
    }
  }
  assert.doesNotMatch(source, /vars\.PINVOU_DESKTOP_CONTROLLER_ENABLED/u);
  const windows = jobs["build-windows-x64"];
  assert.ok(windows.needs?.includes("remote-control-release-gate"), "build-windows-x64 must depend on remote-control-release-gate");
  assert.match(windows.if ?? "", /needs\.remote-control-release-gate\.result == 'success'/u);
  // Reverse inventory: a release build without the flag silently compiles the
  // controller out (build.rs defaults to '0'), so the whitelist above must be
  // exhaustive. Every job whose steps invoke a desktop compile entry point has
  // to be pinned above, and every pinned job has to actually compile, so the
  // two sets must stay equal; the evidence gate is inventoried separately by
  // its controller-release-gate.mjs invocation.
  // 评审 round-2 P2：识别面不止 run 内三种子串——(1) npx/cargo/yarn 等非 npm
  // 标记的构建命令；(2) step/job env 注入的命令文本；(3) 经 uses 委托本地可
  // 复用工作流的影子构建/门禁。识别不到的新 job 由下方全量登记兜底 fail-closed。
  const desktopCompileMarkers = [
    "scripts/tauri/build.js build",
    "npm run build",
    "yarn build",
    "tauri build",
    "scripts/ci-build-linux-loongarch64.sh",
  ];
  const isDesktopCompileStep = (step, jobEnv = {}) => {
    const run = typeof step.run === "string" ? step.run : "";
    const uses = typeof step.uses === "string" ? step.uses : "";
    const envText = [
      ...Object.values(step.env ?? {}),
      ...Object.values(jobEnv ?? {}),
    ].map((value) => String(value)).join("\n");
    return desktopCompileMarkers.some((marker) => run.includes(marker) || envText.includes(marker))
      || /\.ya?ml$/u.test(uses);
  };
  const compileJobNames = Object.keys(jobs)
    .filter((name) => (jobs[name]?.steps ?? []).some((step) => isDesktopCompileStep(step, jobs[name].env)))
    .sort();
  assert.deepEqual(
    compileJobNames,
    Object.keys(pinnedCompileJobs).sort(),
    "every desktop compile job in release-packages.yml must be pinned to an explicit "
      + `${flag} value; unpinned compile jobs would ship official installers `
      + "without the desktop remote control capability",
  );
  const gateJobNames = Object.keys(jobs)
    .filter((name) => (jobs[name]?.steps ?? []).some((step) => (
      typeof step.run === "string" && step.run.includes("remote-control-relay/scripts/controller-release-gate.mjs")
    )));
  assert.deepEqual(gateJobNames, [gateJobName], "controller-release-gate.mjs may only run inside the pinned gate job");
  // Fail-closed 全量登记：release-packages.yml 的每一个 job 都必须在册
  // （编译 job 见 pinnedCompileJobs，门禁 job 见 gateJobName，其余为下表
  // 辅助 job）。任何新增 job 未登记即转红——包括 run 内无标记的包装脚本
  // 形态（评审 round-2 变异实证 1）。
  const registeredAuxJobs = [
    "check-version-bump",
    "validate-windows-runtime-pr",
  ];
  const registeredJobNames = [
    gateJobName,
    ...registeredAuxJobs,
    ...Object.keys(pinnedCompileJobs),
  ].sort();
  assert.deepEqual(
    Object.keys(jobs).sort(),
    registeredJobNames,
    "every job in release-packages.yml must be registered in this inventory "
      + "(compile jobs in pinnedCompileJobs, the evidence gate by name, the rest "
      + "in registeredAuxJobs); unknown jobs fail closed so wrapper-script or "
      + "reusable-workflow shapes cannot silently escape the platform policy",
  );
  // 可复用工作流委托禁令：影子 gate/构建工作流在 uses 上即可见，直接禁止
  // （评审 round-2 变异实证 3）；外部 action（owner/action@版本）不受影响。
  // 评审 round-3 P2-2：GitHub 执行本地可复用工作流的唯一合法形态是 job 级
  // `jobs.<id>.uses`；step 级 uses 根本不会被执行。两种形态一并禁止。
  const localReusableWorkflowUsers = Object.entries(jobs)
    .flatMap(([name, job]) => {
      const delegated = [];
      if (typeof job.uses === "string" && /\.ya?ml$/u.test(job.uses)) {
        delegated.push(`${name}: ${job.uses}`);
      }
      for (const step of job.steps ?? []) {
        if (typeof step.uses === "string" && /\.ya?ml$/u.test(step.uses)) {
          delegated.push(`${name}: ${step.uses}`);
        }
      }
      return delegated;
    });
  assert.deepEqual(localReusableWorkflowUsers, [],
    "release-packages.yml must not delegate build or gate logic to local reusable "
      + "workflows (job-level or step-level uses); inline steps keep the "
      + "compile/gate inventory exhaustive");
});

test("release platform and compiled capability contracts also run when Windows jobs are disabled", async () => {
  const { jobs } = parse(await readFile(new URL("../../.github/workflows/pr-check.yml", import.meta.url), "utf8"));
  const steps = jobs["release-contract-test"].steps;
  const dependencies = steps.find((step) => step.run?.includes("npm --prefix remote-control-relay ci"));
  assert.ok(dependencies, "YAML policy tests require Relay dependencies on Linux too");
  assert.equal(dependencies.if, undefined);
  // Match by test-file presence instead of pinning one node --test invocation:
  // reordering the runner arguments or splitting the contract step in two must
  // stay green, as long as every contract file runs ungated after the Relay
  // dependencies install.
  const contract = steps.find((step) => step.run?.includes("remote-control-relay/test/release-path-policy.test.js"));
  assert.ok(contract, "the shared platform contract must run on every release-contract matrix leg");
  assert.equal(contract.if, undefined);
  for (const companion of [
    "pinvou3-app/tests/remote_control_feature_gate_contract.test.mjs",
    "pinvou3-app/tests/desktop_remote_loaded_capability.test.mjs",
  ]) {
    assert.ok(steps.some((step) => step.run?.includes(companion)),
      `release-contract-test must execute ${companion} on every matrix leg`);
  }
  assert.ok(steps.indexOf(dependencies) < steps.indexOf(contract));
});

test("PR and release workflows execute or route every policy-sensitive change", async () => {
  const release = await readFile(new URL("../../.github/workflows/release-packages.yml", import.meta.url), "utf8");
  const pr = await readFile(new URL("../../.github/workflows/pr-check.yml", import.meta.url), "utf8");
  const releaseWorkflow = parse(release);
  const prWorkflow = parse(pr);
  const remoteGateSteps = releaseWorkflow.jobs["remote-control-release-gate"].steps;
  assert.equal(remoteGateSteps.find((step) => step?.uses?.startsWith("actions/checkout@"))?.with?.["persist-credentials"], false);
  const buildEvidence = parse(await readFile(new URL("../../.github/workflows/windows-remote-control-build-evidence.yml", import.meta.url), "utf8"));
  assert.equal(buildEvidence.jobs["protected-windows-installer"].env.PINVOU_DESKTOP_CONTROLLER_ENABLED, "1");
  assert.doesNotMatch(JSON.stringify(buildEvidence), /WriteAllBytes|resources\/bge-m3/u);
  const windowsSteps = releaseWorkflow.jobs["build-windows-x64"].steps;
  assert.equal(windowsSteps.find((step) => step?.run === "npm run build:nsis:local")?.if, "env.PINVOU_DESKTOP_CONTROLLER_ENABLED != '1'");
  assert.match(windowsSteps.find((step) => step?.name === "Reuse the exact qualified signed installer").run, /New-Item -ItemType Directory/u);
  const remoteGateRendered = JSON.stringify(remoteGateSteps);
  const setupIndex = remoteGateSteps.findIndex((step) => step?.uses?.startsWith("actions/setup-node@"));
  const installIndex = remoteGateSteps.findIndex((step) => step?.run?.includes('npm --prefix remote-control-relay ci'));
  const gateIndex = remoteGateSteps.findIndex((step) => step?.run?.includes('node remote-control-relay/scripts/controller-release-gate.mjs'));
  assert.ok(setupIndex >= 0 && installIndex > setupIndex && gateIndex > installIndex);
  assert.equal(remoteGateSteps[setupIndex].if, "env.PINVOU_DESKTOP_CONTROLLER_ENABLED == '1'");
  assert.equal(remoteGateSteps[installIndex].if, "env.PINVOU_DESKTOP_CONTROLLER_ENABLED == '1'");
  assert.match(remoteGateSteps[installIndex].run, /npm --prefix remote-control-relay ci --prefer-offline --no-audit --ignore-scripts/u);
  assert.match(remoteGateRendered, /node remote-control-relay\/scripts\/controller-release-gate\.mjs/u);
  const prContractRendered = JSON.stringify(prWorkflow.jobs["release-contract-test"].steps);
  assert.match(prContractRendered, /npm --prefix remote-control-relay ci --prefer-offline --no-audit --ignore-scripts/u);
  const prReleasePatterns = releaseContractPatterns(pr);
  const releasePushPatterns = workflowTriggerPathPatterns(release, "push");
  const releasePullRequestPatterns = workflowTriggerPathPatterns(release, "pull_request");
  assert.match(release, /node scripts\/remote-control-release-path-policy\.mjs/u);
  for (const marker of [
    "pinvou3-app/src/features/remote-control/**", "pinvou3-app/src-tauri/src/features/remote_control/**",
    "pinvou3-app/src-tauri/src/app/commands/**", "remote-control-relay/**",
    "pinvou3-app/src-tauri/capabilities/default.json", "pinvou3-app/src-tauri/packaging/windows/**",
    "scripts/remote-control-release-path-policy.mjs", ".github/workflows/windows-remote-control-evidence.yml",
  ]) {
    assert.ok(pr.includes(marker), `PR path filter misses ${marker}`);
  }
  for (const executableTest of [
    "release-path-policy.test.js", "release-evidence-artifacts.test.js", "controller-release-source-binding.test.js", "installer-spdx.test.js", "github-workflow-run-identity.test.js",
    "windows-e2e-orchestrator-contract.test.js", "windows-e2e-orchestrator-helpers.test.js",
    "windows-e2e-path-guard.test.js", "remote_control_real_ui_smoke_contract.test.mjs",
    "remote_control_feature_gate_contract.test.mjs", "nsis_resource_staging_contract.test.js",
  ]) assert.ok(pr.includes(executableTest), `release-contract-test does not execute ${executableTest}`);
  const evidenceStep = prWorkflow.jobs["release-contract-test"].steps.find((step) => step?.run?.includes("remote-control-relay/test/release-evidence-artifacts.test.js"));
  assert.ok(evidenceStep, "artifact IO suite must be selected by release-contract-test");
  const evidenceCommand = evidenceStep.run.split("\n").find((line) => line.includes("remote-control-relay/test/release-evidence-artifacts.test.js"));
  assert.match(evidenceCommand, /^python scripts\/ci-fixture-process\.py --receipt "\$\{\{ runner\.temp \}\}\/remote-evidence-contract-\$\{\{ github\.run_id \}\}-\$\{\{ github\.run_attempt \}\}\.json" -- node --test /u);
  const lines = evidenceStep.run.trim().split("\n");
  assert.equal(lines[lines.indexOf(evidenceCommand) + 1].trim(), "if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }", "supervisor failure must fail the actual Windows step");
  for (const filter of [
    "remote-control-relay/scripts/verify-github-workflow-run.cjs",
    "remote-control-relay/test/release-path-policy.test.js",
    "remote-control-relay/test/windows-e2e-*.test.js",
    "pinvou3-app/tests/nsis_resource_staging_contract.test.js",
  ]) assert.ok(prReleasePatterns.some((pattern) => globMatches(pattern, filter.replace("*", "orchestrator-contract"))), `release-contract filter misses ${filter}`);
  const samples = [
    ...REMOTE_CONTROL_RELEASE_EXACT_PATHS,
    ...REMOTE_CONTROL_RELEASE_PREFIXES.flatMap(samplesForPrefix),
  ];
  for (const sample of samples) {
    assert.equal(isRemoteControlReleasePath(sample), true, `policy itself misses generated sample ${sample}`);
    assert.ok(prReleasePatterns.some((pattern) => globMatches(pattern, sample)), `dorny release_contract misses policy path ${sample}`);
    assert.ok(releasePushPatterns.some((pattern) => globMatches(pattern, sample)), `release push.paths misses policy path ${sample}`);
    assert.ok(releasePullRequestPatterns.some((pattern) => globMatches(pattern, sample)), `release pull_request.paths misses policy path ${sample}`);
  }
  assert.ok(prReleasePatterns.some((pattern) => globMatches(pattern, "remote-control-relay/server.js")));
  assert.ok(prReleasePatterns.some((pattern) => globMatches(pattern, "pinvou3-app/src-tauri/src/features/sessions/remote_proxy/mod.rs")));
});


test("generated native paths are current and checked before actual CI routing", async () => {
  const pr = await readFile(new URL("../../.github/workflows/pr-check.yml", import.meta.url), "utf8");
  const release = await readFile(new URL("../../.github/workflows/release-packages.yml", import.meta.url), "utf8");
  assert.deepEqual(renderReleasePathWorkflows(pr, release), { pr: pr.replaceAll("\r\n", "\n"), release: release.replaceAll("\r\n", "\n") });
  const check = "node scripts/generate-remote-control-release-paths.mjs --check";
  const changesSteps = parse(pr).jobs.changes.steps;
  assert.ok(changesSteps.findIndex((step) => step.run === check) < changesSteps.findIndex((step) => step.id === "filter"));
  assert.ok(changesSteps.some((step) => step.run === check && !step.if && !step["continue-on-error"]));
  const releaseSteps = parse(release).jobs["check-version-bump"].steps;
  assert.ok(releaseSteps.some((step) => step.run === check && !step.if && !step["continue-on-error"]));
  const diff = releaseSteps.find((step) => step.id === "diff").run;
  assert.match(diff, /node scripts\/remote-control-release-path-policy\.mjs \|\|/u);
  const fallback = diff.split("\n").find((line) => line.includes("'^(VERSION|scripts/"));
  assert.doesNotMatch(fallback, /remote-control|remote_control|remote-control-relay/u);
});

test("generator CLI rejects single-consumer drift and declaration additions removals and renames", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-path-generator-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(join(root, "scripts"));
  await mkdir(join(root, ".github", "workflows"), { recursive: true });
  const files = ["scripts/generate-remote-control-release-paths.mjs", "scripts/remote-control-release-paths.mjs",
    ".github/workflows/pr-check.yml", ".github/workflows/release-packages.yml"];
  const originals = new Map();
  for (const file of files) {
    const source = await readFile(new URL(`../../${file}`, import.meta.url), "utf8");
    originals.set(file, source);
    await writeFile(join(root, file), source);
  }
  const execute = promisify(execFile);
  const cli = join(root, files[0]);
  await execute(process.execPath, [cli, "--check"], { timeout: 10_000 });
  // Every generated consumer is changed independently, including both event lists.
  for (const [file, occurrence] of [[files[2], 0], [files[3], 0], [files[3], 1]]) {
    const source = originals.get(file);
    let count = 0;
    const changed = source.replace(/# BEGIN GENERATED release paths[\s\S]*?# END GENERATED release paths/gu, (block) =>
      count++ === occurrence ? block.replace("- '.github/workflows/pr-check.yml'", "- 'removed-critical-path.yml'") : block);
    assert.notEqual(changed, source);
    await writeFile(join(root, file), changed);
    await assert.rejects(execute(process.execPath, [cli, "--check"], { timeout: 10_000 }), /release_path_workflow_drift/u);
    await writeFile(join(root, file), source);
  }
  const declaration = originals.get(files[1]);
  const target = '  "pinvou3-app/src-tauri/src/lib.rs",';
  assert.ok(declaration.includes(target));
  for (const changed of [declaration.replace(target, `${target}\n  "pinvou3-app/src-tauri/src/new-release-input.rs",`),
    declaration.replace(target, ""), declaration.replace(target, '  "pinvou3-app/src-tauri/src/renamed-release-input.rs",')]) {
    await writeFile(join(root, files[1]), changed);
    await assert.rejects(execute(process.execPath, [cli, "--check"], { timeout: 10_000 }), /release_path_workflow_drift/u);
  }
});
