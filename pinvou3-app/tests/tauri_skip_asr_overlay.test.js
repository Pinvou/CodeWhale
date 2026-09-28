// 架构 overlay 里那些「不一定备得出来」的资源，必须按开关正确地摘掉。
//
// ASR 运行时写在版本控制的 aarch64 overlay 里（本仓库自己能构建出来），但 SenseVoice
// 按设计只能在目标架构上原生构建（build-sensevoice-runtime.sh 硬断言
// requested_arch == host_arch），交叉构建必须跳过。跳过之后那个文件不存在，而 overlay
// 里还声明着它，`writeEffectiveArtifacts` 会以「安装包资源不存在」终止。开关
// PINVOU3_SKIP_LINUX_ASR=1，默认不跳。
//
// 为什么摘的手法是「顶替整个 overlay」而不是「叠一层把那条键置 null」，论证在
// build.js 的 `configWithoutResources` 头注释里，唯一一份，这里不复述。
//
// 下面钉的是**当前实现的可观察行为**，不是「只有顶替才对」。真正不许松的是这条性质：
// **开关打开后，备不出来的那个文件不得再出现在任何一层生效的 resources 声明里**。
// 哪天改成 null 删键，下面两条断言（overlay 变内联、文件路径不得并存）该按新形态
// 改写，但那条性质本身不许变。
const assert = require("node:assert");
const fs = require("node:fs");
const test = require("node:test");

const { prepareTauriArgs } = require("../scripts/tauri/build.js");
const { platformArchitectureConfigPath } = require("../scripts/tauri/platform-config.js");

/** 从 prepareTauriArgs 的产物里挑出全部 --config 项（内联 JSON 或路径）。 */
function overlayEntries(args) {
  const specs = [];
  for (let i = 0; i < args.length; i += 1) {
    if (args[i] === "--config" && args[i + 1]) specs.push(args[i + 1]);
  }
  return specs;
}

/** 全部内联 overlay 里声明的资源源路径。 */
function inlineResourceSources(args) {
  const sources = [];
  for (const spec of overlayEntries(args)) {
    if (!spec.trim().startsWith("{")) continue;
    sources.push(...Object.keys(JSON.parse(spec)?.bundle?.resources || {}));
  }
  return sources;
}

const BASE = ["build", "--target", "aarch64-unknown-linux-gnu"];
const OPTS = { platform: "linux", architecture: "x64", stageRuntime: () => null };
const SWITCHES = ["PINVOU3_SKIP_LINUX_ASR"];

/**
 * 在一组指定的开关下跑 prepareTauriArgs，跑完把环境还原。
 *
 * 开关统一存档还原，而不是各测各的：它们作用在同一批 --config 上，漏还原一个就会
 * 让后面的用例读到上一个用例的残留，表现是「单跑全过、一起跑就红」。
 * @param {Record<string, string|undefined>} env 要设置的开关（缺省即删除）。
 * @param {(run: () => string[]) => void} body 断言体，调用 run() 拿参数。
 */
function withSwitches(env, body) {
  const saved = Object.fromEntries(SWITCHES.map((name) => [name, process.env[name]]));
  try {
    for (const name of SWITCHES) {
      if (env[name] === undefined) delete process.env[name];
      else process.env[name] = env[name];
    }
    body(() => prepareTauriArgs(BASE, OPTS));
  } finally {
    for (const name of SWITCHES) {
      if (saved[name] === undefined) delete process.env[name];
      else process.env[name] = saved[name];
    }
  }
}

test("非 Linux 平台：开关打开也不动 overlay 链", () => {
  // 与 tauri_skip_knowledge_host_overlay.test.js 的对称用例同一件事：ASR 顶替
  // 的 `platform === "linux"` 守卫丢了，macOS/Windows 的 overlay 也会被读进来
  // 重写一遍——当前 marker 在那些文件里恰好命中不了，这种「靠 marker 恰好不命中」
  // 的无害是会过期的，所以钉住「非 Linux 上一条内联顶替项都不该出现」。
  // win32 没有架构 overlay（ARCHITECTURE_CONFIG_NAMES 只有 linux），平台 overlay
  // 必须原样走文件路径。
  const saved = process.env.PINVOU3_SKIP_LINUX_ASR;
  try {
    process.env.PINVOU3_SKIP_LINUX_ASR = "1";
    const specs = overlayEntries(
      prepareTauriArgs(["build"], { platform: "win32", stageRuntime: () => null }),
    );
    assert.ok(specs.length > 0, "win32 构建仍应注入平台 overlay");
    for (const spec of specs) {
      assert.ok(
        !spec.trim().startsWith("{"),
        `非 Linux 平台上不该出现内联顶替项：${spec.slice(0, 48)}`,
      );
    }
  } finally {
    if (saved === undefined) delete process.env.PINVOU3_SKIP_LINUX_ASR;
    else process.env.PINVOU3_SKIP_LINUX_ASR = saved;
  }
});

test("默认情况下架构 overlay 走文件路径，ASR 资源照常声明", () => {
  withSwitches({}, (run) => {
    const args = run();
    const specs = overlayEntries(args);
    const archOverlay = specs.find((s) => s.includes("aarch64"));
    assert.ok(archOverlay, "应该注入了 aarch64 的架构 overlay");
    assert.ok(!archOverlay.trim().startsWith("{"), "不跳过时应该原样用配置文件路径");
    // 内联 overlay 里不该冒出 ASR 资源（那说明顶替逻辑在不该生效时生效了）
    assert.deepEqual(
      inlineResourceSources(args).filter((s) => s.includes("linux-asr-runtime")),
      [],
    );
  });
});

test("PINVOU3_SKIP_LINUX_ASR=1 时，架构 overlay 被顶替且 ASR 资源被摘掉", () => {
  withSwitches({ PINVOU3_SKIP_LINUX_ASR: "1" }, (run) => {
    const args = run();
    const specs = overlayEntries(args);
    const inlineArch = specs.find((s) => s.trim().startsWith("{") && s.includes("bundle"));
    assert.ok(inlineArch, "跳过 ASR 时架构 overlay 应该被顶替成内联 JSON");
    assert.ok(
      !Object.keys(JSON.parse(inlineArch).bundle.resources).some((s) =>
        s.includes("linux-asr-runtime"),
      ),
      "ASR 资源必须被摘掉",
    );
    // 顶替不是追加：不能还留着指向配置文件的那一项，否则深合并会把 ASR 并回来
    assert.ok(
      !specs.some((s) => !s.trim().startsWith("{") && s.includes("aarch64")),
      "架构 overlay 的文件路径必须被替换掉，不能与内联版本并存",
    );
    // fixture 非空洞自检（与 tauri_skip_knowledge_host_overlay.test.js 那条对称）：
    // 哪天 aarch64 overlay 里本来就没有 ASR 声明了，上面「摘掉了」的断言会因为
    // 「本来就没有」而白白变绿，这条测试等于没断言。
    const original = JSON.parse(
      fs.readFileSync(platformArchitectureConfigPath("linux", "arm64"), "utf8"),
    );
    assert.ok(
      Object.keys(original?.bundle?.resources || {}).some((s) => s.includes("linux-asr-runtime")),
      "aarch64 架构 overlay 里本来就该有 ASR 声明，否则这条测试等于没断言",
    );
  });
});

test("ASR 的 marker 只摘 ASR 那条，不能摘得更宽", () => {
  // marker 是**子串**匹配。把它放宽（比如 `linux-asr-runtime` 放宽成 `linux-asr`）
  // 会把所有架构 overlay 里名字沾边的声明一起误摘，而上面走 prepareTauriArgs 的
  // 用例看不见这种事：纳入版本控制的 overlay 里恰好没有近似的键。这份合成的
  // overlay 就是来当这个「擦边证人」的。用例走生产包装函数，保证这里钉住的
  // marker 就是构建真正用的那一个。
  const { architectureConfigWithoutAsr } = require("../scripts/tauri/build.js");
  const os = require("node:os");
  const path = require("node:path");
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou3-asr-marker-"));
  try {
    const overlay = path.join(dir, "tauri.conf.json");
    const resources = {
      "target/linux-asr-runtime/aarch64/sense-voice-main": "runtime/asr/sense-voice-main",
      "target/linux-asr-models/config.json": "runtime/asr/config.json",
      "resources/platforms/linux/codex-bridge/": "runtime/codex-bridge",
    };
    fs.writeFileSync(overlay, JSON.stringify({ bundle: { resources } }));
    const result = JSON.parse(architectureConfigWithoutAsr(overlay));
    assert.deepEqual(
      Object.keys(result.bundle.resources),
      ["target/linux-asr-models/config.json", "resources/platforms/linux/codex-bridge/"],
      "只许摘掉 linux-asr-runtime 那条声明",
    );
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
