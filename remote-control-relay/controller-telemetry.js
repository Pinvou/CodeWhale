/**
 * Anonymous operational telemetry for the desktop-controller lane.
 *
 * Production callers submit only a frozen lifecycle event name (or an already
 * schema-shaped aggregate). Identifiers and user-controlled values are never
 * copied into an audit record, counter key, alert, or diagnostic line.
 */
"use strict";

export const STAGES = Object.freeze([
  "register", "claim", "handshake", "lease", "liveness", "resume",
  "task", "permission", "attachment", "artifact",
]);

export const OUTCOMES = Object.freeze([
  "ok", "rejected", "timeout", "busy", "expired", "unsupported",
  "cancelled", "internal_error",
]);

export const REASONS = Object.freeze([
  "none", "not_enrolled", "code_invalid", "slot_conflict", "handshake_failed",
  "receipt_mismatch", "liveness_missing", "watchdog_expired", "resume_failed",
  "task_conflict", "permission_denied", "permission_timeout", "transfer_budget",
  "artifact_missing", "rate_limited", "capacity_reached", "authentication_timeout",
  "peer_closed", "version_incompatible", "invalid_message", "unknown",
]);

export const LATENCY_BUCKETS = Object.freeze([
  "lt_50ms", "lt_250ms", "lt_1s", "lt_5s", "lt_30s", "gte_30s",
]);

export const ALERT_CODES = Object.freeze([
  "pairing_bruteforce_suspected",
  "controller_capacity_pressure",
  "controller_recovery_failure_spike",
]);

export const ALLOWED_FIELDS = Object.freeze([
  "event", "stage", "outcome", "reason", "latency_bucket", "count",
]);

const FORBIDDEN_FIELD_PATTERN =
  /code|endpoint|lease|token|key|secret|credential|\bip\b|ip_|_ip|device|prompt|reply|response|argument|tool|path|file|url|content|addr|host|name/i;

const EVENT_RECORDS = Object.freeze({
  controller_socket_opened: ["register", "ok", "none"],
  controller_socket_capacity_rejected: ["register", "rejected", "capacity_reached"],
  controller_socket_rate_limited: ["register", "rejected", "rate_limited"],
  controller_socket_auth_timeout: ["register", "timeout", "authentication_timeout"],
  desktop_pairing_registered: ["register", "ok", "none"],
  desktop_pairing_register_rejected: ["register", "rejected", "not_enrolled"],
  revocation_persist_failed: ["register", "internal_error", "unknown"],
  claim_request_conflict: ["claim", "rejected", "code_invalid"],
  claim_rate_limited: ["claim", "rejected", "rate_limited"],
  claim_invalid: ["claim", "rejected", "code_invalid"],
  claim_lost: ["claim", "rejected", "code_invalid"],
  claim_busy: ["claim", "busy", "slot_conflict"],
  claim_reserved: ["claim", "ok", "none"],
  handshake_started: ["handshake", "ok", "none"],
  handshake_rejected: ["handshake", "rejected", "handshake_failed"],
  receipt_commitment_accepted: ["handshake", "ok", "none"],
  encrypted_finished_forwarded: ["handshake", "ok", "none"],
  finished_receipt_accepted: ["handshake", "ok", "none"],
  reservation_invalidation_failed: ["handshake", "internal_error", "handshake_failed"],
  reservation_released: ["handshake", "cancelled", "handshake_failed"],
  reservation_promoted: ["lease", "ok", "none"],
  controller_lease_created: ["lease", "ok", "none"],
  controller_joined: ["lease", "ok", "none"],
  controller_lease_terminated: ["lease", "cancelled", "peer_closed"],
  liveness_forwarded: ["liveness", "ok", "none"],
  liveness_rejected: ["liveness", "rejected", "liveness_missing"],
  liveness_expired: ["liveness", "timeout", "watchdog_expired"],
  resume_forwarded: ["resume", "ok", "none"],
  resume_rejected: ["resume", "rejected", "resume_failed"],
  resume_expired: ["resume", "timeout", "resume_failed"],
  controller_resume_completed: ["resume", "ok", "none"],
  task_forwarded: ["task", "ok", "none"],
  task_rejected: ["task", "rejected", "task_conflict"],
  permission_forwarded: ["permission", "ok", "none"],
  permission_rejected: ["permission", "rejected", "permission_denied"],
  attachment_forwarded: ["attachment", "ok", "none"],
  attachment_rejected: ["attachment", "rejected", "transfer_budget"],
  artifact_forwarded: ["artifact", "ok", "none"],
  artifact_rejected: ["artifact", "rejected", "artifact_missing"],
});

export const LIFECYCLE_EVENTS = Object.freeze(Object.keys(EVENT_RECORDS));

const PAYLOAD_STAGE = Object.freeze({
  capability_snapshot_request: "task",
  capability_snapshot: "task",
  capability_snapshot_error: "task",
  task_submit: "task",
  task_cancel: "task",
  task_status: "task",
  remote_action_request: "task",
  remote_action_result: "task",
  temporary_model_package: "task",
  journal_event: "task",
  permission_list: "permission",
  permission_decide: "permission",
  attachment_begin: "attachment",
  attachment_chunk: "attachment",
  attachment_abort: "attachment",
  artifact_index: "artifact",
  artifact_preview: "artifact",
  artifact_download_begin: "artifact",
  artifact_download_chunk: "artifact",
  artifact_download_abort: "artifact",
});

const ALERT_RULES = Object.freeze([
  Object.freeze({
    code: "pairing_bruteforce_suspected",
    window_ms: 60_000,
    threshold: 10,
    matches: (record) => record.stage === "claim"
      && ["code_invalid", "rate_limited"].includes(record.reason),
  }),
  Object.freeze({
    code: "controller_capacity_pressure",
    window_ms: 5 * 60_000,
    threshold: 5,
    matches: (record) => record.reason === "slot_conflict"
      || record.reason === "capacity_reached",
  }),
  Object.freeze({
    code: "controller_recovery_failure_spike",
    window_ms: 5 * 60_000,
    threshold: 3,
    matches: (record) => record.stage === "resume" && record.outcome !== "ok",
  }),
]);

const MAX_ALERT_SAMPLES = 4096;

export function bucketLatency(milliseconds) {
  const value = Number(milliseconds);
  if (!Number.isFinite(value) || value < 0) return "gte_30s";
  if (value < 50) return "lt_50ms";
  if (value < 250) return "lt_250ms";
  if (value < 1000) return "lt_1s";
  if (value < 5000) return "lt_5s";
  if (value < 30000) return "lt_30s";
  return "gte_30s";
}

export function normalizeRecord(record) {
  if (!record || typeof record !== "object" || Array.isArray(record)) {
    throw new Error("controller_telemetry_record_invalid");
  }
  for (const field of Object.keys(record)) {
    if (FORBIDDEN_FIELD_PATTERN.test(field)) {
      throw new Error("controller_telemetry_field_forbidden");
    }
    if (!ALLOWED_FIELDS.includes(field)) {
      throw new Error("controller_telemetry_field_unknown");
    }
  }
  if (!LIFECYCLE_EVENTS.includes(record.event)) throw new Error("controller_telemetry_event_unknown");
  const eventTuple = EVENT_RECORDS[record.event];
  if (!STAGES.includes(record.stage)) throw new Error("controller_telemetry_stage_unknown");
  if (!OUTCOMES.includes(record.outcome)) throw new Error("controller_telemetry_outcome_unknown");
  const reason = record.reason === undefined ? eventTuple[2] : record.reason;
  if (!REASONS.includes(reason)) throw new Error("controller_telemetry_reason_unknown");
  if (record.stage !== eventTuple[0]
    || record.outcome !== eventTuple[1]
    || reason !== eventTuple[2]) {
    throw new Error("controller_telemetry_event_mismatch");
  }
  const latency = record.latency_bucket;
  if (latency !== undefined && !LATENCY_BUCKETS.includes(latency)) {
    throw new Error("controller_telemetry_latency_unknown");
  }
  const count = record.count === undefined ? 1 : Number(record.count);
  if (!Number.isInteger(count) || count < 1 || count > 100_000) {
    throw new Error("controller_telemetry_count_invalid");
  }
  return Object.freeze({
    event: record.event,
    stage: record.stage,
    outcome: record.outcome,
    reason,
    latency_bucket: latency || "lt_50ms",
    count,
  });
}

function eventRecord(eventName, latencyMs) {
  const tuple = EVENT_RECORDS[eventName];
  if (!tuple) throw new Error("controller_audit_event_unknown");
  return normalizeRecord({
    event: eventName,
    stage: tuple[0],
    outcome: tuple[1],
    reason: tuple[2],
    latency_bucket: bucketLatency(latencyMs ?? 0),
  });
}

function immutableAlert(rule, count) {
  return Object.freeze({
    code: rule.code,
    severity: "warning",
    window_seconds: rule.window_ms / 1000,
    count,
    threshold: rule.threshold,
  });
}

/** Revalidates the operations projection at the protected stats boundary. */
export function normalizeSnapshot(snapshot) {
  if (!snapshot || typeof snapshot !== "object" || Array.isArray(snapshot)
    || Object.keys(snapshot).sort().join(",") !== "alerts,counters,schema_version,total"
    || snapshot.schema_version !== 1 || !snapshot.counters
    || typeof snapshot.counters !== "object" || Array.isArray(snapshot.counters)
    || !Array.isArray(snapshot.alerts)) {
    throw new Error("controller_telemetry_snapshot_invalid");
  }
  const counters = {};
  let total = 0;
  for (const [key, count] of Object.entries(snapshot.counters)) {
    const parts = key.split("|");
    if (parts.length !== 5 || !Number.isSafeInteger(count) || count < 1) {
      throw new Error("controller_telemetry_snapshot_invalid");
    }
    const normalized = normalizeRecord({
      event: parts[0], stage: parts[1], outcome: parts[2], reason: parts[3],
      latency_bucket: parts[4], count: 1,
    });
    counters[key] = count;
    total = Math.min(Number.MAX_SAFE_INTEGER, total + count);
  }
  if (snapshot.total !== total) throw new Error("controller_telemetry_snapshot_invalid");
  const seenAlerts = new Set();
  const alerts = snapshot.alerts.map((alert) => {
    const rule = ALERT_RULES.find((candidate) => candidate.code === alert?.code);
    if (!alert || typeof alert !== "object" || Array.isArray(alert)
      || Object.keys(alert).sort().join(",") !== "code,count,severity,threshold,window_seconds"
      || !rule || seenAlerts.has(alert.code) || alert.severity !== "warning"
      || alert.window_seconds !== rule.window_ms / 1000
      || !Number.isSafeInteger(alert.count) || alert.count < 1
      || alert.threshold !== rule.threshold || alert.count < alert.threshold) {
      throw new Error("controller_telemetry_snapshot_invalid");
    }
    seenAlerts.add(alert.code);
    return Object.freeze({ ...alert });
  });
  return Object.freeze({
    schema_version: 1,
    counters: Object.freeze(counters),
    alerts: Object.freeze(alerts),
    total,
  });
}

/** In-memory aggregate exposed by the existing protected operations overview. */
export class ControllerTelemetry {
  constructor({ now = Date.now, auditSink = () => {} } = {}) {
    if (typeof now !== "function" || typeof auditSink !== "function") {
      throw new Error("controller_telemetry_config_invalid");
    }
    this.now = now;
    this.auditSink = auditSink;
    this.counters = new Map();
    this.alertSamplesByRule = new Map(ALERT_RULES.map((rule) => [rule.code, []]));
  }

  record(record) {
    const normalized = normalizeRecord(record);
    const mapKey = `${normalized.event}|${normalized.stage}|${normalized.outcome}|${normalized.reason}|${normalized.latency_bucket}`;
    this.counters.set(
      mapKey,
      Math.min(Number.MAX_SAFE_INTEGER, (this.counters.get(mapKey) || 0) + normalized.count),
    );
    const at = Number(this.now());
    if (Number.isSafeInteger(at) && at >= 0) {
      for (const rule of ALERT_RULES) {
        if (!rule.matches(normalized)) continue;
        const samples = this.alertSamplesByRule.get(rule.code);
        samples.push(Object.freeze({ at, count: normalized.count }));
        if (samples.length > MAX_ALERT_SAMPLES) {
          samples.splice(0, samples.length - MAX_ALERT_SAMPLES);
        }
      }
      this.#prune(at);
    }
    try { this.auditSink(normalized); } catch { /* telemetry never changes authorization */ }
    return normalized;
  }

  audit(eventName, latencyMs) {
    return this.record(eventRecord(eventName, latencyMs));
  }

  auditBusiness(payloadKind, accepted = true) {
    const stage = PAYLOAD_STAGE[payloadKind];
    if (!stage) throw new Error("controller_audit_payload_unknown");
    return this.audit(`${stage}_${accepted ? "forwarded" : "rejected"}`);
  }

  #prune(now) {
    for (const rule of ALERT_RULES) {
      const samples = this.alertSamplesByRule.get(rule.code);
      const oldest = now - rule.window_ms;
      while (samples[0]?.at < oldest) samples.shift();
    }
  }

  snapshot() {
    const counters = {};
    let total = 0;
    for (const [key, value] of [...this.counters.entries()].sort(([left], [right]) => left.localeCompare(right))) {
      counters[key] = value;
      total = Math.min(Number.MAX_SAFE_INTEGER, total + value);
    }
    const now = Number(this.now());
    if (Number.isSafeInteger(now) && now >= 0) this.#prune(now);
    const alerts = ALERT_RULES.map((rule) => {
      const count = this.alertSamplesByRule.get(rule.code)
        .reduce((sum, sample) => sum + sample.count, 0);
      return count >= rule.threshold ? immutableAlert(rule, count) : null;
    }).filter(Boolean);
    return Object.freeze({
      schema_version: 1,
      counters: Object.freeze(counters),
      alerts: Object.freeze(alerts),
      total,
    });
  }

  reset() {
    this.counters.clear();
    for (const samples of this.alertSamplesByRule.values()) samples.length = 0;
  }

  alertSampleCount() {
    return [...this.alertSamplesByRule.values()]
      .reduce((sum, samples) => sum + samples.length, 0);
  }
}
