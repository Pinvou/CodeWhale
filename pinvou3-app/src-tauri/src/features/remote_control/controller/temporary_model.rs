//! `temporary_model_package/v1` construction, validation and cleanup (DEV-15).
//!
//! A exports one task-scoped API-key route; B keeps it in memory only, bound to
//! `connection_id + controller_lease_id + lease_epoch + task_id`, and releases
//! it on every terminal path. Nothing here can carry a `credential_ref`,
//! OAuth/cookie/subscription state or a host path, and a missing package is a
//! hard failure — the task never falls back to a B model.

use std::fmt;
use std::net::IpAddr;
use std::sync::Arc;

use parking_lot::Mutex;
use serde_json::{Map, Value, json};

use super::crypto::SensitiveString;
use super::protocol::{LeaseContext, validate_controller_base_url};
use super::task::RemoteTaskModelScope;
use crate::platform::prefs::{
    ImageCapabilityOverride, MODEL_PROVIDER_KIND_CODING_PLAN, ModelPreset, SavedModel,
};

pub(crate) const PACKAGE_SCHEMA: &str = "temporary_model_package/v1";
pub(crate) const MAX_PACKAGE_BYTES: usize = 64 * 1024;
pub(crate) const MAX_ID_CHARS: usize = 128;
pub(crate) const MAX_DISPLAY_CHARS: usize = 128;
pub(crate) const MAX_MODEL_CHARS: usize = 256;
pub(crate) const MAX_URL_CHARS: usize = 2048;
pub(crate) const MAX_SECRET_BYTES: usize = 4096;

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
const IMAGE_OVERRIDES: &[&str] = &["pinvou", "enabled", "disabled"];

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct TemporaryModelError {
    pub(crate) code: &'static str,
}

impl TemporaryModelError {
    fn new(code: &'static str) -> Self {
        Self { code }
    }
}

impl fmt::Display for TemporaryModelError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for TemporaryModelError {}

type Result<T> = std::result::Result<T, TemporaryModelError>;

fn error(code: &'static str) -> TemporaryModelError {
    TemporaryModelError::new(code)
}

#[derive(Clone, Eq, PartialEq)]
pub(crate) struct TemporaryModelBinding {
    pub(crate) connection_id: String,
    pub(crate) controller_lease_id: String,
    pub(crate) lease_epoch: u64,
    pub(crate) task_id: String,
}

impl fmt::Debug for TemporaryModelBinding {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TemporaryModelBinding")
            .field("connection_id", &self.connection_id)
            .field("controller_lease_id", &self.controller_lease_id)
            .field("lease_epoch", &self.lease_epoch)
            .field("task_id", &self.task_id)
            .finish()
    }
}

impl TemporaryModelBinding {
    fn validate(&self) -> Result<()> {
        for value in [
            &self.connection_id,
            &self.controller_lease_id,
            &self.task_id,
        ] {
            if value.is_empty()
                || value.len() > MAX_ID_CHARS
                || !value
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
            {
                return Err(error("model_binding_invalid"));
            }
        }
        Ok(())
    }

    fn matches(&self, live: &LeaseContext, task_id: &str) -> bool {
        self.connection_id == live.connection_id
            && self.controller_lease_id == live.controller_lease_id
            && self.lease_epoch.to_string() == live.lease_epoch
            && self.task_id == task_id
    }
}

/// One exportable route plus its own API key. `secret` is redacted in `Debug`,
/// `Display` and any error text; it only leaves the process through
/// [`TemporaryModelPackageV1::to_wire_value`].
pub(crate) struct ExportRoute {
    pub(crate) display_name: String,
    pub(crate) preset: String,
    pub(crate) wire_api: String,
    pub(crate) base_url: String,
    pub(crate) model: String,
    pub(crate) provider_kind: Option<String>,
    pub(crate) vendor: Option<String>,
    pub(crate) endpoint_mode: Option<String>,
    pub(crate) context_window_tokens: Option<u32>,
    pub(crate) max_output_tokens: Option<u32>,
    pub(crate) reasoning_effort: Option<String>,
    pub(crate) image_capability_override: String,
    pub(crate) secret: SensitiveString,
}

impl fmt::Debug for ExportRoute {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ExportRoute")
            .field("display_name", &self.display_name)
            .field("preset", &self.preset)
            .field("wire_api", &self.wire_api)
            .field("base_url", &self.base_url)
            .field("model", &self.model)
            .field("secret", &"[REDACTED]")
            .finish()
    }
}

impl ExportRoute {
    fn validate(&self) -> Result<()> {
        if self.display_name.trim().is_empty()
            || self.display_name.chars().count() > MAX_DISPLAY_CHARS
        {
            return Err(error("model_display_name_invalid"));
        }
        if !PRESETS.contains(&self.preset.as_str()) {
            return Err(error("model_preset_unsupported"));
        }
        if !WIRE_APIS.contains(&self.wire_api.as_str()) {
            return Err(error("model_wire_api_unsupported"));
        }
        if (self.preset == "anthropic") != (self.wire_api == "anthropic_messages") {
            return Err(error("model_wire_api_mismatch"));
        }
        if self.model.trim().is_empty() || self.model.len() > MAX_MODEL_CHARS {
            return Err(error("model_wire_name_invalid"));
        }
        validate_export_target(&self.base_url)?;
        if let Some(kind) = &self.provider_kind {
            if !PROVIDER_KINDS.contains(&kind.as_str()) {
                return Err(error("model_provider_kind_unsupported"));
            }
        }
        if let Some(vendor) = &self.vendor {
            if !VENDORS.contains(&vendor.as_str()) {
                return Err(error("model_vendor_unsupported"));
            }
        }
        if let Some(mode) = &self.endpoint_mode {
            if !ENDPOINT_MODES.contains(&mode.as_str()) {
                return Err(error("model_endpoint_mode_unsupported"));
            }
        }
        if let Some(effort) = &self.reasoning_effort {
            if !REASONING_EFFORTS.contains(&effort.as_str()) {
                return Err(error("model_reasoning_effort_unsupported"));
            }
        }
        if !IMAGE_OVERRIDES.contains(&self.image_capability_override.as_str()) {
            return Err(error("model_image_override_unsupported"));
        }
        if self.secret.expose().len() > MAX_SECRET_BYTES {
            return Err(error("model_secret_too_large"));
        }
        Ok(())
    }

    fn to_wire_value(&self) -> Value {
        let mut object = Map::new();
        object.insert("display_name".into(), json!(self.display_name));
        object.insert("preset".into(), json!(self.preset));
        object.insert("wire_api".into(), json!(self.wire_api));
        object.insert("base_url".into(), json!(self.base_url));
        object.insert("model".into(), json!(self.model));
        object.insert(
            "image_capability_override".into(),
            json!(self.image_capability_override),
        );
        if let Some(kind) = &self.provider_kind {
            object.insert("provider_kind".into(), json!(kind));
        }
        if let Some(vendor) = &self.vendor {
            object.insert("vendor".into(), json!(vendor));
        }
        if let Some(mode) = &self.endpoint_mode {
            object.insert("endpoint_mode".into(), json!(mode));
        }
        if let Some(tokens) = self.context_window_tokens {
            object.insert("context_window_tokens".into(), json!(tokens));
        }
        if let Some(tokens) = self.max_output_tokens {
            object.insert("max_output_tokens".into(), json!(tokens));
        }
        if let Some(effort) = &self.reasoning_effort {
            object.insert("reasoning_effort".into(), json!(effort));
        }
        Value::Object(object)
    }

    fn to_redacted_value(&self) -> Value {
        let mut value = self.to_wire_value();
        value["has_credential"] = json!(true);
        value
    }

    fn from_wire_value(value: &Value, label: &'static str) -> Result<Self> {
        let object = exact_object(
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
        let context_window_tokens = optional_u32(object, "context_window_tokens")?;
        let max_output_tokens = optional_u32(object, "max_output_tokens")?;
        Ok(Self {
            display_name: required_string(object, "display_name", MAX_DISPLAY_CHARS)?,
            preset: required_string(object, "preset", MAX_ID_CHARS)?,
            wire_api: required_string(object, "wire_api", MAX_ID_CHARS)?,
            base_url: required_string(object, "base_url", MAX_URL_CHARS)?,
            model: required_string(object, "model", MAX_MODEL_CHARS)?,
            provider_kind: optional_string(object, "provider_kind", MAX_ID_CHARS)?,
            vendor: optional_string(object, "vendor", MAX_ID_CHARS)?,
            endpoint_mode: optional_string(object, "endpoint_mode", MAX_ID_CHARS)?,
            context_window_tokens,
            max_output_tokens,
            reasoning_effort: optional_string(object, "reasoning_effort", MAX_ID_CHARS)?,
            image_capability_override: required_string(
                object,
                "image_capability_override",
                MAX_ID_CHARS,
            )?,
            secret: SensitiveString::new("placeholder")
                .map_err(|_| error("model_secret_invalid"))?,
        })
    }
}

pub(crate) struct TemporaryModelPackageV1 {
    pub(crate) package_id: String,
    pub(crate) binding: TemporaryModelBinding,
    pub(crate) primary: ExportRoute,
    pub(crate) vision_fallback: Option<ExportRoute>,
}

impl fmt::Debug for TemporaryModelPackageV1 {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TemporaryModelPackageV1")
            .field("package_id", &self.package_id)
            .field("binding", &self.binding)
            .field("primary", &self.primary)
            .field("vision_fallback", &self.vision_fallback)
            .finish()
    }
}

impl TemporaryModelPackageV1 {
    pub(crate) fn validate(&self) -> Result<()> {
        if self.package_id.is_empty()
            || self.package_id.len() > MAX_ID_CHARS
            || !self
                .package_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
        {
            return Err(error("model_package_id_invalid"));
        }
        self.binding.validate()?;
        self.primary.validate()?;
        if let Some(fallback) = &self.vision_fallback {
            fallback.validate()?;
        }
        self.enforce_size()
    }

    fn enforce_size(&self) -> Result<()> {
        let bytes = serde_json::to_vec(&self.to_wire_value())
            .map_err(|_| error("model_package_invalid"))?
            .len();
        enforce_package_size(bytes)
    }

    /// The only path allowed to carry the API key.
    pub(crate) fn to_wire_value(&self) -> Value {
        let mut top = Map::new();
        top.insert("schema".into(), json!(PACKAGE_SCHEMA));
        top.insert("package_id".into(), json!(self.package_id));
        top.insert(
            "binding".into(),
            json!({
                "connection_id": self.binding.connection_id,
                "controller_lease_id": self.binding.controller_lease_id,
                "lease_epoch": self.binding.lease_epoch,
                "task_id": self.binding.task_id,
            }),
        );
        top.insert("primary".into(), self.primary.to_wire_value());
        top.insert(
            "primary_credential".into(),
            json!({ "kind": "api_key", "secret": self.primary.secret.expose() }),
        );
        if let Some(fallback) = &self.vision_fallback {
            top.insert(
                "vision_fallback".into(),
                json!({
                    "route": fallback.to_wire_value(),
                    "credential": { "kind": "api_key", "secret": fallback.secret.expose() },
                }),
            );
        }
        Value::Object(top)
    }

    /// Log/telemetry projection: routes only, never a credential value.
    pub(crate) fn to_redacted_value(&self) -> Value {
        json!({
            "schema": PACKAGE_SCHEMA,
            "package_id": self.package_id,
            "binding": {
                "connection_id": self.binding.connection_id,
                "controller_lease_id": self.binding.controller_lease_id,
                "lease_epoch": self.binding.lease_epoch.to_string(),
                "task_id": self.binding.task_id,
            },
            "primary": self.primary.to_redacted_value(),
            "vision_fallback": self
                .vision_fallback
                .as_ref()
                .map(ExportRoute::to_redacted_value),
        })
    }

    /// Strict B-side parse. Unknown fields, unknown enums, missing fields and
    /// invalid combinations all fail closed.
    pub(crate) fn from_wire_value(value: &Value) -> Result<Self> {
        let top = exact_object(
            value,
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
        if required_string(top, "schema", MAX_ID_CHARS)? != PACKAGE_SCHEMA {
            return Err(error("model_package_schema_unsupported"));
        }
        let binding_value = top
            .get("binding")
            .ok_or_else(|| error("model_binding_invalid"))?;
        let binding = exact_object(
            binding_value,
            &[
                "connection_id",
                "controller_lease_id",
                "lease_epoch",
                "task_id",
            ],
            &[],
            "temporary_model_package.binding",
        )?;
        let lease_epoch = binding
            .get("lease_epoch")
            .and_then(Value::as_u64)
            .ok_or_else(|| error("model_binding_invalid"))?;
        let binding = TemporaryModelBinding {
            connection_id: required_string(binding, "connection_id", MAX_ID_CHARS)?,
            controller_lease_id: required_string(binding, "controller_lease_id", MAX_ID_CHARS)?,
            lease_epoch,
            task_id: required_string(binding, "task_id", MAX_ID_CHARS)?,
        };
        let primary = ExportRoute::from_wire_value(
            top.get("primary")
                .ok_or_else(|| error("model_route_invalid"))?,
            "temporary_model_package.primary",
        )?;
        let primary_secret = credential_secret(
            top.get("primary_credential")
                .ok_or_else(|| error("model_credential_invalid"))?,
            "temporary_model_package.primary_credential",
        )?;
        let primary = ExportRoute {
            secret: primary_secret,
            ..primary
        };
        let vision_fallback = match top.get("vision_fallback") {
            None => None,
            Some(fallback) => {
                let fallback = exact_object(
                    fallback,
                    &["route", "credential"],
                    &[],
                    "temporary_model_package.vision_fallback",
                )?;
                let route = ExportRoute::from_wire_value(
                    fallback
                        .get("route")
                        .ok_or_else(|| error("model_route_invalid"))?,
                    "temporary_model_package.vision_fallback.route",
                )?;
                let secret = credential_secret(
                    fallback
                        .get("credential")
                        .ok_or_else(|| error("model_credential_invalid"))?,
                    "temporary_model_package.vision_fallback.credential",
                )?;
                Some(ExportRoute { secret, ..route })
            }
        };
        let package = Self {
            package_id: required_string(top, "package_id", MAX_ID_CHARS)?,
            binding,
            primary,
            vision_fallback,
        };
        package.validate()?;
        Ok(package)
    }

    pub(crate) fn validate_for_task(&self, live: &LeaseContext, task_id: &str) -> Result<()> {
        self.validate()?;
        if !self.binding.matches(live, task_id) {
            return Err(error("model_binding_stale"));
        }
        Ok(())
    }
}

fn enforce_package_size(bytes: usize) -> Result<()> {
    if bytes > MAX_PACKAGE_BYTES {
        return Err(error("model_package_too_large"));
    }
    Ok(())
}

fn exact_object<'a>(
    value: &'a Value,
    required: &[&str],
    optional: &[&str],
    _label: &'static str,
) -> Result<&'a Map<String, Value>> {
    let object = value
        .as_object()
        .ok_or_else(|| error("model_package_invalid"))?;
    for key in object.keys() {
        if !required.contains(&key.as_str()) && !optional.contains(&key.as_str()) {
            return Err(error("model_package_unknown_field"));
        }
    }
    for field in required {
        if !object.contains_key(*field) {
            return Err(error("model_package_missing_field"));
        }
    }
    Ok(object)
}

fn required_string(
    object: &Map<String, Value>,
    field: &'static str,
    max_chars: usize,
) -> Result<String> {
    let value = object
        .get(field)
        .and_then(Value::as_str)
        .ok_or_else(|| error("model_package_missing_field"))?;
    if value.len() > max_chars {
        return Err(error("model_package_field_invalid"));
    }
    Ok(value.to_owned())
}

fn optional_string(
    object: &Map<String, Value>,
    field: &'static str,
    max_chars: usize,
) -> Result<Option<String>> {
    match object.get(field) {
        None | Some(Value::Null) => Ok(None),
        Some(_) => Ok(Some(required_string(object, field, max_chars)?)),
    }
}

fn optional_u32(object: &Map<String, Value>, field: &'static str) -> Result<Option<u32>> {
    match object.get(field) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => {
            let number = value
                .as_u64()
                .filter(|number| *number <= u32::MAX as u64)
                .ok_or_else(|| error("model_package_field_invalid"))?;
            Ok(Some(number as u32))
        }
    }
}

fn credential_secret(value: &Value, label: &'static str) -> Result<SensitiveString> {
    let object = exact_object(value, &["kind", "secret"], &[], label)?;
    if required_string(object, "kind", MAX_ID_CHARS)? != "api_key" {
        return Err(error("model_credential_kind_unsupported"));
    }
    let secret = required_string(object, "secret", MAX_SECRET_BYTES)?;
    SensitiveString::new(secret).map_err(|_| error("model_secret_invalid"))
}

/// Rejects endpoints B must never call on behalf of A: loopback, private,
/// link-local (including the cloud metadata address) and unresolved local
/// names. Redirects are handled by the transport, which must not follow them
/// across hosts.
pub(crate) fn validate_export_target(base_url: &str) -> Result<()> {
    if base_url.len() > MAX_URL_CHARS {
        return Err(error("model_endpoint_invalid"));
    }
    validate_controller_base_url(base_url, "temporary_model_package.base_url")
        .map_err(|_| error("model_endpoint_invalid"))?;
    // Classify the canonical host used by the HTTP URL parser, not a DNS-like
    // spelling that it can normalize into an equivalent numeric address.
    let parsed = reqwest::Url::parse(base_url).map_err(|_| error("model_endpoint_invalid"))?;
    let host = parsed
        .host_str()
        .ok_or_else(|| error("model_endpoint_invalid"))?;
    let host = host.trim_end_matches('.').to_ascii_lowercase();
    if host.is_empty() {
        return Err(error("model_endpoint_invalid"));
    }
    if host == "localhost"
        || host.ends_with(".localhost")
        || host.ends_with(".local")
        || host.ends_with(".internal")
        || host.ends_with(".home.arpa")
    {
        return Err(error("model_endpoint_local"));
    }
    if let Ok(address) = host.parse::<IpAddr>() {
        if !is_globally_routable(address) {
            return Err(error("model_endpoint_private"));
        }
    }
    Ok(())
}

fn is_globally_routable(address: IpAddr) -> bool {
    match address {
        IpAddr::V4(address) => {
            let octets = address.octets();
            // IANA non-global shared/benchmark/protocol ranges. Keep the
            // globally reachable protocol-assignment exceptions .9 and .10.
            !(address.is_loopback()
                || address.is_private()
                || address.is_link_local()
                || address.is_broadcast()
                || address.is_documentation()
                || address.is_unspecified()
                || octets[0] == 0
                || octets[0] >= 224
                || (octets[0] == 100 && (64..=127).contains(&octets[1]))
                || (octets[0] == 198 && (18..=19).contains(&octets[1]))
                || (octets[..3] == [192, 0, 0] && ![9, 10].contains(&octets[3]))
                || octets == [192, 88, 99, 2])
        }
        IpAddr::V6(address) => {
            !(address.is_loopback()
                || address.is_unspecified()
                || address.is_unique_local()
                || address.is_unicast_link_local())
        }
    }
}

/// A-side export decision for one saved model.
#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ExportDecision {
    pub(crate) exportable: bool,
    pub(crate) reason: Option<&'static str>,
}

impl ExportDecision {
    fn exportable() -> Self {
        Self {
            exportable: true,
            reason: None,
        }
    }

    fn refused(reason: &'static str) -> Self {
        Self {
            exportable: false,
            reason: Some(reason),
        }
    }
}

/// Maps one `SavedModel` to the v1 route. Models that depend on OAuth, cookies,
/// subscription login or a pending credential edit are refused, and the
/// `credential_ref` itself is never transported.
pub(crate) fn export_decision(model: &SavedModel) -> ExportDecision {
    if model.is_builtin_llmapi() {
        return ExportDecision::refused("model_not_exportable_builtin");
    }
    if model
        .provider_kind
        .as_deref()
        .is_some_and(|kind| kind == MODEL_PROVIDER_KIND_CODING_PLAN)
    {
        return ExportDecision::refused("model_not_exportable_subscription");
    }
    if model.credential_action.is_some() {
        return ExportDecision::refused("model_credential_edit_pending");
    }
    if model.api_key.trim().is_empty() {
        return ExportDecision::refused("model_credential_missing");
    }
    if wire_api_for_preset(model.preset).is_none() {
        return ExportDecision::refused("model_preset_unsupported");
    }
    if validate_export_target(&model.base_url).is_err() {
        return ExportDecision::refused("model_endpoint_unreachable");
    }
    ExportDecision::exportable()
}

pub(crate) fn project_saved_model(model: &SavedModel) -> Result<ExportRoute> {
    let decision = export_decision(model);
    if !decision.exportable {
        return Err(error(decision.reason.unwrap_or("model_not_exportable")));
    }
    let wire_api =
        wire_api_for_preset(model.preset).ok_or_else(|| error("model_preset_unsupported"))?;
    let display_name = if model.name.trim().is_empty() {
        model.alias.clone().unwrap_or_else(|| model.model.clone())
    } else {
        model.name.trim().to_owned()
    };
    let route = ExportRoute {
        display_name: display_name.chars().take(MAX_DISPLAY_CHARS).collect(),
        preset: model.preset.as_str().to_owned(),
        wire_api: wire_api.to_owned(),
        base_url: model.base_url.clone(),
        model: model.model.clone(),
        provider_kind: model.provider_kind.clone(),
        vendor: model.vendor.clone(),
        endpoint_mode: model.endpoint_mode.clone(),
        context_window_tokens: model.context_window_tokens,
        max_output_tokens: model.max_output_tokens,
        reasoning_effort: model.reasoning_effort.clone(),
        image_capability_override: image_override_str(model.image_capability_override).to_owned(),
        secret: SensitiveString::new(model.api_key.clone())
            .map_err(|_| error("model_secret_invalid"))?,
    };
    route.validate()?;
    Ok(route)
}

fn wire_api_for_preset(preset: ModelPreset) -> Option<&'static str> {
    match preset {
        ModelPreset::Anthropic => Some("anthropic_messages"),
        ModelPreset::LocalVllm
        | ModelPreset::Deepseek
        | ModelPreset::Kimi
        | ModelPreset::OpenaiCompatible
        | ModelPreset::Qwen
        | ModelPreset::Doubao
        | ModelPreset::Minimax
        | ModelPreset::Glm
        | ModelPreset::Mimo
        | ModelPreset::Openai
        | ModelPreset::Gemini
        | ModelPreset::Xai => Some("openai_chat_completions"),
    }
}

fn image_override_str(value: ImageCapabilityOverride) -> &'static str {
    match value {
        ImageCapabilityOverride::Pinvou => "pinvou",
        ImageCapabilityOverride::Enabled => "enabled",
        ImageCapabilityOverride::Disabled => "disabled",
    }
}

/// B-side in-memory holder. Exactly one package is live per task, it is always
/// checked against the live lease before use, and [`RemoteTaskModelScope`]
/// releases it on every task terminal path.
#[derive(Default)]
pub(crate) struct TemporaryModelStore {
    installed: Mutex<Option<InstalledPackage>>,
}

struct InstalledPackage {
    package: TemporaryModelPackageV1,
    installed_at_ms: u64,
}

impl TemporaryModelStore {
    pub(crate) fn install(
        &self,
        package: TemporaryModelPackageV1,
        live: &LeaseContext,
        task_id: &str,
        now_b: u64,
    ) -> Result<()> {
        package.validate_for_task(live, task_id)?;
        let mut installed = self.installed.lock();
        if let Some(existing) = installed.as_ref() {
            if existing.package.binding.task_id == task_id {
                // A retry of the same task replaces its own package.
                *installed = Some(InstalledPackage {
                    package,
                    installed_at_ms: now_b,
                });
                return Ok(());
            }
            return Err(error("model_task_already_active"));
        }
        *installed = Some(InstalledPackage {
            package,
            installed_at_ms: now_b,
        });
        Ok(())
    }

    pub(crate) fn redacted_route(&self, task_id: &str, live: &LeaseContext) -> Result<Value> {
        let installed = self.installed.lock();
        let package = installed
            .as_ref()
            .map(|installed| &installed.package)
            .filter(|package| package.binding.task_id == task_id)
            .ok_or_else(|| error("model_package_missing"))?;
        package.validate_for_task(live, task_id)?;
        Ok(package.to_redacted_value())
    }

    /// Runs `use_route` with access to the live credentials. The closure must
    /// not persist or log them.
    pub(crate) fn with_credentials<R>(
        &self,
        task_id: &str,
        live: &LeaseContext,
        use_route: impl FnOnce(&ExportRoute, Option<&ExportRoute>) -> R,
    ) -> Result<R> {
        let installed = self.installed.lock();
        let package = installed
            .as_ref()
            .map(|installed| &installed.package)
            .filter(|package| package.binding.task_id == task_id)
            .ok_or_else(|| error("model_package_missing"))?;
        package.validate_for_task(live, task_id)?;
        Ok(use_route(
            &package.primary,
            package.vision_fallback.as_ref(),
        ))
    }

    pub(crate) fn installed_at_ms(&self) -> Option<u64> {
        self.installed
            .lock()
            .as_ref()
            .map(|installed| installed.installed_at_ms)
    }

    pub(crate) fn active_task(&self) -> Option<String> {
        self.installed
            .lock()
            .as_ref()
            .map(|installed| installed.package.binding.task_id.clone())
    }

    /// Idempotent release; returns `true` when a package was actually dropped.
    pub(crate) fn clear_task(&self, task_id: &str) -> bool {
        let mut installed = self.installed.lock();
        if installed
            .as_ref()
            .is_some_and(|existing| existing.package.binding.task_id == task_id)
        {
            *installed = None;
            return true;
        }
        false
    }

    pub(crate) fn clear_for_connection(&self, connection_id: &str) -> bool {
        let mut installed = self.installed.lock();
        if installed
            .as_ref()
            .is_some_and(|existing| existing.package.binding.connection_id == connection_id)
        {
            *installed = None;
            return true;
        }
        false
    }

    pub(crate) fn clear_all(&self) {
        *self.installed.lock() = None;
    }

    pub(crate) fn is_empty(&self) -> bool {
        self.installed.lock().is_none()
    }
}

impl RemoteTaskModelScope for TemporaryModelStore {
    fn release_task_credentials(&self, task_id: &str) {
        self.clear_task(task_id);
    }
}

/// Shared handle used by the command layer and the DEV-13 cleanup hook.
pub(crate) type SharedTemporaryModelStore = Arc<TemporaryModelStore>;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::platform::credential_store::CredentialEditAction;

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "7".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    fn route(base_url: &str, preset: &str, key: &str) -> ExportRoute {
        ExportRoute {
            display_name: "DeepSeek".into(),
            preset: preset.into(),
            wire_api: if preset == "anthropic" {
                "anthropic_messages".into()
            } else {
                "openai_chat_completions".into()
            },
            base_url: base_url.into(),
            model: "deepseek-chat".into(),
            provider_kind: Some("official_api".into()),
            vendor: Some("deepseek".into()),
            endpoint_mode: None,
            context_window_tokens: Some(131_072),
            max_output_tokens: Some(8_192),
            reasoning_effort: Some("high".into()),
            image_capability_override: "pinvou".into(),
            secret: SensitiveString::new(key).unwrap(),
        }
    }

    fn package(key: &str) -> TemporaryModelPackageV1 {
        TemporaryModelPackageV1 {
            package_id: "package-0001".into(),
            binding: TemporaryModelBinding {
                connection_id: "connection-0001".into(),
                controller_lease_id: "lease-0001".into(),
                lease_epoch: 7,
                task_id: "task-0001".into(),
            },
            primary: route("https://api.deepseek.com/v1", "deepseek", key),
            vision_fallback: None,
        }
    }

    fn saved_model() -> SavedModel {
        SavedModel {
            id: "model-0001".into(),
            name: "My DeepSeek".into(),
            alias: None,
            preset: ModelPreset::Deepseek,
            context_window_tokens: Some(131_072),
            max_output_tokens: Some(8_192),
            reasoning_effort: Some("high".into()),
            model: "deepseek-chat".into(),
            base_url: "https://api.deepseek.com/v1".into(),
            provider_kind: Some("official_api".into()),
            vendor: Some("deepseek".into()),
            endpoint_mode: None,
            image_capability_override: ImageCapabilityOverride::Pinvou,
            vision_model_id: None,
            api_key: "sk-a-secret-value".into(),
            credential_ref: None,
            credential_state: Default::default(),
            has_secret: true,
            credential_action: None,
        }
    }

    #[test]
    fn wire_serialization_carries_the_key_but_logs_and_debug_never_do() {
        let package = package("sk-a-secret-value");
        let wire = serde_json::to_string(&package.to_wire_value()).unwrap();
        assert!(wire.contains("sk-a-secret-value"));
        let redacted = serde_json::to_string(&package.to_redacted_value()).unwrap();
        assert!(!redacted.contains("sk-a-secret-value"));
        assert!(redacted.contains("has_credential"));
        let debug = format!("{package:?}");
        assert!(!debug.contains("sk-a-secret-value"));
        assert!(debug.contains("REDACTED"));
        assert_eq!(
            format!("{:?}", package.primary.secret),
            "SensitiveString([REDACTED])"
        );
        assert_eq!(format!("{}", package.primary.secret), "[REDACTED]");
    }

    #[test]
    fn strict_parse_rejects_unknown_missing_and_wrong_schema() {
        let package = package("sk-value");
        let wire = package.to_wire_value();
        assert_eq!(
            TemporaryModelPackageV1::from_wire_value(&wire)
                .unwrap()
                .package_id,
            "package-0001"
        );

        let mut unknown = wire.clone();
        unknown["credential_ref"] = json!("vault://x");
        assert_eq!(
            TemporaryModelPackageV1::from_wire_value(&unknown)
                .unwrap_err()
                .code,
            "model_package_unknown_field"
        );

        let mut missing = wire.clone();
        missing.as_object_mut().unwrap().remove("primary");
        assert_eq!(
            TemporaryModelPackageV1::from_wire_value(&missing)
                .unwrap_err()
                .code,
            "model_package_missing_field"
        );

        let mut wrong_schema = wire.clone();
        wrong_schema["schema"] = json!("temporary_model_package/v2");
        assert_eq!(
            TemporaryModelPackageV1::from_wire_value(&wrong_schema)
                .unwrap_err()
                .code,
            "model_package_schema_unsupported"
        );

        let mut bad_route_enum = wire;
        bad_route_enum["primary"]["preset"] = json!("custom_unknown");
        assert_eq!(
            TemporaryModelPackageV1::from_wire_value(&bad_route_enum)
                .unwrap_err()
                .code,
            "model_preset_unsupported"
        );
    }

    #[test]
    fn wire_api_must_match_the_preset() {
        let mut package = package("sk-value");
        package.primary.wire_api = "anthropic_messages".into();
        assert_eq!(
            package.validate().unwrap_err().code,
            "model_wire_api_mismatch"
        );
    }

    #[test]
    fn binding_must_match_the_live_lease_and_task() {
        let package = package("sk-value");
        assert!(package.validate_for_task(&lease(), "task-0001").is_ok());
        assert_eq!(
            package
                .validate_for_task(&lease(), "task-0002")
                .unwrap_err()
                .code,
            "model_binding_stale"
        );
        let mut other = lease();
        other.lease_epoch = "8".into();
        assert_eq!(
            package
                .validate_for_task(&other, "task-0001")
                .unwrap_err()
                .code,
            "model_binding_stale"
        );
        let mut other = lease();
        other.connection_id = "connection-0002".into();
        assert_eq!(
            package
                .validate_for_task(&other, "task-0001")
                .unwrap_err()
                .code,
            "model_binding_stale"
        );
    }

    #[test]
    fn endpoints_that_could_reach_b_local_network_are_rejected() {
        for base_url in [
            "http://127.0.0.1:8000/v1",
            "http://localhost:8000/v1",
            "http://10.1.2.3/v1",
            "http://192.168.1.10/v1",
            "http://172.16.5.5/v1",
            "http://169.254.169.254/latest/meta-data",
            "http://[::1]:8000/v1",
            "http://vllm.internal/v1",
            "https://user:pass@api.deepseek.com/v1",
        ] {
            assert!(
                validate_export_target(base_url).is_err(),
                "{base_url} must not be exportable"
            );
        }
        assert!(validate_export_target("https://api.deepseek.com/v1").is_ok());
        assert!(validate_export_target("https://8.8.8.8/v1").is_ok());
    }

    #[test]
    fn package_size_guard_rejects_oversized_payloads() {
        assert!(enforce_package_size(MAX_PACKAGE_BYTES).is_ok());
        assert_eq!(
            enforce_package_size(MAX_PACKAGE_BYTES + 1)
                .unwrap_err()
                .code,
            "model_package_too_large"
        );
    }

    #[test]
    fn saved_model_projection_excludes_subscription_and_pending_credentials() {
        let exportable = saved_model();
        assert!(export_decision(&exportable).exportable);
        let route = project_saved_model(&exportable).unwrap();
        assert_eq!(route.preset, "deepseek");
        assert_eq!(route.wire_api, "openai_chat_completions");
        assert_eq!(route.secret.expose(), "sk-a-secret-value");

        let mut builtin = saved_model();
        builtin.id = crate::platform::prefs::BUILTIN_LLMAPI_MODEL_ID.to_owned();
        assert_eq!(
            export_decision(&builtin).reason,
            Some("model_not_exportable_builtin")
        );

        let mut coding_plan = saved_model();
        coding_plan.provider_kind = Some(MODEL_PROVIDER_KIND_CODING_PLAN.to_owned());
        assert_eq!(
            export_decision(&coding_plan).reason,
            Some("model_not_exportable_subscription")
        );

        let mut pending = saved_model();
        pending.credential_action = Some(CredentialEditAction::Replace);
        assert_eq!(
            export_decision(&pending).reason,
            Some("model_credential_edit_pending")
        );

        let mut no_key = saved_model();
        no_key.api_key.clear();
        assert_eq!(
            export_decision(&no_key).reason,
            Some("model_credential_missing")
        );

        let mut loopback = saved_model();
        loopback.base_url = "http://127.0.0.1:8000/v1".into();
        assert_eq!(
            export_decision(&loopback).reason,
            Some("model_endpoint_unreachable")
        );
    }

    #[test]
    fn vision_fallback_is_one_layer_with_its_own_credential() {
        let mut package = package("sk-primary");
        package.vision_fallback = Some(route("https://api.openai.com/v1", "openai", "sk-vision"));
        package.validate().unwrap();
        let wire = package.to_wire_value();
        assert_eq!(wire["primary_credential"]["secret"], json!("sk-primary"));
        assert_eq!(
            wire["vision_fallback"]["credential"]["secret"],
            json!("sk-vision")
        );
        let parsed = TemporaryModelPackageV1::from_wire_value(&wire).unwrap();
        assert!(parsed.vision_fallback.is_some());

        let bad_fallback = TemporaryModelPackageV1 {
            package_id: "package-0001".into(),
            binding: TemporaryModelBinding {
                connection_id: "connection-0001".into(),
                controller_lease_id: "lease-0001".into(),
                lease_epoch: 7,
                task_id: "task-0001".into(),
            },
            primary: route("https://api.deepseek.com/v1", "deepseek", "sk-primary"),
            vision_fallback: Some(route("http://10.0.0.5/v1", "openai", "sk-x")),
        };
        assert_eq!(
            bad_fallback.validate().unwrap_err().code,
            "model_endpoint_private"
        );

        let mut nested = wire.clone();
        nested["vision_fallback"]["route"]["vision_fallback"] = json!({});
        assert_eq!(
            TemporaryModelPackageV1::from_wire_value(&nested)
                .unwrap_err()
                .code,
            "model_package_unknown_field"
        );
    }

    #[test]
    fn special_ipv4_export_targets_fail_closed_without_blocking_global_exceptions() {
        for host in [
            "100.64.0.0",
            "100.64.0.1",
            "100.127.255.255",
            "198.18.0.0",
            "198.18.0.1",
            "198.19.255.255",
            "192.0.0.0",
            "192.0.0.8",
            "192.0.0.11",
            "192.0.0.255",
            "192.88.99.2",
        ] {
            assert_eq!(
                validate_export_target(&format!("https://{host}/v1"))
                    .unwrap_err()
                    .code,
                "model_endpoint_private",
                "{host}"
            );
            let mut model = saved_model();
            model.base_url = format!("https://{host}/v1");
            assert_eq!(
                export_decision(&model).reason,
                Some("model_endpoint_unreachable")
            );
            let mut candidate = package("synthetic-test-api-key");
            candidate.primary.base_url = format!("https://{host}/v1");
            assert_eq!(
                candidate.validate().unwrap_err().code,
                "model_endpoint_private"
            );
            let mut fallback = package("synthetic-test-api-key");
            fallback.vision_fallback = Some(route(
                &format!("https://{host}/v1"),
                "openai",
                "synthetic-fallback-api-key",
            ));
            assert_eq!(
                fallback.validate().unwrap_err().code,
                "model_endpoint_private"
            );
        }
        for host in [
            "100.63.255.255",
            "100.128.0.0",
            "198.17.255.255",
            "198.20.0.0",
            "192.0.0.9",
            "192.0.0.10",
            "192.0.1.0",
            "192.31.196.1",
            "192.52.193.1",
            "192.175.48.1",
        ] {
            validate_export_target(&format!("https://{host}/v1")).unwrap();
        }
    }

    #[test]
    fn equivalent_ipv4_hosts_are_classified_after_url_normalization() {
        for (host, canonical) in [
            ("0x64400001", "100.64.0.1"),
            ("0xc6120001", "198.18.0.1"),
            ("0xc0000008", "192.0.0.8"),
            ("0xc0586302", "192.88.99.2"),
            ("0x7f000001", "127.0.0.1"),
        ] {
            let base_url = format!("https://{host}/v1");
            let parsed = reqwest::Url::parse(&base_url).unwrap();
            assert_eq!(parsed.host_str(), Some(canonical));
            assert_eq!(
                validate_export_target(&base_url).unwrap_err().code,
                "model_endpoint_private"
            );
            let mut model = saved_model();
            model.base_url = base_url.clone();
            assert_eq!(
                export_decision(&model).reason,
                Some("model_endpoint_unreachable")
            );
            let mut candidate = package("synthetic-test-api-key");
            candidate.primary.base_url = base_url.clone();
            assert_eq!(
                candidate.validate().unwrap_err().code,
                "model_endpoint_private"
            );
            let mut fallback = package("synthetic-test-api-key");
            fallback.vision_fallback =
                Some(route(&base_url, "openai", "synthetic-fallback-api-key"));
            assert_eq!(
                fallback.validate().unwrap_err().code,
                "model_endpoint_private"
            );
        }
        for host in [
            "0x08080808",
            "0xc0000009",
            "0xc000000a",
            "0xfeed.api.example.com",
        ] {
            validate_export_target(&format!("https://{host}/v1")).unwrap();
        }
    }

    #[test]
    fn store_is_task_scoped_and_releases_idempotently() {
        let store = TemporaryModelStore::default();
        store
            .install(package("sk-value"), &lease(), "task-0001", 1_000)
            .unwrap();
        assert_eq!(store.active_task().as_deref(), Some("task-0001"));
        assert!(store.redacted_route("task-0001", &lease()).is_ok());
        assert_eq!(
            store
                .redacted_route("task-0002", &lease())
                .unwrap_err()
                .code,
            "model_package_missing"
        );
        let copied = store
            .with_credentials("task-0001", &lease(), |primary, fallback| {
                (primary.secret.expose().to_owned(), fallback.is_some())
            })
            .unwrap();
        assert_eq!(copied, ("sk-value".to_owned(), false));

        let mut other = package("sk-other");
        other.binding.task_id = "task-0002".into();
        assert_eq!(
            store
                .install(other, &lease(), "task-0002", 1_100)
                .unwrap_err()
                .code,
            "model_task_already_active"
        );

        assert!(store.clear_task("task-0001"));
        assert!(!store.clear_task("task-0001"));
        assert!(store.is_empty());
        assert_eq!(
            store
                .redacted_route("task-0001", &lease())
                .unwrap_err()
                .code,
            "model_package_missing"
        );
    }

    #[test]
    fn cleanup_hook_releases_the_package_for_the_task() {
        let store = SharedTemporaryModelStore::default();
        store
            .install(package("sk-value"), &lease(), "task-0001", 1_000)
            .unwrap();
        assert_eq!(store.installed_at_ms(), Some(1_000));
        let scope: Arc<dyn RemoteTaskModelScope> = store.clone();
        scope.release_task_credentials("task-0001");
        assert!(store.is_empty());
        assert!(!store.clear_for_connection("connection-0001"));
    }

    #[test]
    fn connection_teardown_clears_the_remaining_package() {
        let store = TemporaryModelStore::default();
        store
            .install(package("sk-value"), &lease(), "task-0001", 1_000)
            .unwrap();
        assert!(store.clear_for_connection("connection-0001"));
        assert!(store.is_empty());
        assert!(!store.clear_for_connection("connection-0001"));
        store.clear_all();
        assert!(store.is_empty());
    }
}
