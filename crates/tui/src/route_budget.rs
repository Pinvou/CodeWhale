use codewhale_config::route::RouteLimits;

use crate::config::{ApiProvider, provider_capability};
use crate::context_budget::ContextBudget;
use crate::models::{DEFAULT_COMPACTION_TOKEN_THRESHOLD, context_window_for_model};

/// Output room reserved by the internal budget for large-context reasoning
/// models. This is deliberately larger than the ordinary API request cap so
/// interleaved thinking cannot exhaust the turn budget.
pub(crate) const TURN_MAX_OUTPUT_TOKENS: u32 = 262_144;

/// Safe ordinary API request cap across provider routes.
const API_MAX_OUTPUT_TOKENS: u32 = 65_536;

/// Large windows reserve the full internal reasoning allowance. Smaller
/// windows reserve their route-effective request cap instead.
const INTERNAL_BUDGET_LARGE_WINDOW_THRESHOLD: u32 = 500_000;

/// Preserve only route limits that came from a concrete offering.
#[must_use]
pub(crate) fn known_route_limits(limits: RouteLimits) -> Option<RouteLimits> {
    limits.has_known_limit().then_some(limits)
}

/// Context window for a resolved runtime route.
///
/// Route/offering facts win when known; otherwise this falls back to the
/// existing provider+model capability matrix so startup and custom/local
/// routes keep their previous conservative behavior.
#[must_use]
pub(crate) fn route_context_window_tokens(
    provider: ApiProvider,
    model: &str,
    route_limits: Option<RouteLimits>,
) -> u32 {
    route_limits
        .and_then(|limits| limits.context_tokens)
        .and_then(|tokens| u32::try_from(tokens).ok())
        .filter(|tokens| *tokens > 0)
        .unwrap_or_else(|| provider_capability(provider, model).context_window)
}

/// Provider/offering output cap, when the resolved route reports one.
#[must_use]
pub(crate) fn route_output_limit_tokens(route_limits: Option<RouteLimits>) -> Option<u32> {
    route_limits
        .and_then(|limits| limits.output_tokens)
        .and_then(|tokens| u32::try_from(tokens).ok())
        .filter(|tokens| *tokens > 0)
}

/// Effective `max_tokens` for a model before provider/route caps are applied.
#[must_use]
pub(crate) fn effective_max_output_tokens(model: &str) -> u32 {
    if let Ok(raw) = std::env::var("CODEWHALE_MAX_OUTPUT_TOKENS")
        .or_else(|_| std::env::var("DEEPSEEK_MAX_OUTPUT_TOKENS"))
        && let Ok(tokens) = raw.trim().parse::<u32>()
        && tokens > 0
    {
        return tokens;
    }

    let window = context_window_for_model(model).unwrap_or(128_000);
    if window >= INTERNAL_BUDGET_LARGE_WINDOW_THRESHOLD {
        API_MAX_OUTPUT_TOKENS
    } else {
        (window / 2).min(API_MAX_OUTPUT_TOKENS)
    }
}

/// Conservative request ceiling for a model the static catalogue does not
/// describe at all.
///
/// An absent compatibility cap is not evidence of a large ceiling. Remote
/// OpenAI-compatible routes serving an unrecognized wire alias frequently
/// publish a much lower `max_tokens` maximum and reject anything above it, so
/// an uncatalogued id keeps this floor rather than inheriting the full
/// [`API_MAX_OUTPUT_TOKENS`] request cap.
const UNCATALOGUED_COMPAT_MAX_OUTPUT_TOKENS: u32 = 8_192;

/// Why a route's compatibility output ceiling has the value it does.
///
/// Carried so a clamp is always attributable: "unknown" is only allowed to
/// mean "no clamp" when a route *truthfully publishes no ceiling*, never when
/// the catalogue simply has no row for the model.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum OutputCeilingSource {
    /// The static catalogue publishes an exact/conservative ceiling.
    Documented(u32),
    /// The route is known to publish no output maximum we can stand behind
    /// (Kimi Code membership ids, operator-owned self-hosted engines). Unknown
    /// stays unknown and nothing is clamped.
    RouteDeclaredUnknown,
    /// The catalogue has no row for this model. Fail closed to a conservative
    /// ceiling rather than treating absence as permission.
    Uncatalogued(u32),
}

impl OutputCeilingSource {
    /// The ceiling to intersect a requested cap with, if any.
    #[must_use]
    pub(crate) const fn clamp_tokens(self) -> Option<u32> {
        match self {
            Self::Documented(tokens) | Self::Uncatalogued(tokens) => Some(tokens),
            Self::RouteDeclaredUnknown => None,
        }
    }
}

/// Whether an absent compatibility ceiling is a *declared* unknown for this
/// route, rather than a gap in the catalogue.
///
/// Deliberately an allowlist. Everything not named here is uncatalogued and
/// gets the conservative ceiling.
#[must_use]
fn route_declares_unknown_output_ceiling(provider: ApiProvider, model: &str) -> bool {
    match provider {
        // Operator-owned engines: the local server, not this process, owns the
        // output ceiling, and it is routinely far above any catalogue row.
        ApiProvider::Ollama | ApiProvider::Sglang | ApiProvider::Vllm => true,
        // Pinvou fork: custom OpenAI-compatible endpoints (the `openai` wire
        // route with a user-configured base_url) are operator-owned routes
        // too — the user configures the endpoint and owns the ceiling. An
        // unregistered alias there aligns with the window heuristic
        // (requested_cap ≤ 65536) instead of the conservative 4096/8192
        // uncatalogued clamp, so 128K-window endpoints keep 64K output room.
        ApiProvider::Openai => true,
        // Kimi Code membership ids publish their limits in the membership
        // catalog rather than the static model catalogue.
        ApiProvider::Moonshot => crate::config::is_kimi_code_membership_model(model),
        _ => false,
    }
}

/// Resolve the compatibility output ceiling for a route, with its provenance.
#[must_use]
pub(crate) fn output_ceiling_source(provider: ApiProvider, model: &str) -> OutputCeilingSource {
    provider_capability(provider, model).max_output.map_or_else(
        || {
            if route_declares_unknown_output_ceiling(provider, model) {
                OutputCeilingSource::RouteDeclaredUnknown
            } else {
                OutputCeilingSource::Uncatalogued(UNCATALOGUED_COMPAT_MAX_OUTPUT_TOKENS)
            }
        },
        OutputCeilingSource::Documented,
    )
}

/// Effective request output cap for a fully resolved provider/model route.
#[must_use]
pub(crate) fn effective_max_output_tokens_for_route(
    provider: ApiProvider,
    model: &str,
    route_limits: Option<RouteLimits>,
) -> u32 {
    let requested_cap = effective_max_output_tokens(model);
    let compatibility_cap = output_ceiling_source(provider, model).clamp_tokens();
    let route_cap = route_output_limit_tokens(route_limits);
    // Unknown means unknown only where a route *declares* it: membership ids
    // such as the `kimi-for-coding` family, and operator-owned self-hosted
    // engines. For those there is nothing to clamp against and the requested
    // cap stands. A model the catalogue simply has no row for is not the same
    // fact — absence is not permission, so it keeps a conservative ceiling
    // (see `output_ceiling_source`). Only a concrete route/offering maximum
    // narrows it further; known compatibility caps stay authoritative and are
    // still intersected with any route maximum.
    let cap = compatibility_cap.map_or(requested_cap, |compat| requested_cap.min(compat));
    let cap = route_cap.map_or(cap, |route_cap| cap.min(route_cap));
    let Some(window) = route_limits
        .and_then(|limits| limits.context_tokens)
        .and_then(|tokens| u32::try_from(tokens).ok())
        .filter(|tokens| *tokens > 0)
    else {
        return cap;
    };

    u32::try_from(ContextBudget::new(u64::from(window), 0, u64::from(cap)).output_cap_tokens)
        .unwrap_or(cap)
        .max(1)
}

/// Output reservation used by the internal input budget for a route.
#[must_use]
pub(crate) fn route_output_reservation_for_window(
    provider: ApiProvider,
    model: &str,
    window_tokens: u32,
    route_limits: Option<RouteLimits>,
) -> u32 {
    if window_tokens >= INTERNAL_BUDGET_LARGE_WINDOW_THRESHOLD {
        route_output_limit_tokens(route_limits).map_or(TURN_MAX_OUTPUT_TOKENS, |route_cap| {
            route_cap.min(TURN_MAX_OUTPUT_TOKENS)
        })
    } else {
        effective_max_output_tokens_for_route(provider, model, route_limits)
    }
}

#[must_use]
pub(crate) fn route_context_budget(
    provider: ApiProvider,
    model: &str,
    route_limits: Option<RouteLimits>,
    input_tokens: usize,
) -> Option<ContextBudget> {
    let window = route_context_window_tokens(provider, model, route_limits);
    let output_cap = route_output_reservation_for_window(provider, model, window, route_limits);
    Some(ContextBudget::new(
        u64::from(window),
        u64::try_from(input_tokens).ok()?,
        u64::from(output_cap),
    ))
}

#[must_use]
pub(crate) fn compaction_threshold_for_route_at_percent(
    provider: ApiProvider,
    model: &str,
    route_limits: Option<RouteLimits>,
    percent: f64,
) -> usize {
    route_context_budget(provider, model, route_limits, 0)
        .and_then(|budget| {
            usize::try_from(budget.compaction_trigger_for_percent(percent.clamp(10.0, 100.0))).ok()
        })
        .unwrap_or(DEFAULT_COMPACTION_TOKEN_THRESHOLD)
}

#[must_use]
pub(crate) fn auto_compact_default_for_route(
    provider: ApiProvider,
    model: &str,
    route_limits: Option<RouteLimits>,
) -> bool {
    // Every resolved route has either concrete offering limits or a
    // conservative provider/model fallback. Large windows need continuity too;
    // their size is not a reason to disable compaction entirely.
    route_context_window_tokens(provider, model, route_limits) > 0
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Absence of a catalogue row is not evidence of a large ceiling. An
    /// unrecognized wire alias on a remote OpenAI-compatible route keeps the
    /// conservative compatibility ceiling, with an attributable source.
    /// (Pinvou fork: `Openai` itself is now operator-owned-unknown; this test
    /// pins the conservative clamp on a provider still outside the allowlist.)
    #[test]
    fn uncatalogued_remote_model_keeps_a_conservative_ceiling() {
        let source = output_ceiling_source(ApiProvider::Novita, "totally-unknown-alias-v9");
        assert_eq!(
            source,
            OutputCeilingSource::Uncatalogued(UNCATALOGUED_COMPAT_MAX_OUTPUT_TOKENS)
        );
        assert_eq!(
            source.clamp_tokens(),
            Some(UNCATALOGUED_COMPAT_MAX_OUTPUT_TOKENS)
        );
        assert!(
            effective_max_output_tokens_for_route(
                ApiProvider::Novita,
                "totally-unknown-alias-v9",
                None
            ) <= UNCATALOGUED_COMPAT_MAX_OUTPUT_TOKENS
        );
    }

    /// Routes that *declare* an unknown ceiling still avoid the clamp.
    #[test]
    fn route_declared_unknown_ceilings_are_not_clamped() {
        for (provider, model) in [
            (ApiProvider::Moonshot, "kimi-for-coding"),
            (ApiProvider::Moonshot, "kimi-for-coding-highspeed"),
            (ApiProvider::Ollama, "some-local-build"),
            // Pinvou fork: custom OpenAI-compatible endpoints are operator-owned.
            (ApiProvider::Openai, "totally-unregistered-cloud-model"),
        ] {
            assert_eq!(
                output_ceiling_source(provider, model),
                OutputCeilingSource::RouteDeclaredUnknown,
                "{provider:?}/{model} must declare its unknown ceiling"
            );
            assert_eq!(output_ceiling_source(provider, model).clamp_tokens(), None);
        }
        // Bare `k3` is a membership id, but unlike the `kimi-for-coding`
        // family the K3 quickstart documents its output maximum, and the model
        // catalogue carries it. A documented ceiling is authoritative — the
        // membership allowlist only covers ids the catalogue has nothing to
        // say about, and must not turn a real fact back into an unknown.
        assert_eq!(
            output_ceiling_source(ApiProvider::Moonshot, "k3"),
            OutputCeilingSource::Documented(131_072)
        );
    }

    #[test]
    fn codex_missing_route_metadata_uses_provider_context_floor() {
        assert_eq!(
            route_context_window_tokens(ApiProvider::OpenaiCodex, "gpt-5.5", None),
            128_000
        );
        assert_eq!(
            compaction_threshold_for_route_at_percent(
                ApiProvider::OpenaiCodex,
                "gpt-5.5",
                None,
                80.0,
            ),
            98_304
        );
        assert!(auto_compact_default_for_route(
            ApiProvider::OpenaiCodex,
            "gpt-5.5",
            None,
        ));
    }

    #[test]
    fn v4_trigger_is_anchored_to_spendable_input() {
        let budget = route_context_budget(ApiProvider::Deepseek, "deepseek-v4-pro", None, 0)
            .expect("V4 route budget");

        assert_eq!(budget.window_tokens, 1_000_000);
        assert_eq!(budget.output_cap_tokens, u64::from(TURN_MAX_OUTPUT_TOKENS));
        assert_eq!(budget.input_budget_ceiling, 736_832);
        assert_eq!(
            compaction_threshold_for_route_at_percent(
                ApiProvider::Deepseek,
                "deepseek-v4-pro",
                None,
                80.0,
            ),
            589_466
        );
    }

    #[test]
    fn kimi_k3_defaults_auto_compaction_on() {
        assert!(auto_compact_default_for_route(
            ApiProvider::Moonshot,
            "kimi-k3",
            None,
        ));
    }

    #[test]
    fn kimi_catalog_output_ceiling_preserves_input_budget() {
        let _lock = crate::test_support::lock_test_env();
        let _max_output = crate::test_support::EnvVarGuard::remove("DEEPSEEK_MAX_OUTPUT_TOKENS");
        // #4368/#4378: Models.dev may report Kimi's full 262K context as both
        // context and output ceilings. On a sub-500K window, reserve the
        // route-effective 32K request cap rather than treating that catalog
        // maximum as the amount every turn will emit.
        let limits = RouteLimits {
            context_tokens: Some(262_144),
            output_tokens: Some(262_144),
            ..RouteLimits::default()
        };
        let budget = route_context_budget(ApiProvider::Moonshot, "kimi-k2.7-code", Some(limits), 0)
            .expect("Kimi route budget");
        let trigger = compaction_threshold_for_route_at_percent(
            ApiProvider::Moonshot,
            "kimi-k2.7-code",
            Some(limits),
            80.0,
        );

        assert_eq!(budget.output_cap_tokens, 32_768);
        assert_eq!(budget.input_budget_ceiling, 228_352);
        assert_eq!(trigger, 182_682);
        assert!(trigger as u64 <= budget.input_budget_ceiling);
        assert!(
            trigger < 209_715,
            "must fire before the old window-relative trigger"
        );
    }

    #[test]
    fn explicit_route_output_limit_beats_unknown_model_name_fallback() {
        let _lock = crate::test_support::lock_test_env();
        let _max_output =
            crate::test_support::EnvVarGuard::set("CODEWHALE_MAX_OUTPUT_TOKENS", "65536");
        let limits = RouteLimits {
            context_tokens: Some(262_144),
            output_tokens: Some(24_576),
            ..RouteLimits::default()
        };

        assert_eq!(
            effective_max_output_tokens_for_route(
                ApiProvider::Vllm,
                "arbitrary-local-wire-alias",
                Some(limits),
            ),
            24_576
        );
        assert_eq!(
            effective_max_output_tokens_for_route(
                ApiProvider::Vllm,
                "arbitrary-local-wire-alias",
                None,
            ),
            65_536,
            "an unknown compatibility cap must not clamp; only the requested cap applies"
        );
        assert_eq!(
            effective_max_output_tokens_for_route(
                ApiProvider::Vllm,
                "kimi-k2.7-code",
                Some(RouteLimits {
                    output_tokens: Some(262_144),
                    ..RouteLimits::default()
                }),
            ),
            32_768,
            "known model caps must remain authoritative on self-hosted routes"
        );
    }

    /// #4368 follow-up: the Kimi Code membership ids deliberately have no
    /// static output cap (the membership catalog owns their limits). The old
    /// generic `unwrap_or(4096)` in `provider_capability` turned that unknown
    /// into a hard 4K clamp here, silently truncating every offline membership
    /// turn. Unknown must mean "no compatibility clamp".
    #[test]
    fn kimi_membership_unknown_output_cap_does_not_clamp_to_4k() {
        let _lock = crate::test_support::lock_test_env();
        let _codewhale = crate::test_support::EnvVarGuard::remove("CODEWHALE_MAX_OUTPUT_TOKENS");
        let _deepseek = crate::test_support::EnvVarGuard::remove("DEEPSEEK_MAX_OUTPUT_TOKENS");

        for model in ["kimi-for-coding", "kimi-for-coding-highspeed"] {
            assert_eq!(
                provider_capability(ApiProvider::Moonshot, model).max_output,
                None,
                "{model}: membership output ceiling must stay unknown, not a placeholder"
            );

            let cap = effective_max_output_tokens_for_route(ApiProvider::Moonshot, model, None);
            assert_eq!(
                cap,
                effective_max_output_tokens(model),
                "{model}: unknown compatibility cap must leave the requested cap intact"
            );
            assert_ne!(cap, 4_096, "{model}: must not inherit the old 4K fallback");
            // No invented sentinel ceiling either.
            assert_ne!(cap, u32::MAX);
            assert_ne!(cap, 32_768);
        }
    }

    /// A concrete membership offering limit is still authoritative — "unknown
    /// means no clamp" must not become "never clamp".
    #[test]
    fn kimi_membership_route_limit_still_caps_output() {
        let _lock = crate::test_support::lock_test_env();
        let _codewhale = crate::test_support::EnvVarGuard::remove("CODEWHALE_MAX_OUTPUT_TOKENS");
        let _deepseek = crate::test_support::EnvVarGuard::remove("DEEPSEEK_MAX_OUTPUT_TOKENS");

        let limits = RouteLimits {
            context_tokens: Some(262_144),
            output_tokens: Some(16_384),
            ..RouteLimits::default()
        };
        assert_eq!(
            effective_max_output_tokens_for_route(
                ApiProvider::Moonshot,
                "kimi-for-coding",
                Some(limits),
            ),
            16_384
        );
    }

    /// GLM and MiniMax publish real output ceilings; those stay authoritative
    /// so relaxing the unknown case cannot leak into known routes.
    #[test]
    fn known_glm_and_minimax_output_caps_remain_authoritative() {
        let _lock = crate::test_support::lock_test_env();
        let _codewhale = crate::test_support::EnvVarGuard::remove("CODEWHALE_MAX_OUTPUT_TOKENS");
        let _deepseek = crate::test_support::EnvVarGuard::remove("DEEPSEEK_MAX_OUTPUT_TOKENS");

        // GLM 5.2: 1M window, documented 131K output. The requested cap is the
        // 65,536 API ceiling, so the known cap is above it and does not bind —
        // what matters is that the capability is *known*.
        let glm = provider_capability(ApiProvider::Zai, "glm-5.2");
        assert_eq!(glm.max_output, Some(131_072));

        let minimax = provider_capability(ApiProvider::Minimax, "minimax-m3");
        assert_eq!(minimax.max_output, Some(524_288));

        // A known cap below the requested cap must still clamp.
        assert_eq!(
            effective_max_output_tokens_for_route(ApiProvider::Moonshot, "kimi-k2.7-code", None),
            32_768,
        );
    }
}
