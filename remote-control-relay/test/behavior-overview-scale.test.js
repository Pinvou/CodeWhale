import assert from "node:assert/strict";
import { test } from "node:test";
import { buildBehaviorOverview } from "../telemetry-service.js";

const now = Date.parse("2026-10-14T12:00:00+08:00");
const device = { device_id: "scale-installation", hardware_source: "installation_id",
  identity_quality: "installation_only", first_seen_at: now - 1000,
  behavior_collection: { status: "active" } };

test("behavior overview handles retained event volumes beyond the JS function argument limit", () => {
  const events = Array.from({ length: 250_000 }, (_, index) => ({
    event_id: `scale-${index}`, identity_id: device.device_id,
    event_name: "scene_impression", occurred_at: now - index - 1000, received_at: now - index,
  }));
  for (const range of ["today", "7", "30"]) {
    const result = buildBehaviorOverview([device], [], events, range, now);
    assert.equal(result.latest_event_received_at, now);
    assert.equal(result.data_status, "ready");
    assert.equal(result.range, range);
  }
});

test("latest behavior receipt preserves fallback, empty and stale-data semantics", () => {
  const event = (occurred_at, received_at) => ({ identity_id: device.device_id,
    event_name: "scene_impression", occurred_at, received_at });
  const events = [event(now - 500, now - 300), event(now - 100, undefined),
    event(now - 200, 0), event(now - 900, "invalid"), event(-100, -50)];
  assert.equal(buildBehaviorOverview([device], [], events, "30", now).latest_event_received_at, now - 100);
  const empty = buildBehaviorOverview([device], [], [], "30", now);
  assert.equal(empty.latest_event_received_at, null);
  assert.equal(empty.data_status, "no_data");
  const stale = buildBehaviorOverview([device], [], [event(now - 2 * 86400000, undefined)], "30", now);
  assert.equal(stale.data_status, "delayed");
  assert.equal(buildBehaviorOverview([], [], [], "30", now).data_status, "tracking_not_enabled");
});
