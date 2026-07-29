const assert = require("assert");
const fs = require("fs");
const path = require("path");

const appRoot = path.resolve(__dirname, "..");
const packageJson = JSON.parse(
  fs.readFileSync(path.join(appRoot, "package.json"), "utf8"),
);
const runtimeScript = fs.readFileSync(
  path.join(
    appRoot,
    "src-tauri",
    "packaging",
    "windows",
    "runtime",
    "scripts",
    "resolve-runtime.ps1",
  ),
  "utf8",
);
const runtimeInitScript = fs.readFileSync(
  path.join(
    appRoot,
    "src-tauri",
    "packaging",
    "windows",
    "runtime",
    "scripts",
    "init-submodule.ps1",
  ),
  "utf8",
);
const installerHooks = fs.readFileSync(
  path.join(
    appRoot,
    "src-tauri",
    "packaging",
    "windows",
    "nsis",
    "installer-hooks.nsh",
  ),
  "utf8",
);
const releaseWorkflow = fs.readFileSync(
  path.resolve(appRoot, "..", ".github", "workflows", "release-packages.yml"),
  "utf8",
);

const stagedBootstrapper =
  "${__FILEDIR__}\\..\\..\\..\\windows-runtime\\nsis\\vc_redist\\VC_redist.x64.exe";
const removedBootstrapper =
  "${__FILEDIR__}\\..\\..\\..\\..\\resources\\windows\\vc_redist\\VC_redist.x64.exe";
const removedArchitectureBootstrapper =
  "${__FILEDIR__}\\..\\..\\..\\..\\packaging\\windows\\vc_redist\\VC_redist.x64.exe";

assert.ok(
  installerHooks.includes(stagedBootstrapper),
  "NSIS hook must load VC_redist from the verified target/windows-runtime staging area",
);
assert.ok(
  !installerHooks.includes(removedBootstrapper),
  "NSIS hook must not reference the deleted source-tree vc_redist directory",
);
assert.ok(
  !installerHooks.includes(removedArchitectureBootstrapper),
  "NSIS hook must not reference an unstaged packaging/windows vc_redist directory",
);

assert.ok(
  runtimeScript.includes('function Stage-NsisBootstrapper'),
  "runtime staging must expose a dedicated NSIS bootstrapper step",
);
assert.ok(
  runtimeScript.includes('$manifestPath = "payload/vc_redist/VC_redist.x64.exe"'),
  "VC_redist must be selected from the locked private-runtime manifest",
);
assert.ok(
  runtimeScript.includes('$destinationRoot = Join-Path $StagingParent "nsis"'),
  "VC_redist must be copied to the stable target/windows-runtime/nsis directory",
);
assert.ok(
  runtimeScript.includes('(Get-Sha256 -Path $temporaryPath) -ne [string]$entry.sha256'),
  "the NSIS bootstrapper copy must be verified against the locked SHA-256",
);
assert.ok(
  runtimeScript.includes('$sourceVerificationMarkerPath = Join-Path $stagingParent ".verified-lock"'),
  "runtime validation must persist a reusable source verification lock",
);
assert.ok(
  runtimeScript.includes("function Get-CachedVerifiedManifest"),
  "later build phases must reuse the verified runtime manifest",
);
assert.ok(
  runtimeScript.includes("function Test-VerifiedStageReusable"),
  "runtime staging must expose a fast verified-stage reuse check",
);
assert.ok(
  runtimeScript.includes("function Test-StageManifest"),
  "runtime staging reuse must validate a complete staged-file manifest",
);
assert.ok(
  runtimeScript.includes("function Test-StagedFilesAgainstTrustedManifest") &&
    runtimeScript.includes("-TrustedEntries $Manifest.stagedFiles"),
  "runtime staging reuse must validate files against the locked private-runtime manifest",
);
assert.match(
  runtimeScript,
  /\[int\]\$manifest\.schemaVersion -ne 2/u,
  "runtime resolution must require the trusted stagedFiles manifest schema",
);
assert.ok(
  runtimeScript.includes("Stage-Submodule -Manifest $verifiedManifest -Rebuild"),
  "failed staging reuse must force a complete atomic rebuild",
);
assert.match(
  runtimeScript,
  /Cached staging failed integrity checks; revalidating source payloads before rebuilding/u,
  "damaged staging must trigger full source payload validation before rebuilding",
);
assert.ok(
  runtimeScript.includes("Reused verified Windows runtime staging"),
  "runtime staging must report when the generated overlay is reused",
);
assert.ok(
  runtimeScript.indexOf("Get-CachedVerifiedManifest") <
    runtimeScript.lastIndexOf("Get-VerifiedManifest"),
  "the cache lookup must happen before full payload SHA-256 validation",
);
for (const lockField of [
  "schemaVersion = $verificationSchemaVersion",
  "sourceCommit =",
  "manifestSha256 =",
  "lockFileSha256 =",
]) {
  assert.ok(
    runtimeScript.includes(lockField),
    `verified-lock must bind cached staging to ${lockField}`,
  );
}
assert.doesNotMatch(
  runtimeInitScript,
  /submodule\s+sync|submodule[^\r\n]*--recursive/u,
  "Windows runtime initialization must not recursively sync unrelated submodules",
);
assert.match(
  runtimeInitScript,
  /currentCommit -ne \$expectedCommit/u,
  "runtime submodule update must be gated by the locked gitlink commit",
);
assert.match(
  runtimeInitScript,
  /Get-LfsPointerPaths/u,
  "runtime initialization must inspect materialized LFS files before pulling",
);
assert.match(
  runtimeInitScript,
  /git lfs pull was skipped/u,
  "runtime initialization must expose the LFS reuse path",
);
assert.match(
  runtimeInitScript,
  /pinvou3-windows-runtime-\$cacheSchema-\$expectedCommit-\$\(\$lockSha256\.Substring/u,
  "runtime initialization must emit a schema, commit and lock-scoped Jenkins cache key",
);
assert.ok(
  runtimeInitScript.includes('$cacheSchema = "v4"') &&
    runtimeScript.includes('$verificationSchemaVersion = 4'),
  "the trusted-staging scheme must invalidate earlier self-attested caches",
);
assert.match(
  runtimeInitScript,
  /SetEnvironmentVariable\("GIT_LFS_SKIP_SMUDGE", "1", "Process"\)[\s\S]*?submodule", "update"/u,
  "submodule checkout must disable automatic LFS smudge before selective pulls",
);
assert.match(
  runtimeInitScript,
  /finally \{[\s\S]*?SetEnvironmentVariable\("GIT_LFS_SKIP_SMUDGE", \$previousSkipSmudge, "Process"\)/u,
  "submodule initialization must restore the caller's LFS smudge setting",
);
assert.ok(
  runtimeInitScript.includes('$resolverLockPath = Join-Path $resolverCacheRoot ".resolver.lock"') &&
    runtimeScript.includes('$resolverLockPath = Join-Path $stagingParent ".resolver.lock"'),
  "submodule initialization and runtime staging must share the same workspace lock",
);
assert.match(
  packageJson.scripts["test:windows-runtime-cache"],
  /windows_runtime_cache_behavior\.ps1/u,
  "the executable cache corruption regression must be part of the npm test surface",
);
assert.match(
  runtimeInitScript,
  /if \(\$CacheKeyOnly\)[\s\S]*?Write-Output \$cacheKey[\s\S]*?exit 0/u,
  "Jenkins must be able to resolve the runtime cache key before remote access",
);
assert.ok(
  !runtimeScript.includes("sensevoice-small-q8.gguf"),
  "the Windows installer overlay must not bundle the on-demand SenseVoice recognition model",
);

const windowsJobStart = releaseWorkflow.indexOf("build-windows-x64:");
const macosJobStart = releaseWorkflow.indexOf("build-macos-universal:", windowsJobStart);
assert.ok(
  windowsJobStart >= 0 && macosJobStart > windowsJobStart,
  "release workflow must contain distinct Windows and macOS build jobs",
);
const windowsJob = releaseWorkflow.slice(windowsJobStart, macosJobStart);
assert.ok(
  windowsJob.includes("secrets.PINVOU3_WINDOWS_RUNTIME_TOKEN || github.token"),
  "Windows release checkout must use the dedicated runtime token when configured",
);
assert.ok(
  windowsJob.includes("npm run test:windows-runtime"),
  "Windows release job must validate packaging contracts when private runtime access is unavailable",
);
const runtimeInit = windowsJob.indexOf("npm run runtime:windows:init");
const nsisBuild = windowsJob.indexOf("npm run build:nsis");
assert.ok(
  runtimeInit >= 0 && nsisBuild > runtimeInit,
  "Windows release job must initialize the locked private runtime before building NSIS",
);

const simulatedFileDir =
  "D:\\workspace\\pinvou3-app\\src-tauri\\target\\release\\nsis\\x64";
const resolvedHookPath = path.win32.resolve(
  simulatedFileDir,
  "..\\..\\..\\windows-runtime\\nsis\\vc_redist\\VC_redist.x64.exe",
);
const expectedStagingPath =
  "D:\\workspace\\pinvou3-app\\src-tauri\\target\\windows-runtime\\nsis\\vc_redist\\VC_redist.x64.exe";
assert.strictEqual(
  resolvedHookPath.toLowerCase(),
  expectedStagingPath.toLowerCase(),
  "NSIS relative path must resolve to the stable staging destination",
);

console.log("✅ Windows runtime NSIS bootstrapper packaging contract passed");
