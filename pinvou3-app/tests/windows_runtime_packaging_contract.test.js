const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const { windowsBundleTargets } = require("../scripts/tauri/build.js");
const { stageWindowsInstaller } = require("../scripts/tauri/windows-installer.js");

const appRoot = path.resolve(__dirname, "..");
const repoRoot = path.resolve(appRoot, "..");
const readRepo = (...parts) =>
  fs.readFileSync(path.join(repoRoot, ...parts), "utf8");
const readApp = (...parts) => fs.readFileSync(path.join(appRoot, ...parts), "utf8");

const packageJson = JSON.parse(readApp("package.json"));
const gitmodules = readRepo(".gitmodules");
const lock = JSON.parse(
  readApp(
    "src-tauri",
    "config",
    "platforms",
    "windows",
    "runtime",
    "x86_64.lock.json",
  ),
);
const windowsConfig = JSON.parse(
  readApp("src-tauri", "config", "platforms", "windows", "tauri.conf.json"),
);
const runtimeInitScript = readApp(
  "src-tauri",
  "packaging",
  "windows",
  "runtime",
  "scripts",
  "init-submodule.ps1",
);
const runtimeScript = readApp(
  "src-tauri",
  "packaging",
  "windows",
  "runtime",
  "scripts",
  "resolve-runtime.ps1",
);
const onnxRuntimeScript = readApp(
  "src-tauri",
  "packaging",
  "windows",
  "runtime",
  "scripts",
  "stage-onnx-runtime.ps1",
);
const onnxRuntimeSmoke = readApp(
  "src-tauri",
  "packaging",
  "windows",
  "runtime",
  "scripts",
  "test-onnx-runtime.ps1",
);
const installerHook = readApp(
  "src-tauri",
  "packaging",
  "windows",
  "nsis",
  "installer-hooks.nsh",
);
const vcRedistTempPreflightPath = path.join(
  appRoot,
  "src-tauri",
  "packaging",
  "windows",
  "nsis",
  "vcredist-temp-preflight.ps1",
);
const vcRedistTempPreflight = fs.readFileSync(vcRedistTempPreflightPath, "utf8");
const runtimeWrapper = readApp("scripts", "tauri", "windows-runtime.js");
const installerAdapter = readApp("scripts", "tauri", "windows-installer.js");
const buildScript = readApp("scripts", "tauri", "build.js");
const bridgeScript = readApp("scripts", "tauri", "codex-bridge.js");
const prCheckWorkflow = readRepo(".github", "workflows", "pr-check.yml");
const releaseWorkflow = readRepo(".github", "workflows", "release-packages.yml");
const runtimeGuide = readRepo("docs", "windows-private-runtime-submodule.md");

assert.match(
  gitmodules,
  /\[submodule "private-runtimes\/windows"\][\s\S]*?url = https:\/\/github\.com\/Pinvou\/pinvou3-windows-runtime\.git[\s\S]*?update = none/,
  "private Windows runtime must be pinned as a non-automatic submodule",
);
assert.equal(lock.schemaVersion, 2);
assert.equal(lock.target, "windows-x86_64");
assert.equal(lock.source.type, "git-submodule");
assert.equal(lock.source.path, "private-runtimes/windows");
assert.equal(lock.source.url, "https://github.com/Pinvou/pinvou3-windows-runtime.git");
assert.match(lock.source.commit, /^[0-9a-f]{40}$/u);
assert.match(lock.manifest.sha256, /^[0-9a-f]{64}$/u);
assert.match(lock.vcRedist.minimumVersion, /^\d+\.\d+\.\d+\.\d+$/u);

const [vcMajor, vcMinor, vcBuild, vcRevision] = lock.vcRedist.minimumVersion
  .split(".")
  .map(Number);
assert.ok(
  [vcMajor, vcMinor, vcBuild, vcRevision].every(Number.isSafeInteger),
  "VC++ minimum version components must be safe integers",
);

const gitlink = spawnSync(
  "git",
  ["ls-files", "--stage", "--", lock.source.path],
  { cwd: repoRoot, encoding: "utf8" },
);
assert.equal(gitlink.error, undefined);
assert.equal(gitlink.status, 0, gitlink.stderr);
assert.match(
  gitlink.stdout,
  new RegExp(`^160000 ${lock.source.commit} 0\\t${lock.source.path.replace("/", "\\/")}`),
  "superproject gitlink must match the runtime lock",
);

for (const contract of [
  "Get-SuperprojectGitlinkCommit",
  "Windows runtime submodule commit mismatch",
  "manifest SHA-256",
  "Test-LfsPointer",
  "Test-ManagedArchiveExpansion",
  "System.IO.Compression.ZipFile",
  "Write-Utf8WithoutBom",
  "Test-StageManifest",
  ".stage-manifest.json",
  "Get-RuntimeDescriptorContent",
  'delivery = "download-on-first-use"',
]) {
  assert.ok(runtimeScript.includes(contract), `runtime staging must retain ${contract}`);
}
assert.match(runtimeScript, /Get-Sha256 -Path \$sourcePath/u);
assert.match(runtimeScript, /vcRedist\.minimumVersion/u);
assert.match(runtimeScript, /System\.Diagnostics\.FileVersionInfo/u);
assert.match(runtimeScript, /\$vcActualVersion -lt \$vcMinimumVersion/u);
assert.match(runtimeScript, /HashSet\[string\]/u);
assert.match(runtimeScript, /Remove-Item -LiteralPath \$bundledAsrModelPath/u);
for (const destination of [
  "runtime/poppler",
  "runtime/pandoc",
  "runtime/tesseract",
  "runtime/python",
  "runtime/node",
  "runtime/onnxruntime",
  "runtime/asr",
  "runtime/7zip",
]) {
  assert.ok(runtimeScript.includes(destination), `runtime overlay must include ${destination}`);
}
assert.doesNotMatch(
  runtimeScript,
  /sensevoice-small-q8\.gguf"\s*=\s*"runtime\/asr/u,
  "download-on-first-use ASR model must not be mapped into the installer",
);
assert.doesNotMatch(
  runtimeScript,
  /Stage-NsisBootstrapper/u,
  "runtime resolution must leave NSIS bootstrapper staging to windows-installer.js",
);
assert.match(
  runtimeScript,
  /source = "\$relativeRoot\/\$vcRedistRelativePath"/u,
  "the runtime descriptor must reference the verified staged VC++ payload",
);
assert.match(runtimeInitScript, /\$ErrorActionPreference = "Continue"/u);
assert.match(runtimeInitScript, /Select-Object -First 1 -ExpandProperty Source/u);
assert.match(runtimeInitScript, /& \$gitExecutable -C/u);
assert.match(runtimeInitScript, /\$exitCode = \$LASTEXITCODE/u);
assert.match(runtimeInitScript, /if \(\$exitCode -ne 0\)/u);
assert.match(runtimeInitScript, /git lfs pull/);
assert.match(runtimeInitScript, /--include=/);
assert.match(runtimeInitScript, /\[switch\]\$OnnxOnly/);
assert.match(runtimeInitScript, /onnxruntime-win-x64-\*-runtime\.zip/);
assert.match(runtimeInitScript, /pinvou3-windows-runtime-\$cacheSchema-\$expectedCommit/u);

assert.match(runtimeWrapper, /runtime-descriptor\.json/);
assert.match(runtimeWrapper, /cleanupLegacyWindowsNodeStaging/);
assert.match(runtimeWrapper, /download-on-first-use/);
assert.match(runtimeWrapper, /onnxRuntimeDylib/);
assert.match(runtimeWrapper, /stageWindowsOnnxRuntime/);
assert.match(onnxRuntimeScript, /-Mode StageOnnx/);
assert.match(runtimeScript, /Get-VerifiedOnnxManifest/);
assert.match(runtimeScript, /Stage-OnnxRuntime/);
assert.match(onnxRuntimeSmoke, /LoadLibraryEx/);
assert.match(onnxRuntimeSmoke, /OrtGetApiBase/);
assert.match(installerAdapter, /bundleTargets\.includes\("nsis"\)/);
assert.equal(
  windowsConfig.bundle.windows.nsis.installerHooks,
  "packaging/windows/nsis/installer-hooks.nsh",
);
assert.match(installerHook, /!macro NSIS_HOOK_PREINSTALL/);
assert.match(installerHook, /VC_redist\.x64\.exe/);
assert.match(installerHook, /\.\.\\\.\.\\\.\.\\windows-runtime\\nsis\\vc_redist/);
assert.doesNotMatch(installerHook, /\.\.\\\.\.\\\.\.\\target\\windows-runtime/);
assert.match(installerHook, /\/install \/quiet \/norestart/);
assert.match(installerHook, /pinvou-vcredist-temp-preflight\.ps1/);
assert.match(
  installerHook,
  /\$\{__FILEDIR__\}\\.\.\\.\.\\.\.\\.\.\\packaging\\windows\\nsis\\vcredist-temp-preflight\.ps1/,
  "preflight script File source must resolve from the installer.nsi output directory",
);
assert.doesNotMatch(
  installerHook,
  /\$\{__FILEDIR__\}\\.\.\\.\.\\.\.\\packaging\\windows\\nsis\\vcredist-temp-preflight\.ps1/,
  "preflight script File source must not resolve past src-tauri",
);
assert.match(
  installerHook,
  /\$\{__FILEDIR__\}\\.\.\\.\.\\.\.\\.\.\\packaging\\windows\\nsis\\runtime-env\.ps1/,
  "runtime-env script File source must resolve from the installer.nsi output directory",
);
assert.doesNotMatch(
  installerHook,
  /\$\{__FILEDIR__\}\\.\.\\.\.\\.\.\\packaging\\windows\\nsis\\runtime-env\.ps1/,
  "runtime-env script File source must not resolve past src-tauri",
);
assert.ok(
  installerHook.indexOf("pinvou-vcredist-temp-preflight.ps1") <
    installerHook.indexOf('ExecWait \'"$PLUGINSDIR\\VC_redist.x64.exe"'),
  "Windows Installer temp preflight must run before VC++ starts",
);
assert.match(installerHook, /SetEnvironmentVariableW\(w "TEMP", w "\$WINDIR\\Temp"\)/);
assert.match(installerHook, /IntCmp \$5 1632 pinvou_vc_redist_temp_failed/);
assert.match(installerHook, /IntCmp \$5 -2147023264 pinvou_vc_redist_temp_failed/);
assert.match(installerHook, /Pinvou3-vcredist\.log/);
for (const recoveryHint of [
  "释放系统盘空间",
  "检查系统临时目录权限",
  "重启 Windows 后重试",
  "日志：$WINDIR\\Temp\\Pinvou3-vcredist.log",
]) {
  assert.ok(
    installerHook.includes(recoveryHint),
    `VC++ temp failure prompt must include: ${recoveryHint}`,
  );
}
assert.match(vcRedistTempPreflight, /S-1-5-18/);
assert.match(vcRedistTempPreflight, /S-1-5-32-544/);
assert.match(
  vcRedistTempPreflight,
  /SetEnvironmentVariable\(\$Name, \$FallbackPath, "Machine"\)/,
  "machine TEMP/TMP repair must persist the resolved absolute fallback path",
);
assert.doesNotMatch(
  vcRedistTempPreflight,
  /FallbackValue|%SystemRoot%\\Temp/u,
  "machine TEMP/TMP repair must not persist an unexpanded REG_SZ value",
);
assert.doesNotMatch(
  vcRedistTempPreflight,
  /Remove-Item|RemoveAccessRule|PurgeAccessRules/u,
  "VC++ temp repair must not delete installer cache or existing ACL entries",
);
assert.match(
  vcRedistTempPreflight,
  /-ResolvedPath \(\[ref\]\$machineTemp\)/u,
  "TEMP repair must return its path separately from diagnostic output",
);
assert.match(
  vcRedistTempPreflight,
  /-ResolvedPath \(\[ref\]\$machineTmp\)/u,
  "TMP repair must return its path separately from diagnostic output",
);
assert.doesNotMatch(
  vcRedistTempPreflight,
  /Write-Output "(?:Repaired|required|Created|Machine|\$Target)/iu,
  "preflight diagnostics must not contaminate PowerShell function return values",
);

if (process.platform === "win32") {
  const smokeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou-vcredist-preflight-"));
  const fakeWindowsRoot = path.join(smokeRoot, "Windows");
  try {
    const runPreflight = () =>
      spawnSync(
        "powershell.exe",
        [
          "-NoProfile",
          "-ExecutionPolicy",
          "Bypass",
          "-File",
          vcRedistTempPreflightPath,
          "-WindowsRoot",
          fakeWindowsRoot,
          "-SkipMachineEnvironment",
        ],
        { encoding: "utf8" },
      );
    for (let attempt = 1; attempt <= 2; attempt += 1) {
      const result = runPreflight();
      assert.equal(
        result.status,
        0,
        `VC++ temp preflight attempt ${attempt} failed: ${result.stderr || result.stdout}`,
      );
    }
    assert.ok(fs.statSync(path.join(fakeWindowsRoot, "Temp")).isDirectory());
    assert.ok(fs.statSync(path.join(fakeWindowsRoot, "Installer")).isDirectory());

  } finally {
    fs.rmSync(smokeRoot, { recursive: true, force: true });
  }
}
for (const [name, version] of [
  ["MAJOR", vcMajor],
  ["MINOR", vcMinor],
  ["BUILD", vcBuild],
  ["REVISION", vcRevision],
]) {
  assert.ok(
    installerHook.includes(`!define PINVOU_VC_REDIST_MIN_${name} ${version}`),
    `NSIS VC++ ${name.toLowerCase()} minimum must match the runtime lock`,
  );
}
for (const registryValue of ["Major", "Minor", "Bld", "Rbld"]) {
  assert.match(
    installerHook,
    new RegExp(`ReadRegDWORD \\$\\d[^\\r\\n]+"${registryValue}"`),
    `NSIS hook must inspect the installed VC++ ${registryValue} value`,
  );
}
assert.match(
  installerHook,
  /IntCmpU \$1 \$\{PINVOU_VC_REDIST_MIN_MAJOR\} pinvou_vc_redist_check_minor pinvou_vc_redist_install pinvou_vc_redist_ready/,
);
assert.match(
  installerHook,
  /IntCmpU \$2 \$\{PINVOU_VC_REDIST_MIN_MINOR\} pinvou_vc_redist_check_build pinvou_vc_redist_install pinvou_vc_redist_ready/,
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
  runtimeInitScript.includes('$cacheSchema = "v5"') &&
    runtimeScript.includes('$verificationSchemaVersion = 4'),
  "the external LFS cache layout must invalidate v4 Jenkins caches without changing the trusted staging schema",
);
assert.ok(
  runtimeInitScript.includes(
    '$lfsStorageRoot = Join-Path $resolverCacheRoot "lfs-storage"',
  ),
  "Git LFS objects must be stored outside the submodule gitdir",
);
assert.match(
  runtimeInitScript,
  /\$lfsArguments = @\("-c", "lfs\.storage=\$lfsStorageRoot", "lfs", "pull"\)/u,
  "Git LFS pulls must use the external workspace cache",
);
assert.ok(
  runtimeInitScript.lastIndexOf("Update-RuntimeSubmoduleWithoutLfsSmudge") <
    runtimeInitScript.indexOf(
      "New-Item -ItemType Directory -Path $lfsStorageRoot",
    ),
  "a fresh submodule must be initialized before the external LFS cache is used",
);
assert.ok(
  runtimeInitScript.indexOf("if ($CacheKeyOnly)") <
    runtimeInitScript.indexOf(
      "New-Item -ItemType Directory -Path $lfsStorageRoot",
    ),
  "cache-key discovery must not create the external LFS storage directory",
);
assert.ok(
  runtimeGuide.includes(
    "不得把 `.git/modules/private-runtimes/windows/lfs/objects` 作为独立缓存路径",
  ) &&
    runtimeGuide.includes(
      "Jenkins 只恢复 `pinvou3-app/src-tauri/target/windows-runtime`",
    ),
  "Jenkins guidance must not restore a partial submodule gitdir into a fresh workspace",
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
  packageJson.scripts["test:windows-runtime-installer"],
  /windows_runtime_installer_integration\.test\.js/u,
  "the staged runtime and NSIS installer adapter must have an integration test entry",
);
assert.match(
  runtimeInitScript,
  /if \(\$CacheKeyOnly\)[\s\S]*?Write-Output \$cacheKey[\s\S]*?exit 0/u,
  "Jenkins must be able to resolve the runtime cache key before remote access",
);
const windowsRuntimeFilterStart = prCheckWorkflow.indexOf(
  "            windows_runtime:",
);
const fastGateStart = prCheckWorkflow.indexOf(
  "\n  fast-gate:",
  windowsRuntimeFilterStart,
);
assert.ok(
  windowsRuntimeFilterStart >= 0 && fastGateStart > windowsRuntimeFilterStart,
  "PR checks must define a dedicated Windows runtime path filter",
);
const windowsRuntimeFilter = prCheckWorkflow.slice(
  windowsRuntimeFilterStart,
  fastGateStart,
);
for (const requiredPath of [
  "pinvou3-app/src-tauri/packaging/windows/runtime/**",
  "pinvou3-app/src-tauri/config/platforms/windows/runtime/**",
  "pinvou3-app/tests/windows_runtime_*",
  "private-runtimes/windows",
  ".gitmodules",
]) {
  assert.ok(
    windowsRuntimeFilter.includes(requiredPath),
    `Windows runtime PR checks must cover ${requiredPath}`,
  );
}

assert.match(
  installerHook,
  /IntCmpU \$3 \$\{PINVOU_VC_REDIST_MIN_BUILD\} pinvou_vc_redist_check_revision pinvou_vc_redist_install pinvou_vc_redist_ready/,
);
assert.match(
  installerHook,
  /IntCmpU \$4 \$\{PINVOU_VC_REDIST_MIN_REVISION\} pinvou_vc_redist_ready pinvou_vc_redist_install pinvou_vc_redist_ready/,
);
assert.match(
  installerHook,
  /ClearErrors\r?\n\s*ExecWait[^\r\n]+\$5\r?\n\s*IfErrors pinvou_vc_redist_exec_failed/,
  "ExecWait errors must be cleared and handled immediately",
);
assert.match(installerHook, /IntCmp \$5 3010/);
assert.match(installerHook, /IntCmp \$5 1641/);
for (const [label, nextLabel] of [
  ["pinvou_vc_redist_exec_failed", "pinvou_vc_redist_exit_failed"],
  ["pinvou_vc_redist_exit_failed", "pinvou_vc_redist_reboot"],
]) {
  const start = installerHook.indexOf(`${label}:`);
  const end = installerHook.indexOf(`${nextLabel}:`, start);
  assert.notEqual(start, -1, `NSIS hook must define ${label}`);
  assert.ok(end > start, `${label} must end before ${nextLabel}`);
  const failureBranch = installerHook.slice(start, end);
  assert.match(
    failureBranch,
    /MessageBox[^\r\n]+\/SD IDOK/,
    `${label} must not prompt indefinitely during an unattended install`,
  );
  assert.match(failureBranch, /\r?\n\s*Abort\s*(?:\r?\n|$)/, `${label} must abort the install`);
}

assert.deepEqual(windowsBundleTargets(["build"]), ["msi", "nsis"]);
assert.deepEqual(windowsBundleTargets(["build", "--bundles", "msi"]), ["msi"]);
assert.deepEqual(windowsBundleTargets(["build", "--bundles=nsis"]), ["nsis"]);
assert.ok(
  buildScript.indexOf("stageWindowsRuntime()") <
    buildScript.indexOf("stageWindowsInstaller({"),
  "runtime resolver must run before the installer adapter",
);
assert.match(
  buildScript,
  /isDev && process\.platform === "win32"[\s\S]*?stageWindowsOnnxRuntime\(\)/u,
  "Windows dev must stage only the pinned ONNX Runtime before starting Tauri",
);
assert.doesNotMatch(
  buildScript,
  /\(hasTauriBuildCommand \|\| isDev\)[\s\S]*?stageWindowsRuntime\(\)/u,
  "Windows dev must not stage the complete packaging runtime",
);
assert.match(
  buildScript,
  /ORT_DYLIB_PATH:\s*runtime\.onnxRuntimeDylib/u,
  "Windows dev must expose the staged ONNX Runtime to fastembed",
);
assert.ok(
  buildScript.indexOf("stageWindowsInstaller({") <
    buildScript.indexOf("prepareWindowsCodexBridge(windowsBridgeOptions)"),
  "installer resources must be staged before the Bridge and Tauri build",
);
assert.doesNotMatch(
  bridgeScript,
  /WINDOWS_NODE_VERSION|nodejs\.org\/dist|curl\.exe|tar\.exe|runtime\/codex-node/,
  "Codex Bridge must not download or package a private Node copy",
);

const temporaryRoot = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou-nsis-adapter-"));
try {
  const sourcePath = path.join(temporaryRoot, "source", "VC_redist.x64.exe");
  const destinationRoot = path.join(temporaryRoot, "output", "nsis");
  fs.mkdirSync(path.dirname(sourcePath), { recursive: true });
  fs.writeFileSync(sourcePath, "locked-vc-runtime");
  const runtime = {
    vcRedist: {
      sourcePath,
      bytes: fs.statSync(sourcePath).size,
      sha256: crypto.createHash("sha256").update(fs.readFileSync(sourcePath)).digest("hex"),
    },
  };
  assert.equal(
    stageWindowsInstaller({
      platform: "win32",
      bundleTargets: ["msi"],
      runtime,
      destinationRoot,
    }),
    null,
  );
  assert.equal(fs.existsSync(destinationRoot), false);
  const staged = stageWindowsInstaller({
    platform: "win32",
    bundleTargets: ["nsis"],
    runtime,
    destinationRoot,
  });
  assert.equal(fs.readFileSync(staged.vcRedistPath, "utf8"), "locked-vc-runtime");
  fs.writeFileSync(sourcePath, "tampered-vc-runtime");
  assert.throws(
    () =>
      stageWindowsInstaller({
        platform: "win32",
        bundleTargets: ["nsis"],
        runtime,
        destinationRoot,
      }),
    /指纹不匹配/u,
  );
} finally {
  fs.rmSync(temporaryRoot, { recursive: true, force: true });
}

const prValidationJobStart = releaseWorkflow.indexOf(
  "  validate-windows-runtime-pr:",
);
const windowsBuildJobStart = releaseWorkflow.indexOf("  build-windows-x64:");
const macosBuildJobStart = releaseWorkflow.indexOf(
  "  build-macos-universal:",
);
assert.notEqual(prValidationJobStart, -1);
assert.ok(windowsBuildJobStart > prValidationJobStart);
assert.ok(macosBuildJobStart > windowsBuildJobStart);

const prValidationJob = releaseWorkflow.slice(
  prValidationJobStart,
  windowsBuildJobStart,
);
const windowsBuildJob = releaseWorkflow.slice(
  windowsBuildJobStart,
  macosBuildJobStart,
);
assert.match(prValidationJob, /github\.event_name == 'pull_request'/);
assert.match(prValidationJob, /npm run test:windows-runtime/);
assert.match(prValidationJob, /npm run test:codex-acp-windows-runtime/);
assert.match(prValidationJob, /submodules: false/);
assert.doesNotMatch(
  prValidationJob,
  /PINVOU3_WINDOWS_RUNTIME_TOKEN|secrets\./,
  "PR validation must never reference private credentials",
);

assert.match(windowsBuildJob, /github\.event_name != 'pull_request'/);
assert.match(windowsBuildJob, /github\.ref == 'refs\/heads\/main'/);
assert.match(windowsBuildJob, /environment:\s*\n\s*name: windows-release/);
assert.match(windowsBuildJob, /persist-credentials:\s*false/);
assert.doesNotMatch(
  windowsBuildJob,
  /PINVOU3_WINDOWS_RUNTIME_TOKEN/,
  "public Windows runtime builds must not depend on runtime repository credentials",
);
assert.match(windowsBuildJob, /npm run runtime:windows:init/);
assert.match(windowsBuildJob, /npm run runtime:windows:onnx-smoke/);
assert.doesNotMatch(
  windowsBuildJob,
  /runtime-access|available == 'true'|available != 'true'/,
  "protected Windows builds must not fall back to a reduced PR path",
);

for (const triggerPath of [
  ".gitmodules",
  "private-runtimes/windows",
  "pinvou3-app/package-lock.json",
  "pinvou3-app/scripts/tauri/windows-runtime.js",
  "pinvou3-app/src-tauri/config/platforms/windows/**",
  "pinvou3-app/src-tauri/packaging/windows/**",
  "pinvou3-app/tests/windows_runtime_packaging_contract.test.js",
]) {
  const escapedPath = triggerPath.replace(/[.*+?^${}()|[\]\\]/gu, "\\$&");
  const occurrences = releaseWorkflow.match(new RegExp(escapedPath, "gu")) ?? [];
  assert.ok(
    occurrences.length >= 2,
    `${triggerPath} must trigger both pull_request and push builds`,
  );
}
assert.match(
  releaseWorkflow,
  /git diff --name-only "\$BEFORE" "\$GITHUB_SHA"/,
);
assert.match(
  releaseWorkflow,
  /build_required: \$\{\{ steps\.diff\.outputs\.build_required \}\}/,
);

console.log("Windows runtime packaging contract: ok");
