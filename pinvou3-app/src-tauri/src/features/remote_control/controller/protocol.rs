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
const MAX_ID_CHARS: usize = 128;
const MAX_SAFE_JSON_INTEGER: u64 = 9_007_199_254_740_991;
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
];

const CONTROLLER_ERROR_CODES: &[&str] = &[
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
                "transcript_hash",
                "peer_handshake_public_key_b64",
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
                "request_id",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "liveness_epoch_a",
                "heartbeat_sequence_a",
                "challenge_id",
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
                "liveness_epoch_a",
                "heartbeat_sequence_a",
                "attestation_sequence",
                "challenge_id",
                "observed_age_ms",
                "remaining_ttl_ms",
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
            optional: &[],
            roles: &[RELAY],
        },
        _ => return None,
    };
    Some(schema)
}

fn payload_schema(kind: &str) -> Option<Schema> {
    let schema = match kind {
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
                "model_source",
                "mode",
                "input_digest",
                "attachment_ids",
            ],
            optional: &[],
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
        "task_terminal" => Schema {
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
                "artifact_ids",
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
    Ok(value)
}

pub(crate) fn validate_controller_payload_json(
    input: &str,
    sender_role: SenderRole,
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
    Ok(value)
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
    for field in object.keys() {
        if !schema.required.contains(&field.as_str()) && !schema.optional.contains(&field.as_str())
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
    for field in schema.required.iter().chain(schema.optional.iter()) {
        if let Some(value) = object.get(*field) {
            validate_named_field(object, field, value)?;
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
        validate_string_list(value, field, CONTROLLER_CAPABILITIES)?;
    } else if field == "attachment_ids" || field == "artifact_ids" {
        validate_string_list(value, field, &[])?;
    } else if field == "sender_role" || field == "receiver_role" {
        validate_enum(value, field, &["A", "B"])?;
    } else if field == "client_role" {
        validate_enum(
            value,
            field,
            &["desktop_controller", "desktop_controlled_endpoint"],
        )?;
    } else if field == "key_algorithm" {
        validate_enum(value, field, &["x25519"])?;
    } else if field == "direction" {
        validate_enum(value, field, &["a_to_b", "b_to_a"])?;
    } else if field == "payload_kind" {
        validate_enum(value, field, CONTROLLER_PAYLOAD_TYPES)?;
    } else if field == "model_source" {
        validate_enum(value, field, &["controlled_device", "controller_temporary"])?;
    } else if field == "mode" {
        validate_enum(value, field, &["chat", "code"])?;
    } else if field == "task_status" {
        validate_enum(
            value,
            field,
            &[
                "completed",
                "failed",
                "cancelled",
                "interrupted",
                "result_unknown",
            ],
        )?;
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
    } else if field == "challenge_b64" || field == "transport_challenge_b64" {
        validate_base64(value, field, 32, Some(16))?;
    } else if field == "receipt_secret_b64" {
        validate_base64(value, field, 32, Some(32))?;
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
    let effective_max = max.min(MAX_SAFE_JSON_INTEGER);
    if value.as_u64().is_none_or(|number| number > effective_max) {
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
    if object.get(declared_field).and_then(Value::as_u64) != Some(actual as u64) {
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
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde::Deserialize;
    use sha2::{Digest, Sha256};

    const FIXTURE: &str = include_str!(
        "../../../../../../remote-control-relay/test/fixtures/controller-protocol-v1.json"
    );

    #[derive(Deserialize)]
    struct Fixtures {
        expected_message_types: Vec<String>,
        expected_payload_types: Vec<String>,
        receipt_commitment_vector: ReceiptVector,
        valid_messages: Vec<MessageCase>,
        valid_payloads: Vec<PayloadCase>,
        invalid_messages: Vec<InvalidMessageCase>,
        invalid_payloads: Vec<InvalidPayloadCase>,
    }

    #[derive(Deserialize)]
    struct MessageCase {
        name: String,
        sender_role: String,
        message: Value,
    }

    #[derive(Deserialize)]
    struct PayloadCase {
        name: String,
        sender_role: String,
        payload: Value,
    }

    #[derive(Deserialize)]
    struct InvalidMessageCase {
        name: String,
        sender_role: String,
        expected_error: String,
        message: Value,
    }

    #[derive(Deserialize)]
    struct InvalidPayloadCase {
        name: String,
        sender_role: String,
        expected_error: String,
        payload: Value,
    }

    #[derive(Deserialize)]
    struct ReceiptVector {
        preimage_hex: String,
        commitment_hex: String,
    }

    fn fixtures() -> Fixtures {
        serde_json::from_str(FIXTURE).expect("shared fixture must be valid JSON")
    }

    fn role(value: &str) -> SenderRole {
        match value {
            "A" => A,
            "B" => B,
            "relay" => RELAY,
            _ => panic!("unknown fixture role {value}"),
        }
    }

    fn decode_hex(value: &str) -> Vec<u8> {
        assert_eq!(value.len() % 2, 0);
        value
            .as_bytes()
            .chunks_exact(2)
            .map(|pair| {
                let text = std::str::from_utf8(pair).expect("ASCII hex");
                u8::from_str_radix(text, 16).expect("valid hex")
            })
            .collect()
    }

    #[test]
    fn shared_fixture_freezes_the_protocol_registry() {
        let fixtures = fixtures();
        let mut actual_messages = CONTROLLER_MESSAGE_TYPES.to_vec();
        let mut expected_messages: Vec<&str> = fixtures
            .expected_message_types
            .iter()
            .map(String::as_str)
            .collect();
        actual_messages.sort_unstable();
        expected_messages.sort_unstable();
        assert_eq!(actual_messages, expected_messages);

        let mut actual_payloads = CONTROLLER_PAYLOAD_TYPES.to_vec();
        let mut expected_payloads: Vec<&str> = fixtures
            .expected_payload_types
            .iter()
            .map(String::as_str)
            .collect();
        actual_payloads.sort_unstable();
        expected_payloads.sort_unstable();
        assert_eq!(actual_payloads, expected_payloads);
    }

    #[test]
    fn shared_valid_fixtures_are_accepted() {
        let fixtures = fixtures();
        for case in fixtures.valid_messages {
            let encoded = serde_json::to_string(&case.message).unwrap();
            validate_controller_message_json(&encoded, role(&case.sender_role))
                .unwrap_or_else(|error| panic!("{}: {error}", case.name));
        }
        for case in fixtures.valid_payloads {
            let encoded = serde_json::to_string(&case.payload).unwrap();
            validate_controller_payload_json(&encoded, role(&case.sender_role))
                .unwrap_or_else(|error| panic!("{}: {error}", case.name));
        }
    }

    #[test]
    fn shared_invalid_fixtures_are_rejected() {
        let fixtures = fixtures();
        for case in fixtures.invalid_messages {
            let encoded = serde_json::to_string(&case.message).unwrap();
            let error =
                validate_controller_message_json(&encoded, role(&case.sender_role)).unwrap_err();
            assert_eq!(error.code, case.expected_error, "{}", case.name);
        }
        for case in fixtures.invalid_payloads {
            let encoded = serde_json::to_string(&case.payload).unwrap();
            let error =
                validate_controller_payload_json(&encoded, role(&case.sender_role)).unwrap_err();
            assert_eq!(error.code, case.expected_error, "{}", case.name);
        }
    }

    #[test]
    fn shared_receipt_commitment_vector_matches_sha256() {
        let vector = fixtures().receipt_commitment_vector;
        let preimage = decode_hex(&vector.preimage_hex);
        assert_eq!(preimage.len(), 194);
        let actual: String = Sha256::digest(preimage)
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect();
        assert_eq!(actual, vector.commitment_hex);
    }

    #[test]
    fn raw_size_limits_and_roles_fail_closed() {
        let message = r#"{"v":1,"type":"controller_capabilities","request_id":"req-1","client_role":"desktop_controller","sender_role":"A","protocol_version":1,"capabilities":[]}"#;
        let oversized = format!("{message}{}", " ".repeat(MAX_CONTROL_MESSAGE_BYTES));
        assert_eq!(
            validate_controller_message_json(&oversized, A)
                .unwrap_err()
                .code,
            "message_too_large"
        );

        let payload = r#"{"v":1,"type":"task_cancel","connection_id":"connection-1","controller_lease_id":"lease-1","lease_epoch":1,"request_id":"req-1","task_id":"task-1","session_id":"session-1"}"#;
        assert_eq!(
            validate_controller_payload_json(payload, B)
                .unwrap_err()
                .code,
            "role_mismatch"
        );
    }
}
