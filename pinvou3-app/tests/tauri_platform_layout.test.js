const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const appRoot = path.resolve(__dirname, "..");
const tauriRoot = path.join(appRoot, "src-tauri");
const repoRoot = path.resolve(appRoot, "..");
const { linuxStartupWindowConfig } = require("../scripts/tauri/startup-window-config.js");

function readJson(relativePath) {
  return JSON.parse(fs.readFileSync(path.join(tauriRoot, relativePath), "utf8"));
}

function resourceSources(config) {
  return Object.keys(config.bundle?.resources || {});
}

function assertResourceSourcesExist(config, label) {
  for (const source of resourceSources(config)) {
    assert.ok(
      fs.existsSync(path.join(tauriRoot, source)),
      `${label} resource source must exist: ${source}`,
    );
  }
}

const common = readJson("tauri.conf.json");
const linux = readJson("config/platforms/linux/tauri.conf.json");
const linuxX64 = readJson("config/platforms/linux/x86_64/tauri.conf.json");
const linuxArm64 = readJson("config/platforms/linux/aarch64/tauri.conf.json");
const linuxLoong64 = readJson("config/platforms/linux/loongarch64/tauri.conf.json");
const macos = readJson("config/platforms/macos/tauri.conf.json");
const windows = readJson("config/platforms/windows/tauri.conf.json");
const windowsSigning = readJson("config/platforms/windows/signing.wosign.conf.json");

assert.equal(resourceSources(common).length, 0, "common bundle assets are embedded by the Rust runtime");
assert.match(common.build.beforeBuildCommand, /scripts\/tauri\/require-wrapper\.js build/);
assert.match(common.build.beforeBundleCommand, /scripts\/tauri\/require-wrapper\.js bundle/);
assert.ok(
  resourceSources(common).every((source) => source.startsWith("resources/common/")),
  "common Tauri config may only package resources/common",
);
assert.ok(
  resourceSources(linux).every((source) => source.startsWith("resources/platforms/linux/")),
  "Linux overlay may only package resources/platforms/linux",
);
assert.deepEqual(resourceSources(linuxX64), [
  "target/linux-asr-runtime/x86_64/sense-voice-main",
]);
assert.deepEqual(resourceSources(linuxArm64), [
  "target/linux-asr-runtime/aarch64/sense-voice-main",
]);
assert.deepEqual(resourceSources(linuxLoong64), [
  "target/linux-asr-runtime/loongarch64/sense-voice-main",
]);
assert.ok(
  resourceSources(macos).every((source) => source.startsWith("resources/platforms/macos/")),
  "macOS overlay may only package resources/platforms/macos",
);
assert.ok(
  resourceSources(windows).every((source) => source.startsWith("resources/platforms/windows/")),
  "Windows overlay may only package resources/platforms/windows",
);

assertResourceSourcesExist(common, "common");
assertResourceSourcesExist(linux, "Linux");
assertResourceSourcesExist(macos, "macOS");
assertResourceSourcesExist(windows, "Windows");

assert.equal(linux.app, undefined, "Linux packaging config must not duplicate base window fields");
const linuxStartupMain = { ...linuxStartupWindowConfig().app.windows[0] };
delete linuxStartupMain.visible;
linuxStartupMain.url = linuxStartupMain.url.replace("&startupWindow=hidden", "");
assert.deepEqual(
  linuxStartupMain,
  common.app.windows[0],
  "generated Linux startup config must inherit the complete base main window",
);

for (const legacyPath of ["resources/bundle", "resources/skill-marketplace", "resources/asr"]) {
  assert.equal(fs.existsSync(path.join(tauriRoot, legacyPath)), false, `legacy resource root must be removed: ${legacyPath}`);
}
assert.equal(
  fs.existsSync(path.join(tauriRoot, "tauri.windows-runtime.generated.conf.json")),
  false,
  "generated Windows runtime config must stay under the ignored target directory",
);

const packagingPaths = [
  linux.bundle.linux.deb.desktopTemplate,
  linux.bundle.linux.deb.postInstallScript,
  linux.bundle.linux.deb.preRemoveScript,
  windows.bundle.windows.wix.template,
  ...windows.bundle.windows.wix.fragmentPaths,
  windows.bundle.windows.nsis.template,
  windows.bundle.windows.nsis.installerHooks,
  windowsSigning.bundle.windows.signCommand.args[
    windowsSigning.bundle.windows.signCommand.args.indexOf("-File") + 1
  ],
];
for (const packagingPath of packagingPaths) {
  assert.ok(
    fs.existsSync(path.join(tauriRoot, packagingPath)),
    `Tauri packaging reference must exist: ${packagingPath}`,
  );
}

for (const legacyPath of [
  "packaging/linux/desktop/pinvou3.desktop",
  "packaging/linux/scripts/postinst.sh",
  "packaging/linux/scripts/prerm.sh",
  "packaging/windows/scripts/windows-runtime-submodule.ps1",
  "packaging/windows/scripts/prepare-windows-runtimes.ps1",
  "packaging/windows/scripts/stage-windows-nsis-resources.ps1",
  "packaging/windows/scripts/clean-nsis-staging.ps1",
  "packaging/windows/wosign/sign.ps1",
  "packaging/windows/main.wxs",
  "packaging/windows/python-node-path.wxs",
]) {
  assert.equal(fs.existsSync(path.join(tauriRoot, legacyPath)), false, `legacy packaging path must be removed: ${legacyPath}`);
}

const packageJson = JSON.parse(fs.readFileSync(path.join(appRoot, "package.json"), "utf8"));
assert.match(packageJson.scripts.tauri, /scripts\/tauri\/build\.js/);
for (const [name, command] of Object.entries(packageJson.scripts)) {
  assert.doesNotMatch(
    command,
    /(?:^|&&\s*)tauri\s+(?:build|bundle)\b/,
    `${name} must route Tauri build/bundle through scripts/tauri/build.js`,
  );
}
const runDev = fs.readFileSync(path.join(appRoot, "run-dev.sh"), "utf8");
assert.match(
  runDev,
  /exec npm run dev -- "\$@"/,
  "the direct dev entry must route through the wrapper that generates the startup overlay",
);

const gitmodules = fs.readFileSync(path.join(repoRoot, ".gitmodules"), "utf8");
assert.match(
  gitmodules,
  /\[submodule "private-runtimes\/windows"\][\s\S]*?\n\s*update\s*=\s*none(?:\r?\n|$)/,
  "private Windows runtime must remain opt-in during recursive submodule updates",
);
assert.doesNotMatch(
  JSON.stringify({ common, windows }),
  /private-runtimes\/windows|target\/windows-runtime/,
  "public Tauri configs must not hard-code private runtime sources",
);
const workflow = fs.readFileSync(path.join(repoRoot, ".github/workflows/pr-check.yml"), "utf8");
const connectorWorkflow = fs.readFileSync(
  path.join(repoRoot, ".github/workflows/connector-verify.yml"),
  "utf8",
);
const architectureGate = workflow.slice(
  workflow.indexOf("- name: 架构边界门禁"),
  workflow.indexOf("- name: 初始化公共底座 submodule"),
);
// merge_group 事件上 github.base_ref 不可靠,必须用 merge_group.base_ref 兜底。
// 契约锁住"两种事件都解析正确 base + 剥 refs/heads/ 前缀 + 传给守卫",防止改回 PR-only。
assert.match(
  architectureGate,
  /github\.event_name == 'merge_group' && github\.event\.merge_group\.base_ref \|\| github\.base_ref/,
);
assert.match(architectureGate, /base_ref="\$\{base_ref#refs\/heads\/\}"/);
assert.match(architectureGate, /architecture-guard\.py --base-ref "origin\/\$base_ref"/);

// fork 合规联动步骤用同款 base_ref 解析:锁住不被改回 PR-only,避免队列里取错基线。
const forkLinkGate = workflow.slice(
  workflow.indexOf("- name: fork 合规联动"),
  workflow.indexOf("- name: Setup Python"),
);
assert.match(
  forkLinkGate,
  /github\.event_name == 'merge_group' && github\.event\.merge_group\.base_ref \|\| github\.base_ref/,
);
assert.match(forkLinkGate, /base_ref="\$\{base_ref#refs\/heads\/\}"/);
assert.match(forkLinkGate, /ci-fork-link-check\.sh "origin\/\$base_ref"/);
const submoduleUpdates = workflow.match(/git submodule update[^\r\n]*/g) || [];
assert.ok(submoduleUpdates.length > 0, "CI must initialize the public CodeWhale submodule");
assert.ok(
  submoduleUpdates.every((command) => command.endsWith("-- CodeWhale")),
  "Linux CI may only initialize CodeWhale, never the private Windows runtime",
);
assert.match(workflow, /Merge Queue diff-selected browser smoke/);
assert.match(workflow, /github\.event\.merge_group\.base_sha/);
assert.match(workflow, /github\.event\.merge_group\.head_sha/);
assert.doesNotMatch(workflow, /npm run test:browser-smoke/);
assert.doesNotMatch(workflow, /frontend-test:[\s\S]{0,300}\n\s*if:\s*\$\{\{\s*false\s*\}\}/);
for (const stalePath of [
  "pinvou3-app/src-tauri/src/app/bridge",
  "pinvou3-app/src-tauri/src/features/assistant/harness.rs",
  "pinvou3-app/src-tauri/src/platform/prefs.rs",
  "resources/common/bundle/connectors/linux-arm64",
]) {
  assert.equal(workflow.includes(stalePath), false, `PR workflow still references migrated path: ${stalePath}`);
  assert.equal(connectorWorkflow.includes(stalePath), false, `connector workflow still references migrated path: ${stalePath}`);
}
// prefs was split into the platform/prefs/ directory by #142; the pet filter anchors on the directory.
assert.match(workflow, /src\/platform\/prefs\//);
assert.match(connectorWorkflow, /resources\/platforms\/\*\*\/bundle\/connectors\/\*\*/);
function assertConnectorMatrix(source) {
  // This workflow uses one known expression shape. Decode its JSON arguments;
  // never evaluate workflow expressions or let comments satisfy the contract.
  const lines = source.split(/\r?\n/).filter((line) => !/^\s*(?:#|$)/.test(line));
  assert.equal(lines.filter((line) => /^jobs:/.test(line)).length, 1, "one live jobs map");
  const jobsStart = lines.findIndex((line) => line === "jobs:");
  assert.ok(jobsStart >= 0, "canonical live jobs map");
  const jobsEnd = lines.findIndex((line, index) => index > jobsStart && /^\S/.test(line));
  const jobs = lines.slice(jobsStart + 1, jobsEnd < 0 ? undefined : jobsEnd);
  const headers = jobs.flatMap((line, index) => /^  verify-connectors:/.test(line) ? [index] : []);
  assert.equal(headers.length, 1, "one live verify-connectors job");
  const start = headers[0];
  assert.equal(jobs[start], "  verify-connectors:", "canonical connector job header");
  const end = jobs.findIndex((line, index) => index > start && /^ {0,2}\S/.test(line));
  const job = jobs.slice(start + 1, end < 0 ? undefined : end);
  assert.equal(job.filter((line) => /^    strategy:/.test(line)).length, 1, "one live connector strategy");
  const strategyStart = job.indexOf("    strategy:");
  assert.ok(strategyStart >= 0, "canonical connector strategy");
  const strategyEnd = job.findIndex((line, index) => index > strategyStart && /^ {0,4}\S/.test(line));
  const strategy = job.slice(strategyStart + 1, strategyEnd < 0 ? undefined : strategyEnd);
  const matrixLines = job.filter((line) => /^\s+matrix:/.test(line));
  assert.equal(matrixLines.length, 1, "one live connector matrix");
  assert.ok(strategy.includes(matrixLines[0]), "matrix belongs to the connector strategy");
  const expression = matrixLines[0].match(
    /^      matrix: \$\{\{ fromJSON\(vars\.PINVOU_WINDOWS_ENABLED != 'false' && \(vars\.PINVOU_MACOS_ENABLED != 'false' && '([^']+)' \|\| '([^']+)'\) \|\| \(vars\.PINVOU_MACOS_ENABLED != 'false' && '([^']+)' \|\| '([^']+)'\)\) \}\}$/,
  );
  assert.ok(expression, "known connector matrix condition and four JSON branches");
  const [enabled, withWindows, withMacos, disabled] = expression.slice(1).map(JSON.parse);
  const linuxRows = [
    { platform: "linux-arm64", "runs-on": "ubuntu-22.04-arm", resources: "linux/aarch64", suffix: "", can_run: true },
    { platform: "linux-x64", "runs-on": "ubuntu-22.04", resources: "linux/x86_64", suffix: "", can_run: true },
  ];
  assert.deepEqual(enabled, { include: [
    ...linuxRows,
    { platform: "darwin-arm64", "runs-on": "macos-15", resources: "macos/aarch64", suffix: "", can_run: true },
    { platform: "darwin-x64", "runs-on": "macos-15-intel", resources: "macos/x86_64", suffix: "", can_run: true },
    { platform: "windows-x64", "runs-on": "windows-latest", resources: "windows/x86_64", suffix: ".exe", can_run: true },
  ] }, "enabled/default connector matrix preserves all five platform mappings");
  assert.deepEqual(disabled, { include: linuxRows }, "disabled native platforms retain exactly both Linux mappings");
  assert.deepEqual(withWindows, {
    include: enabled.include.filter((row) => !row.resources.startsWith("macos/")),
  }, "Windows-only switch preserves Windows and both Linux mappings");
  assert.deepEqual(withMacos, {
    include: enabled.include.filter((row) => !row.resources.startsWith("windows/")),
  }, "macOS-only switch preserves both macOS and both Linux mappings");
  const runners = job.filter((line) => /^    runs-on:/.test(line));
  assert.deepEqual(runners, [
    "    runs-on: ${{ matrix.runs-on == 'ubuntu-22.04' && fromJSON(vars.PINVOU_LINUX_LIGHT_RUNNER || vars.PINVOU_LINUX_RUNNER || '[\"ubuntu-22.04\"]') || matrix.runs-on == 'windows-latest' && fromJSON((github.event_name == 'push' || github.event_name == 'workflow_dispatch' || (github.event_name == 'pull_request' && github.repository != '' && github.repository_id != '' && github.event.pull_request.head.repo.full_name == github.repository && github.event.pull_request.base.repo.full_name == github.repository && github.event.pull_request.head.repo.id == github.repository_id && github.event.pull_request.base.repo.id == github.repository_id)) && (vars.PINVOU_WINDOWS_LIGHT_RUNNER || vars.PINVOU_WINDOWS_RUNNER || '[\"windows-latest\"]') || '[\"windows-latest\"]') || matrix.runs-on }}",
  ], "connector matrix runner mapping retains LIGHT/legacy/default routing and verified Windows PR source");
}
assertConnectorMatrix(connectorWorkflow);
assert.match(connectorWorkflow, /src\/platform\/paths\.rs/);

console.log("tauri platform layout contract: ok");
