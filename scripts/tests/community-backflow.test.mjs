import assert from "node:assert/strict";
import test from "node:test";

import {
  buildSchedule,
  createProvenance,
  encodeProvenanceMarker,
  parseProvenanceMarker,
  validateChangedPaths,
  verifyPullBody,
} from "../community-backflow.mjs";

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
