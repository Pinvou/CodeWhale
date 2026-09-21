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

test("targetArchitecture 认得两种写法，不认的回 null", () => {
  assert.equal(targetArchitecture(["build", "--target", "aarch64-unknown-linux-gnu"]), "arm64");
  assert.equal(targetArchitecture(["build", "--target=x86_64-unknown-linux-gnu"]), "x64");
  assert.equal(targetArchitecture(["build"]), null);
  // 不认识的三元组必须回 null，让调用方回落到宿主架构，而不是猜一个。
  assert.equal(targetArchitecture(["build", "--target=riscv64gc-unknown-linux-gnu"]), null);
});

// ---- 摘资源声明 ----

const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

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
