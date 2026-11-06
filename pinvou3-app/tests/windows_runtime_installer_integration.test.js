const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const { stageWindowsInstaller } = require("../scripts/tauri/windows-installer.js");
const { describeWindowsRuntime } = require("../scripts/tauri/windows-runtime.js");

// The body needs a staged private runtime, so bare `node --test` discovery
// skips everywhere (no PR lane stages it). The `--require-staged-runtime`
// escape hatch must be passed via DIRECT invocation (`node tests/…
// --require-staged-runtime`): `node --test` drops unknown argv flags before
// they reach process.argv. The release build-windows-x64 job stages the
// runtime with `npm run runtime:windows:stage` — `runtime:windows:init`
// only materializes the private submodule and neither expands the runtime
// nor writes the descriptor — and then invokes this file directly.
const descriptorPath = path.join(
  __dirname,
  "..",
  "src-tauri",
  "target",
  "windows-runtime",
  "runtime-descriptor.json",
);
const requireStagedRuntime = process.argv.includes("--require-staged-runtime");
if (requireStagedRuntime && !fs.existsSync(descriptorPath)) {
  throw new Error(
    `--require-staged-runtime was passed but no runtime descriptor exists at ${descriptorPath}; ` +
      "run `npm run runtime:windows:stage` before this test",
  );
}
const skipReason = !requireStagedRuntime && (
  process.platform !== "win32" || !fs.existsSync(descriptorPath)
)
  ? "requires a staged Windows private runtime"
  : false;

test("Windows runtime installer adapter integration", { skip: skipReason }, () => {
  assert.equal(process.platform, "win32", "this integration test requires Windows staging");

  const outputRoot = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou-installer-adapter-"));
  try {
    const runtime = describeWindowsRuntime();
    // The descriptor records the staged VC++ payload destination
    // (resolve-runtime.ps1 Get-RuntimeDescriptorContent writes
    // "target/windows-runtime/<stageId>/payload/vc_redist/..." with forward
    // slashes and fails staging if that file is missing).
    // describeWindowsRuntime resolves that entry to an absolute host path,
    // so the separators are backslashes on Windows and are normalized
    // before matching.
    assert.match(
      runtime.vcRedist.sourcePath.replaceAll("\\", "/"),
      /target\/windows-runtime\/[^/]+\/payload\/vc_redist\/VC_redist\.x64\.exe$/u,
      "the runtime descriptor must expose the verified staged VC++ payload",
    );

    const staged = stageWindowsInstaller({
      platform: "win32",
      bundleTargets: ["nsis"],
      runtime,
      destinationRoot: outputRoot,
    });
    assert.equal(staged.vcRedistPath, path.join(outputRoot, "vc_redist", "VC_redist.x64.exe"));
    assert.equal(fs.statSync(staged.vcRedistPath).size, runtime.vcRedist.bytes);
    // Size alone cannot distinguish a truncated-but-padded copy from the real
    // payload; pin the staged copy's content identity to the descriptor's
    // verified SHA-256 (the same fingerprint the source check above enforces).
    const stagedSha256 = crypto
      .createHash("sha256")
      .update(fs.readFileSync(staged.vcRedistPath))
      .digest("hex");
    assert.equal(stagedSha256, runtime.vcRedist.sha256);
  } finally {
    fs.rmSync(outputRoot, { recursive: true, force: true });
  }
});

// Exit-code regression for review 5796830161: robocopy reports success with
// exit codes 0-7, and the resolver used to leak those codes into $LASTEXITCODE
// on the success path. Both wrapper scripts forward $LASTEXITCODE as their
// process exit code (`if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }`), so a
// fully verified staging still exited non-zero and broke the Windows
// dev/build chain in scripts/tauri/windows-runtime.js (spawnSync status !== 0)
// — while the Jenkins release lane ran resolve-runtime.ps1 directly and never
// saw the residue, which is exactly why this must be observed through the
// unmodified wrappers instead of the resolver. Unlike the Jenkins stage step,
// `npm run runtime:windows:stage` never runs the wrappers, so this release-lane
// test is the only automated coverage of the wrapper exit contract.
//
// stage-runtime.ps1 runs on the reuse path (a full -Force rebuild would
// re-expand the whole payload for no extra exit-code signal; the /E copy arm
// is covered at function level by windows_runtime_cache_behavior.ps1).
// stage-onnx-runtime.ps1 -Force carries the rebuild-path coverage: its
// Remove-TreeLongPath arm mirrors /MIR over the existing ONNX stage — the
// exact shape the review reproduced — and stays cheap (a single dylib).
const runtimeScriptsRoot = path.join(
  __dirname,
  "..",
  "src-tauri",
  "packaging",
  "windows",
  "runtime",
  "scripts",
);
const runRuntimeWrapper = (script, args, extraOptions = {}) =>
  spawnSync(
    "powershell.exe",
    ["-NoProfile", "-ExecutionPolicy", "Bypass", "-File", path.join(runtimeScriptsRoot, script), ...args],
    { encoding: "utf8", timeout: 15 * 60 * 1000, ...extraOptions },
  );

test("Windows runtime wrapper scripts exit 0 on the success path", { skip: skipReason }, () => {
  assert.equal(process.platform, "win32", "this wrapper exit-code test requires Windows");
  const stageResult = runRuntimeWrapper("stage-runtime.ps1", []);
  assert.equal(
    stageResult.status,
    0,
    `stage-runtime.ps1 (reuse path) must exit 0: ${stageResult.stderr || stageResult.stdout}`,
  );
  const onnxResult = runRuntimeWrapper("stage-onnx-runtime.ps1", ["-Force"]);
  assert.equal(
    onnxResult.status,
    0,
    `stage-onnx-runtime.ps1 -Force (rebuild path with robocopy /MIR cleanup) must exit 0: ${onnxResult.stderr || onnxResult.stdout}`,
  );
});

test("Windows runtime wrapper scripts keep robocopy >= 8 failures non-zero", { skip: skipReason }, () => {
  assert.equal(process.platform, "win32", "this wrapper exit-code test requires Windows");
  const stubRoot = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou-robocopy-stub-"));
  try {
    // Same PATH-shadowing technique as the Git failure fixtures in
    // windows_runtime_cache_behavior.ps1: robocopy.cmd in a prepended
    // directory resolves ahead of System32's robocopy.exe. The stub fails the
    // rebuild before the old ONNX stage is replaced, so the staged runtime
    // stays intact for the later onnx-smoke step.
    const stubDirectory = path.join(stubRoot, "stub");
    fs.mkdirSync(stubDirectory);
    fs.writeFileSync(path.join(stubDirectory, "robocopy.cmd"), "@echo off\r\nexit /b 8\r\n", "utf8");
    const result = runRuntimeWrapper("stage-onnx-runtime.ps1", ["-Force"], {
      env: { ...process.env, PATH: `${stubDirectory};${process.env.PATH}` },
    });
    assert.equal(result.error, undefined);
    assert.notEqual(
      result.status,
      0,
      "a robocopy exit code of 8 must keep the wrapper process exit code non-zero",
    );
    assert.match(
      String(result.stderr ?? ""),
      /Robocopy \/MIR cleanup failed with exit code 8/u,
      "the wrapper failure must surface the real robocopy error, not a swallowed cleanup",
    );
  } finally {
    fs.rmSync(stubRoot, { recursive: true, force: true });
  }
});
