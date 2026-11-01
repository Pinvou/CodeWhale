const assert = require("node:assert/strict");
const crypto = require("node:crypto");
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
