const assert = require("node:assert/strict");
const { EventEmitter } = require("node:events");
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
  configSpecs,
  ensureWindowsRustToolchain,
  loadDevelopmentEnvironment,
  parseEnvironmentFile,
  prepareCodexBridge,
  prepareWindowsCodexBridge,
  prepareTauriArgs,
  runTauri,
  tauriRuntimeEnvironment,
  tauriCommandIndex,
  usesValidatedNsisStaging,
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

const noRuntime = () => null;

assert.deepEqual(
  parseEnvironmentFile(`
    # comment
    export PINVOU_ACCOUNT_OIDC_CLIENT_ID="pinvou-desktop-dev"
    PINVOU_ACCOUNT_SERVICE_URL=https://example.test/account # public endpoint
  `),
  {
    PINVOU_ACCOUNT_OIDC_CLIENT_ID: "pinvou-desktop-dev",
    PINVOU_ACCOUNT_SERVICE_URL: "https://example.test/account",
  },
);
const developmentEnvRoot = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou-dev-env-"));
try {
  fs.writeFileSync(path.join(developmentEnvRoot, ".env"), "PINVOU_FILE_VALUE=base\nPINVOU_FILE_ONLY=yes\n");
  fs.writeFileSync(path.join(developmentEnvRoot, ".env.development.local"), "PINVOU_FILE_VALUE=local\n");
  const loadedEnvironment = loadDevelopmentEnvironment(
    { PINVOU_FILE_VALUE: "process" },
    { appRoot: developmentEnvRoot, log: () => {} },
  );
  assert.equal(loadedEnvironment.PINVOU_FILE_VALUE, "process");
  assert.equal(loadedEnvironment.PINVOU_FILE_ONLY, "yes");
} finally {
  fs.rmSync(developmentEnvRoot, { recursive: true, force: true });
}

const isolatedRustEnv = {};
const isolatedRust = configureIsolatedWindowsRustToolchain({
  env: isolatedRustEnv,
  architecture: "x64",
});
assert.equal(isolatedRust.channel, "1.97.1");
assert.equal(isolatedRust.hostTriple, "x86_64-pc-windows-msvc");
assert.equal(isolatedRustEnv.RUSTUP_HOME, isolatedRust.rustupHome);
assert.equal(isolatedRustEnv.PINVOU3_MANAGED_RUSTUP, "1");
assert.match(
  isolatedRust.rustupHome.replaceAll("\\", "/"),
  /\/\.cache\/rustup\/1\.97\.1-x86_64-pc-windows-msvc$/u,
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
  assert.equal(accountRustEnv.RUSTUP_TOOLCHAIN, "1.97.1");
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
    /\/\.cache\/rustup\/1\.97\.1-x86_64-pc-windows-msvc$/u,
  );
});

Promise.all([accountRustCheck, fallbackRustCheck]).catch((error) => {
  console.error(error);
  process.exitCode = 1;
});

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
]);
const linuxArmArgs = prepareTauriArgs(
  ["build", "--bundles", "deb"],
  { platform: "linux", architecture: "arm64", stageRuntime: noRuntime },
);
assert.deepEqual(configSpecs(linuxArmArgs), [
  platformConfigPath("linux"),
  linuxStartupOverlay,
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
assert.deepEqual(
  prepareTauriArgs(["dev"], { platform: "darwin" }),
  ["dev", "--config", platformConfigPath("darwin")],
  "macOS dev must receive the platform overlay (native titlebar) to match packaged output",
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
assert.ok(linux.bundle.resources["resources/common/web-template/"]);
assert.equal(linux.bundle.resources["resources/platforms/linux/asr/"], "runtime/asr");
assert.equal(
  linux.bundle.resources["resources/platforms/linux/codex-bridge/"],
  "runtime/codex-bridge",
);
const linuxManifest = buildResourceManifest(linux, { platform: "linux" });
assert.ok(linuxManifest.resourceFileCount > 0);
assert.ok(linuxManifest.files.some((file) => file.destination.startsWith("web-template/")));
assert.ok(linuxManifest.files.some((file) => file.destination.startsWith("runtime/asr/")));
assert.ok(
  linuxManifest.files.some((file) => file.destination.startsWith("runtime/codex-bridge/")),
);

assert.equal(platformArchitectureConfigPath("linux", "arm64"), null);

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
assert.ok(macos.bundle.resources["resources/common/web-template/"]);
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
