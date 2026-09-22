const assert = require("node:assert/strict");
const { EventEmitter } = require("node:events");
const crypto = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const {
  BASE_CONFIG_PATH,
  buildResourceManifest,
  composeEffectiveConfig,
  mergeConfig,
} = require("../scripts/tauri/effective-config.js");
const {
  configureIsolatedWindowsRustToolchain,
  chromeDevtoolsMcpEnvironment,
  configSpecs,
  ensureWindowsRustToolchain,
  prepareCodexBridge,
  prepareChromeDevtoolsMcpForPlatform,
  prepareWindowsRustcStackWrapper,
  prepareWindowsCodexBridge,
  prepareTauriArgs,
  runTauri,
  tauriRuntimeEnvironment,
  tauriCommandIndex,
  usesValidatedNsisStaging,
  supportsChromeDevtoolsMcp,
} = require("../scripts/tauri/build.js");
const {
  APP_ROOT,
  platformArchitectureConfigPath,
  platformConfigPath,
} = require("../scripts/tauri/platform-config.js");
const {
  linuxStartupWindowConfig,
  linuxStartupWindowConfigSpec,
} = require("../scripts/tauri/startup-window-config.js");
const { requireWrapper, WRAPPER_ENV } = require("../scripts/tauri/require-wrapper.js");
const { WINDOWS_BRIDGE_CONFIG_PATH } = require("../scripts/tauri/codex-bridge.js");
const {
  ADAPTED_RESPONSE_SHA256,
  ADAPTER_VERSION,
  GITKEEP,
  applyTargetIdAdapter,
  assertTargetIdAdapterIntegrity,
  expectedMarker,
  isPreparedRoot,
} = require("../scripts/tauri/chrome-devtools-mcp.js");

assert.equal(ADAPTER_VERSION, "pinvou-target-id-v1");
assert.equal(
  ADAPTED_RESPONSE_SHA256,
  "e08698ba25c72b304152da1de99005d2415b9034c7edd46615d942dac174e0a6",
);
const thirdPartyNotices = fs.readFileSync(
  path.join(__dirname, "..", "..", "THIRD_PARTY_NOTICES.md"),
  "utf8",
);
const chromeDevtoolsMcpNotice = thirdPartyNotices.match(
  /- chrome-devtools-mcp: Modified by Pinvou Agent during vendoring:[\s\S]*?(?=\n- |\n\n)/,
)?.[0];
assert.ok(chromeDevtoolsMcpNotice, "the chrome-devtools-mcp adapter must be disclosed");
for (const requiredNoticeText of [
  "build/src/McpResponse.js",
  "target_id",
  "conversation and tab ownership",
  "SHA-256",
]) {
  assert.ok(
    chromeDevtoolsMcpNotice.includes(requiredNoticeText),
    `the chrome-devtools-mcp notice must include ${requiredNoticeText}`,
  );
}
assert.deepEqual(
  fs.readFileSync(
    path.join(
      __dirname,
      "..",
      "src-tauri",
      "resources",
      "platforms",
      "windows",
      "chrome-devtools-mcp",
      ".gitkeep",
    ),
  ),
  Buffer.from(GITKEEP, "utf8"),
  "the tracked placeholder must remain byte-for-byte identical to the vendor rewrite",
);
{
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou-cdmcp-adapter-"));
  const sourceDir = path.join(root, "build", "src");
  const responsePath = path.join(sourceDir, "McpResponse.js");
  fs.mkdirSync(sourceDir, { recursive: true });
  const originalSource = [
    "function createStructuredPage(mcpPage) {",
    "    const entry = {",
    "        id: mcpPage.id,",
    "        url: mcpPage.pptrPage.url(),",
    "    };",
    "}",
  ].join("\n");
  const adaptedSource = originalSource.replace(
    "        id: mcpPage.id,\n        url: mcpPage.pptrPage.url(),",
    "        id: mcpPage.id,\n        target_id: mcpPage.pptrPage.target()._targetId,\n        url: mcpPage.pptrPage.url(),",
  );
  const fixtureSha256 = crypto.createHash("sha256").update(adaptedSource).digest("hex");
  fs.writeFileSync(responsePath, originalSource);
  applyTargetIdAdapter(root, { expectedSha256: fixtureSha256 });
  applyTargetIdAdapter(root, { expectedSha256: fixtureSha256 }); // idempotent rebuild
  const adapted = fs.readFileSync(responsePath, "utf8");
  assert.equal(adapted, adaptedSource);
  assert.equal(
    adapted.split("target_id: mcpPage.pptrPage.target()._targetId").length - 1,
    1,
  );
  assert.equal(
    assertTargetIdAdapterIntegrity(root, { expectedSha256: fixtureSha256 }),
    fixtureSha256,
  );

  const entry = path.join(sourceDir, "bin", "chrome-devtools-mcp.js");
  fs.mkdirSync(path.dirname(entry), { recursive: true });
  fs.writeFileSync(entry, "// fixture");
  fs.writeFileSync(path.join(root, "catalog-shim.json"), "{}");
  const fixtureMarker = expectedMarker(fixtureSha256);
  fs.writeFileSync(
    path.join(root, ".vendor-version.json"),
    JSON.stringify(fixtureMarker, null, 2),
  );
  assert.equal(isPreparedRoot(root, fixtureMarker), true);

  fs.appendFileSync(responsePath, "\n// unexpected mutation");
  assert.throws(
    () => assertTargetIdAdapterIntegrity(root, { expectedSha256: fixtureSha256 }),
    /SHA-256 mismatch/,
  );
  assert.equal(isPreparedRoot(root, fixtureMarker), false);

  fs.writeFileSync(responsePath, adaptedSource);
  fs.writeFileSync(
    path.join(root, ".vendor-version.json"),
    JSON.stringify({ ...fixtureMarker, responseSha256: "0".repeat(64) }, null, 2),
  );
  assert.equal(isPreparedRoot(root, fixtureMarker), false);

  fs.writeFileSync(responsePath, "// upstream drift");
  assert.throws(
    () => applyTargetIdAdapter(root, { expectedSha256: fixtureSha256 }),
    /adapter anchor state/,
  );
  fs.rmSync(root, { recursive: true, force: true });
}

const noRuntime = () => null;

const isolatedRustEnv = {};
const isolatedRust = configureIsolatedWindowsRustToolchain({
  env: isolatedRustEnv,
  architecture: "x64",
});
assert.equal(isolatedRust.channel, "1.98.0");
assert.equal(isolatedRust.hostTriple, "x86_64-pc-windows-msvc");
assert.equal(isolatedRustEnv.RUSTUP_HOME, isolatedRust.rustupHome);
assert.equal(isolatedRustEnv.PINVOU3_MANAGED_RUSTUP, "1");
assert.match(
  isolatedRust.rustupHome.replaceAll("\\", "/"),
  /\/\.cache\/rustup\/1\.98\.0-x86_64-pc-windows-msvc$/u,
);

const customRustEnv = { PINVOU3_RUSTUP_HOME: "custom-rustup" };
const customRust = configureIsolatedWindowsRustToolchain({
  env: customRustEnv,
  architecture: "arm64",
});
assert.equal(customRust.hostTriple, "aarch64-pc-windows-msvc");
assert.equal(customRust.rustupHome, path.join(APP_ROOT, "custom-rustup"));

function scriptedRustToolchainSpawn(exitCodes, invocations) {
  return (command, args, options) => {
    invocations.push({ command, args, options });
    const child = new EventEmitter();
    const exitCode = exitCodes.shift();
    queueMicrotask(() => child.emit("exit", exitCode, null));
    return child;
  };
}

const accountRustInvocations = [];
const accountRustEnv = {};
const accountRustCheck = ensureWindowsRustToolchain({
  env: accountRustEnv,
  architecture: "x64",
  log: () => {},
  spawnChild: scriptedRustToolchainSpawn([0], accountRustInvocations),
}).then((result) => {
  assert.equal(result.source, "account");
  assert.equal(accountRustInvocations.length, 1);
  assert.equal(accountRustInvocations[0].command, "powershell.exe");
  assert.equal(accountRustInvocations[0].args.at(-1), "-CheckOnly");
  assert.equal(accountRustInvocations[0].options.env.PINVOU3_MANAGED_RUSTUP, undefined);
  assert.equal(accountRustEnv.RUSTUP_HOME, undefined);
  assert.equal(accountRustEnv.RUSTUP_TOOLCHAIN, "1.98.0");
});

const isolatedRustInvocations = [];
const fallbackRustEnv = {};
const fallbackRustCheck = ensureWindowsRustToolchain({
  env: fallbackRustEnv,
  architecture: "x64",
  log: () => {},
  spawnChild: scriptedRustToolchainSpawn([2, 0], isolatedRustInvocations),
}).then((result) => {
  assert.equal(result.source, "isolated");
  assert.equal(isolatedRustInvocations.length, 2);
  assert.equal(isolatedRustInvocations[0].args.at(-1), "-CheckOnly");
  assert.equal(isolatedRustInvocations[1].options.env.PINVOU3_MANAGED_RUSTUP, "1");
  assert.match(
    isolatedRustInvocations[1].options.env.RUSTUP_HOME.replaceAll("\\", "/"),
    /\/\.cache\/rustup\/1\.98\.0-x86_64-pc-windows-msvc$/u,
  );
});

Promise.all([accountRustCheck, fallbackRustCheck]).catch((error) => {
  console.error(error);
  process.exitCode = 1;
});

{
  const scriptsPath = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou-rustc-stack-wrapper-"));
  const sourcePath = path.join(scriptsPath, "rustc-stack-wrapper.rs");
  const wrapperPath = path.join(scriptsPath, "rustc-stack-wrapper.exe");
  fs.writeFileSync(sourcePath, "fn main() {}\n");

  const compileInvocations = [];
  const environment = { RUSTUP_TOOLCHAIN: "fixture-toolchain" };
  const prepared = prepareWindowsRustcStackWrapper({
    environment,
    platform: "win32",
    scriptsPath,
    log: () => {},
    spawnCompiler: (command, args, options) => {
      compileInvocations.push({ command, args, options });
      fs.writeFileSync(wrapperPath, "fixture executable");
      return { status: 0 };
    },
  });
  assert.deepEqual(prepared, { path: wrapperPath, source: "compiled" });
  assert.equal(environment.RUSTC_WRAPPER, wrapperPath);
  assert.equal(compileInvocations.length, 1);
  assert.equal(compileInvocations[0].command, "rustc");
  assert.deepEqual(compileInvocations[0].args, ["-O", sourcePath, "-o", wrapperPath]);
  assert.equal(compileInvocations[0].options.env.RUSTUP_TOOLCHAIN, "fixture-toolchain");
  assert.equal(compileInvocations[0].options.windowsHide, true);

  const cachedEnvironment = {};
  const cached = prepareWindowsRustcStackWrapper({
    environment: cachedEnvironment,
    platform: "win32",
    scriptsPath,
    log: () => {},
    spawnCompiler: () => {
      throw new Error("a fresh wrapper must not be rebuilt");
    },
  });
  assert.deepEqual(cached, { path: wrapperPath, source: "cached" });
  assert.equal(cachedEnvironment.RUSTC_WRAPPER, wrapperPath);

  const configuredEnvironment = { RUSTC_WRAPPER: "  C:\\custom\\rustc-wrapper.exe  " };
  const configured = prepareWindowsRustcStackWrapper({
    environment: configuredEnvironment,
    platform: "win32",
    scriptsPath,
    log: () => {},
    spawnCompiler: () => {
      throw new Error("an explicit wrapper must not be rebuilt");
    },
  });
  assert.deepEqual(configured, {
    path: "C:\\custom\\rustc-wrapper.exe",
    source: "environment",
  });
  assert.equal(configuredEnvironment.RUSTC_WRAPPER, "C:\\custom\\rustc-wrapper.exe");
  assert.equal(
    prepareWindowsRustcStackWrapper({
      environment: {},
      platform: "linux",
      scriptsPath,
      log: () => {},
    }),
    null,
  );
  fs.rmSync(wrapperPath);
  assert.throws(
    () => prepareWindowsRustcStackWrapper({
      environment: {},
      platform: "win32",
      scriptsPath,
      log: () => {},
      spawnCompiler: () => ({ status: 1 }),
    }),
    /Failed to compile Windows rustc stack wrapper \(exit 1\)/,
    "a failed wrapper build must stop before Cargo can fall back to the overflowing compiler stack",
  );
  fs.rmSync(scriptsPath, { recursive: true, force: true });
}

let preparedBridge = null;
prepareCodexBridge({
  platform: "linux",
  spawn: (command, args, options) => {
    preparedBridge = { command, args, options };
    return { status: 0 };
  },
});
assert.match(preparedBridge.command, /prepare-codex-bridge-runtime\.sh$/);
assert.deepEqual(preparedBridge.args, []);
preparedBridge = null;
prepareCodexBridge({
  platform: "darwin",
  spawn: (command, args, options) => {
    preparedBridge = { command, args, options };
    return { status: 0 };
  },
});
assert.match(preparedBridge.command, /prepare-codex-bridge-runtime\.sh$/);
assert.deepEqual(preparedBridge.args, []);
preparedBridge = null;
prepareCodexBridge({
  platform: "win32",
  spawn: () => {
    throw new Error("Windows 不应准备 Codex Bridge");
  },
});
assert.equal(preparedBridge, null);
assert.equal(
  prepareWindowsCodexBridge({ platform: "linux" }),
  false,
  "Linux 不应准备 Windows Codex Bridge",
);

// ---- 交叉构建：targetArch -> PINVOU3_BRIDGE_TARGET_ARCH 的映射与注入 ----
//
// 这条链是交叉构建备料的核心机制：build.js 从 `--target` 解析出 node 记法的
// targetArch，codex-bridge.js 把它映射成 machine 名注入环境变量，备料脚本按这个
// 词挑 Node 官方发行包。映射写反或丢键都不会让构建失败，只会**静默**备错架构的
// 料，所以这里必须逐项钉死。
function capturedBridgeSpawn(options) {
  let captured = null;
  prepareCodexBridge({
    ...options,
    spawn: (command, args, spawnOptions) => {
      captured = { command, args, options: spawnOptions };
      return { status: 0 };
    },
  });
  return captured;
}

const bridgeEnvFixture = { PINVOU_TEST_ENV: "kept" };
const hostBridgeSpawn = capturedBridgeSpawn({
  platform: "linux",
  env: bridgeEnvFixture,
});
assert.equal(
  hostBridgeSpawn.options.env,
  bridgeEnvFixture,
  "非交叉构建必须原样透传环境，让备料脚本回落到 uname -m",
);
assert.equal(
  hostBridgeSpawn.options.env.PINVOU3_BRIDGE_TARGET_ARCH,
  undefined,
  "不传 targetArch 时不能注入目标架构",
);

const injectedBridgeMachines = new Set();
for (const [targetArch, machine] of [
  ["arm64", "aarch64"],
  ["x64", "x86_64"],
]) {
  for (const platform of ["linux", "darwin"]) {
    const crossBridgeSpawn = capturedBridgeSpawn({
      platform,
      targetArch,
      env: bridgeEnvFixture,
    });
    assert.equal(
      crossBridgeSpawn.options.env.PINVOU3_BRIDGE_TARGET_ARCH,
      machine,
      `${platform} 上交叉到 ${targetArch} 必须注入 ${machine}`,
    );
    assert.equal(
      crossBridgeSpawn.options.env.PINVOU_TEST_ENV,
      "kept",
      "注入目标架构不能把其余环境变量丢掉",
    );
    injectedBridgeMachines.add(crossBridgeSpawn.options.env.PINVOU3_BRIDGE_TARGET_ARCH);
  }
}
assert.equal(
  bridgeEnvFixture.PINVOU3_BRIDGE_TARGET_ARCH,
  undefined,
  "注入必须复制一份环境，不能就地污染调用方的 env",
);
assert.equal(
  capturedBridgeSpawn({
    platform: "linux",
    targetArch: "riscv64",
    env: bridgeEnvFixture,
  }).options.env,
  bridgeEnvFixture,
  "映射表里没有的记法只能回落到宿主行为，不能注入一个备料脚本不认识的词",
);

// 注入的词必须落在备料脚本的目标词表里，两个 OS 都要。
//
// 这是 Darwin 别名那条回归的钉子：三元组里 macOS 的 arm64 写作 aarch64
//（aarch64-apple-darwin），而 macOS 的 `uname -m` 返回 arm64。脚本只收
// `Darwin-arm64` 时，darwin 宿主上手工单架构构建会落到 `*)` 硬失败——CI 只用
// universal-apple-darwin（不注入），所以这个洞当时没被任何用例照到。
const bridgeScriptSource = fs.readFileSync(
  path.join(APP_ROOT, "scripts", "prepare-codex-bridge-runtime.sh"),
  "utf8",
);
const bridgeTargetDispatch = bridgeScriptSource.match(
  /case "\$OS_NAME-\$TARGET_MACHINE" in([\s\S]*?)\nesac/u,
)?.[1];
assert.ok(
  bridgeTargetDispatch,
  "备料脚本必须保留按 $OS_NAME-$TARGET_MACHINE 分派的目标词表",
);
const acceptedBridgeTargets = new Set(
  (bridgeTargetDispatch.match(/^[ \t]*[A-Za-z0-9_|-]+\)/gmu) ?? []).flatMap((label) =>
    label.trim().slice(0, -1).split("|"),
  ),
);
for (const machine of injectedBridgeMachines) {
  for (const osName of ["Linux", "Darwin"]) {
    assert.ok(
      acceptedBridgeTargets.has(`${osName}-${machine}`),
      `备料脚本的目标词表必须认得 ${osName}-${machine}，否则交叉构建会落到 *) 硬失败`,
    );
  }
}

assert.throws(() => requireWrapper({}), /禁止绕过平台 overlay/);
assert.doesNotThrow(() => requireWrapper({ [WRAPPER_ENV]: "1" }));

const linuxStartupOverlay = linuxStartupWindowConfigSpec();
const buildArgs = prepareTauriArgs(
  ["--verbose", "build", "--bundles", "deb"],
  { platform: "linux", stageRuntime: noRuntime },
);
assert.equal(tauriCommandIndex(buildArgs), 1, "build command may follow global options");
assert.deepEqual(configSpecs(buildArgs), [
  platformConfigPath("linux"),
  linuxStartupOverlay,
  platformArchitectureConfigPath("linux", "x64"),
]);
const linuxArmArgs = prepareTauriArgs(
  ["build", "--bundles", "deb"],
  { platform: "linux", architecture: "arm64", stageRuntime: noRuntime },
);
assert.deepEqual(configSpecs(linuxArmArgs), [
  platformConfigPath("linux"),
  linuxStartupOverlay,
  platformArchitectureConfigPath("linux", "arm64"),
]);

const explicitOverlay = "src-tauri/config/platforms/windows/signing.wosign.conf.json";
const windowsRuntimeOverlay = "target/windows-runtime/tauri.generated.conf.json";
const bundleArgs = prepareTauriArgs(
  ["bundle", "-c", explicitOverlay],
  { platform: "win32", stageRuntime: () => "runtime.generated.json" },
);
assert.deepEqual(configSpecs(bundleArgs), [
  platformConfigPath("win32"),
  "runtime.generated.json",
  explicitOverlay,
]);
const windowsDescriptorArgs = prepareTauriArgs(
  ["bundle", "-c", explicitOverlay],
  {
    platform: "win32",
    stageRuntime: () => ({ configPath: windowsRuntimeOverlay }),
  },
);
assert.deepEqual(configSpecs(windowsDescriptorArgs), [
  platformConfigPath("win32"),
  windowsRuntimeOverlay,
  explicitOverlay,
]);
const validatedNsisOverlay =
  "src-tauri/target/release/nsis-stage/tauri.nsis-stage.conf.json";
let stagedRuntimeCallCount = 0;
const validatedNsisArgs = prepareTauriArgs(
  ["bundle", "--config", validatedNsisOverlay, "--config", explicitOverlay],
  {
    platform: "win32",
    stageRuntime: () => {
      stagedRuntimeCallCount += 1;
      return windowsRuntimeOverlay;
    },
  },
);
assert.equal(usesValidatedNsisStaging(validatedNsisArgs), true);
assert.equal(stagedRuntimeCallCount, 0);
assert.deepEqual(configSpecs(validatedNsisArgs), [
  platformConfigPath("win32"),
  validatedNsisOverlay,
  explicitOverlay,
]);
assert.equal(
  usesValidatedNsisStaging(["build", "--config", validatedNsisOverlay]),
  false,
  "build must remain responsible for runtime validation and staging",
);
const windowsCodexArgs = prepareTauriArgs(
  ["build", "-c", explicitOverlay],
  {
    platform: "win32",
    // 这里只验证 Codex Bridge overlay 顺序；私有 runtime staging 由上一个用例
    // 和独立 Windows 发布契约覆盖，不能让通用 Windows Rust CI 依赖私有仓凭据。
    stageRuntime: noRuntime,
    additionalConfigs: [WINDOWS_BRIDGE_CONFIG_PATH],
  },
);
assert.deepEqual(configSpecs(windowsCodexArgs), [
  platformConfigPath("win32"),
  WINDOWS_BRIDGE_CONFIG_PATH,
  explicitOverlay,
]);
assert.deepEqual(
  prepareTauriArgs(["dev"], { platform: "linux", stageRuntime: noRuntime }),
  ["dev", "--config", linuxStartupOverlay],
  "Linux dev must hide the main window until the first React commit",
);
assert.deepEqual(
  prepareTauriArgs(["dev"], { platform: "win32", stageRuntime: noRuntime }),
  ["dev"],
  "Windows dev must not receive packaging overlays",
);
assert.deepEqual(
  configSpecs(prepareTauriArgs(["dev", "-c", explicitOverlay], { platform: "linux", stageRuntime: noRuntime })),
  [linuxStartupOverlay, explicitOverlay],
  "explicit Linux dev overlays must override the automatic cold-start overlay",
);
const linuxKnowledgeHostDevOverlay = JSON.stringify({
  bundle: { resources: { "target/knowledge-host-dev/": "runtime/knowledge-host" } },
});
assert.deepEqual(
  configSpecs(prepareTauriArgs(["dev", "-c", explicitOverlay], {
    platform: "linux",
    additionalConfigs: [linuxKnowledgeHostDevOverlay],
  })),
  [linuxStartupOverlay, linuxKnowledgeHostDevOverlay, explicitOverlay],
  "Linux dev host resources must be injected before caller overrides",
);
assert.deepEqual(
  prepareTauriArgs(["dev"], { platform: "darwin" }),
  ["dev", "--config", platformConfigPath("darwin")],
  "macOS dev must receive the platform overlay (native titlebar) to match packaged output",
);
assert.deepEqual(
  prepareTauriArgs(["dev", "--features", "browser-macos-preview"], { platform: "darwin" }),
  [
    "dev",
    "--config",
    platformConfigPath("darwin"),
    "--features",
    "browser-macos-preview",
  ],
  "the isolated macOS BrowserCore preview feature must reach the Tauri Cargo build unchanged",
);
assert.deepEqual(
  prepareTauriArgs(
    ["build", "--features", "browser-macos-preview", "--target", "universal-apple-darwin"],
    { platform: "darwin" },
  ),
  [
    "build",
    "--config",
    platformConfigPath("darwin"),
    "--features",
    "browser-macos-preview",
    "--target",
    "universal-apple-darwin",
  ],
  "preview packaging must remain an explicit opt-in instead of changing normal macOS builds",
);
assert.deepEqual(
  configSpecs(prepareTauriArgs(["dev", "-c", explicitOverlay], { platform: "darwin" })),
  [platformConfigPath("darwin"), explicitOverlay],
  "explicit macOS dev overlays must override the automatic platform overlay",
);
const buildSource = fs.readFileSync(
  path.join(__dirname, "..", "scripts", "tauri", "build.js"),
  "utf8",
);
assert.match(
  buildSource,
  /if \(isDev\)[\s\S]*?prepareWindowsCodexBridge\(\)/,
  "Windows dev must prepare the ACP Bridge without packaging overlays",
);
assert.match(
  buildSource,
  /prepareWindowsRustcStackWrapper\(\{ environment: tauriEnvironment \}\);\s*return runTauri\(preparedArgs, \{ environment: tauriEnvironment \}\);/u,
  "the direct npm/Tauri entry must inject the Windows compiler stack wrapper",
);
// 这条要钉的是**顺序**：备料必须发生在清单生成之前，否则 bundle 会以
// 「安装包资源不存在」失败。
//
// 原先它是逐字匹配那一行的写法（`if (process.env.PINVOU3_SKIP_LINUX_ASR !== "1")
// prepareLinuxAsrRuntime();`）。那样任何等价重构都会把它弄红，而真正要守的顺序
// 反倒没被检查——把开关读取上移成局部常量就是这么撞上的。改成按位置比较。
assert.ok(
  buildSource.includes("prepareLinuxAsrRuntime()"),
  "Linux packaging must still prepare the SenseVoice runtime",
);
assert.ok(
  buildSource.indexOf("prepareLinuxAsrRuntime()") < buildSource.indexOf("writeEffectiveArtifacts("),
  "Linux packaging must prepare the architecture-specific SenseVoice runtime before manifest generation",
);
// 交叉构建要能显式跳过 ASR（SenseVoice 只能在目标架构上原生构建）。
assert.match(buildSource, /skipLinuxAsr/u, "the ASR step must stay skippable for cross builds");
// 上面那组用例只证明「给了 targetArch 就会注入正确的词」；这条补上另一头的接线：
// 解析出来的目标架构确实被交给了 Bridge 备料。断得松（只要求同一次调用里出现
// targetArch），等价重构不会误伤，而整条链断掉会红。
assert.match(
  buildSource,
  /prepareCodexBridge\([^)]*targetArch/u,
  "Bridge 必须按解析出的目标架构备料，否则交叉构建会静默备一份宿主架构的料",
);
const preparedBrowserPlatforms = [];
for (const platform of ["win32", "darwin", "linux"]) {
  const result = prepareChromeDevtoolsMcpForPlatform({
    platform,
    prepare: (options) => {
      preparedBrowserPlatforms.push(options.platform);
      return "prepared";
    },
  });
  assert.equal(
    result,
    platform === "win32" ? "prepared" : false,
    `${platform} must follow its declared Chrome MCP packaging capability`,
  );
  assert.equal(supportsChromeDevtoolsMcp(platform), platform === "win32");
}
assert.deepEqual(
  preparedBrowserPlatforms,
  ["win32"],
  "only the Windows WebView2 backend may prepare chrome-devtools-mcp",
);
const browserDevEnvironment = chromeDevtoolsMcpEnvironment(
  true,
  { PINVOU_TEST_ENV: "kept" },
  "win32",
);
assert.equal(browserDevEnvironment.PINVOU_TEST_ENV, "kept");
assert.match(
  browserDevEnvironment.PINVOU3_CDMCP_BIN,
  /resources[\\/]platforms[\\/]windows[\\/]chrome-devtools-mcp[\\/]build[\\/]src[\\/]bin[\\/]chrome-devtools-mcp\.js$/,
);
for (const platform of ["darwin", "linux"]) {
  const displayOnlyEnvironment = chromeDevtoolsMcpEnvironment(
    true,
    { PINVOU_TEST_ENV: "kept", PINVOU3_CDMCP_BIN: "stale-external-entry" },
    platform,
  );
  assert.deepEqual(
    displayOnlyEnvironment,
    { PINVOU_TEST_ENV: "kept" },
    `${platform} inactive browser substrate must neither inject nor inherit a Chrome MCP entry`,
  );
}
assert.deepEqual(
  chromeDevtoolsMcpEnvironment(
    false,
    { PINVOU_TEST_ENV: "kept", PINVOU3_CDMCP_BIN: "stale-external-entry" },
    "win32",
  ),
  { PINVOU_TEST_ENV: "kept" },
  "packaged Windows builds must resolve chrome-devtools-mcp only from the app resource directory",
);
let tauriInvocation = null;
const tauriLogs = [];
const fakeTauriChild = new EventEmitter();
fakeTauriChild.pid = 4242;
let currentTime = 0;
let heartbeatCallback = null;
let heartbeatCleared = false;
const tauriResult = runTauri(["build"], {
  environment: { PINVOU_TEST_ENV: "kept" },
  spawnChild: (command, args, options) => {
    tauriInvocation = { command, args, options };
    return fakeTauriChild;
  },
  heartbeatIntervalMs: 60_000,
  now: () => currentTime,
  log: (message) => tauriLogs.push(message),
  setIntervalFn: (callback, interval) => {
    assert.equal(interval, 60_000);
    heartbeatCallback = callback;
    return { unref() {} };
  },
  clearIntervalFn: () => {
    heartbeatCleared = true;
  },
});
assert.equal(tauriInvocation.command, process.execPath);
assert.match(tauriInvocation.args[0], /@tauri-apps[\\/]cli[\\/]tauri\.js$/);
assert.equal(tauriInvocation.args[1], "build");
assert.equal(tauriInvocation.options.env[WRAPPER_ENV], "1");
assert.equal(tauriInvocation.options.env.PINVOU_TEST_ENV, "kept");
const ortEnvironment = tauriRuntimeEnvironment(
  { onnxRuntimeDylib: "C:\\runtime\\onnxruntime.dll" },
  { PINVOU_TEST_ENV: "kept" },
);
assert.equal(ortEnvironment.PINVOU_TEST_ENV, "kept");
assert.equal(ortEnvironment.ORT_DYLIB_PATH, "C:\\runtime\\onnxruntime.dll");
assert.match(tauriLogs[0], /pid=4242, phase=build/);
currentTime = 61_000;
heartbeatCallback();
assert.match(tauriLogs[1], /phase=build, elapsed=1m 1s/);
fakeTauriChild.emit("exit", 0, null);
assert.equal(heartbeatCleared, true);
assert.match(tauriLogs[2], /phase=build, elapsed=1m 1s, exit=0/);
tauriResult.then(
  (exitCode) => assert.equal(exitCode, 0),
  (error) => {
    console.error(error);
    process.exitCode = 1;
  },
);

const linux = composeEffectiveConfig([
  platformConfigPath("linux"),
  linuxStartupOverlay,
]).effectiveConfig;
assert.deepEqual(linux.bundle.targets, ["deb"]);
assert.equal(linux.app.windows[0].visible, false);
assert.match(linux.app.windows[0].url, /[?&]startupWindow=hidden(?:&|$)/);
assert.match(linux.build.beforeBuildCommand, /require-wrapper\.js build/);
assert.match(
  linux.build.beforeBuildCommand,
  /npm run build:ui/,
  "release build must resolve Vite from repository dependencies",
);
assert.doesNotMatch(
  linux.build.beforeBuildCommand,
  /&&\s+vite build/,
  "release build must not rely on a globally installed Vite binary",
);
assert.match(linux.build.beforeBundleCommand, /require-wrapper\.js bundle/);
assert.equal(
  linux.bundle.resources["resources/platforms/linux/asr/pinvou3-asr-shim.py"],
  "runtime/asr/pinvou3-asr-shim.py",
);
assert.equal(
  linux.bundle.resources["resources/platforms/linux/asr/LICENSE-SenseVoice.cpp"],
  "runtime/asr/LICENSE-SenseVoice.cpp",
);
assert.equal(
  linux.bundle.resources["resources/platforms/linux/knowledge-host/"],
  "runtime/knowledge-host",
);
assert.equal(
  linux.bundle.resources["resources/platforms/linux/codex-bridge/"],
  "runtime/codex-bridge",
);
assert.equal(
  linux.bundle.resources["resources/platforms/linux/chrome-devtools-mcp/"],
  undefined,
  "Linux BrowserCore must not package the Windows-only chrome-devtools-mcp backend",
);
assert.ok(
  linux.bundle.linux.deb.depends.includes("webkit2gtk-driver"),
  "Linux BrowserCore packages must install the WebKitGTK WebDriver backend",
);
const linuxManifest = buildResourceManifest(linux, { platform: "linux" });
assert.ok(linuxManifest.resourceFileCount > 0);
assert.ok(linuxManifest.files.some((file) => file.destination.startsWith("runtime/asr/")));
assert.ok(
  linuxManifest.files.some((file) => file.destination.startsWith("runtime/codex-bridge/")),
);
assert.ok(
  !linuxManifest.files.some((file) => file.destination.startsWith("runtime/chrome-devtools-mcp/")),
  "Linux resource manifest must exclude chrome-devtools-mcp",
);

assert.match(
  platformArchitectureConfigPath("linux", "x64").replaceAll("\\", "/"),
  /platforms\/linux\/x86_64\/tauri\.conf\.json$/u,
);
assert.match(
  platformArchitectureConfigPath("linux", "arm64").replaceAll("\\", "/"),
  /platforms\/linux\/aarch64\/tauri\.conf\.json$/u,
);

const linuxDev = composeEffectiveConfig([linuxStartupOverlay]).effectiveConfig;
assert.equal(linuxDev.app.windows[0].visible, false);
assert.match(linuxDev.app.windows[0].url, /[?&]startupWindow=hidden(?:&|$)/);
const baseMainWindow = composeEffectiveConfig([]).effectiveConfig.app.windows[0];
for (const [label, config] of [["packaging", linux], ["dev", linuxDev]]) {
  const mainWindow = { ...config.app.windows[0], visible: undefined };
  mainWindow.url = mainWindow.url.replace("&startupWindow=hidden", "");
  assert.deepEqual(
    mainWindow,
    { ...baseMainWindow, visible: undefined },
    `Linux ${label} must only override the main-window cold-start controls`,
  );
}

const generatedFromChangedBase = linuxStartupWindowConfig({
  readFile: () => JSON.stringify({
    app: { windows: [{ label: "main", url: "index.html", width: 1234 }] },
  }),
});
assert.equal(generatedFromChangedBase.app.windows[0].width, 1234);
assert.equal(generatedFromChangedBase.app.windows[0].visible, false);
assert.equal(
  generatedFromChangedBase.app.windows[0].url,
  "index.html?startupWindow=hidden",
  "Linux startup overlay must derive window properties from the base config",
);

const macos = composeEffectiveConfig([platformConfigPath("darwin")]).effectiveConfig;
assert.deepEqual(macos.bundle.targets, ["app", "dmg"]);
assert.equal(
  macos.bundle.resources["resources/platforms/macos/codex-bridge/"],
  "runtime/codex-bridge",
);
assert.equal(
  macos.bundle.resources["resources/platforms/macos/infoplist/"],
  "./",
  "macOS must bundle localized privacy purpose strings",
);
assert.equal(
  macos.bundle.resources["resources/platforms/macos/aarch64/asr/"],
  undefined,
  "macOS system Speech must not bundle the legacy SenseVoice runtime",
);
assert.equal(
  macos.bundle.resources["resources/platforms/macos/chrome-devtools-mcp/"],
  undefined,
  "macOS BrowserCore packages must not carry chrome-devtools-mcp",
);
const macosManifest = buildResourceManifest(macos, { platform: "darwin" });
assert.ok(
  macosManifest.files.some((file) => file.destination.startsWith("runtime/codex-bridge/")),
  "macOS resource manifest must contain the Codex ACP Bridge runtime",
);
for (const locale of ["en", "zh-Hans", "ja"]) {
  assert.ok(
    macosManifest.files.some(
      (file) => file.destination === `${locale}.lproj/InfoPlist.strings`,
    ),
    `macOS resource manifest must contain ${locale} privacy purpose strings`,
  );
}
assert.ok(
  !macosManifest.files.some((file) => file.destination.startsWith("runtime/asr/")),
  "macOS resource manifest must not contain a legacy ASR runtime",
);
assert.ok(
  !macosManifest.files.some((file) => file.destination.startsWith("runtime/chrome-devtools-mcp/")),
  "macOS resource manifest must exclude chrome-devtools-mcp",
);

const windows = composeEffectiveConfig([platformConfigPath("win32")]).effectiveConfig;
assert.equal(
  windows.bundle.resources["resources/platforms/windows/chrome-devtools-mcp/"],
  "runtime/chrome-devtools-mcp",
  "Windows must package the adapter used by the app-owned WebView2 CDP endpoint",
);
const windowsManifest = buildResourceManifest(windows, { platform: "win32" });
assert.ok(
  windowsManifest.files.some((file) => file.destination.startsWith("runtime/chrome-devtools-mcp/")),
  "Windows resource manifest must contain chrome-devtools-mcp",
);

const runtimeBundleExtraction = fs.readFileSync(
  path.join(
    __dirname,
    "..",
    "src-tauri",
    "src",
    "features",
    "runtime_bundle",
    "platform",
    "extraction.rs",
  ),
  "utf8",
);
assert.match(
  runtimeBundleExtraction,
  /#\[cfg\(any\(target_os = "linux", target_os = "macos"\)\)\][\s\S]*?fn browser_mcp_entry_for_session[\s\S]*?@pinvou\/browser-core/,
  "Linux and macOS must register the unified Pinvou BrowserCore wrapper",
);
assert.match(
  runtimeBundleExtraction,
  /#\[cfg\(target_os = "linux"\)\]\s*find_webkit_webdriver\(\)\?;/,
  "Linux BrowserCore must keep its WebKitWebDriver runtime gate",
);
assert.match(
  runtimeBundleExtraction,
  /#\[cfg\(target_os = "windows"\)\]\s*fn browser_mcp_entry_for_session[\s\S]*?PINVOU3_CDMCP_BIN/,
  "the chrome-devtools-mcp environment override must remain inside the Windows-only path",
);

// macOS 主窗口走系统原生红绿灯顶栏(titleBarStyle=Overlay),前端据此隐藏自绘三键。
// --config overlay 按 JSON Merge Patch 合并,windows 数组整体替换,因此 overlay 必须
// 携带完整窗口定义。按基础数组动态生成期望值,确保新增窗口或新增字段也会触发
// 防漂移失败,而不是依赖容易漏项的固定字段清单。
const baseWindows = JSON.parse(fs.readFileSync(BASE_CONFIG_PATH, "utf8")).app.windows;
const expectedMacosWindows = baseWindows.map((window) => (
  window.label === "main"
    ? {
        ...window,
        decorations: true,
        titleBarStyle: "Overlay",
        hiddenTitle: true,
        trafficLightPosition: { x: 12, y: 20 },
      }
    : window
));
assert.deepEqual(
  macos.app.windows,
  expectedMacosWindows,
  "macOS overlay 必须完整同步基础窗口数组,且只覆盖主窗口的原生顶栏字段",
);

const nullRemoval = mergeConfig(
  { bundle: { resources: { common: "common-target", runtime: "runtime-target" } } },
  { bundle: { resources: { runtime: null, staged: "" } } },
);
assert.deepEqual(nullRemoval.bundle.resources, { common: "common-target", staged: "" });

// The private runtime stays optional in Linux/default CI. When it has been
// staged by a Windows build, validate the exact generated overlay as well.
const generatedRuntimeConfig = path.join(
  APP_ROOT,
  "src-tauri",
  "target",
  "windows-runtime",
  "tauri.generated.conf.json",
);
if (fs.existsSync(generatedRuntimeConfig)) {
  const windows = composeEffectiveConfig([
    platformConfigPath("win32"),
    generatedRuntimeConfig,
  ]).effectiveConfig;
  const windowsManifest = buildResourceManifest(windows, { platform: "win32" });
  const destinations = windowsManifest.files.map((file) => file.destination);
  for (const requiredPrefix of [
    "runtime/7zip/",
    "runtime/asr/",
    "runtime/node/",
    "runtime/onnxruntime/",
    "runtime/pandoc/",
    "runtime/poppler/",
    "runtime/python/",
    "runtime/tesseract/",
  ]) {
    assert.ok(
      destinations.some((destination) => destination.startsWith(requiredPrefix)),
      `staged Windows resource manifest must contain ${requiredPrefix}`,
    );
  }
}

console.log("tauri effective config and installer resource manifest: ok");
