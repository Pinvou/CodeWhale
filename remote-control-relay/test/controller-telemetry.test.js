"use strict";

import assert from "node:assert/strict";
import test from "node:test";

import {
  ControllerTelemetry,
  bucketLatency,
  normalizeRecord,
  normalizeSnapshot,
} from "../controller-telemetry.js";

// DEV-24：远控遥测只允许受控枚举与聚合计数，任何身份/内容字段必须被拒绝。

test("only allowlisted enums are accepted", () => {
  const claim = { event: "claim_reserved", stage: "claim", outcome: "ok" };
  assert.equal(normalizeRecord(claim).stage, "claim");
  assert.throws(() => normalizeRecord({ ...claim, event: "private_event" }), /event_unknown/);
  assert.throws(() => normalizeRecord({ ...claim, stage: "pairing" }), /stage_unknown/);
  assert.throws(() => normalizeRecord({ ...claim, outcome: "maybe" }), /outcome_unknown/);
  assert.throws(() => normalizeRecord({ ...claim, reason: "because" }), /reason_unknown/);
  assert.throws(() => normalizeRecord({ ...claim, stage: "task" }), /event_mismatch/);
  assert.throws(
    () => normalizeRecord({ ...claim, latency_bucket: "instant" }),
    /latency_unknown/,
  );
});

test("identity, content and credential fields are rejected outright", () => {
  for (const field of [
    "endpoint_id",
    "lease_id",
    "resume_token",
    "pairing_code",
    "api_key",
    "device_name",
    "prompt",
    "file_path",
    "remote_addr",
    "content",
  ]) {
    assert.throws(
      () => normalizeRecord({ event: "task_forwarded", stage: "task", outcome: "ok", [field]: "x" }),
      /field_forbidden/,
      `${field} must be rejected`,
    );
  }
  const task = { event: "task_forwarded", stage: "task", outcome: "ok" };
  assert.throws(() => normalizeRecord({ ...task, note: "x" }), /field_unknown/);
  assert.doesNotThrow(() => normalizeRecord({ ...task, count: 3 }));
  assert.throws(() => normalizeRecord({ ...task, count: -1 }), /count_invalid/);
});

test("latency buckets are bounded and monotone", () => {
  assert.equal(bucketLatency(0), "lt_50ms");
  assert.equal(bucketLatency(49), "lt_50ms");
  assert.equal(bucketLatency(50), "lt_250ms");
  assert.equal(bucketLatency(999), "lt_1s");
  assert.equal(bucketLatency(5000), "lt_30s");
  assert.equal(bucketLatency(30000), "gte_30s");
  assert.equal(bucketLatency(Number.NaN), "gte_30s");
});

test("counters aggregate without keeping identities", () => {
  const telemetry = new ControllerTelemetry();
  telemetry.record({ event: "task_forwarded", stage: "task", outcome: "ok", latency_bucket: "lt_1s" });
  telemetry.record({ event: "task_forwarded", stage: "task", outcome: "ok", latency_bucket: "lt_1s", count: 2 });
  telemetry.record({ event: "resume_rejected", stage: "resume", outcome: "rejected", reason: "resume_failed" });
  const snapshot = telemetry.snapshot();
  assert.equal(snapshot.total, 4);
  const keys = Object.keys(snapshot.counters);
  assert.ok(keys.includes("task_forwarded|task|ok|none|lt_1s"));
  assert.ok(keys.includes("resume_rejected|resume|rejected|resume_failed|lt_50ms"));
  const encoded = JSON.stringify(snapshot);
  assert.ok(!/endpoint|lease|token|code|device/i.test(encoded));
});

test("fixed lifecycle hooks and encrypted payload kinds produce only controlled audit records", () => {
  const captured = [];
  const telemetry = new ControllerTelemetry({ auditSink: (record) => captured.push(record) });
  telemetry.audit("claim_reserved", 80);
  telemetry.auditBusiness("permission_decide");
  telemetry.auditBusiness("artifact_download_chunk", false);
  assert.deepEqual(captured.map(({ stage, outcome, latency_bucket }) => ({
    stage, outcome, latency_bucket,
  })), [
    { stage: "claim", outcome: "ok", latency_bucket: "lt_250ms" },
    { stage: "permission", outcome: "ok", latency_bucket: "lt_50ms" },
    { stage: "artifact", outcome: "rejected", latency_bucket: "lt_50ms" },
  ]);
  assert.throws(() => telemetry.audit("user_supplied_event"), /audit_event_unknown/);
  assert.throws(() => telemetry.auditBusiness("user_supplied_payload"), /audit_payload_unknown/);
  assert.doesNotMatch(JSON.stringify(captured), /endpoint|lease|token|prompt|path|device/i);
});

test("handshake internals and lease completion remain distinct lifecycle counters", () => {
  const telemetry = new ControllerTelemetry();
  for (const event of [
    "handshake_started", "receipt_commitment_accepted",
    "encrypted_finished_forwarded", "finished_receipt_accepted",
    "controller_lease_created", "controller_joined",
  ]) telemetry.audit(event);
  const keys = Object.keys(telemetry.snapshot().counters);
  assert.equal(keys.length, 6);
  assert(keys.some((key) => key.startsWith("controller_lease_created|lease|ok|")));
  assert(keys.some((key) => key.startsWith("controller_joined|lease|ok|")));
});

test("brute-force, capacity and recovery alerts use fixed rolling thresholds", () => {
  let now = 1_000_000;
  const telemetry = new ControllerTelemetry({ now: () => now });
  for (let index = 0; index < 10; index += 1) telemetry.audit("claim_invalid");
  for (let index = 0; index < 5; index += 1) telemetry.audit("claim_busy");
  for (let index = 0; index < 3; index += 1) telemetry.audit("resume_rejected");
  assert.deepEqual(telemetry.snapshot().alerts.map((alert) => alert.code), [
    "pairing_bruteforce_suspected",
    "controller_capacity_pressure",
    "controller_recovery_failure_spike",
  ]);
  now += 5 * 60_000 + 1;
  assert.deepEqual(telemetry.snapshot().alerts, []);
});

test("irrelevant high-volume traffic cannot evict a live brute-force alert", () => {
  const telemetry = new ControllerTelemetry({ now: () => 1_000_000 });
  for (let index = 0; index < 10; index += 1) telemetry.audit("claim_invalid");
  for (let index = 0; index < 4096; index += 1) telemetry.audit("task_forwarded");
  assert(telemetry.snapshot().alerts.some((alert) => alert.code === "pairing_bruteforce_suspected"));
  assert.equal(telemetry.alertSampleCount(), 10);
  for (let index = 0; index < 5000; index += 1) telemetry.audit("claim_invalid");
  assert(telemetry.alertSampleCount() <= 4096);
});

test("stats boundary revalidates snapshots and rejects high-cardinality projections", () => {
  const telemetry = new ControllerTelemetry();
  telemetry.audit("controller_lease_created");
  assert.deepEqual(normalizeSnapshot(telemetry.snapshot()), telemetry.snapshot());
  assert.throws(() => normalizeSnapshot({
    schema_version: 1,
    counters: { "controller_lease_created|lease|ok|none|lt_50ms": 1 },
    alerts: [],
    total: 1,
    endpoint_id: "endpoint-private",
  }), /snapshot_invalid/);
  assert.throws(() => normalizeSnapshot({
    schema_version: 1,
    counters: { "controller_lease_created|lease|ok|none|lt_50ms|endpoint-private": 1 },
    alerts: [],
    total: 1,
  }), /snapshot_invalid/);
  assert.throws(() => normalizeSnapshot({
    schema_version: 1,
    counters: {},
    alerts: [{
      code: "pairing_bruteforce_suspected",
      severity: "warning",
      window_seconds: 60,
      count: 10,
      threshold: 9,
    }],
    total: 0,
  }), /snapshot_invalid/);
});

test("reset clears every aggregate", () => {
  const telemetry = new ControllerTelemetry();
  telemetry.audit("artifact_forwarded");
  telemetry.reset();
  assert.equal(telemetry.snapshot().total, 0);
  assert.deepEqual(telemetry.snapshot().counters, {});
});
