//! Frozen desktop-to-desktop controller protocol v1 model.
//!
//! DEV-00 deliberately keeps this module disconnected from the live Relay and
//! Tauri command paths. Later tasks may consume these validators after their
//! state machines and cryptography are implemented.

use std::collections::HashSet;
use std::fmt;

use base64::Engine as _;
use serde_json::{Map, Value};

pub(crate) const CONTROLLER_PROTOCOL_VERSION: u64 = 1;
pub(crate) const MAX_CONTROL_MESSAGE_BYTES: usize = 64 * 1024;
pub(crate) const MAX_ENCRYPTED_FRAME_BYTES: usize = 2 * 1024 * 1024;
pub(crate) const MAX_DECRYPTED_PAYLOAD_BYTES: usize = 512 * 1024;
pub(crate) const MAX_TRANSFER_CHUNK_BYTES: usize = 256 * 1024;
pub(crate) const MAX_TASK_INPUT_TEXT_BYTES: usize = 64 * 1024;
const MAX_TEMPORARY_MODEL_PACKAGE_BYTES: usize = 64 * 1024;
const MAX_ID_CHARS: usize = 128;
const MAX_ENCRYPTED_WIRE_BYTES: usize =
    MAX_ENCRYPTED_FRAME_BYTES.div_ceil(3) * 4 + MAX_CONTROL_MESSAGE_BYTES;

pub(crate) const CONTROLLER_CAPABILITIES: &[&str] = &[
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
];

pub(crate) const CONTROLLER_MESSAGE_TYPES: &[&str] = &[
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
];

pub(crate) const CONTROLLER_PAYLOAD_TYPES: &[&str] = &[
    "capability_snapshot_request",
    "capability_snapshot",
    "task_submit",
    "task_cancel",
    "task_status",
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
];

pub(crate) const TASK_STATUSES: &[&str] = &[
    "admitted",
    "running",
    "waiting_permission",
    "succeeded",
    "failed",
    "cancelled",
    "interrupted",
    "result_unknown",
];

const LEASE_CONTEXT_FIELDS: &[&str] = &[
    "connection_id",
    "controller_lease_id",
    "lease_epoch",
    "relay_epoch",
    "remote_control_generation",
];

pub(crate) const CONTROLLER_ERROR_CODES: &[&str] = &[
    "pairing_code_invalid",
    "pairing_code_generation_failed",
    "controller_busy",
    "version_incompatible",
    "endpoint_offline",
    "enrollment_required",
    "invalid_message",
    "unsupported_capability",
    "permission_already_decided",
    "permission_expired",
    "result_unknown",
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SenderRole {
    ControllerA,
    ControlledB,
    Relay,
}

impl SenderRole {
    fn wire(self) -> &'static str {
        match self {
            Self::ControllerA => "A",
            Self::ControlledB => "B",
            Self::Relay => "relay",
        }
    }
}

#[derive(Debug, Eq, PartialEq)]
pub(crate) struct ControllerProtocolError {
    pub(crate) code: &'static str,
    pub(crate) field: String,
}

impl ControllerProtocolError {
    fn new(code: &'static str, field: impl Into<String>) -> Self {
        Self {
            code,
            field: field.into(),
        }
    }
}

impl fmt::Display for ControllerProtocolError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{}: {}", self.code, self.field)
    }
}

impl std::error::Error for ControllerProtocolError {}

type Result<T> = std::result::Result<T, ControllerProtocolError>;

struct Schema {
    required: &'static [&'static str],
    optional: &'static [&'static str],
    roles: &'static [SenderRole],
}

const A: SenderRole = SenderRole::ControllerA;
const B: SenderRole = SenderRole::ControlledB;
const RELAY: SenderRole = SenderRole::Relay;

fn message_schema(kind: &str) -> Option<Schema> {
    let schema = match kind {
        "controller_capabilities" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "client_role",
                "sender_role",
                "protocol_version",
                "capabilities",
            ],
            optional: &[],
            roles: &[A, B],
        },
        "desktop_endpoint_claim" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "endpoint_id",
                "enrollment_proof_b64",
            ],
            optional: &[],
            roles: &[B],
        },
        "desktop_pairing_register" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "endpoint_id",
                "remote_control_generation",
                "code_wrap_public_key_b64",
                "handshake_public_key_b64",
                "key_algorithm",
                "capabilities",
            ],
            optional: &[],
            roles: &[B],
        },
        "desktop_pairing_registered" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "pairing_id",
                "remote_control_generation",
                "sealed_code_b64",
                "expires_at_ms",
                "relay_epoch",
            ],
            optional: &[],
            roles: &[RELAY],
        },
        "desktop_pairing_revoke" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "endpoint_id",
                "remote_control_generation",
                "reason",
            ],
            optional: &[],
            roles: &[B],
        },
        "controller_pairing_claim" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "claim_request_id",
                "pairing_code",
                "handshake_public_key_b64",
                "key_algorithm",
                "capabilities",
            ],
            optional: &[],
            roles: &[A],
        },
        "controller_pairing_reserved" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "pairing_id",
                "reservation_id",
                "remote_control_generation",
                "relay_epoch",
                "transcript",
                "transcript_hash",
                "relay_signing_key_id",
                "signature_algorithm",
                "reservation_expires_at_ms",
                "handshake_expires_at_ms",
                "relay_signature_b64",
            ],
            optional: &[],
            roles: &[RELAY],
        },
        "finished_receipt_commit" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "pairing_id",
                "reservation_id",
                "remote_control_generation",
                "relay_epoch",
                "transcript_hash",
                "sender_role",
                "commitment",
            ],
            optional: &[],
            roles: &[A, B],
        },
        "encrypted_handshake_finished" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "pairing_id",
                "reservation_id",
                "remote_control_generation",
                "relay_epoch",
                "transcript_hash",
                "sender_role",
                "ciphertext_b64",
                "ciphertext_bytes",
            ],
            optional: &[],
            roles: &[A, B],
        },
        "finished_receipt_ack" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "pairing_id",
                "reservation_id",
                "remote_control_generation",
                "relay_epoch",
                "transcript_hash",
                "sender_role",
                "receiver_role",
                "receipt_secret_b64",
            ],
            optional: &[],
            roles: &[A, B],
        },
        "controller_joined" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "remote_control_generation",
                "relay_epoch",
                "liveness_epoch_a",
                "liveness_epoch_b",
                "resume_token_b64",
                "resume_deadline_ms",
            ],
            optional: &[],
            roles: &[RELAY],
        },
        "controller_liveness" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "sender_role",
                "liveness_epoch",
                "heartbeat_sequence",
            ],
            optional: &[],
            roles: &[A, B],
        },
        "controller_liveness_challenge_a" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "challenge_id",
                "challenge_b64",
            ],
            optional: &[],
            roles: &[B],
        },
        "encrypted_controller_liveness_a" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "ciphertext_b64",
                "ciphertext_bytes",
            ],
            optional: &[],
            roles: &[A],
        },
        "controller_liveness_attestation_request_a" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "challenge_id",
                "challenge_b64",
            ],
            optional: &[],
            roles: &[B],
        },
        "controller_liveness_attestation_a" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "relay_epoch",
                "remote_control_generation",
                "liveness_epoch_a",
                "heartbeat_sequence_a",
                "attestation_sequence",
                "challenge_id",
                "challenge_b64",
                "observed_age_ms",
                "remaining_ttl_ms",
                "issued_at_relay",
                "relay_signing_key_id",
                "signature_algorithm",
                "relay_signature_b64",
            ],
            optional: &[],
            roles: &[RELAY],
        },
        "controller_resume_prepare" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "remote_control_generation",
                "sender_role",
                "resume_token_b64",
                "transport_challenge_b64",
                "original_deadline_ms",
            ],
            optional: &[],
            roles: &[A, B],
        },
        "controller_resume_provisional" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "activation_id",
                "original_deadline_ms",
            ],
            optional: &[],
            roles: &[RELAY],
        },
        "encrypted_resume_confirm" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "activation_id",
                "sender_role",
                "ciphertext_b64",
                "ciphertext_bytes",
            ],
            optional: &[],
            roles: &[A, B],
        },
        "controller_resume_activation_ack" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "remote_control_generation",
                "activation_id",
                "original_deadline_ms",
            ],
            optional: &[],
            roles: &[B],
        },
        "controller_resume_activation_pending" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "activation_id",
                "original_deadline_ms",
            ],
            optional: &[],
            roles: &[RELAY],
        },
        "controller_post_activation_liveness" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "activation_id",
                "liveness_epoch_b",
                "heartbeat_sequence_b",
            ],
            optional: &[],
            roles: &[B],
        },
        "controller_post_activation_liveness_confirmed" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "activation_id",
                "liveness_epoch_b",
                "heartbeat_sequence_b",
            ],
            optional: &[],
            roles: &[RELAY],
        },
        "controller_resume_ready_ack" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "activation_id",
                "sender_role",
                "liveness_epoch_b",
                "heartbeat_sequence_b",
            ],
            optional: &[],
            roles: &[A, B],
        },
        "controller_resumed" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "activation_id",
                "liveness_epoch_a",
                "liveness_epoch_b",
                "new_resume_token_b64",
            ],
            optional: &[],
            roles: &[RELAY],
        },
        "encrypted_frame" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "frame_sequence",
                "direction",
                "payload_kind",
                "ciphertext_b64",
                "ciphertext_bytes",
            ],
            optional: &[],
            roles: &[A, B],
        },
        "controller_disconnect" => Schema {
            required: &[
                "v",
                "type",
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "sender_role",
                "reason",
            ],
            optional: &[],
            roles: &[A, B],
        },
        "error" => Schema {
            required: &["v", "type", "request_id", "code", "message"],
            optional: LEASE_CONTEXT_FIELDS,
            roles: &[RELAY],
        },
        _ => return None,
    };
    Some(schema)
}

fn payload_schema(kind: &str) -> Option<Schema> {
    let schema = match kind {
        "capability_snapshot_request" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "known_revision",
            ],
            optional: &[],
            roles: &[A],
        },
        "capability_snapshot" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "capability_revision",
                "expires_at_ms",
                "modes",
                "tools",
                "skills",
                "workspaces",
            ],
            optional: &[],
            roles: &[B],
        },
        "task_submit" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "task_id",
                "client_request_id",
                "session_id",
                "capability_revision",
                "model_source",
                "mode",
                "input_digest",
                "input",
            ],
            optional: &["temporary_model_package"],
            roles: &[A],
        },
        "task_cancel" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "task_id",
                "session_id",
            ],
            optional: &[],
            roles: &[A],
        },
        "task_status" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "task_id",
                "session_id",
                "task_status",
            ],
            optional: &["result_digest"],
            roles: &[B],
        },
        "permission_request" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "task_id",
                "permission_request_id",
                "expires_at_ms",
                "operation_type",
                "risk_level",
                "target_scope",
                "preview",
            ],
            optional: &[],
            roles: &[B],
        },
        "permission_decision" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "task_id",
                "permission_request_id",
                "request_id",
                "decision",
            ],
            optional: &[],
            roles: &[A],
        },
        "attachment_begin" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "task_id",
                "session_id",
                "transfer_id",
                "file_name",
                "mime_type",
                "total_bytes",
                "sha256",
            ],
            optional: &[],
            roles: &[A],
        },
        "attachment_chunk" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "task_id",
                "session_id",
                "transfer_id",
                "chunk_index",
                "chunk_b64",
                "chunk_bytes",
            ],
            optional: &[],
            roles: &[A],
        },
        "attachment_commit" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "task_id",
                "session_id",
                "transfer_id",
                "sha256",
            ],
            optional: &[],
            roles: &[A],
        },
        "artifact_index_request" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "session_id",
            ],
            optional: &[],
            roles: &[A],
        },
        "artifact_index" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "session_id",
                "artifacts",
            ],
            optional: &[],
            roles: &[B],
        },
        "artifact_read_request" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "session_id",
                "artifact_id",
                "offset",
                "max_bytes",
            ],
            optional: &[],
            roles: &[A],
        },
        "artifact_chunk" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "session_id",
                "artifact_id",
                "offset",
                "chunk_b64",
                "chunk_bytes",
                "eof",
            ],
            optional: &["sha256"],
            roles: &[B],
        },
        "session_snapshot_request" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "session_id",
            ],
            optional: &[],
            roles: &[A],
        },
        "session_snapshot" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "session_id",
                "journal_epoch",
                "base_sequence",
                "snapshot_b64",
                "snapshot_bytes",
                "sha256",
            ],
            optional: &[],
            roles: &[B],
        },
        "journal_replay_request" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "request_id",
                "session_id",
                "journal_epoch",
                "after_sequence",
            ],
            optional: &[],
            roles: &[A],
        },
        "journal_event" => Schema {
            required: &[
                "v",
                "type",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "session_id",
                "journal_epoch",
                "event_sequence",
                "event_kind",
                "event_b64",
                "event_bytes",
            ],
            optional: &[],
            roles: &[B],
        },
        _ => return None,
    };
    Some(schema)
}

pub(crate) fn validate_controller_message_json(
    input: &str,
    sender_role: SenderRole,
) -> Result<Value> {
    validate_controller_message_json_in_context(input, sender_role, None)
}

pub(crate) fn validate_controller_message_json_in_context(
    input: &str,
    sender_role: SenderRole,
    expected_context: Option<&LeaseContext>,
) -> Result<Value> {
    if input.len() > MAX_ENCRYPTED_WIRE_BYTES {
        return Err(ControllerProtocolError::new("message_too_large", "json"));
    }
    let value: Value = serde_json::from_str(input)
        .map_err(|_| ControllerProtocolError::new("invalid_json", "json"))?;
    let object = as_object(&value, "message")?;
    let kind = message_kind(object, "message")?;
    let schema = message_schema(kind)
        .ok_or_else(|| ControllerProtocolError::new("unknown_message_type", "message.type"))?;
    if kind != "encrypted_frame" && input.len() > MAX_CONTROL_MESSAGE_BYTES {
        return Err(ControllerProtocolError::new("message_too_large", "json"));
    }
    validate_object(object, kind, &schema, sender_role, "message")?;
    if kind == "controller_pairing_reserved" {
        validate_reserved_transcript(object)?;
    }
    if kind == "controller_liveness_attestation_a" {
        validate_uint(
            object
                .get("observed_age_ms")
                .expect("attestation age required"),
            "observed_age_ms",
            30_000,
        )?;
        validate_uint(
            object
                .get("remaining_ttl_ms")
                .expect("attestation TTL required"),
            "remaining_ttl_ms",
            30_000,
        )?;
    }
    enforce_expected_context(object, expected_context)?;
    Ok(value)
}

pub(crate) fn validate_controller_payload_json(
    input: &str,
    sender_role: SenderRole,
) -> Result<Value> {
    validate_controller_payload_json_in_context(input, sender_role, None)
}

pub(crate) fn validate_controller_payload_json_in_context(
    input: &str,
    sender_role: SenderRole,
    expected_context: Option<&LeaseContext>,
) -> Result<Value> {
    if input.len() > MAX_DECRYPTED_PAYLOAD_BYTES {
        return Err(ControllerProtocolError::new("message_too_large", "json"));
    }
    let value: Value = serde_json::from_str(input)
        .map_err(|_| ControllerProtocolError::new("invalid_json", "json"))?;
    let object = as_object(&value, "payload")?;
    let kind = message_kind(object, "payload")?;
    let schema = payload_schema(kind)
        .ok_or_else(|| ControllerProtocolError::new("unknown_message_type", "payload.type"))?;
    validate_object(object, kind, &schema, sender_role, "payload")?;
    if kind == "task_submit" {
        validate_task_submit(object)?;
    }
    if kind == "capability_snapshot" {
        validate_capability_snapshot(object)?;
    }
    if kind == "artifact_index" {
        validate_artifact_index(object)?;
    }
    enforce_expected_context(object, expected_context)?;
    Ok(value)
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct LeaseContext {
    pub(crate) connection_id: String,
    pub(crate) controller_lease_id: String,
    pub(crate) lease_epoch: String,
    pub(crate) relay_epoch: String,
    pub(crate) remote_control_generation: String,
}

fn requires_full_lease_context(kind: &str, label: &str) -> bool {
    if label == "payload" {
        return true;
    }
    matches!(
        kind,
        "controller_joined"
            | "controller_liveness"
            | "controller_liveness_challenge_a"
            | "controller_liveness_attestation_request_a"
            | "controller_liveness_attestation_a"
            | "controller_resume_prepare"
            | "controller_resume_provisional"
            | "encrypted_resume_confirm"
            | "controller_resume_activation_ack"
            | "controller_resume_activation_pending"
            | "controller_post_activation_liveness"
            | "controller_post_activation_liveness_confirmed"
            | "controller_resume_ready_ack"
            | "controller_resumed"
            | "encrypted_frame"
            | "controller_disconnect"
    )
}

fn as_object<'a>(value: &'a Value, label: &str) -> Result<&'a Map<String, Value>> {
    value
        .as_object()
        .ok_or_else(|| ControllerProtocolError::new("invalid_type", label))
}

fn message_kind<'a>(object: &'a Map<String, Value>, label: &str) -> Result<&'a str> {
    object.get("type").and_then(Value::as_str).ok_or_else(|| {
        ControllerProtocolError::new("unknown_message_type", format!("{label}.type"))
    })
}

fn validate_object(
    object: &Map<String, Value>,
    kind: &str,
    schema: &Schema,
    sender_role: SenderRole,
    label: &str,
) -> Result<()> {
    if !schema.roles.contains(&sender_role) {
        return Err(ControllerProtocolError::new("role_mismatch", kind));
    }
    let full_lease_context = requires_full_lease_context(kind, label);
    for field in object.keys() {
        if !schema.required.contains(&field.as_str())
            && !schema.optional.contains(&field.as_str())
            && !(full_lease_context && LEASE_CONTEXT_FIELDS.contains(&field.as_str()))
        {
            return Err(ControllerProtocolError::new(
                "unknown_field",
                format!("{label}.{}", safe_field_name(field)),
            ));
        }
    }
    for field in schema.required {
        if !object.contains_key(*field) {
            return Err(ControllerProtocolError::new(
                "missing_field",
                format!("{label}.{field}"),
            ));
        }
    }
    if full_lease_context {
        for field in LEASE_CONTEXT_FIELDS {
            if !object.contains_key(*field) {
                return Err(ControllerProtocolError::new(
                    "missing_field",
                    format!("{label}.{field}"),
                ));
            }
        }
    }
    for field in schema.required.iter().chain(schema.optional.iter()) {
        if let Some(value) = object.get(*field) {
            validate_named_field(object, field, value)?;
        }
    }
    if full_lease_context {
        for field in LEASE_CONTEXT_FIELDS {
            validate_named_field(
                object,
                field,
                object.get(*field).expect("lease context presence checked"),
            )?;
        }
    }
    enforce_role_bindings(object, kind, sender_role)
}

fn safe_field_name(value: &str) -> &str {
    if !value.is_empty()
        && value.len() <= 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
    {
        value
    } else {
        "unknown"
    }
}

fn validate_named_field(object: &Map<String, Value>, field: &str, value: &Value) -> Result<()> {
    if field == "v" || field == "protocol_version" {
        if value.as_u64() != Some(CONTROLLER_PROTOCOL_VERSION) {
            return Err(ControllerProtocolError::new("unsupported_version", field));
        }
    } else if field.ends_with("_id")
        || field.ends_with("_generation")
        || field == "relay_epoch"
        || field.starts_with("liveness_epoch")
        || field == "journal_epoch"
        || field == "controller_connection_identity"
        || field == "relay_signing_key_id"
    {
        validate_identifier(value, field)?;
    } else if field.ends_with("_sequence")
        || field.starts_with("heartbeat_sequence")
        || field.ends_with("_bytes")
        || field.ends_with("_ms")
        || [
            "lease_epoch",
            "chunk_index",
            "offset",
            "max_bytes",
            "base_sequence",
            "after_sequence",
            "event_sequence",
            "issued_at_relay",
            "capability_revision",
            "known_revision",
        ]
        .contains(&field)
    {
        let limit = if field == "chunk_bytes" || field == "max_bytes" {
            MAX_TRANSFER_CHUNK_BYTES as u64
        } else {
            u64::MAX
        };
        validate_uint(value, field, limit)?;
    } else if [
        "transcript_hash",
        "capability_hash",
        "commitment",
        "sha256",
        "input_digest",
        "result_digest",
    ]
    .contains(&field)
    {
        validate_hex_64(value, field)?;
    } else if field == "pairing_code" {
        let text = value.as_str().unwrap_or_default();
        if text.len() != 7
            || !text
                .bytes()
                .all(|byte| b"23456789ABCDEFGHJKMNPQRSTUVWXYZ".contains(&byte))
        {
            return Err(ControllerProtocolError::new("invalid_field", field));
        }
    } else if field == "capabilities" {
        if value.as_array().is_some_and(|items| {
            items.iter().any(|item| {
                item.as_str()
                    .is_some_and(|capability| !CONTROLLER_CAPABILITIES.contains(&capability))
            })
        }) {
            return Err(ControllerProtocolError::new(
                "unsupported_capability",
                field,
            ));
        }
        validate_string_list(value, field, CONTROLLER_CAPABILITIES)?;
    } else if field == "attachment_ids" || field == "artifact_ids" || field == "skill_ids" {
        if value.as_array().is_some_and(|items| items.len() > 32) {
            return Err(ControllerProtocolError::new("invalid_field", field));
        }
        validate_string_list(value, field, &[])?;
    } else if field == "sender_role"
        || field == "receiver_role"
        || field == "role_a"
        || field == "role_b"
    {
        validate_enum(value, field, &["A", "B"])?;
    } else if field == "client_role" {
        validate_enum(
            value,
            field,
            &["desktop_controller", "desktop_controlled_endpoint"],
        )?;
    } else if field == "key_algorithm" {
        validate_enum(value, field, &["x25519"])?;
    } else if field == "signature_algorithm" {
        validate_enum(value, field, &["ed25519"])?;
    } else if field == "direction" {
        validate_enum(value, field, &["a_to_b", "b_to_a"])?;
    } else if field == "payload_kind" {
        validate_enum(value, field, CONTROLLER_PAYLOAD_TYPES)?;
    } else if field == "model_source" {
        validate_enum(value, field, &["controlled_device", "controller_temporary"])?;
    } else if field == "mode" {
        validate_enum(value, field, &["chat", "code"])?;
    } else if field == "task_status" {
        validate_enum(value, field, TASK_STATUSES)?;
    } else if field == "decision" {
        validate_enum(value, field, &["allow", "deny"])?;
    } else if field == "risk_level" {
        validate_enum(value, field, &["low", "medium", "high", "critical"])?;
    } else if field == "event_kind" {
        validate_enum(
            value,
            field,
            &[
                "user_message",
                "assistant_delta",
                "reasoning",
                "tool",
                "permission",
                "error",
                "cancel",
                "artifact",
                "terminal",
            ],
        )?;
    } else if field == "code" {
        validate_enum(value, field, CONTROLLER_ERROR_CODES)?;
    } else if field == "eof" {
        if !value.is_boolean() {
            return Err(ControllerProtocolError::new("invalid_field", field));
        }
    } else if field == "mime_type" {
        let text = bounded_text(value, field, 128)?;
        let mut parts = text.split('/');
        let media_type = parts.next().unwrap_or_default();
        let media_subtype = parts.next().unwrap_or_default();
        let valid_mime_part = |part: &str| {
            !part.is_empty()
                && part
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || b"!#$&^_.+-".contains(&byte))
        };
        if parts.next().is_some() || !valid_mime_part(media_type) || !valid_mime_part(media_subtype)
        {
            return Err(ControllerProtocolError::new("invalid_field", field));
        }
    } else if field == "file_name" {
        bounded_text(value, field, 255)?;
    } else if ["message", "preview", "target_scope"].contains(&field) {
        bounded_text(value, field, if field == "message" { 256 } else { 4096 })?;
    } else if field.ends_with("_public_key_b64") {
        validate_base64(value, field, 256, Some(32))?;
    } else if field == "challenge_b64" || field == "transport_challenge_b64" || field == "nonce_b64"
    {
        validate_base64(value, field, 32, Some(16))?;
    } else if field == "receipt_secret_b64" {
        validate_base64(value, field, 32, Some(32))?;
    } else if field == "relay_signature_b64" {
        validate_base64(value, field, 64, Some(64))?;
    } else if field == "chunk_b64" {
        let length = validate_base64(value, field, MAX_TRANSFER_CHUNK_BYTES, None)?;
        validate_declared_length(object, "chunk_bytes", length, field)?;
    } else if field == "snapshot_b64" || field == "event_b64" {
        let length = validate_base64(value, field, MAX_DECRYPTED_PAYLOAD_BYTES, None)?;
        let declared = if field == "snapshot_b64" {
            "snapshot_bytes"
        } else {
            "event_bytes"
        };
        validate_declared_length(object, declared, length, field)?;
    } else if field == "ciphertext_b64" {
        let length = validate_base64(value, field, MAX_ENCRYPTED_FRAME_BYTES, None)?;
        validate_declared_length(object, "ciphertext_bytes", length, field)?;
    } else if field.ends_with("_b64") {
        validate_base64(value, field, MAX_CONTROL_MESSAGE_BYTES, None)?;
    } else if ["type", "reason", "operation_type"].contains(&field) {
        bounded_text(value, field, 64)?;
    } else if [
        "input",
        "temporary_model_package",
        "transcript",
        "modes",
        "tools",
        "skills",
        "workspaces",
        "artifacts",
    ]
    .contains(&field)
    {
        // Strict nested schemas are validated by their owning message/payload validator.
    } else {
        validate_identifier(value, field)?;
    }
    Ok(())
}

fn validate_identifier(value: &Value, field: &str) -> Result<()> {
    let Some(text) = value.as_str() else {
        return Err(ControllerProtocolError::new("invalid_field", field));
    };
    let valid = !text.is_empty()
        && text.len() <= MAX_ID_CHARS
        && text.as_bytes()[0].is_ascii_alphanumeric()
        && text
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"._:-".contains(&byte));
    if !valid {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    Ok(())
}

fn validate_uint(value: &Value, field: &str, max: u64) -> Result<()> {
    let Some(text) = value.as_str() else {
        return Err(ControllerProtocolError::new("invalid_field", field));
    };
    if text.is_empty()
        || (text.len() > 1 && text.starts_with('0'))
        || !text.bytes().all(|byte| byte.is_ascii_digit())
        || text.parse::<u64>().map_or(true, |number| number > max)
    {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    Ok(())
}

fn validate_hex_64(value: &Value, field: &str) -> Result<()> {
    let text = value.as_str().unwrap_or_default();
    if text.len() != 64
        || !text
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    Ok(())
}

fn validate_enum(value: &Value, field: &str, allowed: &[&str]) -> Result<()> {
    if value.as_str().is_none_or(|text| !allowed.contains(&text)) {
        return Err(ControllerProtocolError::new("invalid_enum", field));
    }
    Ok(())
}

fn validate_string_list(value: &Value, field: &str, allowed: &[&str]) -> Result<()> {
    let Some(values) = value.as_array() else {
        return Err(ControllerProtocolError::new("invalid_field", field));
    };
    if values.len() > 64 {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    let mut seen = HashSet::new();
    for value in values {
        validate_identifier(value, field)?;
        let text = value
            .as_str()
            .expect("identifier validation guarantees a string");
        if !seen.insert(text) {
            return Err(ControllerProtocolError::new("invalid_field", field));
        }
        if !allowed.is_empty() && !allowed.contains(&text) {
            return Err(ControllerProtocolError::new("invalid_enum", field));
        }
    }
    Ok(())
}

fn bounded_text<'a>(value: &'a Value, field: &str, max: usize) -> Result<&'a str> {
    let Some(text) = value.as_str() else {
        return Err(ControllerProtocolError::new("invalid_field", field));
    };
    if text.is_empty() || text.chars().count() > max {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    Ok(text)
}

fn validate_base64(value: &Value, field: &str, max: usize, exact: Option<usize>) -> Result<usize> {
    let Some(encoded) = value.as_str() else {
        return Err(ControllerProtocolError::new("invalid_field", field));
    };
    if encoded.is_empty() {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    let decoded = base64::engine::general_purpose::STANDARD
        .decode(encoded)
        .map_err(|_| ControllerProtocolError::new("invalid_field", field))?;
    let canonical = base64::engine::general_purpose::STANDARD.encode(&decoded);
    if canonical != encoded
        || decoded.len() > max
        || exact.is_some_and(|length| decoded.len() != length)
    {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    Ok(decoded.len())
}

fn validate_declared_length(
    object: &Map<String, Value>,
    declared_field: &str,
    actual: usize,
    source_field: &str,
) -> Result<()> {
    if object.get(declared_field).and_then(Value::as_str) != Some(actual.to_string().as_str()) {
        return Err(ControllerProtocolError::new(
            "length_mismatch",
            source_field,
        ));
    }
    Ok(())
}

fn enforce_role_bindings(
    object: &Map<String, Value>,
    kind: &str,
    sender_role: SenderRole,
) -> Result<()> {
    if kind != "finished_receipt_ack" {
        if let Some(declared) = object.get("sender_role").and_then(Value::as_str) {
            if declared != sender_role.wire() {
                return Err(ControllerProtocolError::new("role_mismatch", "sender_role"));
            }
        }
    }
    if kind == "controller_capabilities" {
        let expected = match sender_role {
            SenderRole::ControllerA => "desktop_controller",
            SenderRole::ControlledB => "desktop_controlled_endpoint",
            SenderRole::Relay => unreachable!("schema excludes relay"),
        };
        if object.get("client_role").and_then(Value::as_str) != Some(expected) {
            return Err(ControllerProtocolError::new("role_mismatch", "client_role"));
        }
    }
    if kind == "finished_receipt_ack" {
        let receiver = object.get("receiver_role").and_then(Value::as_str);
        let sender = object.get("sender_role").and_then(Value::as_str);
        if receiver != Some(sender_role.wire()) || sender == Some(sender_role.wire()) {
            return Err(ControllerProtocolError::new(
                "role_mismatch",
                "receiver_role",
            ));
        }
    }
    if kind == "encrypted_frame" {
        let expected = if sender_role == A { "a_to_b" } else { "b_to_a" };
        if object.get("direction").and_then(Value::as_str) != Some(expected) {
            return Err(ControllerProtocolError::new("role_mismatch", "direction"));
        }
        let payload_kind = object
            .get("payload_kind")
            .and_then(Value::as_str)
            .expect("payload kind validated");
        let payload = payload_schema(payload_kind).expect("payload kind enum validated");
        if !payload.roles.contains(&sender_role) {
            return Err(ControllerProtocolError::new(
                "role_mismatch",
                "payload_kind",
            ));
        }
    }
    Ok(())
}

fn exact_nested<'a>(
    value: &'a Value,
    required: &[&str],
    optional: &[&str],
    label: &str,
) -> Result<&'a Map<String, Value>> {
    let object = as_object(value, label)?;
    for field in object.keys() {
        if !required.contains(&field.as_str()) && !optional.contains(&field.as_str()) {
            return Err(ControllerProtocolError::new(
                "unknown_field",
                format!("{label}.{}", safe_field_name(field)),
            ));
        }
    }
    for field in required {
        if !object.contains_key(*field) {
            return Err(ControllerProtocolError::new(
                "missing_field",
                format!("{label}.{field}"),
            ));
        }
    }
    Ok(object)
}

fn object_string<'a>(object: &'a Map<String, Value>, field: &str, label: &str) -> Result<&'a str> {
    object
        .get(field)
        .and_then(Value::as_str)
        .ok_or_else(|| ControllerProtocolError::new("invalid_field", format!("{label}.{field}")))
}

fn validate_task_submit(task: &Map<String, Value>) -> Result<()> {
    let input = exact_nested(
        task.get("input").expect("task input required by schema"),
        &["text", "attachment_ids", "skill_ids"],
        &["workspace_id"],
        "payload.input",
    )?;
    let text = object_string(input, "text", "payload.input")?;
    if text.len() > MAX_TASK_INPUT_TEXT_BYTES {
        return Err(ControllerProtocolError::new("invalid_field", "input.text"));
    }
    validate_string_list(
        input
            .get("attachment_ids")
            .expect("attachment IDs required by nested schema"),
        "input.attachment_ids",
        &[],
    )?;
    if input
        .get("attachment_ids")
        .and_then(Value::as_array)
        .is_some_and(|items| items.len() > 32)
    {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            "input.attachment_ids",
        ));
    }
    validate_string_list(
        input
            .get("skill_ids")
            .expect("skill IDs required by nested schema"),
        "input.skill_ids",
        &[],
    )?;
    if input
        .get("skill_ids")
        .and_then(Value::as_array)
        .is_some_and(|items| items.len() > 32)
    {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            "input.skill_ids",
        ));
    }
    if let Some(workspace_id) = input.get("workspace_id") {
        validate_identifier(workspace_id, "input.workspace_id")?;
    }
    if text.is_empty()
        && input
            .get("attachment_ids")
            .and_then(Value::as_array)
            .is_none_or(Vec::is_empty)
    {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            "payload.input",
        ));
    }
    let model_source = object_string(task, "model_source", "payload")?;
    match (model_source, task.get("temporary_model_package")) {
        ("controller_temporary", Some(package)) => validate_temporary_model_package(package, task),
        ("controller_temporary", None) => Err(ControllerProtocolError::new(
            "missing_field",
            "payload.temporary_model_package",
        )),
        ("controlled_device", None) => Ok(()),
        ("controlled_device", Some(_)) => Err(ControllerProtocolError::new(
            "invalid_field",
            "payload.temporary_model_package",
        )),
        _ => unreachable!("model source validated by outer schema"),
    }
}

fn validate_capability_snapshot(payload: &Map<String, Value>) -> Result<()> {
    validate_uint(
        payload
            .get("capability_revision")
            .expect("capability revision required"),
        "capability_revision",
        u64::MAX,
    )?;
    let modes = payload
        .get("modes")
        .and_then(Value::as_array)
        .ok_or_else(|| ControllerProtocolError::new("invalid_field", "modes"))?;
    if modes.is_empty() || modes.len() > 2 {
        return Err(ControllerProtocolError::new("invalid_field", "modes"));
    }
    let mut seen = HashSet::new();
    for mode in modes {
        validate_enum(mode, "modes", &["chat", "code"])?;
        if !seen.insert(mode.as_str().expect("mode enum validated")) {
            return Err(ControllerProtocolError::new("invalid_field", "modes"));
        }
    }
    validate_descriptor_list(
        payload.get("tools").expect("tools required"),
        "tools",
        "tool_id",
        false,
    )?;
    validate_descriptor_list(
        payload.get("skills").expect("skills required"),
        "skills",
        "skill_id",
        false,
    )?;
    validate_descriptor_list(
        payload.get("workspaces").expect("workspaces required"),
        "workspaces",
        "workspace_id",
        true,
    )
}

fn validate_descriptor_list(
    value: &Value,
    field: &str,
    id_field: &str,
    workspace: bool,
) -> Result<()> {
    let items = value
        .as_array()
        .ok_or_else(|| ControllerProtocolError::new("invalid_field", field))?;
    if items.len() > 256 {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    let mut ids = HashSet::new();
    for (index, item) in items.iter().enumerate() {
        let label = format!("{field}[{index}]");
        let required = if workspace {
            vec![id_field, "display_name", "code_allowed"]
        } else {
            vec![id_field, "display_name"]
        };
        let descriptor = exact_nested(item, &required, &[], &label)?;
        validate_identifier(
            descriptor.get(id_field).expect("descriptor ID required"),
            &format!("{label}.{id_field}"),
        )?;
        let id = object_string(descriptor, id_field, &label)?;
        if !ids.insert(id) {
            return Err(ControllerProtocolError::new("invalid_field", field));
        }
        bounded_text(
            descriptor
                .get("display_name")
                .expect("descriptor display name required"),
            &format!("{label}.display_name"),
            128,
        )?;
        if workspace
            && descriptor
                .get("code_allowed")
                .is_none_or(|value| !value.is_boolean())
        {
            return Err(ControllerProtocolError::new(
                "invalid_field",
                format!("{label}.code_allowed"),
            ));
        }
    }
    Ok(())
}

fn validate_artifact_index(payload: &Map<String, Value>) -> Result<()> {
    let artifacts = payload
        .get("artifacts")
        .and_then(Value::as_array)
        .ok_or_else(|| ControllerProtocolError::new("invalid_field", "artifacts"))?;
    if artifacts.len() > 256 {
        return Err(ControllerProtocolError::new("invalid_field", "artifacts"));
    }
    let mut ids = HashSet::new();
    for (index, item) in artifacts.iter().enumerate() {
        let label = format!("artifacts[{index}]");
        let artifact = exact_nested(
            item,
            &[
                "artifact_id",
                "task_id",
                "display_name",
                "mime_type",
                "size_bytes",
                "sha256",
                "preview_mode",
            ],
            &[],
            &label,
        )?;
        for field in ["artifact_id", "task_id"] {
            validate_identifier(
                artifact.get(field).expect("artifact ID field required"),
                &format!("{label}.{field}"),
            )?;
        }
        let artifact_id = object_string(artifact, "artifact_id", &label)?;
        if !ids.insert(artifact_id) {
            return Err(ControllerProtocolError::new("invalid_field", "artifacts"));
        }
        bounded_text(
            artifact
                .get("display_name")
                .expect("artifact display name required"),
            &format!("{label}.display_name"),
            255,
        )?;
        validate_named_field(
            artifact,
            "mime_type",
            artifact.get("mime_type").expect("artifact MIME required"),
        )?;
        validate_uint(
            artifact.get("size_bytes").expect("artifact size required"),
            &format!("{label}.size_bytes"),
            u64::MAX,
        )?;
        validate_hex_64(
            artifact.get("sha256").expect("artifact digest required"),
            &format!("{label}.sha256"),
        )?;
        validate_enum(
            artifact
                .get("preview_mode")
                .expect("artifact preview mode required"),
            &format!("{label}.preview_mode"),
            &["inline_text", "inline_image", "download_only"],
        )?;
    }
    Ok(())
}

fn validate_temporary_model_package(package: &Value, task: &Map<String, Value>) -> Result<()> {
    if serde_json::to_vec(package)
        .map_err(|_| ControllerProtocolError::new("invalid_field", "temporary_model_package"))?
        .len()
        > MAX_TEMPORARY_MODEL_PACKAGE_BYTES
    {
        return Err(ControllerProtocolError::new(
            "message_too_large",
            "temporary_model_package",
        ));
    }
    let package = exact_nested(
        package,
        &[
            "schema",
            "package_id",
            "binding",
            "primary",
            "primary_credential",
        ],
        &["vision_fallback"],
        "temporary_model_package",
    )?;
    if object_string(package, "schema", "temporary_model_package")? != "temporary_model_package/v1"
    {
        return Err(ControllerProtocolError::new(
            "unsupported_version",
            "temporary_model_package.schema",
        ));
    }
    validate_identifier(
        package.get("package_id").expect("package ID required"),
        "temporary_model_package.package_id",
    )?;
    let binding = exact_nested(
        package.get("binding").expect("binding required"),
        &[
            "connection_id",
            "controller_lease_id",
            "lease_epoch",
            "task_id",
        ],
        &[],
        "temporary_model_package.binding",
    )?;
    for field in ["connection_id", "controller_lease_id", "task_id"] {
        validate_identifier(
            binding.get(field).expect("binding field required"),
            &format!("temporary_model_package.binding.{field}"),
        )?;
    }
    validate_uint(
        binding
            .get("lease_epoch")
            .expect("binding lease epoch required"),
        "temporary_model_package.binding.lease_epoch",
        u64::MAX,
    )?;
    for field in [
        "connection_id",
        "controller_lease_id",
        "lease_epoch",
        "task_id",
    ] {
        if binding.get(field) != task.get(field) {
            return Err(ControllerProtocolError::new(
                "stale_context",
                format!("temporary_model_package.binding.{field}"),
            ));
        }
    }
    validate_route(
        package.get("primary").expect("primary route required"),
        "temporary_model_package.primary",
    )?;
    validate_credential(
        package
            .get("primary_credential")
            .expect("primary credential required"),
        "temporary_model_package.primary_credential",
    )?;
    if let Some(fallback) = package.get("vision_fallback") {
        let fallback = exact_nested(
            fallback,
            &["route", "credential"],
            &[],
            "temporary_model_package.vision_fallback",
        )?;
        validate_route(
            fallback.get("route").expect("fallback route required"),
            "temporary_model_package.vision_fallback.route",
        )?;
        validate_credential(
            fallback
                .get("credential")
                .expect("fallback credential required"),
            "temporary_model_package.vision_fallback.credential",
        )?;
    }
    Ok(())
}

fn validate_route(value: &Value, label: &str) -> Result<()> {
    const PRESETS: &[&str] = &[
        "local_vllm",
        "deepseek",
        "kimi",
        "openai_compatible",
        "qwen",
        "doubao",
        "minimax",
        "glm",
        "mimo",
        "openai",
        "anthropic",
        "gemini",
        "xai",
    ];
    const VENDORS: &[&str] = &[
        "anthropic",
        "deepseek",
        "doubao",
        "gemini",
        "glm",
        "kimi",
        "mimo",
        "minimax",
        "openai",
        "qwen",
        "tencent",
        "xai",
    ];
    let route = exact_nested(
        value,
        &[
            "display_name",
            "preset",
            "wire_api",
            "base_url",
            "model",
            "image_capability_override",
        ],
        &[
            "provider_kind",
            "vendor",
            "endpoint_mode",
            "context_window_tokens",
            "max_output_tokens",
            "reasoning_effort",
        ],
        label,
    )?;
    bounded_text(
        route.get("display_name").expect("display name required"),
        &format!("{label}.display_name"),
        128,
    )?;
    validate_enum(
        route.get("preset").expect("preset required"),
        &format!("{label}.preset"),
        PRESETS,
    )?;
    validate_enum(
        route.get("wire_api").expect("wire API required"),
        &format!("{label}.wire_api"),
        &["openai_chat_completions", "anthropic_messages"],
    )?;
    let preset = object_string(route, "preset", label)?;
    let wire_api = object_string(route, "wire_api", label)?;
    if (preset == "anthropic") != (wire_api == "anthropic_messages") {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            format!("{label}.wire_api"),
        ));
    }
    let base_url = bounded_text(
        route.get("base_url").expect("base URL required"),
        &format!("{label}.base_url"),
        2048,
    )?;
    if base_url.len() > 2048
        || !base_url
            .bytes()
            .all(|byte| byte.is_ascii() && !byte.is_ascii_whitespace())
    {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            format!("{label}.base_url"),
        ));
    }
    let lower = base_url.to_ascii_lowercase();
    if !(lower.starts_with("https://") || lower.starts_with("http://"))
        || base_url.split("//").nth(1).is_none_or(|authority| {
            authority
                .split('/')
                .next()
                .is_none_or(|host| host.is_empty() || host.contains('@'))
        })
    {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            format!("{label}.base_url"),
        ));
    }
    bounded_text(
        route.get("model").expect("model required"),
        &format!("{label}.model"),
        256,
    )?;
    validate_enum(
        route
            .get("image_capability_override")
            .expect("image override required"),
        &format!("{label}.image_capability_override"),
        &["pinvou", "enabled", "disabled"],
    )?;
    if let Some(value) = route.get("provider_kind") {
        validate_enum(
            value,
            &format!("{label}.provider_kind"),
            &["custom", "official_api"],
        )?;
    }
    if let Some(value) = route.get("vendor") {
        validate_enum(value, &format!("{label}.vendor"), VENDORS)?;
    }
    if let Some(value) = route.get("endpoint_mode") {
        validate_enum(
            value,
            &format!("{label}.endpoint_mode"),
            &["full_chat_completions"],
        )?;
    }
    for field in ["context_window_tokens", "max_output_tokens"] {
        if let Some(value) = route.get(field) {
            validate_uint(value, &format!("{label}.{field}"), u32::MAX as u64)?;
        }
    }
    if let Some(value) = route.get("reasoning_effort") {
        validate_enum(
            value,
            &format!("{label}.reasoning_effort"),
            &["off", "low", "medium", "high", "max", "auto"],
        )?;
    }
    Ok(())
}

fn validate_credential(value: &Value, label: &str) -> Result<()> {
    let credential = exact_nested(value, &["kind", "secret"], &[], label)?;
    validate_enum(
        credential.get("kind").expect("credential kind required"),
        &format!("{label}.kind"),
        &["api_key"],
    )?;
    let secret = bounded_text(
        credential
            .get("secret")
            .expect("credential secret required"),
        &format!("{label}.secret"),
        8192,
    )?;
    if secret.len() > 8192 {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            format!("{label}.secret"),
        ));
    }
    Ok(())
}

fn enforce_expected_context(
    object: &Map<String, Value>,
    context: Option<&LeaseContext>,
) -> Result<()> {
    let Some(context) = context else {
        return Ok(());
    };
    let expected = [
        ("connection_id", context.connection_id.as_str()),
        ("controller_lease_id", context.controller_lease_id.as_str()),
        ("lease_epoch", context.lease_epoch.as_str()),
        ("relay_epoch", context.relay_epoch.as_str()),
        (
            "remote_control_generation",
            context.remote_control_generation.as_str(),
        ),
    ];
    for (field, expected) in expected {
        if object.get("type").and_then(Value::as_str) == Some("error")
            && !object.contains_key(field)
        {
            return Err(ControllerProtocolError::new(
                "missing_field",
                format!("message.{field}"),
            ));
        }
        if object
            .get(field)
            .is_some_and(|value| value.as_str() != Some(expected))
        {
            return Err(ControllerProtocolError::new("stale_context", field));
        }
    }
    Ok(())
}

fn length_prefix(output: &mut Vec<u8>, bytes: &[u8]) {
    output.extend_from_slice(&(bytes.len() as u32).to_be_bytes());
    output.extend_from_slice(bytes);
}

fn length_prefix_text(output: &mut Vec<u8>, value: &str) {
    length_prefix(output, value.as_bytes());
}

pub(crate) fn build_receipt_commitment_preimage(
    relay_epoch: &str,
    pairing_id: &str,
    reservation_id: &str,
    transcript_hash: &str,
    sender_role: &str,
    secret: &[u8],
) -> Result<Vec<u8>> {
    for (field, value) in [
        ("relay_epoch", relay_epoch),
        ("pairing_id", pairing_id),
        ("reservation_id", reservation_id),
    ] {
        validate_identifier(&Value::String(value.to_owned()), field)?;
    }
    validate_hex_64(
        &Value::String(transcript_hash.to_owned()),
        "transcript_hash",
    )?;
    if !["A", "B"].contains(&sender_role) || secret.len() != 32 {
        return Err(ControllerProtocolError::new("invalid_field", "receipt"));
    }
    let mut output = b"pinvou-finished-receipt-commit-v1".to_vec();
    for value in [
        relay_epoch,
        pairing_id,
        reservation_id,
        transcript_hash,
        sender_role,
    ] {
        length_prefix_text(&mut output, value);
    }
    length_prefix(&mut output, secret);
    Ok(output)
}

const TRANSCRIPT_FIELDS: &[&str] = &[
    "schema",
    "relay_epoch",
    "pairing_id",
    "reservation_id",
    "endpoint_id",
    "remote_control_generation",
    "controller_connection_identity",
    "role_a",
    "handshake_public_key_a_b64",
    "role_b",
    "handshake_public_key_b_b64",
    "protocol_version",
    "capability_hash",
    "expires_at_ms",
    "nonce_b64",
];

pub(crate) fn build_pairing_transcript_preimage(value: &Value) -> Result<Vec<u8>> {
    let transcript = exact_nested(value, TRANSCRIPT_FIELDS, &[], "transcript")?;
    if object_string(transcript, "schema", "transcript")? != "controller_pairing_transcript/v1" {
        return Err(ControllerProtocolError::new(
            "unsupported_version",
            "transcript.schema",
        ));
    }
    for field in [
        "relay_epoch",
        "pairing_id",
        "reservation_id",
        "endpoint_id",
        "remote_control_generation",
        "controller_connection_identity",
    ] {
        validate_identifier(
            transcript.get(field).expect("transcript field required"),
            &format!("transcript.{field}"),
        )?;
    }
    if object_string(transcript, "role_a", "transcript")? != "A"
        || object_string(transcript, "role_b", "transcript")? != "B"
    {
        return Err(ControllerProtocolError::new(
            "role_mismatch",
            "transcript.role",
        ));
    }
    for field in ["handshake_public_key_a_b64", "handshake_public_key_b_b64"] {
        validate_base64(
            transcript.get(field).expect("transcript key required"),
            &format!("transcript.{field}"),
            32,
            Some(32),
        )?;
    }
    if transcript.get("handshake_public_key_a_b64") == transcript.get("handshake_public_key_b_b64")
    {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            "transcript.handshake_public_keys",
        ));
    }
    if transcript.get("protocol_version").and_then(Value::as_u64)
        != Some(CONTROLLER_PROTOCOL_VERSION)
    {
        return Err(ControllerProtocolError::new(
            "unsupported_version",
            "transcript.protocol_version",
        ));
    }
    validate_hex_64(
        transcript
            .get("capability_hash")
            .expect("capability hash required"),
        "transcript.capability_hash",
    )?;
    validate_uint(
        transcript.get("expires_at_ms").expect("expiry required"),
        "transcript.expires_at_ms",
        u64::MAX,
    )?;
    validate_base64(
        transcript.get("nonce_b64").expect("nonce required"),
        "transcript.nonce_b64",
        16,
        Some(16),
    )?;
    let mut output = b"pinvou-controller-pairing-transcript-v1".to_vec();
    for field in TRANSCRIPT_FIELDS {
        let value = if *field == "protocol_version" {
            CONTROLLER_PROTOCOL_VERSION.to_string()
        } else {
            object_string(transcript, field, "transcript")?.to_owned()
        };
        length_prefix_text(&mut output, &value);
    }
    Ok(output)
}

pub(crate) fn controller_capability_hash(capabilities: &[String]) -> Result<String> {
    if capabilities
        .iter()
        .any(|capability| !CONTROLLER_CAPABILITIES.contains(&capability.as_str()))
    {
        return Err(ControllerProtocolError::new(
            "unsupported_capability",
            "capabilities",
        ));
    }
    let value = Value::Array(capabilities.iter().cloned().map(Value::String).collect());
    validate_string_list(&value, "capabilities", CONTROLLER_CAPABILITIES)?;
    let mut sorted = capabilities.to_vec();
    sorted.sort();
    let mut preimage = b"pinvou-controller-capabilities-v1".to_vec();
    for capability in sorted {
        length_prefix_text(&mut preimage, &capability);
    }
    use sha2::{Digest, Sha256};
    Ok(Sha256::digest(preimage)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect())
}

fn validate_reserved_transcript(message: &Map<String, Value>) -> Result<()> {
    let transcript = message
        .get("transcript")
        .expect("reserved transcript required");
    let transcript_object = as_object(transcript, "transcript")?;
    for field in [
        "pairing_id",
        "reservation_id",
        "remote_control_generation",
        "relay_epoch",
    ] {
        if message.get(field) != transcript_object.get(field) {
            return Err(ControllerProtocolError::new(
                "stale_context",
                format!("transcript.{field}"),
            ));
        }
    }
    use sha2::{Digest, Sha256};
    let actual: String = Sha256::digest(build_pairing_transcript_preimage(transcript)?)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect();
    if message.get("transcript_hash").and_then(Value::as_str) != Some(actual.as_str()) {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            "transcript_hash",
        ));
    }
    Ok(())
}

pub(crate) fn build_direct_liveness_aad(value: &Value) -> Result<Vec<u8>> {
    const FIELDS: &[&str] = &[
        "connection_id",
        "controller_lease_id",
        "lease_epoch",
        "relay_epoch",
        "remote_control_generation",
        "role",
    ];
    let binding = exact_nested(value, FIELDS, &[], "liveness_aad")?;
    for field in [
        "connection_id",
        "controller_lease_id",
        "relay_epoch",
        "remote_control_generation",
    ] {
        validate_identifier(
            binding.get(field).expect("AAD field required"),
            &format!("liveness_aad.{field}"),
        )?;
    }
    validate_uint(
        binding
            .get("lease_epoch")
            .expect("AAD lease epoch required"),
        "liveness_aad.lease_epoch",
        u64::MAX,
    )?;
    if object_string(binding, "role", "liveness_aad")? != "A" {
        return Err(ControllerProtocolError::new(
            "role_mismatch",
            "liveness_aad.role",
        ));
    }
    let mut output = b"pinvou-controller-liveness-aad-v1".to_vec();
    for field in FIELDS {
        length_prefix_text(&mut output, object_string(binding, field, "liveness_aad")?);
    }
    Ok(output)
}

pub(crate) fn validate_controller_liveness_payload_json(
    input: &str,
    context: &LeaseContext,
    expected_liveness_epoch: &str,
    last_sequence: u64,
    challenge_id: &str,
    challenge_b64: &str,
) -> Result<Value> {
    if input.len() > MAX_CONTROL_MESSAGE_BYTES {
        return Err(ControllerProtocolError::new("message_too_large", "json"));
    }
    let value: Value = serde_json::from_str(input)
        .map_err(|_| ControllerProtocolError::new("invalid_json", "json"))?;
    let object = exact_nested(
        &value,
        &[
            "v",
            "type",
            "connection_id",
            "controller_lease_id",
            "lease_epoch",
            "relay_epoch",
            "remote_control_generation",
            "role",
            "liveness_epoch_a",
            "heartbeat_sequence_a",
            "challenge_id",
            "challenge_b64",
        ],
        &[],
        "liveness_payload",
    )?;
    if object.get("v").and_then(Value::as_u64) != Some(1) {
        return Err(ControllerProtocolError::new("unsupported_version", "v"));
    }
    if object_string(object, "type", "liveness_payload")? != "controller_liveness_a" {
        return Err(ControllerProtocolError::new(
            "unknown_message_type",
            "liveness_payload.type",
        ));
    }
    enforce_expected_context(object, Some(context))?;
    if object_string(object, "role", "liveness_payload")? != "A" {
        return Err(ControllerProtocolError::new("role_mismatch", "role"));
    }
    if object_string(object, "liveness_epoch_a", "liveness_payload")? != expected_liveness_epoch
        || object_string(object, "challenge_id", "liveness_payload")? != challenge_id
        || object_string(object, "challenge_b64", "liveness_payload")? != challenge_b64
    {
        return Err(ControllerProtocolError::new(
            "stale_context",
            "liveness_payload",
        ));
    }
    validate_base64(
        object.get("challenge_b64").expect("challenge required"),
        "challenge_b64",
        16,
        Some(16),
    )?;
    validate_identifier(
        object
            .get("liveness_epoch_a")
            .expect("liveness epoch required"),
        "liveness_epoch_a",
    )?;
    validate_identifier(
        object.get("challenge_id").expect("challenge ID required"),
        "challenge_id",
    )?;
    validate_uint(
        object
            .get("heartbeat_sequence_a")
            .expect("sequence required"),
        "heartbeat_sequence_a",
        u64::MAX,
    )?;
    if object_string(object, "heartbeat_sequence_a", "liveness_payload")?
        .parse::<u64>()
        .expect("canonical u64 validated")
        <= last_sequence
    {
        return Err(ControllerProtocolError::new(
            "replay",
            "heartbeat_sequence_a",
        ));
    }
    Ok(value)
}

pub(crate) fn build_liveness_attestation_preimage(message: &Value) -> Result<Vec<u8>> {
    const FIELDS: &[&str] = &[
        "relay_epoch",
        "connection_id",
        "controller_lease_id",
        "lease_epoch",
        "remote_control_generation",
        "liveness_epoch_a",
        "heartbeat_sequence_a",
        "attestation_sequence",
        "challenge_b64",
        "observed_age_ms",
        "remaining_ttl_ms",
        "issued_at_relay",
        "signature_algorithm",
        "relay_signing_key_id",
    ];
    let object = as_object(message, "attestation")?;
    if object.get("type").and_then(Value::as_str) != Some("controller_liveness_attestation_a") {
        return Err(ControllerProtocolError::new("invalid_field", "type"));
    }
    let mut output = b"pinvou-controller-liveness-attestation-v1".to_vec();
    for field in FIELDS {
        length_prefix_text(&mut output, object_string(object, field, "attestation")?);
    }
    Ok(output)
}

pub(crate) fn validate_liveness_attestation_message_json(
    input: &str,
    context: &LeaseContext,
    expected_liveness_epoch: &str,
    last_attestation_sequence: u64,
    challenge_id: &str,
    challenge_b64: &str,
) -> Result<Value> {
    let value = validate_controller_message_json_in_context(input, RELAY, Some(context))?;
    let object = as_object(&value, "attestation")?;
    if object.get("type").and_then(Value::as_str) != Some("controller_liveness_attestation_a") {
        return Err(ControllerProtocolError::new("invalid_field", "type"));
    }
    if object_string(object, "liveness_epoch_a", "attestation")? != expected_liveness_epoch
        || object_string(object, "challenge_id", "attestation")? != challenge_id
        || object_string(object, "challenge_b64", "attestation")? != challenge_b64
    {
        return Err(ControllerProtocolError::new("stale_context", "attestation"));
    }
    let sequence = object_string(object, "attestation_sequence", "attestation")?
        .parse::<u64>()
        .expect("canonical u64 validated");
    if sequence <= last_attestation_sequence {
        return Err(ControllerProtocolError::new(
            "replay",
            "attestation_sequence",
        ));
    }
    Ok(value)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use sha2::{Digest, Sha256};

    const FIXTURE: &str = include_str!(
        "../../../../../../remote-control-relay/test/fixtures/controller-protocol-v1.json"
    );

    fn fixture() -> Value {
        serde_json::from_str(FIXTURE).expect("shared fixture must be valid JSON")
    }

    fn decode_hex(value: &str) -> Vec<u8> {
        value
            .as_bytes()
            .chunks_exact(2)
            .map(|pair| {
                u8::from_str_radix(std::str::from_utf8(pair).expect("ASCII hex"), 16)
                    .expect("valid hex")
            })
            .collect()
    }

    fn sha256_hex(value: &[u8]) -> String {
        Sha256::digest(value)
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect()
    }

    fn context() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "epoch-test-1".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    fn role(value: &str) -> SenderRole {
        match value {
            "A" => A,
            "B" => B,
            "relay" => RELAY,
            _ => panic!("unknown fixture role {value}"),
        }
    }

    #[test]
    fn shared_fixture_freezes_registries_and_contract_hash() {
        let fixture = fixture();
        assert_eq!(
            serde_json::to_value(CONTROLLER_CAPABILITIES).unwrap(),
            fixture["expected_capabilities"]
        );
        assert_eq!(
            serde_json::to_value(CONTROLLER_ERROR_CODES).unwrap(),
            fixture["expected_error_codes"]
        );
        assert_eq!(
            serde_json::to_value(CONTROLLER_MESSAGE_TYPES).unwrap(),
            fixture["expected_message_types"]
        );
        assert_eq!(
            serde_json::to_value(CONTROLLER_PAYLOAD_TYPES).unwrap(),
            fixture["expected_payload_types"]
        );
        let manifest = format!(
            concat!(
                "{}\nmessages:{}\npayloads:{}\ncapabilities:{}\nerrors:{}\ninteger:{}\n",
                "lease-context:connection_id,controller_lease_id,lease_epoch,relay_epoch,remote_control_generation\n",
                "task-statuses:{}\n",
                "temporary-model-package:temporary_model_package/v1\n",
                "pairing-transcript:controller_pairing_transcript/v1\n",
                "direct-liveness:controller_liveness_a\n"
            ),
            fixture["contract"]["id"].as_str().unwrap(),
            CONTROLLER_MESSAGE_TYPES.join(","),
            CONTROLLER_PAYLOAD_TYPES.join(","),
            CONTROLLER_CAPABILITIES.join(","),
            CONTROLLER_ERROR_CODES.join(","),
            fixture["contract"]["integer_wire_format"].as_str().unwrap(),
            TASK_STATUSES.join(","),
        );
        assert_eq!(
            sha256_hex(manifest.as_bytes()),
            fixture["contract"]["sha256"].as_str().unwrap()
        );

        let matrix = fixture["message_role_matrix"].as_object().unwrap();
        assert_eq!(matrix.len(), CONTROLLER_MESSAGE_TYPES.len());
        for kind in CONTROLLER_MESSAGE_TYPES {
            let actual: Vec<&str> = message_schema(kind)
                .expect("registered message has a schema")
                .roles
                .iter()
                .map(|role| role.wire())
                .collect();
            let expected: Vec<&str> = matrix[*kind]
                .as_array()
                .unwrap()
                .iter()
                .map(|role| role.as_str().unwrap())
                .collect();
            assert_eq!(actual, expected, "{kind}");
        }
    }

    #[test]
    fn receipt_preimage_is_constructed_from_independent_fields() {
        let fixture = fixture();
        let vector = &fixture["receipt_commitment_vector"];
        let secret = decode_hex(vector["secret_hex"].as_str().unwrap());
        let preimage = build_receipt_commitment_preimage(
            vector["relay_epoch"].as_str().unwrap(),
            vector["pairing_id"].as_str().unwrap(),
            vector["reservation_id"].as_str().unwrap(),
            vector["transcript_hash"].as_str().unwrap(),
            vector["sender_role"].as_str().unwrap(),
            &secret,
        )
        .unwrap();
        assert_eq!(hex(&preimage), vector["preimage_hex"].as_str().unwrap());
        assert_eq!(
            sha256_hex(&preimage),
            vector["commitment_hex"].as_str().unwrap()
        );
    }

    #[test]
    fn capability_and_pairing_transcript_vectors_match_javascript() {
        let fixture = fixture();
        let capabilities: Vec<String> =
            serde_json::from_value(fixture["capability_vector"]["capabilities"].clone()).unwrap();
        assert_eq!(
            controller_capability_hash(&capabilities).unwrap(),
            fixture["capability_vector"]["sha256"].as_str().unwrap()
        );
        let transcript = &fixture["transcript_vector"]["transcript"];
        let preimage = build_pairing_transcript_preimage(transcript).unwrap();
        assert_eq!(
            hex(&preimage),
            fixture["transcript_vector"]["preimage_hex"]
                .as_str()
                .unwrap()
        );
        assert_eq!(
            sha256_hex(&preimage),
            fixture["transcript_vector"]["sha256"].as_str().unwrap()
        );
        let tampered = {
            let mut value = transcript.clone();
            value["endpoint_id"] = json!("endpoint-tampered");
            value
        };
        assert_ne!(
            sha256_hex(&build_pairing_transcript_preimage(&tampered).unwrap()),
            fixture["transcript_vector"]["sha256"].as_str().unwrap()
        );
    }

    #[test]
    fn strict_task_inputs_package_binding_and_statuses_match_fixture() {
        let fixture = fixture();
        for (field, role) in [
            ("capability_snapshot_vector", B),
            ("artifact_index_vector", B),
        ] {
            validate_controller_payload_json_in_context(
                &serde_json::to_string(&fixture[field]).unwrap(),
                role,
                Some(&context()),
            )
            .unwrap();
        }
        let mut leaked_capability = fixture["capability_snapshot_vector"].clone();
        leaked_capability["workspaces"][0]["path"] = json!("C:\\private");
        assert_eq!(
            validate_controller_payload_json(
                &serde_json::to_string(&leaked_capability).unwrap(),
                B
            )
            .unwrap_err()
            .code,
            "unknown_field"
        );
        let mut leaked_artifact = fixture["artifact_index_vector"].clone();
        leaked_artifact["artifacts"][0]["absolute_path"] = json!("C:\\private\\report.md");
        assert_eq!(
            validate_controller_payload_json(&serde_json::to_string(&leaked_artifact).unwrap(), B)
                .unwrap_err()
                .code,
            "unknown_field"
        );
        for name in ["controlled", "temporary"] {
            let payload = &fixture["task_submit_vectors"][name];
            validate_controller_payload_json_in_context(
                &serde_json::to_string(payload).unwrap(),
                A,
                Some(&context()),
            )
            .unwrap();
        }

        let mut temporary = fixture["task_submit_vectors"]["temporary"].clone();
        temporary["temporary_model_package"]["binding"]["task_id"] = json!("task-other");
        assert_eq!(
            validate_controller_payload_json(&serde_json::to_string(&temporary).unwrap(), A)
                .unwrap_err()
                .code,
            "stale_context"
        );
        let mut unknown = fixture["task_submit_vectors"]["temporary"].clone();
        unknown["temporary_model_package"]["primary"]["cookie"] = json!("forbidden");
        assert_eq!(
            validate_controller_payload_json(&serde_json::to_string(&unknown).unwrap(), A)
                .unwrap_err()
                .code,
            "unknown_field"
        );
        let mut oauth = fixture["task_submit_vectors"]["temporary"].clone();
        oauth["temporary_model_package"]["primary_credential"]["kind"] = json!("oauth");
        assert_eq!(
            validate_controller_payload_json(&serde_json::to_string(&oauth).unwrap(), A)
                .unwrap_err()
                .code,
            "invalid_enum"
        );
        let mut oversized_package = fixture["task_submit_vectors"]["temporary"].clone();
        oversized_package["temporary_model_package"]["primary_credential"]["secret"] =
            json!("x".repeat(MAX_TEMPORARY_MODEL_PACKAGE_BYTES));
        assert_eq!(
            validate_controller_payload_json(
                &serde_json::to_string(&oversized_package).unwrap(),
                A
            )
            .unwrap_err()
            .code,
            "message_too_large"
        );
        let mut overlong = fixture["task_submit_vectors"]["controlled"].clone();
        overlong["input"]["text"] = json!("x".repeat(MAX_TASK_INPUT_TEXT_BYTES + 1));
        assert_eq!(
            validate_controller_payload_json(&serde_json::to_string(&overlong).unwrap(), A)
                .unwrap_err()
                .code,
            "invalid_field"
        );

        let base = json!({
            "v": 1, "type": "task_status",
            "connection_id": "connection-0001", "controller_lease_id": "lease-0001",
            "lease_epoch": "1", "relay_epoch": "epoch-test-1",
            "remote_control_generation": "generation-0001",
            "request_id": "request-1", "task_id": "task-1", "session_id": "session-1",
            "task_status": "admitted"
        });
        for status in TASK_STATUSES {
            let mut payload = base.clone();
            payload["task_status"] = json!(status);
            validate_controller_payload_json(&serde_json::to_string(&payload).unwrap(), B).unwrap();
        }
        let mut legacy = base;
        legacy["task_status"] = json!("completed");
        assert_eq!(
            validate_controller_payload_json(&serde_json::to_string(&legacy).unwrap(), B)
                .unwrap_err()
                .code,
            "invalid_enum"
        );
    }

    #[test]
    fn canonical_decimal_strings_and_stale_context_fail_closed() {
        let base = json!({
            "v": 1, "type": "task_cancel",
            "connection_id": "connection-0001", "controller_lease_id": "lease-0001",
            "lease_epoch": "1", "relay_epoch": "epoch-test-1",
            "remote_control_generation": "generation-0001",
            "request_id": "request-1", "task_id": "task-1", "session_id": "session-1"
        });
        validate_controller_payload_json_in_context(
            &serde_json::to_string(&base).unwrap(),
            A,
            Some(&context()),
        )
        .unwrap();
        for invalid in [
            json!(1),
            json!(1.0),
            json!("01"),
            json!("-1"),
            json!("18446744073709551616"),
        ] {
            let mut value = base.clone();
            value["lease_epoch"] = invalid;
            assert_eq!(
                validate_controller_payload_json(&serde_json::to_string(&value).unwrap(), A)
                    .unwrap_err()
                    .code,
                "invalid_field"
            );
        }
        let mut stale = base;
        stale["remote_control_generation"] = json!("generation-old");
        assert_eq!(
            validate_controller_payload_json_in_context(
                &serde_json::to_string(&stale).unwrap(),
                A,
                Some(&context())
            )
            .unwrap_err()
            .code,
            "stale_context"
        );
    }

    #[test]
    fn direct_liveness_and_attestation_vectors_bind_all_context() {
        let fixture = fixture();
        let direct = &fixture["direct_liveness_vector"];
        validate_controller_message_json_in_context(
            &serde_json::to_string(&direct["outer"]).unwrap(),
            A,
            Some(&context()),
        )
        .unwrap();
        validate_controller_liveness_payload_json(
            &serde_json::to_string(&direct["inner"]).unwrap(),
            &context(),
            "live-a-1",
            1,
            "challenge-0001",
            "AAAAAAAAAAAAAAAAAAAAAA==",
        )
        .unwrap();
        let aad = build_direct_liveness_aad(&direct["aad_binding"]).unwrap();
        assert_eq!(hex(&aad), direct["aad_preimage_hex"].as_str().unwrap());
        assert_eq!(sha256_hex(&aad), direct["aad_sha256"].as_str().unwrap());

        let mut replay = direct["inner"].clone();
        replay["heartbeat_sequence_a"] = json!("1");
        assert_eq!(
            validate_controller_liveness_payload_json(
                &serde_json::to_string(&replay).unwrap(),
                &context(),
                "live-a-1",
                1,
                "challenge-0001",
                "AAAAAAAAAAAAAAAAAAAAAA==",
            )
            .unwrap_err()
            .code,
            "replay"
        );

        let attestation = &fixture["attestation_vector"];
        validate_liveness_attestation_message_json(
            &serde_json::to_string(&attestation["message"]).unwrap(),
            &context(),
            "live-a-1",
            0,
            "challenge-0001",
            "AAAAAAAAAAAAAAAAAAAAAA==",
        )
        .unwrap();
        assert_eq!(
            validate_liveness_attestation_message_json(
                &serde_json::to_string(&attestation["message"]).unwrap(),
                &context(),
                "live-a-1",
                1,
                "challenge-0001",
                "AAAAAAAAAAAAAAAAAAAAAA==",
            )
            .unwrap_err()
            .code,
            "replay"
        );
        let preimage = build_liveness_attestation_preimage(&attestation["message"]).unwrap();
        assert_eq!(
            hex(&preimage),
            attestation["preimage_hex"].as_str().unwrap()
        );
        assert_eq!(
            sha256_hex(&preimage),
            attestation["sha256"].as_str().unwrap()
        );
        let mut tampered = attestation["message"].clone();
        tampered["remote_control_generation"] = json!("generation-old");
        assert_ne!(
            sha256_hex(&build_liveness_attestation_preimage(&tampered).unwrap()),
            attestation["sha256"].as_str().unwrap()
        );
    }

    #[test]
    fn roles_unknown_fields_and_size_limits_fail_closed() {
        let fixture = fixture();
        for item in fixture["invalid_wire_examples"].as_array().unwrap() {
            let encoded = serde_json::to_string(&item["value"]).unwrap();
            let sender = role(item["sender_role"].as_str().unwrap());
            let error = if item["target"] == "message" {
                validate_controller_message_json(&encoded, sender).unwrap_err()
            } else {
                validate_controller_payload_json(&encoded, sender).unwrap_err()
            };
            assert_eq!(
                error.code,
                item["expected_error"].as_str().unwrap(),
                "{}",
                item["name"].as_str().unwrap()
            );
        }
        let payload = json!({
            "v": 1, "type": "task_cancel",
            "connection_id": "connection-0001", "controller_lease_id": "lease-0001",
            "lease_epoch": "1", "relay_epoch": "epoch-test-1",
            "remote_control_generation": "generation-0001",
            "request_id": "request-1", "task_id": "task-1", "session_id": "session-1"
        });
        assert_eq!(
            validate_controller_payload_json(&serde_json::to_string(&payload).unwrap(), B)
                .unwrap_err()
                .code,
            "role_mismatch"
        );
        let mut unknown = payload;
        unknown["future"] = json!(true);
        assert_eq!(
            validate_controller_payload_json(&serde_json::to_string(&unknown).unwrap(), A)
                .unwrap_err()
                .code,
            "unknown_field"
        );
        let missing = r#"{"v":1,"type":"controller_capabilities","request_id":"req-1","client_role":"desktop_controller","sender_role":"A","protocol_version":1}"#;
        assert_eq!(
            validate_controller_message_json(missing, A)
                .unwrap_err()
                .code,
            "missing_field"
        );
        let unsupported = r#"{"v":1,"type":"controller_capabilities","request_id":"req-1","client_role":"desktop_controller","sender_role":"A","protocol_version":1,"capabilities":["future_v2"]}"#;
        assert_eq!(
            validate_controller_message_json(unsupported, A)
                .unwrap_err()
                .code,
            "unsupported_capability"
        );
        let wrong_version = r#"{"v":1,"type":"controller_capabilities","request_id":"req-1","client_role":"desktop_controller","sender_role":"A","protocol_version":2,"capabilities":[]}"#;
        assert_eq!(
            validate_controller_message_json(wrong_version, A)
                .unwrap_err()
                .code,
            "unsupported_version"
        );
        let message = r#"{"v":1,"type":"controller_capabilities","request_id":"req-1","client_role":"desktop_controller","sender_role":"A","protocol_version":1,"capabilities":[]}"#;
        let oversized = format!("{message}{}", " ".repeat(MAX_CONTROL_MESSAGE_BYTES));
        assert_eq!(
            validate_controller_message_json(&oversized, A)
                .unwrap_err()
                .code,
            "message_too_large"
        );
    }

    fn hex(bytes: &[u8]) -> String {
        bytes.iter().map(|byte| format!("{byte:02x}")).collect()
    }
}
