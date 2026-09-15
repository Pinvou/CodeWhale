//! Frozen desktop-to-desktop controller protocol v1 model.
//!
//! DEV-00 deliberately keeps this module disconnected from the live Relay and
//! Tauri command paths. Later tasks may consume these validators after their
//! state machines and cryptography are implemented.

use std::collections::{HashMap, HashSet};
use std::fmt;

use aws_lc_rs::signature::{ED25519, UnparsedPublicKey};
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
const MAX_TASK_REFERENCES: usize = 32;
const MAX_DESCRIPTORS: usize = 256;
const MAX_ARTIFACTS: usize = 256;
const MAX_RELAY_TRUST_KEYS: usize = 32;
const MAX_ENCRYPTED_WIRE_BYTES: usize =
    MAX_ENCRYPTED_FRAME_BYTES.div_ceil(3) * 4 + MAX_CONTROL_MESSAGE_BYTES;
const CONTROLLER_CONTRACT_SHA256: &str =
    "6f3c23d36f899d9f61c0c0a231ea52778581ee5a81b94110d2eb566c5e5b8cf3";

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
pub(crate) const LEASE_INSTALL_MESSAGE_TYPES: &[&str] = &["controller_joined"];

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
const SENDER_ROLES: &[&str] = &["A", "B"];
const CLIENT_ROLES: &[&str] = &["desktop_controller", "desktop_controlled_endpoint"];
const KEY_ALGORITHMS: &[&str] = &["x25519"];
const SIGNATURE_ALGORITHMS: &[&str] = &["ed25519"];
const DIRECTIONS: &[&str] = &["a_to_b", "b_to_a"];
const MODEL_SOURCES: &[&str] = &["controlled_device", "controller_temporary"];
const MODES: &[&str] = &["chat", "code"];
const DECISIONS: &[&str] = &["allow", "deny"];
const RISK_LEVELS: &[&str] = &["low", "medium", "high", "critical"];
const EVENT_KINDS: &[&str] = &[
    "user_message",
    "assistant_delta",
    "reasoning",
    "tool",
    "permission",
    "error",
    "cancel",
    "artifact",
    "terminal",
];
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
const WIRE_APIS: &[&str] = &["openai_chat_completions", "anthropic_messages"];
const PROVIDER_KINDS: &[&str] = &["custom", "official_api"];
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
const ENDPOINT_MODES: &[&str] = &["full_chat_completions"];
const REASONING_EFFORTS: &[&str] = &["off", "low", "medium", "high", "max", "auto"];
const IMAGE_CAPABILITY_OVERRIDES: &[&str] = &["pinvou", "enabled", "disabled"];
const ARTIFACT_PREVIEW_MODES: &[&str] = &["inline_text", "inline_image", "download_only"];
const CREDENTIAL_KINDS: &[&str] = &["api_key"];

const LEASE_CONTEXT_FIELDS: &[&str] = &[
    "connection_id",
    "controller_lease_id",
    "lease_epoch",
    "relay_epoch",
    "remote_control_generation",
];
const ERROR_CONTEXT_FIELDS: &[&str] = &[
    "connection_id",
    "controller_lease_id",
    "lease_epoch",
    "relay_epoch",
    "remote_control_generation",
    "registration_request_id",
];
const LEASE_INSTALL_CONTEXT_FIELDS: &[&str] = &["verified_transcript"];

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

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SequenceDisposition {
    Advance,
    SameSequenceTighten,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DeadlineUpdate {
    Replace,
    TightenOnly,
}

pub(crate) fn classify_monotonic_sequence(
    incoming: &str,
    previous: u64,
    field: &'static str,
) -> Result<SequenceDisposition> {
    let incoming = incoming
        .parse::<u64>()
        .map_err(|_| ControllerProtocolError::new("invalid_field", field))?;
    if incoming < previous {
        return Err(ControllerProtocolError::new("replay", field));
    }
    Ok(if incoming == previous {
        SequenceDisposition::SameSequenceTighten
    } else {
        SequenceDisposition::Advance
    })
}

pub(crate) fn apply_liveness_deadline(
    existing_deadline_ms: u64,
    candidate_deadline_ms: u64,
    policy: DeadlineUpdate,
) -> u64 {
    match policy {
        DeadlineUpdate::Replace => candidate_deadline_ms,
        DeadlineUpdate::TightenOnly => existing_deadline_ms.min(candidate_deadline_ms),
    }
}

struct Schema {
    required: &'static [&'static str],
    optional: &'static [&'static str],
    roles: &'static [SenderRole],
}

struct CapabilityBinding {
    kind: &'static str,
    capabilities: &'static [&'static str],
}

const PAYLOAD_CAPABILITY_BINDINGS: &[CapabilityBinding] = &[
    CapabilityBinding {
        kind: "capability_snapshot_request",
        capabilities: &["task_v1"],
    },
    CapabilityBinding {
        kind: "capability_snapshot",
        capabilities: &["task_v1"],
    },
    CapabilityBinding {
        kind: "task_submit",
        capabilities: &["task_v1"],
    },
    CapabilityBinding {
        kind: "task_cancel",
        capabilities: &["task_v1"],
    },
    CapabilityBinding {
        kind: "task_status",
        capabilities: &["task_v1"],
    },
    CapabilityBinding {
        kind: "permission_request",
        capabilities: &["permission_v1"],
    },
    CapabilityBinding {
        kind: "permission_decision",
        capabilities: &["permission_v1"],
    },
    CapabilityBinding {
        kind: "attachment_begin",
        capabilities: &["attachment_v1"],
    },
    CapabilityBinding {
        kind: "attachment_chunk",
        capabilities: &["attachment_v1"],
    },
    CapabilityBinding {
        kind: "attachment_commit",
        capabilities: &["attachment_v1"],
    },
    CapabilityBinding {
        kind: "artifact_index_request",
        capabilities: &["artifact_v1"],
    },
    CapabilityBinding {
        kind: "artifact_index",
        capabilities: &["artifact_v1"],
    },
    CapabilityBinding {
        kind: "artifact_read_request",
        capabilities: &["artifact_v1"],
    },
    CapabilityBinding {
        kind: "artifact_chunk",
        capabilities: &["artifact_v1"],
    },
    CapabilityBinding {
        kind: "session_snapshot_request",
        capabilities: &["snapshot_journal_v1"],
    },
    CapabilityBinding {
        kind: "session_snapshot",
        capabilities: &["snapshot_journal_v1"],
    },
    CapabilityBinding {
        kind: "journal_replay_request",
        capabilities: &["snapshot_journal_v1"],
    },
    CapabilityBinding {
        kind: "journal_event",
        capabilities: &["snapshot_journal_v1"],
    },
];

const OUTER_MESSAGE_CAPABILITY_BINDINGS: &[CapabilityBinding] = &[
    CapabilityBinding {
        kind: "desktop_endpoint_claim",
        capabilities: &["pairing_v1"],
    },
    CapabilityBinding {
        kind: "desktop_pairing_register",
        capabilities: &["pairing_v1"],
    },
    CapabilityBinding {
        kind: "desktop_pairing_registered",
        capabilities: &["pairing_v1"],
    },
    CapabilityBinding {
        kind: "desktop_pairing_revoke",
        capabilities: &["pairing_v1"],
    },
    CapabilityBinding {
        kind: "controller_pairing_claim",
        capabilities: &["pairing_v1"],
    },
    CapabilityBinding {
        kind: "controller_pairing_reserved",
        capabilities: &["pairing_v1"],
    },
    CapabilityBinding {
        kind: "controller_joined",
        capabilities: &["pairing_v1"],
    },
    CapabilityBinding {
        kind: "finished_receipt_commit",
        capabilities: &["finished_receipt_v1"],
    },
    CapabilityBinding {
        kind: "encrypted_handshake_finished",
        capabilities: &["finished_receipt_v1"],
    },
    CapabilityBinding {
        kind: "finished_receipt_ack",
        capabilities: &["finished_receipt_v1"],
    },
    CapabilityBinding {
        kind: "controller_liveness",
        capabilities: &["role_liveness_v1"],
    },
    CapabilityBinding {
        kind: "controller_liveness_challenge_a",
        capabilities: &["role_liveness_v1"],
    },
    CapabilityBinding {
        kind: "encrypted_controller_liveness_a",
        capabilities: &["role_liveness_v1"],
    },
    CapabilityBinding {
        kind: "controller_liveness_attestation_request_a",
        capabilities: &["role_liveness_v1"],
    },
    CapabilityBinding {
        kind: "controller_liveness_attestation_a",
        capabilities: &["role_liveness_v1"],
    },
    CapabilityBinding {
        kind: "controller_resume_prepare",
        capabilities: &["resume_v1"],
    },
    CapabilityBinding {
        kind: "controller_resume_provisional",
        capabilities: &["resume_v1"],
    },
    CapabilityBinding {
        kind: "encrypted_resume_confirm",
        capabilities: &["resume_v1"],
    },
    CapabilityBinding {
        kind: "controller_resume_activation_ack",
        capabilities: &["resume_v1"],
    },
    CapabilityBinding {
        kind: "controller_resume_activation_pending",
        capabilities: &["resume_v1"],
    },
    CapabilityBinding {
        kind: "controller_post_activation_liveness",
        capabilities: &["resume_v1"],
    },
    CapabilityBinding {
        kind: "controller_post_activation_liveness_confirmed",
        capabilities: &["resume_v1"],
    },
    CapabilityBinding {
        kind: "controller_resume_ready_ack",
        capabilities: &["resume_v1"],
    },
    CapabilityBinding {
        kind: "controller_resumed",
        capabilities: &["resume_v1"],
    },
    CapabilityBinding {
        kind: "encrypted_frame",
        capabilities: &["encrypted_frames_v1"],
    },
    CapabilityBinding {
        kind: "controller_disconnect",
        capabilities: &["encrypted_frames_v1"],
    },
];

const ROUTE_REQUIRED: &[&str] = &[
    "display_name",
    "preset",
    "wire_api",
    "base_url",
    "model",
    "image_capability_override",
];
const ROUTE_OPTIONAL: &[&str] = &[
    "provider_kind",
    "vendor",
    "endpoint_mode",
    "context_window_tokens",
    "max_output_tokens",
    "reasoning_effort",
];

const NESTED_SCHEMAS: &[(&str, Schema)] = &[
    (
        "task_input",
        Schema {
            required: &["text", "attachment_ids", "skill_ids"],
            optional: &["workspace_id"],
            roles: &[],
        },
    ),
    (
        "temporary_model_package_v1",
        Schema {
            required: &[
                "schema",
                "package_id",
                "binding",
                "primary",
                "primary_credential",
            ],
            optional: &["vision_fallback"],
            roles: &[],
        },
    ),
    (
        "temporary_model_binding",
        Schema {
            required: &[
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "task_id",
            ],
            optional: &[],
            roles: &[],
        },
    ),
    (
        "vision_fallback",
        Schema {
            required: &["route", "credential"],
            optional: &[],
            roles: &[],
        },
    ),
    (
        "route",
        Schema {
            required: ROUTE_REQUIRED,
            optional: ROUTE_OPTIONAL,
            roles: &[],
        },
    ),
    (
        "credential",
        Schema {
            required: &["kind", "secret"],
            optional: &[],
            roles: &[],
        },
    ),
    (
        "tool_descriptor",
        Schema {
            required: &["tool_id", "display_name"],
            optional: &[],
            roles: &[],
        },
    ),
    (
        "skill_descriptor",
        Schema {
            required: &["skill_id", "display_name"],
            optional: &[],
            roles: &[],
        },
    ),
    (
        "workspace_descriptor",
        Schema {
            required: &["workspace_id", "display_name", "code_allowed"],
            optional: &[],
            roles: &[],
        },
    ),
    (
        "artifact_descriptor",
        Schema {
            required: &[
                "artifact_id",
                "task_id",
                "display_name",
                "mime_type",
                "size_bytes",
                "sha256",
                "preview_mode",
            ],
            optional: &[],
            roles: &[],
        },
    ),
    (
        "controller_liveness_a",
        Schema {
            required: &[
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
            optional: &[],
            roles: &[],
        },
    ),
];

fn nested_schema(name: &str) -> &'static Schema {
    &NESTED_SCHEMAS
        .iter()
        .find(|(candidate, _)| *candidate == name)
        .expect("registered nested schema")
        .1
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
                "registration_request_id",
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
                "registration_request_id",
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
            optional: &["pairing_id"],
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
                "pairing_id",
                "reservation_id",
                "transcript_hash",
                "controller_connection_identity",
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
                "sender_role",
                "reason",
            ],
            optional: &[],
            roles: &[A, B],
        },
        "error" => Schema {
            required: &["v", "type", "request_id", "code", "message"],
            optional: ERROR_CONTEXT_FIELDS,
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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
                "relay_epoch",
                "remote_control_generation",
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

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct RuntimeValidationContext {
    pub(crate) lease: Option<LeaseContext>,
    pub(crate) negotiated_capabilities: Vec<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct LeaseInstallValidationContext {
    request_id: String,
    expected_pairing_id: String,
    reservation_id: String,
    remote_control_generation: String,
    relay_epoch: String,
    transcript_hash: String,
    expected_controller_connection_identity: String,
    negotiated_capabilities: Vec<String>,
    capability_hash: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct VerifiedTranscript {
    request_id: String,
    expected_pairing_id: String,
    reservation_id: String,
    remote_control_generation: String,
    relay_epoch: String,
    transcript_hash: String,
    expected_controller_connection_identity: String,
    negotiated_capabilities: Vec<String>,
    capability_hash: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PinnedRelayKey {
    key_id: String,
    public_key: [u8; 32],
}

#[derive(Clone, Debug)]
pub(crate) struct RelayTrustStore {
    keys: HashMap<String, PinnedRelayKey>,
}

impl RelayTrustStore {
    pub(crate) fn pinned_key(&self, key_id: &str) -> Result<&PinnedRelayKey> {
        validate_identifier(&Value::String(key_id.to_owned()), "relay_signing_key_id")?;
        self.keys
            .get(key_id)
            .ok_or_else(|| ControllerProtocolError::new("stale_context", "relay_signing_key_id"))
    }
}

/// Trust-boundary loader reserved for the DEV-01/DEV-05 enrollment adapters.
/// The returned immutable entries atomically bind one key ID to one Ed25519 key.
pub(crate) fn load_relay_trust_store_from_enrollment_config(
    input: &Value,
) -> Result<RelayTrustStore> {
    let config = exact_nested(input, &["source", "keys"], &[], "relay_trust_store_config")?;
    validate_enum(
        config.get("source").expect("trust store source required"),
        "relay_trust_store_config.source",
        &["official_enrollment", "self_hosted_enrollment"],
    )?;
    let raw_keys = config
        .get("keys")
        .and_then(Value::as_array)
        .filter(|keys| !keys.is_empty() && keys.len() <= MAX_RELAY_TRUST_KEYS)
        .ok_or_else(|| {
            ControllerProtocolError::new("invalid_field", "relay_trust_store_config.keys")
        })?;
    let mut keys = HashMap::with_capacity(raw_keys.len());
    for (index, value) in raw_keys.iter().enumerate() {
        let label = format!("relay_trust_store_config.keys[{index}]");
        let key = exact_nested(value, &["key_id", "public_key_b64"], &[], &label)?;
        let key_id = object_string(key, "key_id", &label)?.to_owned();
        validate_identifier(&Value::String(key_id.clone()), &format!("{label}.key_id"))?;
        let public_key_b64 = object_string(key, "public_key_b64", &label)?;
        validate_base64(
            &Value::String(public_key_b64.to_owned()),
            &format!("{label}.public_key_b64"),
            32,
            Some(32),
        )?;
        let public_key: [u8; 32] = base64::engine::general_purpose::STANDARD
            .decode(public_key_b64)
            .expect("validated base64")
            .try_into()
            .expect("validated key length");
        if keys
            .insert(key_id.clone(), PinnedRelayKey { key_id, public_key })
            .is_some()
        {
            return Err(ControllerProtocolError::new(
                "invalid_field",
                "relay_trust_store_config.keys",
            ));
        }
    }
    Ok(RelayTrustStore { keys })
}

impl LeaseInstallValidationContext {
    pub(crate) fn new(verified: VerifiedTranscript) -> Self {
        Self {
            request_id: verified.request_id,
            expected_pairing_id: verified.expected_pairing_id,
            reservation_id: verified.reservation_id,
            remote_control_generation: verified.remote_control_generation,
            relay_epoch: verified.relay_epoch,
            transcript_hash: verified.transcript_hash,
            expected_controller_connection_identity: verified
                .expected_controller_connection_identity,
            negotiated_capabilities: verified.negotiated_capabilities,
            capability_hash: verified.capability_hash,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct LeaseInstallResult {
    pub(crate) message: Value,
    pub(crate) lease: LeaseContext,
}

#[derive(Clone, Debug)]
pub(crate) struct RegistrationValidationContext {
    request: Value,
    sealed_result: Option<Value>,
}

impl RegistrationValidationContext {
    pub(crate) fn new(request: Value, sealed_result: Option<Value>) -> Result<Self> {
        let encoded = serde_json::to_string(&request)
            .map_err(|_| ControllerProtocolError::new("invalid_message", "registration request"))?;
        let validated = validate_controller_message_json(&encoded, B)?;
        if validated.get("type").and_then(Value::as_str) != Some("desktop_pairing_register") {
            return Err(ControllerProtocolError::new(
                "invalid_message",
                "registration request",
            ));
        }
        let sealed_result = if let Some(value) = sealed_result {
            let encoded = serde_json::to_string(&value).map_err(|_| {
                ControllerProtocolError::new("invalid_message", "registration result")
            })?;
            let value = validate_controller_message_json(&encoded, RELAY)?;
            let kind = value.get("type").and_then(Value::as_str);
            if kind != Some("desktop_pairing_registered") && kind != Some("error") {
                return Err(ControllerProtocolError::new(
                    "invalid_message",
                    "registration result",
                ));
            }
            for field in ["registration_request_id", "remote_control_generation"] {
                let Some(actual) = value.get(field) else {
                    return Err(ControllerProtocolError::new(
                        "missing_field",
                        format!("message.{field}"),
                    ));
                };
                if Some(actual) != validated.get(field) {
                    return Err(ControllerProtocolError::new("invalid_message", field));
                }
            }
            Some(value)
        } else {
            None
        };
        Ok(Self {
            request: validated,
            sealed_result,
        })
    }
}

fn registration_comparable(value: &Value) -> Value {
    let mut value = value.clone();
    if let Some(object) = value.as_object_mut() {
        object.remove("request_id");
    }
    value
}

pub(crate) fn validate_controller_registration_message_json(
    input: &str,
    sender_role: SenderRole,
    context: &RegistrationValidationContext,
) -> Result<Value> {
    let candidate: Value = serde_json::from_str(input)
        .map_err(|_| ControllerProtocolError::new("invalid_json", "json"))?;
    if candidate.get("type").and_then(Value::as_str) == Some("desktop_pairing_register")
        && candidate.get("registration_request_id")
            == context.request.get("registration_request_id")
        && registration_comparable(&candidate) != registration_comparable(&context.request)
    {
        return Err(ControllerProtocolError::new(
            "invalid_message",
            "registration idempotency conflict",
        ));
    }
    let value = validate_controller_message_json(input, sender_role)?;
    let kind = value
        .get("type")
        .and_then(Value::as_str)
        .expect("validated type");
    if kind == "desktop_pairing_register" {
        if value.get("registration_request_id") != context.request.get("registration_request_id")
            || registration_comparable(&value) != registration_comparable(&context.request)
        {
            return Err(ControllerProtocolError::new(
                "invalid_message",
                "registration idempotency conflict",
            ));
        }
        return Ok(value);
    }
    if kind != "desktop_pairing_registered" && kind != "error" {
        return Err(ControllerProtocolError::new(
            "invalid_message",
            "registration result",
        ));
    }
    for field in ["registration_request_id", "remote_control_generation"] {
        let Some(actual) = value.get(field) else {
            return Err(ControllerProtocolError::new(
                "missing_field",
                format!("message.{field}"),
            ));
        };
        if Some(actual) != context.request.get(field) {
            return Err(ControllerProtocolError::new("invalid_message", field));
        }
    }
    if let Some(sealed) = &context.sealed_result {
        if registration_comparable(&value) != registration_comparable(sealed) {
            return Err(ControllerProtocolError::new(
                "invalid_message",
                "sealed registration result",
            ));
        }
    }
    Ok(value)
}

fn capability_binding<'a>(
    bindings: &'a [CapabilityBinding],
    kind: &str,
) -> Option<&'a CapabilityBinding> {
    bindings.iter().find(|binding| binding.kind == kind)
}

fn enforce_negotiated_payload(kind: &str, capabilities: &[String]) -> Result<()> {
    let Some(binding) = capability_binding(PAYLOAD_CAPABILITY_BINDINGS, kind) else {
        return Err(ControllerProtocolError::new("unsupported_capability", kind));
    };
    for required in binding.capabilities {
        if !capabilities.iter().any(|capability| capability == required) {
            return Err(ControllerProtocolError::new("unsupported_capability", kind));
        }
    }
    Ok(())
}

fn outer_message_capabilities(kind: &str) -> &'static [&'static str] {
    capability_binding(OUTER_MESSAGE_CAPABILITY_BINDINGS, kind)
        .map(|binding| binding.capabilities)
        .unwrap_or(&[])
}

fn validate_runtime_capabilities(capabilities: &[String]) -> Result<()> {
    if capabilities.len() > CONTROLLER_CAPABILITIES.len() {
        return Err(ControllerProtocolError::new(
            "unsupported_capability",
            "negotiated_capabilities",
        ));
    }
    for (index, capability) in capabilities.iter().enumerate() {
        if validate_identifier(
            &Value::String(capability.clone()),
            "negotiated_capabilities",
        )
        .is_err()
            || !CONTROLLER_CAPABILITIES.contains(&capability.as_str())
            || (index > 0 && capabilities[index - 1].as_str() >= capability.as_str())
        {
            return Err(ControllerProtocolError::new(
                "unsupported_capability",
                "negotiated_capabilities",
            ));
        }
    }
    Ok(())
}

fn require_runtime_lease(context: &RuntimeValidationContext) -> Result<&LeaseContext> {
    context
        .lease
        .as_ref()
        .ok_or_else(|| ControllerProtocolError::new("missing_field", "runtime_context.lease"))
}

pub(crate) fn validate_controller_lease_install_message_json(
    input: &str,
    sender_role: SenderRole,
    context: &LeaseInstallValidationContext,
) -> Result<LeaseInstallResult> {
    validate_runtime_capabilities(&context.negotiated_capabilities)?;
    let message = validate_controller_message_json(input, sender_role)?;
    let kind = message
        .get("type")
        .and_then(Value::as_str)
        .expect("validated type");
    if !LEASE_INSTALL_MESSAGE_TYPES.contains(&kind) {
        return Err(ControllerProtocolError::new(
            "invalid_message",
            "lease_install_message",
        ));
    }
    for required in outer_message_capabilities(kind) {
        if !context
            .negotiated_capabilities
            .iter()
            .any(|capability| capability == required)
        {
            return Err(ControllerProtocolError::new("unsupported_capability", kind));
        }
    }
    let expected = [
        ("request_id", context.request_id.as_str()),
        ("pairing_id", context.expected_pairing_id.as_str()),
        ("reservation_id", context.reservation_id.as_str()),
        (
            "remote_control_generation",
            context.remote_control_generation.as_str(),
        ),
        ("relay_epoch", context.relay_epoch.as_str()),
        ("transcript_hash", context.transcript_hash.as_str()),
        (
            "controller_connection_identity",
            context.expected_controller_connection_identity.as_str(),
        ),
    ];
    for (field, expected) in expected {
        if message.get(field).and_then(Value::as_str) != Some(expected) {
            return Err(ControllerProtocolError::new("stale_context", field));
        }
    }
    let lease = LeaseContext {
        connection_id: object_string(message.as_object().unwrap(), "connection_id", "message")?
            .into(),
        controller_lease_id: object_string(
            message.as_object().unwrap(),
            "controller_lease_id",
            "message",
        )?
        .into(),
        lease_epoch: object_string(message.as_object().unwrap(), "lease_epoch", "message")?.into(),
        relay_epoch: object_string(message.as_object().unwrap(), "relay_epoch", "message")?.into(),
        remote_control_generation: object_string(
            message.as_object().unwrap(),
            "remote_control_generation",
            "message",
        )?
        .into(),
    };
    Ok(LeaseInstallResult { message, lease })
}

pub(crate) fn validate_controller_runtime_message_json(
    input: &str,
    sender_role: SenderRole,
    context: Option<&RuntimeValidationContext>,
) -> Result<Value> {
    let context = context.ok_or_else(|| {
        ControllerProtocolError::new("unsupported_capability", "negotiated_capabilities")
    })?;
    validate_runtime_capabilities(&context.negotiated_capabilities)?;
    let shape = validate_controller_message_json(input, sender_role)?;
    let kind = shape
        .get("type")
        .and_then(Value::as_str)
        .expect("validated type");
    if LEASE_INSTALL_MESSAGE_TYPES.contains(&kind) {
        return Err(ControllerProtocolError::new(
            "missing_field",
            "lease_install_validation_context",
        ));
    }
    let lease = if message_schema(kind)
        .expect("registered schema")
        .required
        .contains(&"controller_lease_id")
    {
        Some(require_runtime_lease(context)?)
    } else {
        context.lease.as_ref()
    };
    let value = if lease.is_some() {
        validate_controller_message_json_in_context(input, sender_role, lease)?
    } else {
        shape
    };
    let kind = value
        .get("type")
        .and_then(Value::as_str)
        .expect("validated type");
    if kind != "controller_capabilities" && kind != "error" {
        for required in outer_message_capabilities(kind) {
            if !context
                .negotiated_capabilities
                .iter()
                .any(|value| value == required)
            {
                return Err(ControllerProtocolError::new("unsupported_capability", kind));
            }
        }
    }
    if kind == "encrypted_frame" {
        enforce_negotiated_payload(
            value
                .get("payload_kind")
                .and_then(Value::as_str)
                .expect("validated payload kind"),
            &context.negotiated_capabilities,
        )?;
    }
    Ok(value)
}

pub(crate) fn validate_controller_runtime_payload_json(
    input: &str,
    sender_role: SenderRole,
    context: Option<&RuntimeValidationContext>,
) -> Result<Value> {
    let context = context.ok_or_else(|| {
        ControllerProtocolError::new("unsupported_capability", "negotiated_capabilities")
    })?;
    validate_runtime_capabilities(&context.negotiated_capabilities)?;
    let shape = validate_controller_payload_json(input, sender_role)?;
    let kind = shape
        .get("type")
        .and_then(Value::as_str)
        .expect("validated type");
    enforce_negotiated_payload(kind, &context.negotiated_capabilities)?;
    let lease = require_runtime_lease(context)?;
    validate_controller_payload_json_in_context(input, sender_role, Some(lease))
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
        if value
            .as_array()
            .is_some_and(|items| items.len() > MAX_TASK_REFERENCES)
        {
            return Err(ControllerProtocolError::new("invalid_field", field));
        }
        validate_string_list(value, field, &[])?;
    } else if field == "sender_role"
        || field == "receiver_role"
        || field == "role_a"
        || field == "role_b"
    {
        validate_enum(value, field, SENDER_ROLES)?;
    } else if field == "client_role" {
        validate_enum(value, field, CLIENT_ROLES)?;
    } else if field == "key_algorithm" {
        validate_enum(value, field, KEY_ALGORITHMS)?;
    } else if field == "signature_algorithm" {
        validate_enum(value, field, SIGNATURE_ALGORITHMS)?;
    } else if field == "direction" {
        validate_enum(value, field, DIRECTIONS)?;
    } else if field == "payload_kind" {
        validate_enum(value, field, CONTROLLER_PAYLOAD_TYPES)?;
    } else if field == "model_source" {
        validate_enum(value, field, MODEL_SOURCES)?;
    } else if field == "mode" {
        validate_enum(value, field, MODES)?;
    } else if field == "task_status" {
        validate_enum(value, field, TASK_STATUSES)?;
    } else if field == "decision" {
        validate_enum(value, field, DECISIONS)?;
    } else if field == "risk_level" {
        validate_enum(value, field, RISK_LEVELS)?;
    } else if field == "event_kind" {
        validate_enum(value, field, EVENT_KINDS)?;
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
    let input_schema = nested_schema("task_input");
    let input = exact_nested(
        task.get("input").expect("task input required by schema"),
        input_schema.required,
        input_schema.optional,
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
        .is_some_and(|items| items.len() > MAX_TASK_REFERENCES)
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
        .is_some_and(|items| items.len() > MAX_TASK_REFERENCES)
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
        validate_enum(mode, "modes", MODES)?;
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
    if items.len() > MAX_DESCRIPTORS {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    let mut ids = HashSet::new();
    for (index, item) in items.iter().enumerate() {
        let label = format!("{field}[{index}]");
        let descriptor_schema = if workspace {
            nested_schema("workspace_descriptor")
        } else if id_field == "tool_id" {
            nested_schema("tool_descriptor")
        } else {
            nested_schema("skill_descriptor")
        };
        let descriptor = exact_nested(
            item,
            descriptor_schema.required,
            descriptor_schema.optional,
            &label,
        )?;
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
    if artifacts.len() > MAX_ARTIFACTS {
        return Err(ControllerProtocolError::new("invalid_field", "artifacts"));
    }
    let mut ids = HashSet::new();
    for (index, item) in artifacts.iter().enumerate() {
        let label = format!("artifacts[{index}]");
        let artifact_schema = nested_schema("artifact_descriptor");
        let artifact = exact_nested(
            item,
            artifact_schema.required,
            artifact_schema.optional,
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
            ARTIFACT_PREVIEW_MODES,
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
    let package_schema = nested_schema("temporary_model_package_v1");
    let package = exact_nested(
        package,
        package_schema.required,
        package_schema.optional,
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
    let binding_schema = nested_schema("temporary_model_binding");
    let binding = exact_nested(
        package.get("binding").expect("binding required"),
        binding_schema.required,
        binding_schema.optional,
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
        let fallback_schema = nested_schema("vision_fallback");
        let fallback = exact_nested(
            fallback,
            fallback_schema.required,
            fallback_schema.optional,
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
    let route_schema = nested_schema("route");
    let route = exact_nested(value, route_schema.required, route_schema.optional, label)?;
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
        WIRE_APIS,
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
    validate_controller_base_url(base_url, &format!("{label}.base_url"))?;
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
        IMAGE_CAPABILITY_OVERRIDES,
    )?;
    if let Some(value) = route.get("provider_kind") {
        validate_enum(value, &format!("{label}.provider_kind"), PROVIDER_KINDS)?;
    }
    if let Some(value) = route.get("vendor") {
        validate_enum(value, &format!("{label}.vendor"), VENDORS)?;
    }
    if let Some(value) = route.get("endpoint_mode") {
        validate_enum(value, &format!("{label}.endpoint_mode"), ENDPOINT_MODES)?;
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
            REASONING_EFFORTS,
        )?;
    }
    Ok(())
}

pub(crate) fn validate_controller_base_url(value: &str, field: &str) -> Result<()> {
    if value.is_empty()
        || value.len() > 2048
        || !value.bytes().all(|byte| (0x21..=0x7e).contains(&byte))
    {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    let rest = value
        .strip_prefix("https://")
        .or_else(|| value.strip_prefix("http://"))
        .ok_or_else(|| ControllerProtocolError::new("invalid_field", field))?;
    let authority_end = rest
        .bytes()
        .position(|byte| matches!(byte, b'/' | b'?' | b'#'))
        .unwrap_or(rest.len());
    let authority = &rest[..authority_end];
    let suffix = &rest[authority_end..];
    if authority.is_empty()
        || authority.contains('@')
        || authority.contains('[')
        || authority.contains(']')
        || suffix.contains('#')
    {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    let (host, port) = authority
        .rsplit_once(':')
        .map_or((authority, None), |(host, port)| (host, Some(port)));
    if !valid_ascii_host(host) {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    if let Some(port) = port {
        if port.is_empty()
            || (port.len() > 1 && port.starts_with('0'))
            || !port.bytes().all(|byte| byte.is_ascii_digit())
            || port.parse::<u16>().map_or(true, |number| number == 0)
        {
            return Err(ControllerProtocolError::new("invalid_field", field));
        }
    }
    if !suffix.is_empty() && !suffix.starts_with('/') && !suffix.starts_with('?') {
        return Err(ControllerProtocolError::new("invalid_field", field));
    }
    let bytes = suffix.as_bytes();
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index] == b'%' {
            if index + 2 >= bytes.len()
                || !bytes[index + 1].is_ascii_digit() && !(b'A'..=b'F').contains(&bytes[index + 1])
                || !bytes[index + 2].is_ascii_digit() && !(b'A'..=b'F').contains(&bytes[index + 2])
            {
                return Err(ControllerProtocolError::new("invalid_field", field));
            }
            index += 3;
            continue;
        }
        if !b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~!$&'()*+,;=:@/?"
            .contains(&bytes[index])
        {
            return Err(ControllerProtocolError::new("invalid_field", field));
        }
        index += 1;
    }
    Ok(())
}

fn valid_ascii_host(host: &str) -> bool {
    if host.is_empty()
        || host.len() > 253
        || host != host.to_ascii_lowercase()
        || host.ends_with('.')
    {
        return false;
    }
    if host
        .bytes()
        .all(|byte| byte.is_ascii_digit() || byte == b'.')
    {
        let parts = host.split('.').collect::<Vec<_>>();
        return parts.len() == 4
            && parts.iter().all(|part| {
                !part.is_empty()
                    && (part.len() == 1 || !part.starts_with('0'))
                    && part.parse::<u8>().is_ok()
            });
    }
    host.split('.').all(|label| {
        !label.is_empty()
            && label.len() <= 63
            && label
                .bytes()
                .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
            && label
                .as_bytes()
                .first()
                .is_some_and(u8::is_ascii_alphanumeric)
            && label
                .as_bytes()
                .last()
                .is_some_and(u8::is_ascii_alphanumeric)
    })
}

fn validate_credential(value: &Value, label: &str) -> Result<()> {
    let credential_schema = nested_schema("credential");
    let credential = exact_nested(
        value,
        credential_schema.required,
        credential_schema.optional,
        label,
    )?;
    validate_enum(
        credential.get("kind").expect("credential kind required"),
        &format!("{label}.kind"),
        CREDENTIAL_KINDS,
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
    "negotiated_capabilities",
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
    let negotiated_value = transcript
        .get("negotiated_capabilities")
        .expect("negotiated capabilities required");
    validate_string_list(
        negotiated_value,
        "transcript.negotiated_capabilities",
        CONTROLLER_CAPABILITIES,
    )?;
    let negotiated = negotiated_value
        .as_array()
        .expect("capability list validated")
        .iter()
        .map(|value| {
            value
                .as_str()
                .expect("capability string validated")
                .to_owned()
        })
        .collect::<Vec<_>>();
    if negotiated.is_empty() {
        return Err(ControllerProtocolError::new(
            "unsupported_capability",
            "transcript.negotiated_capabilities",
        ));
    }
    if negotiated.windows(2).any(|items| items[0] >= items[1]) {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            "transcript.negotiated_capabilities",
        ));
    }
    validate_hex_64(
        transcript
            .get("capability_hash")
            .expect("capability hash required"),
        "transcript.capability_hash",
    )?;
    if controller_capability_hash(&negotiated)?
        != object_string(transcript, "capability_hash", "transcript")?
    {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            "transcript.capability_hash",
        ));
    }
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
    for field in &TRANSCRIPT_FIELDS[..12] {
        let value = if *field == "protocol_version" {
            CONTROLLER_PROTOCOL_VERSION.to_string()
        } else {
            object_string(transcript, field, "transcript")?.to_owned()
        };
        length_prefix_text(&mut output, &value);
    }
    length_prefix_text(&mut output, &negotiated.len().to_string());
    for capability in &negotiated {
        length_prefix_text(&mut output, capability);
    }
    for field in &TRANSCRIPT_FIELDS[13..] {
        length_prefix_text(&mut output, object_string(transcript, field, "transcript")?);
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

pub(crate) fn negotiate_controller_capabilities(
    capabilities_a: &[String],
    capabilities_b: &[String],
) -> Result<Vec<String>> {
    for (field, capabilities) in [
        ("capabilities_a", capabilities_a),
        ("capabilities_b", capabilities_b),
    ] {
        let value = Value::Array(capabilities.iter().cloned().map(Value::String).collect());
        validate_string_list(&value, field, CONTROLLER_CAPABILITIES)?;
    }
    let supported_by_b = capabilities_b.iter().collect::<HashSet<_>>();
    let mut negotiated = capabilities_a
        .iter()
        .filter(|capability| supported_by_b.contains(capability))
        .cloned()
        .collect::<Vec<_>>();
    negotiated.sort();
    if negotiated.is_empty() {
        return Err(ControllerProtocolError::new(
            "unsupported_capability",
            "negotiated_capabilities",
        ));
    }
    Ok(negotiated)
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

/// Produces the only constructible transcript trust token after verification
/// with an atomic key-ID/public-key binding loaded from enrollment trust data.
pub(crate) fn verify_controller_pairing_transcript(
    input: &str,
    pinned_relay_key: &PinnedRelayKey,
) -> Result<VerifiedTranscript> {
    let value = validate_controller_message_json(input, RELAY)?;
    let message = as_object(&value, "message")?;
    if message.get("type").and_then(Value::as_str) != Some("controller_pairing_reserved") {
        return Err(ControllerProtocolError::new(
            "invalid_message",
            "verified_transcript",
        ));
    }
    if object_string(message, "relay_signing_key_id", "message")? != pinned_relay_key.key_id {
        return Err(ControllerProtocolError::new(
            "stale_context",
            "relay_signing_key_id",
        ));
    }
    let transcript = message
        .get("transcript")
        .expect("reserved transcript required");
    let preimage = build_pairing_transcript_preimage(transcript)?;
    let signature = base64::engine::general_purpose::STANDARD
        .decode(object_string(message, "relay_signature_b64", "message")?)
        .map_err(|_| ControllerProtocolError::new("invalid_field", "relay_signature_b64"))?;
    if signature.len() != 64
        || UnparsedPublicKey::new(&ED25519, pinned_relay_key.public_key)
            .verify(&preimage, &signature)
            .is_err()
    {
        return Err(ControllerProtocolError::new(
            "invalid_field",
            "relay_signature_b64",
        ));
    }
    let transcript = as_object(transcript, "transcript")?;
    Ok(VerifiedTranscript {
        request_id: object_string(message, "request_id", "message")?.to_owned(),
        expected_pairing_id: object_string(transcript, "pairing_id", "transcript")?.to_owned(),
        reservation_id: object_string(transcript, "reservation_id", "transcript")?.to_owned(),
        remote_control_generation: object_string(
            transcript,
            "remote_control_generation",
            "transcript",
        )?
        .to_owned(),
        relay_epoch: object_string(transcript, "relay_epoch", "transcript")?.to_owned(),
        transcript_hash: object_string(message, "transcript_hash", "message")?.to_owned(),
        expected_controller_connection_identity: object_string(
            transcript,
            "controller_connection_identity",
            "transcript",
        )?
        .to_owned(),
        negotiated_capabilities: serde_json::from_value(
            transcript
                .get("negotiated_capabilities")
                .expect("validated capabilities required")
                .clone(),
        )
        .expect("validated capabilities are strings"),
        capability_hash: object_string(transcript, "capability_hash", "transcript")?.to_owned(),
    })
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
    let liveness_schema = nested_schema("controller_liveness_a");
    let object = exact_nested(
        &value,
        liveness_schema.required,
        liveness_schema.optional,
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
    classify_monotonic_sequence(
        object_string(object, "heartbeat_sequence_a", "liveness_payload")?,
        last_sequence,
        "heartbeat_sequence_a",
    )?;
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
    last_heartbeat_sequence: u64,
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
    classify_liveness_attestation_sequences(
        object,
        last_heartbeat_sequence,
        last_attestation_sequence,
    )?;
    Ok(value)
}

fn classify_liveness_attestation_sequences(
    object: &Map<String, Value>,
    last_heartbeat_sequence: u64,
    last_attestation_sequence: u64,
) -> Result<DeadlineUpdate> {
    let heartbeat = classify_monotonic_sequence(
        object_string(object, "heartbeat_sequence_a", "attestation")?,
        last_heartbeat_sequence,
        "heartbeat_sequence_a",
    )?;
    let attestation = object_string(object, "attestation_sequence", "attestation")?
        .parse::<u64>()
        .map_err(|_| ControllerProtocolError::new("invalid_field", "attestation_sequence"))?;
    if attestation <= last_attestation_sequence {
        return Err(ControllerProtocolError::new(
            "replay",
            "attestation_sequence",
        ));
    }
    Ok(if heartbeat == SequenceDisposition::Advance {
        DeadlineUpdate::Replace
    } else {
        DeadlineUpdate::TightenOnly
    })
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

    fn schema_manifest(types: &[&str], lookup: fn(&str) -> Option<Schema>) -> Value {
        let mut output = Map::new();
        for kind in types {
            let schema = lookup(kind).expect("registered schema");
            output.insert(
                (*kind).into(),
                json!({
                    "required": schema.required,
                    "optional": schema.optional,
                    "roles": schema.roles.iter().map(|role| role.wire()).collect::<Vec<_>>(),
                }),
            );
        }
        Value::Object(output)
    }

    fn capability_manifest(bindings: &[CapabilityBinding], scalar: bool) -> Value {
        let mut output = Map::new();
        for binding in bindings {
            output.insert(
                binding.kind.into(),
                if scalar {
                    json!(binding.capabilities[0])
                } else {
                    json!(binding.capabilities)
                },
            );
        }
        Value::Object(output)
    }

    fn nested_schema_manifest() -> Value {
        let mut output = Map::new();
        for (name, schema) in NESTED_SCHEMAS {
            output.insert(
                (*name).into(),
                json!({"required": schema.required, "optional": schema.optional}),
            );
        }
        Value::Object(output)
    }

    fn runtime_pre_lease_messages() -> Vec<&'static str> {
        CONTROLLER_MESSAGE_TYPES
            .iter()
            .copied()
            .filter(|kind| {
                !message_schema(kind)
                    .expect("registered schema")
                    .required
                    .contains(&"controller_lease_id")
            })
            .collect()
    }

    fn rust_contract_manifest() -> Value {
        json!({
            "protocol_version": CONTROLLER_PROTOCOL_VERSION,
            "subprotocol": "pinvou-desktop-controller-v1",
            "integer_wire_format": "canonical-u64-decimal-string",
            "limits": {
                "max_control_message_bytes": MAX_CONTROL_MESSAGE_BYTES,
                "max_encrypted_frame_bytes": MAX_ENCRYPTED_FRAME_BYTES,
                "max_decrypted_payload_bytes": MAX_DECRYPTED_PAYLOAD_BYTES,
                "max_transfer_chunk_bytes": MAX_TRANSFER_CHUNK_BYTES,
                "max_task_input_text_bytes": MAX_TASK_INPUT_TEXT_BYTES,
                "max_temporary_model_package_bytes": MAX_TEMPORARY_MODEL_PACKAGE_BYTES,
                "max_id_chars": MAX_ID_CHARS
            },
            "capabilities": CONTROLLER_CAPABILITIES,
            "message_types": CONTROLLER_MESSAGE_TYPES,
            "payload_types": CONTROLLER_PAYLOAD_TYPES,
            "task_statuses": TASK_STATUSES,
            "error_codes": CONTROLLER_ERROR_CODES,
            "message_schemas": schema_manifest(CONTROLLER_MESSAGE_TYPES, message_schema),
            "payload_schemas": schema_manifest(CONTROLLER_PAYLOAD_TYPES, payload_schema),
            "enums": {
                "sender_role":SENDER_ROLES, "client_role":CLIENT_ROLES,
                "key_algorithm":KEY_ALGORITHMS, "signature_algorithm":SIGNATURE_ALGORITHMS, "direction":DIRECTIONS,
                "model_source":MODEL_SOURCES, "mode":MODES, "task_status":TASK_STATUSES,
                "decision":DECISIONS, "risk_level":RISK_LEVELS,
                "event_kind":EVENT_KINDS, "preset":PRESETS,
                "wire_api":WIRE_APIS, "provider_kind":PROVIDER_KINDS, "vendor":VENDORS,
                "endpoint_mode":ENDPOINT_MODES, "reasoning_effort":REASONING_EFFORTS,
                "image_capability_override":IMAGE_CAPABILITY_OVERRIDES, "artifact_preview_mode":ARTIFACT_PREVIEW_MODES,
                "credential_kind":CREDENTIAL_KINDS
            },
            "payload_capabilities": capability_manifest(PAYLOAD_CAPABILITY_BINDINGS, true),
            "outer_message_capabilities": capability_manifest(OUTER_MESSAGE_CAPABILITY_BINDINGS, false),
            "transcript_fields":TRANSCRIPT_FIELDS,
            "route_required":ROUTE_REQUIRED,
            "route_optional":ROUTE_OPTIONAL,
            "nested_schemas": nested_schema_manifest(),
            "lease_install_messages": LEASE_INSTALL_MESSAGE_TYPES,
            "lease_install_context_required": LEASE_INSTALL_CONTEXT_FIELDS,
            "runtime_pre_lease_messages": runtime_pre_lease_messages(),
            "collection_limits":{"task_attachment_ids":MAX_TASK_REFERENCES,"task_skill_ids":MAX_TASK_REFERENCES,"descriptors":MAX_DESCRIPTORS,"artifacts":MAX_ARTIFACTS,"capabilities":CONTROLLER_CAPABILITIES.len()},
            "base_url_grammar":"lowercase-http(s)-dns-or-ipv4-port1-65535-no-userinfo-ipv6-fragment-uppercase-percent",
            "liveness_sequence_policy":{"heartbeat":{"lower":"replay","equal":"same_sequence_tighten","greater":"advance"},"attestation":{"lower":"replay","equal":"replay","greater":"advance"}},
            "validation_modes":{"shape_only":"schema-role-lease-shape-without-capability-state","runtime":"negotiated-capabilities-required-for-state-machine-messages","lease_install":"trusted-handshake-bindings-produce-new-lease","registration":"independent-generation-registration-idempotency-context"},
            "runtime_capability_context":{"order":"bytewise-strict-ascending","invalid_error":{"code":"unsupported_capability","field":"negotiated_capabilities"}},
            "verified_transcript":{"constructor":"full-canonical-transcript-hash-and-pinned-relay-ed25519-signature","lease_install_derivation":"opaque-verified-transcript-only"},
            "relay_trust_store":{"sources":["official_enrollment","self_hosted_enrollment"],"max_keys":MAX_RELAY_TRUST_KEYS,"key_binding":"opaque-atomic-key-id-plus-ed25519-public-key","activation_owner":"DEV-01-or-DEV-05"}
        })
    }

    fn canonical_json(value: &Value) -> String {
        match value {
            Value::Array(values) => format!(
                "[{}]",
                values
                    .iter()
                    .map(canonical_json)
                    .collect::<Vec<_>>()
                    .join(",")
            ),
            Value::Object(values) => {
                let mut keys = values.keys().collect::<Vec<_>>();
                keys.sort();
                format!(
                    "{{{}}}",
                    keys.into_iter()
                        .map(|key| format!(
                            "{}:{}",
                            serde_json::to_string(key).unwrap(),
                            canonical_json(&values[key])
                        ))
                        .collect::<Vec<_>>()
                        .join(",")
                )
            }
            _ => serde_json::to_string(value).unwrap(),
        }
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

    fn runtime(capabilities: &[&str]) -> RuntimeValidationContext {
        RuntimeValidationContext {
            lease: Some(context()),
            negotiated_capabilities: capabilities.iter().map(|value| (*value).into()).collect(),
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
        let mut fixture = fixture();
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
        assert_eq!(
            CONTROLLER_CONTRACT_SHA256,
            fixture["contract"]["sha256"].as_str().unwrap()
        );
        fixture["contract"]
            .as_object_mut()
            .unwrap()
            .remove("sha256");
        let combined = json!({"contract": rust_contract_manifest(), "fixture": fixture.clone()});
        let rust_hash = sha256_hex(canonical_json(&combined).as_bytes());
        assert_eq!(rust_hash, CONTROLLER_CONTRACT_SHA256);

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
    fn pairing_registration_freezes_generation_scoped_idempotency_key() {
        let fixture = fixture();
        let request = &fixture["pairing_registration_vector"]["request"];
        let result = &fixture["pairing_registration_vector"]["result"];
        let retry_request = &fixture["pairing_registration_vector"]["retry_request"];
        let retry_result = &fixture["pairing_registration_vector"]["retry_result"];
        let failure_result = &fixture["pairing_registration_vector"]["failure_result"];
        let failure_retry_result = &fixture["pairing_registration_vector"]["failure_retry_result"];
        validate_controller_message_json(&serde_json::to_string(request).unwrap(), B).unwrap();
        validate_controller_message_json(&serde_json::to_string(result).unwrap(), RELAY).unwrap();
        validate_controller_message_json(&serde_json::to_string(retry_request).unwrap(), B)
            .unwrap();
        validate_controller_message_json(&serde_json::to_string(retry_result).unwrap(), RELAY)
            .unwrap();
        validate_controller_message_json(&serde_json::to_string(failure_result).unwrap(), RELAY)
            .unwrap();
        validate_controller_message_json(
            &serde_json::to_string(failure_retry_result).unwrap(),
            RELAY,
        )
        .unwrap();
        assert_eq!(
            request["registration_request_id"],
            result["registration_request_id"]
        );
        assert_eq!(
            request["remote_control_generation"],
            result["remote_control_generation"]
        );
        let mut sealed_result = result.as_object().unwrap().clone();
        let mut sealed_retry_result = retry_result.as_object().unwrap().clone();
        sealed_result.remove("request_id");
        sealed_retry_result.remove("request_id");
        assert_eq!(sealed_retry_result, sealed_result);
        let mut sealed_failure = failure_result.as_object().unwrap().clone();
        let mut sealed_failure_retry = failure_retry_result.as_object().unwrap().clone();
        sealed_failure.remove("request_id");
        sealed_failure_retry.remove("request_id");
        assert_eq!(sealed_failure_retry, sealed_failure);
        let mut missing = request.clone();
        missing
            .as_object_mut()
            .unwrap()
            .remove("registration_request_id");
        assert_eq!(
            validate_controller_message_json(&serde_json::to_string(&missing).unwrap(), B)
                .unwrap_err()
                .code,
            "missing_field"
        );
        let success_context =
            RegistrationValidationContext::new(request.clone(), Some(result.clone())).unwrap();
        validate_controller_registration_message_json(
            &serde_json::to_string(retry_request).unwrap(),
            B,
            &success_context,
        )
        .unwrap();
        validate_controller_registration_message_json(
            &serde_json::to_string(retry_result).unwrap(),
            RELAY,
            &success_context,
        )
        .unwrap();
        for field in fixture["pairing_registration_vector"]["conflict_fields"]
            .as_array()
            .unwrap()
            .iter()
            .map(|value| value.as_str().unwrap())
        {
            let mut conflict = retry_request.clone();
            conflict[field] = if field == "capabilities" {
                json!(["pairing_v1"])
            } else {
                json!(format!("{}-other", conflict[field].as_str().unwrap()))
            };
            assert_eq!(
                validate_controller_registration_message_json(
                    &serde_json::to_string(&conflict).unwrap(),
                    B,
                    &success_context
                )
                .unwrap_err()
                .code,
                fixture["pairing_registration_vector"]["conflict_expected_error"]
                    .as_str()
                    .unwrap()
            );
        }
        let failure_context =
            RegistrationValidationContext::new(request.clone(), Some(failure_result.clone()))
                .unwrap();
        validate_controller_registration_message_json(
            &serde_json::to_string(failure_retry_result).unwrap(),
            RELAY,
            &failure_context,
        )
        .unwrap();
        for field in ["registration_request_id", "remote_control_generation"] {
            let mut missing = failure_retry_result.clone();
            missing.as_object_mut().unwrap().remove(field);
            assert_eq!(
                validate_controller_registration_message_json(
                    &serde_json::to_string(&missing).unwrap(),
                    RELAY,
                    &failure_context
                )
                .unwrap_err()
                .code,
                "missing_field"
            );
        }
    }

    #[test]
    fn legacy_web_v2_fixture_matches_live_relay_envelopes() {
        let fixture = fixture();
        let legacy = &fixture["legacy_web_v2"];
        assert_eq!(
            legacy["desktop_endpoint_register"],
            json!({"v":2,"type":"desktop_endpoint_register","endpoint_id":"endpoint_fixture","access_token":"fixture-access-token","desktop_secret":"fixture-desktop-secret"})
        );
        assert_eq!(
            legacy["desktop_endpoint_registered"],
            json!({"v":2,"type":"desktop_endpoint_registered","endpoint_id":"endpoint_fixture","web_client_connected":false,"lease_id":null})
        );
        assert_eq!(
            legacy["web_client_join"],
            json!({"v":2,"type":"web_client_join","endpoint_id":"endpoint_fixture","access_token":"fixture-access-token","stream_epoch":"stream-fixture","after_seq":7})
        );
        assert_eq!(
            legacy["web_client_joined"],
            json!({"v":2,"type":"web_client_joined","endpoint_id":"endpoint_fixture","lease_id":"lease_fixture","desktop_connected":true})
        );
        assert_eq!(legacy["conflict"]["message"]["type"], "error");
        assert_eq!(legacy["conflict"]["message"]["code"], "endpoint_not_found");
        assert_eq!(legacy["conflict"]["closeCode"], 1008);
        for state in [
            "pairing_reserved",
            "handshake_pending",
            "controller_connected",
            "reconnecting",
            "resume_provisional",
            "resume_activated_pending_liveness",
        ] {
            assert_eq!(
                legacy["controller_conflict_expectations"][state], legacy["conflict"],
                "{state}"
            );
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
        let subset = &fixture["capability_negotiation_vectors"][0];
        let subset_a: Vec<String> =
            serde_json::from_value(subset["capabilities_a"].clone()).unwrap();
        let subset_b: Vec<String> =
            serde_json::from_value(subset["capabilities_b"].clone()).unwrap();
        let negotiated = negotiate_controller_capabilities(&subset_a, &subset_b).unwrap();
        assert_eq!(
            serde_json::to_value(&negotiated).unwrap(),
            subset["negotiated"]
        );
        assert_eq!(
            controller_capability_hash(&negotiated).unwrap(),
            subset["capability_hash"].as_str().unwrap()
        );
        let empty = &fixture["capability_negotiation_vectors"][1];
        let empty_a: Vec<String> = serde_json::from_value(empty["capabilities_a"].clone()).unwrap();
        let empty_b: Vec<String> = serde_json::from_value(empty["capabilities_b"].clone()).unwrap();
        assert_eq!(
            negotiate_controller_capabilities(&empty_a, &empty_b)
                .unwrap_err()
                .code,
            empty["expected_error"].as_str().unwrap()
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
        let negotiated: Vec<String> =
            serde_json::from_value(transcript["negotiated_capabilities"].clone()).unwrap();
        assert_eq!(
            controller_capability_hash(&negotiated).unwrap(),
            transcript["capability_hash"].as_str().unwrap()
        );
        let mut unsorted = transcript.clone();
        unsorted["negotiated_capabilities"] = json!(["task_v1", "pairing_v1"]);
        unsorted["capability_hash"] =
            json!(controller_capability_hash(&["task_v1".into(), "pairing_v1".into()]).unwrap());
        assert_eq!(
            build_pairing_transcript_preimage(&unsorted)
                .unwrap_err()
                .code,
            "invalid_field"
        );
        assert_eq!(
            validate_controller_runtime_payload_json(
                &serde_json::to_string(&fixture["task_submit_vectors"]["controlled"]).unwrap(),
                A,
                Some(&runtime(&["pairing_v1"])),
            )
            .unwrap_err()
            .code,
            "unsupported_capability"
        );
    }

    #[test]
    fn runtime_capabilities_gate_outer_and_inner_messages() {
        let fixture = fixture();
        let capabilities_message = json!({"v":1,"type":"controller_capabilities","request_id":"request-0001","client_role":"desktop_controller","sender_role":"A","protocol_version":1,"capabilities":["pairing_v1"]});
        let error_message = json!({"v":1,"type":"error","request_id":"request-0001","code":"pairing_code_invalid","message":"连接码无效或已失效"});
        let frame = json!({
            "v":1,"type":"encrypted_frame","connection_id":"connection-0001",
            "controller_lease_id":"lease-0001","lease_epoch":"1","relay_epoch":"epoch-test-1",
            "remote_control_generation":"generation-0001","frame_sequence":"1","direction":"a_to_b",
            "payload_kind":"task_submit","ciphertext_b64":"AQID","ciphertext_bytes":"3"
        });
        let liveness = json!({
            "v":1,"type":"controller_liveness","connection_id":"connection-0001","controller_lease_id":"lease-0001",
            "lease_epoch":"1","relay_epoch":"epoch-test-1","remote_control_generation":"generation-0001",
            "sender_role":"A","liveness_epoch":"live-a-1","heartbeat_sequence":"1"
        });
        let receipt = json!({"v":1,"type":"finished_receipt_commit","request_id":"request-1","pairing_id":"pairing-0001","reservation_id":"reservation-0001","remote_control_generation":"generation-0001","relay_epoch":"epoch-test-1","transcript_hash":"0000000000000000000000000000000000000000000000000000000000000000","sender_role":"A","commitment":"0000000000000000000000000000000000000000000000000000000000000000"});
        let resume = json!({"v":1,"type":"controller_resume_prepare","request_id":"request-1","connection_id":"connection-0001","controller_lease_id":"lease-0001","lease_epoch":"1","relay_epoch":"epoch-test-1","remote_control_generation":"generation-0001","sender_role":"A","resume_token_b64":"AQID","transport_challenge_b64":"AAAAAAAAAAAAAAAAAAAAAA==","original_deadline_ms":"30000"});
        validate_controller_message_json(&serde_json::to_string(&capabilities_message).unwrap(), A)
            .unwrap();
        validate_controller_message_json(&serde_json::to_string(&error_message).unwrap(), RELAY)
            .unwrap();
        for case in fixture["runtime_capability_cases"].as_array().unwrap() {
            let kind = case["type"].as_str().unwrap();
            let capabilities: Option<Vec<String>> = case
                .get("capabilities")
                .map(|value| serde_json::from_value(value.clone()).unwrap());
            let runtime_context = capabilities.as_ref().map(|values| {
                let mut lease = context();
                if let Some(overrides) = case.get("lease_overrides").and_then(Value::as_object) {
                    if let Some(value) =
                        overrides.get("controller_lease_id").and_then(Value::as_str)
                    {
                        lease.controller_lease_id = value.into();
                    }
                }
                RuntimeValidationContext {
                    lease: if case.get("omit_lease").and_then(Value::as_bool) == Some(true) {
                        None
                    } else {
                        Some(lease)
                    },
                    negotiated_capabilities: values.clone(),
                }
            });
            let error = if case["target"] == "payload" {
                validate_controller_runtime_payload_json(
                    &serde_json::to_string(&fixture["task_submit_vectors"]["controlled"]).unwrap(),
                    A,
                    runtime_context.as_ref(),
                )
                .unwrap_err()
            } else {
                let value = match kind {
                    "controller_capabilities" => &capabilities_message,
                    "error" => &error_message,
                    "desktop_pairing_register" => {
                        &fixture["pairing_registration_vector"]["request"]
                    }
                    "controller_joined" => &fixture["lease_install_vector"]["message"],
                    "finished_receipt_commit" => &receipt,
                    "controller_resume_prepare" => &resume,
                    "encrypted_frame" => &frame,
                    _ => &liveness,
                };
                validate_controller_runtime_message_json(
                    &serde_json::to_string(value).unwrap(),
                    role(case["sender_role"].as_str().unwrap()),
                    runtime_context.as_ref(),
                )
                .unwrap_err()
            };
            assert_eq!(
                error.code,
                case["expected_error"].as_str().unwrap(),
                "{}",
                case["name"].as_str().unwrap()
            );
            if let Some(expected_field) = case.get("expected_field").and_then(Value::as_str) {
                assert_eq!(error.field, expected_field, "{}", case["name"]);
            }
        }
        for capabilities in [&["task_v1"][..], &["encrypted_frames_v1"][..]] {
            assert_eq!(
                validate_controller_runtime_message_json(
                    &serde_json::to_string(&frame).unwrap(),
                    A,
                    Some(&runtime(capabilities))
                )
                .unwrap_err()
                .code,
                "unsupported_capability"
            );
        }
        validate_controller_runtime_message_json(
            &serde_json::to_string(&frame).unwrap(),
            A,
            Some(&runtime(&["encrypted_frames_v1", "task_v1"])),
        )
        .unwrap();
        let payload = &fixture["task_submit_vectors"]["controlled"];
        assert_eq!(
            validate_controller_runtime_payload_json(
                &serde_json::to_string(payload).unwrap(),
                A,
                Some(&runtime(&["pairing_v1"]))
            )
            .unwrap_err()
            .code,
            "unsupported_capability"
        );
        validate_controller_runtime_payload_json(
            &serde_json::to_string(payload).unwrap(),
            A,
            Some(&runtime(&["task_v1"])),
        )
        .unwrap();
        validate_controller_runtime_message_json(
            &serde_json::to_string(&capabilities_message).unwrap(),
            A,
            Some(&RuntimeValidationContext {
                lease: None,
                negotiated_capabilities: Vec::new(),
            }),
        )
        .unwrap();
        validate_controller_runtime_message_json(
            &serde_json::to_string(&error_message).unwrap(),
            RELAY,
            Some(&RuntimeValidationContext {
                lease: None,
                negotiated_capabilities: Vec::new(),
            }),
        )
        .unwrap();
        for item in fixture["invalid_runtime_capability_contexts"]
            .as_array()
            .unwrap()
        {
            let capabilities: Vec<String> =
                serde_json::from_value(item["capabilities"].clone()).unwrap();
            let error = validate_controller_runtime_message_json(
                &serde_json::to_string(&capabilities_message).unwrap(),
                A,
                Some(&RuntimeValidationContext {
                    lease: None,
                    negotiated_capabilities: capabilities,
                }),
            )
            .unwrap_err();
            assert_eq!(error.code, item["expected_error"].as_str().unwrap());
            assert_eq!(error.field, item["expected_field"].as_str().unwrap());
        }
        let pre_lease = &fixture["runtime_pre_lease_case"];
        let capabilities: Vec<String> =
            serde_json::from_value(pre_lease["capabilities"].clone()).unwrap();
        validate_controller_runtime_message_json(
            &serde_json::to_string(&fixture["pairing_registration_vector"]["request"]).unwrap(),
            B,
            Some(&RuntimeValidationContext {
                lease: None,
                negotiated_capabilities: capabilities,
            }),
        )
        .unwrap();
    }

    #[test]
    fn controller_joined_installs_lease_from_trusted_handshake_expectations() {
        let fixture = fixture();
        let vector = &fixture["lease_install_vector"];
        let verification = &vector["verification_input"];
        let trust_store = load_relay_trust_store_from_enrollment_config(
            &fixture["relay_trust_store_vector"]["trusted_config"],
        )
        .unwrap();
        for invalid in fixture["relay_trust_store_vector"]["invalid_configs"]
            .as_array()
            .unwrap()
        {
            let error =
                load_relay_trust_store_from_enrollment_config(&invalid["config"]).unwrap_err();
            assert_eq!(error.code, invalid["expected_error"].as_str().unwrap());
            assert_eq!(error.field, invalid["expected_field"].as_str().unwrap());
        }
        let pinned_key = trust_store
            .pinned_key(
                verification["expected_relay_signing_key_id"]
                    .as_str()
                    .unwrap(),
            )
            .unwrap();
        let verify_input = |message: &Value| {
            verify_controller_pairing_transcript(
                &serde_json::to_string(message).unwrap(),
                pinned_key,
            )
        };
        let verified = verify_input(&verification["reserved_message"]).unwrap();
        assert_eq!(
            verified.negotiated_capabilities,
            serde_json::from_value::<Vec<String>>(
                fixture["transcript_vector"]["transcript"]["negotiated_capabilities"].clone()
            )
            .unwrap()
        );
        assert_eq!(
            verified.capability_hash,
            fixture["transcript_vector"]["transcript"]["capability_hash"]
                .as_str()
                .unwrap()
        );
        let install_context = LeaseInstallValidationContext::new(verified);
        let mut wrong_key_message = verification["reserved_message"].clone();
        wrong_key_message["relay_signing_key_id"] = json!("relay-key-other");
        let wrong_key_id = verify_input(&wrong_key_message).unwrap_err();
        assert_eq!(wrong_key_id.code, "stale_context");
        assert_eq!(wrong_key_id.field, "relay_signing_key_id");
        let attacker_store = load_relay_trust_store_from_enrollment_config(
            &fixture["relay_trust_store_vector"]["attacker_same_id_config"],
        )
        .unwrap();
        let attacker_key = attacker_store
            .pinned_key(
                verification["expected_relay_signing_key_id"]
                    .as_str()
                    .unwrap(),
            )
            .unwrap();
        let attacker_error = verify_controller_pairing_transcript(
            &serde_json::to_string(&verification["reserved_message"]).unwrap(),
            attacker_key,
        )
        .unwrap_err();
        assert_eq!(attacker_error.code, "invalid_field");
        assert_eq!(attacker_error.field, "relay_signature_b64");
        let result = validate_controller_lease_install_message_json(
            &serde_json::to_string(&vector["message"]).unwrap(),
            RELAY,
            &install_context,
        )
        .unwrap();
        assert_eq!(result.message, vector["message"]);
        assert_eq!(
            json!({
                "connection_id": result.lease.connection_id,
                "controller_lease_id": result.lease.controller_lease_id,
                "lease_epoch": result.lease.lease_epoch,
                "relay_epoch": result.lease.relay_epoch,
                "remote_control_generation": result.lease.remote_control_generation,
            }),
            vector["expected_lease"]
        );

        for field in vector["mismatch_fields"]
            .as_array()
            .unwrap()
            .iter()
            .map(|value| value.as_str().unwrap())
        {
            let mut message = vector["message"].clone();
            message[field] = if field == "transcript_hash" {
                json!("0000000000000000000000000000000000000000000000000000000000000000")
            } else {
                json!(format!("{field}-other"))
            };
            let error = validate_controller_lease_install_message_json(
                &serde_json::to_string(&message).unwrap(),
                RELAY,
                &install_context,
            )
            .unwrap_err();
            assert_eq!(error.code, vector["expected_error"].as_str().unwrap());
            assert_eq!(error.field, field);
        }
        for negative in vector["verified_transcript_negative_inputs"]
            .as_array()
            .unwrap()
        {
            let mut message = verification["reserved_message"].clone();
            message["transcript"] = negative["transcript"].clone();
            message["transcript_hash"] = negative["transcript_hash"].clone();
            message["relay_signature_b64"] = negative["relay_signature_b64"].clone();
            let error = verify_input(&message).unwrap_err();
            assert_eq!(error.code, negative["expected_error"].as_str().unwrap());
            assert_eq!(error.field, negative["expected_field"].as_str().unwrap());
        }
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
    fn temporary_model_urls_match_javascript_grammar() {
        let fixture = fixture();
        for value in fixture["base_url_vectors"]["valid"].as_array().unwrap() {
            let url = value.as_str().unwrap();
            validate_controller_base_url(url, "base_url").unwrap();
        }
        for value in fixture["base_url_vectors"]["invalid"].as_array().unwrap() {
            assert_eq!(
                validate_controller_base_url(value.as_str().unwrap(), "base_url")
                    .unwrap_err()
                    .code,
                "invalid_field"
            );
        }
        let exact_boundary = format!("https://a/{}", "x".repeat(2038));
        assert_eq!(exact_boundary.len(), 2048);
        validate_controller_base_url(&exact_boundary, "base_url").unwrap();
        assert_eq!(
            validate_controller_base_url(&(exact_boundary + "x"), "base_url")
                .unwrap_err()
                .code,
            "invalid_field"
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

        let mut same = direct["inner"].clone();
        same["heartbeat_sequence_a"] = json!("1");
        validate_controller_liveness_payload_json(
            &serde_json::to_string(&same).unwrap(),
            &context(),
            "live-a-1",
            1,
            "challenge-0001",
            "AAAAAAAAAAAAAAAAAAAAAA==",
        )
        .unwrap();
        let mut regression = direct["inner"].clone();
        regression["heartbeat_sequence_a"] = json!("0");
        assert_eq!(
            validate_controller_liveness_payload_json(
                &serde_json::to_string(&regression).unwrap(),
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
            1,
            0,
            "challenge-0001",
            "AAAAAAAAAAAAAAAAAAAAAA==",
        )
        .unwrap();
        let message = attestation["message"].as_object().unwrap();
        assert_eq!(
            classify_liveness_attestation_sequences(message, 1, 0).unwrap(),
            DeadlineUpdate::Replace
        );
        assert_eq!(
            classify_liveness_attestation_sequences(message, 2, 0).unwrap(),
            DeadlineUpdate::TightenOnly
        );
        assert_eq!(
            classify_liveness_attestation_sequences(message, 1, 1)
                .unwrap_err()
                .code,
            "replay"
        );
        assert_eq!(
            apply_liveness_deadline(20_000, 25_000, DeadlineUpdate::TightenOnly),
            20_000
        );
        assert_eq!(
            apply_liveness_deadline(20_000, 25_000, DeadlineUpdate::Replace),
            25_000
        );
        assert_eq!(
            validate_liveness_attestation_message_json(
                &serde_json::to_string(&attestation["message"]).unwrap(),
                &context(),
                "live-a-1",
                2,
                1,
                "challenge-0001",
                "AAAAAAAAAAAAAAAAAAAAAA==",
            )
            .unwrap_err()
            .code,
            "replay"
        );
        assert_eq!(
            classify_liveness_attestation_sequences(message, 3, 0)
                .unwrap_err()
                .code,
            "replay"
        );
        assert_eq!(
            classify_liveness_attestation_sequences(message, 1, 2)
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
