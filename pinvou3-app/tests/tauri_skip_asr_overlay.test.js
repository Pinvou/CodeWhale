// 架构 overlay 里那些「不一定备得出来」的资源，必须按开关正确地摘掉。
//
// 这类约束很容易在重构里悄悄失效，而失效的表现是构建跑了二十多分钟之后才在 bundle
// 阶段报「安装包资源不存在」——反馈太慢，所以钉一条测试在这里。
//
// ASR 运行时写在版本控制的 aarch64 overlay 里（本仓库自己能构建出来），但 SenseVoice
// 按设计只能在目标架构上原生构建（build-sensevoice-runtime.sh 硬断言
// requested_arch == host_arch），交叉构建必须跳过。跳过之后那个文件不存在，所以要用
// 一份摘掉它的配置**顶替**整个 overlay。开关 PINVOU3_SKIP_LINUX_ASR=1，默认不跳。
//
// 下面钉的是**当前实现（顶替整个 overlay）的可观察行为**，不是"只有顶替才对"。
// Tauri 的 --config 是 JSON Merge Patch 语义、null 能删继承键（见 effective-config.js
// 的说明；stage-resources.ps1 在 NSIS 那条链路上就是这么摘资源声明的），所以"追加一层
// 把那条键置 null"同样能达到目的——原先这里写「深合并删不掉键」是错的。
//
// 之所以仍选顶替：null 只删得掉你显式写出来的那条键，哪天架构 overlay 多声明一个同样
// 备不出来的资源，null 层会静默漏掉它，失败又推迟回二十分钟后的 bundle 阶段；顶替是从
// overlay 自己的键里过滤，漏不掉。
//
// 真正不许松的是这条性质：**开关打开后，备不出来的那个文件不得再出现在任何一层生效的
// resources 声明里**。哪天改成 null 删键，下面两条断言（overlay 变内联、文件路径不得
// 并存）该按新形态改写，但那条性质本身不许变。
const assert = require("node:assert");
const test = require("node:test");

const { prepareTauriArgs } = require("../scripts/tauri/build.js");

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
  });
});
