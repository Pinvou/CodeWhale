//! Pure, shared model-to-wire semantics. No environment, credentials or I/O.
//! Keep the local bridge and transported task routes on the same protocol.

pub(crate) fn is_official_deepseek_base_url(base_url: &str) -> bool {
    let normalized = base_url
        .trim()
        .trim_end_matches('/')
        .trim_end_matches("/beta")
        .trim_end_matches("/v1")
        .to_ascii_lowercase();
    matches!(normalized.as_str(), "https://api.deepseek.com")
}

pub(crate) fn is_official_openai_base_url(base_url: &str) -> bool {
    let normalized = base_url
        .trim()
        .trim_end_matches('/')
        .to_ascii_lowercase()
        .trim_end_matches("/v1")
        .to_string();
    matches!(normalized.as_str(), "https://api.openai.com")
}

pub(crate) fn openai_responses_wire_model(model: &str) -> bool {
    let lower = model.trim().to_ascii_lowercase();
    lower.starts_with("gpt-6")
        || lower.starts_with("gpt-5.6")
        || lower.starts_with("gpt-5.5")
        || lower.starts_with("gpt-5.4-mini")
}

/// Effective wire for a frozen model route, before local-only env overrides.
/// This preserves the bridge's existing host/prefix normalization exactly.
pub(crate) fn model_route_uses_responses(
    preset: &str,
    vendor: Option<&str>,
    model: &str,
    base_url: &str,
) -> bool {
    if is_official_deepseek_base_url(base_url) {
        return false;
    }
    preset == "openai_responses"
        || ((preset == "openai"
            || vendor.is_some_and(|vendor| vendor.trim().eq_ignore_ascii_case("openai")))
            && openai_responses_wire_model(model)
            && is_official_openai_base_url(base_url))
}

/// Canonical wire for the catalog metadata accepted by transported routes.
/// Responses is decided first; otherwise host and vendor precedence matches
/// the bridge's provider selection, not just the saved preset label.
pub(crate) fn model_route_wire_api(
    preset: &str,
    vendor: Option<&str>,
    model: &str,
    base_url: &str,
) -> &'static str {
    if is_official_deepseek_base_url(base_url) {
        return "openai_chat_completions";
    }
    if model_route_uses_responses(preset, vendor, model, base_url) {
        return "openai_responses";
    }
    if let Some(vendor) = vendor.map(|vendor| vendor.trim().to_ascii_lowercase()) {
        if vendor == "anthropic" {
            return "anthropic_messages";
        }
        if [
            "deepseek",
            "kimi",
            "moonshot",
            "glm",
            "zai",
            "zhipu",
            "minimax",
            "openrouter",
            "siliconflow",
            "mimo",
            "xiaomi",
            "xiaomi-mimo",
            "doubao",
            "volcengine",
            "xai",
            "grok",
            "qwen",
            "tencent",
            "openai",
            "gemini",
            "google",
        ]
        .contains(&vendor.as_str())
        {
            return "openai_chat_completions";
        }
    }
    if preset == "anthropic" {
        "anthropic_messages"
    } else {
        "openai_chat_completions"
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn frozen_wire_preserves_explicit_and_vendor_responses_routes() {
        for model in ["gpt-6-sol", "gpt-5.6-terra", "gpt-5.5", "gpt-5.4-mini"] {
            assert!(model_route_uses_responses(
                "openai",
                None,
                model,
                "https://api.openai.com/v1"
            ));
            assert!(model_route_uses_responses(
                "openai_compatible",
                Some(" OpenAI "),
                model,
                " https://API.OPENAI.COM/V1/ "
            ));
        }
        assert!(model_route_uses_responses(
            "openai_responses",
            None,
            "custom-id",
            "https://relay.example.test/v1"
        ));
    }

    #[test]
    fn frozen_wire_preserves_legacy_aggregator_and_host_guards() {
        assert!(!model_route_uses_responses(
            "openai",
            None,
            "gpt-4o",
            "https://api.openai.com/v1"
        ));
        assert!(!model_route_uses_responses(
            "openai_compatible",
            None,
            "gpt-6-sol",
            "https://api.openai.com/v1"
        ));
        for base in [
            "https://aggregator.example.test/v1",
            "http://api.openai.com/v1",
            "https://api.openai.com/v1?query=1",
            "https://api.openai.com/v1/other",
            "https://user@api.openai.com/v1",
            "https://api.openai.com.example.test/v1",
        ] {
            assert!(
                !model_route_uses_responses("openai", Some("openai"), "gpt-6-sol", base),
                "{base}"
            );
        }
        assert!(!model_route_uses_responses(
            "openai_responses",
            None,
            "custom-id",
            "https://api.deepseek.com/v1"
        ));
    }
}
