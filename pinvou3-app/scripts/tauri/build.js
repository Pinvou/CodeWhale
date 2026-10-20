const { spawn, spawnSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");
const { writeEffectiveArtifacts } = require("./effective-config.js");
const {
  prepareCodexBridge,
  prepareWindowsCodexBridge,
  WINDOWS_BRIDGE_CONFIG_PATH,
} = require("./codex-bridge.js");
const {
  outputRoot: chromeDevtoolsMcpOutputRoot,
  prepareChromeDevtoolsMcp,
} = require("./chrome-devtools-mcp.js");
const {
  APP_ROOT,
  platformArchitectureConfigPath,
  platformConfigPath,
} = require("./platform-config.js");
const { linuxStartupWindowConfigSpec } = require("./startup-window-config.js");
const { prepareLinuxAsrRuntime } = require("./linux-asr-runtime.js");
const { prepareKnowledgeHost } = require("./knowledge-host.js");
const { WRAPPER_ENV } = require("./require-wrapper.js");
const { stageWindowsInstaller } = require("./windows-installer.js");
const {
  describeWindowsRuntime,
  stageWindowsOnnxRuntime,
  stageWindowsRuntime,
} = require("./windows-runtime.js");

const NSIS_STAGING_CONFIG_SUFFIX =
  "/src-tauri/target/release/nsis-stage/tauri.nsis-stage.conf.json";
const RUST_TOOLCHAIN_CONFIG_PATH = path.join(APP_ROOT, "src-tauri", "rust-toolchain.toml");
const RUST_TOOLCHAIN_SCRIPT_PATH = path.join(
  APP_ROOT,
  "scripts",
  "ci",
  "ensure-rust-toolchain.ps1",
);
const RUSTC_STACK_WRAPPER_SCRIPTS_PATH = path.join(APP_ROOT, "src-tauri", "scripts");

function pinnedRustToolchainChannel(configPath = RUST_TOOLCHAIN_CONFIG_PATH) {
  const config = fs.readFileSync(configPath, "utf8");
  const match = config.match(/^\s*channel\s*=\s*"([^"]+)"/mu);
  if (!match) throw new Error(`Unable to read Rust channel from: ${configPath}`);
  return match[1];
}

function windowsRustHostTriple(architecture = process.arch) {
  const triples = {
    arm64: "aarch64-pc-windows-msvc",
    ia32: "i686-pc-windows-msvc",
    x64: "x86_64-pc-windows-msvc",
  };
  const triple = triples[architecture];
  if (!triple) throw new Error(`Unsupported Windows Rust architecture: ${architecture}`);
  return triple;
}

function configureIsolatedWindowsRustToolchain(
  {
    env = process.env,
    architecture = process.arch,
    appRoot = APP_ROOT,
    configPath = RUST_TOOLCHAIN_CONFIG_PATH,
  } = {},
) {
  const channel = pinnedRustToolchainChannel(configPath);
  const hostTriple = windowsRustHostTriple(architecture);
  const cacheName = `${channel}-${hostTriple}`.replace(/[^a-zA-Z0-9._-]/gu, "_");
  const configuredHome = env.PINVOU3_RUSTUP_HOME?.trim();
  const rustupHome = configuredHome
    ? path.resolve(appRoot, configuredHome)
    : path.join(appRoot, ".cache", "rustup", cacheName);
  env.RUSTUP_HOME = rustupHome;
  env.PINVOU3_MANAGED_RUSTUP = "1";
  env.RUSTUP_TOOLCHAIN = channel;
  return { channel, hostTriple, rustupHome };
}

function runRustToolchainScript(
  {
    args = [],
    env,
    spawnChild,
    appRoot,
    scriptPath,
  },
) {
  const child = spawnChild(
    "powershell.exe",
    [
      "-NoProfile",
      "-NonInteractive",
      "-ExecutionPolicy",
      "Bypass",
      "-File",
      scriptPath,
      ...args,
    ],
    { cwd: appRoot, env: { ...env }, stdio: "inherit" },
  );
  return new Promise((resolve, reject) => {
    let settled = false;
    child.once("error", (error) => {
      if (settled) return;
      settled = true;
      reject(error);
    });
    child.once("exit", (code, signal) => {
      if (settled) return;
      settled = true;
      if (signal) {
        reject(new Error(`Rust toolchain check stopped by signal: ${signal}`));
        return;
      }
      resolve(Number.isInteger(code) ? code : 1);
    });
  });
}

async function ensureWindowsRustToolchain(
  {
    env = process.env,
    spawnChild = spawn,
    architecture = process.arch,
    appRoot = APP_ROOT,
    configPath = RUST_TOOLCHAIN_CONFIG_PATH,
    scriptPath = RUST_TOOLCHAIN_SCRIPT_PATH,
    log = console.log,
  } = {},
) {
  const channel = pinnedRustToolchainChannel(configPath);
  const hostTriple = windowsRustHostTriple(architecture);
  const accountEnv = { ...env };
  delete accountEnv.PINVOU3_MANAGED_RUSTUP;
  const accountExitCode = await runRustToolchainScript({
    args: ["-CheckOnly"],
    env: accountEnv,
    spawnChild,
    appRoot,
    scriptPath,
  });
  if (accountExitCode === 0) {
    env.RUSTUP_TOOLCHAIN = channel;
    log(`[build] Reusing account Rust toolchain: ${channel} (${hostTriple})`);
    return {
      channel,
      hostTriple,
      rustupHome: env.RUSTUP_HOME?.trim() || null,
      source: "account",
    };
  }
  if (accountExitCode !== 2) {
    throw new Error(`Account Rust toolchain check failed with exit code ${accountExitCode}`);
  }

  const toolchain = configureIsolatedWindowsRustToolchain({
    env,
    architecture,
    appRoot,
    configPath,
  });
  log(`[build] Account Rust toolchain is unavailable; using cache: ${toolchain.rustupHome}`);
  const isolatedExitCode = await runRustToolchainScript({
    env,
    spawnChild,
    appRoot,
    scriptPath,
  });
  if (isolatedExitCode !== 0) {
    throw new Error(`Isolated Rust toolchain repair failed with exit code ${isolatedExitCode}`);
  }
  return { ...toolchain, source: "isolated" };
}

// Windows 上以默认 2 MiB 编译器线程栈编译 codewhale-tui 会栈溢出
// (STATUS_STACK_OVERFLOW)。run-dev.sh 与 CI 会自行注入 rustc stack wrapper,
// 但从 PowerShell 或 cmd 直接运行的 `npm run dev` / `npm run build*` 绕开了
// 它们。这里编译(或复用)原生 .exe wrapper 并经 RUSTC_WRAPPER 注入,使
// RUST_MIN_STACK 只作用于编译器子进程;应用运行时永不继承该变量。
function prepareWindowsRustcStackWrapper(
  {
    environment = process.env,
    platform = process.platform,
    scriptsPath = RUSTC_STACK_WRAPPER_SCRIPTS_PATH,
    spawnCompiler = spawnSync,
    log = console.log,
  } = {},
) {
  if (platform !== "win32") return null;

  const configuredWrapper = environment.RUSTC_WRAPPER?.trim();
  const stackWrapperPath = path.join(scriptsPath, "rustc-stack-wrapper.exe");
  if (configuredWrapper) {
    // 调用方显式指定的 wrapper 优先(与 run-dev.sh 相同的优先级)。run-dev.sh
    // 与 release-packages 的 Windows job 传回来的正是本 stack wrapper,只有
    // 其它 wrapper(sccache 等)会放弃 RUST_MIN_STACK 注入,保留默认 2 MiB
    // 编译器栈——已知会在 codewhale-tui 上溢出。
    environment.RUSTC_WRAPPER = configuredWrapper;
    const isStackWrapper = path.win32.resolve(configuredWrapper).toLowerCase()
      === path.win32.resolve(stackWrapperPath).toLowerCase();
    log(
      isStackWrapper
        ? `[build] Reusing caller-provided RUSTC_WRAPPER (the Windows rustc stack wrapper): ${configuredWrapper}`
        : `[build] Reusing caller-provided RUSTC_WRAPPER: ${configuredWrapper}; ` +
          "the 16 MiB compiler stack fix is NOT applied to compiler children.",
    );
    return { path: configuredWrapper, source: "environment" };
  }

  const sourcePath = path.join(scriptsPath, "rustc-stack-wrapper.rs");
  const wrapperPath = stackWrapperPath;
  if (!fs.existsSync(sourcePath)) {
    throw new Error(`Windows rustc stack wrapper source is missing: ${sourcePath}`);
  }

  const needsCompilation = !fs.existsSync(wrapperPath)
    || fs.statSync(sourcePath).mtimeMs > fs.statSync(wrapperPath).mtimeMs;
  if (needsCompilation) {
    // 先编译到唯一临时名再原子改名:被中断的构建(Ctrl-C、CI kill)绝不能
    // 留下半个可执行文件被 mtime 缓存永久复用。
    const tempWrapperPath = `${wrapperPath}.${process.pid}.tmp`;
    // MSVC 链接带 /DEBUG,临时编译还会在临时可执行文件旁写一个同名 .pdb;
    // 两个双胞胎一起清理。
    const tempPdbPath = `${wrapperPath}.${process.pid}.pdb`;
    let result;
    try {
      result = spawnCompiler(
        "rustc",
        ["-O", sourcePath, "-o", tempWrapperPath],
        {
          cwd: APP_ROOT,
          env: { ...environment },
          stdio: "inherit",
          windowsHide: true,
        },
      );
      // 绝不回落到无 wrapper 的编译器:已知会栈溢出。
      if (result.error) {
        throw new Error(`Failed to compile Windows rustc stack wrapper: ${result.error.message}`);
      }
      if (result.status !== 0 || !fs.existsSync(tempWrapperPath)) {
        throw new Error(
          `Failed to compile Windows rustc stack wrapper (exit ${result.status ?? "unknown"})`,
        );
      }
      fs.renameSync(tempWrapperPath, wrapperPath);
    } finally {
      for (const leftover of [tempWrapperPath, tempPdbPath]) {
        if (fs.existsSync(leftover)) {
          fs.rmSync(leftover, { force: true });
        }
      }
    }
  }

  environment.RUSTC_WRAPPER = wrapperPath;
  log(
    `[build] ${needsCompilation ? "Prepared" : "Reused"} Windows rustc stack wrapper: ${wrapperPath}`,
  );
  return { path: wrapperPath, source: needsCompilation ? "compiled" : "cached" };
}

function tauriCommandIndex(args) {
  return args.findIndex((argument) => argument === "build" || argument === "bundle");
}

function configSpecs(args) {
  const specs = [];
  for (let index = 0; index < args.length; index += 1) {
    if (args[index] === "--config" || args[index] === "-c") {
      if (!args[index + 1]) throw new Error("--config 缺少配置值");
      specs.push(args[index + 1]);
      index += 1;
    } else if (args[index].startsWith("--config=")) {
      specs.push(args[index].slice("--config=".length));
    }
  }
  return specs;
}

function windowsBundleTargets(args) {
  const explicit = [];
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    if (argument === "--bundles" || argument === "-b") {
      if (!args[index + 1]) throw new Error(`${argument} 缺少 bundle 类型`);
      explicit.push(args[index + 1]);
      index += 1;
    } else if (argument.startsWith("--bundles=")) {
      explicit.push(argument.slice("--bundles=".length));
    }
  }
  if (explicit.length === 0 || explicit.includes("all")) return ["msi", "nsis"];
  return [...new Set(explicit.flatMap((value) => value.split(",")).filter(Boolean))];
}

function usesValidatedNsisStaging(args) {
  const commandIndex = tauriCommandIndex(args);
  if (commandIndex < 0 || args[commandIndex] !== "bundle") return false;

  return configSpecs(args).some((spec) => {
    const normalized = `/${spec.replaceAll("\\", "/")}`;
    return normalized.endsWith(NSIS_STAGING_CONFIG_SUFFIX);
  });
}

/**
 * Remove every `--bundles none` request from `args` and report whether at
 * least one was present.
 *
 * `none` is a value owned by this wrapper, not by the Tauri CLI: the pinned
 * @tauri-apps/cli 2.11.4 rejects it at arg-parse time ("error: invalid value
 * 'none'" — its `--bundles` possible values on Linux are deb/rpm/appimage),
 * and tauri-bundler could not bundle it anyway (`binary_arch()` has no
 * LoongArch variant and panics on the loongarch64 triple). It means "run the
 * full pipeline (frontend, sidecar preparation, tauri build) but skip
 * bundling", for pipelines that assemble the installer themselves — the
 * loong64 release build hands the compiled binary to
 * `scripts/package-deb-loongarch64.sh`. The CLI's own equivalent is
 * `--no-bundle` ("Skip the bundling step even if `bundle > active` is
 * `true`"), which `prepareTauriArgs` injects in its place.
 *
 * Accepted spellings mirror `windowsBundleTargets` (`--bundles none`,
 * `-b none`, plus the `=` forms). A `none` mixed with real bundle types
 * (`--bundles none,deb`) is deliberately NOT absorbed: it stays in the args
 * and fails at the CLI's value validation, exactly like any other unknown
 * bundle type, instead of silently degrading to no-bundle.
 *
 * @param {string[]} args Full argument list passed to tauri.
 * @returns {{args: string[], noBundle: boolean}} The args without the
 *   request, and whether at least one `none` request was found.
 */
function extractBundlesNone(args) {
  const rest = [];
  let none = false;
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    if (
      (argument === "--bundles" || argument === "-b")
      && args[index + 1] === "none"
    ) {
      none = true;
      index += 1;
    } else if (
      argument === "--bundles=none"
      || argument === "-b=none"
    ) {
      none = true;
    } else {
      rest.push(argument);
    }
  }
  return { args: rest, noBundle: none };
}

/**
 * The wrapper-owned `--bundles none` error for a subcommand that has no
 * `--no-bundle`. Shared by the fail-fast validation and the translation so
 * the wording cannot drift between the two sites.
 */
function bundlesNoneCommandError() {
  return new Error(
    "--bundles none is a wrapper-owned build request: the `bundle` subcommand has"
      + " no --no-bundle and cannot skip bundling. Use `tauri build --bundles none`"
      + " (compile without bundling, package yourself) or pass real bundle types.",
  );
}

/**
 * Absorb the wrapper-owned `--bundles none` request and validate it. The two
 * request shapes that can never succeed fail here, from argv alone:
 * `bundle --bundles none` (the subcommand has no --no-bundle), and a real
 * bundle type requested NEXT to an absorbed `none` (after absorption the
 * leftover spelling would sit next to the injected `--no-bundle`, where the
 * CLI silently lets --no-bundle win — the same silent degradation the
 * `none,deb` value mix above refuses).
 *
 * `main()` calls this before any preparation work (the sidecar steps take
 * minutes and the outcome is knowable from argv); `prepareTauriArgs` calls
 * it again on the same raw argv next to the translation.
 *
 * @param {string[]} args Full argument list passed to tauri.
 * @returns {{args: string[], noBundle: boolean}} The args without the
 *   request, and whether at least one `none` request was found.
 */
function bundlesNonePlan(args) {
  const { args: rest, noBundle } = extractBundlesNone(args);
  if (noBundle) {
    const commandIndex = tauriCommandIndex(rest);
    if (commandIndex >= 0) {
      if (rest[commandIndex] === "bundle") throw bundlesNoneCommandError();
      if (rest[commandIndex] === "build") {
        // Every remaining `--bundles`/`-b` spelling carries a real bundle
        // type: the `none` values were absorbed above.
        const conflicting = rest.some((argument) =>
          argument === "--bundles"
          || argument === "-b"
          || argument.startsWith("--bundles=")
          || argument.startsWith("-b="));
        if (conflicting) {
          throw new Error(
            "conflicting --bundles requests: a real bundle type was requested alongside"
              + " `--bundles none`, and the CLI's --no-bundle would silently win."
              + " Drop one of the two --bundles spellings.",
          );
        }
      }
    }
  }
  return { args: rest, noBundle };
}

/**
 * `--target` 三元组的机器段 → node 的 arch 记法。
 *
 * 往这里加新架构时，`codex-bridge.js` 里反向的那张 machine 映射必须同步加——两张
 * 表的耦合由 `tests/tauri_effective_config.test.js` 里从两处源码派生的断言钉住，
 * 只改一边会红。
 */
const TARGET_MACHINE_ARCHITECTURES = {
  aarch64: "arm64",
  amd64: "x64",
  arm64: "arm64",
  loongarch64: "loong64",
  x86_64: "x64",
};

/**
 * 明确允许回落宿主架构的三元组（只对 darwin 宿主生效）。
 *
 * `universal-apple-darwin` 一次编出 x86_64 与 aarch64 两份再合成通用二进制，
 * 本来就没有单一「目标架构」可言，回落宿主架构才是对的，不能当成「不认识」拒绝。
 * 限定 darwin 宿主：Linux 上传这个三元组没有任何意义（cargo 编不出 macOS 产物，
 * 构建本来就走不通），落到未知机器段硬失败才是对的，别给它开一条绕过闸门的口子。
 */
const HOST_FALLBACK_TARGET_TRIPLES = new Set(["universal-apple-darwin"]);

/**
 * 从 `--target <三元组>` 解析出目标架构，按 node 的 arch 记法返回。
 *
 * 交叉构建时必须按**目标**架构挑 overlay，不能按宿主架构：否则
 * `--target aarch64-unknown-linux-gnu` 会套上 x86_64 的配置，既引用了错误架构
 * 的资源（报「安装包资源不存在」），又漏掉目标架构 overlay 里真正要打的东西。
 */
/**
 * Extract the `--target <triple>` value (`--target X` or `--target=X`); the
 * last occurrence wins, matching cargo's override semantics.
 *
 * The parsing rules and their error messages are shared with
 * `targetArchitecture`, which consumes the machine segment. `main()` also uses
 * the full triple to locate pre-built cross artifacts: cargo target
 * directories are keyed by the whole triple, not by the machine segment.
 *
 * @param {string[]} args Full argument list passed to tauri.
 * @returns {string|null} The triple, or null without `--target`.
 * @throws {Error} `--target` is present without a usable value.
 */
function targetTriple(args) {
  let triple = null;
  for (let i = 0; i < args.length; i += 1) {
    if (args[i] === "--target") {
      // 值缺失、或下一个 token 又是个 flag：显式报错，别把 `--bundles` 当三元组往下
      // 传。否则机器段会被解析成「--bundles」，报出来的是「机器段「--bundles」不在
      // 架构映射表里」，指着一个根本不存在的架构问题，查半天才发现是少打了个值。
      const value = args[i + 1];
      if (!value || value.startsWith("--")) {
        throw new Error(
          `--target 后面必须跟目标三元组（如 aarch64-unknown-linux-gnu），实际是`
            + `「${value ?? "（没有下一个参数）"}」。`,
        );
      }
      triple = value;
      i += 1;
    } else if (args[i].startsWith("--target=")) {
      const value = args[i].slice("--target=".length);
      if (!value) throw new Error("--target= 后面必须跟目标三元组，当前是空值。");
      triple = value;
    }
  }
  return triple;
}

/**
 * Parse the target architecture (Node notation) from `--target <triple>`.
 *
 * 没传 --target 时返回 null，调用方回退到 process.arch，原行为不变。
 *
 * `--target` 的解析复用 `targetTriple`（`--target X` 与 `--target=X` 都认，
 * 最后一次出现生效）。
 *
 * 机器段**不认识**时（`riscv64gc-…`、`armv7-…`）不能也回 null：闸门对
 * `!targetArch` 是直接放行的，overlay、ASR、共享知识库服务会全部回落宿主架构，
 * 产出的正是闸门要防的那种「主程序是目标架构、侧车全是宿主架构」的静默错包，
 * 只是换了个入口进来。所以这种情况硬失败——宁可现在失败，不要静默错包。
 *
 * 两个例外仍然回落，因为它们根本不是「交叉构建到未知架构」：
 *   - 三元组在 `HOST_FALLBACK_TARGET_TRIPLES` 里（通用二进制没有单一目标架构）；
 *   - 宿主自己的 arch 就不在表里（比如在 riscv64 机器上原生构建
 *     `riscv64gc-unknown-linux-gnu`），这时我们无从判断是不是交叉，不该越权拦。
 * 同理只在 Linux 宿主上拦：这套「按宿主架构备料」的侧车只存在于 Linux 打包路径。
 *
 * @param {string[]} args 传给 tauri 的完整参数。
 * @param {object} [options] 宿主平台与宿主架构，用来判断是不是真的在交叉构建。
 * @returns {string|null} "arm64" / "loong64" / "x64"；无法判定且允许回落时 null。
 * @throws {Error} Linux 宿主确实在交叉构建，而机器段不在映射表里。
 */
function targetArchitecture(
  args,
  { platform = process.platform, hostArch = process.arch } = {},
) {
  const triple = targetTriple(args);
  if (!triple) return null;
  if (platform === "darwin" && HOST_FALLBACK_TARGET_TRIPLES.has(triple)) return null;
  const machine = triple.split("-")[0];
  const architecture = TARGET_MACHINE_ARCHITECTURES[machine];
  if (architecture) return architecture;
  const hostIsKnown = Object.values(TARGET_MACHINE_ARCHITECTURES).includes(hostArch);
  if (platform !== "linux" || !hostIsKnown) return null;
  throw new Error(
    `无法从 --target ${triple} 判定目标架构：机器段「${machine}」不在架构映射表里。`
      + `宿主是 ${hostArch}，回落宿主架构会挑错 overlay，并让语音识别运行时和共享`
      + `知识库服务按宿主架构备料，产出「主程序是目标架构、侧车全是宿主架构」的`
      + `静默错包，所以这里直接拒绝。要支持这个架构，下面几处都要补——漏②或④`
      + `会重新引入静默错包，漏①、③、⑤或⑥会在构建时当场报错：`
      + `①本文件的 TARGET_MACHINE_ARCHITECTURES；`
      + `②scripts/tauri/platform-config.js 的 ARCHITECTURE_CONFIG_NAMES（漏了它 `
      + `platformArchitectureConfigPath 返回 null，整层架构 overlay 会被静默跳过）；`
      + `③src-tauri/config/platforms/linux/ 下对应的架构 overlay（漏了它是`
      + `「解析配置失败/Tauri overlay 不存在」的响亮失败）；`
      + `④scripts/tauri/codex-bridge.js 里 node arch → 机器段的那张 machine 映射`
      + `（漏了它 machine 会是 undefined，环境变量不注入，备料脚本回落 uname -m 按`
      + `**宿主**架构备 Node，包里的 Bridge node 架构就错了，而且全程零报错）；`
      + `⑤scripts/prepare-codex-bridge-runtime.sh 的目标词表（漏了它落到 *) 硬失败）；`
      + `⑥scripts/tauri/linux-asr-runtime.js 的 LINUX_ASR_ARCHITECTURES 表。`
      + `若它是通用二进制那类没有单一目标架构的三元组（仅 darwin 宿主回落），`
      + `则加进 HOST_FALLBACK_TARGET_TRIPLES。`,
  );
}

/**
 * 摘掉一份 overlay 里被 marker 命中的资源声明，用摘过的那份**顶替**原 overlay。
 *
 * 【本 PR「摘资源声明」方案论证的唯一落点：两个包装函数和两个测试文件的头注释都
 * 只留一行指过来，不要再抄一份——这段论证已经因为两处抄本各自漂移被逮到过。】
 *
 * ## 要解决的问题
 * 交叉构建时有两步按开关跳过了备料（ASR 运行时、共享知识库服务），但 overlay 里还
 * 声明着它们的资源路径。`main()` 在 `runTauri` 之前先把 `configSpecs(preparedArgs)`
 * 交给 `writeEffectiveArtifacts`，它对合并后生效配置里的每条 resources 做磁盘存在性
 * 校验，于是以「安装包资源不存在」终止。
 *
 * ## 为什么是顶替而不是叠一层 `{资源键: null}`
 * 先说清楚：叠 null 是条**走得通、而且更短**的路。overlay 走 JSON Merge Patch，
 * null 能删继承键（见 `effective-config.js` 的 `mergeConfig`；NSIS 那条链路的
 * `stage-resources.ps1` 就在生产里靠它摘掉已暂存的资源声明）。要写出的键也都现成：
 * 平台层的 `knowledge-host/` 本来就是静态串；架构层那条能直接从本文件顶上已经
 * import 的 `linux-asr-runtime.js` 拿（`LINUX_ASR_ARCHITECTURES` 是机器段词表，
 * `linuxAsrRuntimeOutput()` 连完整的 `binaryPath` 都拼好了），补个命名导入即可，
 * 零新增映射。
 *
 * 选顶替只剩**一条**理由，这也是全部理由：摘除是从 overlay 文件**自己的键**里过滤
 * 出来的。overlay 里以后新增一条同样备不出来的资源声明时，摘除自动跟随，不必回这里
 * 补第二份键清单；叠 null 只删得掉显式写出来的那几条。
 *
 * 对称的代价一并写明，别只记着好处：
 *   - marker 是**子串**匹配，比精确键宽。overlay 里哪天出现一条名字里含
 *     `linux-asr-runtime` / `knowledge-host`、却本该打进包的声明，会被一起误摘。
 *   - 顶替是整份 overlay 重写，风险从「漏摘」换成了「丢键」，只能靠测试逐层比对原
 *     文件来兜（见 `tests/tauri_skip_knowledge_host_overlay.test.js`）。
 * 叠 null 这两条风险都没有。取舍成立，但不是白捡的。
 *
 * ## 一条**不成立**的理由，记在这里免得又被写回来
 * 「漏摘一条的代价是构建跑到二十分钟后的 bundle 阶段才报错」是假的：上面那道
 * `writeEffectiveArtifacts` 校验在 cargo 启动之前几秒就跑完，漏键在那里就以同一条
 * 「安装包资源不存在」失败。两种方案的失败点和报错文案完全相同，不构成取舍依据。
 *
 * ## 为什么不直接改那个纳入版本控制的配置文件
 * 那样每次交叉构建都要「改了记得还原」，而且 `tests/tauri_platform_layout.test.js`
 * 会一直是红的。构建期的临时差异就该留在构建脚本里。
 *
 * `--config` 既收路径也收内联 JSON 串（`linuxStartupWindowConfigSpec` 就是这么用的）。
 * 这里返回内联 JSON 串只是为了不落临时文件——省掉清理和「进程中断留下脏文件」的
 * 尾巴。资源路径的解析基准始终是 src-tauri，与 `--config` 是文件还是内联无关
 * （Tauri CLI 对两种形态走同一条合并链）。
 * @param {string} configPath overlay 的绝对路径。
 * @param {string[]} markers 命中即摘除的资源源路径子串。
 * @returns {string} 摘过之后的内联 JSON；没有可摘的就原样返回路径。
 */
function configWithoutResources(configPath, markers) {
  let raw;
  try {
    raw = JSON.parse(fs.readFileSync(configPath, "utf8"));
  } catch (error) {
    // 原样抛 SyntaxError 的话只有一句 "Unexpected token"，看不出是哪个文件——
    // 而这里读的是按平台/架构拼出来的路径，出错时最需要知道的就是文件名。
    // 带上 cause：包装错误保留原始 SyntaxError 的位置信息，不然只剩一句文案。
    throw new Error(`解析配置失败 ${configPath}: ${error.message}`, { cause: error });
  }
  const resources = raw?.bundle?.resources;
  if (!resources || typeof resources !== "object") return configPath;
  const kept = Object.fromEntries(
    Object.entries(resources).filter(
      ([source]) => !markers.some((marker) => source.includes(marker)),
    ),
  );
  if (Object.keys(kept).length === Object.keys(resources).length) return configPath;
  return JSON.stringify({ ...raw, bundle: { ...raw.bundle, resources: kept } });
}

/**
 * 跳过 ASR 运行时构建时，摘掉架构 overlay 里的 ASR 资源声明。
 *
 * 跳过构建之后 `target/linux-asr-runtime/<机器段>/sense-voice-main` 不存在，而架构
 * overlay 里声明了它。为什么用顶替而不是叠 null 见 `configWithoutResources`。
 * @param {string} configPath 架构 overlay 的绝对路径。
 * @returns {string} 摘掉之后的内联 JSON；没有可摘的就原样返回路径。
 */
function architectureConfigWithoutAsr(configPath) {
  return configWithoutResources(configPath, ["linux-asr-runtime"]);
}

/**
 * 跳过共享知识库服务时，摘掉**平台** overlay 里的 knowledge-host 资源声明。
 *
 * 为什么是平台层而不是架构层：`knowledge-host/` 这条声明写在
 * `config/platforms/linux/tauri.conf.json` 里，架构 overlay 只声明了 ASR。
 * 第一版只加了「跳过构建」的开关、没摘这条声明，后果分两种，**都不报错**：
 *   - 干净树：目录不存在，资源枚举出零个文件，包里静默缺 server；
 *   - 脏树：目录里躺着上次**宿主架构**构建留下的 server，它被原样打进 arm64 包。
 * 两种都要等用户在设备上用到共享知识库才暴露，比构建时直接失败难查得多。
 *
 * 为什么用顶替而不是叠 null 见 `configWithoutResources`。
 * @param {string} configPath 平台 overlay 的绝对路径。
 * @returns {string} 摘掉之后的内联 JSON；没有可摘的就原样返回路径。
 */
function platformConfigWithoutKnowledgeHost(configPath) {
  return configWithoutResources(configPath, ["knowledge-host"]);
}

/**
 * 目标架构 ≠ 宿主架构时，检查那些「只会按宿主架构备料」的步骤有没有被妥善处理。
 *
 * 为什么必须是**硬失败**而不是警告：不做决定的后果要么报错报得晚、要么**静默地错**——
 *   - 忘开 `PINVOU3_SKIP_LINUX_ASR`：备料脚本被要求的是**宿主**架构，
 *     `requested_arch == host_arch` 断言照常通过；构建要到后面 effective-config
 *     的资源存在性校验才停下来——目标架构的 overlay 声明了一个从没备出来的文件；
 *   - 忘开 `PINVOU3_SKIP_KNOWLEDGE_HOST`：`cargo build` 不带 `--target`，编出来的
 *     **宿主架构** server 恰好落在平台 overlay 期望的位置，被原样打进另一个架构的
 *     包里，构建全程零警告。
 *     用户要等到在设备上用共享知识库时才发现，而那时候根本想不到是构建的问题。
 *
 * 每个侧车对同一个决定有两种可接受的解法：跳过该步（并摘掉它的资源声明），或者
 * 用配套的 `PINVOU3_*_PREBUILT=1` 开关预先保证目标架构产物就位。预构建路线保留
 * overlay 声明，产物照常打包；`main()` 把同一个决定传给备料步骤（此时 ASR 步骤
 * 要求引擎已在 `target/linux-asr-runtime/<machine>/sense-voice-main`，共享知识库
 * 从 `pinvou-knowledge/target/<triple>/release/` 复制）。
 *
 * codex-bridge 不在这张清单上：它已经改成按目标架构备料了（见
 * `prepare-codex-bridge-runtime.sh` 里的 `PINVOU3_BRIDGE_TARGET_ARCH`），
 * 交叉构建能产出**正确**的包，不需要跳过、也就不需要闸门。
 *
 * @param {object} options 目标/宿主架构与跳过/预构建开关。
 * @returns {string[]} 违规项的说明；空数组表示没问题。
 */
function crossBuildViolations({
  platform,
  targetArch,
  hostArch,
  skipLinuxAsr,
  skipKnowledgeHost,
  prebuiltLinuxAsr = false,
  prebuiltKnowledgeHost = false,
}) {
  if (platform !== "linux" || !targetArch || targetArch === hostArch) return [];
  const problems = [];
  if (!skipLinuxAsr && !prebuiltLinuxAsr) {
    problems.push(
      "语音识别运行时（SenseVoice）只能在目标架构上原生构建，交叉构建请设 PINVOU3_SKIP_LINUX_ASR=1，"
        + "或预先构建目标架构引擎并设 PINVOU3_LINUX_ASR_PREBUILT=1",
    );
  }
  if (!skipKnowledgeHost && !prebuiltKnowledgeHost) {
    problems.push(
      "共享知识库服务的构建不认 --target，只会编出宿主架构的二进制，交叉构建请设 PINVOU3_SKIP_KNOWLEDGE_HOST=1，"
        + "或预先构建目标三元组的服务端并设 PINVOU3_KNOWLEDGE_HOST_PREBUILT=1",
    );
  }
  return problems;
}

function prepareTauriArgs(
  args,
  {
    platform = process.platform,
    architecture = process.arch,
    stageRuntime = stageWindowsRuntime,
    additionalConfigs = [],
    // 两个跳过开关正常是**从参数进来**的：`main()` 在入口一次性把 env 读成布尔，
    // 闸门、备料和这里的 overlay 摘除全用同一份结论。
    //
    // 这不是洁癖：同一个变量要是在这里和 main() 里各读一次，两处**永远有可能读出
    // 两种结论**——中途有人改了环境，或者以后哪次重构把读取位置挪到了会动 env 的
    // 步骤后面。两处一旦不一致，表现是「资源声明摘掉了但东西照样构建」或者反过来
    // 「东西没构建但声明还在」，后者直接让 bundle 报「安装包资源不存在」。
    //
    // 默认值只兜底不经 main() 的调用方（测试、别的脚本直接 require 本模块），
    // 让它们不必自己拼开关；真实构建路径上这两个默认值不会被求值。
    skipLinuxAsr = process.env.PINVOU3_SKIP_LINUX_ASR === "1",
    skipKnowledgeHost = process.env.PINVOU3_SKIP_KNOWLEDGE_HOST === "1",
  } = {},
) {
  // Absorb the wrapper-owned `--bundles none` request (see the helper doc):
  // the value never reaches the Tauri CLI, which rejects it at arg-parse
  // time. `main()` hands the raw argv here unfiltered — its own
  // `windowsBundleTargets` pre-parse is unaffected because staging returns
  // null unless the targets include "nsis" — so this is the single point
  // where the request is translated into the CLI's `--no-bundle`. The plan
  // also fails the two never-succeedable request shapes from argv alone
  // (bundle+none, none next to a real bundle type); `main()` already ran it
  // for fail-fast, and re-running it here keeps the translation site honest
  // for direct callers that bypass `main()`.
  const { args: withoutBundlesNone, noBundle } = bundlesNonePlan(args);
  const prepared = [...withoutBundlesNone];
  const commandIndex = tauriCommandIndex(prepared);
  if (commandIndex < 0) {
    // dev 不注入 packaging overlay。macOS 复用平台 overlay 保持原生顶栏一致；
    // Linux 只注入 dev overlay，让冷启动窗口等 React 首次提交后再显示，避开
    // Mutter/XWayland 首次映射期间视觉表面与输入表面短暂错位。
    const devIndex = prepared.indexOf("dev");
    const devConfig = platform === "darwin"
      ? platformConfigPath(platform)
      : platform === "linux"
        ? linuxStartupWindowConfigSpec()
        : null;
    const automaticConfigs = [devConfig, ...additionalConfigs].filter(Boolean);
    if (devIndex >= 0 && automaticConfigs.length > 0) {
      // 与 build/bundle 保持相同优先级:自动平台配置在前,调用方显式
      // --config 在后,从而仍可有意覆盖平台默认值。
      const injected = automaticConfigs.flatMap((configPath) => ["--config", configPath]);
      prepared.splice(devIndex + 1, 0, ...injected);
    }
    return prepared;
  }

  // 平台 overlay。跳过共享知识库服务时要同时摘掉它那条资源声明——声明写在
  // **平台层**（`knowledge-host/`），不在架构层，所以摘的是这个文件。
  const platformConfig = platformConfigPath(platform);
  const automaticConfigs = [
    skipKnowledgeHost && platform === "linux"
      ? platformConfigWithoutKnowledgeHost(platformConfig)
      : platformConfig,
  ];
  if (platform === "linux") automaticConfigs.push(linuxStartupWindowConfigSpec());
  // 交叉构建时 overlay 必须跟 --target 走，不能跟宿主架构走。宿主平台/架构一并传
  // 进去：`targetArchitecture` 要靠它判断「机器段不认识」到底是交叉构建（拒绝）
  // 还是原生构建（回落）。
  const effectiveArchitecture = targetArchitecture(prepared, {
    platform,
    hostArch: architecture,
  }) || architecture;
  const architectureConfig = platformArchitectureConfigPath(platform, effectiveArchitecture);
  if (architectureConfig) {
    // `platform === "linux"` 守卫与上面 knowledge-host 那条对称：ASR 顶替只针对
    // Linux 的 `target/linux-asr-runtime/…` 声明。少了它，macOS/Windows 的架构
    // overlay 也会被读进来重写一遍——当前 marker 在那些文件里命中不了、顶替会原样
    // 返回路径，所以无害，但这种「靠 marker 恰好不命中」的无害是会过期的。
    automaticConfigs.push(
      skipLinuxAsr && platform === "linux"
        ? architectureConfigWithoutAsr(architectureConfig)
        : architectureConfig,
    );
  }
  // bundle:nsis has already validated the staging overlay. Do not inject the
  // original runtime resource mappings again or Tauri will enumerate and
  // verify the pre-signed binaries that the NSIS hook owns.
  const stagedRuntime = usesValidatedNsisStaging(prepared)
    ? null
    : stageRuntime({ platform });
  const runtimeConfig =
    typeof stagedRuntime === "string" ? stagedRuntime : stagedRuntime?.configPath;
  if (runtimeConfig) automaticConfigs.push(runtimeConfig);
  automaticConfigs.push(...additionalConfigs);
  const injected = automaticConfigs.flatMap((configPath) => ["--config", configPath]);
  // Automatic overlays must precede explicit signing/staging overlays so the
  // caller can intentionally override or remove inherited resource mappings.
  prepared.splice(commandIndex + 1, 0, ...injected);
  // `build`-gated: `bundlesNonePlan` already threw for `bundle` (no
  // --no-bundle there) and for a contradictory second spelling, and `dev`
  // falls through on purpose (it never bundles, so stripping IS the
  // requested semantics). Translate the absorbed `--bundles none` request
  // into the flag the pinned @tauri-apps/cli 2.11.4 actually accepts
  // (verified against `tauri build --help`: `--no-bundle`, "Skip the
  // bundling step even if `bundle > active` is `true`"). The compiled
  // binary stays at the cargo target dir (`target/<triple>/release/`) for
  // the caller's own packager; value-flag pairs (`--config <spec>`) keep
  // their value adjacency, which is why the flag is spliced before them.
  // Read the subcommand from `prepared`, not the raw argv: the absorbed
  // `--bundles none` tokens shift every index after them, so when the
  // request precedes the subcommand, an index computed on `prepared`
  // points at the wrong token in `args`.
  if (noBundle && prepared[commandIndex] === "build") {
    prepared.splice(commandIndex + 1, 0, "--no-bundle");
  }
  return prepared;
}

function formatElapsed(milliseconds) {
  const totalSeconds = Math.max(0, Math.floor(milliseconds / 1000));
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  return [
    hours > 0 ? `${hours}h` : null,
    minutes > 0 || hours > 0 ? `${minutes}m` : null,
    `${seconds}s`,
  ].filter(Boolean).join(" ");
}

function tauriPhase(preparedArgs) {
  // dev 必须报告自己的阶段,而不是落进泛化的 "command" 标签。
  if (preparedArgs[0] === "dev") return "dev";
  const commandIndex = tauriCommandIndex(preparedArgs);
  return commandIndex >= 0 ? preparedArgs[commandIndex] : "command";
}

function runTauri(
  preparedArgs,
  {
    environment = process.env,
    spawnChild = spawn,
    heartbeatIntervalMs = 60_000,
    now = Date.now,
    log = console.log,
    setIntervalFn = setInterval,
    clearIntervalFn = clearInterval,
  } = {},
) {
  const tauriCli = require.resolve("@tauri-apps/cli/tauri.js");
  const startedAt = now();
  const phase = tauriPhase(preparedArgs);
  const child = spawnChild(process.execPath, [tauriCli, ...preparedArgs], {
    cwd: APP_ROOT,
    env: { ...environment, [WRAPPER_ENV]: "1" },
    stdio: "inherit",
  });
  const pid = child.pid ?? "unknown";
  log(`[build] Tauri CLI started: pid=${pid}, phase=${phase}`);

  let heartbeat = null;
  if (heartbeatIntervalMs > 0) {
    heartbeat = setIntervalFn(() => {
      log(
        `[build] Tauri CLI still running: pid=${pid}, phase=${phase}, elapsed=${formatElapsed(now() - startedAt)}`,
      );
    }, heartbeatIntervalMs);
    heartbeat?.unref?.();
  }

  return new Promise((resolve, reject) => {
    let settled = false;
    const cleanup = () => {
      if (heartbeat !== null) clearIntervalFn(heartbeat);
    };
    child.once("error", (error) => {
      if (settled) return;
      settled = true;
      cleanup();
      reject(error);
    });
    child.once("exit", (code, signal) => {
      if (settled) return;
      settled = true;
      cleanup();
      const elapsed = formatElapsed(now() - startedAt);
      if (signal) {
        log(
          `[build] Tauri CLI stopped by signal: pid=${pid}, phase=${phase}, elapsed=${elapsed}, signal=${signal}`,
        );
        resolve(1);
        return;
      }
      const exitCode = Number.isInteger(code) ? code : 1;
      log(
        `[build] Tauri CLI finished: pid=${pid}, phase=${phase}, elapsed=${elapsed}, exit=${exitCode}`,
      );
      resolve(exitCode);
    });
  });
}

function tauriRuntimeEnvironment(runtime, environment = process.env) {
  return runtime
    ? { ...environment, ORT_DYLIB_PATH: runtime.onnxRuntimeDylib }
    : environment;
}

function supportsChromeDevtoolsMcp(platform = process.platform) {
  return platform === "win32";
}

function prepareChromeDevtoolsMcpForPlatform({
  platform = process.platform,
  prepare = prepareChromeDevtoolsMcp,
} = {}) {
  if (!supportsChromeDevtoolsMcp(platform)) return false;
  return prepare({ platform });
}

function withoutChromeDevtoolsMcpOverride(environment) {
  if (!Object.prototype.hasOwnProperty.call(environment, "PINVOU3_CDMCP_BIN")) {
    return environment;
  }
  const sanitized = { ...environment };
  delete sanitized.PINVOU3_CDMCP_BIN;
  return sanitized;
}

function chromeDevtoolsMcpEnvironment(
  development,
  environment = process.env,
  platform = process.platform,
) {
  // Only the Windows WebView2 host currently exposes the app-owned CDP endpoint.
  // Never leak a caller-provided MCP binary override into packaged builds or into
  // the currently inactive WKWebView/WebKitGTK substrate.
  if (!development || !supportsChromeDevtoolsMcp(platform)) {
    return withoutChromeDevtoolsMcpOverride(environment);
  }
  return {
    ...environment,
    PINVOU3_CDMCP_BIN: path.join(
      chromeDevtoolsMcpOutputRoot(platform),
      "build",
      "src",
      "bin",
      "chrome-devtools-mcp.js",
    ),
  };
}

async function main() {
  const args = process.argv.slice(2);
  const validateOnly = args[0] === "--validate-only";
  if (validateOnly) args.shift();

  if (validateOnly) return 0;

  const isDev = args.includes("dev");
  const hasTauriBuildCommand = tauriCommandIndex(args) >= 0;
  // `--bundles none` 里两种注定无法成功的请求形态（bundle+none、none 旁边再要
  // 一种真实打包类型）只看 argv 就能判定，而下面的备料步骤一跑就是几分钟：
  // 在任何备料开始前先快速失败。`prepareTauriArgs` 会在翻译点对绕过 `main()`
  // 的直接调用方再跑一次同一份 plan。
  bundlesNonePlan(args);
  // 两个跳过开关在整条流程里**只在这里读一次**：闸门、备料和 overlay 摘除
  // （`prepareTauriArgs`）都收这同一份布尔，中间隔着多少 await 都不会读出第二种
  // 结论。不一致的代价是包静默地错，见 `prepareTauriArgs` 的参数注释。
  const skipLinuxAsr = process.env.PINVOU3_SKIP_LINUX_ASR === "1";
  const skipKnowledgeHost = process.env.PINVOU3_SKIP_KNOWLEDGE_HOST === "1";
  // The pre-built switches are the alternative resolution of the same two
  // decisions (see crossBuildViolations): they keep the overlay declarations
  // and make the preparation steps accept a target-architecture artifact
  // instead of building one. Read once here and passed down, like the skip
  // switches.
  const prebuiltLinuxAsr = process.env.PINVOU3_LINUX_ASR_PREBUILT === "1";
  const prebuiltKnowledgeHost = process.env.PINVOU3_KNOWLEDGE_HOST_PREBUILT === "1";
  const additionalConfigs = [];
  // dev 与 build/bundle 一样经由 cargo 编译,所以半安装的工具链也必须在
  // `tauri dev` 之前修复;下方的 wrapper 注入对两条路径一视同仁。
  if (process.platform === "win32" && (hasTauriBuildCommand || isDev)) {
    await ensureWindowsRustToolchain();
  }
  const windowsRuntime = hasTauriBuildCommand && process.platform === "win32"
    ? (usesValidatedNsisStaging(args)
        ? describeWindowsRuntime()
        : stageWindowsRuntime())
    : null;
  const windowsDevRuntime = isDev && process.platform === "win32"
    ? stageWindowsOnnxRuntime()
    : null;
  if (windowsRuntime) {
    stageWindowsInstaller({
      bundleTargets: windowsBundleTargets(args),
      runtime: windowsRuntime,
    });
  }
  const windowsBridgeOptions = windowsRuntime
    ? {
        nodeExecutable: windowsRuntime.nodeExecutable,
        npmExecPath: windowsRuntime.npmExecPath,
      }
    : undefined;
  if (isDev) {
    prepareCodexBridge();
    // Tauri dev does not apply the package resource overlay, so the development process
    // points directly to the verified workspace vendor entry. Only Windows WebView2 exposes
    // app-owned CDP. Linux uses BrowserCore/WebKitWebDriver and macOS product capability is
    // currently disabled; neither platform may prepare or fall back to external Chrome.
    await prepareChromeDevtoolsMcpForPlatform();
    prepareWindowsCodexBridge();
    const developmentHost = prepareKnowledgeHost({ development: true });
    if (developmentHost?.configSpec) additionalConfigs.push(developmentHost.configSpec);
  }
  if (hasTauriBuildCommand) {
    // SenseVoice 按设计只能在目标架构上原生构建（build-sensevoice-runtime.sh 要求
    // requested_arch == host_arch，产物还链接宿主 glibc），所以交叉构建必须显式
    // 跳过它；跳过后 overlay 里的资源声明由 prepareTauriArgs 摘掉。
    const hostArch = process.arch;
    const triple = targetTriple(args);
    // 机器段不认识时这里会直接抛（见 `targetArchitecture`），不会漏进闸门——
    // 闸门对 `!targetArch` 是放行的，回 null 等于给未知架构开了条静默后门。
    const targetArch = targetArchitecture(args);
    const isCrossBuild = targetArch !== null && targetArch !== hostArch;

    // 交叉构建的一致性闸门：宁可现在就失败，也不要产出一个静默错架构的包。
    const violations = crossBuildViolations({
      platform: process.platform,
      targetArch,
      hostArch,
      skipLinuxAsr,
      skipKnowledgeHost,
      prebuiltLinuxAsr,
      prebuiltKnowledgeHost,
    });
    if (violations.length > 0) {
      const detail = violations.map((line) => `  - ${line}`).join("\n");
      throw new Error(
        `交叉构建（宿主 ${hostArch} → 目标 ${targetArch}）配置不完整：\n${detail}`,
      );
    }

    // SenseVoice 按设计只能在目标架构上原生构建（build-sensevoice-runtime.sh 要求
    // requested_arch == host_arch，产物还链接宿主 glibc）。交叉构建改按目标架构
    // 取引擎：要么流水线已预构建引擎并设 PINVOU3_LINUX_ASR_PREBUILT=1（此步改为
    // 校验并接受它），要么脚本的架构断言当场失败；只有跳过时才由 prepareTauriArgs
    // 摘掉 overlay 里的资源声明。
    if (!skipLinuxAsr) prepareLinuxAsrRuntime(isCrossBuild ? { architecture: targetArch, prebuilt: prebuiltLinuxAsr } : {});
    // Bridge 按**目标**架构备料：它不编译任何东西，只是下 Node 官方 tarball、
    // 校验 sha256、解压，所以交叉构建能产出正确的包，不需要跳过开关。
    // 不传 targetArch 时脚本回落到 uname -m，非交叉场景行为不变。
    prepareCodexBridge(targetArch ? { targetArch } : {});
    await prepareChromeDevtoolsMcpForPlatform();
    prepareWindowsCodexBridge(windowsBridgeOptions);
    // 共享知识库服务编的是**宿主架构**的二进制（cargo build 不带 --target），
    // 交叉构建时它会被打进另一个架构的包里，本来就是错的；跟 SenseVoice 同一类
    // 问题。交叉构建要么跳过它，要么在 PINVOU3_KNOWLEDGE_HOST_PREBUILT=1 时复制
    // 流水线为精确目标三元组预构建的 server。
    if (!skipKnowledgeHost) prepareKnowledgeHost(isCrossBuild ? { targetTriple: triple, prebuilt: prebuiltKnowledgeHost } : {});
    if (process.platform === "win32") {
      additionalConfigs.push(WINDOWS_BRIDGE_CONFIG_PATH);
    }
  }

  const preparedArgs = prepareTauriArgs(args, {
    additionalConfigs,
    stageRuntime: () => windowsRuntime,
    // 显式传上面算好的那份，而不是让 prepareTauriArgs 再读一次 env：两次读取之间
    // 隔着若干 await，摘声明和跳过备料必须是同一个结论。
    skipLinuxAsr,
    skipKnowledgeHost,
  });
  if (hasTauriBuildCommand) {
    const artifacts = writeEffectiveArtifacts(configSpecs(preparedArgs));
    console.log(`[build] 有效 Tauri 配置: ${artifacts.effectiveConfigPath}`);
    console.log(
      `[build] 安装包资源清单: ${artifacts.resourceManifestPath} (${artifacts.resourceManifest.resourceFileCount} files)`,
    );
  }

  const tauriEnvironment = chromeDevtoolsMcpEnvironment(
    isDev,
    tauriRuntimeEnvironment(windowsRuntime || windowsDevRuntime),
  );
  prepareWindowsRustcStackWrapper({ environment: tauriEnvironment });
  return runTauri(preparedArgs, { environment: tauriEnvironment });
}

if (require.main === module) {
  main().then((exitCode) => {
    process.exitCode = exitCode;
  }).catch((error) => {
    console.error(`[build] ${error.message}`);
    process.exitCode = 1;
  });
}

module.exports = {
  // 导出给测试：扩展新架构要同步改的几张表之间有「漏改一处就静默错包」的约束，
  // 测试必须从这张表**派生**出断言，硬编码一份的话新增架构照样绿。
  TARGET_MACHINE_ARCHITECTURES,
  architectureConfigWithoutAsr,
  configWithoutResources,
  configureIsolatedWindowsRustToolchain,
  configSpecs,
  crossBuildViolations,
  bundlesNonePlan,
  extractBundlesNone,
  platformConfigWithoutKnowledgeHost,
  targetArchitecture,
  chromeDevtoolsMcpEnvironment,
  ensureWindowsRustToolchain,
  formatElapsed,
  main,
  prepareChromeDevtoolsMcp,
  prepareChromeDevtoolsMcpForPlatform,
  prepareCodexBridge,
  prepareKnowledgeHost,
  prepareLinuxAsrRuntime,
  prepareWindowsRustcStackWrapper,
  prepareWindowsCodexBridge,
  stageWindowsInstaller,
  stageWindowsOnnxRuntime,
  stageWindowsRuntime,
  prepareTauriArgs,
  runTauri,
  supportsChromeDevtoolsMcp,
  targetTriple,
  tauriRuntimeEnvironment,
  pinnedRustToolchainChannel,
  tauriCommandIndex,
  usesValidatedNsisStaging,
  windowsBundleTargets,
  windowsRustHostTriple,
};
