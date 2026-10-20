import assert from "node:assert/strict";
import test from "node:test";
import { buildRemoteOverview, projectRemoteTasks } from "../remote-telemetry.js";
import { buildBehaviorOverview, buildStatsOverview, buildDeviceList, buildBehaviorModelRanking, buildBehaviorToolRanking, TelemetryStore } from "../telemetry-service.js";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const now = Date.parse("2026-10-20T06:00:00Z");
const devices = ["A", "B"].map(device_id => ({ device_id, hardware_source: "installation_id", first_seen_at: now - 86400000, last_seen_at: now, platform: "windows" }));
const key = "a".repeat(64);
function event(role, stage, extra = {}) {
  return { event_name: "remote_control_event", event_id: `${role}_${stage}`, device_id: role === "controller" ? "A" : "B",
    occurred_at: now - 10000, received_at: now, platform: "windows", arch: "x86_64", app_version: "0.10.12",
    remote_role: role, remote_stage: stage, remote_outcome: "ok", remote_reason: "none", remote_latency_bucket: "lt_1s",
    remote_task_key: key, remote_mode: "assistant", ...extra };
}
function invocation() {
  return [event("controller", "task_submit"), event("controlled", "task_admit"),
    event("controlled", "first_output", { remote_duration_ms: 500 }),
    event("controlled", "model_used", { remote_provider_key: "openai", remote_model_key: "gpt_5" }),
    event("controlled", "task_terminal", { occurred_at: now - 2000, remote_task_status: "success", remote_duration_ms: 8000, remote_model_source: "controlled" }),
    event("controller", "result_received", { occurred_at: now - 1000, remote_task_status: "success" })];
}

function completedInvocations(count, completedAt = now - 20000) {
  return Array.from({ length: count }, (_, index) => {
    const taskKey = index.toString(16).padStart(64, "0");
    return [
      event("controller", "task_submit", { occurred_at: completedAt - 2000 + index }),
      event("controlled", "task_admit", { occurred_at: completedAt - 1000 + index }),
      event("controlled", "task_terminal", { occurred_at: completedAt + index, remote_task_status: "success" }),
    ].map(row => ({ ...row, remote_task_key: taskKey, event_id: `${index}_${row.event_id}` }));
  }).flat();
}

test("A/B reports join once across total calls, per-device calls, WSTA and model ranking", () => {
  const rows = invocation();
  for (const events of [rows, [...rows].reverse(), [...rows, ...rows]]) {
    const report = buildBehaviorOverview(devices, [], events, "today", now);
    const stats = buildStatsOverview(devices, [], events, now);
    const detail = buildDeviceList(devices, [], events, now);
    assert.equal(stats.usage_trend.at(-1).turns, 1);
    assert.equal(stats.usage_trend.at(-1).active_devices, 1);
    assert.equal(report.metrics.task_finished_count, 1);
    assert.equal(report.metrics.task_success_count, 1);
    assert.equal(report.metrics.wsta_installations, 1);
    assert.equal(report.metrics.explicit_value_ineligible_installations, 1);
    assert.equal(report.remote_control.task_count, 1);
    assert.equal(report.remote_control.result_received_count, 1);
    assert.equal(detail.find(d => d.device_id === "A").turns_today, 1);
    assert.equal(detail.find(d => d.device_id === "B").turns_today, 0);
    assert.equal(buildBehaviorModelRanking(devices, [], events, "today", now).rows[0].use_count, 1);
  }
});

test("B-only execution counts once without making passive B active or retained", () => {
  const rows = invocation().filter(e => e.remote_role === "controlled");
  const report = buildBehaviorOverview(devices, [], rows, "today", now);
  assert.equal(report.metrics.task_finished_count, 1);
  assert.equal(report.metrics.wsta_installations, 0);
  assert.equal(report.metrics.task_active_installations, 0);
  assert.equal(report.remote_control.unattributed_count, 1);
  assert.equal(report.remote_control.awaiting_receipt_count, 1);
  assert.equal(buildStatsOverview(devices, [], rows, now).usage_trend.at(-1).active_devices, 0);
});

test("passive B success cannot become first-day activation or technical value", () => {
  const cohortTime = now - 30 * 3600000;
  const rows = [
    { event_name: "analytics_cohort_started", event_id: "cohort_B", device_id: "B", occurred_at: cohortTime },
    ...invocation().filter(e => e.remote_role === "controlled")
      .map(e => ({ ...e, occurred_at: now - 10 * 3600000 })),
  ];
  const report = buildBehaviorOverview(devices, [], rows, "7d", now);
  assert.equal(report.metrics.cohort_matured_installations, 1);
  assert.equal(report.metrics.task_finished_count, 1);
  assert.equal(report.metrics.first_technical_value_24h_installations, 0);
  assert.equal(report.metrics.wsta_installations, 0);
  assert.equal(report.funnels.activation.task_succeeded_installations, 0);
});

test("B-only admission rejection reports its reason without an invocation or origin activity", () => {
  const rows = [event("controlled", "task_rejected", { remote_outcome: "rejected", remote_reason: "permission_required" })];
  const report = buildBehaviorOverview(devices, [], rows, "today", now);
  assert.equal(report.remote_control.rejected_count, 1);
  assert.equal(report.remote_control.rows[0].reason, "permission_required");
  assert.equal(report.metrics.task_finished_count, 0);
  assert.equal(report.remote_control.submitted_count, 0);
  assert.equal(report.metrics.task_active_installations, 0);
});

test("never-sent, uncertain send, and subsequent actual dispatch remain distinct", () => {
  const failed = event("controller", "task_not_sent", { remote_outcome: "internal_error", remote_reason: "unknown" });
  for (const observation of [failed, { ...failed, remote_stage: "task_submit" }]) {
    const report = buildBehaviorOverview(devices, [], [observation], "today", now);
    assert.equal(report.remote_control.not_sent_count, 1);
    assert.equal(report.remote_control.pending_count, 0);
    assert.equal(report.remote_control.submitted_count, 0);
    assert.equal(report.metrics.task_submitted_count, 0);
    assert.equal(report.metrics.task_finished_count, 0);
  }
  const uncertain = event("controller", "task_submit", { remote_outcome: "internal_error", remote_task_status: "unknown" });
  const report = buildBehaviorOverview(devices, [], [failed, uncertain], "today", now);
  assert.equal(report.remote_control.unknown_count, 1);
  assert.equal(report.remote_control.not_sent_count, 0);
  assert.equal(report.remote_control.pending_count, 0);
  assert.equal(report.remote_control.submitted_count, 1);
  assert.equal(report.metrics.task_finished_count, 0);
  const recovered = buildBehaviorOverview(devices, [], [failed, uncertain, ...invocation().slice(1)], "today", now);
  assert.equal(recovered.remote_control.task_count, 1);
  assert.equal(recovered.remote_control.unknown_count, 0);
  assert.equal(recovered.remote_control.not_sent_count, 0);
});

test("A-only authenticated receipt counts once; submission alone remains pending", () => {
  const rows = invocation().filter(e => e.remote_role === "controller");
  assert.equal(buildBehaviorOverview(devices, [], rows, "today", now).metrics.task_finished_count, 1);
  const waiting = buildBehaviorOverview(devices, [], rows.slice(0, 1), "today", now);
  assert.equal(waiting.metrics.task_finished_count, 0);
  assert.equal(waiting.remote_control.pending_count, 1);
});

test("B-local shared-session task belongs to B and does not count A's mirror", () => {
  const rows = invocation();
  rows[0] = event("controlled", "task_submit");
  const detail = buildDeviceList(devices, [], rows, now);
  assert.equal(detail.find(d => d.device_id === "A").turns_today, 0);
  assert.equal(detail.find(d => d.device_id === "B").turns_today, 1);
  const remote = buildBehaviorOverview(devices, [], rows, "today", now).remote_control;
  assert.equal(remote.execution_terminal_count, 0);
  assert.equal(remote.result_received_count, 0);
  assert.equal(remote.awaiting_receipt_count, 0);
});

test("unknown B outcome overrides observer success; rejection is not an execution", () => {
  const rows = invocation();
  rows.find(e => e.remote_stage === "task_terminal").remote_task_status = "unknown";
  const report = buildBehaviorOverview(devices, [], rows, "today", now);
  assert.equal(report.metrics.task_finished_count, 0);
  assert.equal(report.remote_control.unknown_count, 1);
  const rejected = [rows[0], event("controller", "task_rejected", { remote_outcome: "rejected", remote_reason: "permission_denied" })];
  const rejection = buildBehaviorOverview(devices, [], rejected, "today", now);
  assert.equal(rejection.metrics.task_finished_count, 0);
  assert.equal(rejection.remote_control.rejected_count, 1);
});

test("projection is idempotent; real new invocation adds exactly one call", () => {
  const rows = invocation();
  const once = projectRemoteTasks(rows);
  assert.deepEqual(projectRemoteTasks(once), once);
  const retry = rows.map(e => ({ ...e, remote_task_key: "b".repeat(64), event_id: `retry_${e.event_id}` }));
  assert.equal(buildBehaviorOverview(devices, [], [...rows, ...retry], "today", now).metrics.task_finished_count, 2);
});

test("day boundary uses execution completion day and keeps prior-day attribution", () => {
  const rows = invocation();
  rows[0].occurred_at = now - 86400000;
  const report = buildBehaviorOverview(devices, [], rows, "today", now);
  assert.equal(report.remote_control.task_count, 1);
  assert.equal(report.remote_control.submitted_count, 0);
  assert.equal(report.remote_control.unattributed_count, 0);
});

test("result wait samples use receipt time and obey raw-event clipping", () => {
  const rows = invocation();
  rows.at(-1).remote_duration_ms = 9000;
  const beforeReceipt = buildRemoteOverview(rows, now - 10000, now - 2000);
  assert.equal(beforeReceipt.task_count, 1);
  assert.equal(beforeReceipt.result_wait_p50_ms, null);
  const receivedToday = buildRemoteOverview(rows, now - 1500, now);
  assert.equal(receivedToday.task_count, 0);
  assert.equal(receivedToday.result_wait_p50_ms, 9000);
  const clipped = projectRemoteTasks(rows).filter(e => e.event_id !== rows.at(-1).event_id);
  const report = buildRemoteOverview(rows, now - 20000, now, 30, null, clipped);
  assert.equal(report.result_wait_p50_ms, null);
  assert.equal(report.result_wait_p90_ms, null);
  // Coverage remains an execution-completion cohort observed over history;
  // it is deliberately independent of the receipt-day duration sample.
  assert.equal(report.result_received_count, 1);
});

test("legacy uncorrelated events stay diagnostic and small samples hide rates", () => {
  const rows = invocation().map(({ remote_task_key, ...e }) => e);
  const report = buildBehaviorOverview(devices, [], rows, "today", now);
  assert.equal(report.remote_control.task_count, 0);
  assert.equal(report.remote_control.legacy_event_count, 3);
  assert.equal(report.remote_control.success_rate, null);
});

test("failed/cancelled tasks count once and model source never changes ownership", () => {
  for (const status of ["failed", "cancelled", "interrupted"]) {
    const rows = invocation().map(e => ({ ...e, remote_mode: "code", remote_model_source: "controller_temporary",
      ...(e.remote_stage === "task_terminal" ? { remote_task_status: status } : {}) }));
    const report = buildBehaviorOverview(devices, [], rows, "today", now);
    assert.equal(report.metrics.task_finished_count, 1);
    assert.equal(report.remote_control.task_modes.code, 1);
    assert.equal(report.remote_control.task_model_sources.controller_temporary, 1);
    assert.equal(report.remote_control.succeeded_count, 0);
  }
});

test("connection and permission events never add a call", () => {
  const rows = ["connection", "permission", "resume"].map(stage => event("controller", stage));
  const report = buildRemoteOverview(rows, now - 20000, now);
  assert.equal(report.task_count, 0);
  assert.equal(report.connection_attempts, 1);
});

test("collector persists remote extensions, dedupes after restart and forgets deleted origin attribution", () => {
  const directory = mkdtempSync(join(tmpdir(), "pinvou-remote-stats-"));
  const pepper = "remote-stats-test-pepper-at-least-24-chars";
  try {
    const store = new TelemetryStore(directory, pepper);
    const endpoints = new Map(["controller", "controlled"].map(role => {
      const credentials = store.register({ hardware_claim: `install-remote-stats-${role}`, hardware_source: "installation_id",
        registration_secret: `remote-stats-registration-secret-${role}`, identity_quality: "installation_only",
        app_version: "0.10.12", platform: "windows", arch: "x86_64" }, null);
      const device = store.authenticate(credentials.device_id, credentials.device_token);
      const collection = store.startBehaviorCollection(device, { request_id: `request_remote_start_${role}` });
      return [role, { credentials, device, collection }];
    }));
    const accepted = [];
    for (const row of invocation()) {
      const { device, collection } = endpoints.get(row.remote_role);
      const { device_id, received_at, ...payload } = row;
      Object.assign(payload, { schema_version: 1, event_id: `beh_${row.event_id}`, occurred_at: Date.now() - 1000,
        event_source: "client", producer: "pinvou_agent", producer_version: "0.10.12", channel: "dev", locale: "zh-CN",
        collection_period_id: collection.collection_period_id });
      assert.equal(store.appendBehaviorEvents(device, [payload]).results[0].status, "accepted");
      accepted.push([row.remote_role, payload]);
    }
    store.saveNow();
    const restored = new TelemetryStore(directory, pepper);
    for (const [role, payload] of accepted) {
      const { credentials } = endpoints.get(role);
      assert.equal(restored.appendBehaviorEvents(restored.authenticate(credentials.device_id, credentials.device_token), [payload]).results[0].status, "duplicate");
    }
    assert.equal(restored.behaviorOverview("today").remote_control.task_count, 1);
    assert.equal(restored.behaviorOverview("today").metrics.wsta_installations, 1);
    const a = endpoints.get("controller").credentials;
    restored.deleteBehaviorData(restored.authenticate(a.device_id, a.device_token), { request_id: "request_delete_remote_controller" });
    const report = restored.behaviorOverview("today");
    assert.equal(report.remote_control.task_count, 1);
    assert.equal(report.remote_control.unattributed_count, 1);
    assert.equal(report.metrics.wsta_installations, 0);
    assert.equal(report.metrics.task_active_installations, 0);
  } finally { rmSync(directory, { recursive: true, force: true }); }
});

test("remote detail uses the same daily call contribution cap as the overall dashboard", () => {
  const local = Array.from({ length: 100 }, (_, index) => ({ device_id: "A", event_id: `local_${index}`,
    event_name: "task_finished", attempt_id: `local_attempt_${index}`, mode: "assistant", initiator_type: "user",
    status: "success", occurred_at: now - 50000 }));
  const report = buildBehaviorOverview(devices, [], [...local, ...invocation()], "today", now);
  assert.equal(report.metrics.task_finished_count, 100);
  assert.equal(report.remote_control.task_count, 0);
  assert.equal(report.remote_control.execution_terminal_count, 0);
  assert.equal(report.remote_control.result_received_count, 0);
  assert.equal(report.remote_control.awaiting_receipt_count, 0);
  assert.equal(report.remote_control.result_receipt_rate, null);
  assert.ok(report.data_quality.warnings.includes("subject_contribution_clipped"));
});

for (const count of [1, 100, 200]) {
  test(`execution receipt cohort respects the daily cap for ${count} remote tasks`, () => {
    const report = buildBehaviorOverview(devices, [], completedInvocations(count), "today", now);
    const expected = Math.min(count, 100);
    assert.equal(report.metrics.task_finished_count, expected);
    assert.equal(report.remote_control.task_count, expected);
    assert.equal(report.remote_control.execution_terminal_count, expected);
    assert.equal(report.remote_control.awaiting_receipt_count, expected);
    assert.equal(report.remote_control.result_received_count, 0);
    assert.equal(report.remote_control.result_receipt_rate, expected < 30 ? null : 0);
  });
}

test("clipped execution cohort finds cross-day receipts without including overflow tasks", () => {
  const executions = completedInvocations(200, now - 86400000 - 20000);
  const receipts = executions.filter(row => row.remote_stage === "task_terminal")
    .filter((_, index) => index < 50 || index >= 100)
    .map(row => event("controller", "result_received", {
      remote_task_key: row.remote_task_key, event_id: `receipt_${row.remote_task_key}`,
      occurred_at: now - 1000, remote_task_status: "success", remote_duration_ms: 9000,
    }));
  const rows = [...executions, ...receipts];
  const report = buildBehaviorOverview(devices, [], rows, "7d", now);
  assert.equal(report.metrics.task_finished_count, 100);
  assert.equal(report.remote_control.task_count, 100);
  assert.equal(report.remote_control.execution_terminal_count, 100);
  assert.equal(report.remote_control.result_received_count, 50);
  assert.equal(report.remote_control.awaiting_receipt_count, 50);
  assert.equal(report.remote_control.result_receipt_rate, 0.5);
  const today = buildBehaviorOverview(devices, [], rows, "today", now).remote_control;
  assert.equal(today.execution_terminal_count, 0);
  assert.equal(today.result_received_count, 0);
  assert.equal(today.awaiting_receipt_count, 0);
  assert.equal(today.result_wait_p50_ms, 9000);
});

test("remote tools are counted only on B and attributed to the consenting initiator", () => {
  const tools = ["tool_call_started", "tool_call_completed"].map(event_name => ({
    event_name, event_id: `tool_${event_name}`, device_id: "B", occurred_at: now - 3000,
    session_id: "B-local-session", turn_id: "B-engine-turn", operation_id: "tool-operation-1", attempt_index: 1,
    tool_key: "web_search", tool_type: "builtin", success: true, duration_ms: 100,
    execution_task_key: key,
  }));
  const rows = [...invocation(), ...tools];
  const normalized = projectRemoteTasks(rows);
  assert.deepEqual(projectRemoteTasks(normalized), normalized);
  assert.ok(normalized.filter(e => e.event_name.startsWith("tool_call")).every(e => e.device_id === "A"));
  const ranking = buildBehaviorToolRanking(devices, [], rows, "today", "call", now);
  assert.equal(ranking.rows[0].call_count, 1);
  assert.equal(ranking.rows[0].user_count, 1);
});
