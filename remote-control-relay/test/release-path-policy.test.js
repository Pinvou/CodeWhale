import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
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
  "remote-control-relay/scripts/verify-github-workflow-run.cjs",
  "remote-control-relay/test/controller-release-evidence.test.js",
  "remote-control-relay/test/installer-spdx.test.js",
  "scripts/create-installer-spdx.mjs",
  "scripts/run-windows-remote-control-e2e.ps1",
  "scripts/windows-e2e-orchestrator-helpers.ps1",
  ".github/workflows/windows-remote-control-build-evidence.yml",
  ".github/workflows/windows-remote-control-evidence.yml",
  ".github/workflows/release-packages.yml",
  ".github/workflows/pr-check.yml",
];

test("single release path policy covers every remote-control security boundary", () => {
  for (const path of criticalPaths) assert.equal(isRemoteControlReleasePath(path), true, path);
  assert.equal(changedPathsRequireRemoteControlRelease(`docs/unrelated.md\n${criticalPaths[0]}\n`), true);
  assert.equal(changedPathsRequireRemoteControlRelease("docs/unrelated.md\nREADME.md\n"), false);
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
    "pinvou3-app/src-tauri/src/app/commands/**", "remote-control-relay/docs/schemas/**",
    "pinvou3-app/src-tauri/capabilities/default.json", "pinvou3-app/src-tauri/packaging/windows/**",
    "scripts/remote-control-release-path-policy.mjs", ".github/workflows/windows-remote-control-evidence.yml",
  ]) {
    assert.ok(pr.includes(marker), `PR path filter misses ${marker}`);
  }
  for (const executableTest of [
    "release-path-policy.test.js", "installer-spdx.test.js", "github-workflow-run-identity.test.js",
    "windows-e2e-orchestrator-contract.test.js", "windows-e2e-orchestrator-helpers.test.js",
    "windows-e2e-path-guard.test.js", "remote_control_real_ui_smoke_contract.test.mjs",
    "remote_control_feature_gate_contract.test.mjs", "nsis_resource_staging_contract.test.js",
  ]) assert.ok(pr.includes(executableTest), `release-contract-test does not execute ${executableTest}`);
  for (const filter of [
    "remote-control-relay/scripts/verify-github-workflow-run.cjs",
    "remote-control-relay/test/release-path-policy.test.js",
    "remote-control-relay/test/windows-e2e-*.test.js",
    "pinvou3-app/tests/nsis_resource_staging_contract.test.js",
  ]) assert.ok(pr.includes(filter), `release-contract filter misses ${filter}`);
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
