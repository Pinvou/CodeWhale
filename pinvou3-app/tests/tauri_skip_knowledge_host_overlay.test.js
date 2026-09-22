// 跳过共享知识库服务时，平台 overlay 里那条资源声明必须跟着被摘掉。
//
// 与 `tauri_skip_asr_overlay.test.js` 对称的一条：那边钉的是**架构**层的 ASR 声明，
// 这边钉的是**平台**层的 `knowledge-host/` 声明。两条都只有纯函数单测是不够的——
// 纯函数对了、接线错了（开关名写串、Linux 守卫丢了、忘了把摘过的那份换回去），
// 表现同样是**不报错**：
//   - 干净树：目录不存在，资源枚举出零个文件，包里静默缺 server；
//   - 脏树：目录里躺着上次**宿主架构**构建留下的 server，被原样打进另一个架构的包。
// 两种都要等用户在设备上用到共享知识库才暴露。所以这里走真正的 `prepareTauriArgs`，
// 从它吐出来的 `--config` 列表上做断言。
const assert = require("node:assert/strict");
const fs = require("node:fs");
const test = require("node:test");

const { configSpecs, prepareTauriArgs } = require("../scripts/tauri/build.js");
const { platformConfigPath } = require("../scripts/tauri/platform-config.js");

const BASE = ["build", "--target", "aarch64-unknown-linux-gnu"];
const LINUX = { platform: "linux", architecture: "x64", stageRuntime: () => null };
const SWITCH = "PINVOU3_SKIP_KNOWLEDGE_HOST";

/** overlay 既可能是文件路径也可能是内联 JSON 串，用开头的 `{` 区分。 */
const isInline = (spec) => spec.trim().startsWith("{");

/** 某个 overlay 声明的全部资源源路径；文件路径版本不在这里展开。 */
function inlineResourceSources(spec) {
  return isInline(spec) ? Object.keys(JSON.parse(spec)?.bundle?.resources || {}) : [];
}

/**
 * 在指定开关下跑一段断言，跑完还原环境。
 *
 * 开关必须存档还原：它作用在同一批 `--config` 上，漏还原一次就会让后面的用例读到
 * 上一个用例的残留，表现是「单跑全过、一起跑就红」。
 * @param {string|undefined} value 开关值，缺省即删除该变量。
 * @param {() => void} body 断言体。
 */
function withSwitch(value, body) {
  const saved = process.env[SWITCH];
  try {
    if (value === undefined) delete process.env[SWITCH];
    else process.env[SWITCH] = value;
    body();
  } finally {
    if (saved === undefined) delete process.env[SWITCH];
    else process.env[SWITCH] = saved;
  }
}

test("默认不跳过：平台 overlay 原样走文件路径，knowledge-host 照常声明", () => {
  withSwitch(undefined, () => {
    const specs = configSpecs(prepareTauriArgs(BASE, LINUX));
    assert.equal(
      specs[0],
      platformConfigPath("linux"),
      "平台 overlay 必须排在最前、且不跳过时原样用配置文件路径",
    );
    // 内联 overlay 里也不该冒出 knowledge-host（那说明顶替在不该生效时生效了）。
    assert.deepEqual(
      specs.flatMap(inlineResourceSources).filter((source) => source.includes("knowledge-host")),
      [],
    );
  });
});

test("skipKnowledgeHost=true：平台 overlay 被顶替，只摘掉 knowledge-host 那条", () => {
  withSwitch(undefined, () => {
    // 显式传参——这正是 main() 的接线方式：开关只在入口读一次，再传下来。
    const specs = configSpecs(prepareTauriArgs(BASE, { ...LINUX, skipKnowledgeHost: true }));
    const platformOverlay = specs[0];
    assert.ok(isInline(platformOverlay), "跳过时平台 overlay 应该被顶替成内联 JSON");

    // 顶替不是追加：文件路径版不能与内联版并存，否则合并时那条声明又被并回来。
    assert.ok(
      !specs.includes(platformConfigPath("linux")),
      "平台 overlay 的文件路径必须被替换掉，不能与内联版本并存",
    );

    // 顶替整份配置最大的风险是**丢键**，所以逐层比对原文件：只能少 knowledge-host
    // 那一条资源，其余任何字段都得原样留下。
    const original = JSON.parse(fs.readFileSync(platformConfigPath("linux"), "utf8"));
    const replaced = JSON.parse(platformOverlay);
    assert.deepEqual(Object.keys(replaced), Object.keys(original), "顶层字段不能丢");
    assert.deepEqual(Object.keys(replaced.bundle), Object.keys(original.bundle), "bundle 字段不能丢");
    assert.deepEqual(
      Object.keys(replaced.bundle.resources),
      Object.keys(original.bundle.resources).filter((source) => !source.includes("knowledge-host")),
      "除 knowledge-host 外的资源声明必须逐条、按原顺序留下",
    );
    assert.ok(
      Object.keys(original.bundle.resources).some((source) => source.includes("knowledge-host")),
      "平台 overlay 里本来就该有 knowledge-host 声明，否则这条测试等于没断言",
    );
  });
});

test("非 Linux 平台：开关打开也不动平台 overlay", () => {
  withSwitch("1", () => {
    // knowledge-host 只在 Linux 打包路径上，守卫丢了会把 Windows 的平台 overlay
    // 也顶替一遍——那份里根本没有这条声明，白白把配置文件换成内联串。
    const specs = configSpecs(
      prepareTauriArgs(["build"], { ...LINUX, platform: "win32" }),
    );
    assert.equal(specs[0], platformConfigPath("win32"));
  });
});

test("不经 main() 的调用方：环境变量兜底仍然生效", () => {
  withSwitch("1", () => {
    const specs = configSpecs(prepareTauriArgs(BASE, LINUX));
    assert.ok(isInline(specs[0]), "没显式传参时应回落到 PINVOU3_SKIP_KNOWLEDGE_HOST");
    assert.deepEqual(
      inlineResourceSources(specs[0]).filter((source) => source.includes("knowledge-host")),
      [],
    );
  });
  withSwitch("0", () => {
    // 只认 "1"：别的值一律当没开，免得 `PINVOU3_SKIP_KNOWLEDGE_HOST=0` 反而跳过。
    assert.equal(configSpecs(prepareTauriArgs(BASE, LINUX))[0], platformConfigPath("linux"));
  });
});
