// 交叉构建的一致性闸门，以及两个「摘资源声明」的手法。
//
// 评审指出的根因是同一个：有几步只会按**宿主**架构备料，交叉构建时它们要么产出
// 错架构的东西、要么什么都不产出，而**两种都不报错**——包静默地错，用户要到设备
// 上用到那个功能才发现。所以这里钉四件事：闸门拦得住、`main()` 真的用上了闸门和
// 跳过开关、摘得掉、别误伤。
//
//（为什么「摘」用顶替整份 overlay 而不是叠一层 null，论证在 build.js 的
// `configWithoutResources` 头注释里，唯一一份，这里不复述。）
const assert = require("node:assert/strict");
const test = require("node:test");

const {
  architectureConfigWithoutAsr,
  configWithoutResources,
  crossBuildViolations,
  main,
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
  // 最后一次出现生效，与 cargo 的覆盖语义一致；两种写法必须参加同一场
  // 「谁在后面谁赢」，不能各算各的。
  assert.equal(
    targetArchitecture([
      "build",
      "--target",
      "x86_64-unknown-linux-gnu",
      "--target=aarch64-unknown-linux-gnu",
    ]),
    "arm64",
  );
  assert.equal(
    targetArchitecture([
      "build",
      "--target=aarch64-unknown-linux-gnu",
      "--target",
      "x86_64-unknown-linux-gnu",
    ]),
    "x64",
  );
  // 没传 --target 就是原生构建，回 null 让调用方用 process.arch，原行为不变。
  assert.equal(targetArchitecture(["build"]), null);
});

test("--target 没跟值：报「少了值」，不要把下一个 flag 当三元组", () => {
  // 原先 `--target --bundles deb` 会把 "--bundles" 当三元组，机器段解析成
  // "--bundles"，报出来的是「机器段「--bundles」不在架构映射表里」——指着一个根本
  // 不存在的架构问题，查半天才发现只是少打了个值。
  for (const args of [
    ["build", "--target"],
    ["build", "--target", "--bundles", "deb"],
    ["build", "--target="],
  ]) {
    assert.throws(
      // 解析阶段就该拒，跟宿主是什么平台/架构无关，所以这里不传 options。
      () => targetArchitecture(args),
      (error) => {
        assert.match(error.message, /--target/u, `要点名是哪个参数：${error.message}`);
        assert.ok(
          !error.message.includes("架构映射表"),
          `不能把它伪装成架构不支持：${error.message}`,
        );
        return true;
      },
    );
  }
});

// 不认识的机器段原先也回 null。但闸门对 `!targetArch` 是**直接放行**的，于是
// overlay、ASR、共享知识库服务会全部回落宿主架构，产出的正是闸门要防的那种
// 「主程序是目标架构、侧车全是宿主架构」的静默错包——只是绕开了闸门这个入口。
// 所以这里改成硬失败，并把「什么时候才算确实在交叉构建」钉住，免得误伤。
const LINUX_X64 = { platform: "linux", hostArch: "x64" };

test("Linux 宿主交叉构建到不认识的机器段：硬失败，不静默回落宿主架构", () => {
  // 反例选几乎不会进词表的机器段：riscv64 是相对主流的目标，哪天真被支持，
  // 这组「不认识」反例会自己撞红，还得回头改测试。
  for (const triple of ["loongarch64-unknown-linux-gnu", "armv7-unknown-linux-gnueabihf"]) {
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
  // 白名单只对 darwin 宿主生效：Linux 上传这个三元组 cargo 编不出 macOS 产物，
  // 这条构建路径本来就不存在，不能给它开一条绕过未知架构硬失败的口子。
  assert.throws(
    () => targetArchitecture(
      ["build", "--target=universal-apple-darwin"],
      LINUX_X64,
    ),
    /universal/,
  );
});

test("硬失败的范围只到「确实在交叉构建」为止", () => {
  const unknown = ["build", "--target=loongarch64-unknown-linux-gnu"];
  // 非 Linux 宿主：那几步「只按宿主架构备料」的侧车不在这条打包路径上，没有要防的错包。
  assert.equal(targetArchitecture(unknown, { platform: "win32", hostArch: "x64" }), null);
  assert.equal(targetArchitecture(unknown, { platform: "darwin", hostArch: "arm64" }), null);
  // 宿主自己的 arch 就不在表里：在 loongarch64 机器上编 loongarch64 是**原生**构建，
  // 我们无从判断这是不是交叉，不该越权拦。
  assert.equal(
    targetArchitecture(unknown, { platform: "linux", hostArch: "loongarch64" }),
    null,
  );
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

// ---- main() 的编排 ----
//
// 上面所有用例测的都是纯函数。纯函数全对、`main()` 里没接上，包照样静默地错：
// 评审实测把 `if (violations.length > 0) throw` 整段删掉、把
// `if (!skipKnowledgeHost) prepareKnowledgeHost()` 退化成无条件调用，全部测试照绿。
// 下面这几条就钉这两处：闸门走真 `main()` 的行为断言，备料守卫走源码结构断言。

/**
 * 用指定的宿主平台/架构跑真正的 `main()`，跑完还原全局状态。
 *
 * 为什么敢在进程内直接调 `main()`：闸门前面只有几步纯计算（平台判断、Windows 专属
 * 分支、`targetArchitecture`），Linux + 非 dev 这条路上不 spawn、不写盘，闸门抛出时
 * 什么副作用都还没发生。闸门**后面**的场景不能这么测——那边会真的下 Node tarball、
 * 跑 cargo，一跑就是几十分钟。
 *
 * `process.platform` / `process.arch` 只能改全局，`main()` 直接读它们、没有注入口。
 * node --test 每个测试文件跑在独立子进程里，本文件内部用 finally 还原即可。
 * @param {string[]} argv 命令行参数（不含 node 与脚本名）。
 * @param {{platform: string, arch: string, env?: Record<string, string|undefined>}} host 宿主环境。
 * @returns {Promise<number>} `main()` 的返回值。
 */
async function runMainAs(argv, { platform, arch, env = {} }) {
  const savedArgv = process.argv;
  const savedPlatform = Object.getOwnPropertyDescriptor(process, "platform");
  const savedArch = Object.getOwnPropertyDescriptor(process, "arch");
  const savedEnv = Object.fromEntries(
    Object.keys(env).map((name) => [name, process.env[name]]),
  );
  try {
    process.argv = [process.execPath, "build.js", ...argv];
    Object.defineProperty(process, "platform", { value: platform, configurable: true });
    Object.defineProperty(process, "arch", { value: arch, configurable: true });
    for (const [name, value] of Object.entries(env)) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
    return await main();
  } finally {
    process.argv = savedArgv;
    Object.defineProperty(process, "platform", savedPlatform);
    Object.defineProperty(process, "arch", savedArch);
    for (const [name, value] of Object.entries(savedEnv)) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
  }
}

// 伪装的宿主架构**故意跟真机相反**，目标三元组则指向真机那个架构。
//
// 这么挑有两层用意：
//   1. 目标 ≠ 宿主，构成交叉构建，闸门该拦；
//   2. 万一闸门被删掉、流程漏到 `prepareLinuxAsrRuntime()`，它会按伪装的架构去跑
//      `scripts/asr/build-sensevoice-runtime.sh`，而那个脚本开头第一件事就是
//      `requested_arch == host_arch` 的硬断言（在任何下载和编译之前），于是几毫秒内
//      非零退出、抛出另一条完全不同的错——本用例照样红，而且**不会**把 CI 挂住。
//      直接写死 x64 的话，在 x86_64 机器上删掉闸门会真的去编 SenseVoice，一挂几十分钟。
const REAL_MACHINE_IS_ARM = process.arch === "arm64";
const FAKE_HOST_ARCH = REAL_MACHINE_IS_ARM ? "x64" : "arm64";
const CROSS_ARGV = [
  "build",
  "--target",
  REAL_MACHINE_IS_ARM ? "aarch64-unknown-linux-gnu" : "x86_64-unknown-linux-gnu",
];
const CROSS_TARGET_ARCH = REAL_MACHINE_IS_ARM ? "arm64" : "x64";

test("main() 真的会因为闸门终止，不只是算出了 violations 就扔掉", async () => {
  // 把 `if (violations.length > 0) throw` 整段删掉，这条就会跑过闸门、一路走到
  // prepareLinuxAsrRuntime()（那一步按上面的设计会立刻以另一条错误失败），拿不到下面
  // 这几条文案——所以它钉得住那段删除。
  await assert.rejects(
    () => runMainAs(CROSS_ARGV, {
      platform: "linux",
      arch: FAKE_HOST_ARCH,
      env: { PINVOU3_SKIP_LINUX_ASR: undefined, PINVOU3_SKIP_KNOWLEDGE_HOST: undefined },
    }),
    (error) => {
      assert.match(error.message, /交叉构建/u, `要说明是交叉构建配置不完整：${error.message}`);
      assert.match(error.message, /PINVOU3_SKIP_LINUX_ASR/u, error.message);
      assert.match(error.message, /PINVOU3_SKIP_KNOWLEDGE_HOST/u, error.message);
      // 宿主和目标都要点出来，否则光看报错不知道是哪次构建触发的。
      assert.ok(error.message.includes(FAKE_HOST_ARCH), error.message);
      assert.ok(error.message.includes(CROSS_TARGET_ARCH), error.message);
      return true;
    },
  );
});

test("main() 的闸门认开关：只开一个仍然拦，且只报还缺的那个", async () => {
  // 证明 main() 里那两个 `process.env.PINVOU3_SKIP_*` 真的喂进了闸门，而不是算完就丢。
  //
  // 这里开的是 knowledge 那个、留 ASR 没开，方向是有讲究的：反过来（开 ASR）在闸门
  // 被删掉时会跳过 ASR 直奔 `prepareCodexBridge()`，那一步要下 Node 官方 tarball，
  // 挂住不动；留着 ASR 不开，漏过闸门就会撞上 SenseVoice 的架构硬断言，秒退。
  await assert.rejects(
    () => runMainAs(CROSS_ARGV, {
      platform: "linux",
      arch: FAKE_HOST_ARCH,
      env: { PINVOU3_SKIP_LINUX_ASR: undefined, PINVOU3_SKIP_KNOWLEDGE_HOST: "1" },
    }),
    (error) => {
      assert.match(error.message, /PINVOU3_SKIP_LINUX_ASR/u, error.message);
      assert.ok(
        !error.message.includes("PINVOU3_SKIP_KNOWLEDGE_HOST"),
        `已经开了的开关不该再报：${error.message}`,
      );
      return true;
    },
  );
});

// 闸门「别误伤原生构建」那一面没法这么测：放行之后 main() 会真的去下 Node tarball、
// 跑 cargo，一跑几十分钟。那一面由上面 `crossBuildViolations` 的纯函数用例覆盖。

test("闸门和两步备料的守卫必须留在 main() 里，不能退化成无条件执行", () => {
  // 这三处被删/被退化时的共同表现是**包静默地错**：闸门没了就放任交叉构建继续，
  // `prepareKnowledgeHost()` 无条件调用则让 `cargo build`（不带 --target）编出宿主
  // 架构的 server，被原样打进另一个架构的包，构建全程零警告——用户要到设备上用共享
  // 知识库时才发现。
  //
  // 行为级地测备料那两处要真的跑备料，所以这里退一步做源码结构断言，写法参照上面
  // 那条「开关全文件只读两处」。断的是「守卫跟调用在同一行」，等价重构（改名、调
  // 顺序）不误伤，退化成无条件调用则必红。
  const mainBody = BUILD_SCRIPT_SOURCE.match(/\r?\nasync function main\(\) \{\r?\n([\s\S]*?)\r?\n\}\r?\n/u);
  assert.ok(mainBody, "build.js 必须有 main() 编排入口");

  // 闸门本身：上面那两条行为用例已经钉住它，这里再钉一遍结构，是为了让「整段删掉」
  // 这种改法立刻红在一条文案清楚的断言上，而不是绕到别的用例里去。
  assert.match(
    mainBody[1],
    /if \(violations\.length > 0\) \{[\s\S]*?throw new Error\(/u,
    "main() 必须在 violations 非空时抛错终止，光算出 violations 不用等于没有闸门",
  );

  for (const [step, guard] of [
    ["prepareLinuxAsrRuntime", "!skipLinuxAsr"],
    ["prepareKnowledgeHost", "!skipKnowledgeHost"],
  ]) {
    // dev 那次 `prepareKnowledgeHost({ development: true })` 不在闸门管辖范围内，
    // 它不进安装包；只看打包路径上那次不带 development 的调用。
    const packagingCalls = mainBody[1]
      .split("\n")
      .filter((line) => line.includes(`${step}(`) && !line.includes("development"));
    assert.equal(
      packagingCalls.length,
      1,
      `main() 的打包路径上应当只有一处 ${step}() 调用，实际 ${packagingCalls.length} 处`,
    );
    assert.ok(
      packagingCalls[0].includes(`if (${guard})`),
      `${step}() 必须由 ${guard} 守住，否则交叉构建会静默备一份宿主架构的料：`
        + packagingCalls[0].trim(),
    );
  }
});

// ---- 摘资源声明 ----

// 整个文件共用一个临时目录，跑完删掉。原先每个 fixture 各 mkdtemp 一个、谁都不删，
// 跑一次在 tmp 里留 5 个目录。
const TEMP_ROOT = fs.mkdtempSync(path.join(os.tmpdir(), "xbuild-"));
test.after(() => fs.rmSync(TEMP_ROOT, { recursive: true, force: true }));

let tempConfigSeq = 0;

/**
 * 写一份临时 overlay 配置，返回绝对路径。
 * @param {object} content 配置内容。
 * @param {string} [name] 文件名，只影响报错文案里出现的名字。
 * @returns {string} 配置文件的绝对路径。
 */
function writeTempConfig(content, name = "tauri.conf.json") {
  // 每份各自一个子目录：`configWithoutResources` 的报错里带文件名，同名文件放一起
  // 会让「报的是哪一份」变得说不清。
  const dir = path.join(TEMP_ROOT, `c${(tempConfigSeq += 1)}`);
  fs.mkdirSync(dir);
  const file = path.join(dir, name);
  fs.writeFileSync(file, typeof content === "string" ? content : JSON.stringify(content));
  return file;
}

function writeConfig(resources) {
  return writeTempConfig({ bundle: { resources, targets: ["deb"] } });
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
  const file = writeTempConfig({ productName: "x" }, "c.json");
  assert.equal(configWithoutResources(file, ["whatever"]), file);
});

test("JSON 坏了要报出是哪个文件", () => {
  const file = writeTempConfig("{ not json", "broken.json");
  // 原来直接抛 SyntaxError，只有一句 "Unexpected token"——而这里读的是按平台/
  // 架构拼出来的路径，出错时最需要知道的恰恰是文件名。
  assert.throws(() => configWithoutResources(file, ["x"]), (e) => e.message.includes("broken.json"));
});
