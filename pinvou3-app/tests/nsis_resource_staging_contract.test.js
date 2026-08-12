const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const appRoot = path.resolve(__dirname, "..");
const read = (...segments) => fs.readFileSync(path.join(appRoot, ...segments), "utf8");

const stageScript = read(
  "src-tauri",
  "packaging",
  "windows",
  "nsis",
  "scripts",
  "stage-resources.ps1",
);
const installerTemplate = read(
  "src-tauri",
  "packaging",
  "windows",
  "nsis",
  "installer-template.nsi",
);
const baseInstallerHooks = read(
  "src-tauri",
  "packaging",
  "windows",
  "nsis",
  "installer-hooks.nsh",
);
const rustToolchainGuard = read(
  "scripts",
  "ci",
  "ensure-rust-toolchain.ps1",
);
const rustupRepairSmoke = read("tests", "windows_rustup_repair_smoke.ps1");
const buildScript = read("scripts", "tauri", "build.js");
const signScript = read(
  "src-tauri",
  "packaging",
  "windows",
  "signing",
  "wosign",
  "sign.ps1",
);
const packageJson = JSON.parse(read("package.json"));

assert.doesNotMatch(
  stageScript,
  /\$resourceOverrides\["target\/release\/nsis-stage\/resources\/"\]/u,
  "pre-signed staged resources must not be registered as Tauri bundle.resources",
);
assert.match(
  stageScript,
  /installerHooks = "target\/release\/nsis-stage\/installer-hooks\.nsh"/u,
);
assert.match(stageScript, /!macro PINVOU_INSTALL_STAGED_RESOURCES/u);
assert.match(stageScript, /PINVOU_STAGED_RESOURCES_SIZE_KIB/u);
assert.match(stageScript, /File \/a \/r/u);
assert.match(stageScript, /!macro PINVOU_UNINSTALL_STAGED_RESOURCES/u);
assert.match(stageScript, /function Get-StagedRelativeDirectories/u);
assert.match(stageScript, /Delete \/REBOOTOK/u);
assert.match(stageScript, /RMDir \/REBOOTOK/u);
assert.doesNotMatch(stageScript, /RMDir \/r/u);
assert.match(stageScript, /NSIS staging installer hooks changed after collection/u);
assert.match(stageScript, /schemaVersion = 2/u);
assert.match(
  stageScript,
  /NSIS staging config must only remove the original Tauri resource mappings/u,
);

assert.match(installerTemplate, /!insertmacro PINVOU_INSTALL_STAGED_RESOURCES/u);
assert.match(installerTemplate, /!insertmacro PINVOU_UNINSTALL_STAGED_RESOURCES/u);
assert.match(
  installerTemplate,
  /!define \/math ESTIMATEDSIZE \{\{estimated_size\}\} \+ \$\{PINVOU_STAGED_RESOURCES_SIZE_KIB\}/u,
);
assert.doesNotMatch(baseInstallerHooks, /PINVOU_(?:UN)?INSTALL_STAGED_RESOURCES/u);

const installSectionOffset = installerTemplate.indexOf("Section Install");
const installProcessCheckOffset = installerTemplate.indexOf(
  "CheckIfAppIsRunning",
  installSectionOffset,
);
const stagedInstallOffset = installerTemplate.indexOf(
  "!insertmacro PINVOU_INSTALL_STAGED_RESOURCES",
  installSectionOffset,
);
assert.ok(
  installProcessCheckOffset >= 0 && stagedInstallOffset > installProcessCheckOffset,
  "staged resources must be copied after the running-app check",
);

const uninstallSectionOffset = installerTemplate.indexOf("Section Uninstall");
const uninstallProcessCheckOffset = installerTemplate.indexOf(
  "CheckIfAppIsRunning",
  uninstallSectionOffset,
);
const stagedUninstallOffset = installerTemplate.indexOf(
  "!insertmacro PINVOU_UNINSTALL_STAGED_RESOURCES",
  uninstallSectionOffset,
);
assert.ok(
  uninstallProcessCheckOffset >= 0 && stagedUninstallOffset > uninstallProcessCheckOffset,
  "staged resources must be removed after the running-app check",
);

assert.match(packageJson.scripts["build:nsis"], /npm run build:nsis:stage/u);
assert.match(packageJson.scripts["build:nsis"], /npm run bundle:nsis/u);
assert.equal(
  packageJson.scripts["build:nsis:local"],
  "npm run build:nsis:stage:local && npm run bundle:nsis:local",
);
assert.match(packageJson.scripts["build:nsis:stage:local"], /--no-bundle/u);
assert.doesNotMatch(packageJson.scripts["build:nsis:stage:local"], /--features/u);
assert.doesNotMatch(packageJson.scripts["build:nsis:stage:local"], /check:wosign/u);
assert.match(packageJson.scripts["bundle:nsis:local"], /--bundles nsis/u);
assert.match(packageJson.scripts["bundle:nsis:local"], /tauri\.nsis-stage\.conf\.json/u);
assert.doesNotMatch(packageJson.scripts["bundle:nsis:local"], /--features/u);
assert.doesNotMatch(packageJson.scripts["bundle:nsis:local"], /check:wosign/u);
assert.doesNotMatch(packageJson.scripts["bundle:nsis:local"], /signing\.wosign\.conf\.json/u);
assert.match(packageJson.scripts["prebuild:nsis"], /clean-staging\.ps1/u);
assert.doesNotMatch(
  packageJson.scripts["prebuild:nsis"],
  /ensure-rust-toolchain\.ps1/u,
  "build.js must establish the isolated RUSTUP_HOME before invoking the guard",
);
assert.match(buildScript, /await ensureWindowsRustToolchain\(\)/u);
assert.match(buildScript, /args: \["-CheckOnly"\]/u);
assert.match(buildScript, /accountExitCode === 0/u);
assert.match(buildScript, /accountExitCode !== 2/u);
assert.match(buildScript, /PINVOU3_MANAGED_RUSTUP = "1"/u);
assert.match(buildScript, /\.cache", "rustup"/u);
assert.match(rustToolchainGuard, /workspace-scoped RUSTUP_HOME/u);
assert.match(rustToolchainGuard, /if \(\$CheckOnly\)/u);
assert.match(rustToolchainGuard, /exit 2/u);
assert.doesNotMatch(rustToolchainGuard, /TEMPORARY|JENKINS_/u);
assert.doesNotMatch(rustToolchainGuard, /Get-Process|Stop-Process/u);
assert.doesNotMatch(rustToolchainGuard, /SkipProcessCleanup/u);
assert.match(rustToolchainGuard, /\.pinvou3-managed-rustup/u);
assert.match(rustToolchainGuard, /\.pinvou3-toolchain\.lock/u);
assert.match(rustToolchainGuard, /shared RUSTUP_HOME/u);
assert.match(rustToolchainGuard, /rust-toolchain\.toml/u);
assert.match(rustToolchainGuard, /"toolchain", "install"/u);
assert.match(rustToolchainGuard, /"--profile", "minimal"/u);
assert.match(rustToolchainGuard, /"toolchain", "uninstall"/u);
assert.match(rustToolchainGuard, /RUSTUP_DOWNLOAD_TIMEOUT/u);
assert.match(rustToolchainGuard, /function Invoke-Rustup/u);
assert.match(rustToolchainGuard, /ErrorActionPreference = "Continue"/u);
assert.match(rustToolchainGuard, /RUSTUP_DIST_SERVER/u);
assert.match(rustToolchainGuard, /https:\/\/static\.rust-lang\.org/u);
assert.match(rustToolchainGuard, /mirrors\.tuna\.tsinghua\.edu\.cn\/rustup/u);
assert.match(rustToolchainGuard, /https:\/\/rsproxy\.cn/u);
assert.match(rustToolchainGuard, /Name = "configured source"/u);
assert.match(rustToolchainGuard, /InstallAttemptTimeoutSeconds = 600/u);
assert.match(rustToolchainGuard, /RepairAttemptsPerSource = 2/u);
assert.match(
  rustToolchainGuard,
  /Repair attempt \$attempt\/\$RepairAttemptsPerSource/u,
);
assert.match(rustToolchainGuard, /WaitForExit\(\$TimeoutSeconds \* 1000\)/u);
assert.match(rustToolchainGuard, /return 124/u);
assert.match(rustToolchainGuard, /postInstallInvalid/u);
assert.match(rustupRepairSmoke, /pinvou-rustup-repair-/u);
assert.match(rustupRepairSmoke, /-CheckOnly/u);
assert.match(rustupRepairSmoke, /127\.0\.0\.1:1/u);
assert.match(rustupRepairSmoke, /-RepairAttemptsPerSource 1/u);
assert.match(rustupRepairSmoke, /rust-toolchain\.toml/u);
assert.match(rustupRepairSmoke, /\.pinvou3-managed-rustup/u);
assert.match(rustupRepairSmoke, /Isolated inconsistent-toolchain repair: PASS/u);
assert.match(
  packageJson.scripts["test:windows-rustup-repair"],
  /windows_rustup_repair_smoke\.ps1/u,
);
assert.match(packageJson.scripts["build:nsis:stage"], /--verbose/u);
assert.match(packageJson.scripts["build:nsis:stage"], /--ci/u);
assert.doesNotMatch(
  packageJson.scripts["prebuild:nsis"],
  /stage-runtime\.ps1|runtime:windows:stage/u,
  "prebuild:nsis must leave runtime validation and staging to build.js",
);
assert.ok(
  packageJson.scripts["bundle:nsis"].indexOf("stage-resources.ps1 -ValidateOnly") <
    packageJson.scripts["bundle:nsis"].indexOf("node scripts/tauri/build.js bundle"),
  "bundle:nsis must validate staging before build.js skips runtime staging",
);
assert.match(packageJson.scripts["bundle:nsis"], /tauri\.nsis-stage\.conf\.json/u);

assert.doesNotMatch(
  signScript,
  /nsis-stage|runtime[\\/]node|resources[\\/]/u,
  "the signing command must not use fragile resource-path special cases",
);

console.log("NSIS pre-signed resource staging contract: ok");
