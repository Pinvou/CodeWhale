import { existsSync, readFileSync } from "node:fs";

const deployedSchemaUrl = new URL("./docs/schemas/behavior-telemetry-v1.schema.json", import.meta.url);
const schemaUrl = existsSync(deployedSchemaUrl)
  ? deployedSchemaUrl
  : new URL("../docs/schemas/behavior-telemetry-v1.schema.json", import.meta.url);

export const BEHAVIOR_TELEMETRY_SCHEMA = Object.freeze(
  JSON.parse(readFileSync(schemaUrl, "utf8")),
);
export const BEHAVIOR_EVENT_NAMES = new Set(BEHAVIOR_TELEMETRY_SCHEMA.properties.event_name.enum);
export const BEHAVIOR_EVENT_CONTRACTS = Object.freeze(BEHAVIOR_TELEMETRY_SCHEMA["x-event-contracts"]);

const SERVER_FIELDS = new Set(BEHAVIOR_TELEMETRY_SCHEMA["x-server-injected-fields"]);
const CLIENT_FIELDS = new Set([
  ...BEHAVIOR_TELEMETRY_SCHEMA.required,
  ...BEHAVIOR_TELEMETRY_SCHEMA["x-client-optional-fields"],
]);
const DIAGNOSTIC_FIELDS = [...CLIENT_FIELDS].filter(field => field.startsWith("diagnostic_"));
const REMOTE_CONTROL_EVENT = "remote_control_event";
const REMOTE_CONTROL_FIELDS = new Set(
  BEHAVIOR_EVENT_CONTRACTS[REMOTE_CONTROL_EVENT]?.allowed || [],
);
export const BEHAVIOR_STORED_FIELDS = new Set([...CLIENT_FIELDS, ...SERVER_FIELDS]);
const ID_PATTERN = /^[A-Za-z0-9_-]+$/;
const MAX_EVENT_BYTES = 8 * 1024;

function invalid(reason, field) {
  return { ok: false, reason, ...(field ? { field } : {}) };
}

function validateValue(field, value, definition) {
  definition = resolvedDefinition(definition);
  if (definition.const !== undefined && value !== definition.const) return false;
  if (definition.enum && !definition.enum.includes(value)) return false;
  if (definition.type === "boolean" && typeof value !== "boolean") return false;
  if (definition.type === "integer" && (!Number.isSafeInteger(value)
    || value < (definition.minimum ?? Number.MIN_SAFE_INTEGER)
    || value > (definition.maximum ?? Number.MAX_SAFE_INTEGER))) return false;
  if (definition.type === "string") {
    if (typeof value !== "string") return false;
    if (definition.minLength !== undefined && value.length < definition.minLength) return false;
    if (definition.maxLength !== undefined && value.length > definition.maxLength) return false;
    if (definition.pattern && !(new RegExp(definition.pattern)).test(value)) return false;
  }
  if (definition.type === "array") {
    if (!Array.isArray(value) || value.length > (definition.maxItems ?? 100)) return false;
    if (!value.every(item => validateValue(field, item, definition.items))) return false;
  }
  if (definition.type === "object") {
    if (!value || typeof value !== "object" || Array.isArray(value)) return false;
    if ((definition.required || []).some(key => value[key] === undefined)) return false;
    for (const [key, item] of Object.entries(value)) {
      if (!Object.hasOwn(definition.properties || {}, key)) { if (definition.additionalProperties === false) return false; }
      else if (!validateValue(key, item, definition.properties[key])) return false;
    }
  }
  return true;
}

function resolvedDefinition(definition) {
  const ref = definition?.$ref;
  if (!ref) return definition || {};
  const key = ref.split("/").at(-1);
  return BEHAVIOR_TELEMETRY_SCHEMA.$defs[key] || {};
}

export function validateBehaviorEventShape(raw, options = {}) {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return invalid("invalid_event");
  if (Buffer.byteLength(JSON.stringify(raw), "utf8") > MAX_EVENT_BYTES) return invalid("too_large");
  if (raw.schema_version !== 1) return invalid("unsupported_schema", "schema_version");
  const eventName = raw.event_name;
  if (raw.diagnostic_id !== undefined) {
    if (raw.diagnostic_id !== raw.event_id || raw.status === "cancelled") return invalid("invalid_diagnostic");
    const failure = raw.success === false || ["failed", "interrupted"].includes(raw.status)
      || ["task_rejected", "provider_setup_failed", "voice_recognition_failed"].includes(eventName)
      || (eventName === "remote_control_event" && raw.remote_outcome !== "ok")
      || (eventName === "response_feedback_changed" && raw.rating === "negative" && raw.feedback_action === "set");
    if (!failure || !raw.diagnostic_code || !raw.diagnostic_component || !raw.diagnostic_trace_id || !raw.diagnostic_site) return invalid("invalid_diagnostic");
  } else if (Object.keys(raw).some(key => key.startsWith("diagnostic_"))) return invalid("invalid_diagnostic");
  const contract = BEHAVIOR_EVENT_CONTRACTS[eventName];
  if (!contract) return invalid("unknown_event", "event_name");
  const eventFields = Array.isArray(contract.allowed)
    ? new Set([...BEHAVIOR_TELEMETRY_SCHEMA.required, ...contract.allowed, ...DIAGNOSTIC_FIELDS])
    : CLIENT_FIELDS;
  for (const field of Object.keys(raw)) {
    if (SERVER_FIELDS.has(field)) return invalid("invalid_field", field);
    if (!eventFields.has(field)) return invalid("invalid_field", field);
    if (eventName !== REMOTE_CONTROL_EVENT && REMOTE_CONTROL_FIELDS.has(field)) {
      return invalid("invalid_field", field);
    }
  }
  for (const field of [...BEHAVIOR_TELEMETRY_SCHEMA.required, ...contract.required]) {
    if (raw[field] === undefined || raw[field] === null || raw[field] === "") {
      return invalid("invalid_field", field);
    }
  }
  if (eventName === REMOTE_CONTROL_EVENT) {
    if (raw.remote_task_key !== undefined && raw.remote_role === undefined) {
      return invalid("invalid_field", "remote_role");
    }
    const permissionStage = raw.remote_stage === "permission";
    if (permissionStage && raw.remote_permission_step === undefined) {
      return invalid("invalid_field", "remote_permission_step");
    }
    if (!permissionStage && raw.remote_permission_step !== undefined) {
      return invalid("invalid_field", "remote_permission_step");
    }
  }
  if (raw.execution_task_key !== undefined && !["tool_call_started", "tool_call_completed"].includes(eventName)) {
    return invalid("invalid_field", "execution_task_key");
  }
  for (const [field, value] of Object.entries(raw)) {
    const definition = resolvedDefinition(BEHAVIOR_TELEMETRY_SCHEMA.properties[field]);
    if (!validateValue(field, value, definition)) return invalid("invalid_field", field);
  }
  if (raw.tool_name !== undefined && !eventName.startsWith("tool_")) {
    return invalid("invalid_field", "tool_name");
  }
  if (typeof raw.event_id !== "string" || !ID_PATTERN.test(raw.event_id)) {
    return invalid("invalid_field", "event_id");
  }
  const now = options.now ?? Date.now();
  const retentionMs = options.retentionMs ?? 35 * 24 * 60 * 60_000;
  if (raw.occurred_at < now - retentionMs) return invalid("too_old", "occurred_at");
  if (raw.occurred_at > now + 5 * 60_000) return invalid("invalid_time", "occurred_at");
  if (raw.success === false && !raw.error_category) return invalid("invalid_field", "error_category");
  if (["failed", "cancelled"].includes(raw.status) && !raw.error_category) {
    return invalid("invalid_field", "error_category");
  }
  if (raw.event_name === "response_feedback_changed") {
    if (raw.feedback_action === "set" && !raw.rating) return invalid("invalid_field", "rating");
    if (raw.feedback_action === "clear" && raw.rating !== undefined) return invalid("invalid_field", "rating");
  }
  return { ok: true };
}

export function behaviorTelemetrySchemaSummary() {
  return {
    schema_version: 1,
    max_event_bytes: MAX_EVENT_BYTES,
    event_names: [...BEHAVIOR_EVENT_NAMES],
  };
}
