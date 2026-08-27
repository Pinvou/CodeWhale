import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  TelemetryStore,
  buildBehaviorModelRanking,
  buildBehaviorOverview,
  buildBehaviorSceneRanking,
  buildBehaviorToolRanking,
  buildCommercialMock,
  buildDeviceList,
  buildStatsOverview,
  formatDeviceRegion,
  parseIp2Region,
} from "../telemetry-service.js";

const at = (value) => Date.parse(`${value}+08:00`);

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
    { event_id: "task_a", event_name: "task_started", identity_id: "dev_a", occurred_at: at("2026-08-12T09:01:00") },
    { event_id: "task_done_a", event_name: "task_finished", identity_id: "dev_a", status: "success", occurred_at: at("2026-08-12T09:02:00") },
    { event_id: "task_done_b", event_name: "task_finished", identity_id: "dev_b", status: "interrupted", occurred_at: at("2026-08-12T10:02:00") },
    { event_id: "voice_a", event_name: "voice_started", identity_id: "dev_a", occurred_at: at("2026-08-12T09:03:00") },
    { event_id: "voice_c", event_name: "voice_started", identity_id: "dev_c", occurred_at: at("2026-08-12T11:03:00") },
    { event_id: "tool_a", event_name: "tool_call_completed", identity_id: "dev_a", tool_key: "search", tool_name: "Search", tool_type: "mcp", success: true, occurred_at: at("2026-08-12T09:04:00") },
    { event_id: "tool_b", event_name: "tool_call_completed", identity_id: "dev_b", tool_key: "search", tool_name: "Search", tool_type: "builtin", success: false, occurred_at: at("2026-08-12T10:04:00") },
    { event_id: "install_a", event_name: "tool_install_completed", identity_id: "dev_a", tool_key: "search", tool_name: "Search", tool_type: "mcp", success: true, occurred_at: at("2026-08-12T09:05:00") },
    { event_id: "scene_a", event_name: "scene_triggered", identity_id: "dev_a", scene_l1: "work", scene_l2: "document-writing", occurred_at: at("2026-08-12T09:06:00") },
    { event_id: "model_a", event_name: "model_used", identity_id: "dev_a", provider_key: "deepseek", provider_name: "DeepSeek", model_id: "deepseek-chat", model_name: "DeepSeek Chat", occurred_at: at("2026-08-12T09:07:00") },
    { event_id: "model_b", event_name: "model_used", identity_id: "dev_b", provider_key: "deepseek", provider_name: "DeepSeek", model_id: "deepseek-chat", model_name: "DeepSeek Chat", occurred_at: at("2026-08-12T10:07:00") },
    { event_id: "model_c", event_name: "model_used", identity_id: "dev_a", provider_key: "openai", provider_name: "OpenAI", model_id: "gpt-5", model_name: "GPT-5", occurred_at: at("2026-08-12T11:07:00") },
  ];

  const overview = buildBehaviorOverview(devices, usageEvents, behaviorEvents, "today", now);
  assert.equal(overview.active_users, 3);
  assert.equal(overview.metrics.task_started_count, 1);
  assert.equal(overview.metrics.task_success_count, 1);
  assert.equal(overview.metrics.task_interrupted_count, 1);
  assert.equal(overview.metrics.voice_started_users, 2);
  assert.equal(overview.metrics.voice_penetration_rate, 2 / 3);

  const tools = buildBehaviorToolRanking(devices, usageEvents, behaviorEvents, "today", "call", now);
    assert.equal(tools.rows[0].tool_key, "search");
    assert.equal(tools.rows[0].call_count, 2);
    assert.equal(tools.rows[0].user_count, 2);
    assert.equal(tools.rows[0].tool_type, "mcp");
    assert.equal(tools.rows[0].success_rate, 0.5);

  const installs = buildBehaviorToolRanking(devices, usageEvents, behaviorEvents, "today", "install", now);
  assert.equal(installs.rows[0].install_count, 1);

  const scenes = buildBehaviorSceneRanking(devices, usageEvents, behaviorEvents, "today", now);
  assert.equal(scenes.rows[0].scene_l1, "work");
  assert.equal(scenes.rows[0].trigger_count, 1);

  const models = buildBehaviorModelRanking(devices, usageEvents, behaviorEvents, "today", now);
  assert.equal(models.rows[0].provider_key, "deepseek");
  assert.equal(models.rows[0].model_id, "deepseek-chat");
  assert.equal(models.rows[0].user_count, 2);
  assert.equal(models.rows[0].use_count, 2);
  assert.equal(models.rows[1].model_id, "gpt-5");

  const commercial = buildCommercialMock("30", now);
  assert.equal(commercial.mock, true);
  assert.equal(typeof commercial.metrics.arpu, "number");
});

test("behavior event store requires authenticated device identity and deduplicates", () => {
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
    const result = store.appendBehaviorEvents(device, [
      {
        event_id: "behavior_evt_000001",
        event_name: "voice_started",
        identity_id: credential.device_id,
        identity_type: "device",
        app_version: "0.8.1",
        platform: "windows",
        tool_type: "builtin",
        provider_key: "deepseek",
        provider_name: "DeepSeek",
        model_id: "deepseek-chat",
        model_name: "DeepSeek Chat",
        occurred_at: Date.now(),
      },
      {
        event_id: "behavior_evt_000001",
        event_name: "voice_started",
        identity_id: credential.device_id,
        identity_type: "device",
        occurred_at: Date.now(),
      },
      {
        event_id: "behavior_evt_000002",
        event_name: "voice_started",
        identity_id: "spoofed",
        identity_type: "device",
        occurred_at: Date.now(),
      },
    ]);
    assert.deepEqual(result.accepted, ["behavior_evt_000001"]);
    assert.deepEqual(result.duplicates, ["behavior_evt_000001"]);
    assert.deepEqual(result.rejected, ["behavior_evt_000002"]);
    assert.equal(store.behaviorEvents.length, 1);
    assert.equal(store.behaviorEvents[0].app_version, "0.8.1");
    assert.equal(store.behaviorEvents[0].platform, "windows");
    assert.equal(store.behaviorEvents[0].tool_type, "unknown");
    assert.equal(store.behaviorEvents[0].provider_key, "deepseek");
    assert.equal(store.behaviorEvents[0].model_id, "deepseek-chat");
    assert.equal(readFileSync(join(dataDir, "behavior-events.jsonl"), "utf8").trim().split("\n").length, 1);
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
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
