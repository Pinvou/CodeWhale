/**
 * Relay-side telemetry schema for the desktop controller (DEV-24).
 *
 * Only the enumerations listed here may leave the process. Every other field is
 * dropped, so a caller cannot accidentally add a code, endpoint, lease, token,
 * key, IP, device name or prompt to the pipeline. High-cardinality identity
 * fields are rejected by the schema rather than by caller discipline.
 */
"use strict";

export const STAGES = Object.freeze([
  "register",
  "claim",
  "handshake",
  "lease",
  "liveness",
  "resume",
  "task",
  "permission",
  "attachment",
  "artifact",
]);

export const OUTCOMES = Object.freeze([
  "ok",
  "rejected",
  "timeout",
  "busy",
  "expired",
  "unsupported",
  "internal_error",
]);

export const REASONS = Object.freeze([
  "none",
  "not_enrolled",
  "code_invalid",
  "slot_conflict",
  "handshake_failed",
  "receipt_mismatch",
  "liveness_missing",
  "watchdog_expired",
  "resume_failed",
  "task_conflict",
  "permission_denied",
  "transfer_budget",
  "artifact_missing",
  "rate_limited",
  "unknown",
]);

export const LATENCY_BUCKETS = Object.freeze([
  "lt_50ms",
  "lt_250ms",
  "lt_1s",
  "lt_5s",
  "lt_30s",
  "gte_30s",
]);

export const ALLOWED_FIELDS = Object.freeze(["stage", "outcome", "reason", "latency_bucket", "count"]);

const FORBIDDEN_FIELD_PATTERN =
  /code|endpoint|lease|token|key|secret|credential|\bip\b|ip_|_ip|device|prompt|path|file|url|content|addr|host/i;

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

/**
 * Validates one telemetry record. Returns a frozen, schema-shaped record or
 * throws when the caller uses an unknown enum or a forbidden field.
 */
export function normalizeRecord(record) {
  if (!record || typeof record !== "object" || Array.isArray(record)) {
    throw new Error("controller_telemetry_record_invalid");
  }
  for (const field of Object.keys(record)) {
    if (FORBIDDEN_FIELD_PATTERN.test(field)) {
      throw new Error(`controller_telemetry_field_forbidden:${field}`);
    }
    if (!ALLOWED_FIELDS.includes(field)) {
      throw new Error(`controller_telemetry_field_unknown:${field}`);
    }
  }
  if (!STAGES.includes(record.stage)) throw new Error("controller_telemetry_stage_unknown");
  if (!OUTCOMES.includes(record.outcome)) throw new Error("controller_telemetry_outcome_unknown");
  const reason = record.reason === undefined ? "none" : record.reason;
  if (!REASONS.includes(reason)) throw new Error("controller_telemetry_reason_unknown");
  const latency = record.latency_bucket;
  if (latency !== undefined && !LATENCY_BUCKETS.includes(latency)) {
    throw new Error("controller_telemetry_latency_unknown");
  }
  const count = record.count === undefined ? 1 : Number(record.count);
  if (!Number.isInteger(count) || count < 0 || count > 100000) {
    throw new Error("controller_telemetry_count_invalid");
  }
  return Object.freeze({
    stage: record.stage,
    outcome: record.outcome,
    reason,
    latency_bucket: latency || "lt_50ms",
    count,
  });
}

/**
 * Aggregating counter store. Only anonymous counts are kept; nothing here can
 * reconstruct a user, a device or a pairing instance.
 */
export class ControllerTelemetry {
  constructor(now = () => Date.now()) {
    this.now = now;
    this.counters = new Map();
    this.dedupe = new Map();
  }

  record(record, dedupeKey) {
    const normalized = normalizeRecord(record);
    if (dedupeKey !== undefined && dedupeKey !== null) {
      const key = String(dedupeKey);
      if (this.dedupe.has(key)) return null;
      this.dedupe.set(key, this.now());
      if (this.dedupe.size > 4096) {
        const oldest = this.dedupe.keys().next().value;
        this.dedupe.delete(oldest);
      }
    }
    const mapKey = `${normalized.stage}|${normalized.outcome}|${normalized.reason}|${normalized.latency_bucket}`;
    const current = this.counters.get(mapKey) || 0;
    this.counters.set(mapKey, current + normalized.count);
    return normalized;
  }

  snapshot() {
    const counters = {};
    let total = 0;
    for (const [key, value] of this.counters.entries()) {
      counters[key] = value;
      total += value;
    }
    return Object.freeze({ counters: Object.freeze(counters), total });
  }

  reset() {
    this.counters.clear();
    this.dedupe.clear();
  }
}
