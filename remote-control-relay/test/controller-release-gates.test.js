"use strict";

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// DEV-25：把发布/回滚门禁固化成可执行断言，避免 runbook 与代码漂移。
// 这里只做静态事实校验 + 关键约束存在性检查，真实双机旅程仍需人工执行。

const read = (relative) => readFileSync(new URL(`../${relative}`, import.meta.url), "utf8");
const runbook = read("../docs/Windows双端远程控制发布与回滚.md");
const migrationHandler = read("enrollment-migration-handler.js");
const registry = read("enrollment-registry.js");
const gateway = read("enrollment-gateway.js");
const nginxGateway = read("nginx-enrollment-gateway.conf");

test("runbook enumerates every required section", () => {
  for (const heading of [
    "## 1. 发布前置门禁",
    "## 2. 灰度步骤",
    "## 3. 真实双机 E2E 旅程",
    "## 4. 监控与阈值",
    "## 5. 回滚",
    "## 6. 残余风险与声明",
  ]) {
    assert.ok(runbook.includes(heading), `runbook must keep "${heading}"`);
  }
});

test("runbook keeps the release gates and evidence commands", () => {
  for (const command of [
    "npm --prefix remote-control-relay test",
    "cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml remote_control::",
    "npm --prefix pinvou3-app run build:ui",
    "npm --prefix pinvou3-app run test:webui",
    "python scripts/architecture-guard.py",
    "controller-release-gates.test.js",
  ]) {
    assert.ok(runbook.includes(command), `runbook must keep the gate: ${command}`);
  }
});

test("rollback never deletes durable enrollment or tombstone state", () => {
  // The runbook states the invariant; the code must not contain a destructive
  // rollback path either.
  assert.ok(runbook.includes("不回退 state schema"));
  assert.ok(runbook.includes("不删除 registry"));
  for (const source of [migrationHandler, registry]) {
    assert.ok(!/deleteAll|clearRegistry|dropRegistry|wipeState/i.test(source));
  }
  // A bare v2 rollback (no gateway) must remain impossible.
  assert.ok(runbook.includes("裸 v2 回滚"));
  assert.ok(nginxGateway.includes("proxy_pass"), "gateway config must remain a real proxy");
  assert.ok(gateway.includes("preflight") || gateway.includes("verify"), "gateway must expose a verification path");
});

test("sensitive Rust evidence names existing exact tests and rejects zero matches", () => {
  for (const [module, name] of [
    ["relay_runtime", "log_allowlist_rejects_identifier_shaped_values_and_secrets"],
    ["relay_runtime", "real_remote_failure_projections_do_not_persist_any_sensitive_category"],
    ["resume", "resume_debug_redacts_current_and_retired_material"],
  ]) {
    const source = read(`../pinvou3-app/src-tauri/src/features/remote_control/controller/${module}.rs`);
    assert.match(source, /#\[cfg\(test\)\]\s*mod tests\s*\{/);
    assert.match(source, new RegExp(`#\\[test\\]\\s*fn ${name}\\(`));
    assert.ok(runbook.includes(`features::remote_control::controller::${module}::tests::${name} -- --exact --test-threads=1`));
  }
  assert.ok(!runbook.includes("cargo test ... controller::telemetry"));
  assert.ok(runbook.includes("1 passed; 0 failed"));
  assert.ok(runbook.includes("零匹配视为失败"));
  assert.ok(runbook.includes("controller-sensitive-scan.test.js"));
});

test("the trusted-Relay residual risk stays declared", () => {
  assert.ok(runbook.includes("不声称抵御恶意 Relay 的主动 MITM"));
  assert.ok(runbook.includes("受信 Relay"));
});

test("the runbook keeps the 18.3 journeys and the risky ones are called out", () => {
  for (const marker of ["旅程 9～10、19、21", "旅程 13", "旅程 16", "旅程 20"]) {
    assert.ok(runbook.includes(marker), `runbook must keep ${marker}`);
  }
  assert.ok(runbook.includes("24000ms / 19000ms"));
});

test("telemetry thresholds stay inside the DEV-24 enum", () => {
  const telemetry = read("controller-telemetry.js");
  for (const stage of ["claim", "handshake", "liveness", "resume", "task", "attachment"]) {
    assert.ok(telemetry.includes(`"${stage}"`), `telemetry must keep the ${stage} stage`);
  }
  // The runbook may only reference aggregate counters, never identity fields.
  assert.ok(!/pairing_code\s*[:=]/.test(runbook));
  assert.ok(!/lease_id\s*[:=]/.test(runbook));
});
