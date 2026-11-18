const assert = require("node:assert/strict");
const { spawn, spawnSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const {
  BRIDGE_ENTRYPOINT,
  CLAUDE_BRIDGE_ENTRYPOINT,
  expectedMarker,
  prepareWindowsCodexBridge,
  WINDOWS_BRIDGE_CONFIG_PATH,
  WINDOWS_BRIDGE_ROOT,
} = require("../scripts/tauri/codex-bridge.js");
const {
  buildResourceManifest,
  composeEffectiveConfig,
} = require("../scripts/tauri/effective-config.js");
const { platformConfigPath } = require("../scripts/tauri/platform-config.js");
const {
  WINDOWS_RUNTIME_CONFIG_PATH,
  describeWindowsRuntime,
} = require("../scripts/tauri/windows-runtime.js");

assert.equal(process.platform, "win32", "此冒烟测试必须在 Windows 原生 runner 执行");
assert.equal(process.arch, "x64", "Windows Codex Runtime 当前只支持 x64");

const stagedRuntime = fs.existsSync(WINDOWS_RUNTIME_CONFIG_PATH)
  ? describeWindowsRuntime()
  : null;
const nodeExecutable = stagedRuntime?.nodeExecutable ?? process.execPath;
prepareWindowsCodexBridge({
  nodeExecutable,
  npmExecPath: stagedRuntime?.npmExecPath,
});

const marker = expectedMarker({ architecture: "x64" });
const sourcePackage = JSON.parse(
  fs.readFileSync(
    path.join(__dirname, "..", "scripts", "codex-bridge-runtime", "package.json"),
    "utf8",
  ),
);
const bridgeVersion = spawnSync(
  nodeExecutable,
  [path.join(WINDOWS_BRIDGE_ROOT, BRIDGE_ENTRYPOINT), "--version"],
  {
    encoding: "utf8",
    env: { ...process.env, CODEX_PATH: "" },
  },
);
assert.equal(bridgeVersion.error, undefined);
assert.equal(bridgeVersion.status, 0, bridgeVersion.stderr);
assert.equal(
  bridgeVersion.stdout.trim(),
  `@agentclientprotocol/codex-acp ${sourcePackage.dependencies["@agentclientprotocol/codex-acp"]}`,
);

// Claude Code 走系统安装（与 Codex/Kimi 一致），Bridge 不得携带 claude 平台原生二进制。
const anthropicScope = path.join(
  WINDOWS_BRIDGE_ROOT,
  "acp",
  "node_modules",
  "@anthropic-ai",
);
const leftoverClaudePackages = fs
  .readdirSync(anthropicScope)
  .filter((entry) => entry.startsWith("claude-agent-sdk-"));
assert.deepEqual(
  leftoverClaudePackages,
  [],
  `Bridge 中不得残留 Claude 平台二进制: ${leftoverClaudePackages.join(", ")}`,
);

const { effectiveConfig } = composeEffectiveConfig([
  platformConfigPath("win32"),
  ...(stagedRuntime ? [stagedRuntime.configPath] : []),
  WINDOWS_BRIDGE_CONFIG_PATH,
]);
const manifest = buildResourceManifest(effectiveConfig, { platform: "win32" });
const destinations = new Set(manifest.files.map((file) => file.destination));
assert.ok(
  destinations.has(
    `runtime/codex-bridge/${BRIDGE_ENTRYPOINT.replaceAll(path.sep, "/")}`,
  ),
);
assert.ok(destinations.has("runtime/codex-bridge/manifest.json"));
if (stagedRuntime) {
  assert.ok(destinations.has("runtime/node/node.exe"));
  assert.ok(destinations.has("runtime/node/LICENSE"));
} else {
  assert.ok(
    [...destinations].every((destination) => !destination.startsWith("runtime/node/")),
    "Codex Bridge overlay must not package a second Node runtime",
  );
}

function initializeClaudeBridge() {
  return new Promise((resolve, reject) => {
    const started = process.hrtime.bigint();
    const elapsed = () => Number(process.hrtime.bigint() - started) / 1e6;
    const entrypoint = path.join(WINDOWS_BRIDGE_ROOT, CLAUDE_BRIDGE_ENTRYPOINT);
    const child = spawn(nodeExecutable, [
      "--require", path.join(__dirname, "helpers", "claude-acp-startup-probe.cjs"), entrypoint,
    ], {
      cwd: WINDOWS_BRIDGE_ROOT,
      env: process.env,
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
    });
    let settled = false;
    let stdout = "";
    let stderr = "";
    let stdoutBytes = 0;
    let stderrBytes = 0;
    let spawnMs = null;
    let firstStdoutMs = null;
    const timer = setTimeout(() => {
      finish(new Error("Claude ACP initialize 超时（15秒；启动阶段见 acp-startup）"));
    }, 15_000);

    function finish(error) {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      // Only metadata emitted by the observer is retained; arbitrary upstream
      // stderr may contain managed settings and must not enter CI diagnostics.
      const phases = stderr.split(/\r?\n/u).filter((line) => line.startsWith("[acp-startup] "));
      for (const line of phases.slice(0, 48)) {
        try {
          const source = JSON.parse(line.slice("[acp-startup] ".length));
          const metadata = {};
          if (!/^(?:preload|load-start|load-end|registry-start|registry-close|stdin-data-listener|heartbeat)$/u.test(source.phase)) continue;
          metadata.phase = source.phase;
          if (["sdk", "managed-policy", "entry"].includes(source.label)) metadata.label = source.label;
          for (const key of ["elapsed_ms", "cpu_ms", "rss_mib", "duration_ms", "code"]) {
            if (typeof source[key] === "number" && Number.isFinite(source[key])) metadata[key] = source[key];
          }
          console.log(`[acp-startup] ${JSON.stringify(metadata)}`);
        } catch { /* Partial or unrecognized stderr is not diagnostic metadata. */ }
      }
      console.log(`[acp-startup-summary] ${JSON.stringify({
        elapsed_ms: elapsed(), spawn_ms: spawnMs, first_stdout_ms: firstStdoutMs,
        stdout_bytes: stdoutBytes, stderr_bytes: stderrBytes, success: !error,
      })}`);
      child.kill();
      if (error) reject(error);
      else resolve();
    }

    child.on("error", finish);
    child.once("spawn", () => { spawnMs = elapsed(); });
    child.on("exit", (code) => {
      if (!settled) {
        finish(new Error(`Claude ACP initialize 前退出: code=${code}（启动阶段见 acp-startup）`));
      }
    });
    child.stderr.on("data", (chunk) => {
      stderrBytes += chunk.length;
      if (stderr.length < 64 * 1024) stderr += chunk.toString().slice(0, 64 * 1024 - stderr.length);
    });
    child.stdout.on("data", (chunk) => {
      stdoutBytes += chunk.length;
      firstStdoutMs ??= elapsed();
      if (stdoutBytes > 1024 * 1024) {
        finish(new Error("Claude ACP initialize 响应超过1MiB上限"));
        return;
      }
      stdout += chunk.toString();
      const lines = stdout.split(/\r?\n/u);
      stdout = lines.pop() || "";
      for (const line of lines) {
        if (!line.trim()) continue;
        let response;
        try {
          response = JSON.parse(line);
        } catch {
          continue;
        }
        if (response.id !== 1) continue;
        if (response.error) {
          const code = Number.isSafeInteger(response.error.code) ? response.error.code : "unknown";
          finish(new Error(`Claude ACP initialize 失败: code=${code}`));
          return;
        }
        try {
          assert.equal(response.result.protocolVersion, 1);
          assert.equal(typeof response.result.agentInfo?.name, "string");
        } catch (error) {
          finish(error);
          return;
        }
        finish();
        return;
      }
    });
    child.stdin.on("error", () => {});
    child.stdin.write(
      `${JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "initialize",
        params: {
          protocolVersion: 1,
          clientCapabilities: {},
          clientInfo: { name: "pinvou3-windows-smoke", version: "1.0.0" },
        },
      })}\n`,
    );
  });
}

assert.equal(marker.platform, "win32");
initializeClaudeBridge()
  .then(() => {
    console.log("Windows Codex + Claude ACP Bridge existing-Node runtime: ok");
  })
  // eslint-disable-next-line unicorn/prefer-top-level-await -- smoke script keeps its existing async main() structure
  .catch((error) => {
    console.error(error);
    process.exitCode = 1;
  });
