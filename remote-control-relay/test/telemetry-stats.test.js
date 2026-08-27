import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
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
  assert.equal(overview.metrics.task_admission_rate_24h, 2 / 3);
  assert.equal(overview.metrics.task_rejection_rate_24h, 1 / 3);
  assert.equal(overview.metrics.task_submission_resolution_rate_24h, 1);
  assert.equal(overview.metrics.active_installation_days, 6);
  assert.equal(overview.metrics.task_submitted_per_active_installation_day, 0.5);
  assert.equal(overview.metrics.task_terminal_completeness_rate, 1);
  assert.equal(overview.metrics.task_compensation_rate, 1 / 3);
  assert.equal(overview.metrics.wsta_installations, 1);
  assert.equal(overview.metrics.wevta_installations, 1);
  assert.equal(overview.metrics.wevta_rate, 1);
  assert.equal(overview.metrics.cohort_matured_installations, 2);
  assert.equal(overview.metrics.first_technical_value_24h_installations, 1);
  assert.equal(overview.metrics.first_technical_value_24h_rate, 0.5);
  assert.equal(overview.metrics.tool_call_completion_rate, 1);
  assert.equal(overview.metrics.tool_call_success_rate, 0.5);
  assert.equal(overview.metrics.tool_install_completion_rate, 1);
  assert.equal(overview.metrics.pipeline_rejection_rate, 0.03);
  assert.equal(overview.metrics.task_success_count, 1);
  assert.equal(overview.metrics.task_interrupted_count, 1);
  assert.equal(overview.metrics.voice_started_installations, 2);
  assert.equal(overview.metrics.voice_penetration_rate, 2 / 3);

  const tools = buildBehaviorToolRanking(devices, usageEvents, behaviorEvents, "7", "call", now);
    assert.equal(tools.rows[0].tool_key, "search");
    assert.equal(tools.rows[0].call_count, 2);
    assert.equal(tools.rows[0].user_count, 2);
    assert.equal(tools.rows[0].tool_type, "mcp");
    assert.equal(tools.rows[0].success_rate, 0.5);

  const installs = buildBehaviorToolRanking(devices, usageEvents, behaviorEvents, "7", "install", now);
  assert.equal(installs.rows[0].install_count, 1);

  const scenes = buildBehaviorSceneRanking(devices, usageEvents, behaviorEvents, "7", now);
  assert.equal(scenes.rows[0].scene_l1, "work");
  assert.equal(scenes.rows[0].trigger_count, 1);

  const models = buildBehaviorModelRanking(devices, usageEvents, behaviorEvents, "7", now);
  assert.equal(models.rows[0].provider_key, "deepseek");
  assert.equal(models.rows[0].model_key, "deepseek");
  assert.equal(models.rows[0].user_count, 2);
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
    wevta_rate: 0.25,
    positive_feedback_installations: 0,
    response_copied_installations: 1,
    artifact_exported_installations: 0,
    negative_feedback_installations: 0,
    negative_feedback_rate: 0,
  });
});

test("P1 dashboard metrics cover retention funnels version quality and confidence", () => {
  const now = at("2026-08-12T12:00:00");
  const behaviorEvents = [
    { event_id: "cohort_d1", event_name: "analytics_cohort_started", identity_id: "retained_d1", occurred_at: at("2026-08-10T08:00:00") },
    { event_id: "active_d1", event_name: "app_opened", identity_id: "retained_d1", occurred_at: at("2026-08-11T08:00:00") },
    { event_id: "cohort_d7", event_name: "analytics_cohort_started", identity_id: "retained_d7", occurred_at: at("2026-08-04T08:00:00") },
    { event_id: "active_d7", event_name: "app_opened", identity_id: "retained_d7", occurred_at: at("2026-08-11T08:00:00") },
    { event_id: "cohort_d30", event_name: "analytics_cohort_started", identity_id: "retained_d30", occurred_at: at("2026-07-13T08:00:00") },
    { event_id: "active_d30", event_name: "app_opened", identity_id: "retained_d30", occurred_at: at("2026-08-12T08:00:00") },
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
  assert.deepEqual(overview.retention.d1, { horizon_days: 1, eligible_installations: 3, retained_installations: 1, retention_rate: 1 / 3, confidence: "low" });
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
  const tools = buildBehaviorToolRanking([], [], events, "today", "install", now);
  assert.deepEqual(tools.funnel_rows[0], {
    tool_key: "search", tool_name: "search", tool_type: "mcp",
    impression_count: 1, detail_count: 1, install_started_count: 1, install_succeeded_count: 1,
    configuration_started_count: 1, configuration_succeeded_count: 1, call_succeeded_count: 1,
    failed_count: 0, cancelled_count: 0, impression_to_install_rate: 1, install_success_rate: 1,
    install_to_configuration_rate: 1, configuration_success_rate: 1, configuration_to_call_rate: 1,
    configuration_to_call_p50_ms: 60_000,
    confidence: "low",
  });
  const scenes = buildBehaviorSceneRanking([], [], events, "today", now);
  assert.equal(scenes.rows[0].selection_rate, 1);
  assert.equal(scenes.rows[0].submission_rate, 1);
  assert.equal(scenes.rows[0].task_success_rate, 1);
  assert.equal(scenes.rows[0].explicit_value_rate, 1);
  assert.equal(scenes.rows[0].task_duration_p50_ms, 1200);
});

test("behavior collection control is opt-in, idempotent and deletion removes facts", () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-behavior-control-"));
  try {
    const store = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
    const credential = store.register({
      hardware_claim: "behavior-control-hardware-0001",
      registration_secret: "registration-secret-control-00000001",
      app_version: "0.8.7",
      platform: "windows",
      arch: "x86_64",
    }, null);
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

    store.behaviorEvents.push({
      event_id: "behavior_delete_test_01",
      event_name: "voice_started",
      device_id: device.device_id,
      occurred_at: Date.now(),
      received_at: Date.now(),
    });
    store.behaviorEventIds.add("behavior_delete_test_01");
    const deletion = store.deleteBehaviorData(device, { request_id: "request_delete_0001" });
    assert.equal(deletion.status, "completed");
    assert.match(deletion.deletion_request_id, /^delete_/);
    assert.equal(store.behaviorEvents.length, 0);
    assert.deepEqual(
      store.deleteBehaviorData(device, { request_id: "request_delete_0001" }),
      deletion,
    );

    store.saveNow();
    const reloaded = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
    const reloadedDevice = reloaded.authenticate(credential.device_id, credential.device_token);
    assert.equal(reloaded.behaviorCollectionStatus(reloadedDevice).status, "ended");
    assert.deepEqual(
      reloaded.deleteBehaviorData(reloadedDevice, { request_id: "request_delete_0001" }),
      deletion,
    );
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
    };
    const store = new TelemetryStore(
      dataDir,
      "test-device-pepper-at-least-24-chars",
      options,
    );
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
