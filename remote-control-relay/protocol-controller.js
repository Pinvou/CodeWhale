import { Buffer } from "node:buffer";
import { createHash } from "node:crypto";

export const CONTROLLER_PROTOCOL_VERSION = 1;
export const CONTROLLER_SUBPROTOCOL = "pinvou-desktop-controller-v1";
export const MAX_CONTROL_MESSAGE_BYTES = 64 * 1024;
export const MAX_ENCRYPTED_FRAME_BYTES = 2 * 1024 * 1024;
export const MAX_DECRYPTED_PAYLOAD_BYTES = 512 * 1024;
export const MAX_TRANSFER_CHUNK_BYTES = 256 * 1024;
export const MAX_TASK_INPUT_TEXT_BYTES = 64 * 1024;
export const MAX_TEMPORARY_MODEL_PACKAGE_BYTES = 64 * 1024;
export const MAX_ID_CHARS = 128;
const MAX_TASK_REFERENCES = 32;
const MAX_DESCRIPTORS = 256;
const MAX_ARTIFACTS = 256;
export const MAX_ENCRYPTED_WIRE_BYTES = Math.ceil(MAX_ENCRYPTED_FRAME_BYTES / 3) * 4 + MAX_CONTROL_MESSAGE_BYTES;
export const CONTROLLER_CONTRACT_SHA256 = "e1e511c4819d2298b735e6b049cb59cbd8935858f7da67d43beaf9106a068fd9";

export const CONTROLLER_CAPABILITIES = Object.freeze([
  "pairing_v1", "encrypted_frames_v1", "finished_receipt_v1", "role_liveness_v1", "resume_v1",
  "task_v1", "permission_v1", "attachment_v1", "artifact_v1", "snapshot_journal_v1",
]);

export const CONTROLLER_MESSAGE_TYPES = Object.freeze([
  "controller_capabilities", "desktop_endpoint_claim", "desktop_pairing_register", "desktop_pairing_registered",
  "desktop_pairing_revoke", "controller_pairing_claim", "controller_pairing_reserved", "finished_receipt_commit",
  "encrypted_handshake_finished", "finished_receipt_ack", "controller_joined", "controller_liveness",
  "controller_liveness_challenge_a", "encrypted_controller_liveness_a", "controller_liveness_attestation_request_a",
  "controller_liveness_attestation_a", "controller_resume_prepare", "controller_resume_provisional",
  "encrypted_resume_confirm", "controller_resume_activation_ack", "controller_resume_activation_pending",
  "controller_post_activation_liveness", "controller_post_activation_liveness_confirmed", "controller_resume_ready_ack",
  "controller_resumed", "encrypted_frame", "controller_disconnect", "error",
]);

export const CONTROLLER_PAYLOAD_TYPES = Object.freeze([
  "capability_snapshot_request", "capability_snapshot", "task_submit", "task_cancel", "task_status", "permission_request", "permission_decision", "attachment_begin",
  "attachment_chunk", "attachment_commit", "artifact_index_request", "artifact_index", "artifact_read_request",
  "artifact_chunk", "session_snapshot_request", "session_snapshot", "journal_replay_request", "journal_event",
]);

export const TASK_STATUSES = Object.freeze([
  "admitted", "running", "waiting_permission", "succeeded", "failed", "cancelled", "interrupted", "result_unknown",
]);

export const CONTROLLER_ERROR_CODES = Object.freeze([
  "pairing_code_invalid", "pairing_code_generation_failed", "controller_busy", "version_incompatible", "endpoint_offline", "enrollment_required",
  "invalid_message", "unsupported_capability", "permission_already_decided", "permission_expired", "result_unknown",
]);

export const LEGACY_WEB_CONFLICT = Object.freeze({
  message: Object.freeze({ v: 2, type: "error", code: "endpoint_not_found", message: "endpoint not found" }),
  closeCode: 1008,
});

export const CONTROLLER_SENSITIVE_FIELDS = Object.freeze([
  "endpoint_id", "connection_id", "pairing_id", "reservation_id", "controller_lease_id", "remote_control_generation",
  "controller_connection_identity", "task_id", "session_id", "permission_request_id", "transfer_id", "artifact_id",
  "workspace_id", "tool_id", "skill_id", "skill_ids", "tools", "skills", "workspaces", "artifacts",
  "pairing_code", "sealed_code_b64", "enrollment_proof_b64", "code_wrap_public_key_b64", "handshake_public_key_b64",
  "handshake_public_key_a_b64", "handshake_public_key_b_b64", "relay_signature_b64", "challenge_b64",
  "transport_challenge_b64", "ciphertext_b64", "resume_token_b64", "new_resume_token_b64",
  "receipt_secret_b64", "chunk_b64", "snapshot_b64", "event_b64", "api_key", "secret", "credential_ref",
  "file_name", "preview", "target_scope", "attachment_ids", "artifact_ids", "input", "text", "input_digest",
  "result_digest", "temporary_model_package",
]);

const ID = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/u;
const HEX_64 = /^[0-9a-f]{64}$/u;
const DECIMAL_U64 = /^(?:0|[1-9][0-9]{0,19})$/u;
const PAIRING_CODE = /^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{7}$/u;
const MIME = /^[A-Za-z0-9!#$&^_.+-]+\/[A-Za-z0-9!#$&^_.+-]+$/u;
const B64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/u;
const U64_MAX = 18_446_744_073_709_551_615n;
const U32_MAX = 4_294_967_295n;

const LEASE_CONTEXT_FIELDS = Object.freeze([
  "connection_id", "controller_lease_id", "lease_epoch", "relay_epoch", "remote_control_generation",
]);
const ROUTING_CONTEXT_FIELDS = Object.freeze(["connection_id", "controller_lease_id", "lease_epoch"]);

const MESSAGE_SCHEMAS = Object.freeze({
  controller_capabilities: schema(["v", "type", "request_id", "client_role", "sender_role", "protocol_version", "capabilities"], [], ["A", "B"]),
  desktop_endpoint_claim: schema(["v", "type", "request_id", "endpoint_id", "enrollment_proof_b64"], [], ["B"]),
  desktop_pairing_register: schema(["v", "type", "request_id", "registration_request_id", "endpoint_id", "remote_control_generation", "code_wrap_public_key_b64", "handshake_public_key_b64", "key_algorithm", "capabilities"], [], ["B"]),
  desktop_pairing_registered: schema(["v", "type", "request_id", "registration_request_id", "pairing_id", "remote_control_generation", "sealed_code_b64", "expires_at_ms", "relay_epoch"], [], ["relay"]),
  desktop_pairing_revoke: schema(["v", "type", "request_id", "endpoint_id", "remote_control_generation", "reason"], ["pairing_id"], ["B"]),
  controller_pairing_claim: schema(["v", "type", "request_id", "claim_request_id", "pairing_code", "handshake_public_key_b64", "key_algorithm", "capabilities"], [], ["A"]),
  controller_pairing_reserved: schema(["v", "type", "request_id", "pairing_id", "reservation_id", "remote_control_generation", "relay_epoch", "transcript", "transcript_hash", "relay_signing_key_id", "signature_algorithm", "reservation_expires_at_ms", "handshake_expires_at_ms", "relay_signature_b64"], [], ["relay"]),
  finished_receipt_commit: schema(["v", "type", "request_id", "pairing_id", "reservation_id", "remote_control_generation", "relay_epoch", "transcript_hash", "sender_role", "commitment"], [], ["A", "B"]),
  encrypted_handshake_finished: schema(["v", "type", "request_id", "pairing_id", "reservation_id", "remote_control_generation", "relay_epoch", "transcript_hash", "sender_role", "ciphertext_b64", "ciphertext_bytes"], [], ["A", "B"]),
  finished_receipt_ack: schema(["v", "type", "request_id", "pairing_id", "reservation_id", "remote_control_generation", "relay_epoch", "transcript_hash", "sender_role", "receiver_role", "receipt_secret_b64"], [], ["A", "B"]),
  controller_joined: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "liveness_epoch_a", "liveness_epoch_b", "resume_token_b64", "resume_deadline_ms"], [], ["relay"]),
  controller_liveness: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "sender_role", "liveness_epoch", "heartbeat_sequence"], [], ["A", "B"]),
  controller_liveness_challenge_a: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "challenge_id", "challenge_b64"], [], ["B"]),
  encrypted_controller_liveness_a: schema(["v", "type", ...ROUTING_CONTEXT_FIELDS, "ciphertext_b64", "ciphertext_bytes"], [], ["A"]),
  controller_liveness_attestation_request_a: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "challenge_id", "challenge_b64"], [], ["B"]),
  controller_liveness_attestation_a: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "liveness_epoch_a", "heartbeat_sequence_a", "attestation_sequence", "challenge_id", "challenge_b64", "observed_age_ms", "remaining_ttl_ms", "issued_at_relay", "relay_signing_key_id", "signature_algorithm", "relay_signature_b64"], [], ["relay"]),
  controller_resume_prepare: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "sender_role", "resume_token_b64", "transport_challenge_b64", "original_deadline_ms"], [], ["A", "B"]),
  controller_resume_provisional: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "activation_id", "original_deadline_ms"], [], ["relay"]),
  encrypted_resume_confirm: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "activation_id", "sender_role", "ciphertext_b64", "ciphertext_bytes"], [], ["A", "B"]),
  controller_resume_activation_ack: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "activation_id", "original_deadline_ms"], [], ["B"]),
  controller_resume_activation_pending: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "activation_id", "original_deadline_ms"], [], ["relay"]),
  controller_post_activation_liveness: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "activation_id", "liveness_epoch_b", "heartbeat_sequence_b"], [], ["B"]),
  controller_post_activation_liveness_confirmed: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "activation_id", "liveness_epoch_b", "heartbeat_sequence_b"], [], ["relay"]),
  controller_resume_ready_ack: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "activation_id", "sender_role", "liveness_epoch_b", "heartbeat_sequence_b"], [], ["A", "B"]),
  controller_resumed: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "activation_id", "liveness_epoch_a", "liveness_epoch_b", "new_resume_token_b64"], [], ["relay"]),
  encrypted_frame: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "frame_sequence", "direction", "payload_kind", "ciphertext_b64", "ciphertext_bytes"], [], ["A", "B"]),
  controller_disconnect: schema(["v", "type", "request_id", ...LEASE_CONTEXT_FIELDS, "sender_role", "reason"], [], ["A", "B"]),
  error: schema(["v", "type", "request_id", "code", "message"], [...LEASE_CONTEXT_FIELDS, "registration_request_id"], ["relay"]),
});

const PAYLOAD_SCHEMAS = Object.freeze({
  capability_snapshot_request: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "known_revision"], [], ["A"]),
  capability_snapshot: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "capability_revision", "expires_at_ms", "modes", "tools", "skills", "workspaces"], [], ["B"]),
  task_submit: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "task_id", "client_request_id", "session_id", "capability_revision", "model_source", "mode", "input_digest", "input"], ["temporary_model_package"], ["A"]),
  task_cancel: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "task_id", "session_id"], [], ["A"]),
  task_status: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "task_id", "session_id", "task_status"], ["result_digest"], ["B"]),
  permission_request: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "task_id", "permission_request_id", "expires_at_ms", "operation_type", "risk_level", "target_scope", "preview"], [], ["B"]),
  permission_decision: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "task_id", "permission_request_id", "request_id", "decision"], [], ["A"]),
  attachment_begin: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "task_id", "session_id", "transfer_id", "file_name", "mime_type", "total_bytes", "sha256"], [], ["A"]),
  attachment_chunk: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "task_id", "session_id", "transfer_id", "chunk_index", "chunk_b64", "chunk_bytes"], [], ["A"]),
  attachment_commit: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "task_id", "session_id", "transfer_id", "sha256"], [], ["A"]),
  artifact_index_request: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "session_id"], [], ["A"]),
  artifact_index: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "session_id", "artifacts"], [], ["B"]),
  artifact_read_request: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "session_id", "artifact_id", "offset", "max_bytes"], [], ["A"]),
  artifact_chunk: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "session_id", "artifact_id", "offset", "chunk_b64", "chunk_bytes", "eof"], ["sha256"], ["B"]),
  session_snapshot_request: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "session_id"], [], ["A"]),
  session_snapshot: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "session_id", "journal_epoch", "base_sequence", "snapshot_b64", "snapshot_bytes", "sha256"], [], ["B"]),
  journal_replay_request: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "request_id", "session_id", "journal_epoch", "after_sequence"], [], ["A"]),
  journal_event: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "session_id", "journal_epoch", "event_sequence", "event_kind", "event_b64", "event_bytes"], [], ["B"]),
});

const TRANSCRIPT_FIELDS = Object.freeze([
  "schema", "relay_epoch", "pairing_id", "reservation_id", "endpoint_id", "remote_control_generation",
  "controller_connection_identity", "role_a", "handshake_public_key_a_b64", "role_b", "handshake_public_key_b_b64",
  "protocol_version", "negotiated_capabilities", "capability_hash", "expires_at_ms", "nonce_b64",
]);
const TRANSCRIPT_SCALAR_PREFIX_FIELDS = Object.freeze(TRANSCRIPT_FIELDS.slice(0, 12));
const TRANSCRIPT_SCALAR_SUFFIX_FIELDS = Object.freeze(TRANSCRIPT_FIELDS.slice(13));
const ROUTE_REQUIRED = Object.freeze(["display_name", "preset", "wire_api", "base_url", "model", "image_capability_override"]);
const ROUTE_OPTIONAL = Object.freeze(["provider_kind", "vendor", "endpoint_mode", "context_window_tokens", "max_output_tokens", "reasoning_effort"]);
const NESTED_SCHEMAS = Object.freeze({
  task_input: schema(["text", "attachment_ids", "skill_ids"], ["workspace_id"], []),
  temporary_model_package_v1: schema(["schema", "package_id", "binding", "primary", "primary_credential"], ["vision_fallback"], []),
  temporary_model_binding: schema(["connection_id", "controller_lease_id", "lease_epoch", "task_id"], [], []),
  vision_fallback: schema(["route", "credential"], [], []),
  route: schema(ROUTE_REQUIRED, ROUTE_OPTIONAL, []),
  credential: schema(["kind", "secret"], [], []),
  tool_descriptor: schema(["tool_id", "display_name"], [], []),
  skill_descriptor: schema(["skill_id", "display_name"], [], []),
  workspace_descriptor: schema(["workspace_id", "display_name", "code_allowed"], [], []),
  artifact_descriptor: schema(["artifact_id", "task_id", "display_name", "mime_type", "size_bytes", "sha256", "preview_mode"], [], []),
  controller_liveness_a: schema(["v", "type", ...LEASE_CONTEXT_FIELDS, "role", "liveness_epoch_a", "heartbeat_sequence_a", "challenge_id", "challenge_b64"], [], []),
});

const PAYLOAD_CAPABILITIES = Object.freeze({
  capability_snapshot_request: "task_v1", capability_snapshot: "task_v1",
  task_submit: "task_v1", task_cancel: "task_v1", task_status: "task_v1",
  permission_request: "permission_v1", permission_decision: "permission_v1",
  attachment_begin: "attachment_v1", attachment_chunk: "attachment_v1", attachment_commit: "attachment_v1",
  artifact_index_request: "artifact_v1", artifact_index: "artifact_v1", artifact_read_request: "artifact_v1", artifact_chunk: "artifact_v1",
  session_snapshot_request: "snapshot_journal_v1", session_snapshot: "snapshot_journal_v1",
  journal_replay_request: "snapshot_journal_v1", journal_event: "snapshot_journal_v1",
});

const OUTER_MESSAGE_CAPABILITIES = Object.freeze({
  desktop_endpoint_claim: ["pairing_v1"],
  desktop_pairing_register: ["pairing_v1"], desktop_pairing_registered: ["pairing_v1"], desktop_pairing_revoke: ["pairing_v1"],
  controller_pairing_claim: ["pairing_v1"], controller_pairing_reserved: ["pairing_v1"], controller_joined: ["pairing_v1"],
  finished_receipt_commit: ["finished_receipt_v1"], encrypted_handshake_finished: ["finished_receipt_v1"], finished_receipt_ack: ["finished_receipt_v1"],
  controller_liveness: ["role_liveness_v1"], controller_liveness_challenge_a: ["role_liveness_v1"],
  encrypted_controller_liveness_a: ["role_liveness_v1"], controller_liveness_attestation_request_a: ["role_liveness_v1"], controller_liveness_attestation_a: ["role_liveness_v1"],
  controller_resume_prepare: ["resume_v1"], controller_resume_provisional: ["resume_v1"], encrypted_resume_confirm: ["resume_v1"],
  controller_resume_activation_ack: ["resume_v1"], controller_resume_activation_pending: ["resume_v1"], controller_post_activation_liveness: ["resume_v1"],
  controller_post_activation_liveness_confirmed: ["resume_v1"], controller_resume_ready_ack: ["resume_v1"], controller_resumed: ["resume_v1"],
  encrypted_frame: ["encrypted_frames_v1"], controller_disconnect: ["encrypted_frames_v1"],
});

const CONTRACT_ENUMS = Object.freeze({
  sender_role: ["A", "B"],
  client_role: ["desktop_controller", "desktop_controlled_endpoint"],
  key_algorithm: ["x25519"],
  signature_algorithm: ["ed25519"],
  direction: ["a_to_b", "b_to_a"],
  model_source: ["controlled_device", "controller_temporary"],
  mode: ["chat", "code"],
  task_status: TASK_STATUSES,
  decision: ["allow", "deny"],
  risk_level: ["low", "medium", "high", "critical"],
  event_kind: ["user_message", "assistant_delta", "reasoning", "tool", "permission", "error", "cancel", "artifact", "terminal"],
  preset: ["local_vllm", "deepseek", "kimi", "openai_compatible", "qwen", "doubao", "minimax", "glm", "mimo", "openai", "anthropic", "gemini", "xai"],
  wire_api: ["openai_chat_completions", "anthropic_messages"],
  provider_kind: ["custom", "official_api"],
  vendor: ["anthropic", "deepseek", "doubao", "gemini", "glm", "kimi", "mimo", "minimax", "openai", "qwen", "tencent", "xai"],
  endpoint_mode: ["full_chat_completions"],
  reasoning_effort: ["off", "low", "medium", "high", "max", "auto"],
  image_capability_override: ["pinvou", "enabled", "disabled"],
  artifact_preview_mode: ["inline_text", "inline_image", "download_only"],
  credential_kind: ["api_key"],
});

export class ControllerProtocolError extends Error {
  constructor(code, detail) {
    super(`${code}: ${detail}`);
    this.name = "ControllerProtocolError";
    this.code = code;
  }
}

function schema(required, optional, roles) {
  return Object.freeze({ required: Object.freeze(required), optional: Object.freeze(optional), roles: Object.freeze(roles) });
}

function fail(code, detail) { throw new ControllerProtocolError(code, detail); }

function plainObject(value, label) {
  if (!value || typeof value !== "object" || Array.isArray(value) || Object.getPrototypeOf(value) !== Object.prototype) fail("invalid_type", `${label} must be an object`);
  return value;
}

function safeFieldName(value) { return /^[A-Za-z0-9_]{1,64}$/u.test(value) ? value : "unknown"; }

function exactObject(value, required, optional, label) {
  const object = plainObject(value, label);
  const allowed = new Set([...required, ...optional]);
  for (const key of Object.keys(object)) if (!allowed.has(key)) fail("unknown_field", `${label}.${safeFieldName(key)}`);
  for (const key of required) if (!Object.hasOwn(object, key)) fail("missing_field", `${label}.${key}`);
  return object;
}

function text(value, field, maxCharacters, maxBytes = maxCharacters * 4, min = 1) {
  if (typeof value !== "string") fail("invalid_field", field);
  const characters = [...value].length;
  if (characters < min || characters > maxCharacters || Buffer.byteLength(value, "utf8") > maxBytes) fail("invalid_field", field);
  return value;
}

function identifier(value, field) { if (typeof value !== "string" || !ID.test(value)) fail("invalid_field", field); }

function decimal(value, field, max = U64_MAX) {
  if (typeof value !== "string" || !DECIMAL_U64.test(value)) fail("invalid_field", field);
  let parsed;
  try { parsed = BigInt(value); } catch { fail("invalid_field", field); }
  if (parsed > max) fail("invalid_field", field);
  return parsed;
}

export function classifyMonotonicSequence(incoming, previous, field = "sequence") {
  const incomingValue = decimal(incoming, field);
  const previousValue = decimal(previous, `last_${field}`);
  if (incomingValue < previousValue) fail("replay", field);
  return incomingValue === previousValue ? "same_sequence_tighten" : "advance";
}

export function classifyLivenessAttestation(message, expectedContext = {}) {
  const heartbeat = expectedContext.last_heartbeat_sequence_a === undefined
    ? "advance"
    : classifyMonotonicSequence(message.heartbeat_sequence_a, expectedContext.last_heartbeat_sequence_a, "heartbeat_sequence_a");
  const attestation = expectedContext.last_attestation_sequence === undefined
    ? "advance"
    : classifyStrictSequence(message.attestation_sequence, expectedContext.last_attestation_sequence, "attestation_sequence");
  return Object.freeze({
    heartbeat_sequence: heartbeat,
    attestation_sequence: attestation,
    deadline_policy: heartbeat === "advance" && attestation === "advance" ? "replace" : "tighten_only",
  });
}

function classifyStrictSequence(incoming, previous, field) {
  const incomingValue = decimal(incoming, field);
  const previousValue = decimal(previous, `last_${field}`);
  if (incomingValue <= previousValue) fail("replay", field);
  return "advance";
}

export function applyLivenessDeadline(existingDeadlineMs, candidateDeadlineMs, deadlinePolicy) {
  const existing = decimal(existingDeadlineMs, "existing_deadline_ms");
  const candidate = decimal(candidateDeadlineMs, "candidate_deadline_ms");
  oneOf(deadlinePolicy, ["replace", "tighten_only"], "deadline_policy");
  return (deadlinePolicy === "tighten_only" && existing < candidate ? existing : candidate).toString();
}

function oneOf(value, allowed, field) { if (!allowed.includes(value)) fail("invalid_enum", field); }
function hex64(value, field) { if (typeof value !== "string" || !HEX_64.test(value)) fail("invalid_field", field); }

function base64(value, field, maxBytes, exactBytes) {
  if (typeof value !== "string" || value.length === 0 || !B64.test(value)) fail("invalid_field", field);
  const bytes = Buffer.from(value, "base64");
  if (bytes.toString("base64") !== value || bytes.length > maxBytes || (exactBytes !== undefined && bytes.length !== exactBytes)) fail("invalid_field", field);
  return bytes.length;
}

function stringList(value, field, allowed, maxItems = 64) {
  if (!Array.isArray(value) || value.length > maxItems) fail("invalid_field", field);
  const seen = new Set();
  for (const item of value) {
    identifier(item, field);
    if (seen.has(item)) fail("invalid_field", field);
    if (allowed && !allowed.includes(item)) fail("invalid_enum", field);
    seen.add(item);
  }
}

function validateNamedField(object, field) {
  const value = object[field];
  if (value === undefined) return;
  if (field === "v" || field === "protocol_version") {
    if (value !== CONTROLLER_PROTOCOL_VERSION) fail("unsupported_version", field);
  } else if (field.endsWith("_id") || field.endsWith("_generation") || field === "relay_epoch" || field.startsWith("liveness_epoch") || field === "journal_epoch" || field === "controller_connection_identity" || field === "relay_signing_key_id") {
    identifier(value, field);
  } else if (field.endsWith("_sequence") || field.startsWith("heartbeat_sequence") || field.endsWith("_bytes") || field.endsWith("_ms") || field === "lease_epoch" || ["chunk_index", "offset", "max_bytes", "base_sequence", "after_sequence", "event_sequence", "issued_at_relay", "capability_revision", "known_revision"].includes(field)) {
    decimal(value, field, field === "chunk_bytes" || field === "max_bytes" ? BigInt(MAX_TRANSFER_CHUNK_BYTES) : U64_MAX);
  } else if (["transcript_hash", "capability_hash", "commitment", "sha256", "input_digest", "result_digest"].includes(field)) {
    hex64(value, field);
  } else if (field === "pairing_code") {
    if (typeof value !== "string" || !PAIRING_CODE.test(value)) fail("invalid_field", field);
  } else if (field === "capabilities" || field === "negotiated_capabilities") {
    if (Array.isArray(value) && value.some((item) => typeof item === "string" && !CONTROLLER_CAPABILITIES.includes(item))) fail("unsupported_capability", field);
    stringList(value, field, CONTROLLER_CAPABILITIES, CONTROLLER_CAPABILITIES.length);
  } else if (field === "attachment_ids" || field === "artifact_ids" || field === "skill_ids") {
    stringList(value, field, undefined, MAX_TASK_REFERENCES);
  } else if (field === "sender_role" || field === "receiver_role" || field === "role_a" || field === "role_b") {
    oneOf(value, ["A", "B"], field);
  } else if (field === "client_role") {
    oneOf(value, ["desktop_controller", "desktop_controlled_endpoint"], field);
  } else if (field === "key_algorithm") {
    oneOf(value, ["x25519"], field);
  } else if (field === "signature_algorithm") {
    oneOf(value, ["ed25519"], field);
  } else if (field === "direction") {
    oneOf(value, ["a_to_b", "b_to_a"], field);
  } else if (field === "payload_kind") {
    oneOf(value, CONTROLLER_PAYLOAD_TYPES, field);
  } else if (field === "model_source") {
    oneOf(value, ["controlled_device", "controller_temporary"], field);
  } else if (field === "mode") {
    oneOf(value, ["chat", "code"], field);
  } else if (field === "task_status") {
    oneOf(value, TASK_STATUSES, field);
  } else if (field === "decision") {
    oneOf(value, ["allow", "deny"], field);
  } else if (field === "risk_level") {
    oneOf(value, ["low", "medium", "high", "critical"], field);
  } else if (field === "event_kind") {
    oneOf(value, ["user_message", "assistant_delta", "reasoning", "tool", "permission", "error", "cancel", "artifact", "terminal"], field);
  } else if (field === "code") {
    oneOf(value, CONTROLLER_ERROR_CODES, field);
  } else if (field === "eof") {
    if (typeof value !== "boolean") fail("invalid_field", field);
  } else if (field === "mime_type") {
    if (typeof value !== "string" || value.length > 128 || !MIME.test(value)) fail("invalid_field", field);
  } else if (field === "file_name") {
    text(value, field, 255, 1024);
  } else if (["message", "preview", "target_scope"].includes(field)) {
    text(value, field, field === "message" ? 256 : 4096, field === "message" ? 1024 : 16 * 1024);
  } else if (field.endsWith("_public_key_b64")) {
    base64(value, field, 32, 32);
  } else if (field === "challenge_b64" || field === "transport_challenge_b64" || field === "nonce_b64") {
    base64(value, field, 16, 16);
  } else if (field === "receipt_secret_b64") {
    base64(value, field, 32, 32);
  } else if (field === "relay_signature_b64") {
    base64(value, field, 64, 64);
  } else if (field === "chunk_b64") {
    const decoded = base64(value, field, MAX_TRANSFER_CHUNK_BYTES);
    if (decimal(object.chunk_bytes, "chunk_bytes", BigInt(MAX_TRANSFER_CHUNK_BYTES)) !== BigInt(decoded)) fail("length_mismatch", field);
  } else if (field === "snapshot_b64" || field === "event_b64") {
    const decoded = base64(value, field, MAX_DECRYPTED_PAYLOAD_BYTES);
    const declaredField = field === "snapshot_b64" ? "snapshot_bytes" : "event_bytes";
    if (decimal(object[declaredField], declaredField, BigInt(MAX_DECRYPTED_PAYLOAD_BYTES)) !== BigInt(decoded)) fail("length_mismatch", field);
  } else if (field === "ciphertext_b64") {
    const decoded = base64(value, field, MAX_ENCRYPTED_FRAME_BYTES);
    if (decimal(object.ciphertext_bytes, "ciphertext_bytes") !== BigInt(decoded)) fail("length_mismatch", field);
  } else if (field.endsWith("_b64")) {
    base64(value, field, MAX_CONTROL_MESSAGE_BYTES);
  } else if (["type", "reason", "operation_type"].includes(field)) {
    text(value, field, 64, 256);
  } else if (!["input", "temporary_model_package", "transcript", "modes", "tools", "skills", "workspaces", "artifacts"].includes(field)) {
    identifier(value, field);
  }
}

function parseJson(input, maxBytes) {
  if (typeof input === "string") {
    const encodedBytes = Buffer.byteLength(input, "utf8");
    if (encodedBytes > maxBytes) fail("message_too_large", "json");
    try { return { value: JSON.parse(input), encodedBytes }; } catch { fail("invalid_json", "json"); }
  }
  const encoded = JSON.stringify(input);
  if (encoded === undefined || Buffer.byteLength(encoded, "utf8") > maxBytes) fail("message_too_large", "json");
  return { value: input, encodedBytes: Buffer.byteLength(encoded, "utf8") };
}

function enforceRoleBindings(object, senderRole) {
  if (object.sender_role !== undefined && object.type !== "finished_receipt_ack" && object.sender_role !== senderRole) fail("role_mismatch", "sender_role");
  if (object.type === "controller_capabilities") {
    const expected = senderRole === "A" ? "desktop_controller" : "desktop_controlled_endpoint";
    if (object.client_role !== expected || object.sender_role !== senderRole) fail("role_mismatch", "client_role");
  }
  if (object.type === "finished_receipt_ack" && (object.receiver_role !== senderRole || object.sender_role === senderRole)) fail("role_mismatch", "receiver_role");
  if (object.type === "encrypted_frame") {
    const expected = senderRole === "A" ? "a_to_b" : "b_to_a";
    if (object.direction !== expected) fail("role_mismatch", "direction");
    if (!PAYLOAD_SCHEMAS[object.payload_kind].roles.includes(senderRole)) fail("role_mismatch", "payload_kind");
  }
}

function enforceExpectedContext(object, expectedContext) {
  if (!expectedContext) return;
  for (const field of LEASE_CONTEXT_FIELDS) {
    if (object.type === "error" && !Object.hasOwn(object, field)) fail("missing_field", `message.${field}`);
    if (Object.hasOwn(object, field) && object[field] !== expectedContext[field]) fail("stale_context", field);
  }
  if ((object.type === "desktop_pairing_register" || object.type === "desktop_pairing_registered")
    && expectedContext.registration_request_id !== undefined
    && object.registration_request_id !== expectedContext.registration_request_id) fail("stale_context", "registration_request_id");
}

function validateWithSchema(input, senderRole, schemas, maxBytes, label, expectedContext) {
  oneOf(senderRole, ["A", "B", "relay"], "transport_sender_role");
  const { value, encodedBytes } = parseJson(input, maxBytes);
  const object = plainObject(value, label);
  if (typeof object.type !== "string" || !Object.hasOwn(schemas, object.type)) fail("unknown_message_type", `${label}.type`);
  const definition = schemas[object.type];
  if (!definition.roles.includes(senderRole)) fail("role_mismatch", object.type);
  exactObject(object, definition.required, definition.optional, label);
  for (const field of definition.required) validateNamedField(object, field);
  for (const field of definition.optional) validateNamedField(object, field);
  enforceRoleBindings(object, senderRole);
  enforceExpectedContext(object, expectedContext);
  return { value: Object.freeze({ ...object }), encodedBytes };
}

export function validateControllerMessage(input, senderRole, expectedContext) {
  const result = validateWithSchema(input, senderRole, MESSAGE_SCHEMAS, MAX_ENCRYPTED_WIRE_BYTES, "message", expectedContext);
  if (result.value.type !== "encrypted_frame" && result.encodedBytes > MAX_CONTROL_MESSAGE_BYTES) fail("message_too_large", "json");
  if (result.value.type === "controller_pairing_reserved") validateReservedTranscript(result.value);
  if (result.value.type === "controller_liveness_attestation_a") validateAttestation(result.value, expectedContext);
  return result.value;
}

function validateAttestation(message, expectedContext) {
  decimal(message.observed_age_ms, "observed_age_ms", 30_000n);
  decimal(message.remaining_ttl_ms, "remaining_ttl_ms", 30_000n);
  if (!expectedContext) return;
  if (expectedContext.liveness_epoch_a !== undefined && message.liveness_epoch_a !== expectedContext.liveness_epoch_a) fail("stale_context", "liveness_epoch_a");
  if (expectedContext.challenge_id !== undefined && message.challenge_id !== expectedContext.challenge_id) fail("stale_context", "challenge_id");
  if (expectedContext.challenge_b64 !== undefined && message.challenge_b64 !== expectedContext.challenge_b64) fail("stale_context", "challenge_b64");
  classifyLivenessAttestation(message, expectedContext);
}

export function validateControllerPayload(input, senderRole, expectedContext) {
  const result = validateWithSchema(input, senderRole, PAYLOAD_SCHEMAS, MAX_DECRYPTED_PAYLOAD_BYTES, "payload", expectedContext);
  if (result.value.type === "task_submit") validateTaskSubmit(result.value);
  if (result.value.type === "capability_snapshot") validateCapabilitySnapshot(result.value);
  if (result.value.type === "artifact_index") validateArtifactIndex(result.value);
  return result.value;
}

function requireNegotiatedCapabilities(runtimeContext) {
  if (!runtimeContext || !Array.isArray(runtimeContext.negotiated_capabilities)) fail("unsupported_capability", "negotiated_capabilities");
  stringList(runtimeContext.negotiated_capabilities, "negotiated_capabilities", CONTROLLER_CAPABILITIES, CONTROLLER_CAPABILITIES.length);
  return runtimeContext.negotiated_capabilities;
}

function requireRuntimeLease(runtimeContext) {
  if (!runtimeContext || !runtimeContext.lease) fail("missing_field", "runtime_context.lease");
  const lease = exactObject(runtimeContext.lease, LEASE_CONTEXT_FIELDS, [], "runtime_context.lease");
  for (const field of LEASE_CONTEXT_FIELDS) validateNamedField(lease, field);
  return lease;
}

function enforceNegotiatedPayload(payloadKind, runtimeContext) {
  const capabilities = requireNegotiatedCapabilities(runtimeContext);
  const required = PAYLOAD_CAPABILITIES[payloadKind];
  if (!required || !capabilities.includes(required)) fail("unsupported_capability", payloadKind);
}

function enforceNegotiatedMessage(message, runtimeContext) {
  if (message.type === "controller_capabilities" || message.type === "error") return;
  const capabilities = requireNegotiatedCapabilities(runtimeContext);
  for (const required of OUTER_MESSAGE_CAPABILITIES[message.type] ?? []) {
    if (!capabilities.includes(required)) fail("unsupported_capability", message.type);
  }
  if (message.type === "encrypted_frame") enforceNegotiatedPayload(message.payload_kind, runtimeContext);
}

export function validateControllerRuntimeMessage(input, senderRole, runtimeContext) {
  const shape = validateControllerMessage(input, senderRole);
  enforceNegotiatedMessage(shape, runtimeContext);
  const needsLease = MESSAGE_SCHEMAS[shape.type].required.includes("controller_lease_id");
  const lease = needsLease ? requireRuntimeLease(runtimeContext) : runtimeContext?.lease;
  const message = lease === undefined ? shape : validateControllerMessage(input, senderRole, lease);
  return message;
}

export function validateControllerRuntimePayload(input, senderRole, runtimeContext) {
  const shape = validateControllerPayload(input, senderRole);
  enforceNegotiatedPayload(shape.type, runtimeContext);
  const lease = requireRuntimeLease(runtimeContext);
  const payload = validateControllerPayload(input, senderRole, lease);
  return payload;
}

function comparableRegistrationRequest(value) {
  const clone = { ...value };
  delete clone.request_id;
  return canonicalJson(clone);
}

function comparableRegistrationResult(value) {
  const clone = { ...value };
  delete clone.request_id;
  return canonicalJson(clone);
}

export class RegistrationValidationContext {
  constructor(request, sealedResult) {
    const validated = validateControllerMessage(request, "B");
    if (validated.type !== "desktop_pairing_register") fail("invalid_message", "registration request");
    let validatedResult;
    if (sealedResult !== undefined) {
      validatedResult = validateControllerMessage(sealedResult, "relay");
      if (validatedResult.type !== "desktop_pairing_registered" && validatedResult.type !== "error") fail("invalid_message", "registration result");
      for (const field of ["registration_request_id", "remote_control_generation"]) {
        if (!Object.hasOwn(validatedResult, field)) fail("missing_field", `message.${field}`);
        if (validatedResult[field] !== validated[field]) fail("invalid_message", field);
      }
    }
    this.request = validated;
    this.requestKey = comparableRegistrationRequest(validated);
    this.sealedResult = validatedResult === undefined ? undefined : comparableRegistrationResult(validatedResult);
    Object.freeze(this);
  }
}

export function validateControllerRegistrationMessage(input, senderRole, context) {
  if (!(context instanceof RegistrationValidationContext)) fail("invalid_message", "registration_validation_context");
  const candidate = parseJson(input, MAX_CONTROL_MESSAGE_BYTES).value;
  if (candidate?.type === "desktop_pairing_register"
    && candidate.registration_request_id === context.request.registration_request_id
    && comparableRegistrationRequest(candidate) !== context.requestKey) fail("invalid_message", "registration idempotency conflict");
  const message = validateControllerMessage(input, senderRole);
  if (message.type === "desktop_pairing_register") {
    if (message.registration_request_id === context.request.registration_request_id
      && comparableRegistrationRequest(message) !== context.requestKey) fail("invalid_message", "registration idempotency conflict");
    if (message.registration_request_id !== context.request.registration_request_id) fail("invalid_message", "registration_request_id");
    return message;
  }
  if (message.type !== "desktop_pairing_registered" && message.type !== "error") fail("invalid_message", "registration result");
  for (const field of ["registration_request_id", "remote_control_generation"]) {
    if (!Object.hasOwn(message, field)) fail("missing_field", `message.${field}`);
    if (message[field] !== context.request[field]) fail("invalid_message", field);
  }
  if (context.sealedResult !== undefined && comparableRegistrationResult(message) !== context.sealedResult) fail("invalid_message", "sealed registration result");
  return message;
}

function validateTaskSubmit(payload) {
  const input = exactObject(payload.input, NESTED_SCHEMAS.task_input.required, NESTED_SCHEMAS.task_input.optional, "payload.input");
  text(input.text, "input.text", MAX_TASK_INPUT_TEXT_BYTES, MAX_TASK_INPUT_TEXT_BYTES, 0);
  stringList(input.attachment_ids, "input.attachment_ids", undefined, MAX_TASK_REFERENCES);
  stringList(input.skill_ids, "input.skill_ids", undefined, MAX_TASK_REFERENCES);
  if (input.workspace_id !== undefined) identifier(input.workspace_id, "input.workspace_id");
  if (input.text.length === 0 && input.attachment_ids.length === 0) fail("invalid_field", "payload.input");
  if (payload.model_source === "controller_temporary") {
    if (!Object.hasOwn(payload, "temporary_model_package")) fail("missing_field", "payload.temporary_model_package");
    validateTemporaryModelPackage(payload.temporary_model_package, payload);
  } else if (Object.hasOwn(payload, "temporary_model_package")) {
    fail("invalid_field", "payload.temporary_model_package");
  }
}

function validateCapabilitySnapshot(payload) {
  decimal(payload.capability_revision, "capability_revision");
  if (!Array.isArray(payload.modes) || payload.modes.length === 0 || payload.modes.length > 2 || new Set(payload.modes).size !== payload.modes.length) fail("invalid_field", "modes");
  for (const mode of payload.modes) oneOf(mode, ["chat", "code"], "modes");
  validateDescriptorList(payload.tools, "tools", "tool_id", false);
  validateDescriptorList(payload.skills, "skills", "skill_id", false);
  validateDescriptorList(payload.workspaces, "workspaces", "workspace_id", true);
}

function validateDescriptorList(value, field, idField, workspace) {
  if (!Array.isArray(value) || value.length > MAX_DESCRIPTORS) fail("invalid_field", field);
  const ids = new Set();
  for (const [index, item] of value.entries()) {
    const label = `${field}[${index}]`;
    const definition = NESTED_SCHEMAS[workspace ? "workspace_descriptor" : idField === "tool_id" ? "tool_descriptor" : "skill_descriptor"];
    const descriptor = exactObject(item, definition.required, definition.optional, label);
    identifier(descriptor[idField], `${label}.${idField}`);
    if (ids.has(descriptor[idField])) fail("invalid_field", field);
    ids.add(descriptor[idField]);
    text(descriptor.display_name, `${label}.display_name`, 128, 512);
    if (workspace && typeof descriptor.code_allowed !== "boolean") fail("invalid_field", `${label}.code_allowed`);
  }
}

function validateArtifactIndex(payload) {
  if (!Array.isArray(payload.artifacts) || payload.artifacts.length > MAX_ARTIFACTS) fail("invalid_field", "artifacts");
  const ids = new Set();
  for (const [index, item] of payload.artifacts.entries()) {
    const label = `artifacts[${index}]`;
    const artifact = exactObject(item, NESTED_SCHEMAS.artifact_descriptor.required, NESTED_SCHEMAS.artifact_descriptor.optional, label);
    identifier(artifact.artifact_id, `${label}.artifact_id`);
    identifier(artifact.task_id, `${label}.task_id`);
    if (ids.has(artifact.artifact_id)) fail("invalid_field", "artifacts");
    ids.add(artifact.artifact_id);
    text(artifact.display_name, `${label}.display_name`, 255, 1024);
    if (typeof artifact.mime_type !== "string" || artifact.mime_type.length > 128 || !MIME.test(artifact.mime_type)) fail("invalid_field", `${label}.mime_type`);
    decimal(artifact.size_bytes, `${label}.size_bytes`);
    hex64(artifact.sha256, `${label}.sha256`);
    oneOf(artifact.preview_mode, ["inline_text", "inline_image", "download_only"], `${label}.preview_mode`);
  }
}

function validateTemporaryModelPackage(value, task) {
  if (Buffer.byteLength(JSON.stringify(value), "utf8") > MAX_TEMPORARY_MODEL_PACKAGE_BYTES) fail("message_too_large", "temporary_model_package");
  const object = exactObject(value, NESTED_SCHEMAS.temporary_model_package_v1.required, NESTED_SCHEMAS.temporary_model_package_v1.optional, "temporary_model_package");
  if (object.schema !== "temporary_model_package/v1") fail("unsupported_version", "temporary_model_package.schema");
  identifier(object.package_id, "temporary_model_package.package_id");
  const binding = exactObject(object.binding, NESTED_SCHEMAS.temporary_model_binding.required, NESTED_SCHEMAS.temporary_model_binding.optional, "temporary_model_package.binding");
  for (const field of ["connection_id", "controller_lease_id", "task_id"]) identifier(binding[field], `temporary_model_package.binding.${field}`);
  decimal(binding.lease_epoch, "temporary_model_package.binding.lease_epoch");
  for (const field of ["connection_id", "controller_lease_id", "lease_epoch", "task_id"]) if (binding[field] !== task[field]) fail("stale_context", `temporary_model_package.binding.${field}`);
  validateRoute(object.primary, "temporary_model_package.primary");
  validateCredential(object.primary_credential, "temporary_model_package.primary_credential");
  if (object.vision_fallback !== undefined) {
    const fallback = exactObject(object.vision_fallback, NESTED_SCHEMAS.vision_fallback.required, NESTED_SCHEMAS.vision_fallback.optional, "temporary_model_package.vision_fallback");
    validateRoute(fallback.route, "temporary_model_package.vision_fallback.route");
    validateCredential(fallback.credential, "temporary_model_package.vision_fallback.credential");
  }
}

function validateRoute(value, label) {
  const route = exactObject(value, ROUTE_REQUIRED, ROUTE_OPTIONAL, label);
  text(route.display_name, `${label}.display_name`, 128, 512);
  oneOf(route.preset, ["local_vllm", "deepseek", "kimi", "openai_compatible", "qwen", "doubao", "minimax", "glm", "mimo", "openai", "anthropic", "gemini", "xai"], `${label}.preset`);
  oneOf(route.wire_api, ["openai_chat_completions", "anthropic_messages"], `${label}.wire_api`);
  if ((route.preset === "anthropic") !== (route.wire_api === "anthropic_messages")) fail("invalid_field", `${label}.wire_api`);
  text(route.base_url, `${label}.base_url`, 2048, 2048);
  validateControllerBaseUrl(route.base_url, `${label}.base_url`);
  text(route.model, `${label}.model`, 256, 1024);
  oneOf(route.image_capability_override, ["pinvou", "enabled", "disabled"], `${label}.image_capability_override`);
  if (route.provider_kind !== undefined) oneOf(route.provider_kind, ["custom", "official_api"], `${label}.provider_kind`);
  if (route.vendor !== undefined) oneOf(route.vendor, ["anthropic", "deepseek", "doubao", "gemini", "glm", "kimi", "mimo", "minimax", "openai", "qwen", "tencent", "xai"], `${label}.vendor`);
  if (route.endpoint_mode !== undefined) oneOf(route.endpoint_mode, ["full_chat_completions"], `${label}.endpoint_mode`);
  if (route.context_window_tokens !== undefined) decimal(route.context_window_tokens, `${label}.context_window_tokens`, U32_MAX);
  if (route.max_output_tokens !== undefined) decimal(route.max_output_tokens, `${label}.max_output_tokens`, U32_MAX);
  if (route.reasoning_effort !== undefined) oneOf(route.reasoning_effort, ["off", "low", "medium", "high", "max", "auto"], `${label}.reasoning_effort`);
}

export function validateControllerBaseUrl(value, field = "base_url") {
  if (typeof value !== "string" || value.length === 0 || value.length > 2048 || !/^[\x21-\x7e]+$/u.test(value)) fail("invalid_field", field);
  const schemeEnd = value.indexOf("://");
  if (schemeEnd < 0) fail("invalid_field", field);
  const scheme = value.slice(0, schemeEnd);
  if (scheme !== "http" && scheme !== "https") fail("invalid_field", field);
  const rest = value.slice(schemeEnd + 3);
  const authorityEnd = rest.search(/[/?#]/u);
  const authority = authorityEnd < 0 ? rest : rest.slice(0, authorityEnd);
  const suffix = authorityEnd < 0 ? "" : rest.slice(authorityEnd);
  if (!authority || authority.includes("@") || authority.includes("[") || authority.includes("]") || suffix.includes("#")) fail("invalid_field", field);

  const colon = authority.lastIndexOf(":");
  const host = colon < 0 ? authority : authority.slice(0, colon);
  const port = colon < 0 ? undefined : authority.slice(colon + 1);
  if (!host || !validAsciiHost(host)) fail("invalid_field", field);
  if (port !== undefined && (!/^(?:[1-9]|[1-9][0-9]{1,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5])$/u.test(port) || Number(port) > 65_535)) fail("invalid_field", field);
  if (suffix && suffix[0] !== "/" && suffix[0] !== "?") fail("invalid_field", field);
  if (!validUrlSuffix(suffix)) fail("invalid_field", field);
  return value;
}

function validAsciiHost(host) {
  if (host.length > 253 || host !== host.toLowerCase() || host.endsWith(".")) return false;
  if (/^[0-9.]+$/u.test(host)) {
    const parts = host.split(".");
    return parts.length === 4 && parts.every((part) => /^(?:0|[1-9][0-9]{0,2})$/u.test(part) && Number(part) <= 255);
  }
  return host.split(".").every((label) => /^(?:[a-z0-9]|[a-z0-9][a-z0-9-]{0,61}[a-z0-9])$/u.test(label));
}

function validUrlSuffix(suffix) {
  for (let index = 0; index < suffix.length; index += 1) {
    const char = suffix[index];
    if (char === "%") {
      if (!/^[0-9A-F]{2}$/u.test(suffix.slice(index + 1, index + 3))) return false;
      index += 2;
    } else if (!/[A-Za-z0-9\-._~!$&'()*+,;=:@/?]/u.test(char)) {
      return false;
    }
  }
  return true;
}

function validateCredential(value, label) {
  const credential = exactObject(value, NESTED_SCHEMAS.credential.required, NESTED_SCHEMAS.credential.optional, label);
  if (credential.kind !== "api_key") fail("invalid_enum", `${label}.kind`);
  text(credential.secret, `${label}.secret`, 8192, 8192);
}

function lp(value) {
  const bytes = Buffer.isBuffer(value) ? value : Buffer.from(String(value), "utf8");
  const length = Buffer.alloc(4);
  length.writeUInt32BE(bytes.length);
  return Buffer.concat([length, bytes]);
}

export function controllerCapabilityHash(capabilities) {
  if (Array.isArray(capabilities) && capabilities.some((item) => typeof item === "string" && !CONTROLLER_CAPABILITIES.includes(item))) fail("unsupported_capability", "capabilities");
  stringList(capabilities, "capabilities", CONTROLLER_CAPABILITIES, CONTROLLER_CAPABILITIES.length);
  const sorted = [...capabilities].sort();
  return createHash("sha256").update(Buffer.concat([Buffer.from("pinvou-controller-capabilities-v1", "ascii"), ...sorted.map(lp)])).digest("hex");
}

export function buildPairingTranscriptPreimage(value) {
  const transcript = validateTranscript(value);
  return Buffer.concat([
    Buffer.from("pinvou-controller-pairing-transcript-v1", "ascii"),
    ...TRANSCRIPT_SCALAR_PREFIX_FIELDS.map((field) => lp(transcript[field])),
    lp(String(transcript.negotiated_capabilities.length)),
    ...transcript.negotiated_capabilities.map(lp),
    ...TRANSCRIPT_SCALAR_SUFFIX_FIELDS.map((field) => lp(transcript[field])),
  ]);
}

function validateTranscript(value) {
  const transcript = exactObject(value, TRANSCRIPT_FIELDS, [], "transcript");
  if (transcript.schema !== "controller_pairing_transcript/v1") fail("unsupported_version", "transcript.schema");
  for (const field of ["relay_epoch", "pairing_id", "reservation_id", "endpoint_id", "remote_control_generation", "controller_connection_identity"]) identifier(transcript[field], `transcript.${field}`);
  if (transcript.role_a !== "A" || transcript.role_b !== "B") fail("role_mismatch", "transcript.role");
  base64(transcript.handshake_public_key_a_b64, "transcript.handshake_public_key_a_b64", 32, 32);
  base64(transcript.handshake_public_key_b_b64, "transcript.handshake_public_key_b_b64", 32, 32);
  if (transcript.handshake_public_key_a_b64 === transcript.handshake_public_key_b_b64) fail("invalid_field", "transcript.handshake_public_keys");
  if (transcript.protocol_version !== CONTROLLER_PROTOCOL_VERSION) fail("unsupported_version", "transcript.protocol_version");
  stringList(transcript.negotiated_capabilities, "transcript.negotiated_capabilities", CONTROLLER_CAPABILITIES, CONTROLLER_CAPABILITIES.length);
  if (transcript.negotiated_capabilities.length === 0) fail("unsupported_capability", "transcript.negotiated_capabilities");
  if (transcript.negotiated_capabilities.some((item, index, list) => index > 0 && list[index - 1] >= item)) fail("invalid_field", "transcript.negotiated_capabilities");
  hex64(transcript.capability_hash, "transcript.capability_hash");
  if (controllerCapabilityHash(transcript.negotiated_capabilities) !== transcript.capability_hash) fail("invalid_field", "transcript.capability_hash");
  decimal(transcript.expires_at_ms, "transcript.expires_at_ms");
  base64(transcript.nonce_b64, "transcript.nonce_b64", 16, 16);
  return transcript;
}

export function negotiateControllerCapabilities(capabilitiesA, capabilitiesB) {
  stringList(capabilitiesA, "capabilities_a", CONTROLLER_CAPABILITIES, CONTROLLER_CAPABILITIES.length);
  stringList(capabilitiesB, "capabilities_b", CONTROLLER_CAPABILITIES, CONTROLLER_CAPABILITIES.length);
  const supportedByB = new Set(capabilitiesB);
  const negotiated = capabilitiesA.filter((capability) => supportedByB.has(capability)).sort();
  if (negotiated.length === 0) fail("unsupported_capability", "negotiated_capabilities");
  return Object.freeze(negotiated);
}

function validateReservedTranscript(message) {
  const transcript = validateTranscript(message.transcript);
  for (const field of ["pairing_id", "reservation_id", "remote_control_generation", "relay_epoch"]) if (message[field] !== transcript[field]) fail("stale_context", `transcript.${field}`);
  const hash = createHash("sha256").update(buildPairingTranscriptPreimage(transcript)).digest("hex");
  if (message.transcript_hash !== hash) fail("invalid_field", "transcript_hash");
}

export function buildDirectLivenessAad(value) {
  const fields = ["connection_id", "controller_lease_id", "lease_epoch", "relay_epoch", "remote_control_generation", "role"];
  const binding = exactObject(value, fields, [], "liveness_aad");
  for (const field of fields.slice(0, 2).concat(fields.slice(3, 5))) identifier(binding[field], `liveness_aad.${field}`);
  decimal(binding.lease_epoch, "liveness_aad.lease_epoch");
  if (binding.role !== "A") fail("role_mismatch", "liveness_aad.role");
  return Buffer.concat([Buffer.from("pinvou-controller-liveness-aad-v1", "ascii"), ...fields.map((field) => lp(binding[field]))]);
}

export function validateControllerLivenessPayload(input, expectedContext) {
  const { value } = parseJson(input, MAX_CONTROL_MESSAGE_BYTES);
  const object = exactObject(value, NESTED_SCHEMAS.controller_liveness_a.required, NESTED_SCHEMAS.controller_liveness_a.optional, "liveness_payload");
  if (object.v !== 1) fail("unsupported_version", "v");
  if (object.type !== "controller_liveness_a") fail("unknown_message_type", "liveness_payload.type");
  for (const field of LEASE_CONTEXT_FIELDS) validateNamedField(object, field);
  if (object.role !== "A") fail("role_mismatch", "role");
  identifier(object.liveness_epoch_a, "liveness_epoch_a");
  decimal(object.heartbeat_sequence_a, "heartbeat_sequence_a");
  identifier(object.challenge_id, "challenge_id");
  base64(object.challenge_b64, "challenge_b64", 16, 16);
  enforceExpectedContext(object, expectedContext);
  if (expectedContext?.liveness_epoch_a !== undefined && object.liveness_epoch_a !== expectedContext.liveness_epoch_a) fail("stale_context", "liveness_epoch_a");
  if (expectedContext?.challenge_id !== undefined && object.challenge_id !== expectedContext.challenge_id) fail("stale_context", "challenge_id");
  if (expectedContext?.challenge_b64 !== undefined && object.challenge_b64 !== expectedContext.challenge_b64) fail("stale_context", "challenge_b64");
  if (expectedContext?.last_heartbeat_sequence_a !== undefined) classifyMonotonicSequence(object.heartbeat_sequence_a, expectedContext.last_heartbeat_sequence_a, "heartbeat_sequence_a");
  return Object.freeze({ ...object });
}

const ATTESTATION_FIELDS = Object.freeze([
  "relay_epoch", "connection_id", "controller_lease_id", "lease_epoch", "remote_control_generation",
  "liveness_epoch_a", "heartbeat_sequence_a", "attestation_sequence", "challenge_b64", "observed_age_ms",
  "remaining_ttl_ms", "issued_at_relay", "signature_algorithm", "relay_signing_key_id",
]);

export function buildLivenessAttestationPreimage(message) {
  if (message.type !== "controller_liveness_attestation_a") fail("invalid_field", "type");
  return Buffer.concat([Buffer.from("pinvou-controller-liveness-attestation-v1", "ascii"), ...ATTESTATION_FIELDS.map((field) => lp(message[field]))]);
}

export function controllerAllowedRoles(type) {
  const definition = MESSAGE_SCHEMAS[type];
  return definition ? [...definition.roles] : undefined;
}

function normalizedSchemas(schemas) {
  return Object.fromEntries(Object.entries(schemas).map(([type, definition]) => [type, {
    required: [...definition.required], optional: [...definition.optional], roles: [...definition.roles],
  }]));
}

export function controllerContractManifest() {
  return {
    protocol_version: CONTROLLER_PROTOCOL_VERSION,
    subprotocol: CONTROLLER_SUBPROTOCOL,
    integer_wire_format: "canonical-u64-decimal-string",
    limits: {
      max_control_message_bytes: MAX_CONTROL_MESSAGE_BYTES,
      max_encrypted_frame_bytes: MAX_ENCRYPTED_FRAME_BYTES,
      max_decrypted_payload_bytes: MAX_DECRYPTED_PAYLOAD_BYTES,
      max_transfer_chunk_bytes: MAX_TRANSFER_CHUNK_BYTES,
      max_task_input_text_bytes: MAX_TASK_INPUT_TEXT_BYTES,
      max_temporary_model_package_bytes: MAX_TEMPORARY_MODEL_PACKAGE_BYTES,
      max_id_chars: MAX_ID_CHARS,
    },
    capabilities: [...CONTROLLER_CAPABILITIES],
    message_types: [...CONTROLLER_MESSAGE_TYPES],
    payload_types: [...CONTROLLER_PAYLOAD_TYPES],
    task_statuses: [...TASK_STATUSES],
    error_codes: [...CONTROLLER_ERROR_CODES],
    message_schemas: normalizedSchemas(MESSAGE_SCHEMAS),
    payload_schemas: normalizedSchemas(PAYLOAD_SCHEMAS),
    enums: Object.fromEntries(Object.entries(CONTRACT_ENUMS).map(([key, values]) => [key, [...values]])),
    payload_capabilities: { ...PAYLOAD_CAPABILITIES },
    outer_message_capabilities: Object.fromEntries(Object.entries(OUTER_MESSAGE_CAPABILITIES).map(([key, values]) => [key, [...values]])),
    transcript_fields: [...TRANSCRIPT_FIELDS],
    route_required: [...ROUTE_REQUIRED],
    route_optional: [...ROUTE_OPTIONAL],
    nested_schemas: Object.fromEntries(Object.entries(NESTED_SCHEMAS).map(([name, definition]) => [name, { required: [...definition.required], optional: [...definition.optional] }])),
    runtime_pre_lease_messages: CONTROLLER_MESSAGE_TYPES.filter((type) => !MESSAGE_SCHEMAS[type].required.includes("controller_lease_id")),
    collection_limits: { task_attachment_ids: MAX_TASK_REFERENCES, task_skill_ids: MAX_TASK_REFERENCES, descriptors: MAX_DESCRIPTORS, artifacts: MAX_ARTIFACTS, capabilities: CONTROLLER_CAPABILITIES.length },
    base_url_grammar: "lowercase-http(s)-dns-or-ipv4-port1-65535-no-userinfo-ipv6-fragment-uppercase-percent",
    liveness_sequence_policy: {
      heartbeat: { lower: "replay", equal: "same_sequence_tighten", greater: "advance" },
      attestation: { lower: "replay", equal: "replay", greater: "advance" },
    },
    validation_modes: {
      shape_only: "schema-role-lease-shape-without-capability-state",
      runtime: "negotiated-capabilities-required-for-state-machine-messages",
      registration: "independent-generation-registration-idempotency-context",
    },
  };
}

function canonicalJson(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonicalJson(value[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

export function canonicalControllerContractHash(fixture) {
  const fixtureWithoutHash = structuredClone(fixture);
  if (fixtureWithoutHash?.contract) delete fixtureWithoutHash.contract.sha256;
  return createHash("sha256").update(canonicalJson({ contract: controllerContractManifest(), fixture: fixtureWithoutHash }), "utf8").digest("hex");
}

export function controllerLogProjection(input) {
  const object = plainObject(input, "message");
  const safe = {};
  if (object.v === CONTROLLER_PROTOCOL_VERSION) safe.v = object.v;
  if (CONTROLLER_MESSAGE_TYPES.includes(object.type)) safe.type = object.type;
  if (typeof object.request_id === "string" && ID.test(object.request_id)) safe.request_id = object.request_id;
  if (["A", "B"].includes(object.sender_role)) safe.sender_role = object.sender_role;
  if (CONTROLLER_ERROR_CODES.includes(object.code)) safe.code = object.code;
  return safe;
}
