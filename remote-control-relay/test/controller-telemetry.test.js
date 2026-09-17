"use strict";

import assert from "node:assert/strict";
import test from "node:test";

import {
  ControllerTelemetry,
  bucketLatency,
  normalizeRecord,
} from "../controller-telemetry.js";

// DEV-24：远控遥测只允许受控枚举与聚合计数，任何身份/内容字段必须被拒绝。

test("only allowlisted enums are accepted", () => {
  assert.equal(normalizeRecord({ stage: "register", outcome: "ok" }).stage, "register");
  assert.throws(() => normalizeRecord({ stage: "pairing", outcome: "ok" }), /stage_unknown/);
  assert.equal(normalizeRecord({ stage: "claim", outcome: "busy", reason: "slot_conflict" }).reason, "slot_conflict");
  assert.throws(() => normalizeRecord({ stage: "claim", outcome: "maybe" }), /outcome_unknown/);
  assert.throws(() => normalizeRecord({ stage: "claim", outcome: "ok", reason: "because" }), /reason_unknown/);
  assert.throws(
    () => normalizeRecord({ stage: "claim", outcome: "ok", latency_bucket: "instant" }),
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
      () => normalizeRecord({ stage: "task", outcome: "ok", [field]: "x" }),
      /field_forbidden/,
      `${field} must be rejected`,
    );
  }
  assert.throws(() => normalizeRecord({ stage: "task", outcome: "ok", note: "x" }), /field_unknown/);
  assert.doesNotThrow(() => normalizeRecord({ stage: "task", outcome: "ok", count: 3 }));
  assert.throws(() => normalizeRecord({ stage: "task", outcome: "ok", count: -1 }), /count_invalid/);
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
  telemetry.record({ stage: "task", outcome: "ok", latency_bucket: "lt_1s" });
  telemetry.record({ stage: "task", outcome: "ok", latency_bucket: "lt_1s", count: 2 });
  telemetry.record({ stage: "resume", outcome: "rejected", reason: "resume_failed" });
  const snapshot = telemetry.snapshot();
  assert.equal(snapshot.total, 4);
  const keys = Object.keys(snapshot.counters);
  assert.ok(keys.includes("task|ok|none|lt_1s"));
  assert.ok(keys.includes("resume|rejected|resume_failed|lt_50ms"));
  const encoded = JSON.stringify(snapshot);
  assert.ok(!/endpoint|lease|token|code|device/i.test(encoded));
});

test("duplicate delivery is dropped once and bounded", () => {
  const telemetry = new ControllerTelemetry();
  assert.ok(telemetry.record({ stage: "claim", outcome: "ok" }, "client-event-1"));
  assert.equal(telemetry.record({ stage: "claim", outcome: "ok" }, "client-event-1"), null);
  assert.equal(telemetry.snapshot().total, 1);
  for (let index = 0; index < 5000; index += 1) {
    telemetry.record({ stage: "claim", outcome: "ok" }, `client-event-${index}`);
  }
  assert.ok(telemetry.dedupe.size <= 4096);
});

test("reset clears every aggregate", () => {
  const telemetry = new ControllerTelemetry();
  telemetry.record({ stage: "artifact", outcome: "ok" });
  telemetry.reset();
  assert.equal(telemetry.snapshot().total, 0);
  assert.deepEqual(telemetry.snapshot().counters, {});
});
