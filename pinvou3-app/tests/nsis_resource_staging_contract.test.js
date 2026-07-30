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
assert.ok(
  packageJson.scripts["bundle:nsis"].indexOf("stage-resources.ps1 -ValidateOnly") <
    packageJson.scripts["bundle:nsis"].indexOf("node scripts/tauri/build.js bundle"),
  "bundle:nsis must validate staging before Tauri bundling",
);
assert.match(packageJson.scripts["bundle:nsis"], /tauri\.nsis-stage\.conf\.json/u);

assert.doesNotMatch(
  signScript,
  /nsis-stage|runtime[\\/]node|resources[\\/]/u,
  "the signing command must not use fragile resource-path special cases",
);

console.log("NSIS pre-signed resource staging contract: ok");
