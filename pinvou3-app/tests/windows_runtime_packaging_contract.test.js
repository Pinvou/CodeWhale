const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const { windowsBundleTargets } = require("../scripts/tauri/build.js");
const { stageWindowsInstaller } = require("../scripts/tauri/windows-installer.js");
const { resolveDescriptorPath } = require("../scripts/tauri/windows-runtime.js");

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
const releaseAssetsScript = readApp("src-tauri", "packaging", "windows", "runtime", "scripts", "release-assets.ps1");
const runtimeScript = readApp(
  "src-tauri",
  "packaging",
  "windows",
  "runtime",
  "scripts",
  "resolve-runtime.ps1",
);
const pythonDependencyRuntimeTest = readApp(
  "tests",
  "windows_python_dependency_contract.ps1",
);
const onnxRuntimeScript = readApp(
  "src-tauri",
  "packaging",
  "windows",
  "runtime",
  "scripts",
  "stage-onnx-runtime.ps1",
);
const stageRuntimeWrapper = readApp(
  "src-tauri",
  "packaging",
  "windows",
  "runtime",
  "scripts",
  "stage-runtime.ps1",
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
const runtimeCacheBehaviorHarness = readApp(
  "tests",
  "windows_runtime_cache_behavior.ps1",
);
const runtimeReleaseBehaviorHarness = readApp(
  "tests",
  "windows_runtime_release_behavior.ps1",
);
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
  "Test-PythonDependencyTarget",
  "Test-PythonDependencyTargets",
  "Test-PythonWheelTarget",
  "python_dependencies",
  "windows-x64",
  "files.pythonhosted.org",
  // Long-path hardening: the staged node tree (npm's bundled node_modules)
  // passes MAX_PATH on the self-hosted windows-heavy runners, so every
  // staging operation must stay inside the \\?\ namespace or robocopy.
  "Get-ExtendedPath",
  "Get-LongPathFiles",
  "Get-PlainPath",
  "Remove-TreeLongPath",
  "Expand-ZipArchiveLongPath",
]) {
  assert.ok(runtimeScript.includes(contract), `runtime staging must retain ${contract}`);
}
const reusableStageStart = runtimeScript.indexOf("function Test-VerifiedStageReusable");
const freshStageStart = runtimeScript.indexOf("function Stage-Submodule", reusableStageStart);
const onnxDescriptorStart = runtimeScript.indexOf(
  "function Get-OnnxDevDescriptorContent",
  freshStageStart,
);
assert.ok(reusableStageStart >= 0 && freshStageStart > reusableStageStart);
assert.ok(onnxDescriptorStart > freshStageStart);
assert.match(
  runtimeScript.slice(reusableStageStart, freshStageStart),
  /Test-PythonDependencyTargets/,
  "verified stage reuse must revalidate bundled Python ABI and wheel locks",
);
assert.match(
  runtimeScript.slice(freshStageStart, onnxDescriptorStart),
  /Assert-FreshStagePythonDependencies/,
  "fresh runtime staging must validate bundled Python ABI and wheel locks",
);
assert.match(
  pythonDependencyRuntimeTest,
  /ABI mismatch must fail closed/,
  "PowerShell contract must exercise fail-closed ABI validation",
);
assert.match(
  runtimeScript,
  /\$null = & \$pythonExe -I -S -B -c \$probe/u,
  "ABI probe stdout must be discarded so stray output cannot flip the exit-code check",
);
assert.match(
  packageJson.scripts["test:windows-runtime"],
  /windows_python_dependency_contract\.ps1/u,
  "the windows runtime npm chain must execute the python dependency contract, or the ps1 loses its only CI runner",
);
assert.match(runtimeScript, /Get-Sha256 -Path \$sourcePath/u);
assert.match(runtimeScript, /vcRedist\.minimumVersion/u);
assert.match(runtimeScript, /System\.Diagnostics\.FileVersionInfo/u);
assert.match(runtimeScript, /\$vcActualVersion -lt \$vcMinimumVersion/u);
assert.match(runtimeScript, /HashSet\[string\]/u);
assert.match(runtimeScript, /Remove-Item -LiteralPath \$bundledAsrModelPath/u);
assert.match(
  runtimeScript,
  /& robocopy \$requiredDirectory \$Destination \/E /u,
  "the flattened runtime copy arm must go through long-path-native robocopy",
);
assert.match(
  runtimeScript,
  /& robocopy \$emptyRoot \$Path \/MIR /u,
  "runtime staging tree cleanup must go through long-path-native robocopy",
);
// Review 5796830161: robocopy reports success with exit codes 0-7 (a /MIR
// cleanup of a non-empty tree usually returns 2, an /E copy 1 or 3). Both
// wrapper scripts forward $LASTEXITCODE as their process exit code after the
// resolver returns, so a leftover informational code used to fail an otherwise
// verified staging and break the Windows dev/build chain. Each robocopy arm
// must therefore clear the success boundary itself — only AFTER the >= 8
// throw, so real robocopy failures stay fatal.
const removeTreeFnStart = runtimeScript.indexOf("function Remove-TreeLongPath");
const removeTreeFnEnd = runtimeScript.indexOf("\nfunction ", removeTreeFnStart);
assert.ok(removeTreeFnStart >= 0 && removeTreeFnEnd > removeTreeFnStart);
const removeTreeFnBody = runtimeScript.slice(removeTreeFnStart, removeTreeFnEnd);
const mirThrowIndex = removeTreeFnBody.indexOf('throw ("Robocopy /MIR cleanup failed');
const mirResetIndex = removeTreeFnBody.indexOf("$global:LASTEXITCODE = 0");
assert.ok(mirThrowIndex > 0, "the /MIR cleanup arm must keep its >= 8 failure throw");
assert.ok(
  mirResetIndex > mirThrowIndex,
  "the /MIR cleanup arm must clear $LASTEXITCODE only after the >= 8 throw (robocopy >= 8 stays fatal)",
);
const flattenedFnStart = runtimeScript.indexOf("function Expand-FlattenedRuntime");
const flattenedFnEnd = runtimeScript.indexOf("\nfunction ", flattenedFnStart);
assert.ok(flattenedFnStart >= 0 && flattenedFnEnd > flattenedFnStart);
const flattenedFnBody = runtimeScript.slice(flattenedFnStart, flattenedFnEnd);
const copyThrowIndex = flattenedFnBody.indexOf('throw ("Robocopy failed with exit code');
const copyResetIndex = flattenedFnBody.indexOf("$global:LASTEXITCODE = 0");
assert.ok(copyThrowIndex > 0, "the flattened /E copy arm must keep its >= 8 failure throw");
assert.ok(
  copyResetIndex > copyThrowIndex,
  "the flattened /E copy arm must clear $LASTEXITCODE only after the >= 8 throw (robocopy >= 8 stays fatal)",
);
assert.equal(
  (runtimeScript.match(/\$global:LASTEXITCODE = 0/gu) || []).length,
  2,
  "exactly the two robocopy arms clear the success boundary; every other external command in the resolver (git, the python ABI probe) leaves $LASTEXITCODE at 0 on success and must be re-audited before this count grows",
);
for (const wrapperScript of [stageRuntimeWrapper, onnxRuntimeScript]) {
  assert.match(
    wrapperScript,
    /if \(\$LASTEXITCODE -ne 0\) \{[\s\S]*?exit \$LASTEXITCODE/u,
    "the wrapper must keep forwarding the resolver's $LASTEXITCODE as the process exit code — that propagation is the reason the resolver's success boundary must be cleared in the resolver itself",
  );
}
assert.match(
  runtimeScript,
  /\[System\.IO\.File\]::OpenRead\(\(Get-ExtendedPath \$Path\)\)/u,
  "staged-file hashing must stream through the extended-length namespace",
);
assert.match(
  runtimeScript,
  /Get-LongPathFiles -Root \$temporary \|/u,
  "the flattened runtime required-file search must enumerate past MAX_PATH",
);
assert.match(
  runtimeScript,
  /\[System\.IO\.Compression\.ZipFileExtensions\]::ExtractToFile\(\$entry, \(Get-ExtendedPath \$entryTargetPath\), \$true\)/u,
  "archive extraction must materialize entries through the extended-length namespace",
);
assert.doesNotMatch(
  runtimeScript,
  /ExtractToDirectory\(/u,
  "ZipFile.ExtractToDirectory normalizes target paths and fails past MAX_PATH; it must not come back",
);
assert.match(
  runtimeScript,
  /\(\$Path -split '\[\\\\\/\]'\) -contains '\.\.'/u,
  "the staging containment guard must refuse traversal segments outright (GetFullPath skips normalization on \\\\?\\ device paths, so an interior '..' would not collapse before the comparison)",
);
assert.match(
  runtimeScript,
  /Assert-ChildPath -Root \$Destination -Path \$entryTargetPath/u,
  "archive extraction must enforce per-entry containment inside the destination",
);
assert.doesNotMatch(
  runtimeScript,
  /Expand-Archive\(/u,
  "Expand-Archive is provider-routed and MAX_PATH-bound; archive extraction must stay on the long-path channel",
);
assert.match(
  runtimeScript,
  /Get-PlainPath \(\[System\.IO\.Path\]::GetFullPath\(\(Get-ExtendedPath \(\$Path\.Replace\('\/', '\\'\)\)\)\)\)/u,
  "the containment comparison must strip the device prefix by shape (Get-PlainPath), not by a fixed offset that corrupts UNC roots",
);
// Review 5800111649 non-blocking follow-up: root and child must reach the
// containment comparison in the SAME path namespace — GetFullPath keeps an
// explicit \\?\ prefix on the root while the child chain strips it, so an
// explicitly extended RuntimeRoot used to be mis-rejected as outside the
// root for its own children. Both containment helpers must run the root
// through the same extend-canonicalize-strip chain (plain inputs unchanged).
assert.match(
  runtimeScript,
  /Get-PlainPath \(\[System\.IO\.Path\]::GetFullPath\(\(Get-ExtendedPath \(\$Root\.Replace\('\/', '\\'\)\)\)\)\)/u,
  "the root side of containment must take the same namespace form as the child (an explicitly extended RuntimeRoot must not be mis-rejected for its own children)",
);
assert.match(
  runtimeScript,
  /\$script:RuntimeExpandCleanupFailures = @\(\$script:RuntimeExpandCleanupFailures\) \+ @\{ Path = \$temporary/u,
  "extraction cleanup failures must be recorded by path so litter is claimed by record instead of diffing pinvou3-runtime-expand-* names in shared TEMP (a before/after name diff can swallow a concurrent run's active extraction tree on a self-hosted runner)",
);
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
assert.match(runtimeInitScript, /Sync-RuntimeReleaseAssets/);
assert.match(runtimeInitScript, /\[switch\]\$OnnxOnly/);
assert.match(releaseAssetsScript, /component -ceq 'onnxruntime'/);
assert.match(runtimeInitScript, /pinvou3-windows-runtime-\$cacheSchema-\$expectedCommit/u);

assert.match(runtimeWrapper, /runtime-descriptor\.json/);
assert.match(runtimeWrapper, /cleanupLegacyWindowsNodeStaging/);
assert.match(runtimeWrapper, /download-on-first-use/);
assert.match(runtimeWrapper, /onnxRuntimeDylib/);
assert.match(runtimeWrapper, /stageWindowsOnnxRuntime/);
assert.match(onnxRuntimeScript, /-Mode StageOnnx/);
assert.match(runtimeScript, /Get-VerifiedOnnxManifest/);
assert.match(runtimeScript, /Stage-OnnxRuntime/);
// The ZIP extraction parent calculation must stay on the long-path channel:
// Path.GetDirectoryName on the plain entry path normalizes its argument and
// throws past MAX_PATH on Windows PowerShell 5.1 hosts with
// UseLegacyPathHandling=false plus BlockLongPaths=true, before the extended
// directory creation or file extraction can run. The parent is derived on
// the \\?\ form (device paths skip normalization) and stripped back to the
// plain shape for the CreateDirectory/ExtractToFile consumers.
assert.doesNotMatch(
  runtimeScript,
  /\[System\.IO\.Path\]::GetDirectoryName\(\$entryTargetPath\)/u,
  "the ZIP extraction parent must not be computed from the plain entry path (GetDirectoryName normalizes and throws past MAX_PATH on long-path-blocking hosts)",
);
assert.match(
  runtimeScript,
  /Get-PlainPath \(\[System\.IO\.Path\]::GetDirectoryName\(\(Get-ExtendedPath \$entryTargetPath\)\)\)/u,
  "the ZIP extraction parent must be derived on the extended \\?\ form and stripped back to the plain shape",
);
assert.match(onnxRuntimeSmoke, /LoadLibraryEx/);
assert.match(onnxRuntimeSmoke, /OrtGetApiBase/);
// The ONNX dev descriptor key produced by resolve-runtime.ps1's
// Get-RelativeStagePath must flow through the real Node descriptor consumer:
// the exact slash-joined production shape resolves under src-tauri, while the
// leading-slash shape a swallowed string concatenation in the PowerShell root
// normalization used to emit must be rejected as an absolute path (the
// stageWindowsOnnxRuntime consumer would otherwise fail dev startup before
// any staged file is touched).
{
  const descriptorKey =
    "target/windows-runtime/dcde8f01abcd-0123456789ab-onnx-dev/onnxruntime/onnxruntime.dll";
  assert.equal(
    resolveDescriptorPath(descriptorKey, "onnxRuntimeDylib"),
    path.resolve(appRoot, "src-tauri", descriptorKey),
    "the production ONNX dev descriptor key must resolve under src-tauri",
  );
  assert.throws(
    () => resolveDescriptorPath(`/${descriptorKey}`, "onnxRuntimeDylib"),
    /必须是相对路径/u,
    "a leading-slash descriptor key must be rejected as an absolute path",
  );
}
assert.match(installerAdapter, /bundleTargets\.includes\("nsis"\)/);
assert.equal(
  windowsConfig.bundle.windows.nsis.installerHooks,
  "packaging/windows/nsis/installer-hooks.nsh",
);
assert.match(installerHook, /!macro NSIS_HOOK_PREINSTALL/);
assert.match(installerHook, /VC_redist\.x64\.exe/);
assert.match(
  installerHook,
  /!define PINVOU_HOOKS_DIR "\$\{__FILEDIR__\}"/,
  "hook macro bodies must anchor the hooks directory at file level: ${__FILEDIR__} inside !macro keeps the insertion-site value",
);
assert.match(
  installerHook,
  /\$\{PINVOU_HOOKS_DIR\}\\\.\.\\\.\.\\\.\.\\target\\windows-runtime\\nsis\\vc_redist/,
  "vc_redist is staged to src-tauri\\target\\windows-runtime\\nsis by windows-installer.js",
);
assert.doesNotMatch(
  installerHook,
  /File "[^"]*\$\{__FILEDIR__\}/,
  "File sources must go through ${PINVOU_HOOKS_DIR}, not the macro insertion-site ${__FILEDIR__}",
);
assert.match(installerHook, /\/install \/quiet \/norestart/);
assert.match(installerHook, /pinvou-vcredist-temp-preflight\.ps1/);
assert.match(
  installerHook,
  /\$\{PINVOU_HOOKS_DIR\}\\\.\.\\\.\.\\\.\.\\packaging\\windows\\nsis\\vcredist-temp-preflight\.ps1/,
  "hook-packed scripts must resolve against src-tauri (both hook dirs sit three levels below it)",
);
assert.doesNotMatch(
  installerHook,
  /\$\{PINVOU_HOOKS_DIR\}\\\.\.\\\.\.\\\.\.\\\.\.\\packaging/,
);
assert.match(
  installerHook,
  /\$\{PINVOU_HOOKS_DIR\}\\\.\.\\\.\.\\\.\.\\packaging\\windows\\nsis\\runtime-env\.ps1/,
  "runtime-env script File source must resolve against src-tauri via the file-level hooks anchor",
);
assert.doesNotMatch(
  installerHook,
  /\$\{PINVOU_HOOKS_DIR\}\\\.\.\\\.\.\\\.\.\\\.\.\\packaging/,
);
assert.ok(
  installerHook.indexOf("pinvou-vcredist-temp-preflight.ps1") <
    installerHook.indexOf('ExecWait \'"$PLUGINSDIR\\VC_redist.x64.exe"'),
  "Windows Installer temp preflight must run before VC++ starts",
);
assert.match(installerHook, /SetEnvironmentVariableW\(w "TEMP", w "\$WINDIR\\Temp"\)/);
assert.match(installerHook, /IntCmp \$5 1632 pinvou_vc_redist_temp_failed/);
assert.match(installerHook, /IntCmp \$5 -2147023264 pinvou_vc_redist_temp_failed/);
// The exit-ladder pins carry the full jump targets, not just the compared
// code: a one-letter drift in a target spelling is a makensis-dangling
// label that no CI lane compiles away (no PR lane runs makensis), so the
// prefix-only form cannot catch it. 1638 = ERROR_PRODUCT_VERSION: a newer
// VC++ runtime already present must count as ready, not abort the install.
assert.match(installerHook, /IntCmp \$5 3010 pinvou_vc_redist_reboot 0 0/);
assert.match(installerHook, /IntCmp \$5 1638 pinvou_vc_redist_ready 0 0/);
assert.match(
  installerHook,
  /IntCmp \$5 1641 pinvou_vc_redist_reboot pinvou_vc_redist_exit_failed pinvou_vc_redist_exit_failed/,
);
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
// SID constants alone are not a behavior pin: require the full grant chain
// (both SIDs in the applied set, rule granted and written back, every ensured
// directory running the grant) so deleting the ACL repair still fails here.
assert.match(
  vcRedistTempPreflight,
  /\$requiredSids = @\(\$systemSid, \$administratorsSid\)/,
  "SYSTEM and Administrators SIDs must form the required-access set",
);
assert.match(
  vcRedistTempPreflight,
  /\[void\]\$acl\.AddAccessRule\(\$rule\)/,
  "the required-access set must be granted through ACL access rules",
);
assert.match(
  vcRedistTempPreflight,
  /Set-Acl -LiteralPath \$Path -AclObject \$acl/,
  "granted ACL rules must be written back to the repaired directory",
);
assert.match(
  vcRedistTempPreflight,
  /Add-RequiredAccess -Path \$Path/,
  "every ensured system directory must receive the required ACL entries",
);
assert.match(
  vcRedistTempPreflight,
  /Ensure-SystemDirectory -Path \$windowsTempPath/,
  "the Windows temp directory must run the ACL-preserving ensure",
);
assert.match(
  vcRedistTempPreflight,
  /Ensure-SystemDirectory -Path \$windowsInstallerPath -HiddenSystem/,
  "the Windows Installer directory must run the ACL-preserving ensure",
);
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
  /Sync-RuntimeReleaseAssets/u,
  "runtime initialization must invoke the verified Release transport",
);
assert.match(releaseAssetsScript, /\$handler\.AllowAutoRedirect = \$false/u,
  "redirects must remain subject to per-hop endpoint validation");
assert.ok(releaseAssetsScript.indexOf("Assert-RuntimeReleaseUri -Uri $Uri", releaseAssetsScript.indexOf("function Write-RuntimeReleaseDownload")) <
  releaseAssetsScript.indexOf("$client.GetAsync"), "URI policy must execute before every request");
assert.match(releaseAssetsScript, /Get-RuntimeReleaseProxy/u,
  "the production transport must preserve explicit Runner proxy routing");
assert.doesNotMatch(releaseAssetsScript, /ServerCertificateCustomValidationCallback|ServerCertificateValidationCallback|DangerousAcceptAnyServerCertificateValidator/u,
  "transport tests must never weaken production certificate validation");
assert.match(releaseAssetsScript, /& \$Download \$uri \$temporary \(\[long\]\$entry\.bytes\)/u,
  "the byte budget must come from the verified immutable manifest");
assert.match(
  releaseAssetsScript,
  /Test-RuntimeReleaseFile -Path \$target -Entry \$entry/u,
  "runtime initialization must verify materialized files before offline reuse",
);
assert.match(
  runtimeInitScript,
  /pinvou3-windows-runtime-\$cacheSchema-\$expectedCommit-\$\(\$lockSha256\.Substring/u,
  "runtime initialization must emit a schema, commit and lock-scoped Jenkins cache key",
);
assert.ok(
  runtimeInitScript.includes('$cacheSchema = "v6"') &&
    runtimeScript.includes('$verificationSchemaVersion = 5'),
  "Release transport must use a fresh cache schema while retaining the trusted Python-dependency staging gate",
);
assert.match(
  runtimeScript,
  /Get-ChildItem -LiteralPath \$manifestRoot -Filter manifest\.json -File -Recurse/iu,
  "the Python dependency gate must enumerate every built-in MCP manifest",
);
assert.match(
  runtimeScript,
  /Test-PythonWheelTarget -Filename \$wheel\.filename -Target \$wheelTarget/u,
  "the Python dependency gate must validate locked wheel PEP 425 tags",
);
assert.match(
  runtimeScript,
  /\$wheelTarget = "cp\$\{versionDigits\}-win_amd64"/u,
  "the Python dependency gate must bind wheel validation to the declared interpreter target",
);
assert.ok(
  runtimeInitScript.includes(
    '$releaseCacheRoot = Join-Path $resolverCacheRoot "release-assets"',
  ),
  "verified Release assets must be cached outside the submodule gitdir",
);
assert.match(
  runtimeInitScript,
  /Sync-RuntimeReleaseAssets -RuntimeRoot \$runtimeRoot -CacheRoot \$releaseCacheRoot/u,
  "initialization must use the hash-verified Release transport",
);
assert.doesNotMatch(runtimeInitScript, /"lfs", "pull"/u, "initialization must not download Git LFS objects");
assert.match(runtimeInitScript, /\[Alias\("ForceLfsPull"\)\]/u,
  "legacy ForceLfsPull callers must retain their ForceDownload alias");
const runtimePrJob = prCheckWorkflow.slice(prCheckWorkflow.indexOf("\n  windows-runtime-test:"),
  prCheckWorkflow.indexOf("\n  macos-codex-runtime-test:"));
for (const script of ["windows_runtime_cache_behavior.ps1", "windows_runtime_release_behavior.ps1"]) {
  assert.ok(runtimePrJob.includes(script), `${script} must run in the ordinary PR runtime job`);
}
assert.match(runtimePrJob, /@\('powershell', 'pwsh'\)/u,
  "runtime regressions must exercise both Windows PowerShell and PowerShell 7");
assert.ok(runtimePrJob.includes("scripts/ci-fixture-process.py --receipt $receipt"),
  "runtime behavior regressions must preserve process ownership and retirement receipts");
assert.ok(
  runtimeInitScript.lastIndexOf("Update-RuntimeSubmoduleWithoutLfsSmudge") <
    runtimeInitScript.indexOf("  Sync-RuntimeReleaseAssets"),
  "a fresh submodule must be initialized before its locked manifest selects Release assets",
);
assert.ok(
  runtimeInitScript.indexOf("if ($CacheKeyOnly)") <
    runtimeInitScript.indexOf("  Sync-RuntimeReleaseAssets"),
  "cache-key discovery must not initialize runtime payloads or contact GitHub",
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
  "submodule checkout must disable automatic LFS downloads before Release materialization",
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
// The release lane is the only place the two wrapper scripts can run against
// a fully staged runtime, and no other lane exercises the wrapper exit
// contract at all (`runtime:windows:stage` invokes the resolver directly, so
// its -File process code never sees the wrapper forwarding). Pin the tests'
// presence so they cannot be silently dropped (review 5796830161).
const runtimeInstallerIntegration = readApp(
  "tests",
  "windows_runtime_installer_integration.test.js",
);
assert.match(
  runtimeInstallerIntegration,
  /runRuntimeWrapper\("stage-runtime\.ps1"/u,
  "the release-lane integration test must observe the stage wrapper's final process exit code through the unmodified script",
);
assert.match(
  runtimeInstallerIntegration,
  /runRuntimeWrapper\("stage-onnx-runtime\.ps1", \["-Force"\]\)/u,
  "the release-lane integration test must observe the ONNX wrapper's rebuild path (robocopy /MIR cleanup) through the unmodified script",
);
assert.match(
  runtimeInstallerIntegration,
  /exit \/b 8/u,
  "the release-lane integration test must prove a stubbed robocopy >= 8 keeps the wrapper process exit code non-zero",
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
  "pinvou3-app/resources/mcp-servers/**/manifest.json",
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
  ["pinvou_vc_redist_exec_failed", "pinvou_vc_redist_temp_failed"],
  ["pinvou_vc_redist_temp_failed", "pinvou_vc_redist_exit_failed"],
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
// Positive anchor: only a build/bundle command may widen into the full
// packaging runtime. A negative "(hasTauriBuildCommand || isDev)" scan would
// also hit the unrelated Windows toolchain-check gate above. Official's
// runtime staging widens through the NSIS-staging validator, so the anchor
// points at that gate expression.
assert.match(
  buildScript,
  /hasTauriBuildCommand && process\.platform === "win32"\s*\?\s*\(usesValidatedNsisStaging\(args\)/u,
  "only a build/bundle command must stage the complete packaging runtime",
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
function verifyRuntimePrSupervision(job) {
  for (const entry of ["test:windows-runtime", "test:windows-runtime-cache"]) {
    assert.ok(job.includes(`python ../scripts/ci-fixture-process.py --receipt "$PINVOU_CI_RESOURCE_LEDGER/${entry === "test:windows-runtime" ? "packaging" : "cache-release"}.json" -- cmd.exe /d /c "npm run ${entry}"`),
      `${entry} must execute inside the owned process supervisor`);
  }
  assert.doesNotMatch(job, /^    continue-on-error:/mu, "the release PR job must remain blocking");
  const steps = job.split("\n      - name: ").slice(1);
  const advisory = steps.filter(step => step.includes("\n        continue-on-error:"));
  assert.equal(advisory.length, 1, "only the reviewed receipt upload may be advisory");
  assert.ok(advisory[0].startsWith("上传 Windows runtime PR 资源收据\n"));
  for (const field of ["timeout-minutes: 3", "retention-days: 7", "if: always()",
    "uses: actions/upload-artifact@v7", "continue-on-error: true", "if-no-files-found: warn",
    "path: ${{ runner.temp }}/windows-runtime-pr-${{ github.run_id }}-${{ github.run_attempt }}/"]) {
    assert.ok(advisory[0].includes(field), `release PR resource receipts must preserve ${field}`);
  }
  const transportEnv = [
    "        env:",
    "          NODE_OPTIONS: ${{ env.PINVOU_CI_NETWORK_NODE_OPTIONS }}",
    "          PATH: ${{ env.PINVOU_CI_NETWORK_PATH }}",
    "          PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1'",
    "",
  ].join("\n");
  assert.ok(advisory[0].includes(transportEnv), "receipt upload must use the scoped storage transport");
  assert.doesNotMatch(advisory[0].replace(transportEnv, ""), /^        (?:run|env):/mu);
}
verifyRuntimePrSupervision(prValidationJob);
for (const mutant of [
  prValidationJob.replace('-- cmd.exe /d /c "npm run test:windows-runtime-cache"', '-- cmd.exe /d /c "echo ignored"'),
  prValidationJob.replace('        timeout-minutes: 3\n', ''),
  prValidationJob.replace('          retention-days: 7\n', '          retention-days: 90\n'),
  prValidationJob.replace('        continue-on-error: true\n', '').replace('      - name: 验证 Windows 打包契约\n', '      - name: 验证 Windows 打包契约\n        continue-on-error: true\n'),
]) {
  assert.notEqual(mutant, prValidationJob, "the negative control must change the real entry");
  assert.throws(() => verifyRuntimePrSupervision(mutant), "supervision or failure suppression must be rejected");
}
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
assert.match(prValidationJob, /test_windows_release_workspace\.ps1/);
const workspacePrepareStep = windowsBuildJob.indexOf("id: windows-workspace");
assert.ok(workspacePrepareStep > 0);
assert.ok(
  workspacePrepareStep < windowsBuildJob.indexOf("git submodule update"),
  "the pinned checkout must be selected before any submodule is initialized",
);
// All run steps that consume source or output must use the same checkout.
// Runner transport helpers use the original checkout, as does cleanup.
const runnerTransport = /^node scripts\/(?:ci-rust-network\.cjs(?: --report)?|ci-network-node-options\.cjs|ci-artifact-node-options\.cjs --network)$/u;
for (const step of windowsBuildJob.split(/\n      - name:/u).slice(1)) {
  const run = /\n        run: ([^\n]+)/u.exec(step);
  if (!run || /id: windows-workspace| -Mode Cleanup/u.test(step) || runnerTransport.test(run[1])) continue;
  assert.match(
    step,
    /working-directory: \$\{\{ env\.PINVOU_WINDOWS_BUILD_ROOT \}\}/u,
    "Windows source, runtime and installer steps must share the selected short checkout",
  );
}
assert.match(
  windowsBuildJob,
  /path: \|\n\s+\$\{\{ env\.PINVOU_WINDOWS_BUILD_ROOT \}\}[^\n]+\.exe\n\s+\$\{\{ env\.PINVOU_WINDOWS_BUILD_ROOT \}\}[^\n]+\.sha256/u,
);
assert.match(
  windowsBuildJob,
  /if: always\(\) && steps\.windows-workspace\.outputs\.state != ''[\s\S]+-Mode Cleanup/u,
  "failed or successful builds must clean up only their owned temporary checkout",
);
// The installer integration test self-skips unless the runtime is staged and
// it is invoked directly with --require-staged-runtime; the release job goes
// through the npm alias, and the pin below keeps that alias a direct node
// invocation (node --test would drop the flag and skip silently forever).
const windowsInitStep = windowsBuildJob.indexOf("npm run runtime:windows:init");
const windowsStageStep = windowsBuildJob.indexOf("npm run runtime:windows:stage");
const windowsInstallerTestStep = windowsBuildJob.indexOf(
  "npm run test:windows-runtime-installer",
);
assert.ok(
  windowsStageStep > windowsInitStep,
  "the release job must stage the Windows runtime after materializing the submodule",
);
assert.ok(
  windowsInstallerTestStep > windowsStageStep,
  "the installer integration test must run directly, after the runtime is staged",
);
// The npm alias must invoke node directly with the flag: node --test would
// drop --require-staged-runtime and the test would silently skip.
assert.match(
  packageJson.scripts["test:windows-runtime-installer"],
  /node tests\/windows_runtime_installer_integration\.test\.js --require-staged-runtime/u,
);
assert.doesNotMatch(
  windowsBuildJob,
  /runtime-access|available == 'true'|available != 'true'/,
  "protected Windows builds must not fall back to a reduced PR path",
);

for (const triggerPath of [
  ".gitmodules",
  "scripts/windows-release-workspace.ps1",
  "scripts/tests/test_windows_release_workspace.ps1",
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

// Windows PowerShell 5.1 spawned under a pwsh 7 parent (CI step shells, pwsh
// terminals) inherits pwsh's PSModulePath: built-in module autoload then
// fails (Add-Type and module-dependent cmdlets raise CommandNotFound), so
// the native harnesses die before their assertions while the aggregate
// CI step still reports green. Each direct 5.1 entry point must restore the
// host default module path before its first module-dependent cmdlet.
const modulePathGuard =
  /PSVersionTable\.PSVersion\.Major -lt 6[\s\S]*?System32\\WindowsPowerShell\\v1\.0\\Modules/u;
assert.match(
  runtimeScript,
  modulePathGuard,
  "the runtime resolver must restore the 5.1 module path (it runs as a child process and is dot-sourced by the wrappers)",
);
assert.match(
  vcRedistTempPreflight,
  modulePathGuard,
  "the VC++ temp preflight must restore the 5.1 module path itself (nothing runs before it)",
);
assert.match(
  runtimeInitScript,
  modulePathGuard,
  "the runtime init script must restore the 5.1 module path before its lock-file hashing",
);
for (const [name, harness, firstDependency] of [
  ["windows_runtime_cache_behavior.ps1", runtimeCacheBehaviorHarness, "$appRoot ="],
  ["windows_runtime_release_behavior.ps1", runtimeReleaseBehaviorHarness, ". (Join-Path"],
]) {
  assert.match(harness, modulePathGuard, `${name} must restore its own 5.1 module path`);
  const guardStart = harness.indexOf("if ($PSVersionTable.PSVersion.Major -lt 6)");
  const dependencyStart = harness.indexOf(firstDependency);
  assert.ok(
    guardStart >= 0 && dependencyStart > guardStart,
    `${name} must restore the module path before resolving or importing fixture dependencies`,
  );
}
assert.ok(
  runtimeScript.indexOf("PSVersionTable.PSVersion.Major -lt 6") <
    runtimeScript.indexOf("Add-Type -AssemblyName"),
  "the resolver must restore the module path before Add-Type so module autoload works",
);
assert.match(
  runtimeScript,
  /Get-CachedVerifiedManifest -VerifyContent:\(\$Mode -eq "Validate"\)/u,
  "Stage may trust cached source metadata; Validate must re-hash the locked content",
);
assert.ok(
  runtimeScript.includes(
    "if ($VerifyContent -and (Get-Sha256 -Path $sourcePath) -ne [string]$entry.sha256)",
  ),
  "the Validate-mode content re-hash guard must stay inside Get-CachedVerifiedManifest, not just at its call site",
);
// The size-only cache is safe only while the cached path re-runs the full
// runtime identity check itself (marker match alone must never authorize
// per-file sizes), and a vanished checkout must decline to the full path so
// the actionable "not initialized" message survives the cache. Pin the call
// ordering inside Get-CachedVerifiedManifest's own body; removing either the
// checkout guard or the identity re-check there must fail here loudly.
const cachedFnStart = runtimeScript.indexOf("function Get-CachedVerifiedManifest");
const cachedMarkerIdx = runtimeScript.indexOf(
  "Test-VerificationMarker -Path $sourceVerificationMarkerPath",
  cachedFnStart,
);
const cachedCheckoutIdx = runtimeScript.indexOf(
  "Test-Path -LiteralPath $RuntimeRoot -PathType Container",
  cachedFnStart,
);
const cachedIdentityIdx = runtimeScript.indexOf("Assert-RuntimeIdentity", cachedFnStart);
const cachedSizeIdx = runtimeScript.indexOf(
  "$sourcePath).Length -ne [long]$entry.bytes",
  cachedFnStart,
);
assert.ok(
  cachedFnStart !== -1 &&
    cachedMarkerIdx > cachedFnStart &&
    cachedCheckoutIdx > cachedMarkerIdx &&
    cachedIdentityIdx > cachedCheckoutIdx &&
    cachedSizeIdx > cachedIdentityIdx,
  "the cached path must decline to the full path when the checkout is gone and re-run the runtime identity check between the marker check and the per-file size loop, not trust sizes on the marker alone",
);
assert.equal(
  (runtimeScript.match(/Write-Utf8Atomically -Path \$sourceVerificationMarkerPath/gu) || []).length,
  2,
  "the workspace source-metadata marker must be rewritten on both the full-verify and the cached-rebuild paths",
);
assert.match(runtimeScript, /Write-Utf8Atomically -Path \$runtimeDescriptorPath/u);
assert.match(runtimeScript, /Write-Utf8Atomically -Path \$generatedConfigPath/u);
assert.match(
  runtimeScript,
  /Write-Utf8Atomically -Path \$onnxDevDescriptorPath/u,
  "the ONNX dev descriptor is shared-visibility state and must be written atomically",
);
const atomicFnStart = runtimeScript.indexOf("function Write-Utf8Atomically");
const atomicFnEnd = runtimeScript.indexOf("\nfunction ", atomicFnStart);
assert.ok(atomicFnStart !== -1 && atomicFnEnd > atomicFnStart);
const atomicFnBody = runtimeScript.slice(atomicFnStart, atomicFnEnd);
assert.match(
  atomicFnBody,
  /^\s*if \(Test-Path -LiteralPath \$Path -PathType Leaf\) \{$/mu,
  "the atomic writer must branch on an existing destination",
);
assert.match(
  atomicFnBody,
  /^\s*\[System\.IO\.File\]::Replace\(\$temporaryPath, \$Path, \[NullString\]::Value\)$/mu,
  "an existing destination must be swapped in via File.Replace (atomic ReplaceFile) with a real null backup (PowerShell coerces a plain $null argument to an empty string, which Replace would resolve as the backup path), because Move-Item -Force deletes the destination before the rename and the unlocked descriptor readers in scripts/tauri/windows-runtime.js would observe the file missing",
);
assert.match(
  atomicFnBody,
  /^\s*\[System\.IO\.File\]::Move\(\$temporaryPath, \$Path\)$/mu,
  "a fresh destination must still take the plain move path (File.Replace requires an existing destination)",
);
const atomicFnCode = atomicFnBody
  .split("\n")
  .filter((line) => !line.trimStart().startsWith("#"))
  .join("\n");
assert.doesNotMatch(
  atomicFnCode,
  /Move-Item/,
  "no delete-before-move primitive may re-enter the atomic writer",
);
assert.match(
  runtimeCacheBehaviorHarness,
  /foreach \(\$generation in @\("second", "third"\)\)/u,
  "the cache-behavior harness must exercise repeated atomic replacement (the re-stage path), not only first creation",
);
assert.match(
  runtimeCacheBehaviorHarness,
  /atomic replace under a sharing reader must fail loudly/u,
  "the cache-behavior harness must prove a failed atomic replacement preserves the previous descriptor",
);
assert.match(
  runtimeCacheBehaviorHarness,
  /successful flattened runtime expansion \(robocopy \/E\) must leave the process-visible exit code at zero/u,
  "the cache-behavior harness must prove the robocopy /E success arm finishes at $LASTEXITCODE 0 for the wrapper scripts",
);
assert.match(
  runtimeCacheBehaviorHarness,
  /successful long-path cleanup \(robocopy \/MIR\) must leave the process-visible exit code at zero/u,
  "the cache-behavior harness must prove the robocopy /MIR success arm finishes at $LASTEXITCODE 0 for the wrapper scripts",
);
assert.match(
  runtimeCacheBehaviorHarness,
  /Robocopy \/MIR cleanup failed with exit code 8/u,
  "the cache-behavior harness must prove a stubbed robocopy 8 keeps the /MIR cleanup arm fatal",
);
assert.match(
  runtimeCacheBehaviorHarness,
  /Robocopy failed with exit code 8/u,
  "the cache-behavior harness must prove a stubbed robocopy 8 keeps the flattened copy arm fatal",
);

console.log("Windows runtime packaging contract: ok");
