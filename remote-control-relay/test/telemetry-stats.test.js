import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Readable } from "node:stream";
import test from "node:test";

import {
  TelemetryStore,
  buildBehaviorModelRanking,
  buildBehaviorOverview,
  buildWeeklyExplicitValueMetric,
  buildBehaviorSceneRanking,
  buildBehaviorToolRanking,
  buildCommercialMock,
  buildDeviceList,
  buildStatsOverview,
  createTelemetryService,
  formatDeviceRegion,
  parseIp2Region,
} from "../telemetry-service.js";

const at = (value) => Date.parse(`${value}+08:00`);

function behaviorEvent(collectionPeriodId, overrides = {}) {
  return {
    schema_version: 1,
    event_id: "behavior_event_test_000001",
    event_name: "task_submitted",
    event_source: "client",
    occurred_at: Date.now(),
    collection_period_id: collectionPeriodId,
    producer: "pinvou_agent",
    producer_version: "0.8.7",
    app_version: "0.8.7",
    platform: "windows",
    arch: "x86_64",
    channel: "dev",
    locale: "zh-CN",
    session_id: "session_test_000001",
    attempt_id: "attempt_test_000001",
    mode: "assistant",
    initiator_type: "user",
    input_type: "text",
    has_attachment: false,
    ...overrides,
  };
}

async function serviceRequest(service, method, path, body, headers = {}) {
  const request = Readable.from(body === undefined ? [] : [Buffer.from(JSON.stringify(body))]);
  request.method = method;
  request.url = path;
  request.headers = headers;
  request.socket = { remoteAddress: "127.0.0.1" };
  let status;
  let responseHeaders;
  let payload = "";
  const response = {
    writeHead(value, values) {
      status = value;
      responseHeaders = values;
    },
    end(value = "") {
      payload = String(value);
    },
  };
  await service.handleHttp(request, response, new URL(path, "http://localhost").pathname, {
    clientIp: "127.0.0.1",
  });
  return {
    status,
    headers: responseHeaders,
    body: payload ? JSON.parse(payload) : null,
  };
}

test("dashboard metrics use unique devices and 30 complete Beijing calendar days", () => {
  const now = at("2026-07-14T12:00:00");
  const devices = [
    {
      device_id: "dev_a",
      first_seen_at: at("2026-07-06T10:00:00"),
      last_seen_at: now - 10_000,
      last_active_at: at("2026-07-14T11:00:00"),
      app_version: "1.0.0",
      platform: "linux",
      state: "online",
      region: {
        country: "中国",
        province: "广东省",
        city: "深圳市",
        country_code: "CN",
      },
    },
    {
      device_id: "dev_b",
      first_seen_at: at("2026-07-13T09:00:00"),
      last_seen_at: now - 360_000,
      last_active_at: at("2026-07-14T10:00:00"),
      app_version: "2.0.0",
      platform: "windows",
      state: "online",
    },
    {
      device_id: "dev_c",
      first_seen_at: at("2026-07-14T08:00:00"),
      last_seen_at: now - 360_000,
      last_active_at: at("2026-07-14T09:00:00"),
      app_version: "1.0.0",
      platform: "linux",
      state: "online",
    },
  ];
  const events = [
    { event_id: "evt_a_d1", device_id: "dev_a", occurred_at: at("2026-07-07T10:00:00"), success: true },
    { event_id: "evt_a_d7", device_id: "dev_a", occurred_at: at("2026-07-13T10:00:00"), success: true },
    { event_id: "evt_a_today", device_id: "dev_a", occurred_at: at("2026-07-14T11:00:00"), success: false },
    { event_id: "evt_b_today", device_id: "dev_b", occurred_at: at("2026-07-14T10:00:00"), success: true },
    { event_id: "evt_c_today", device_id: "dev_c", occurred_at: at("2026-07-14T09:00:00"), success: true },
  ];

  const overview = buildStatsOverview(devices, events, now);
  assert.equal(overview.online_window_seconds, 300);
  assert.deepEqual(overview.counts, { online: 1, active_today: 3, active_7d: 3 });
  assert.deepEqual(overview.active_versions, [
    { version: "1.0.0", count: 2 },
    { version: "2.0.0", count: 1 },
  ]);
  assert.equal(overview.usage_trend.length, 30);
  assert.deepEqual(overview.usage_trend.at(-2), {
    day: "2026-07-13",
    active_devices: 1,
    turns: 1,
  });
  assert.deepEqual(overview.usage_trend.at(-1), {
    day: "2026-07-14",
    active_devices: 3,
    turns: 3,
  });

  const list = buildDeviceList(devices, events, now);
  assert.equal(list[0].device_id, "dev_a");
  assert.equal(list[0].first_seen_at, at("2026-07-06T10:00:00"));
  assert.equal(list[0].region, "广东省 / 深圳市");
  assert.equal(list[0].turns_today, 1);
  assert.equal(list[0].turns_7d, 2);
  assert.equal(list[1].turns_today, 1);
  assert.equal(list[1].turns_7d, 1);
  assert.equal(list[1].region, "未知");
  assert.equal("failure_rate_7d" in list[0], false);
});

test("behavior analytics aggregates tasks voice tools installs scenes and models", () => {
  const now = at("2026-08-12T12:00:00");
  const devices = [
    { device_id: "dev_a", last_active_at: at("2026-08-12T09:00:00") },
    { device_id: "dev_b", last_active_at: at("2026-08-12T10:00:00") },
    { device_id: "dev_c", last_active_at: null },
  ];
  const usageEvents = [
    { event_id: "usage_a", device_id: "dev_a", occurred_at: at("2026-08-12T09:00:00") },
    { event_id: "usage_b", device_id: "dev_b", occurred_at: at("2026-08-12T10:00:00") },
  ];
  const behaviorEvents = [
    { event_id: "cohort_a", event_name: "analytics_cohort_started", identity_id: "dev_a", occurred_at: at("2026-08-10T08:00:00") },
    { event_id: "cohort_b", event_name: "analytics_cohort_started", identity_id: "dev_b", occurred_at: at("2026-08-10T08:00:00") },
    { event_id: "task_a", event_name: "task_submitted", attempt_id: "attempt_a", identity_id: "dev_a", mode: "assistant", initiator_type: "user", occurred_at: at("2026-08-10T09:01:00") },
    { event_id: "task_admit_a", event_name: "task_admitted", attempt_id: "attempt_a", identity_id: "dev_a", occurred_at: at("2026-08-10T09:01:01") },
    { event_id: "task_done_a", event_name: "task_finished", attempt_id: "attempt_a", turn_id: "turn_a", identity_id: "dev_a", mode: "assistant", initiator_type: "user", status: "success", occurred_at: at("2026-08-10T09:02:00") },
    { event_id: "copy_a", event_name: "response_copied", turn_id: "turn_a", identity_id: "dev_a", content_type: "text", occurred_at: at("2026-08-10T09:22:00") },
    { event_id: "task_b", event_name: "task_submitted", attempt_id: "attempt_b", identity_id: "dev_b", mode: "code", initiator_type: "user", occurred_at: at("2026-08-10T10:01:00") },
    { event_id: "task_admit_b", event_name: "task_admitted", attempt_id: "attempt_b", identity_id: "dev_b", occurred_at: at("2026-08-10T10:01:01") },
    { event_id: "task_done_b", event_name: "task_finished", attempt_id: "attempt_b", identity_id: "dev_b", status: "interrupted", occurred_at: at("2026-08-10T10:02:00") },
    { event_id: "task_c", event_name: "task_submitted", attempt_id: "attempt_c", identity_id: "dev_c", mode: "assistant", initiator_type: "user", occurred_at: at("2026-08-10T11:01:00") },
    { event_id: "task_reject_c", event_name: "task_rejected", attempt_id: "attempt_c", identity_id: "dev_c", stage: "shutdown", occurred_at: at("2026-08-10T11:01:01") },
    { event_id: "voice_a", event_name: "voice_started", identity_id: "dev_a", occurred_at: at("2026-08-12T09:03:00") },
    { event_id: "voice_c", event_name: "voice_started", identity_id: "dev_c", occurred_at: at("2026-08-12T11:03:00") },
    { event_id: "tool_start_a", event_name: "tool_call_started", operation_id: "op_tool_a", identity_id: "dev_a", tool_key: "search", tool_name: "Search", tool_type: "mcp", occurred_at: at("2026-08-12T09:03:59") },
    { event_id: "tool_a", event_name: "tool_call_completed", operation_id: "op_tool_a", identity_id: "dev_a", tool_key: "search", tool_name: "Search", tool_type: "mcp", success: true, occurred_at: at("2026-08-12T09:04:00") },
    { event_id: "tool_start_b", event_name: "tool_call_started", operation_id: "op_tool_b", identity_id: "dev_b", tool_key: "search", tool_name: "Search", tool_type: "builtin", occurred_at: at("2026-08-12T10:03:59") },
    { event_id: "tool_b", event_name: "tool_call_completed", operation_id: "op_tool_b", identity_id: "dev_b", tool_key: "search", tool_name: "Search", tool_type: "builtin", success: false, occurred_at: at("2026-08-12T10:04:00") },
    { event_id: "install_start_a", event_name: "tool_install_started", operation_id: "op_install_a", identity_id: "dev_a", tool_key: "search", tool_name: "Search", tool_type: "mcp", occurred_at: at("2026-08-12T09:04:59") },
    { event_id: "install_a", event_name: "tool_install_completed", operation_id: "op_install_a", identity_id: "dev_a", tool_key: "search", tool_name: "Search", tool_type: "mcp", success: true, occurred_at: at("2026-08-12T09:05:00") },
    { event_id: "scene_a", event_name: "scene_submitted", identity_id: "dev_a", scene_l1: "work", scene_l2: "document-writing", occurred_at: at("2026-08-12T09:06:00") },
    { event_id: "model_a", event_name: "model_used", identity_id: "dev_a", provider_key: "deepseek", model_key: "deepseek", occurred_at: at("2026-08-12T09:07:00") },
    { event_id: "model_b", event_name: "model_used", identity_id: "dev_b", provider_key: "deepseek", model_key: "deepseek", occurred_at: at("2026-08-12T10:07:00") },
    { event_id: "model_c", event_name: "model_used", identity_id: "dev_a", provider_key: "openai", model_key: "gpt_5", occurred_at: at("2026-08-12T11:07:00") },
  ];

  const overview = buildBehaviorOverview(devices, usageEvents, behaviorEvents, "7", now, [
    { occurred_at: now, accepted: 90, duplicate: 5, rejected: 3, retryable: 2 },
  ]);
  assert.equal(overview.subject, "installation");
  assert.equal(overview.active_installations, 3);
  assert.equal(overview.metrics.task_submitted_count, 3);
  assert.equal(overview.metrics.task_admitted_count, 2);
  assert.equal(overview.metrics.task_rejected_count, 1);
  assert.equal(overview.metrics.task_admission_rate_24h, null);
  assert.equal(overview.metrics.task_rejection_rate_24h, null);
  assert.equal(overview.metrics.task_submission_resolution_rate_24h, null);
  assert.equal(overview.metrics.active_installation_days, 3);
  assert.equal(overview.metrics.task_submitted_per_active_installation_day, 1);
  assert.equal(overview.metrics.task_terminal_completeness_rate, null);
  assert.equal(overview.metrics.task_compensation_rate, null);
  assert.equal(overview.metrics.wsta_installations, 1);
  assert.equal(overview.metrics.wevta_installations, 1);
  assert.equal(overview.metrics.wevta_rate, null);
  assert.equal(overview.metrics.cohort_matured_installations, 2);
  assert.equal(overview.metrics.first_technical_value_24h_installations, 1);
  assert.equal(overview.metrics.first_technical_value_24h_rate, null);
  assert.equal(overview.metrics.tool_call_completion_rate, null);
  assert.equal(overview.metrics.tool_call_success_rate, null);
  assert.equal(overview.metrics.tool_install_completion_rate, null);
  assert.equal(overview.metrics.pipeline_rejection_rate, 0.03);
  assert.equal(overview.metrics.task_success_count, 1);
  assert.equal(overview.metrics.task_interrupted_count, 1);
  assert.equal(overview.metrics.voice_started_installations, 1);
  assert.equal(overview.metrics.voice_observed_installations, 2);
  assert.equal(overview.metrics.voice_penetration_rate, null);

  const tools = buildBehaviorToolRanking(devices, usageEvents, behaviorEvents, "7", "call", now, { minimumGroupSubjects: 0 });
    assert.equal(tools.rows[0].tool_key, "search");
    assert.equal(tools.rows[0].call_count, 2);
    assert.equal(tools.rows[0].user_count, 1);
    assert.equal(tools.rows[0].observed_user_count, 2);
    assert.equal(tools.rows[0].tool_type, "mcp");
    assert.equal(tools.rows[0].success_rate, null);

  const installs = buildBehaviorToolRanking(devices, usageEvents, behaviorEvents, "7", "install", now, { minimumGroupSubjects: 0 });
  assert.equal(installs.rows[0].install_count, 1);

  const scenes = buildBehaviorSceneRanking(devices, usageEvents, behaviorEvents, "7", now, { minimumGroupSubjects: 0 });
  assert.equal(scenes.rows[0].scene_l1, "work");
  assert.equal(scenes.rows[0].trigger_count, 1);

  const models = buildBehaviorModelRanking(devices, usageEvents, behaviorEvents, "7", now, { minimumGroupSubjects: 0 });
  assert.equal(models.rows[0].provider_key, "deepseek");
  assert.equal(models.rows[0].model_key, "deepseek");
  assert.equal(models.rows[0].user_count, 1);
  assert.equal(models.rows[0].observed_user_count, 2);
  assert.equal(models.rows[0].use_count, 2);
  assert.equal(models.rows[1].model_key, "gpt_5");

  const commercial = buildCommercialMock("30", now);
  assert.equal(commercial.available, false);
  assert.equal("metrics" in commercial, false);
});

test("behavior event store validates v1 independently, injects identity and deduplicates", () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-behavior-events-"));
  try {
    const store = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars", {
      eventRatePerMinute: 1_000,
      globalEventRatePerMinute: 2_000,
    });
    const credential = store.register({
      hardware_claim: "behavior-hardware-0001",
      registration_secret: "registration-secret-behavior-00001",
      hardware_source: "test",
      identity_quality: "hardware_serial",
      app_version: "0.5.10",
      platform: "windows",
      arch: "x64",
    }, null);
    const device = store.authenticate(credential.device_id, credential.device_token);
    const collection = store.startBehaviorCollection(device, {
      request_id: "request_behavior_start_000001",
    });
    const result = store.appendBehaviorEvents(device, [
      behaviorEvent(collection.collection_period_id),
      behaviorEvent(collection.collection_period_id),
      behaviorEvent(collection.collection_period_id, {
        event_id: "behavior_event_test_000002",
        device_id: "spoofed_device_test_01",
      }),
      behaviorEvent(collection.collection_period_id, {
        event_id: "behavior_event_test_000003",
        event_name: "unknown_event",
      }),
      behaviorEvent("period_inactive_test_001", {
        event_id: "behavior_event_test_000004",
      }),
      behaviorEvent(collection.collection_period_id, {
        event_id: "behavior_event_test_000005",
        occurred_at: Date.now() - 2 * 60 * 60_000,
      }),
      behaviorEvent(collection.collection_period_id, {
        event_id: "behavior_event_test_000006",
        account_subject_id: "account_subject_test_001",
      }),
    ], "batch_behavior_test_000001");
    assert.equal(result.batch_id, "batch_behavior_test_000001");
    assert.deepEqual(result.results, [
      { event_id: "behavior_event_test_000001", status: "accepted", reason_code: null },
      { event_id: "behavior_event_test_000001", status: "duplicate", reason_code: null },
      { event_id: "behavior_event_test_000002", status: "rejected", reason_code: "invalid_field" },
      { event_id: "behavior_event_test_000003", status: "rejected", reason_code: "unknown_event" },
      { event_id: "behavior_event_test_000004", status: "rejected", reason_code: "collection_inactive" },
      { event_id: "behavior_event_test_000005", status: "accepted", reason_code: null },
      { event_id: "behavior_event_test_000006", status: "rejected", reason_code: "invalid_identity_claim" },
    ]);
    assert.equal(result.results.length, 7);
    assert.equal(store.behaviorEvents.length, 2);
    assert.equal(store.behaviorEvents[0].schema_version, 1);
    assert.equal(store.behaviorEvents[0].app_version, "0.8.7");
    assert.equal(store.behaviorEvents[0].platform, "windows");
    assert.equal(store.behaviorEvents[0].device_id, credential.device_id);
    assert.equal(store.behaviorEvents[0].environment, "production");
    assert.equal(store.behaviorEvents[0].clock_skew_bucket, "normal");
    assert.equal(store.behaviorEvents[1].clock_skew_bucket, "past_1h_plus");
    assert.equal(readFileSync(join(dataDir, "behavior-events.jsonl"), "utf8").trim().split("\n").length, 2);
    assert.deepEqual(store.behaviorPipelineEvents, [{
      occurred_at: store.behaviorPipelineEvents[0].occurred_at,
      accepted: 2,
      duplicate: 1,
      rejected: 4,
      retryable: 0,
    }]);
    assert.equal(existsSync(join(dataDir, "behavior-pipeline.jsonl")), true);
    assert.throws(
      () => store.appendBehaviorEvents(device, Array.from({ length: 51 }, (_, index) => (
        behaviorEvent(collection.collection_period_id, {
          event_id: `behavior_event_limit_${String(index).padStart(6, "0")}`,
        })
      ))),
      (error) => error.status === 413 && error.message === "too_many_events",
    );
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("WEVTA uses the latest feedback state and same-turn 30 minute window", () => {
  const now = at("2026-08-12T12:00:00");
  const successful = (id, identity, turn, time) => ({
    event_id: id,
    event_name: "task_finished",
    attempt_id: `attempt_${id}`,
    turn_id: turn,
    identity_id: identity,
    mode: "assistant",
    initiator_type: "user",
    status: "success",
    occurred_at: at(time),
  });
  const events = [
    successful("a", "dev_a", "turn_a", "2026-08-10T09:00:00"),
    { event_id: "positive_a", event_name: "response_feedback_changed", feedback_id: "feedback_a", feedback_action: "set", rating: "positive", turn_id: "turn_a", identity_id: "dev_a", occurred_at: at("2026-08-10T09:10:00") },
    { event_id: "clear_a", event_name: "response_feedback_changed", feedback_id: "feedback_a", feedback_action: "clear", turn_id: "turn_a", identity_id: "dev_a", occurred_at: at("2026-08-10T09:11:00") },
    successful("b", "dev_b", "turn_b", "2026-08-10T10:00:00"),
    { event_id: "late_copy_b", event_name: "response_copied", turn_id: "turn_b", identity_id: "dev_b", occurred_at: at("2026-08-10T10:31:00") },
    successful("c", "dev_c", "turn_c", "2026-08-10T11:00:00"),
    { event_id: "wrong_turn_c", event_name: "artifact_exported", turn_id: "turn_other", identity_id: "dev_c", occurred_at: at("2026-08-10T11:05:00") },
    successful("d", "dev_d", "turn_d", "2026-08-10T12:00:00"),
    { event_id: "copy_d", event_name: "response_copied", turn_id: "turn_d", identity_id: "dev_d", occurred_at: at("2026-08-10T12:30:00") },
  ];
  assert.deepEqual(buildWeeklyExplicitValueMetric(events, now), {
    wevta_installations: 1,
    weekly_successful_task_installations: 4,
    explicit_value_sample_size: 4,
    explicit_value_confidence: "low",
    wevta_rate: null,
    positive_feedback_installations: 0,
    response_copied_installations: 1,
    artifact_exported_installations: 0,
    negative_feedback_installations: 0,
    negative_feedback_rate: null,
  });
});

test("P1 dashboard metrics cover retention funnels version quality and confidence", () => {
  const now = at("2026-08-12T12:00:00");
  const behaviorEvents = [
    { event_id: "cohort_d1", event_name: "analytics_cohort_started", identity_id: "retained_d1", occurred_at: at("2026-08-10T08:00:00") },
    { event_id: "opened_only", event_name: "app_opened", identity_id: "not_retained", occurred_at: at("2026-08-11T08:00:00") },
    { event_id: "active_d1", event_name: "task_finished", attempt_id: "retention_attempt_d1", mode: "assistant", initiator_type: "user", status: "success", identity_id: "retained_d1", occurred_at: at("2026-08-11T08:00:00") },
    { event_id: "cohort_d7", event_name: "analytics_cohort_started", identity_id: "retained_d7", occurred_at: at("2026-08-04T08:00:00") },
    { event_id: "active_d7", event_name: "task_finished", attempt_id: "retention_attempt_d7", mode: "code", initiator_type: "user", status: "success", identity_id: "retained_d7", occurred_at: at("2026-08-11T08:00:00") },
    { event_id: "cohort_d30", event_name: "analytics_cohort_started", identity_id: "retained_d30", occurred_at: at("2026-07-13T08:00:00") },
    { event_id: "active_d30", event_name: "task_finished", attempt_id: "retention_attempt_d30", mode: "team", initiator_type: "user", status: "success", identity_id: "retained_d30", occurred_at: at("2026-08-12T08:00:00") },
    { event_id: "voice_entry", event_name: "voice_entry_clicked", operation_id: "voice_op", identity_id: "quality_a", occurred_at: at("2026-08-12T09:00:00") },
    { event_id: "voice_permission", event_name: "voice_permission_result", operation_id: "voice_op", voice_permission_status: "granted", identity_id: "quality_a", occurred_at: at("2026-08-12T09:00:01") },
    { event_id: "voice_started", event_name: "voice_started", operation_id: "voice_op", identity_id: "quality_a", occurred_at: at("2026-08-12T09:00:02") },
    { event_id: "voice_recognized", event_name: "voice_recognition_completed", operation_id: "voice_op", identity_id: "quality_a", occurred_at: at("2026-08-12T09:00:03") },
    { event_id: "voice_submitted", event_name: "voice_message_submitted", operation_id: "voice_op", attempt_id: "quality_attempt_0", identity_id: "quality_a", occurred_at: at("2026-08-12T09:00:04") },
  ];
  for (let index = 0; index < 30; index += 1) {
    const attempt = `quality_attempt_${index}`;
    const common = { attempt_id: attempt, identity_id: `quality_${index}`, app_version: "0.8.7", platform: "windows", arch: "x86_64" };
    behaviorEvents.push(
      { ...common, event_id: `submitted_${index}`, event_name: "task_submitted", occurred_at: at("2026-08-12T09:01:00") + index },
      { ...common, event_id: `admitted_${index}`, event_name: "task_admitted", occurred_at: at("2026-08-12T09:01:01") + index },
      { ...common, event_id: `output_${index}`, event_name: "task_first_output", duration_ms: index + 1, occurred_at: at("2026-08-12T09:01:02") + index },
      { ...common, event_id: `finished_${index}`, event_name: "task_finished", turn_id: `turn_${index}`, mode: "assistant", initiator_type: "user", status: "success", duration_ms: (index + 1) * 10, occurred_at: at("2026-08-12T09:01:03") + index },
    );
  }
  const overview = buildBehaviorOverview([], [], behaviorEvents, "30", now);
  assert.deepEqual(overview.retention.d1, { horizon_days: 1, eligible_installations: 3, retained_installations: 1, retention_rate: null, confidence: "low" });
  assert.equal(overview.retention.d7.retained_installations, 1);
  assert.equal(overview.retention.d30.retained_installations, 1);
  assert.equal(overview.funnels.voice.entry_clicked, 1);
  assert.equal(overview.funnels.voice.message_submitted, 1);
  assert.equal(overview.funnels.voice.task_succeeded, 1);
  assert.equal(overview.version_quality[0].confidence, "sufficient");
  assert.equal(overview.version_quality[0].first_output_p50_ms, 15);
  assert.equal(overview.version_quality[0].first_output_p90_ms, 27);
  assert.equal(overview.version_quality[0].total_duration_p90_ms, 270);
  assert.equal(overview.data_quality.confidence, "sufficient");
});

test("P1 tool and scene funnels preserve every stage and value outcome", () => {
  const now = at("2026-08-12T12:00:00");
  const events = [
    { event_id: "tool_impression", event_name: "tool_impression", view_id: "view_a", tool_key: "search", tool_type: "mcp", identity_id: "dev_a", occurred_at: at("2026-08-12T09:00:00") },
    { event_id: "tool_detail", event_name: "tool_detail_opened", view_id: "view_a", tool_key: "search", tool_type: "mcp", identity_id: "dev_a", occurred_at: at("2026-08-12T09:01:00") },
    { event_id: "tool_install_start", event_name: "tool_install_started", operation_id: "install_a", tool_key: "search", tool_type: "mcp", identity_id: "dev_a", occurred_at: at("2026-08-12T09:02:00") },
    { event_id: "tool_install_done", event_name: "tool_install_completed", operation_id: "install_a", tool_key: "search", tool_type: "mcp", success: true, identity_id: "dev_a", occurred_at: at("2026-08-12T09:03:00") },
    { event_id: "tool_config_start", event_name: "tool_configuration_started", operation_id: "config_a", tool_key: "search", tool_type: "mcp", identity_id: "dev_a", occurred_at: at("2026-08-12T09:04:00") },
    { event_id: "tool_config_done", event_name: "tool_configuration_finished", operation_id: "config_a", tool_key: "search", tool_type: "mcp", success: true, identity_id: "dev_a", occurred_at: at("2026-08-12T09:05:00") },
    { event_id: "tool_call_done", event_name: "tool_call_completed", operation_id: "call_a", tool_key: "search", tool_type: "mcp", success: true, identity_id: "dev_a", occurred_at: at("2026-08-12T09:06:00") },
    { event_id: "scene_seen", event_name: "scene_impression", operation_id: "scene_view", scene_l1: "work", scene_l2: "document-writing", identity_id: "dev_a", occurred_at: at("2026-08-12T10:00:00") },
    { event_id: "scene_pick", event_name: "scene_selected", operation_id: "scene_pick", scene_l1: "work", scene_l2: "document-writing", identity_id: "dev_a", occurred_at: at("2026-08-12T10:01:00") },
    { event_id: "scene_submit", event_name: "scene_submitted", operation_id: "scene_submit", attempt_id: "scene_attempt", scene_l1: "work", scene_l2: "document-writing", identity_id: "dev_a", occurred_at: at("2026-08-12T10:02:00") },
    { event_id: "scene_finish", event_name: "task_finished", attempt_id: "scene_attempt", turn_id: "scene_turn", status: "success", duration_ms: 1200, identity_id: "dev_a", occurred_at: at("2026-08-12T10:03:00") },
    { event_id: "scene_copy", event_name: "response_copied", turn_id: "scene_turn", identity_id: "dev_a", occurred_at: at("2026-08-12T10:04:00") },
  ];
  const tools = buildBehaviorToolRanking([], [], events, "today", "install", now, { minimumGroupSubjects: 0 });
  assert.deepEqual(tools.funnel_rows[0], {
    tool_key: "search", tool_name: "search", tool_type: "mcp",
    user_count: 1,
    impression_count: 1, detail_count: 1, install_started_count: 1, install_succeeded_count: 1,
    configuration_started_count: 1, configuration_succeeded_count: 1, call_succeeded_count: 1,
    failed_count: 0, cancelled_count: 0, impression_to_install_rate: null, install_success_rate: null,
    install_to_configuration_rate: null, configuration_success_rate: null, configuration_to_call_rate: null,
    configuration_to_call_p50_ms: 60_000,
    confidence: "low",
  });
  const scenes = buildBehaviorSceneRanking([], [], events, "today", now, { minimumGroupSubjects: 0 });
  assert.equal(scenes.rows[0].selection_rate, null);
  assert.equal(scenes.rows[0].submission_rate, null);
  assert.equal(scenes.rows[0].task_success_rate, null);
  assert.equal(scenes.rows[0].explicit_value_rate, null);
  assert.equal(scenes.rows[0].task_duration_p50_ms, 1200);
  assert.equal(buildBehaviorToolRanking([], [], events, "today", "install", now).funnel_rows.length, 0);
  assert.equal(buildBehaviorSceneRanking([], [], events, "today", now).rows.length, 0);
});

test("retention ignores opens and failures while terminal completeness excludes tasks still within 24h", () => {
  const now = at("2026-08-12T12:00:00");
  const base = [
    { event_id: "cohort_retention", event_name: "analytics_cohort_started", identity_id: "retention_subject", occurred_at: at("2026-08-10T08:00:00") },
    { event_id: "retention_open", event_name: "app_opened", identity_id: "retention_subject", occurred_at: at("2026-08-11T08:00:00") },
    { event_id: "retention_failed", event_name: "task_finished", attempt_id: "failed_retention", mode: "assistant", initiator_type: "user", status: "failed", identity_id: "retention_subject", occurred_at: at("2026-08-11T09:00:00") },
    { event_id: "admitted_recent", event_name: "task_admitted", attempt_id: "recent_attempt", identity_id: "recent_subject", occurred_at: at("2026-08-12T11:00:00") },
  ];
  const recentOnly = buildBehaviorOverview([], [], base, "30", now);
  assert.equal(recentOnly.retention.d1.retained_installations, 0);
  assert.equal(recentOnly.metrics.task_terminal_matured_count, 0);
  assert.equal(recentOnly.metrics.task_terminal_pending_within_24h_count, 1);
  assert(!recentOnly.data_quality.warnings.includes("task_terminal_incomplete"));

  const matured = buildBehaviorOverview([], [], [...base, {
    event_id: "admitted_matured", event_name: "task_admitted", attempt_id: "matured_attempt",
    identity_id: "matured_subject", occurred_at: at("2026-08-10T09:00:00"),
  }], "30", now);
  assert.equal(matured.metrics.task_terminal_matured_count, 1);
  assert.equal(matured.metrics.task_terminal_missing_count, 1);
  assert(matured.data_quality.warnings.includes("task_terminal_incomplete"));
});

test("active and feature-adoption denominators use core submissions and successful active installations", () => {
  const now = at("2026-08-12T12:00:00");
  const events = [];
  for (let index = 0; index < 30; index += 1) {
    const identity = `core_${index}`;
    const attempt = `core_attempt_${index}`;
    events.push(
      { event_id: `submitted_${index}`, event_name: "task_submitted", attempt_id: attempt, identity_id: identity, mode: "assistant", initiator_type: "user", occurred_at: at("2026-08-12T08:00:00") + index },
      { event_id: `finished_${index}`, event_name: "task_finished", attempt_id: attempt, identity_id: identity, mode: "assistant", initiator_type: "user", status: "success", occurred_at: at("2026-08-12T08:10:00") + index },
    );
    if (index < 12) events.push(
      { event_id: `voice_${index}`, event_name: "voice_started", identity_id: identity, occurred_at: at("2026-08-12T09:00:00") + index },
      { event_id: `tool_${index}`, event_name: "tool_call_completed", operation_id: `tool_op_${index}`, tool_key: "search", tool_type: "mcp", success: true, identity_id: identity, occurred_at: at("2026-08-12T09:10:00") + index },
    );
  }
  events.push(
    { event_id: "passive_open", event_name: "app_opened", identity_id: "passive", occurred_at: at("2026-08-12T10:00:00") },
    { event_id: "passive_voice", event_name: "voice_started", identity_id: "passive", occurred_at: at("2026-08-12T10:01:00") },
    { event_id: "passive_tool", event_name: "tool_call_completed", operation_id: "passive_tool_op", tool_key: "search", tool_type: "mcp", success: true, identity_id: "passive", occurred_at: at("2026-08-12T10:02:00") },
    { event_id: "scheduled", event_name: "task_submitted", attempt_id: "scheduled_attempt", identity_id: "scheduled", mode: "scheduled", initiator_type: "system", occurred_at: at("2026-08-12T10:03:00") },
  );
  const overview = buildBehaviorOverview(
    [{ device_id: "passive_device", last_active_at: at("2026-08-12T11:00:00") }],
    [{ event_id: "passive_usage", device_id: "passive_usage", occurred_at: at("2026-08-12T11:00:00") }],
    events,
    "today",
    now,
  );
  assert.equal(overview.active_installations, 30);
  assert.equal(overview.metrics.active_installation_days, 30);
  assert.equal(overview.metrics.task_submitted_per_active_installation_day, 1);
  assert.equal(overview.metrics.wsta_installations, 30);
  assert.equal(overview.metrics.voice_observed_installations, 13);
  assert.equal(overview.metrics.voice_started_installations, 12);
  assert.equal(overview.metrics.voice_penetration_rate, 0.4);

  const tool = buildBehaviorToolRanking([], [], events, "today", "call", now, { minimumGroupSubjects: 0 }).rows[0];
  assert.equal(tool.observed_user_count, 13);
  assert.equal(tool.user_count, 12);
  assert.equal(tool.penetration_sample_size, 30);
  assert.equal(tool.penetration_rate, 0.4);
});

test("WSTA and feature adoption attribute cross-day tasks to their completion day", () => {
  const now = at("2026-08-13T12:00:00");
  const events = [
    {
      event_id: "submitted_before_midnight",
      event_name: "task_submitted",
      attempt_id: "cross_day_attempt",
      identity_id: "cross_day_installation",
      mode: "assistant",
      initiator_type: "user",
      occurred_at: at("2026-08-12T23:59:00"),
    },
    {
      event_id: "finished_after_midnight",
      event_name: "task_finished",
      attempt_id: "cross_day_attempt",
      identity_id: "cross_day_installation",
      mode: "assistant",
      initiator_type: "user",
      status: "success",
      occurred_at: at("2026-08-13T00:01:00"),
    },
    {
      event_id: "voice_after_midnight",
      event_name: "voice_started",
      identity_id: "cross_day_installation",
      occurred_at: at("2026-08-13T00:02:00"),
    },
  ];
  const overview = buildBehaviorOverview([], [], events, "today", now);
  assert.equal(overview.active_installations, 0);
  assert.equal(overview.metrics.task_success_count, 1);
  assert.equal(overview.metrics.wsta_installations, 1);
  assert.equal(overview.metrics.wsta_rate, null);
  assert.equal(overview.metrics.voice_started_installations, 1);
  assert.equal(overview.metrics.voice_observed_installations, 1);
});

test("activation funnel and model ranking expose complete quality dimensions", () => {
  const now = at("2026-08-12T12:00:00");
  const events = [];
  for (let index = 0; index < 30; index += 1) {
    const identity = `activation_${index}`;
    const attempt = `activation_attempt_${index}`;
    const startedAt = at("2026-08-10T08:00:00") + index;
    events.push({ event_id: `cohort_${index}`, event_name: "analytics_cohort_started", identity_id: identity, occurred_at: startedAt });
    if (index < 20) {
      events.push(
        { event_id: `ready_${index}`, event_name: "task_environment_ready", identity_id: identity, occurred_at: startedAt + 60_000 },
        { event_id: `submitted_${index}`, event_name: "task_submitted", attempt_id: attempt, identity_id: identity, occurred_at: startedAt + 120_000 },
        { event_id: `admitted_${index}`, event_name: "task_admitted", attempt_id: attempt, identity_id: identity, occurred_at: startedAt + 180_000 },
        { event_id: `output_${index}`, event_name: "task_first_output", attempt_id: attempt, identity_id: identity, occurred_at: startedAt + 240_000 },
      );
    }
    if (index < 15) events.push({
      event_id: `success_${index}`, event_name: "task_finished", attempt_id: attempt,
      mode: "assistant", initiator_type: "user", status: "success", duration_ms: (index + 1) * 100,
      identity_id: identity, occurred_at: startedAt + 300_000,
    });
    events.push(
      { event_id: `model_${index}`, event_name: "model_used", attempt_id: attempt, provider_key: "deepseek", model_key: "deepseek", selection_source: index < 10 ? "user_selected" : "app_default", identity_id: identity, occurred_at: at("2026-08-12T09:00:00") + index },
      { event_id: `model_finish_${index}`, event_name: "task_finished", attempt_id: attempt, status: index < 24 ? "success" : "failed", duration_ms: (index + 1) * 100, identity_id: identity, occurred_at: at("2026-08-12T10:00:00") + index },
    );
    if (index < 5) events.push({ event_id: `model_selected_${index}`, event_name: "model_selected", provider_key: "deepseek", model_key: "deepseek", selection_scope: "session", identity_id: identity, occurred_at: at("2026-08-12T08:00:00") + index });
  }
  const overview = buildBehaviorOverview([], [], events, "30", now);
  assert.deepEqual(overview.funnels.activation, {
    cohort_installations: 30,
    matured_installations: 30,
    pending_maturity_installations: 0,
    environment_ready_installations: 20,
    task_submitted_installations: 20,
    task_admitted_installations: 20,
    task_first_output_installations: 20,
    task_succeeded_installations: 15,
    activation_rate_24h: 0.5,
    first_value_p50_ms: 300_000,
    first_value_p75_ms: 300_000,
    first_value_p90_ms: 300_000,
    confidence: "sufficient",
  });
  const model = buildBehaviorModelRanking([], [], events, "30", now).rows[0];
  assert.equal(model.task_terminal_count, 30);
  assert.equal(model.task_success_count, 24);
  assert.equal(model.task_success_rate, 0.8);
  assert.equal(model.task_duration_p50_ms, 1500);
  assert.equal(model.task_duration_p90_ms, 2700);
  assert.deepEqual(model.selection_sources, { app_default: 20, user_selected: 10 });
  assert.equal(model.explicit_selection_count, 5);
  assert.deepEqual(model.selection_scopes, { session: 5 });
  assert.equal(model.confidence, "sufficient");
});

test("percentage metrics hide 29 samples and appear at 30", () => {
  const now = at("2026-08-12T12:00:00");
  const events = [];
  for (let index = 0; index < 29; index += 1) events.push({
    event_id: `low_tool_${index}`, event_name: "tool_call_completed", operation_id: `low_op_${index}`,
    tool_key: "search", tool_type: "mcp", success: true, identity_id: `low_${index}`,
    occurred_at: at("2026-08-12T09:00:00") + index,
  });
  for (let index = 0; index < 30; index += 1) events.push({
    event_id: `enough_tool_${index}`, event_name: "tool_call_completed", operation_id: `enough_op_${index}`,
    tool_key: "shell", tool_type: "cli", success: true, identity_id: `enough_${index}`,
    occurred_at: at("2026-08-12T10:00:00") + index,
  });
  const rows = buildBehaviorToolRanking([], [], events, "today", "call", now).rows;
  const low = rows.find(row => row.tool_key === "search");
  const enough = rows.find(row => row.tool_key === "shell");
  assert.equal(low.success_sample_size, 29);
  assert.equal(low.success_confidence, "low");
  assert.equal(low.success_rate, null);
  assert.equal(enough.success_sample_size, 30);
  assert.equal(enough.success_confidence, "sufficient");
  assert.equal(enough.success_rate, 1);
});

test("legacy behavior files are rewritten without raw tool or model aliases", () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-legacy-behavior-"));
  const privateTool = "customer-private-tool-alias";
  const privateModel = "customer-private-model-alias";
  try {
    writeFileSync(join(dataDir, "behavior-events.jsonl"), [
      JSON.stringify({ event_id: "legacy_tool_event_0001", event_name: "tool_call_completed", device_id: "legacy_device", tool_name: privateTool, tool_type: "custom", success: true, occurred_at: Date.now(), received_at: Date.now() }),
      JSON.stringify({ event_id: "legacy_model_event_0001", event_name: "model_used", device_id: "legacy_device", provider_name: "private-provider", model_name: privateModel, occurred_at: Date.now(), received_at: Date.now() }),
      JSON.stringify({ event_id: "xai_model_event_0001", event_name: "model_used", device_id: "xai_device", provider_key: "xai", model_key: "grok", occurred_at: Date.now(), received_at: Date.now() }),
      JSON.stringify({ event_id: "local_model_event_0001", event_name: "model_used", device_id: "local_device", provider_key: "local", model_key: "qwen", occurred_at: Date.now(), received_at: Date.now() }),
    ].join("\n") + "\n");
    const store = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
    assert.equal(store.behaviorEvents[0].tool_key, "custom");
    assert.equal(store.behaviorEvents[1].provider_key, "custom");
    assert.equal(store.behaviorEvents[1].model_key, "custom");
    assert.equal(store.behaviorEvents[2].provider_key, "xai");
    assert.equal(store.behaviorEvents[3].provider_key, "local");
    const persisted = readFileSync(join(dataDir, "behavior-events.jsonl"), "utf8");
    assert(!persisted.includes(privateTool));
    assert(!persisted.includes(privateModel));
    assert(!persisted.includes("tool_name"));
    assert(!persisted.includes("model_name"));
    const tools = buildBehaviorToolRanking([], [], store.behaviorEvents, "today", "call", Date.now(), { minimumGroupSubjects: 0 });
    const models = buildBehaviorModelRanking([], [], store.behaviorEvents, "today", Date.now(), { minimumGroupSubjects: 0 });
    assert.equal(tools.rows[0].tool_name, "custom");
    assert.equal(models.rows[0].model_key, "custom");
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("privacy gates suppress small groups and clip per-subject daily contributions", () => {
  const now = at("2026-08-12T12:00:00");
  const events = Array.from({ length: 101 }, (_, index) => ({
    event_id: `privacy_task_${String(index).padStart(4, "0")}`,
    event_name: "task_submitted",
    attempt_id: `privacy_attempt_${String(index).padStart(4, "0")}`,
    identity_id: "privacy_installation",
    occurred_at: at("2026-08-12T09:00:00") + index,
  }));
  events.push({
    event_id: "privacy_model_0001",
    event_name: "model_used",
    model_key: "deepseek",
    provider_key: "deepseek",
    identity_id: "privacy_installation",
    occurred_at: at("2026-08-12T10:00:00"),
  });
  const overview = buildBehaviorOverview([], [], events, "today", now);
  assert.equal(overview.metrics.task_submitted_count, 100);
  assert.deepEqual(overview.data_quality.contribution_clipping, {
    subject_count: 1,
    clipped_subject_count: 1,
    clipped_subject_rate: 1,
    task_limit_per_event_day: 100,
    tool_call_limit_per_day: 500,
  });
  assert(overview.data_quality.warnings.includes("subject_contribution_clipped"));

  const models = buildBehaviorModelRanking([], [], events, "today", now);
  assert.equal(models.rows.length, 0);
  assert.equal(models.suppressed_group_count, 1);
  assert.equal(models.minimum_group_subjects, 20);

  const forbiddenKeys = new Set(["device_id", "account_subject_id", "session_id", "turn_id", "attempt_id", "operation_id", "event_id"]);
  const visit = (value) => {
    if (!value || typeof value !== "object") return;
    for (const [key, child] of Object.entries(value)) {
      assert(!forbiddenKeys.has(key), `aggregate response exposed ${key}`);
      visit(child);
    }
  };
  visit(overview);
  visit(models);
});

test("stats roles keep behavior analysts away from device-level endpoints", async () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-stats-roles-"));
  const names = [
    "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
    "PINVOU_TELEMETRY_DEVICE_PEPPER",
    "PINVOU_STATS_ADMIN_PASSWORD",
    "PINVOU_BEHAVIOR_ANALYTICS_PASSWORD",
    "PINVOU_TELEMETRY_DATA_DIR",
  ];
  const previous = Object.fromEntries(names.map(name => [name, process.env[name]]));
  try {
    process.env.PINVOU_TELEMETRY_ENROLLMENT_TOKEN = "test-enrollment-token-at-least-24";
    process.env.PINVOU_TELEMETRY_DEVICE_PEPPER = "test-device-pepper-at-least-24";
    process.env.PINVOU_STATS_ADMIN_PASSWORD = "operations-admin-password";
    process.env.PINVOU_BEHAVIOR_ANALYTICS_PASSWORD = "behavior-analyst-password";
    process.env.PINVOU_TELEMETRY_DATA_DIR = dataDir;
    const service = createTelemetryService({ regionResolver: { enabled: false, lookup: async () => null } });

    const analystLogin = await serviceRequest(service, "POST", "/pinvou3/stats/api/login", {
      password: "behavior-analyst-password",
    });
    assert.equal(analystLogin.status, 200);
    assert.equal(analystLogin.body.role, "behavior_analyst");
    const analystCookie = analystLogin.headers["set-cookie"].split(";")[0];
    assert.equal((await serviceRequest(service, "GET", "/pinvou3/stats/api/session", undefined, { cookie: analystCookie })).body.role, "behavior_analyst");
    assert.equal((await serviceRequest(service, "GET", "/pinvou3/stats/api/behavior/overview", undefined, { cookie: analystCookie })).status, 200);
    assert.equal((await serviceRequest(service, "GET", "/pinvou3/stats/api/devices", undefined, { cookie: analystCookie })).status, 403);

    const adminLogin = await serviceRequest(service, "POST", "/pinvou3/stats/api/login", {
      password: "operations-admin-password",
    });
    const adminCookie = adminLogin.headers["set-cookie"].split(";")[0];
    assert.equal(adminLogin.body.role, "operations_admin");
    assert.equal((await serviceRequest(service, "GET", "/pinvou3/stats/api/devices", undefined, { cookie: adminCookie })).status, 200);
  } finally {
    for (const name of names) {
      if (previous[name] === undefined) delete process.env[name];
      else process.env[name] = previous[name];
    }
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("removed credentials return gone while unknown credentials stay unauthorized", async () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-removed-credential-status-"));
  const names = [
    "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
    "PINVOU_TELEMETRY_DEVICE_PEPPER",
    "PINVOU_STATS_ADMIN_PASSWORD",
    "PINVOU_TELEMETRY_DATA_DIR",
  ];
  const previous = Object.fromEntries(names.map(name => [name, process.env[name]]));
  let service;
  try {
    process.env.PINVOU_TELEMETRY_ENROLLMENT_TOKEN = "test-enrollment-token-at-least-24";
    process.env.PINVOU_TELEMETRY_DEVICE_PEPPER = "test-device-pepper-at-least-24";
    process.env.PINVOU_STATS_ADMIN_PASSWORD = "operations-admin-password";
    process.env.PINVOU_TELEMETRY_DATA_DIR = dataDir;
    service = createTelemetryService({ regionResolver: { enabled: false, lookup: async () => null } });

    const registrationBody = {
      enrollment_token: "test-enrollment-token-at-least-24",
      hardware_claim: "removed-credential-hardware-0001",
      registration_secret: "registration-secret-removed-credential-0001",
      app_version: "0.8.7",
      platform: "windows",
      arch: "x86_64",
    };
    const registration = await serviceRequest(
      service,
      "POST",
      "/pinvou3/telemetry/v1/register",
      registrationBody,
    );
    assert.equal(registration.status, 200);
    const authorization = { authorization: `Bearer ${registration.body.device_token}` };
    const deletion = await serviceRequest(service, "POST", "/pinvou3/stats/api/behavior/deletion", {
      device_id: registration.body.device_id,
      request_id: "request_removed_credential_0001",
    }, authorization);
    assert.equal(deletion.status, 202);

    const removed = await serviceRequest(
      service,
      "GET",
      `/pinvou3/stats/api/behavior/collection?device_id=${registration.body.device_id}`,
      undefined,
      authorization,
    );
    assert.equal(removed.status, 410);
    assert.deepEqual(removed.body, { error: "device_data_deleted" });
    const resurrect = await serviceRequest(
      service,
      "POST",
      "/pinvou3/telemetry/v1/register",
      registrationBody,
    );
    assert.equal(resurrect.status, 410);
    assert.deepEqual(resurrect.body, { error: "installation_data_deleted" });
    const unknown = await serviceRequest(
      service,
      "GET",
      `/pinvou3/stats/api/behavior/collection?device_id=${registration.body.device_id}`,
      undefined,
      { authorization: "Bearer unknown-device-token" },
    );
    assert.equal(unknown.status, 401);
    assert.deepEqual(unknown.body, { error: "invalid_device_token" });
    for (let index = 0; index < 118; index += 1) {
      const attempt = await serviceRequest(
        service,
        "GET",
        `/pinvou3/stats/api/behavior/collection?device_id=unknown_${index}`,
        undefined,
        { authorization: `Bearer unknown-device-token-${index}` },
      );
      assert.equal(attempt.status, 401);
    }
    const limited = await serviceRequest(
      service,
      "GET",
      "/pinvou3/stats/api/behavior/collection?device_id=unknown_limited",
      undefined,
      { authorization: "Bearer unknown-device-token-limited" },
    );
    assert.equal(limited.status, 429);
  } finally {
    service?.close();
    for (const name of names) {
      if (previous[name] === undefined) delete process.env[name];
      else process.env[name] = previous[name];
    }
    rmSync(dataDir, { recursive: true, force: true, maxRetries: 3, retryDelay: 50 });
  }
});

test("naturally expired credentials return a renewable gone response", async () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-expired-credential-status-"));
  const names = [
    "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
    "PINVOU_TELEMETRY_DEVICE_PEPPER",
    "PINVOU_STATS_ADMIN_PASSWORD",
    "PINVOU_TELEMETRY_DATA_DIR",
  ];
  const previous = Object.fromEntries(names.map(name => [name, process.env[name]]));
  let service;
  try {
    const pepper = "test-device-pepper-at-least-24";
    process.env.PINVOU_TELEMETRY_ENROLLMENT_TOKEN = "test-enrollment-token-at-least-24";
    process.env.PINVOU_TELEMETRY_DEVICE_PEPPER = pepper;
    process.env.PINVOU_STATS_ADMIN_PASSWORD = "operations-admin-password";
    process.env.PINVOU_TELEMETRY_DATA_DIR = dataDir;
    const registrationBody = {
      enrollment_token: "test-enrollment-token-at-least-24",
      hardware_claim: "expired-credential-hardware-0001",
      registration_secret: "registration-secret-expired-credential-0001",
      app_version: "0.8.7",
      platform: "windows",
      arch: "x86_64",
    };
    const bootstrap = new TelemetryStore(dataDir, pepper);
    const original = bootstrap.register(registrationBody, null);
    const originalDevice = bootstrap.authenticate(original.device_id, original.device_token);
    originalDevice.first_seen_at = Date.now() - 36 * 24 * 60 * 60_000;
    bootstrap.saveNow();

    service = createTelemetryService({ regionResolver: { enabled: false, lookup: async () => null } });
    const expired = await serviceRequest(
      service,
      "GET",
      `/pinvou3/stats/api/behavior/collection?device_id=${original.device_id}`,
      undefined,
      { authorization: `Bearer ${original.device_token}` },
    );
    assert.equal(expired.status, 410);
    assert.deepEqual(expired.body, { error: "device_credentials_expired" });

    const renewed = await serviceRequest(
      service,
      "POST",
      "/pinvou3/telemetry/v1/register",
      registrationBody,
    );
    assert.equal(renewed.status, 200);
    assert.notEqual(renewed.body.device_id, original.device_id);
    assert.notEqual(renewed.body.device_token, original.device_token);
  } finally {
    service?.close();
    for (const name of names) {
      if (previous[name] === undefined) delete process.env[name];
      else process.env[name] = previous[name];
    }
    rmSync(dataDir, { recursive: true, force: true, maxRetries: 3, retryDelay: 50 });
  }
});

test("behavior deletion removes identity and facts while tombstones survive backup restore", () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-behavior-control-"));
  try {
    const store = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
    const registration = {
      hardware_claim: "behavior-control-hardware-0001",
      registration_secret: "registration-secret-control-00000001",
      app_version: "0.8.7",
      platform: "windows",
      arch: "x86_64",
    };
    const credential = store.register(registration, null);
    const device = store.authenticate(credential.device_id, credential.device_token);
    assert.deepEqual(store.behaviorCollectionStatus(device), {
      status: "inactive",
      collection_period_id: null,
      started_at: null,
      collection_ended_at: null,
    });

    const start = store.startBehaviorCollection(device, { request_id: "request_start_0001" });
    assert.equal(start.status, "active");
    assert.match(start.collection_period_id, /^period_/);
    assert.deepEqual(
      store.startBehaviorCollection(device, { request_id: "request_start_0001" }),
      start,
    );
    const end = store.endBehaviorCollection(device, {
      request_id: "request_end_0001",
      collection_period_id: start.collection_period_id,
    });
    assert.equal(end.status, "ended");
    const second = store.startBehaviorCollection(device, { request_id: "request_start_0002" });
    assert.notEqual(second.collection_period_id, start.collection_period_id);

    store.appendEvents(device, [{
      event_id: "usage_delete_test_0001",
      occurred_at: Date.now(),
      input_tokens: 1,
      output_tokens: 1,
      success: true,
    }]);
    const event = behaviorEvent(second.collection_period_id, {
      event_id: "behavior_delete_test_01",
    });
    assert.equal(store.appendBehaviorEvents(device, [event]).results[0].status, "accepted");
    store.saveNow();
    const backup = {
      devices: readFileSync(join(dataDir, "devices.json")),
      usage: readFileSync(join(dataDir, "usage-events.jsonl")),
      behavior: readFileSync(join(dataDir, "behavior-events.jsonl")),
    };
    const deletion = store.deleteBehaviorData(device, { request_id: "request_delete_0001" });
    assert.equal(deletion.status, "completed");
    assert.match(deletion.deletion_request_id, /^delete_/);
    assert.equal(store.events.length, 0);
    assert.equal(store.behaviorEvents.length, 0);
    assert.equal(store.authenticate(credential.device_id, credential.device_token), null);
    assert.throws(
      () => store.register(registration, null),
      error => error.status === 410 && error.message === "installation_data_deleted",
    );
    assert.deepEqual(
      store.priorDeletionResponse("request_delete_0001", credential.device_token),
      deletion,
    );
    assert.deepEqual(
      store.deletionStatus(deletion.deletion_request_id, credential.device_token),
      deletion,
    );
    const resultExpiredAt = deletion.accepted_at + 30 * 24 * 60 * 60_000;
    assert.equal(
      store.priorDeletionResponse("request_delete_0001", credential.device_token, resultExpiredAt),
      null,
    );
    assert.throws(
      () => store.deletionStatus(deletion.deletion_request_id, credential.device_token, resultExpiredAt),
      error => error.status === 410 && error.message === "deletion_result_expired",
    );

    // Simulate restoring a pre-deletion backup while preserving the deletion ledger.
    writeFileSync(join(dataDir, "devices.json"), backup.devices);
    writeFileSync(join(dataDir, "usage-events.jsonl"), backup.usage);
    writeFileSync(join(dataDir, "behavior-events.jsonl"), backup.behavior);
    const deletionLedgerPath = join(dataDir, "behavior-deletion-tombstones.json");
    const deletionLedger = JSON.parse(readFileSync(deletionLedgerPath, "utf8"));
    deletionLedger.records[0].tombstone_expires_at = 1;
    writeFileSync(deletionLedgerPath, JSON.stringify(deletionLedger));

    const reloaded = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
    assert.equal(reloaded.authenticate(credential.device_id, credential.device_token), null);
    assert.equal(reloaded.events.length, 0);
    assert.equal(reloaded.behaviorEvents.length, 0);
    assert.deepEqual(
      reloaded.priorDeletionResponse("request_delete_0001", credential.device_token),
      deletion,
    );
    assert.equal(reloaded.deletionTombstones.length, 1);
    assert.equal(reloaded.removedSubjectHashes.size, 1);
    assert.equal(reloaded.removedCredentialReasons.size, 1);
    assert.equal(reloaded.deletedRegistrationIdentities.size, 1);
    assert.equal(reloaded.deletionResultsByRequest.size, 1);
    assert.equal(reloaded.deletionResultsById.size, 1);
    reloaded.compactExpiredData(deletion.accepted_at + 36 * 24 * 60 * 60_000);
    assert.deepEqual(reloaded.deletionTombstones, [{
      subject_hash: reloaded.deletionSubjectHash(credential.device_id),
      token_hash: reloaded.deletionTokenHash(credential.device_token),
      removal_reason: "deleted",
      hardware_hash: reloaded.hardwareHash(registration.hardware_claim),
      registration_secret_hash: reloaded.registrationSecretHash(
        registration.registration_secret,
      ),
    }]);
    assert.equal(reloaded.credentialWasRemoved(credential.device_token), true);
    assert.equal(reloaded.credentialRemovalReason(credential.device_token), "deleted");
    assert.equal(reloaded.removedSubjectHashes.size, 1);
    assert.equal(reloaded.removedCredentialReasons.size, 1);
    assert.equal(reloaded.deletedRegistrationIdentities.size, 1);
    assert.equal(reloaded.deletionResultsByRequest.size, 0);
    assert.equal(reloaded.deletionResultsById.size, 0);
    assert.throws(
      () => reloaded.register(registration, null),
      error => error.status === 410 && error.message === "installation_data_deleted",
    );
    writeFileSync(join(dataDir, "devices.json"), backup.devices);
    const restoredAfterResultExpiry = new TelemetryStore(
      dataDir,
      "test-device-pepper-at-least-24-chars",
    );
    assert.equal(restoredAfterResultExpiry.authenticate(
      credential.device_id,
      credential.device_token,
    ), null);
    assert.equal(JSON.parse(readFileSync(join(dataDir, "devices.json"), "utf8")).devices.length, 0);
    if (process.platform !== "win32") {
      assert.equal(statSync(dataDir).mode & 0o777, 0o700);
      for (const name of ["devices.json", "usage-events.jsonl", "behavior-events.jsonl", "behavior-deletion-tombstones.json"]) {
        assert.equal(statSync(join(dataDir, name)).mode & 0o777, 0o600);
      }
    }
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("retention compaction removes expired identity mappings and facts without request traffic", () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-retention-control-"));
  try {
    const now = Date.now();
    const store = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
    const registration = {
      hardware_claim: "retention-control-hardware-0001",
      registration_secret: "registration-secret-retention-000001",
      app_version: "0.8.7",
      platform: "windows",
      arch: "x86_64",
    };
    const credential = store.register(registration, null);
    const device = store.authenticate(credential.device_id, credential.device_token);
    device.first_seen_at = now - 36 * 24 * 60 * 60_000;
    device.last_seen_at = now;
    device.last_active_at = now;
    store.appendEvents(device, [{
      event_id: "usage_retention_test_0001",
      occurred_at: now,
      input_tokens: 1,
      output_tokens: 1,
      success: true,
    }]);
    assert.equal(store.devices.size, 1);
    assert.equal(store.events.length, 1);

    assert.equal(store.compactExpiredData(now), true);
    assert.equal(store.devices.size, 0);
    assert.equal(store.devicesByHardwareHash.size, 0);
    assert.equal(store.events.length, 0);
    assert.equal(store.credentialWasRemoved(credential.device_token), true);
    assert.equal(store.credentialRemovalReason(credential.device_token), "expired");
    assert.equal(JSON.parse(readFileSync(join(dataDir, "devices.json"), "utf8")).devices.length, 0);
    assert.equal(readFileSync(join(dataDir, "usage-events.jsonl"), "utf8"), "");
    const renewed = store.register(registration, null);
    assert.notEqual(renewed.device_id, credential.device_id);
    assert.notEqual(renewed.device_token, credential.device_token);

    const staleBackup = JSON.parse(readFileSync(join(dataDir, "devices.json"), "utf8"));
    staleBackup.devices = [{
      ...device,
      first_seen_at: now - 36 * 24 * 60 * 60_000,
      last_seen_at: now - 36 * 24 * 60 * 60_000,
      last_active_at: now - 36 * 24 * 60 * 60_000,
    }];
    writeFileSync(join(dataDir, "devices.json"), JSON.stringify(staleBackup));
    const reloaded = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
    assert.equal(reloaded.devices.size, 0);
    assert.equal(JSON.parse(readFileSync(join(dataDir, "devices.json"), "utf8")).devices.length, 0);
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("behavior event rate limits return retryable results without persisting", () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-behavior-rate-limit-"));
  try {
    const store = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
    const credential = store.register({
      hardware_claim: "behavior-rate-limit-hardware-0001",
      registration_secret: "registration-secret-rate-limit-00000001",
      app_version: "0.8.7",
      platform: "windows",
      arch: "x86_64",
    }, null);
    const device = store.authenticate(credential.device_id, credential.device_token);
    const collection = store.startBehaviorCollection(device, {
      request_id: "request_rate_start_000001",
    });
    store.eventRatePerMinute = 1;
    const result = store.appendBehaviorEvents(device, [
      behaviorEvent(collection.collection_period_id),
      behaviorEvent(collection.collection_period_id, {
        event_id: "behavior_event_test_000002",
      }),
    ], "batch_rate_limited_000001");
    assert.deepEqual(result.results, [
      { event_id: "behavior_event_test_000001", status: "retryable", reason_code: "rate_limited" },
      { event_id: "behavior_event_test_000002", status: "retryable", reason_code: "rate_limited" },
    ]);
    assert.equal(store.behaviorEvents.length, 0);
    assert.equal(existsSync(join(dataDir, "behavior-events.jsonl")), false);
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("offline IP region data keeps only coarse location fields", () => {
  const region = parseIp2Region("中国|广东省|深圳市|电信|CN");
  assert.deepEqual(region, {
    country: "中国",
    province: "广东省",
    city: "深圳市",
    country_code: "CN",
  });
  assert.equal(formatDeviceRegion(region), "广东省 / 深圳市");
  assert.equal(formatDeviceRegion(parseIp2Region("Singapore|0|Singapore|0|SG")), "Singapore");
  assert.equal(parseIp2Region(""), null);
});

test("registration requires the installation secret and enforces device capacity", () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-telemetry-store-"));
  try {
    const store = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars", {
      maxDevices: 2,
    });
    const registration = {
      hardware_claim: "hardware-claim-0001",
      registration_secret: "registration-secret-000000000000000001",
      hardware_source: "test",
      identity_quality: "hardware_serial",
      app_version: "0.5.10",
      platform: "linux",
      arch: "aarch64",
    };
    const first = store.register(registration, null);
    assert.deepEqual(store.register(registration, null), first);
    assert.throws(
      () => store.register({
        ...registration,
        registration_secret: "different-secret-0000000000000000001",
      }, null),
      (error) => error.status === 409 && error.message === "device_already_registered",
    );

    const legacyDevice = store.authenticate(first.device_id, first.device_token);
    delete legacyDevice.registration_secret_hash;
    assert.throws(
      () => store.register(registration, null),
      (error) => error.status === 409 && error.message === "device_already_registered",
    );
    store.heartbeat(legacyDevice, {
      registration_secret: registration.registration_secret,
      state: "online",
    });
    assert.deepEqual(store.register(registration, null), first);

    store.register({
      ...registration,
      hardware_claim: "hardware-claim-0002",
      registration_secret: "registration-secret-000000000000000002",
    }, null);
    assert.throws(
      () => store.register({
        ...registration,
        hardware_claim: "hardware-claim-0003",
        registration_secret: "registration-secret-000000000000000003",
      }, null),
      (error) => error.status === 503 && error.message === "device_capacity_reached",
    );
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("event store keeps a bounded recent window on disk and after restart", () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-telemetry-events-"));
  try {
    const options = {
      maxEvents: 100,
      eventRatePerMinute: 1_000,
      globalEventRatePerMinute: 200,
      eventRetentionMs: 90 * 24 * 60 * 60_000,
    };
    const store = new TelemetryStore(
      dataDir,
      "test-device-pepper-at-least-24-chars",
      options,
    );
    assert.equal(store.eventRetentionMs, 35 * 24 * 60 * 60_000);
    const registration = {
      hardware_claim: "bounded-events-hardware",
      registration_secret: "registration-secret-bounded-events-00001",
      hardware_source: "test",
      identity_quality: "hardware_serial",
      app_version: "0.5.10",
      platform: "linux",
      arch: "aarch64",
    };
    const credential = store.register(registration, null);
    const device = store.authenticate(credential.device_id, credential.device_token);
    const now = Date.now();
    const events = Array.from({ length: 120 }, (_, index) => ({
      event_id: `evt_bounded_${String(index).padStart(6, "0")}`,
      occurred_at: now + index,
      input_tokens: 1,
      output_tokens: 1,
      success: true,
    }));
    assert.equal(store.appendEvents(device, events).accepted.length, 120);
    assert.equal(store.events.length, 90);
    assert.equal(readFileSync(join(dataDir, "usage-events.jsonl"), "utf8").trim().split("\n").length, 90);
    assert.throws(
      () => store.appendEvents(device, events.slice(0, 100)),
      (error) => error.status === 429 && error.message === "event_rate_limited",
    );

    const restarted = new TelemetryStore(
      dataDir,
      "test-device-pepper-at-least-24-chars",
      options,
    );
    assert.equal(restarted.events.length, 90);
    assert.equal(restarted.eventIds.size, 90);
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
});
