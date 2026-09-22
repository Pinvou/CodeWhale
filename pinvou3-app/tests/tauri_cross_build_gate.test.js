// 交叉构建的一致性闸门，以及两个「摘资源声明」的手法。
//
// 评审指出的根因是同一个：有几步只会按**宿主**架构备料，交叉构建时它们要么产出
// 错架构的东西、要么什么都不产出，而**两种都不报错**——包静默地错，用户要到设备
// 上用到那个功能才发现。所以这里钉三件事：闸门拦得住、摘得掉、别误伤。
const assert = require("node:assert/strict");
const test = require("node:test");

const {
  architectureConfigWithoutAsr,
  configWithoutResources,
  crossBuildViolations,
  platformConfigWithoutKnowledgeHost,
  targetArchitecture,
} = require("../scripts/tauri/build.js");

const cross = (over = {}) => ({
  platform: "linux",
  targetArch: "arm64",
  hostArch: "x64",
  skipLinuxAsr: false,
  skipKnowledgeHost: false,
  ...over,
});

test("交叉构建：两个开关都没开，两条都要报", () => {
  assert.equal(crossBuildViolations(cross()).length, 2);
});

test("交叉构建：只开 ASR 忘开 knowledge —— 正是评审指出的那个漏洞", () => {
  const v = crossBuildViolations(cross({ skipLinuxAsr: true }));
  assert.equal(v.length, 1);
  assert.ok(v[0].includes("PINVOU3_SKIP_KNOWLEDGE_HOST"), `文案要指出该设哪个开关：${v[0]}`);
});

test("交叉构建：两个都开才放行", () => {
  assert.deepEqual(crossBuildViolations(cross({ skipLinuxAsr: true, skipKnowledgeHost: true })), []);
});

test("不是交叉构建就不该拦", () => {
  // 同架构：原生构建这两步都是对的，拦了等于把默认路径堵死。
  assert.deepEqual(crossBuildViolations(cross({ targetArch: "x64" })), []);
  // 没传 --target：调用方回落到 process.arch，也是原生构建。
  assert.deepEqual(crossBuildViolations(cross({ targetArch: null })), []);
  // 非 Linux：这两步本来就不在那条路径上。
  assert.deepEqual(crossBuildViolations(cross({ platform: "win32" })), []);
  assert.deepEqual(crossBuildViolations(cross({ platform: "darwin" })), []);
});

test("targetArchitecture 认得两种写法", () => {
  assert.equal(targetArchitecture(["build", "--target", "aarch64-unknown-linux-gnu"]), "arm64");
  assert.equal(targetArchitecture(["build", "--target=x86_64-unknown-linux-gnu"]), "x64");
  // 没传 --target 就是原生构建，回 null 让调用方用 process.arch，原行为不变。
  assert.equal(targetArchitecture(["build"]), null);
});

// 不认识的机器段原先也回 null。但闸门对 `!targetArch` 是**直接放行**的，于是
// overlay、ASR、共享知识库服务会全部回落宿主架构，产出的正是闸门要防的那种
// 「主程序是目标架构、侧车全是宿主架构」的静默错包——只是绕开了闸门这个入口。
// 所以这里改成硬失败，并把「什么时候才算确实在交叉构建」钉住，免得误伤。
const LINUX_X64 = { platform: "linux", hostArch: "x64" };

test("Linux 宿主交叉构建到不认识的机器段：硬失败，不静默回落宿主架构", () => {
  for (const triple of ["riscv64gc-unknown-linux-gnu", "armv7-unknown-linux-gnueabihf"]) {
    assert.throws(
      () => targetArchitecture(["build", `--target=${triple}`], LINUX_X64),
      (error) => {
        // 文案三件事：拒的是哪个三元组、为什么拒、怎么绕过去。
        assert.ok(error.message.includes(triple), `要点名三元组：${error.message}`);
        assert.ok(
          error.message.includes(triple.split("-")[0]),
          `要点名不认识的机器段：${error.message}`,
        );
        assert.ok(
          error.message.includes("TARGET_MACHINE_ARCHITECTURES"),
          `要给出补进映射表这条出路：${error.message}`,
        );
        return true;
      },
    );
  }
});

test("universal-apple-darwin 必须继续回落宿主架构", () => {
  // 通用二进制一次编出两种架构，本来就没有单一「目标架构」可言；把它当成不认识
  // 而拒绝，等于把 macOS 打包整条路堵死。
  assert.equal(
    targetArchitecture(
      ["build", "--target=universal-apple-darwin"],
      { platform: "darwin", hostArch: "arm64" },
    ),
    null,
  );
});

test("硬失败的范围只到「确实在交叉构建」为止", () => {
  const unknown = ["build", "--target=riscv64gc-unknown-linux-gnu"];
  // 非 Linux 宿主：那几步「只按宿主架构备料」的侧车不在这条打包路径上，没有要防的错包。
  assert.equal(targetArchitecture(unknown, { platform: "win32", hostArch: "x64" }), null);
  assert.equal(targetArchitecture(unknown, { platform: "darwin", hostArch: "arm64" }), null);
  // 宿主自己的 arch 就不在表里：在 riscv64 机器上编 riscv64 是**原生**构建，
  // 我们无从判断这是不是交叉，不该越权拦。
  assert.equal(targetArchitecture(unknown, { platform: "linux", hostArch: "riscv64" }), null);
  // 没传 --target 时根本不必判断机器段。
  assert.equal(targetArchitecture(["build"], LINUX_X64), null);
});

// ---- 开关的读取点 ----

const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const BUILD_SCRIPT_SOURCE = fs.readFileSync(
  path.join(__dirname, "..", "scripts", "tauri", "build.js"),
  "utf8",
);

test("两个跳过开关由 main() 算一次再显式传下去", () => {
  // 闸门和 overlay 摘除必须用同一份结论。原先 main() 和 prepareTauriArgs 各读一次
  // env，中间还隔着若干 await——两处一旦读出两种结论，表现是「声明摘了但东西照样
  // 构建」或者反过来「东西没构建但声明还在」，后者让 bundle 报「安装包资源不存在」。
  const call = BUILD_SCRIPT_SOURCE.match(
    /const preparedArgs = prepareTauriArgs\(args, \{[\s\S]*?\n {2}\}\);/u,
  );
  assert.ok(call, "main() 必须经 prepareTauriArgs 组装 Tauri 参数");
  assert.match(call[0], /\bskipLinuxAsr,/u, "main() 必须把算好的 skipLinuxAsr 传下去");
  assert.match(call[0], /\bskipKnowledgeHost,/u, "main() 必须把算好的 skipKnowledgeHost 传下去");
  for (const name of ["PINVOU3_SKIP_LINUX_ASR", "PINVOU3_SKIP_KNOWLEDGE_HOST"]) {
    assert.equal(
      BUILD_SCRIPT_SOURCE.split(`process.env.${name}`).length - 1,
      2,
      `${name} 全文件只该有两处读取：main() 入口算一次，外加 prepareTauriArgs 给`
        + "不经 main() 的调用方兜底的默认值。多出来的读取点都是分裂的来源。",
    );
  }
});

// ---- 摘资源声明 ----

function writeConfig(resources) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "xbuild-"));
  const file = path.join(dir, "tauri.conf.json");
  fs.writeFileSync(file, JSON.stringify({ bundle: { resources, targets: ["deb"] } }));
  return file;
}

test("摘掉 ASR 的那条，其余原样留下", () => {
  const file = writeConfig({
    "target/linux-asr-runtime/aarch64/sense-voice-main": "runtime/asr/sense-voice-main",
    "resources/platforms/linux/codex-bridge/": "runtime/codex-bridge",
  });
  const out = JSON.parse(architectureConfigWithoutAsr(file));
  assert.deepEqual(Object.keys(out.bundle.resources), ["resources/platforms/linux/codex-bridge/"]);
  assert.deepEqual(out.bundle.targets, ["deb"], "bundle 里其它字段不能丢");
});

test("摘掉 knowledge-host 的那条", () => {
  const file = writeConfig({
    "resources/platforms/linux/knowledge-host/": "runtime/knowledge-host",
    "resources/platforms/linux/codex-bridge/": "runtime/codex-bridge",
  });
  const out = JSON.parse(platformConfigWithoutKnowledgeHost(file));
  assert.deepEqual(Object.keys(out.bundle.resources), ["resources/platforms/linux/codex-bridge/"]);
});

test("没有可摘的就原样返回路径，不白生成一份内联 JSON", () => {
  const file = writeConfig({ "resources/platforms/linux/codex-bridge/": "runtime/codex-bridge" });
  assert.equal(platformConfigWithoutKnowledgeHost(file), file);
  assert.equal(architectureConfigWithoutAsr(file), file);
});

test("配置里根本没有 bundle.resources 时不炸", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "xbuild-"));
  const file = path.join(dir, "c.json");
  fs.writeFileSync(file, JSON.stringify({ productName: "x" }));
  assert.equal(configWithoutResources(file, ["whatever"]), file);
});

test("JSON 坏了要报出是哪个文件", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "xbuild-"));
  const file = path.join(dir, "broken.json");
  fs.writeFileSync(file, "{ not json");
  // 原来直接抛 SyntaxError，只有一句 "Unexpected token"——而这里读的是按平台/
  // 架构拼出来的路径，出错时最需要知道的恰恰是文件名。
  assert.throws(() => configWithoutResources(file, ["x"]), (e) => e.message.includes("broken.json"));
});
