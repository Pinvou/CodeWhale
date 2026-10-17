import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { requireCompleteCoverage } from "../../scripts/check-enrollment-gated-v2-rollback.mjs";

const relayDir = dirname(dirname(fileURLToPath(import.meta.url)));
const repoRoot = dirname(relayDir);

test("rollback gate fails closed for empty, incomplete and inconsistent inventories", () => {
  const complete = { total: 2, migrated: 2, not_migrated: 0, coverage_percent: 100 };
  assert.doesNotThrow(() => requireCompleteCoverage(complete));
  for (const candidate of [null, {},
    { ...complete, total: 0, migrated: 0 },
    { ...complete, migrated: 1, not_migrated: 1, coverage_percent: 50 },
    { ...complete, total: 1.5 }, { ...complete, migrated: 1 },
    { ...complete, total: "2" }, { ...complete, coverage_percent: 99 },
    { ...complete, not_migrated: -1 }]) {
    assert.throws(() => requireCompleteCoverage(candidate), { code: "legacy_endpoint_coverage_incomplete" });
  }
});

test("legacy rollback deployment has one guarded public websocket route", async () => {
  const nginx = await readFile(join(relayDir, "nginx-enrollment-gateway.conf"), "utf8");
  assert.match(nginx, /location = \/pinvou3\/remote\/ws/u);
  assert.match(nginx, /proxy_pass http:\/\/127\.0\.0\.1:8790/u);
  assert.doesNotMatch(nginx, /proxy_pass\s+[^;]*8787/u);
  assert.match(nginx, /location = \/pinvou3\/remote\/healthz/u);
});

test("rollback gate composes coverage and every-entrypoint gateway preflight without a shell", async () => {
  const gate = await readFile(join(repoRoot, "scripts", "check-enrollment-gated-v2-rollback.mjs"), "utf8");
  const gatewayPreflight = await readFile(join(relayDir, "scripts", "enrollment-gateway-preflight.js"), "utf8");
  assert.match(gate, /requestEnrollmentCoverage/u);
  assert.doesNotMatch(gate, /run\("enrollment-migrate\.js"/u);
  assert.match(gate, /enrollment-gateway-preflight\.js/u);
  assert.match(gate, /safe_to_run_legacy_v2/u);
  assert.match(gate, /not_migrated/u);
  assert.match(gate, /shell: false/u);
  assert.match(gatewayPreflight, /PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON/u);
  assert.match(gatewayPreflight, /PINVOU_REMOTE_LEGACY_RELAY_BIND_HOST/u);
});

test("gateway sample is loopback-only and identifies all explicit rollback inputs", async () => {
  const sample = await readFile(join(relayDir, "enrollment-gateway.env.example"), "utf8");
  assert.match(sample, /PINVOU_REMOTE_GATEWAY_HOST=127\.0\.0\.1/u);
  assert.match(sample, /PINVOU_REMOTE_LEGACY_RELAY_BIND_HOST=127\.0\.0\.1/u);
  assert.match(sample, /PINVOU_REMOTE_HOST=127\.0\.0\.1/u);
  assert.match(sample, /PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON=/u);
  assert.match(sample, /# PINVOU_REMOTE_GATEWAY_CONTROL_HOST=127\.0\.0\.1/u);
  assert.match(sample, /# PINVOU_REMOTE_GATEWAY_CONTROL_PORT=8791/u);
  assert.match(sample, /^# PINVOU_REMOTE_GATEWAY_CONTROL_KEY=$/mu);
  assert.doesNotMatch(sample, /^PINVOU_REMOTE_GATEWAY_CONTROL_/mu);
});
