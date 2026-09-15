import { Buffer } from "node:buffer";

export const CONTROLLER_PROTOCOL_VERSION = 1;
export const MAX_CONTROL_MESSAGE_BYTES = 64 * 1024;
export const MAX_ENCRYPTED_FRAME_BYTES = 2 * 1024 * 1024;
export const MAX_ENCRYPTED_WIRE_BYTES = Math.ceil(MAX_ENCRYPTED_FRAME_BYTES / 3) * 4 + MAX_CONTROL_MESSAGE_BYTES;
export const MAX_DECRYPTED_PAYLOAD_BYTES = 512 * 1024;
export const MAX_TRANSFER_CHUNK_BYTES = 256 * 1024;
export const MAX_ID_CHARS = 128;

export const CONTROLLER_CAPABILITIES = Object.freeze([
  "pairing_v1",
  "encrypted_frames_v1",
  "finished_receipt_v1",
  "role_liveness_v1",
  "resume_v1",
  "task_v1",
  "permission_v1",
  "attachment_v1",
  "artifact_v1",
  "snapshot_journal_v1",
]);

export const CONTROLLER_MESSAGE_TYPES = Object.freeze([
  "controller_capabilities",
  "desktop_endpoint_claim",
  "desktop_pairing_register",
  "desktop_pairing_registered",
  "desktop_pairing_revoke",
  "controller_pairing_claim",
  "controller_pairing_reserved",
  "finished_receipt_commit",
  "encrypted_handshake_finished",
  "finished_receipt_ack",
  "controller_joined",
  "controller_liveness",
  "controller_liveness_challenge_a",
  "encrypted_controller_liveness_a",
  "controller_liveness_attestation_request_a",
  "controller_liveness_attestation_a",
  "controller_resume_prepare",
  "controller_resume_provisional",
  "encrypted_resume_confirm",
  "controller_resume_activation_ack",
  "controller_resume_activation_pending",
  "controller_post_activation_liveness",
  "controller_post_activation_liveness_confirmed",
  "controller_resume_ready_ack",
  "controller_resumed",
  "encrypted_frame",
  "controller_disconnect",
  "error",
]);

export const CONTROLLER_PAYLOAD_TYPES = Object.freeze([
  "task_submit",
  "task_cancel",
  "task_terminal",
  "permission_request",
  "permission_decision",
  "attachment_begin",
  "attachment_chunk",
  "attachment_commit",
  "artifact_index_request",
  "artifact_index",
  "artifact_read_request",
  "artifact_chunk",
  "session_snapshot_request",
  "session_snapshot",
  "journal_replay_request",
  "journal_event",
]);

export const CONTROLLER_ERROR_CODES = Object.freeze([
  "pairing_code_invalid",
  "controller_busy",
  "version_incompatible",
  "endpoint_offline",
  "enrollment_required",
  "invalid_message",
  "unsupported_capability",
  "permission_already_decided",
  "permission_expired",
  "result_unknown",
]);

export const LEGACY_WEB_CONFLICT = Object.freeze({
  message: Object.freeze({
    v: 2,
    type: "error",
    code: "endpoint_not_found",
    message: "endpoint not found",
  }),
  closeCode: 1008,
});

export const CONTROLLER_SENSITIVE_FIELDS = Object.freeze([
  "endpoint_id",
  "connection_id",
  "pairing_id",
  "reservation_id",
  "controller_lease_id",
  "remote_control_generation",
  "task_id",
  "session_id",
  "permission_request_id",
  "transfer_id",
  "artifact_id",
  "pairing_code",
  "sealed_code_b64",
  "enrollment_proof_b64",
  "code_wrap_public_key_b64",
  "handshake_public_key_b64",
  "peer_handshake_public_key_b64",
  "relay_signature_b64",
  "challenge_b64",
  "transport_challenge_b64",
  "ciphertext_b64",
  "resume_token_b64",
  "new_resume_token_b64",
  "receipt_secret_b64",
  "chunk_b64",
  "snapshot_b64",
  "event_b64",
  "api_key",
  "credential_ref",
  "file_name",
  "preview",
  "target_scope",
  "attachment_ids",
  "artifact_ids",
  "input_digest",
  "result_digest",
]);

const ID = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/u;
const HEX_64 = /^[0-9a-f]{64}$/u;
const PAIRING_CODE = /^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{7}$/u;
const MIME = /^[A-Za-z0-9!#$&^_.+-]+\/[A-Za-z0-9!#$&^_.+-]+$/u;
const B64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/u;

const MESSAGE_SCHEMAS = Object.freeze({
  controller_capabilities: schema(["v", "type", "request_id", "client_role", "sender_role", "protocol_version", "capabilities"], [], ["A", "B"]),
  desktop_endpoint_claim: schema(["v", "type", "request_id", "endpoint_id", "enrollment_proof_b64"], [], ["B"]),
  desktop_pairing_register: schema(["v", "type", "request_id", "endpoint_id", "remote_control_generation", "code_wrap_public_key_b64", "handshake_public_key_b64", "key_algorithm", "capabilities"], [], ["B"]),
  desktop_pairing_registered: schema(["v", "type", "request_id", "pairing_id", "remote_control_generation", "sealed_code_b64", "expires_at_ms", "relay_epoch"], [], ["relay"]),
  desktop_pairing_revoke: schema(["v", "type", "request_id", "endpoint_id", "remote_control_generation", "reason"], [], ["B"]),
  controller_pairing_claim: schema(["v", "type", "request_id", "claim_request_id", "pairing_code", "handshake_public_key_b64", "key_algorithm", "capabilities"], [], ["A"]),
  controller_pairing_reserved: schema(["v", "type", "request_id", "pairing_id", "reservation_id", "remote_control_generation", "relay_epoch", "transcript_hash", "peer_handshake_public_key_b64", "reservation_expires_at_ms", "handshake_expires_at_ms", "relay_signature_b64"], [], ["relay"]),
  finished_receipt_commit: schema(["v", "type", "request_id", "pairing_id", "reservation_id", "remote_control_generation", "relay_epoch", "transcript_hash", "sender_role", "commitment"], [], ["A", "B"]),
  encrypted_handshake_finished: schema(["v", "type", "request_id", "pairing_id", "reservation_id", "remote_control_generation", "relay_epoch", "transcript_hash", "sender_role", "ciphertext_b64", "ciphertext_bytes"], [], ["A", "B"]),
  finished_receipt_ack: schema(["v", "type", "request_id", "pairing_id", "reservation_id", "remote_control_generation", "relay_epoch", "transcript_hash", "sender_role", "receiver_role", "receipt_secret_b64"], [], ["A", "B"]),
  controller_joined: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "remote_control_generation", "relay_epoch", "liveness_epoch_a", "liveness_epoch_b", "resume_token_b64", "resume_deadline_ms"], [], ["relay"]),
  controller_liveness: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "sender_role", "liveness_epoch", "heartbeat_sequence"], [], ["A", "B"]),
  controller_liveness_challenge_a: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "challenge_id", "challenge_b64"], [], ["B"]),
  encrypted_controller_liveness_a: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "liveness_epoch_a", "heartbeat_sequence_a", "challenge_id", "ciphertext_b64", "ciphertext_bytes"], [], ["A"]),
  controller_liveness_attestation_request_a: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "challenge_id", "challenge_b64"], [], ["B"]),
  controller_liveness_attestation_a: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "liveness_epoch_a", "heartbeat_sequence_a", "attestation_sequence", "challenge_id", "observed_age_ms", "remaining_ttl_ms", "relay_signature_b64"], [], ["relay"]),
  controller_resume_prepare: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "remote_control_generation", "sender_role", "resume_token_b64", "transport_challenge_b64", "original_deadline_ms"], [], ["A", "B"]),
  controller_resume_provisional: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "activation_id", "original_deadline_ms"], [], ["relay"]),
  encrypted_resume_confirm: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "activation_id", "sender_role", "ciphertext_b64", "ciphertext_bytes"], [], ["A", "B"]),
  controller_resume_activation_ack: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "remote_control_generation", "activation_id", "original_deadline_ms"], [], ["B"]),
  controller_resume_activation_pending: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "activation_id", "original_deadline_ms"], [], ["relay"]),
  controller_post_activation_liveness: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "activation_id", "liveness_epoch_b", "heartbeat_sequence_b"], [], ["B"]),
  controller_post_activation_liveness_confirmed: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "activation_id", "liveness_epoch_b", "heartbeat_sequence_b"], [], ["relay"]),
  controller_resume_ready_ack: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "activation_id", "sender_role", "liveness_epoch_b", "heartbeat_sequence_b"], [], ["A", "B"]),
  controller_resumed: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "activation_id", "liveness_epoch_a", "liveness_epoch_b", "new_resume_token_b64"], [], ["relay"]),
  encrypted_frame: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "frame_sequence", "direction", "payload_kind", "ciphertext_b64", "ciphertext_bytes"], [], ["A", "B"]),
  controller_disconnect: schema(["v", "type", "request_id", "connection_id", "controller_lease_id", "lease_epoch", "sender_role", "reason"], [], ["A", "B"]),
  error: schema(["v", "type", "request_id", "code", "message"], [], ["relay"]),
});

const PAYLOAD_SCHEMAS = Object.freeze({
  task_submit: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "request_id", "task_id", "client_request_id", "session_id", "model_source", "mode", "input_digest", "attachment_ids"], [], ["A"]),
  task_cancel: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "request_id", "task_id", "session_id"], [], ["A"]),
  task_terminal: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "request_id", "task_id", "session_id", "task_status"], ["result_digest"], ["B"]),
  permission_request: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "task_id", "permission_request_id", "expires_at_ms", "operation_type", "risk_level", "target_scope", "preview"], [], ["B"]),
  permission_decision: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "task_id", "permission_request_id", "request_id", "decision"], [], ["A"]),
  attachment_begin: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "task_id", "session_id", "transfer_id", "file_name", "mime_type", "total_bytes", "sha256"], [], ["A"]),
  attachment_chunk: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "task_id", "session_id", "transfer_id", "chunk_index", "chunk_b64", "chunk_bytes"], [], ["A"]),
  attachment_commit: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "task_id", "session_id", "transfer_id", "sha256"], [], ["A"]),
  artifact_index_request: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "request_id", "session_id"], [], ["A"]),
  artifact_index: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "request_id", "session_id", "artifact_ids"], [], ["B"]),
  artifact_read_request: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "request_id", "session_id", "artifact_id", "offset", "max_bytes"], [], ["A"]),
  artifact_chunk: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "request_id", "session_id", "artifact_id", "offset", "chunk_b64", "chunk_bytes", "eof"], ["sha256"], ["B"]),
  session_snapshot_request: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "request_id", "session_id"], [], ["A"]),
  session_snapshot: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "request_id", "session_id", "journal_epoch", "base_sequence", "snapshot_b64", "snapshot_bytes", "sha256"], [], ["B"]),
  journal_replay_request: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "request_id", "session_id", "journal_epoch", "after_sequence"], [], ["A"]),
  journal_event: schema(["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "session_id", "journal_epoch", "event_sequence", "event_kind", "event_b64", "event_bytes"], [], ["B"]),
});

function schema(required, optional, roles) {
  return Object.freeze({ required: Object.freeze(required), optional: Object.freeze(optional), roles: Object.freeze(roles) });
}

export class ControllerProtocolError extends Error {
  constructor(code, detail) {
    super(`${code}: ${detail}`);
    this.name = "ControllerProtocolError";
    this.code = code;
  }
}

function fail(code, detail) {
  throw new ControllerProtocolError(code, detail);
}

function plainObject(value, label) {
  if (!value || typeof value !== "object" || Array.isArray(value) || Object.getPrototypeOf(value) !== Object.prototype) {
    fail("invalid_type", `${label} must be an object`);
  }
  return value;
}

function exactFields(value, definition, label) {
  const object = plainObject(value, label);
  const allowed = new Set([...definition.required, ...definition.optional]);
  for (const key of Object.keys(object)) {
    if (!allowed.has(key)) fail("unknown_field", `${label}.${safeFieldName(key)}`);
  }
  for (const key of definition.required) {
    if (!Object.hasOwn(object, key)) fail("missing_field", `${label}.${key}`);
  }
  return object;
}

function safeFieldName(value) {
  return /^[A-Za-z0-9_]{1,64}$/u.test(value) ? value : "unknown";
}

function text(value, field, max = MAX_ID_CHARS, min = 1) {
  if (typeof value !== "string") fail("invalid_field", field);
  const characters = [...value].length;
  if (characters < min || characters > max) fail("invalid_field", field);
  return value;
}

function identifier(value, field) {
  if (typeof value !== "string" || !ID.test(value)) fail("invalid_field", field);
}

function uint(value, field, max = Number.MAX_SAFE_INTEGER) {
  if (!Number.isSafeInteger(value) || value < 0 || value > max) fail("invalid_field", field);
}

function bool(value, field) {
  if (typeof value !== "boolean") fail("invalid_field", field);
}

function oneOf(value, allowed, field) {
  if (!allowed.includes(value)) fail("invalid_enum", field);
}

function hex64(value, field) {
  if (typeof value !== "string" || !HEX_64.test(value)) fail("invalid_field", field);
}

function base64(value, field, maxBytes, exactBytes) {
  if (typeof value !== "string" || value.length === 0 || !B64.test(value)) fail("invalid_field", field);
  const bytes = Buffer.from(value, "base64");
  if (bytes.toString("base64") !== value || bytes.length > maxBytes || (exactBytes !== undefined && bytes.length !== exactBytes)) {
    fail("invalid_field", field);
  }
  return bytes.length;
}

function stringList(value, field, maxItems = 64) {
  if (!Array.isArray(value) || value.length > maxItems) fail("invalid_field", field);
  const seen = new Set();
  for (const item of value) {
    identifier(item, field);
    if (seen.has(item)) fail("invalid_field", field);
    seen.add(item);
  }
}

function validateNamedField(object, field) {
  const value = object[field];
  if (value === undefined) return;
  if (field === "v" || field === "protocol_version") {
    if (value !== CONTROLLER_PROTOCOL_VERSION) fail("unsupported_version", field);
  } else if (field.endsWith("_id") || field.endsWith("_generation") || field === "relay_epoch" || field.startsWith("liveness_epoch") || field === "journal_epoch") {
    identifier(value, field);
  } else if (field.endsWith("_sequence") || field.startsWith("heartbeat_sequence") || field.endsWith("_bytes") || field.endsWith("_ms") || ["lease_epoch", "chunk_index", "offset", "max_bytes", "base_sequence", "after_sequence", "event_sequence"].includes(field)) {
    uint(value, field, field === "chunk_bytes" || field === "max_bytes" ? MAX_TRANSFER_CHUNK_BYTES : Number.MAX_SAFE_INTEGER);
  } else if (["transcript_hash", "commitment", "sha256", "input_digest", "result_digest"].includes(field)) {
    hex64(value, field);
  } else if (field === "pairing_code") {
    if (typeof value !== "string" || !PAIRING_CODE.test(value)) fail("invalid_field", field);
  } else if (field === "capabilities") {
    stringList(value, field, CONTROLLER_CAPABILITIES.length);
    for (const capability of value) oneOf(capability, CONTROLLER_CAPABILITIES, field);
  } else if (field === "attachment_ids" || field === "artifact_ids") {
    stringList(value, field);
  } else if (field === "sender_role" || field === "receiver_role") {
    oneOf(value, ["A", "B"], field);
  } else if (field === "client_role") {
    oneOf(value, ["desktop_controller", "desktop_controlled_endpoint"], field);
  } else if (field === "key_algorithm") {
    oneOf(value, ["x25519"], field);
  } else if (field === "direction") {
    oneOf(value, ["a_to_b", "b_to_a"], field);
  } else if (field === "payload_kind") {
    oneOf(value, CONTROLLER_PAYLOAD_TYPES, field);
  } else if (field === "model_source") {
    oneOf(value, ["controlled_device", "controller_temporary"], field);
  } else if (field === "mode") {
    oneOf(value, ["chat", "code"], field);
  } else if (field === "task_status") {
    oneOf(value, ["completed", "failed", "cancelled", "interrupted", "result_unknown"], field);
  } else if (field === "decision") {
    oneOf(value, ["allow", "deny"], field);
  } else if (field === "risk_level") {
    oneOf(value, ["low", "medium", "high", "critical"], field);
  } else if (field === "event_kind") {
    oneOf(value, ["user_message", "assistant_delta", "reasoning", "tool", "permission", "error", "cancel", "artifact", "terminal"], field);
  } else if (field === "code") {
    oneOf(value, CONTROLLER_ERROR_CODES, field);
  } else if (field === "eof") {
    bool(value, field);
  } else if (field === "mime_type") {
    if (typeof value !== "string" || value.length > 128 || !MIME.test(value)) fail("invalid_field", field);
  } else if (field === "file_name") {
    text(value, field, 255);
  } else if (["message", "preview", "target_scope"].includes(field)) {
    text(value, field, field === "message" ? 256 : 4096);
  } else if (field.endsWith("_public_key_b64")) {
    base64(value, field, 256, 32);
  } else if (field === "challenge_b64" || field === "transport_challenge_b64") {
    base64(value, field, 32, 16);
  } else if (field === "receipt_secret_b64") {
    base64(value, field, 32, 32);
  } else if (field === "chunk_b64") {
    const decoded = base64(value, field, MAX_TRANSFER_CHUNK_BYTES);
    if (object.chunk_bytes !== decoded) fail("length_mismatch", field);
  } else if (field === "snapshot_b64" || field === "event_b64") {
    const limit = MAX_DECRYPTED_PAYLOAD_BYTES;
    const decoded = base64(value, field, limit);
    const declared = field === "snapshot_b64" ? object.snapshot_bytes : object.event_bytes;
    if (declared !== decoded) fail("length_mismatch", field);
  } else if (field === "ciphertext_b64") {
    const decoded = base64(value, field, MAX_ENCRYPTED_FRAME_BYTES);
    if (object.ciphertext_bytes !== decoded) fail("length_mismatch", field);
  } else if (field.endsWith("_b64")) {
    base64(value, field, MAX_CONTROL_MESSAGE_BYTES);
  } else if (["type", "reason", "operation_type"].includes(field)) {
    text(value, field, 64);
  } else {
    identifier(value, field);
  }
}

function enforceRoleBindings(object, senderRole) {
  if (object.sender_role !== undefined && object.type !== "finished_receipt_ack" && object.sender_role !== senderRole) {
    fail("role_mismatch", "sender_role");
  }
  if (object.type === "controller_capabilities") {
    const expected = senderRole === "A" ? "desktop_controller" : "desktop_controlled_endpoint";
    if (object.client_role !== expected || object.sender_role !== senderRole) fail("role_mismatch", "client_role");
  }
  if (object.type === "finished_receipt_ack") {
    if (object.receiver_role !== senderRole || object.sender_role === senderRole) fail("role_mismatch", "receiver_role");
  }
  if (object.type === "encrypted_frame") {
    const expected = senderRole === "A" ? "a_to_b" : "b_to_a";
    if (object.direction !== expected) fail("role_mismatch", "direction");
  }
}

function parseJson(input, maxBytes) {
  if (typeof input === "string") {
    if (Buffer.byteLength(input, "utf8") > maxBytes) fail("message_too_large", "json");
    try {
      return JSON.parse(input);
    } catch {
      fail("invalid_json", "json");
    }
  }
  const encoded = JSON.stringify(input);
  if (encoded === undefined || Buffer.byteLength(encoded, "utf8") > maxBytes) fail("message_too_large", "json");
  return input;
}

function validateWithSchema(input, senderRole, schemas, maxBytes, label) {
  oneOf(senderRole, ["A", "B", "relay"], "transport_sender_role");
  const encodedBytes = typeof input === "string"
    ? Buffer.byteLength(input, "utf8")
    : Buffer.byteLength(JSON.stringify(input) ?? "", "utf8");
  const value = parseJson(input, maxBytes);
  const object = plainObject(value, label);
  if (typeof object.type !== "string" || !Object.hasOwn(schemas, object.type)) fail("unknown_message_type", `${label}.type`);
  const definition = schemas[object.type];
  if (!definition.roles.includes(senderRole)) fail("role_mismatch", object.type);
  exactFields(object, definition, label);
  for (const field of definition.required) validateNamedField(object, field);
  for (const field of definition.optional) validateNamedField(object, field);
  enforceRoleBindings(object, senderRole);
  return { value: Object.freeze({ ...object }), encodedBytes };
}

export function validateControllerMessage(input, senderRole) {
  const { value: validated, encodedBytes } = validateWithSchema(input, senderRole, MESSAGE_SCHEMAS, MAX_ENCRYPTED_WIRE_BYTES, "message");
  if (validated.type !== "encrypted_frame" && encodedBytes > MAX_CONTROL_MESSAGE_BYTES) {
    fail("message_too_large", "json");
  }
  return validated;
}

export function validateControllerPayload(input, senderRole) {
  return validateWithSchema(input, senderRole, PAYLOAD_SCHEMAS, MAX_DECRYPTED_PAYLOAD_BYTES, "payload").value;
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
