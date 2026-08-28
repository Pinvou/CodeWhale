//! Route-scoped capability facts.
//!
//! Capability state is deliberately three-valued: an absent catalog fact is
//! unknown, not unsupported, and must never be promoted to supported by a
//! transport/protocol heuristic. These values travel with the exact provider
//! offering selected by [`super::resolver::RouteResolver`].

use serde::{Deserialize, Serialize};

use crate::{
    DEFAULT_KIMI_CODE_BASE_URL, DEFAULT_MOONSHOT_BASE_URL, ProviderKind,
    provider_base_url_is_official,
};

/// Whether a resolved provider/model offering supports one capability.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CapabilityState {
    /// The selected offering explicitly reports support.
    Supported,
    /// The selected offering explicitly reports no support.
    Unsupported,
    /// The selected offering did not state the fact.
    #[default]
    Unknown,
}

impl CapabilityState {
    /// Preserve a sourced optional boolean as a three-state fact.
    #[must_use]
    pub const fn from_optional_bool(value: Option<bool>) -> Self {
        match value {
            Some(true) => Self::Supported,
            Some(false) => Self::Unsupported,
            None => Self::Unknown,
        }
    }

    /// Whether the source explicitly reports support.
    #[must_use]
    pub const fn is_supported(self) -> bool {
        matches!(self, Self::Supported)
    }
}

/// Return the documented server-side web-search fact for one exact direct
/// provider/model offering.
///
/// This is intentionally a small sourced table, not a protocol or model-family
/// heuristic. Aggregators, custom endpoints, aliases, snapshots, and nearby
/// model names remain [`CapabilityState::Unknown`] until a provider-owned fact
/// exists for that exact offering.
///
/// Sources:
/// - OpenAI Responses web search: <https://developers.openai.com/api/docs/guides/tools-web-search>
/// - Anthropic web search tool: <https://platform.claude.com/docs/en/agents-and-tools/tool-use/web-search-tool>
/// - xAI web search tool: <https://docs.x.ai/developers/tools/web-search>
/// - DeepSeek Responses web search: <https://api-docs.deepseek.com/api/create-response/>
/// - Kimi built-in web search: <https://platform.kimi.ai/docs/guide/use-web-search>
/// - Alibaba Model Studio Token Plan Harness tools: <https://help.aliyun.com/en/model-studio/token-plan-harness-tool>
/// - Z.AI Web Search API: <https://docs.z.ai/api-reference/tools/web-search>
/// - Zhipu Web Search API: <https://docs.bigmodel.cn/api-reference/工具-api/网络搜索>
/// - Xiaomi MiMo web search: <https://mimo.mi.com/docs/en-US/usage-guide/tool-calling/web-search>
#[must_use]
pub(crate) fn documented_server_side_web_search(
    provider_id: &str,
    wire_model_id: &str,
) -> CapabilityState {
    let provider_id = provider_id.trim().to_ascii_lowercase();
    let wire_model_id = wire_model_id.trim().to_ascii_lowercase();
    let supported = match provider_id.as_str() {
        "openai" => matches!(
            wire_model_id.as_str(),
            "gpt-5.6" | "gpt-5.5" | "gpt-5.4" | "gpt-4.1" | "gpt-4.1-mini" | "o4-mini"
        ),
        "anthropic" => matches!(
            wire_model_id.as_str(),
            "claude-fable-5"
                | "claude-opus-4-8"
                | "claude-mythos-5"
                | "claude-mythos-preview"
                | "claude-opus-4-7"
                | "claude-opus-4-6"
                | "claude-sonnet-5"
                | "claude-sonnet-4-6"
        ),
        "xai" => wire_model_id == "grok-4.5",
        "deepseek" => matches!(
            wire_model_id.as_str(),
            "deepseek-v4-flash" | "deepseek-v4-pro" | "deepseek-v4-flash-vision-exp"
        ),
        // K3 uses the Formula API official-tools channel; K2.6 retains the
        // earlier `$web_search` contract with thinking disabled.
        "moonshot" => matches!(wire_model_id.as_str(), "kimi-k3" | "kimi-k2.6"),
        "modelstudio-token-plan" => matches!(
            wire_model_id.as_str(),
            "qwen3.8-max" | "qwen3.7-plus" | "qwen3.7-max"
        ),
        "zai" => matches!(
            wire_model_id.as_str(),
            "glm-5.3" | "glm-5.3-flash" | "glm-5.2" | "glm-5.1" | "glm-5-turbo"
        ),
        "xiaomi-mimo" => matches!(wire_model_id.as_str(), "mimo-v2.5-pro" | "mimo-v2.5"),
        _ => false,
    };
    if supported {
        CapabilityState::Supported
    } else {
        CapabilityState::Unknown
    }
}

/// Refine the provider/model fact against the exact resolved first-party
/// endpoint. This prevents a compatible gateway, a provider's coding-only
/// plan, or an alternate wire dialect from inheriting a native-search claim
/// that belongs to another product surface.
#[must_use]
pub(crate) fn documented_server_side_web_search_for_route(
    provider: ProviderKind,
    wire_model_id: &str,
    base_url: &str,
) -> CapabilityState {
    let normalized = base_url.trim().trim_end_matches('/').to_ascii_lowercase();
    if !provider_base_url_is_official(provider, base_url) {
        return CapabilityState::Unknown;
    }

    if provider == ProviderKind::Moonshot
        && normalized == DEFAULT_KIMI_CODE_BASE_URL
        && matches!(
            wire_model_id.trim().to_ascii_lowercase().as_str(),
            "k3" | "k3-256k" | "kimi-for-coding" | "kimi-for-coding-highspeed"
        )
    {
        // Kimi Code exposes a provider-owned structured `/search` service;
        // it is independent of the chat model's `$web_search` support.
        return CapabilityState::Supported;
    }

    let model_fact = documented_server_side_web_search(provider.as_str(), wire_model_id);
    if !model_fact.is_supported() {
        return model_fact;
    }

    let exact_product_support = match provider {
        ProviderKind::Deepseek
        | ProviderKind::Openai
        | ProviderKind::Anthropic
        | ProviderKind::Xai
        | ProviderKind::XiaomiMimo => true,
        // Moonshot's Formula/legacy `$web_search` contracts belong to the
        // exact direct API product. Kimi Code's exact `/coding/v1` product is
        // handled above; adjacent coding paths must remain fail-closed.
        ProviderKind::Moonshot => normalized == DEFAULT_MOONSHOT_BASE_URL,
        // Token Plan exposes Harness web_search only on its Responses API.
        // The Anthropic and Coding Plan products do not inherit that fact.
        ProviderKind::ModelstudioTokenPlan => true,
        ProviderKind::ModelstudioTokenPlanAnthropic
        | ProviderKind::ModelstudioCodingPlan
        | ProviderKind::ModelstudioCodingPlanAnthropic => false,
        // Z.AI and Zhipu expose the same structured Web Search contract on
        // their general API products, not on the Coding Plan chat endpoint.
        ProviderKind::Zai => matches!(
            normalized.as_str(),
            "https://api.z.ai/api/paas/v4" | "https://open.bigmodel.cn/api/paas/v4"
        ),
        _ => false,
    };

    if exact_product_support {
        CapabilityState::Supported
    } else {
        CapabilityState::Unknown
    }
}

/// Capability facts owned by one provider/model route offering.
///
/// Fields without a current authoritative catalog source remain `Unknown`.
/// They are present now so live/provider-native facts can be added without
/// changing the candidate contract or guessing from request protocol.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct RouteCapabilities {
    #[serde(default)]
    pub attachments: CapabilityState,
    /// Whether the exact offering explicitly accepts image input.
    #[serde(default)]
    pub image_input: CapabilityState,
    #[serde(default)]
    pub reasoning: CapabilityState,
    #[serde(default)]
    pub native_tool_calls: CapabilityState,
    #[serde(default)]
    pub structured_output: CapabilityState,
    #[serde(default)]
    pub parallel_tool_calls: CapabilityState,
    #[serde(default)]
    pub streaming: CapabilityState,
    #[serde(default)]
    pub prompt_caching: CapabilityState,
    #[serde(default)]
    pub server_side_web_search: CapabilityState,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn optional_boolean_preserves_unknown_and_false() {
        assert_eq!(
            CapabilityState::from_optional_bool(None),
            CapabilityState::Unknown
        );
        assert_eq!(
            CapabilityState::from_optional_bool(Some(false)),
            CapabilityState::Unsupported
        );
        assert_eq!(
            CapabilityState::from_optional_bool(Some(true)),
            CapabilityState::Supported
        );
    }

    #[test]
    fn unsourced_route_capabilities_default_to_unknown() {
        let capabilities = RouteCapabilities::default();
        assert_eq!(capabilities.streaming, CapabilityState::Unknown);
        assert_eq!(
            capabilities.server_side_web_search,
            CapabilityState::Unknown
        );
    }

    #[test]
    fn documented_web_search_is_exact_and_provider_owned() {
        assert_eq!(
            documented_server_side_web_search("xai", "grok-4.5"),
            CapabilityState::Supported
        );
        assert_eq!(
            documented_server_side_web_search("openai", "gpt-5.6"),
            CapabilityState::Supported
        );
        assert_eq!(
            documented_server_side_web_search("anthropic", "claude-sonnet-4-6"),
            CapabilityState::Supported
        );
        assert_eq!(
            documented_server_side_web_search("deepseek", "deepseek-v4-flash"),
            CapabilityState::Supported
        );
        assert_eq!(
            documented_server_side_web_search("moonshot", "kimi-k2.6"),
            CapabilityState::Supported
        );
        assert_eq!(
            documented_server_side_web_search("moonshot", "kimi-k3"),
            CapabilityState::Supported
        );
        assert_eq!(
            documented_server_side_web_search("modelstudio-token-plan", "qwen3.8-max"),
            CapabilityState::Supported
        );
        assert_eq!(
            documented_server_side_web_search("xiaomi-mimo", "mimo-v2.5-pro"),
            CapabilityState::Supported
        );

        for (provider, model) in [
            ("openrouter", "openai/gpt-5.6"),
            ("custom", "gpt-5.6"),
            ("openai", "gpt-5.6-sol"),
            ("xai", "grok-4.5-fast"),
            ("anthropic", "claude-haiku-4-5"),
            ("modelstudio-coding-plan", "qwen3.8-max"),
            ("modelstudio-token-plan", "qwen3.8-max-preview"),
            ("xiaomi-mimo", "mimo-v2.5-pro-ultraspeed"),
        ] {
            assert_eq!(
                documented_server_side_web_search(provider, model),
                CapabilityState::Unknown,
                "{provider}/{model} must not inherit a capability by similarity"
            );
        }
    }

    #[test]
    fn route_fact_rejects_unproven_product_surfaces() {
        assert_eq!(
            documented_server_side_web_search_for_route(
                ProviderKind::Moonshot,
                "kimi-k3",
                "https://api.moonshot.ai/v1",
            ),
            CapabilityState::Supported
        );
        assert_eq!(
            documented_server_side_web_search_for_route(
                ProviderKind::Moonshot,
                "kimi-k2.6",
                "https://api.moonshot.ai/v1",
            ),
            CapabilityState::Supported
        );
        assert_eq!(
            documented_server_side_web_search_for_route(
                ProviderKind::ModelstudioTokenPlan,
                "qwen3.8-max",
                "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1",
            ),
            CapabilityState::Supported
        );
        assert_eq!(
            documented_server_side_web_search_for_route(
                ProviderKind::Moonshot,
                "k3",
                "https://api.kimi.com/coding/v1",
            ),
            CapabilityState::Supported
        );
        for (model, base_url) in [
            ("kimi-k3", "https://api.kimi.com/coding/v2"),
            ("kimi-k2.6", "https://api.kimi.com/coding/v1/preview"),
        ] {
            assert_eq!(
                documented_server_side_web_search_for_route(
                    ProviderKind::Moonshot,
                    model,
                    base_url,
                ),
                CapabilityState::Unknown,
                "adjacent Kimi Code route {base_url} must remain fail-closed"
            );
        }
        assert_eq!(
            documented_server_side_web_search_for_route(
                ProviderKind::ModelstudioCodingPlan,
                "qwen3.8-max",
                "https://coding-intl.dashscope.aliyuncs.com/v1",
            ),
            CapabilityState::Unknown
        );
        assert_eq!(
            documented_server_side_web_search_for_route(
                ProviderKind::Zai,
                "GLM-5.3",
                "https://api.z.ai/api/coding/paas/v4",
            ),
            CapabilityState::Unknown
        );
        assert_eq!(
            documented_server_side_web_search_for_route(
                ProviderKind::Zai,
                "GLM-5.3",
                "https://api.z.ai/api/paas/v4",
            ),
            CapabilityState::Supported
        );
        assert_eq!(
            documented_server_side_web_search_for_route(
                ProviderKind::Zai,
                "GLM-5.3",
                "https://open.bigmodel.cn/api/paas/v4",
            ),
            CapabilityState::Supported
        );
    }
}
