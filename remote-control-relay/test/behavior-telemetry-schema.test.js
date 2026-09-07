import assert from "node:assert/strict";
import test from "node:test";

import {
  BEHAVIOR_EVENT_NAMES,
  BEHAVIOR_EVENT_CONTRACTS,
  BEHAVIOR_TELEMETRY_SCHEMA,
  validateBehaviorEventShape,
} from "../behavior-telemetry-schema.js";

const NOW = Date.parse("2026-08-27T09:00:00Z");

function taskFinished(overrides = {}) {
  return {
    schema_version: 1,
    event_id: "beh_test_task_finished_001",
    event_name: "task_finished",
    event_source: "client",
    occurred_at: NOW,
    collection_period_id: "period_test_001",
    producer: "pinvou_agent",
    producer_version: "0.8.7",
    app_version: "0.8.7",
    platform: "windows",
    arch: "x86_64",
    channel: "dev",
    locale: "zh-CN",
    session_id: "session_test_001",
    attempt_id: "attempt_test_001",
    turn_id: "turn_test_001",
    mode: "assistant",
    initiator_type: "user",
    input_type: "text",
    has_attachment: false,
    status: "success",
    duration_ms: 1234,
    ...overrides,
  };
}

test("v1 schema freezes every P0/P1 event and excludes deferred P2 events", () => {
  assert.equal(BEHAVIOR_TELEMETRY_SCHEMA.$schema, "https://json-schema.org/draft/2020-12/schema");
  assert.equal(BEHAVIOR_EVENT_NAMES.size, Object.keys(BEHAVIOR_EVENT_CONTRACTS).length);
  assert(BEHAVIOR_EVENT_NAMES.has("task_submitted"));
  assert(BEHAVIOR_EVENT_NAMES.has("response_copied"));
  assert(!BEHAVIOR_EVENT_NAMES.has("payment_succeeded"));
  assert(!BEHAVIOR_EVENT_NAMES.has("experiment_exposed"));
});

test("validates required fields, controlled enums and server-owned fields", () => {
  assert.deepEqual(validateBehaviorEventShape(taskFinished(), { now: NOW }), { ok: true });
  assert.deepEqual(validateBehaviorEventShape(taskFinished({ mode: "other" }), { now: NOW }), {
    ok: false, reason: "invalid_field", field: "mode",
  });
  assert.deepEqual(validateBehaviorEventShape(taskFinished({ device_id: "device_test_001" }), { now: NOW }), {
    ok: false, reason: "invalid_field", field: "device_id",
  });
  const missing = taskFinished();
  delete missing.attempt_id;
  assert.deepEqual(validateBehaviorEventShape(missing, { now: NOW }), {
    ok: false, reason: "invalid_field", field: "attempt_id",
  });
});

test("rejects unknown, oversized, stale and future events independently", () => {
  assert.deepEqual(validateBehaviorEventShape(taskFinished({ schema_version: 2 }), { now: NOW }), {
    ok: false, reason: "unsupported_schema", field: "schema_version",
  });
  assert.deepEqual(validateBehaviorEventShape(taskFinished({ event_name: "payment_succeeded" }), { now: NOW }), {
    ok: false, reason: "unknown_event", field: "event_name",
  });
  assert.deepEqual(validateBehaviorEventShape(taskFinished({ occurred_at: NOW - 36 * 86400000 }), { now: NOW }), {
    ok: false, reason: "too_old", field: "occurred_at",
  });
  assert.deepEqual(validateBehaviorEventShape(taskFinished({ occurred_at: NOW + 300001 }), { now: NOW }), {
    ok: false, reason: "invalid_time", field: "occurred_at",
  });
  assert.deepEqual(validateBehaviorEventShape(taskFinished({ unexpected: "x" }), { now: NOW }), {
    ok: false, reason: "invalid_field", field: "unexpected",
  });
  assert.deepEqual(validateBehaviorEventShape(taskFinished({ event_id: `beh_${"x".repeat(9000)}` }), { now: NOW }), {
    ok: false, reason: "too_large",
  });
});

test("enforces failure and mutable feedback conditions", () => {
  assert.deepEqual(validateBehaviorEventShape(taskFinished({ status: "failed" }), { now: NOW }), {
    ok: false, reason: "invalid_field", field: "error_category",
  });
  const feedback = {
    ...taskFinished(),
    event_name: "response_feedback_changed",
    feedback_id: "feedback_test_001",
    feedback_action: "set",
  };
  assert.deepEqual(validateBehaviorEventShape(feedback, { now: NOW }), {
    ok: false, reason: "invalid_field", field: "rating",
  });
});
