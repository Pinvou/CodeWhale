//! DEV-29-owned A-side outbox metadata. Durable authority remains exclusively
//! in `RemoteProxyRecord`; this module never persists wire payloads or keys.

use serde::Serialize;
use serde_json::Value;
use sha2::{Digest, Sha256};

use super::binding::{LiveRemoteContext, require_live_locked};
use crate::features::sessions::SessionStore;

const FINGERPRINT_DOMAIN: &[u8] = b"pinvou-remote-request-semantics-v1\0";
const MODEL_EXECUTION_DOMAIN: &[u8] = b"pinvou-remote-model-execution-v1\0";
const TASK_INPUT_DOMAIN: &[u8] = b"pinvou-controller-task-input-v1";
const ROUTE_FIELDS: &[&str] = &[
    "display_name",
    "preset",
    "wire_api",
    "base_url",
    "model",
    "image_capability_override",
    "provider_kind",
    "vendor",
    "endpoint_mode",
    "context_window_tokens",
    "max_output_tokens",
    "reasoning_effort",
];

/// Every non-secret request parameter that can affect B's task admission or
/// execution. New requests derive the model route digest from DEV-15's actual
/// prepared package. Package ID and credentials are deliberately excluded;
/// retries use only an opaque fingerprint handle and the original cached bytes.
#[derive(Clone, Serialize)]
pub(crate) struct RemoteRequestSemantics {
    pub(crate) local_session_id: String,
    pub(crate) remote_session_ref: String,
    pub(crate) controlled_session_id: String,
    pub(crate) session_id: String,
    pub(crate) capability_revision: u64,
    pub(crate) mode: String,
    pub(crate) model_source: String,
    pub(crate) model_profile_id: Option<String>,
    /// Digest of every non-secret DEV-15 export route field (including vision
    /// fallback), excluding credentials and randomized package identity.
    pub(crate) model_execution_digest: String,
    pub(crate) text: String,
    pub(crate) input_digest: String,
    pub(crate) attachment_ids: Vec<String>,
    pub(crate) skill_ids: Vec<String>,
    pub(crate) workspace_ids: Vec<String>,
}

fn hash_bytes(domain: &[u8], canonical: &[u8]) -> String {
    let mut hash = Sha256::new();
    hash.update(domain);
    hash.update((canonical.len() as u64).to_be_bytes());
    hash.update(canonical);
    hash.finalize()
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

fn exact_public_route(route: &Value) -> anyhow::Result<&Value> {
    let object = route
        .as_object()
        .ok_or_else(|| anyhow::anyhow!("invalid model route"))?;
    if object
        .keys()
        .any(|key| !ROUTE_FIELDS.contains(&key.as_str()))
        || !object.contains_key("model")
        || !object.contains_key("base_url")
    {
        anyhow::bail!("invalid or sensitive model route field");
    }
    Ok(route)
}

/// Digest only the public routes extracted from the one actual DEV-15 package.
fn model_execution_digest_from_routes(
    primary: &Value,
    vision: Option<&Value>,
) -> anyhow::Result<String> {
    let primary = exact_public_route(primary)?;
    let vision = vision.map(exact_public_route).transpose()?;
    let canonical = serde_json::to_vec(&serde_json::json!({
        "schema": "temporary_model_package/v1",
        "primary": primary,
        "vision_fallback": vision,
    }))?;
    Ok(hash_bytes(MODEL_EXECUTION_DOMAIN, &canonical))
}

/// Extract only the DEV-15 wire package's route semantics. This works on the
/// actual export package, so changes to model/base URL/preset/provider/vision
/// configuration conflict even when a saved profile ID stays unchanged.
pub(crate) fn model_execution_digest(package: Option<&Value>) -> anyhow::Result<String> {
    match package {
        None => Ok(hash_bytes(
            MODEL_EXECUTION_DOMAIN,
            &serde_json::to_vec(&serde_json::json!({"source": "controlled_device"}))?,
        )),
        Some(package) => {
            let object = package
                .as_object()
                .ok_or_else(|| anyhow::anyhow!("invalid model package"))?;
            if object.get("schema").and_then(Value::as_str) != Some("temporary_model_package/v1")
                || object.get("package_id").and_then(Value::as_str).is_none()
                || !object.get("binding").is_some_and(Value::is_object)
                || !object
                    .get("primary_credential")
                    .is_some_and(Value::is_object)
            {
                anyhow::bail!("incomplete model package");
            }
            if object.keys().any(|key| {
                ![
                    "schema",
                    "package_id",
                    "binding",
                    "primary",
                    "primary_credential",
                    "vision_fallback",
                ]
                .contains(&key.as_str())
            }) {
                anyhow::bail!("unexpected model package field");
            }
            let primary = object
                .get("primary")
                .ok_or_else(|| anyhow::anyhow!("missing primary route"))?;
            let vision = object
                .get("vision_fallback")
                .map(|fallback| {
                    let fallback = fallback
                        .as_object()
                        .ok_or_else(|| anyhow::anyhow!("invalid vision fallback"))?;
                    if fallback
                        .keys()
                        .any(|key| !["route", "credential"].contains(&key.as_str()))
                    {
                        anyhow::bail!("unexpected vision fallback field");
                    }
                    if !fallback.get("credential").is_some_and(Value::is_object) {
                        anyhow::bail!("missing vision credential envelope");
                    }
                    fallback
                        .get("route")
                        .ok_or_else(|| anyhow::anyhow!("missing vision route"))
                })
                .transpose()?;
            model_execution_digest_from_routes(primary, vision)
        }
    }
}

/// Domain-separated, deterministic lowercase SHA-256. Persist only the digest
/// in the strict v3 pending record; the text itself remains in process memory.
pub(crate) fn request_fingerprint(
    semantics: &RemoteRequestSemantics,
    live: &LiveRemoteContext,
) -> anyhow::Result<String> {
    let canonical = serde_json::to_vec(&serde_json::json!({
        "semantics": semantics,
        "binding": {
            "peer_id": live.peer_id,
            "resume_boundary_id": live.resume_boundary_id,
            "connection_id": live.connection_id,
            "controller_lease_id": live.controller_lease_id,
            "lease_epoch": live.lease_epoch,
            "relay_epoch": live.relay_epoch,
            "generation": live.generation,
        }
    }))?;
    Ok(hash_bytes(FINGERPRINT_DOMAIN, &canonical))
}

/// The frozen task-input digest includes the text and both reference lists.
/// It is independent from the outbox fingerprint, which additionally binds
/// the model, local SessionStore identity and current encrypted lease.
pub(crate) fn remote_task_input_digest(
    text: &str,
    attachment_ids: &[String],
    skill_ids: &[String],
) -> String {
    let mut hash = Sha256::new();
    hash.update(TASK_INPUT_DOMAIN);
    for field in [
        text.as_bytes().to_vec(),
        attachment_ids.join("\0").into_bytes(),
        skill_ids.join("\0").into_bytes(),
    ] {
        hash.update((field.len() as u64).to_be_bytes());
        hash.update(field);
    }
    hash.finalize()
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

/// Ephemeral, sensitive in-process payload. It must never derive `Debug` or
/// serialization and is dropped after the first dispatch attempt.
pub(crate) struct PreparedOutboxEntry {
    pub(crate) fingerprint: String,
    pub(crate) context: LiveRemoteContext,
    pub(crate) semantics: RemoteRequestSemantics,
    pub(crate) wire_payload: Value,
}

impl SessionStore {
    /// A command may obtain only the currently authenticated, confirmed live
    /// binding; UI-supplied connection IDs never become dispatch authority.
    pub(crate) fn remote_dispatch_live_context(
        &self,
        local_session_id: &str,
        remote_ref: &str,
    ) -> anyhow::Result<LiveRemoteContext> {
        let state = self.remote_proxies.lock();
        let context = state
            .current_context
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("remote controller is not connected"))?;
        require_live_locked(&state, local_session_id, remote_ref, context)?;
        Ok(context.clone())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn attachment_input_digest_is_stable_and_binds_each_reference() {
        assert_eq!(
            remote_task_input_digest("hello", &[], &[]),
            remote_task_input_digest("hello", &[], &[]),
        );
        let one = remote_task_input_digest("hello", &["attachment-1".into()], &[]);
        assert_eq!(one.len(), 64);
        assert_ne!(one, remote_task_input_digest("hello", &[], &[]));
        assert_ne!(
            one,
            remote_task_input_digest("hello", &["attachment-2".into()], &[])
        );
        assert_eq!(
            remote_task_input_digest("hello", &["attachment-1".into()], &["skill-1".into()],),
            "edc6e22106c7785a93b3c6f7482a294fe5bea7748b31380728101e07b87a8065"
        );
    }

    #[test]
    fn fingerprint_covers_semantics_and_binding_without_serializing_payload() {
        let semantics = RemoteRequestSemantics {
            local_session_id: "remote-1".into(),
            remote_session_ref: "ref-1".into(),
            controlled_session_id: "b-1".into(),
            session_id: "session-1".into(),
            capability_revision: 3,
            mode: "chat".into(),
            model_source: "controller_temporary".into(),
            model_profile_id: Some("model-1".into()),
            model_execution_digest: "b".repeat(64),
            text: "hello".into(),
            input_digest: "a".repeat(64),
            attachment_ids: Vec::new(),
            skill_ids: Vec::new(),
            workspace_ids: Vec::new(),
        };
        let live = LiveRemoteContext {
            peer_id: "peer-1".into(),
            resume_boundary_id: "resume-1".into(),
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "epoch-1".into(),
            relay_epoch: "relay-1".into(),
            generation: "generation-1".into(),
            control_connected: true,
            encrypted_channel_connected: true,
        };
        let first = request_fingerprint(&semantics, &live).unwrap();
        assert_eq!(first.len(), 64);
        assert_eq!(first, request_fingerprint(&semantics, &live).unwrap());
        let mut other = semantics.clone();
        other.model_profile_id = Some("model-2".into());
        assert_ne!(first, request_fingerprint(&other, &live).unwrap());
        other = semantics.clone();
        other.text = "changed".into();
        assert_ne!(first, request_fingerprint(&other, &live).unwrap());
        let mut other_live = live.clone();
        other_live.generation = "generation-2".into();
        assert_ne!(first, request_fingerprint(&semantics, &other_live).unwrap());
    }

    #[test]
    fn model_digest_covers_every_export_route_but_not_secret_or_package_nonce() {
        let mut package = serde_json::json!({
            "schema": "temporary_model_package/v1",
            "package_id": "package-random-1",
            "binding": {"task_id": "task-1"},
            "primary": {
                "display_name": "Primary", "preset": "openai", "wire_api": "chat_completions",
                "base_url": "https://model.example/v1", "model": "model-a",
                "image_capability_override": "enabled", "provider_kind": "openai",
                "vendor": "openai", "endpoint_mode": "chat_completions",
                "context_window_tokens": "1000", "max_output_tokens": "500",
                "reasoning_effort": "low"
            },
            "primary_credential": {"kind": "api_key", "secret": "sk-first"},
            "vision_fallback": {
                "route": {
                    "display_name": "Vision", "preset": "openai", "wire_api": "chat_completions",
                    "base_url": "https://vision.example/v1", "model": "vision-a",
                    "image_capability_override": "enabled"
                },
                "credential": {"kind": "api_key", "secret": "sk-vision-first"}
            }
        });
        let first = model_execution_digest(Some(&package)).unwrap();
        package["package_id"] = "package-random-2".into();
        package["primary_credential"]["secret"] = "sk-second".into();
        package["vision_fallback"]["credential"]["secret"] = "sk-vision-second".into();
        assert_eq!(first, model_execution_digest(Some(&package)).unwrap());
        for path in [
            ("primary", "model"),
            ("primary", "base_url"),
            ("primary", "preset"),
            ("primary", "provider_kind"),
            ("primary", "context_window_tokens"),
            ("vision_fallback", "route"),
        ] {
            let mut changed = package.clone();
            if path.0 == "vision_fallback" {
                changed["vision_fallback"]["route"]["model"] = "vision-b".into();
            } else {
                changed[path.0][path.1] = "different".into();
            }
            assert_ne!(first, model_execution_digest(Some(&changed)).unwrap());
        }
        package["primary"]["secret"] = "smuggled".into();
        assert!(model_execution_digest(Some(&package)).is_err());
    }
}
