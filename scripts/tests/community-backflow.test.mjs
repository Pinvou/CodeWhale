import assert from "node:assert/strict";
import test from "node:test";

import {
  buildSchedule,
  createProvenance,
  encodeProvenanceMarker,
  githubRequest,
  githubToken,
  parseProvenanceMarker,
  parseQueueIssues,
  validateChangedPaths,
  verifyPullBody,
} from "../community-backflow.mjs";

test("Issue marker 可以闭环聚合为调度输入", () => {
  const queue = parseQueueIssues([
    {
      number: 601,
      body: '<!-- community-backflow-plan:{"schema":1,"source":241,"depends_on":[],"release":false} -->',
    },
    {
      number: 602,
      body: '<!-- community-backflow-plan:{"schema":1,"source":242,"depends_on":[241],"release":true} -->',
    },
  ]);

  assert.deepEqual(queue, [
    { source: 241, issue: 601, paths: [], release: false, depends_on: [] },
    { source: 242, issue: 602, paths: [], release: true, depends_on: [241] },
  ]);
  assert.deepEqual(buildSchedule(queue).waves, [[241], [242]]);

  assert.deepEqual(parseQueueIssues([{
    number: 603,
    body: '<!-- community-backflow-plan:{"schema":1,"source":243,"depends_on":[241],"release":false} -->',
  }])[0].depends_on, [], "已关闭的前置 Issue 应在重新聚合时视为完成");
});

test("缺少环境 Token 时不再隐式选择个人 GCM 账号", () => {
  const githubTokenValue = process.env.GITHUB_TOKEN;
  const ghTokenValue = process.env.GH_TOKEN;
  delete process.env.GITHUB_TOKEN;
  delete process.env.GH_TOKEN;
  try {
    assert.throws(() => githubToken({}), /显式传入 --credential/);
  } finally {
    if (githubTokenValue === undefined) delete process.env.GITHUB_TOKEN;
    else process.env.GITHUB_TOKEN = githubTokenValue;
    if (ghTokenValue === undefined) delete process.env.GH_TOKEN;
    else process.env.GH_TOKEN = ghTokenValue;
  }
});

test("GitHub API 只读请求对临时 5xx 自动重试", { concurrency: false }, async () => {
  const originalFetch = globalThis.fetch;
  const originalDelay = process.env.COMMUNITY_BACKFLOW_RETRY_BASE_MS;
  let attempts = 0;
  process.env.COMMUNITY_BACKFLOW_RETRY_BASE_MS = "1";
  globalThis.fetch = async () => {
    attempts += 1;
    return attempts === 1
      ? new Response("temporary", { status: 503 })
      : new Response('{"ok":true}', { status: 200 });
  };
  try {
    assert.deepEqual(await githubRequest("test-token", "GET", "/test"), { ok: true });
    assert.equal(attempts, 2);
  } finally {
    globalThis.fetch = originalFetch;
    if (originalDelay === undefined) delete process.env.COMMUNITY_BACKFLOW_RETRY_BASE_MS;
    else process.env.COMMUNITY_BACKFLOW_RETRY_BASE_MS = originalDelay;
  }
});

test("GitHub API 写请求遇到模糊网络结果时统一禁止盲重试", { concurrency: false }, async () => {
  const originalFetch = globalThis.fetch;
  const attempts = new Map();
  globalThis.fetch = async (_url, options) => {
    const method = options.method;
    attempts.set(method, (attempts.get(method) || 0) + 1);
    throw new TypeError("socket closed after request body was sent");
  };
  try {
    for (const method of ["POST", "PATCH", "DELETE"]) {
      await assert.rejects(
        githubRequest("test-token", method, "/test", { value: 1 }),
        /网络结果不明确.*避免重复写入不会自动重试/u,
      );
      assert.equal(attempts.get(method), 1, `${method} 不得透明重试`);
    }
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("调度器并行无依赖项并串行化重叠路径", () => {
  const plan = buildSchedule([
    { source: 10, issue: 100, paths: ["src/a.js"] },
    { source: 11, issue: 101, paths: ["src/b.js"] },
    { source: 12, issue: 102, paths: ["src/a.js"] },
  ]);

  assert.deepEqual(plan.waves, [[10, 11], [12]]);
  assert.deepEqual(plan.items.find((item) => item.source === 12).depends_on, [10]);
});

test("Release 回流排在同批次功能回流之后", () => {
  const plan = buildSchedule([
    { source: 20, paths: ["src/a.js"] },
    { source: 21, paths: ["docs/readme.md"] },
    { source: 22, paths: ["VERSION"], release: true },
  ]);

  assert.deepEqual(plan.waves, [[20, 21], [22]]);
  assert.deepEqual(plan.items.at(-1).depends_on, [20, 21]);
});

test("较早合并的 Release 仍在较晚功能回流之后处理", () => {
  const plan = buildSchedule([
    { source: 23, paths: ["VERSION", "src/a.js"], release: true },
    { source: 24, paths: ["src/a.js"] },
    { source: 25, paths: ["src/b.js"] },
  ]);

  assert.deepEqual(plan.waves, [[24, 25], [23]]);
});

test("调度器拒绝依赖环", () => {
  assert.throws(
    () => buildSchedule([
      { source: 30, depends_on: [31] },
      { source: 31, depends_on: [30] },
    ]),
    /依赖存在环/,
  );
});

test("调度器拒绝无效来源编号", () => {
  assert.throws(() => buildSchedule([{ source: -1 }]), /唯一的正整数 source/);
  assert.throws(
    () => buildSchedule([{ source: 1, depends_on: ["invalid"] }]),
    /包含无效依赖/,
  );
});

test("三平台路径规则拒绝大小写冲突和 Windows 保留名", () => {
  const errors = validateChangedPaths(["src/File.js", "src/file.js", "docs/CON.txt"]);
  assert.equal(errors.some((error) => error.includes("大小写")), true);
  assert.equal(errors.some((error) => error.includes("跨平台不安全")), true);
  assert.deepEqual(validateChangedPaths(["src/正常文件.js", "docs/readme.md"]), []);
});

test("provenance 使用 UTF-8 安全 marker 并校验实际处理平台", () => {
  const provenance = createProvenance({
    base: "a".repeat(40),
    head: "b".repeat(40),
    source: 42,
    paths: ["文档/说明.md"],
    platform: "win32",
  });
  const marker = encodeProvenanceMarker(provenance);
  const body = `回流说明\n\n<!-- community-backflow:Pinvou/pinvou-agent#42 -->\n${marker}\n`;

  assert.deepEqual(parseProvenanceMarker(body), provenance);
  assert.equal(
    verifyPullBody({ body, expectedHead: "b".repeat(40), expectedSource: 42 }).processor_os,
    "windows",
  );
});

test("provenance 必须绑定当前 PR head", () => {
  const provenance = createProvenance({
    base: "a".repeat(40),
    head: "b".repeat(40),
    source: 42,
    paths: [],
    platform: "darwin",
  });
  const body = `<!-- community-backflow:Pinvou/pinvou-agent#42 -->\n${encodeProvenanceMarker(provenance)}`;
  assert.throws(
    () => verifyPullBody({ body, expectedHead: "c".repeat(40) }),
    /与 PR head .* 不一致/,
  );
});
