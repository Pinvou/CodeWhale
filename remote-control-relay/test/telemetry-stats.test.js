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

test("behavior overview handles event counts above the function argument limit", () => {
  const now = at("2026-10-08T17:00:00");
  const devices = [{ device_id: "large_install", hardware_source: "installation_id" }];
  const events = Array.from({ length: 200_000 }, (_, index) => ({
    device_id: "large_install",
    event_name: "scene_impression",
    occurred_at: now - 10_000,
    received_at: index === 123 ? now - 1 : now - 5_000,
  }));
  events[456].received_at = 0;
  events[456].occurred_at = now;
  const report = buildBehaviorOverview(devices, [], events, "30", now);
  assert.equal(report.latest_event_received_at, now);
  assert.equal(report.data_status, "ready");
  assert.equal(report.range, "30");
  assert.equal(buildBehaviorOverview(devices, [], [], "30", now).latest_event_received_at, null);
});

test("event capacity accepts the deployed ten million limit without changing the default", () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-telemetry-capacity-"));
  try {
    const pepper = "test-device-pepper-at-least-24-chars";
    assert.equal(new TelemetryStore(dataDir, pepper).maxEvents, 100_000);
    assert.equal(new TelemetryStore(dataDir, pepper, { maxEvents: 10_000_000 }).maxEvents, 10_000_000);
    assert.equal(new TelemetryStore(dataDir, pepper, { maxEvents: 20_000_000 }).maxEvents, 10_000_000);
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("device details use the latest observed behavior version regardless of event order", () => {
  const now = at("2026-09-10T12:00:00");
  const devices = [{
    device_id: "install_version_current",
    hardware_source: "installation_id",
    identity_quality: "installation_only",
    app_version: "0.9.0",
    first_seen_at: now - 10_000,
    last_seen_at: now,
  }];
  const common = {
    event_name: "task_submitted",
    device_id: "install_version_current",
    identity_id: "install_version_current",
    mode: "assistant",
    initiator_type: "user",
  };
  const behaviorEvents = [
    {
      ...common,
      event_id: "version_newer",
      attempt_id: "attempt_newer",
      app_version: "0.9.2",
      occurred_at: now - 1_000,
      received_at: now - 900,
    },
    {
      ...common,
      event_id: "version_older_but_later_in_file",
      attempt_id: "attempt_older",
      app_version: "0.9.1",
      occurred_at: now - 5_000,
      received_at: now - 4_900,
    },
  ];

  assert.equal(buildDeviceList(devices, [], behaviorEvents, now)[0].app_version, "0.9.2");
  assert.deepEqual(buildStatsOverview(devices, [], behaviorEvents, now).active_versions, [
    { version: "0.9.2", count: 1 },
  ]);
});
const installationDevicesFor = (events) => [...new Set(events
  .map((event) => event.identity_id || event.device_id)
  .filter(Boolean))]
  .map((device_id) => ({ device_id, hardware_source: "installation_id" }));

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
      app_version: "0.8.3",
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
      app_version: "0.8.2",
      platform: "windows",
      state: "online",
    },
    {
      device_id: "dev_c",
      first_seen_at: at("2026-07-14T08:00:00"),
      last_seen_at: now - 360_000,
      last_active_at: at("2026-07-14T09:00:00"),
      app_version: "0.8.3",
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

  for (const device of devices) device.hardware_source = "installation_id";
  const behaviorEvents = events.map((event) => ({
    ...event,
    event_name: "task_finished",
    attempt_id: event.event_id,
    mode: "assistant",
    initiator_type: "user",
    status: event.success === false ? "failed" : "success",
  }));
  const overview = buildStatsOverview(devices, [], behaviorEvents, now);
  assert.equal(overview.online_window_seconds, 300);
  assert.deepEqual(overview.counts, { online: 1, active_today: 3, active_7d: 3 });
  assert.deepEqual(overview.active_versions, [
    { version: "0.8.3", count: 2 },
    { version: "0.8.2", count: 1 },
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

  const list = buildDeviceList(devices, [], behaviorEvents, now);
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

test("dashboard normalizes second timestamps for activity and first registration", () => {
  const now = at("2026-07-14T12:00:00");
  const firstSeenAt = at("2026-07-14T08:00:00");
  const occurredAt = at("2026-07-14T10:00:00");
  const devices = [{
    device_id: "dev_seconds",
    first_seen_at: Math.floor(firstSeenAt / 1_000),
    last_seen_at: now - 10_000,
    last_active_at: occurredAt,
    app_version: "0.9.1",
    platform: "windows",
  }];
  const events = [{
    event_id: "usage_seconds",
    device_id: "dev_seconds",
    occurred_at: Math.floor(occurredAt / 1_000),
  }];

  for (const device of devices) device.hardware_source = "installation_id";
  const behaviorEvents = events.map((event) => ({
    ...event,
    event_name: "task_finished",
    attempt_id: event.event_id,
    mode: "assistant",
    initiator_type: "user",
    status: event.success === false ? "failed" : "success",
  }));
  const overview = buildStatsOverview(devices, [], behaviorEvents, now);
  assert.deepEqual(overview.counts, { online: 1, active_today: 1, active_7d: 1 });
  assert.equal(overview.usage_trend.at(-1).turns, 1);

  const list = buildDeviceList(devices, [], behaviorEvents, now);
  assert.equal(list[0].first_seen_at, firstSeenAt);
  assert.equal(list[0].turns_today, 1);
  assert.equal(list[0].turns_7d, 1);
});

test("store migrates historical device and event timestamps without moving future events to today", (t) => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-timestamp-migration-"));
  t.after(() => rmSync(dataDir, { recursive: true, force: true }));
  const now = Date.now();
  const firstSeenAt = now - 2 * 60 * 60_000;
  const lastSeenAt = now - 30_000;
  const lastActiveAt = now - 60 * 60_000;
  const receivedAt = now - 30 * 60_000;
  const futureAt = now + 24 * 60 * 60_000;
  const device = {
    device_id: "dev_migrated",
    hardware_hash: "hardware_hash_migrated",
    hardware_source: "windows_cim_uuid",
    device_token: "device_token_migrated",
    first_seen_at: Math.floor(firstSeenAt / 1_000),
    last_seen_at: Math.floor(lastSeenAt / 1_000),
    last_active_at: Math.floor(lastActiveAt / 1_000),
  };
  const invalidFirstSeenDevices = [
    { suffix: "missing" },
    { suffix: "zero", first_seen_at: 0 },
    { suffix: "invalid", first_seen_at: "invalid" },
    { suffix: "future", first_seen_at: Math.floor(futureAt / 1_000) },
  ].map(({ suffix, first_seen_at }) => {
    const candidate = {
      ...device,
      device_id: `dev_${suffix}_first_seen`,
      hardware_hash: `hardware_hash_${suffix}_first_seen`,
      device_token: `device_token_${suffix}_first_seen`,
    };
    if (first_seen_at === undefined) delete candidate.first_seen_at;
    else candidate.first_seen_at = first_seen_at;
    return candidate;
  });
  writeFileSync(join(dataDir, "devices.json"), JSON.stringify({
    version: 2,
    devices: [device, ...invalidFirstSeenDevices],
  }));
  writeFileSync(join(dataDir, "usage-events.jsonl"), `${JSON.stringify({
    event_id: "usage_future_migrated",
    device_id: device.device_id,
    occurred_at: Math.floor(futureAt / 1_000),
    received_at: Math.floor(receivedAt / 1_000),
  })}\n`);
  writeFileSync(join(dataDir, "behavior-events.jsonl"), `${JSON.stringify({
    event_id: "behavior_future_migrated",
    event_name: "task_finished",
    device_id: device.device_id,
    attempt_id: "attempt_future_migrated",
    mode: "assistant",
    initiator_type: "user",
    status: "success",
    occurred_at: Math.floor(futureAt / 1_000),
    received_at: Math.floor(receivedAt / 1_000),
  })}\n`);

  const store = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
  const migratedDevice = store.devices.get(device.device_id);
  assert.ok(Math.abs(migratedDevice.first_seen_at - firstSeenAt) < 1_000);
  assert.ok(Math.abs(migratedDevice.last_seen_at - lastSeenAt) < 1_000);
  assert.ok(Math.abs(migratedDevice.last_active_at - receivedAt) < 1_000);
  for (const invalidDevice of invalidFirstSeenDevices) {
    assert.equal(store.devices.get(invalidDevice.device_id).first_seen_at, null);
  }
  assert.ok(Math.abs(store.events[0].occurred_at - receivedAt) < 1_000);
  assert.ok(Math.abs(store.behaviorEvents[0].occurred_at - receivedAt) < 1_000);

  const persistedDevices = JSON.parse(readFileSync(join(dataDir, "devices.json"), "utf8"));
  assert.equal(persistedDevices.devices[0].first_seen_at, migratedDevice.first_seen_at);
  assert.equal(persistedDevices.devices.filter((entry) => (
    invalidFirstSeenDevices.some((invalidDevice) => invalidDevice.device_id === entry.device_id)
      && entry.first_seen_at === null
  )).length, invalidFirstSeenDevices.length);
  const persistedUsage = JSON.parse(readFileSync(join(dataDir, "usage-events.jsonl"), "utf8").trim());
  const persistedBehavior = JSON.parse(readFileSync(join(dataDir, "behavior-events.jsonl"), "utf8").trim());
  assert.equal(persistedUsage.occurred_at, store.events[0].occurred_at);
  assert.equal(persistedBehavior.occurred_at, store.behaviorEvents[0].occurred_at);
});

test("device dashboard uses installation identities and core behavior exclusively", () => {
  const now = at("2026-09-04T12:00:00");
  const devices = [
    {
      device_id: "dev_legacy",
      first_seen_at: at("2026-09-01T08:00:00"),
      last_seen_at: now - 360_000,
      last_active_at: at("2026-09-03T09:00:00"),
      app_version: "0.8.3",
      hardware_source: "windows_cim_uuid",
    },
    {
      device_id: "dev_hw",
      first_seen_at: at("2026-09-02T08:00:00"),
      last_seen_at: now - 20_000,
      last_active_at: at("2026-09-04T10:05:00"),
      app_version: "0.9.1",
      hardware_source: "windows_cim_uuid",
    },
    {
      device_id: "dev_install",
      first_seen_at: at("2026-09-02T08:00:00"),
      last_seen_at: now - 10_000,
      app_version: "0.9.1",
      hardware_source: "installation_id",
    },
  ];
  const usageEvents = [
    { event_id: "usage_legacy_previous", device_id: "dev_legacy", occurred_at: at("2026-09-03T09:00:00") },
    { event_id: "usage_hw_1", device_id: "dev_hw", occurred_at: at("2026-09-04T10:00:00") },
    { event_id: "usage_hw_2", device_id: "dev_hw", occurred_at: at("2026-09-04T10:05:00") },
  ];
  const behaviorEvents = [
    {
      event_id: "behavior_installation_1",
      event_name: "task_finished",
      attempt_id: "attempt_installation_1",
      device_id: "dev_install",
      app_version: "0.9.1",
      mode: "assistant",
      initiator_type: "user",
      status: "success",
      occurred_at: at("2026-09-04T11:00:00"),
    },
    {
      event_id: "behavior_installation_duplicate",
      event_name: "task_finished",
      attempt_id: "attempt_installation_1",
      device_id: "dev_install",
      app_version: "0.9.1",
      mode: "assistant",
      initiator_type: "user",
      status: "success",
      occurred_at: at("2026-09-04T11:01:00"),
    },
    {
      event_id: "behavior_scheduled",
      event_name: "task_finished",
      attempt_id: "attempt_scheduled",
      device_id: "dev_install",
      app_version: "0.9.1",
      mode: "scheduled",
      initiator_type: "system",
      status: "success",
      occurred_at: at("2026-09-04T11:02:00"),
    },
    {
      event_id: "behavior_installation_2",
      event_name: "task_finished",
      attempt_id: "attempt_installation_2",
      device_id: "dev_install",
      app_version: "0.9.1",
      mode: "team",
      initiator_type: "user",
      status: "failed",
      occurred_at: at("2026-09-04T11:05:00"),
    },
  ];

  const overview = buildStatsOverview(devices, usageEvents, behaviorEvents, now);
  assert.deepEqual(overview.counts, { online: 1, active_today: 1, active_7d: 1 });
  assert.deepEqual(overview.usage_trend.at(-2), {
    day: "2026-09-03",
    active_devices: 0,
    turns: 0,
  });
  assert.deepEqual(overview.usage_trend.at(-1), {
    day: "2026-09-04",
    active_devices: 1,
    turns: 2,
  });
  assert.deepEqual(overview.active_versions, [
    { version: "0.9.1", count: 1 },
  ]);

  const list = buildDeviceList(devices, usageEvents, behaviorEvents, now);
  assert.deepEqual(list.map((device) => device.device_id), ["dev_install"]);
  assert.equal(list[0].turns_today, 2);
  assert.equal(list[0].turns_7d, 2);
});

test("homepage conversations and behavior successes share installation identity and deduplication", () => {
  const now = at("2026-09-04T12:00:00");
  const devices = [
    { device_id: "install_a", hardware_source: "installation_id", last_seen_at: now },
    { device_id: "install_b", hardware_source: "installation_id", last_seen_at: now },
    { device_id: "legacy_hw", hardware_source: "windows_cim_uuid", last_seen_at: now },
  ];
  const behaviorEvents = [
    { event_id: "submit_a", event_name: "task_submitted", attempt_id: "shared_attempt", device_id: "install_a", identity_id: "install_a", mode: "assistant", initiator_type: "user", occurred_at: now - 3_000 },
    { event_id: "finish_a", event_name: "task_finished", attempt_id: "shared_attempt", device_id: "install_a", identity_id: "install_a", mode: "assistant", initiator_type: "user", status: "success", occurred_at: now - 2_000 },
    { event_id: "submit_b", event_name: "task_submitted", attempt_id: "shared_attempt", device_id: "install_b", identity_id: "install_b", mode: "assistant", initiator_type: "user", occurred_at: now - 3_000 },
    { event_id: "finish_b", event_name: "task_finished", attempt_id: "shared_attempt", device_id: "install_b", identity_id: "install_b", mode: "assistant", initiator_type: "user", status: "failed", occurred_at: now - 2_000 },
    { event_id: "finish_legacy", event_name: "task_finished", attempt_id: "shared_attempt", device_id: "legacy_hw", identity_id: "legacy_hw", mode: "assistant", initiator_type: "user", status: "success", occurred_at: now - 1_000 },
  ];

  const homepage = buildStatsOverview(devices, [], behaviorEvents, now);
  const detail = buildDeviceList(devices, [], behaviorEvents, now);
  const behavior = buildBehaviorOverview(devices, [], behaviorEvents, "today", now);
  assert.equal(homepage.usage_trend.at(-1).turns, 2);
  assert.equal(homepage.counts.active_today, 2);
  assert.equal(detail.reduce((sum, device) => sum + device.turns_today, 0), 2);
  assert.equal(behavior.metrics.task_finished_count, 2);
  assert.equal(behavior.metrics.task_success_count, 1);
});

test("homepage and behavior activity include submitted tasks without terminal events", () => {
  const now = at("2026-09-04T12:00:00");
  const devices = [{
    device_id: "install_pending",
    hardware_source: "installation_id",
    identity_quality: "installation_only",
    first_seen_at: now - 10_000,
    last_seen_at: now,
  }];
  const behaviorEvents = [{
    event_id: "pending_submit",
    event_name: "task_submitted",
    attempt_id: "pending_attempt",
    device_id: "install_pending",
    identity_id: "install_pending",
    mode: "assistant",
    initiator_type: "user",
    occurred_at: now,
  }];

  const homepage = buildStatsOverview(devices, [], behaviorEvents, now);
  const behavior = buildBehaviorOverview(devices, [], behaviorEvents, "today", now);
  assert.equal(homepage.counts.active_today, 1);
  assert.equal(homepage.usage_trend.at(-1).turns, 0);
  assert.equal(behavior.active_installations, 1);
});

test("conversation deduplication falls back per installation to turn and event ids", () => {
  const now = at("2026-09-04T12:00:00");
  const devices = [
    { device_id: "install_a", hardware_source: "installation_id" },
    { device_id: "install_b", hardware_source: "installation_id" },
  ];
  const behaviorEvents = [
    { event_id: "event_a_1", event_name: "task_finished", turn_id: "shared_turn", device_id: "install_a", identity_id: "install_a", mode: "assistant", initiator_type: "user", status: "success", occurred_at: now },
    { event_id: "event_a_duplicate", event_name: "task_finished", turn_id: "shared_turn", device_id: "install_a", identity_id: "install_a", mode: "assistant", initiator_type: "user", status: "success", occurred_at: now },
    { event_id: "event_b_1", event_name: "task_finished", turn_id: "shared_turn", device_id: "install_b", identity_id: "install_b", mode: "assistant", initiator_type: "user", status: "success", occurred_at: now },
    { event_id: "event_only", event_name: "task_finished", device_id: "install_a", identity_id: "install_a", mode: "assistant", initiator_type: "user", status: "failed", occurred_at: now },
  ];

  assert.equal(buildStatsOverview(devices, [], behaviorEvents, now).usage_trend.at(-1).turns, 3);
  assert.equal(buildBehaviorOverview(devices, [], behaviorEvents, "today", now).metrics.task_finished_count, 3);
});

test("homepage and behavior task totals share clipping before installation-scoped deduplication", () => {
  const now = at("2026-09-04T12:00:00");
  const devices = [{ device_id: "install_clip", hardware_source: "installation_id" }];
  const behaviorEvents = [
    { event_id: "duplicate_first", event_name: "task_finished", attempt_id: "duplicate", device_id: "install_clip", mode: "assistant", initiator_type: "user", status: "success", occurred_at: now - 1_000 },
    { event_id: "duplicate_second", event_name: "task_finished", attempt_id: "duplicate", device_id: "install_clip", mode: "assistant", initiator_type: "user", status: "success", occurred_at: now - 999 },
    ...Array.from({ length: 99 }, (_, index) => ({
      event_id: `clipped_finished_${index}`,
      event_name: "task_finished",
      attempt_id: `unique_${index}`,
      device_id: "install_clip",
      mode: "assistant",
      initiator_type: "user",
      status: "success",
      occurred_at: now - 900 + index,
    })),
  ];
  const homepage = buildStatsOverview(devices, [], behaviorEvents, now);
  const behavior = buildBehaviorOverview(devices, [], behaviorEvents, "today", now);

  assert.equal(homepage.usage_trend.at(-1).turns, 99);
  assert.equal(buildDeviceList(devices, [], behaviorEvents, now)[0].turns_today, 99);
  assert.equal(behavior.metrics.task_finished_count, 99);
  assert.equal(behavior.metrics.task_success_count, 99);
});

test("scene and model task associations are isolated by installation", () => {
  const now = at("2026-09-04T12:00:00");
  const events = [
    { event_id: "scene_a", event_name: "scene_submitted", operation_id: "shared_scene", attempt_id: "shared_attempt", scene_l1: "work", scene_l2: "writing", identity_id: "install_a", session_id: "session_a", occurred_at: now - 6_000 },
    { event_id: "scene_b", event_name: "scene_submitted", operation_id: "shared_scene", attempt_id: "shared_attempt", scene_l1: "work", scene_l2: "writing", identity_id: "install_b", session_id: "session_b", occurred_at: now - 6_000 },
    { event_id: "model_a", event_name: "model_used", attempt_id: "shared_attempt", provider_key: "deepseek", model_key: "deepseek", identity_id: "install_a", occurred_at: now - 5_000 },
    { event_id: "model_b", event_name: "model_used", attempt_id: "shared_attempt", provider_key: "deepseek", model_key: "deepseek", identity_id: "install_b", occurred_at: now - 5_000 },
    { event_id: "finish_a", event_name: "task_finished", attempt_id: "shared_attempt", session_id: "session_a", turn_id: "shared_turn", status: "success", duration_ms: 100, identity_id: "install_a", occurred_at: now - 4_000 },
    { event_id: "finish_b", event_name: "task_finished", attempt_id: "shared_attempt", session_id: "session_b", turn_id: "shared_turn", status: "failed", duration_ms: 900, identity_id: "install_b", occurred_at: now - 4_000 },
    { event_id: "feedback_a", event_name: "response_feedback_changed", session_id: "session_a", turn_id: "shared_turn", feedback_action: "set", rating: "positive", identity_id: "install_a", occurred_at: now - 3_000 },
    { event_id: "feedback_b", event_name: "response_feedback_changed", session_id: "session_b", turn_id: "shared_turn", feedback_action: "set", rating: "negative", identity_id: "install_b", occurred_at: now - 2_000 },
  ];
  const devices = installationDevicesFor(events);
  const scene = buildBehaviorSceneRanking(devices, [], events, "today", now, { minimumGroupSubjects: 0 }).rows[0];
  const model = buildBehaviorModelRanking(devices, [], events, "today", now, { minimumGroupSubjects: 0 }).rows[0];

  assert.equal(scene.trigger_count, 2);
  assert.equal(scene.task_duration_p50_ms, 100);
  assert.equal(scene.negative_feedback_rate, null);
  assert.equal(model.task_terminal_count, 2);
  assert.equal(model.task_success_count, 1);
  assert.equal(model.task_duration_p50_ms, 100);
});

test("scene value signals require the same installation, session, and turn", () => {
  const now = at("2026-09-04T12:00:00");
  const events = Array.from({ length: 30 }, (_, index) => {
    const base = now - 60_000 + index * 1_000;
    return [
      { event_id: `scene_${index}`, event_name: "scene_submitted", operation_id: `scene_${index}`, attempt_id: `attempt_${index}`, scene_l1: "work", scene_l2: "writing", identity_id: "install_a", session_id: "session_a", occurred_at: base },
      { event_id: `finish_${index}`, event_name: "task_finished", attempt_id: `attempt_${index}`, session_id: "session_a", turn_id: `turn_${index}`, mode: "assistant", initiator_type: "user", status: "success", duration_ms: 100, identity_id: "install_a", occurred_at: base + 100 },
      { event_id: `cross_session_copy_${index}`, event_name: "response_copied", session_id: "session_b", turn_id: `turn_${index}`, identity_id: "install_a", occurred_at: base + 200 },
    ];
  }).flat();
  events.push({ event_id: "cross_session_feedback", event_name: "response_feedback_changed", session_id: "session_b", turn_id: "turn_0", feedback_action: "set", rating: "negative", identity_id: "install_a", occurred_at: now - 1_000 });

  const devices = installationDevicesFor(events);
  const crossSessionOnly = buildBehaviorSceneRanking(devices, [], events, "today", now, { minimumGroupSubjects: 0 }).rows[0];
  assert.equal(crossSessionOnly.explicit_value_sample_size, 30);
  assert.equal(crossSessionOnly.explicit_value_rate, 0);
  assert.equal(crossSessionOnly.negative_feedback_rate, 0);

  const sameSessionEvents = [
    ...events,
    { event_id: "same_session_copy", event_name: "response_copied", session_id: "session_a", turn_id: "turn_0", identity_id: "install_a", occurred_at: now - 900 },
    { event_id: "same_session_feedback", event_name: "response_feedback_changed", session_id: "session_a", turn_id: "turn_1", feedback_action: "set", rating: "negative", identity_id: "install_a", occurred_at: now - 800 },
  ];
  const sameSession = buildBehaviorSceneRanking(devices, [], sameSessionEvents, "today", now, { minimumGroupSubjects: 0 }).rows[0];
  assert.equal(sameSession.explicit_value_rate, 1 / 30);
  assert.equal(sameSession.negative_feedback_rate, 1 / 30);
});

test("tool funnel operation identifiers are isolated by installation", () => {
  const now = at("2026-09-04T12:00:00");
  const events = ["install_a", "install_b"].flatMap((identity) => [
    { event_id: `impression_${identity}`, event_name: "tool_impression", operation_id: "shared_operation", tool_key: "search", tool_type: "mcp", identity_id: identity, occurred_at: now - 3_000 },
    { event_id: `started_${identity}`, event_name: "tool_install_started", operation_id: "shared_operation", tool_key: "search", tool_type: "mcp", identity_id: identity, occurred_at: now - 2_000 },
    { event_id: `completed_${identity}`, event_name: "tool_install_completed", operation_id: "shared_operation", tool_key: "search", tool_type: "mcp", success: true, identity_id: identity, occurred_at: now - 1_000 },
  ]);
  const funnel = buildBehaviorToolRanking(
    installationDevicesFor(events), [], events, "today", "install", now, { minimumGroupSubjects: 0 },
  ).funnel_rows[0];

  assert.equal(funnel.impression_count, 2);
  assert.equal(funnel.install_started_count, 2);
  assert.equal(funnel.install_succeeded_count, 2);
});

test("behavior status and pipeline quality include verifiable installation facts only", () => {
  const now = at("2026-09-04T12:00:00");
  const devices = [
    { device_id: "install_a", hardware_source: "installation_id", behavior_collection: { status: "inactive" } },
    { device_id: "legacy_hardware", hardware_source: "windows_cim_uuid", behavior_collection: { status: "active" } },
  ];
  const pipelineEvents = [
    { occurred_at: now, accepted: 0, rejected: 100 },
    { occurred_at: now, device_id: "legacy_hardware", accepted: 0, rejected: 100 },
    { occurred_at: now, device_id: "install_a", accepted: 99, rejected: 1 },
  ];
  const overview = buildBehaviorOverview(devices, [], [], "today", now, pipelineEvents);

  assert.equal(overview.data_status, "tracking_not_enabled");
  assert.equal(overview.metrics.pipeline_accepted_count, 99);
  assert.equal(overview.metrics.pipeline_rejection_rate, 0.01);
});

test("behavior first-output metrics exclude scheduled and system tasks", () => {
  const now = at("2026-09-04T12:00:00");
  const devices = [{ device_id: "install_a", hardware_source: "installation_id" }];
  const behaviorEvents = [
    { event_id: "core_submit", event_name: "task_submitted", attempt_id: "core", identity_id: "install_a", mode: "assistant", initiator_type: "user", occurred_at: now - 2_000 },
    { event_id: "core_output", event_name: "task_first_output", attempt_id: "core", identity_id: "install_a", duration_ms: 100, occurred_at: now - 1_000 },
    { event_id: "system_submit", event_name: "task_submitted", attempt_id: "system", identity_id: "install_a", mode: "scheduled", initiator_type: "system", occurred_at: now - 2_000 },
    { event_id: "system_output", event_name: "task_first_output", attempt_id: "system", identity_id: "install_a", duration_ms: 1, occurred_at: now - 1_000 },
  ];

  const overview = buildBehaviorOverview(devices, [], behaviorEvents, "today", now);
  assert.equal(overview.metrics.task_submitted_count, 1);
  assert.equal(overview.metrics.task_first_output_p50_ms, 100);
  assert.equal(overview.metrics.task_first_output_p90_ms, 100);
});

test("device inventory contains installation registrations only", () => {
  const now = at("2026-09-04T12:00:00");
  const hardware = {
    device_id: "dev_hw_current",
    first_seen_at: at("2026-09-04T08:00:00"),
    last_seen_at: now - 10_000,
    app_version: "0.9.1",
    hardware_source: "windows_cim_uuid",
  };
  const idleBehavior = {
    device_id: "dev_install_idle",
    first_seen_at: at("2026-09-04T08:01:00"),
    last_seen_at: now - 5_000,
    app_version: "0.9.1",
    hardware_source: "installation_id",
  };
  const unexpectedBehavior = {
    ...idleBehavior,
    device_id: "dev_install_unexpected",
  };

  for (const [devices, expected] of [[[hardware], 0], [[hardware, idleBehavior, unexpectedBehavior], 2]]) {
    const overview = buildStatsOverview(devices, [], [], now);
    assert.deepEqual(overview.counts, { online: expected, active_today: 0, active_7d: 0 });
    const list = buildDeviceList(devices, [], [], now);
    assert.equal(list.length, expected);
    assert.equal(list.some((device) => device.device_id === "dev_hw_current"), false);
  }
});

test("legacy hardware usage does not enter the new dashboard", () => {
  const now = at("2026-09-04T12:00:00");
  const devices = [{
    device_id: "dev_hw_upgraded",
    first_seen_at: at("2026-08-01T08:00:00"),
    last_seen_at: now - 10_000,
    last_active_at: at("2026-09-04T10:00:00"),
    app_version: "0.9.1",
    hardware_source: "windows_cim_uuid",
  }];
  const usageEvents = [{
    event_id: "usage_before_upgrade_without_version",
    device_id: "dev_hw_upgraded",
    occurred_at: at("2026-09-04T10:00:00"),
  }];

  const overview = buildStatsOverview(devices, usageEvents, [], now);
  assert.deepEqual(overview.counts, { online: 0, active_today: 0, active_7d: 0 });
  assert.equal(overview.usage_trend.at(-1).turns, 0);
  assert.equal(buildDeviceList(devices, usageEvents, [], now).length, 0);
});

test("scheduled and system behavior tasks never enter homepage metrics", () => {
  const now = at("2026-09-04T12:00:00");
  const devices = [
    {
      device_id: "dev_hw",
      first_seen_at: at("2026-09-04T08:00:00"),
      last_seen_at: now - 10_000,
      app_version: "0.8.3",
      hardware_source: "windows_cim_uuid",
    },
    {
      device_id: "dev_install",
      first_seen_at: at("2026-09-04T08:00:00"),
      last_seen_at: now - 5_000,
      app_version: "0.9.1",
      hardware_source: "installation_id",
    },
  ];
  const usageEvents = [
    { event_id: "usage_hw", device_id: "dev_hw", occurred_at: at("2026-09-04T10:00:00") },
  ];
  const behaviorEvents = [
    {
      event_id: "behavior_system",
      event_name: "task_finished",
      attempt_id: "attempt_system",
      device_id: "dev_install",
      app_version: "0.9.1",
      mode: "scheduled",
      initiator_type: "system",
      status: "success",
      occurred_at: at("2026-09-04T11:00:00"),
    },
  ];

  const overview = buildStatsOverview(devices, usageEvents, behaviorEvents, now);
  assert.deepEqual(overview.counts, { online: 1, active_today: 0, active_7d: 0 });
  assert.deepEqual(overview.usage_trend.at(-1), {
    day: "2026-09-04",
    active_devices: 0,
    turns: 0,
  });
  assert.deepEqual(buildDeviceList(devices, usageEvents, behaviorEvents, now)
    .map((device) => device.device_id), ["dev_install"]);
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
    { event_id: "task_done_a", event_name: "task_finished", attempt_id: "attempt_a", session_id: "session-a", turn_id: "turn_a", identity_id: "dev_a", mode: "assistant", initiator_type: "user", status: "success", occurred_at: at("2026-08-10T09:02:00") },
    { event_id: "copy_a", event_name: "response_copied", session_id: "session-a", turn_id: "turn_a", identity_id: "dev_a", content_type: "text", occurred_at: at("2026-08-10T09:22:00") },
    { event_id: "task_b", event_name: "task_submitted", attempt_id: "attempt_b", identity_id: "dev_b", mode: "code", initiator_type: "user", occurred_at: at("2026-08-10T10:01:00") },
    { event_id: "task_admit_b", event_name: "task_admitted", attempt_id: "attempt_b", identity_id: "dev_b", occurred_at: at("2026-08-10T10:01:01") },
    { event_id: "task_done_b", event_name: "task_finished", attempt_id: "attempt_b", identity_id: "dev_b", mode: "code", initiator_type: "user", status: "interrupted", occurred_at: at("2026-08-10T10:02:00") },
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
  for (const device of devices) device.hardware_source = "installation_id";

  const overview = buildBehaviorOverview(devices, usageEvents, behaviorEvents, "7", now, [
    { occurred_at: now, device_id: "dev_a", accepted: 90, duplicate: 5, rejected: 3, retryable: 2 },
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
  const mcpTool = tools.rows.find(row => row.tool_type === "mcp");
  const builtinTool = tools.rows.find(row => row.tool_type === "builtin");
  assert.equal(mcpTool.tool_key, "search");
  assert.equal(mcpTool.tool_name, "search");
  assert.equal(mcpTool.call_count, 1);
  assert.equal(mcpTool.user_count, 1);
  assert.equal(mcpTool.observed_user_count, 1);
  assert.equal(mcpTool.success_rate, null);
  assert.equal(builtinTool.tool_key, "search");
  assert.equal(builtinTool.tool_name, "search");
  assert.equal(builtinTool.call_count, 1);
  assert.equal(builtinTool.user_count, 0);
  assert.equal(builtinTool.observed_user_count, 1);

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
      behaviorEvent(collection.collection_period_id, {
        event_id: "behavior_event_test_000007",
        occurred_at: Date.now() + 10 * 60_000,
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
      { event_id: "behavior_event_test_000007", status: "rejected", reason_code: "invalid_time" },
    ]);
    assert.equal(result.results.length, 8);
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
      device_id: credential.device_id,
      accepted: 2,
      duplicate: 1,
      rejected: 5,
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
  const finished = (id, identity, turn, time, sessionId, status = "success") => ({
    event_id: id,
    event_name: "task_finished",
    attempt_id: `attempt_${id}`,
    turn_id: turn,
    identity_id: identity,
    mode: "assistant",
    initiator_type: "user",
    status,
    ...(sessionId ? { session_id: sessionId } : {}),
    occurred_at: at(time),
  });
  const successful = (id, identity, turn, time, sessionId) => (
    finished(id, identity, turn, time, sessionId)
  );
  const events = [
    successful("a", "dev_a", "turn_a", "2026-08-10T09:00:00", "session-a"),
    { event_id: "positive_a", event_name: "response_feedback_changed", feedback_id: "feedback_a", feedback_action: "set", rating: "positive", session_id: "session-a", turn_id: "turn_a", identity_id: "dev_a", occurred_at: at("2026-08-10T09:10:00") },
    { event_id: "clear_a", event_name: "response_feedback_changed", feedback_id: "feedback_a", feedback_action: "clear", rating: "positive", session_id: "session-a", turn_id: "turn_a", identity_id: "dev_a", occurred_at: at("2026-08-10T09:11:00") },
    successful("b", "dev_b", "turn_b", "2026-08-10T10:00:00", "session-b"),
    { event_id: "late_copy_b", event_name: "response_copied", session_id: "session-b", turn_id: "turn_b", identity_id: "dev_b", occurred_at: at("2026-08-10T10:31:00") },
    successful("c", "dev_c", "turn_c", "2026-08-10T11:00:00", "session-c"),
    { event_id: "wrong_turn_c", event_name: "artifact_exported", session_id: "session-c", turn_id: "turn_other", identity_id: "dev_c", occurred_at: at("2026-08-10T11:05:00") },
    successful("d", "dev_d", "turn_d", "2026-08-10T12:00:00", "session-d"),
    { event_id: "copy_d", event_name: "response_copied", session_id: "session-d", turn_id: "turn_d", identity_id: "dev_d", occurred_at: at("2026-08-10T12:30:00") },
    successful("boundary", "dev_boundary", "turn_boundary", "2026-08-09T23:50:00", "session-boundary"),
    { event_id: "cross_week_copy", event_name: "response_copied", session_id: "session-boundary", turn_id: "turn_boundary", identity_id: "dev_boundary", occurred_at: at("2026-08-10T00:05:00") },
    successful("negative", "dev_negative", "turn_negative", "2026-08-10T13:00:00", "session-negative"),
    { event_id: "negative_feedback", event_name: "response_feedback_changed", feedback_id: "feedback_negative", feedback_action: "set", rating: "negative", session_id: "session-negative", turn_id: "wrong_negative_turn", identity_id: "dev_negative", occurred_at: at("2026-08-10T13:05:00") },
    successful("cross_session", "dev_cross_session", "shared_turn", "2026-08-10T10:00:00", "session-a"),
    { event_id: "cross_session_copy", event_name: "response_copied", session_id: "session-b", turn_id: "shared_turn", identity_id: "dev_cross_session", occurred_at: at("2026-08-10T10:01:00") },
    { event_id: "cross_session_positive", event_name: "response_feedback_changed", feedback_id: "cross_session_positive", feedback_action: "set", rating: "positive", session_id: "session-b", turn_id: "shared_turn", identity_id: "dev_cross_session", occurred_at: at("2026-08-10T10:02:00") },
    { event_id: "cross_session_artifact", event_name: "artifact_exported", session_id: "session-b", turn_id: "shared_turn", identity_id: "dev_cross_session", occurred_at: at("2026-08-10T10:03:00") },
    { event_id: "cross_session_negative", event_name: "response_feedback_changed", feedback_id: "cross_session_negative", feedback_action: "set", rating: "negative", session_id: "session-b", turn_id: "shared_turn", identity_id: "dev_cross_session", occurred_at: at("2026-08-10T10:04:00") },
    successful("failed_neighbor", "dev_failed_neighbor", "turn-ok", "2026-08-10T14:30:00", "session-terminal"),
    finished("failed_terminal", "dev_failed_neighbor", "turn-failed", "2026-08-10T14:01:00", "session-terminal", "failed"),
    { event_id: "failed_turn_copy", event_name: "response_copied", session_id: "session-terminal", turn_id: "turn-failed", identity_id: "dev_failed_neighbor", occurred_at: at("2026-08-10T14:40:00") },
    successful("interrupted_neighbor", "dev_interrupted_neighbor", "turn-ok", "2026-08-10T15:00:00", "session-interrupted"),
    finished("interrupted_terminal", "dev_interrupted_neighbor", "turn-interrupted", "2026-08-10T15:01:00", "session-interrupted", "interrupted"),
    { event_id: "interrupted_artifact", event_name: "artifact_exported", session_id: "session-interrupted", turn_id: "turn-interrupted", identity_id: "dev_interrupted_neighbor", occurred_at: at("2026-08-10T15:02:00") },
    { event_id: "interrupted_feedback", event_name: "response_feedback_changed", feedback_id: "interrupted_feedback", feedback_action: "set", rating: "positive", session_id: "session-interrupted", turn_id: "turn-interrupted", identity_id: "dev_interrupted_neighbor", occurred_at: at("2026-08-10T15:03:00") },
    successful("legacy", "dev_legacy", "turn-legacy", "2026-08-10T16:00:00"),
    { event_id: "future_copy", event_name: "response_copied", session_id: "session-c", turn_id: "wrong_future_turn", identity_id: "dev_c", occurred_at: at("2026-08-12T12:01:00") },
  ];
  assert.deepEqual(buildWeeklyExplicitValueMetric(events, now), {
    wevta_installations: 1,
    weekly_successful_task_installations: 9,
    explicit_value_sample_size: 8,
    explicit_value_ineligible_installations: 1,
    explicit_value_confidence: "low",
    wevta_rate: null,
    positive_feedback_installations: 0,
    response_copied_installations: 1,
    artifact_exported_installations: 0,
    negative_feedback_installations: 0,
    negative_feedback_rate: null,
    turn_mismatch_value_signal_count: 1,
  });
  const overview = buildBehaviorOverview(installationDevicesFor(events), [], events, "30", now);
  assert.equal(overview.metrics.explicit_value_ineligible_installations, 1);
  assert(overview.data_quality.warnings.includes("explicit_value_join_key_missing"));
});

test("weekly explicit value feedback state is isolated by installation for explicit and fallback ids", () => {
  const now = at("2026-08-12T12:00:00");
  const successful = (identity, attempt, turn, sessionId, occurredAt) => ({
    event_id: `finished_${identity}`,
    event_name: "task_finished",
    attempt_id: attempt,
    turn_id: turn,
    identity_id: identity,
    mode: "assistant",
    initiator_type: "user",
    status: "success",
    session_id: sessionId,
    occurred_at: occurredAt,
  });
  const events = [
    successful("explicit_a", "shared_attempt", "shared_turn", "session-explicit", at("2026-08-10T09:00:00")),
    successful("explicit_b", "shared_attempt", "shared_turn", "session-explicit", at("2026-08-10T09:00:00")),
    { event_id: "feedback_explicit_a", event_name: "response_feedback_changed", feedback_id: "shared_feedback", feedback_action: "set", rating: "positive", session_id: "session-explicit", turn_id: "shared_turn", identity_id: "explicit_a", occurred_at: at("2026-08-10T09:05:00") },
    { event_id: "feedback_explicit_b", event_name: "response_feedback_changed", feedback_id: "shared_feedback", feedback_action: "clear", rating: "positive", session_id: "session-explicit", turn_id: "shared_turn", identity_id: "explicit_b", occurred_at: at("2026-08-10T09:06:00") },
    successful("fallback_a", "fallback_attempt", "fallback_turn", "shared_session", at("2026-08-10T10:00:00")),
    successful("fallback_b", "fallback_attempt", "fallback_turn", "shared_session", at("2026-08-10T10:00:00")),
    { event_id: "feedback_fallback_a", event_name: "response_feedback_changed", session_id: "shared_session", turn_id: "fallback_turn", feedback_action: "set", rating: "positive", identity_id: "fallback_a", occurred_at: at("2026-08-10T10:05:00") },
    { event_id: "feedback_fallback_b", event_name: "response_feedback_changed", session_id: "shared_session", turn_id: "fallback_turn", feedback_action: "clear", rating: "positive", identity_id: "fallback_b", occurred_at: at("2026-08-10T10:06:00") },
  ];
  const metric = buildWeeklyExplicitValueMetric(events, now);

  assert.equal(metric.weekly_successful_task_installations, 4);
  assert.equal(metric.explicit_value_sample_size, 4);
  assert.equal(metric.explicit_value_ineligible_installations, 0);
  assert.equal(metric.wevta_installations, 2);
  assert.equal(metric.positive_feedback_installations, 2);
});

test("version quality counts and durations isolate identical attempts by installation", () => {
  const now = at("2026-08-12T12:00:00");
  const events = [];
  for (let index = 0; index < 30; index += 1) {
    const common = {
      attempt_id: "shared_attempt",
      identity_id: `version_install_${index}`,
      app_version: "0.9.1",
      platform: "windows",
      arch: "x86_64",
      occurred_at: now - 1_000 + index,
    };
    events.push(
      { ...common, event_id: `version_submit_${index}`, event_name: "task_submitted" },
      { ...common, event_id: `version_admit_${index}`, event_name: "task_admitted" },
      { ...common, event_id: `version_output_${index}`, event_name: "task_first_output", duration_ms: index + 1 },
      { ...common, event_id: `version_finish_${index}`, event_name: "task_finished", status: index < 15 ? "success" : "cancelled", duration_ms: index + 1 },
    );
  }
  const quality = buildBehaviorOverview(
    installationDevicesFor(events), [], events, "today", now,
  ).version_quality[0];

  assert.equal(quality.sample_size, 30);
  assert.equal(quality.admission_rate, 1);
  assert.equal(quality.success_rate, 0.5);
  assert.equal(quality.cancellation_rate, 0.5);
  assert.equal(quality.first_output_p50_ms, 15);
  assert.equal(quality.first_output_p90_ms, 27);
  assert.equal(quality.total_duration_p50_ms, 15);
  assert.equal(quality.total_duration_p90_ms, 27);
});

test("voice funnel operations and successful attempts are isolated by installation", () => {
  const now = at("2026-08-12T12:00:00");
  const events = ["voice_a", "voice_b"].flatMap((identity) => [
    { event_id: `entry_${identity}`, event_name: "voice_entry_clicked", operation_id: "shared_operation", identity_id: identity, occurred_at: now - 6_000 },
    { event_id: `permission_${identity}`, event_name: "voice_permission_result", operation_id: "shared_operation", voice_permission_status: "granted", identity_id: identity, occurred_at: now - 5_000 },
    { event_id: `started_${identity}`, event_name: "voice_started", operation_id: "shared_operation", identity_id: identity, occurred_at: now - 4_000 },
    { event_id: `recognized_${identity}`, event_name: "voice_recognition_completed", operation_id: "shared_operation", identity_id: identity, occurred_at: now - 3_000 },
    { event_id: `submitted_${identity}`, event_name: "voice_message_submitted", operation_id: "shared_operation", attempt_id: "shared_attempt", identity_id: identity, occurred_at: now - 2_000 },
    { event_id: `finished_${identity}`, event_name: "task_finished", attempt_id: "shared_attempt", identity_id: identity, mode: "assistant", initiator_type: "user", status: "success", occurred_at: now - 1_000 },
  ]);
  const voice = buildBehaviorOverview(
    installationDevicesFor(events), [], events, "today", now,
  ).funnels.voice;

  assert.equal(voice.entry_clicked, 2);
  assert.equal(voice.permission_granted, 2);
  assert.equal(voice.started, 2);
  assert.equal(voice.recognition_completed, 2);
  assert.equal(voice.message_submitted, 2);
  assert.equal(voice.task_succeeded, 2);
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
    { event_id: "voice_entry", event_name: "voice_entry_clicked", operation_id: "voice_op", identity_id: "quality_0", occurred_at: at("2026-08-12T09:00:00") },
    { event_id: "voice_permission", event_name: "voice_permission_result", operation_id: "voice_op", voice_permission_status: "granted", identity_id: "quality_0", occurred_at: at("2026-08-12T09:00:01") },
    { event_id: "voice_started", event_name: "voice_started", operation_id: "voice_op", identity_id: "quality_0", occurred_at: at("2026-08-12T09:00:02") },
    { event_id: "voice_recognized", event_name: "voice_recognition_completed", operation_id: "voice_op", identity_id: "quality_0", occurred_at: at("2026-08-12T09:00:03") },
    { event_id: "voice_submitted", event_name: "voice_message_submitted", operation_id: "voice_op", attempt_id: "quality_attempt_0", identity_id: "quality_0", occurred_at: at("2026-08-12T09:00:04") },
  ];
  for (let index = 0; index < 30; index += 1) {
    const attempt = `quality_attempt_${index}`;
    const common = { attempt_id: attempt, identity_id: `quality_${index}`, app_version: "0.8.7", platform: "windows", arch: "x86_64", mode: "assistant", initiator_type: "user" };
    behaviorEvents.push(
      { ...common, event_id: `submitted_${index}`, event_name: "task_submitted", occurred_at: at("2026-08-12T09:01:00") + index },
      { ...common, event_id: `admitted_${index}`, event_name: "task_admitted", occurred_at: at("2026-08-12T09:01:01") + index },
      { ...common, event_id: `output_${index}`, event_name: "task_first_output", duration_ms: index + 1, occurred_at: at("2026-08-12T09:01:02") + index },
      { ...common, event_id: `finished_${index}`, event_name: "task_finished", turn_id: `turn_${index}`, mode: "assistant", initiator_type: "user", status: "success", duration_ms: (index + 1) * 10, occurred_at: at("2026-08-12T09:01:03") + index },
    );
  }
  const overview = buildBehaviorOverview(installationDevicesFor(behaviorEvents), [], behaviorEvents, "30", now);
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
    { event_id: "scene_submit", event_name: "scene_submitted", operation_id: "scene_submit", attempt_id: "scene_attempt", scene_l1: "work", scene_l2: "document-writing", identity_id: "dev_a", session_id: "session_a", occurred_at: at("2026-08-12T10:02:00") },
    { event_id: "scene_finish", event_name: "task_finished", attempt_id: "scene_attempt", session_id: "session_a", turn_id: "scene_turn", status: "success", duration_ms: 1200, identity_id: "dev_a", occurred_at: at("2026-08-12T10:03:00") },
    { event_id: "scene_copy", event_name: "response_copied", session_id: "session_a", turn_id: "scene_turn", identity_id: "dev_a", occurred_at: at("2026-08-12T10:04:00") },
  ];
  const tools = buildBehaviorToolRanking(installationDevicesFor(events), [], events, "today", "install", now, { minimumGroupSubjects: 0 });
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
  const scenes = buildBehaviorSceneRanking(installationDevicesFor(events), [], events, "today", now, { minimumGroupSubjects: 0 });
  assert.equal(scenes.rows[0].selection_rate, null);
  assert.equal(scenes.rows[0].submission_rate, null);
  assert.equal(scenes.rows[0].task_success_rate, null);
  assert.equal(scenes.rows[0].explicit_value_rate, null);
  assert.equal(scenes.rows[0].explicit_value_sample_size, 1);
  assert.equal(scenes.rows[0].task_duration_p50_ms, 1200);
  assert.equal(buildBehaviorToolRanking(installationDevicesFor(events), [], events, "today", "install", now).funnel_rows.length, 1);
  assert.equal(buildBehaviorSceneRanking(installationDevicesFor(events), [], events, "today", now).rows.length, 1);
});

test("retention ignores opens and failures while terminal completeness excludes tasks still within 24h", () => {
  const now = at("2026-08-12T12:00:00");
  const base = [
    { event_id: "cohort_retention", event_name: "analytics_cohort_started", identity_id: "retention_subject", occurred_at: at("2026-08-10T08:00:00") },
    { event_id: "retention_open", event_name: "app_opened", identity_id: "retention_subject", occurred_at: at("2026-08-11T08:00:00") },
    { event_id: "retention_failed", event_name: "task_finished", attempt_id: "failed_retention", mode: "assistant", initiator_type: "user", status: "failed", identity_id: "retention_subject", occurred_at: at("2026-08-11T09:00:00") },
    { event_id: "submitted_recent", event_name: "task_submitted", attempt_id: "recent_attempt", mode: "assistant", initiator_type: "user", identity_id: "recent_subject", occurred_at: at("2026-08-12T10:59:00") },
    { event_id: "admitted_recent", event_name: "task_admitted", attempt_id: "recent_attempt", identity_id: "recent_subject", occurred_at: at("2026-08-12T11:00:00") },
  ];
  const recentOnly = buildBehaviorOverview(installationDevicesFor(base), [], base, "30", now);
  assert.equal(recentOnly.retention.d1.retained_installations, 0);
  assert.equal(recentOnly.metrics.task_terminal_matured_count, 0);
  assert.equal(recentOnly.metrics.task_terminal_pending_within_24h_count, 1);
  assert(!recentOnly.data_quality.warnings.includes("task_terminal_incomplete"));

  const maturedEvents = [...base,
    { event_id: "submitted_matured", event_name: "task_submitted", attempt_id: "matured_attempt", mode: "assistant", initiator_type: "user", identity_id: "matured_subject", occurred_at: at("2026-08-10T08:59:00") },
    { event_id: "admitted_matured", event_name: "task_admitted", attempt_id: "matured_attempt", identity_id: "matured_subject", occurred_at: at("2026-08-10T09:00:00") },
  ];
  const matured = buildBehaviorOverview(installationDevicesFor(maturedEvents), [], maturedEvents, "30", now);
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
    installationDevicesFor(events),
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

  const tool = buildBehaviorToolRanking(installationDevicesFor(events), [], events, "today", "call", now, { minimumGroupSubjects: 0 }).rows[0];
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
  const overview = buildBehaviorOverview(installationDevicesFor(events), [], events, "today", now);
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
  const overview = buildBehaviorOverview(installationDevicesFor(events), [], events, "30", now);
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
  const model = buildBehaviorModelRanking(installationDevicesFor(events), [], events, "30", now).rows[0];
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
  const rows = buildBehaviorToolRanking(installationDevicesFor(events), [], events, "today", "call", now).rows;
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
    const tools = buildBehaviorToolRanking(installationDevicesFor(store.behaviorEvents), [], store.behaviorEvents, "today", "call", Date.now(), { minimumGroupSubjects: 0 });
    const models = buildBehaviorModelRanking(installationDevicesFor(store.behaviorEvents), [], store.behaviorEvents, "today", Date.now(), { minimumGroupSubjects: 0 });
    assert.equal(tools.rows[0].tool_name, "custom");
    assert.equal(models.rows[0].model_key, "custom");
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("tool rankings expose normalized new names without reconstructing historical custom names", () => {
  const now = at("2026-08-12T12:00:00");
  const events = [
    { event_id: "named_tool", event_name: "tool_call_completed", operation_id: "named_op", identity_id: "dev_a", tool_key: "mcp_custom", tool_name: "mcp__private_crm__lookup", tool_type: "mcp", success: true, occurred_at: at("2026-08-12T10:00:00") },
    { event_id: "legacy_tool", event_name: "tool_call_completed", operation_id: "legacy_op", identity_id: "dev_b", tool_key: "mcp_custom", tool_type: "mcp", success: true, occurred_at: at("2026-08-12T10:01:00") },
    { event_id: "known_search", event_name: "tool_call_completed", operation_id: "known_search_op", identity_id: "dev_a", tool_key: "search", tool_name: "search", tool_type: "mcp", success: true, occurred_at: at("2026-08-12T10:02:00") },
    { event_id: "custom_search", event_name: "tool_call_completed", operation_id: "custom_search_op", identity_id: "dev_b", tool_key: "mcp_custom", tool_name: "search", tool_type: "mcp", success: true, occurred_at: at("2026-08-12T10:03:00") },
  ];
  const result = buildBehaviorToolRanking(installationDevicesFor(events), [], events, "today", "call", now, { minimumGroupSubjects: 0 });
  assert(result.rows.some(row => row.tool_name === "mcp__private_crm__lookup" && row.tool_key === "mcp_custom"));
  assert(result.rows.some(row => row.tool_name === "mcp_custom" && row.tool_key === "mcp_custom"));
  const customFunnel = result.funnel_rows.find(row => row.tool_key === "mcp_custom");
  assert.equal(result.funnel_rows.filter(row => row.tool_key === "mcp_custom").length, 1);
  assert.equal(customFunnel.tool_name, "mcp__private_crm__lookup");
  assert.equal(customFunnel.call_succeeded_count, 3);
  assert.equal(result.rows.filter(row => row.tool_name === "search").length, 2);
  assert(result.rows.some(row => row.tool_name === "search" && row.tool_key === "search"));
  assert(result.rows.some(row => row.tool_name === "search" && row.tool_key === "mcp_custom"));
});

test("tool adoption funnel joins stages by stable identity and prefers the call-side name", () => {
  const now = at("2026-08-12T12:00:00");
  const events = [
    { event_id: "tool_seen", event_name: "tool_impression", view_id: "tool_view", identity_id: "dev_a", tool_key: "custom", tool_name: "workspace_helper", tool_type: "skill", occurred_at: at("2026-08-12T09:00:00") },
    { event_id: "tool_configured", event_name: "tool_configuration_finished", operation_id: "tool_config", identity_id: "dev_a", tool_key: "custom", tool_name: "workspace_helper", tool_type: "skill", success: true, occurred_at: at("2026-08-12T09:01:00") },
    { event_id: "tool_called", event_name: "tool_call_completed", operation_id: "tool_call", identity_id: "dev_a", tool_key: "custom", tool_name: "skill__workspace_helper", tool_type: "skill", success: true, occurred_at: at("2026-08-12T09:02:00") },
  ];

  const result = buildBehaviorToolRanking(installationDevicesFor(events), [], events, "today", "install", now, { minimumGroupSubjects: 0 });
  assert.equal(result.funnel_rows.length, 1);
  assert.equal(result.funnel_rows[0].tool_name, "skill__workspace_helper");
  assert.equal(result.funnel_rows[0].impression_count, 1);
  assert.equal(result.funnel_rows[0].configuration_succeeded_count, 1);
  assert.equal(result.funnel_rows[0].call_succeeded_count, 1);
  assert.equal(result.funnel_rows[0].configuration_to_call_p50_ms, 60_000);
});

test("behavior rankings include small groups and clip per-subject daily contributions", () => {
  const now = at("2026-08-12T12:00:00");
  const events = Array.from({ length: 101 }, (_, index) => ({
    event_id: `privacy_task_${String(index).padStart(4, "0")}`,
    event_name: "task_submitted",
    attempt_id: `privacy_attempt_${String(index).padStart(4, "0")}`,
    identity_id: "privacy_installation",
    mode: "assistant",
    initiator_type: "user",
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
  const overview = buildBehaviorOverview(installationDevicesFor(events), [], events, "today", now);
  assert.equal(overview.metrics.task_submitted_count, 100);
  assert.deepEqual(overview.data_quality.contribution_clipping, {
    subject_count: 1,
    clipped_subject_count: 1,
    clipped_subject_rate: 1,
    task_limit_per_event_day: 100,
    tool_call_limit_per_day: 500,
  });
  assert(overview.data_quality.warnings.includes("subject_contribution_clipped"));

  const models = buildBehaviorModelRanking(installationDevicesFor(events), [], events, "today", now);
  assert.equal(models.rows.length, 1);
  assert.equal(models.rows[0].observed_user_count, 1);
  assert.equal(models.suppressed_group_count, 0);
  assert.equal(models.minimum_group_subjects, 1);

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

test("old device credentials remain valid until explicit deletion", async () => {
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
    assert.equal(expired.status, 200);

    const renewed = await serviceRequest(
      service,
      "POST",
      "/pinvou3/telemetry/v1/register",
      registrationBody,
    );
    assert.equal(renewed.status, 200);
    assert.equal(renewed.body.device_id, original.device_id);
    assert.equal(renewed.body.device_token, original.device_token);
  } finally {
    service?.close();
    for (const name of names) {
      if (previous[name] === undefined) delete process.env[name];
      else process.env[name] = previous[name];
    }
    rmSync(dataDir, { recursive: true, force: true, maxRetries: 3, retryDelay: 50 });
  }
});

test("legacy expired tombstones reject old credentials but allow fresh registration", async () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-legacy-expired-"));
  const names = [
    "PINVOU_TELEMETRY_ENROLLMENT_TOKEN",
    "PINVOU_TELEMETRY_DEVICE_PEPPER",
    "PINVOU_STATS_ADMIN_PASSWORD",
    "PINVOU_TELEMETRY_DATA_DIR",
  ];
  const previous = Object.fromEntries(names.map((name) => [name, process.env[name]]));
  let service;
  try {
    const pepper = "test-device-pepper-at-least-24";
    process.env.PINVOU_TELEMETRY_ENROLLMENT_TOKEN = "test-enrollment-token-at-least-24";
    process.env.PINVOU_TELEMETRY_DEVICE_PEPPER = pepper;
    process.env.PINVOU_STATS_ADMIN_PASSWORD = "operations-admin-password";
    process.env.PINVOU_TELEMETRY_DATA_DIR = dataDir;
    const registrationBody = {
      enrollment_token: "test-enrollment-token-at-least-24",
      hardware_claim: "legacy-expired-installation-0001",
      registration_secret: "registration-secret-legacy-expired-0001",
      hardware_source: "installation_id",
      identity_quality: "installation_only",
      app_version: "0.8.7",
      platform: "windows",
      arch: "x86_64",
    };
    const bootstrap = new TelemetryStore(dataDir, pepper);
    const original = bootstrap.register(registrationBody, null);
    bootstrap.saveNow();
    writeFileSync(join(dataDir, "behavior-deletion-tombstones.json"), JSON.stringify({
      version: 2,
      records: [{
        subject_hash: bootstrap.deletionSubjectHash(original.device_id),
        token_hash: bootstrap.deletionTokenHash(original.device_token),
        hardware_hash: bootstrap.hardwareHash(registrationBody.hardware_claim),
        registration_secret_hash: bootstrap.registrationSecretHash(registrationBody.registration_secret),
        status: "completed",
        accepted_at: Date.now() - 36 * 24 * 60 * 60_000,
        completed_at: Date.now() - 36 * 24 * 60 * 60_000,
        result_expires_at: Date.now() - 6 * 24 * 60 * 60_000,
      }],
    }));

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
    rmSync(dataDir, { recursive: true, force: true });
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
      pipeline: readFileSync(join(dataDir, "behavior-pipeline.jsonl")),
    };
    const deletion = store.deleteBehaviorData(device, { request_id: "request_delete_0001" });
    assert.equal(deletion.status, "completed");
    assert.match(deletion.deletion_request_id, /^delete_/);
    assert.equal(store.events.length, 0);
    assert.equal(store.behaviorEvents.length, 0);
    assert.equal(store.behaviorPipelineEvents.length, 0);
    assert.equal(readFileSync(join(dataDir, "behavior-pipeline.jsonl"), "utf8"), "");
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
    writeFileSync(join(dataDir, "behavior-pipeline.jsonl"), backup.pipeline);
    const deletionLedgerPath = join(dataDir, "behavior-deletion-tombstones.json");
    const deletionLedger = JSON.parse(readFileSync(deletionLedgerPath, "utf8"));
    deletionLedger.records[0].tombstone_expires_at = 1;
    writeFileSync(deletionLedgerPath, JSON.stringify(deletionLedger));

    const reloaded = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
    assert.equal(reloaded.authenticate(credential.device_id, credential.device_token), null);
    assert.equal(reloaded.events.length, 0);
    assert.equal(reloaded.behaviorEvents.length, 0);
    assert.equal(reloaded.behaviorPipelineEvents.length, 0);
    assert.equal(readFileSync(join(dataDir, "behavior-pipeline.jsonl"), "utf8"), "");
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
      for (const name of ["devices.json", "usage-events.jsonl", "behavior-events.jsonl", "behavior-pipeline.jsonl", "behavior-deletion-tombstones.json"]) {
        assert.equal(statSync(join(dataDir, name)).mode & 0o777, 0o600);
      }
    }
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("retention compaction removes expired facts but preserves device registrations", () => {
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
    store.events[0].occurred_at = now - 36 * 24 * 60 * 60_000;
    store.events[0].received_at = now - 36 * 24 * 60 * 60_000;
    assert.equal(store.devices.size, 1);
    assert.equal(store.events.length, 1);

    assert.equal(store.compactExpiredData(now), true);
    assert.equal(store.devices.size, 1);
    assert.equal(store.devicesByHardwareHash.size, 1);
    assert.equal(store.events.length, 0);
    assert.equal(store.credentialWasRemoved(credential.device_token), false);
    assert.equal(store.authenticate(credential.device_id, credential.device_token), device);
    store.saveNow();
    assert.equal(JSON.parse(readFileSync(join(dataDir, "devices.json"), "utf8")).devices.length, 1);
    assert.equal(readFileSync(join(dataDir, "usage-events.jsonl"), "utf8"), "");

    const staleBackup = JSON.parse(readFileSync(join(dataDir, "devices.json"), "utf8"));
    staleBackup.devices = [{
      ...device,
      first_seen_at: now - 36 * 24 * 60 * 60_000,
      last_seen_at: now - 36 * 24 * 60 * 60_000,
      last_active_at: now - 36 * 24 * 60 * 60_000,
    }];
    writeFileSync(join(dataDir, "devices.json"), JSON.stringify(staleBackup));
    const reloaded = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
    assert.equal(reloaded.devices.size, 1);
    assert.equal(reloaded.authenticate(credential.device_id, credential.device_token).device_id, credential.device_id);
    assert.equal(JSON.parse(readFileSync(join(dataDir, "devices.json"), "utf8")).devices.length, 1);
  } finally {
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("behavior event rate limits persist attributable retryable pipeline facts", () => {
  const dataDir = mkdtempSync(join(tmpdir(), "pinvou-behavior-rate-limit-"));
  try {
    const store = new TelemetryStore(dataDir, "test-device-pepper-at-least-24-chars");
    const credential = store.register({
      hardware_claim: "behavior-rate-limit-hardware-0001",
      registration_secret: "registration-secret-rate-limit-00000001",
      app_version: "0.8.7",
      platform: "windows",
      arch: "x86_64",
      hardware_source: "installation_id",
    }, null);
    const device = store.authenticate(credential.device_id, credential.device_token);
    const collection = store.startBehaviorCollection(device, {
      request_id: "request_rate_start_000001",
    });
    store.eventRatePerMinute = 1;
    const inputEvents = Array.from({ length: 30 }, (_, index) => behaviorEvent(
      collection.collection_period_id,
      { event_id: `behavior_rate_limited_${String(index).padStart(4, "0")}` },
    ));
    const result = store.appendBehaviorEvents(device, inputEvents, "batch_rate_limited_000001");
    assert.equal(result.results.length, 30);
    assert(result.results.every((entry) => entry.status === "retryable"
      && entry.reason_code === "rate_limited"));
    assert.equal(store.behaviorEvents.length, 0);
    assert.equal(existsSync(join(dataDir, "behavior-events.jsonl")), false);
    assert.deepEqual(store.behaviorPipelineEvents, [{
      occurred_at: store.behaviorPipelineEvents[0].occurred_at,
      device_id: credential.device_id,
      accepted: 0,
      duplicate: 0,
      rejected: 0,
      retryable: 30,
    }]);
    assert.equal(JSON.parse(readFileSync(join(dataDir, "behavior-pipeline.jsonl"), "utf8")).device_id, credential.device_id);
    const overview = buildBehaviorOverview(
      [device], [], [], "today", Date.now(), store.behaviorPipelineEvents,
    );
    assert.equal(overview.metrics.pipeline_retryable_rate, 1);
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
