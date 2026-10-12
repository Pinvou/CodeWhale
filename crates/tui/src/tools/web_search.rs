//! Bounded provider-native/configured web search with explicit fallback receipts.
//! Adapters include Firecrawl, Tavily, Bocha, Metaso, SearXNG, Baidu,
//! Volcengine, and Sofya; browsing remains a separate `web.run` workflow.
//! `[search]` example:
//!   provider = "firecrawl"  # keyless on Firecrawl Cloud; optional api_key
//!   base_url = `"https://search.example/"`  # DDG-compatible URL or SearXNG instance

use super::spec::{
    ApprovalRequirement, ToolCapability, ToolContext, ToolError, ToolResult, ToolSpec, optional_u64,
};
use crate::config::SearchProvider;
use crate::network_policy::{Decision, NetworkPolicyDecider};
use async_trait::async_trait;
use regex::Regex;
use serde::Serialize;
use serde_json::{Value, json};
use std::sync::OnceLock;
use std::time::{Duration, Instant};

use super::web::backend::SearchBackendChain;
use super::web::cache;
use super::web::contract::{
    BackendId, BackendSearch, DEFAULT_SEARCH_RESULTS, DEFAULT_SEARCH_TIMEOUT_MS, DegradedReason,
    HonoredQueryCapabilities, MAX_SEARCH_RESULTS, MAX_SEARCH_TIMEOUT_MS, QueryKnob, Recency,
    SearchQuery, SearchReceipt, SearchResponse, SearchResult,
};
use super::web::scrape::{
    BROWSER_USER_AGENT as USER_AGENT, ScrapedSearchResult, is_duckduckgo_challenge,
    parse_bing_results as scrape_bing_results,
    parse_duckduckgo_results as scrape_duckduckgo_results,
};

const DUCKDUCKGO_ENDPOINT: &str = "https://html.duckduckgo.com/html/";
const BING_HOST: &str = "www.bing.com";
const BING_ENDPOINT: &str = "https://www.bing.com/search";
const FIRECRAWL_ENDPOINT: &str = "https://api.firecrawl.dev/v2/search";
const TAVILY_ENDPOINT: &str = "https://api.tavily.com/search";
const BOCHA_ENDPOINT: &str = "https://api.bochaai.com/v1/web-search";
const METASO_ENDPOINT: &str = "https://metaso.cn/api/v1";
const BAIDU_ENDPOINT: &str = "https://qianfan.baidubce.com/v2/ai_search/web_search";
const VOLCENGINE_RESPONSES_ENDPOINT: &str = "https://ark.cn-beijing.volces.com/api/v3/responses";
const SOFYA_ENDPOINT: &str = "https://sofya.co/v1/search";
const ERROR_BODY_PREVIEW_BYTES: usize = 512;
const PROVIDER_NATIVE_MIN_TIMEOUT_MS: u64 = 45_000;
const KIMI_K3_FORMULA_MIN_TIMEOUT_MS: u64 = 180_000;
const VOLCENGINE_MIN_TIMEOUT_MS: u64 = 90_000;

/// Credential-free endpoint selected for an explicit doctor reachability
/// probe. The ordinary search request builders remain the source of truth for
/// provider endpoints; doctor borrows those endpoints without constructing a
/// query or reading an API key.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct SearchProbeTarget {
    pub(crate) url: reqwest::Url,
    pub(crate) host: String,
}

/// Safe configuration failures for a search reachability probe.
///
/// These variants deliberately carry no configured URL: userinfo, paths, and
/// query strings may contain credentials and must never be echoed by doctor.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum SearchProbeTargetError {
    Missing,
    Unsupported,
    Invalid,
}

/// Resolve the configured provider's transport endpoint for doctor.
///
/// Built-in endpoints keep their known request path. User-configured
/// DuckDuckGo-compatible and SearXNG URLs are reduced to their HTTP(S)
/// authority so the probe cannot transmit userinfo, path/query credentials,
/// or a real search query.
pub(crate) fn search_probe_target(
    provider: SearchProvider,
    base_url: Option<&str>,
) -> Result<SearchProbeTarget, SearchProbeTargetError> {
    let configured_base_url = configured_search_base_url(base_url);
    if configured_base_url.is_some()
        && !matches!(
            provider,
            SearchProvider::DuckDuckGo | SearchProvider::Searxng
        )
    {
        return Err(SearchProbeTargetError::Unsupported);
    }

    let (raw, configured) = match provider {
        SearchProvider::Bing => (BING_ENDPOINT, false),
        SearchProvider::DuckDuckGo => (
            configured_base_url.unwrap_or(DUCKDUCKGO_ENDPOINT),
            configured_base_url.is_some(),
        ),
        SearchProvider::Firecrawl => (FIRECRAWL_ENDPOINT, false),
        SearchProvider::Tavily => (TAVILY_ENDPOINT, false),
        SearchProvider::Bocha => (BOCHA_ENDPOINT, false),
        SearchProvider::Metaso => (METASO_ENDPOINT, false),
        SearchProvider::Searxng => (
            configured_base_url.ok_or(SearchProbeTargetError::Missing)?,
            true,
        ),
        SearchProvider::Baidu => (BAIDU_ENDPOINT, false),
        SearchProvider::Volcengine => (VOLCENGINE_RESPONSES_ENDPOINT, false),
        SearchProvider::Sofya => (SOFYA_ENDPOINT, false),
    };

    let mut url = reqwest::Url::parse(raw).map_err(|_| SearchProbeTargetError::Invalid)?;
    if !matches!(url.scheme(), "http" | "https") || url.host_str().is_none() {
        return Err(SearchProbeTargetError::Invalid);
    }

    url.set_fragment(None);
    url.set_query(None);
    if configured {
        url.set_username("")
            .map_err(|_| SearchProbeTargetError::Invalid)?;
        url.set_password(None)
            .map_err(|_| SearchProbeTargetError::Invalid)?;
        url.set_path("/");
    }
    let host = url
        .host_str()
        .ok_or(SearchProbeTargetError::Invalid)?
        .to_string();

    Ok(SearchProbeTarget { url, host })
}

/// Returns `Ok(())` if the policy allows the call, or a `ToolError` otherwise.
/// Falls through silently when no policy is attached (back-compat).
pub(crate) fn check_policy(
    decider: Option<&NetworkPolicyDecider>,
    host: &str,
) -> Result<(), ToolError> {
    let Some(decider) = decider else {
        return Ok(());
    };
    match decider.evaluate(host, "web_search") {
        Decision::Allow => Ok(()),
        Decision::Deny => Err(ToolError::permission_denied(format!(
            "web search to '{host}' blocked by network policy"
        ))),
        Decision::Prompt => Err(ToolError::permission_denied(format!(
            "web search to '{host}' requires approval; \
             re-run after `/network allow {host}` or set network.default = \"allow\" in config"
        ))),
    }
}

// Cached regex for secret redaction in error bodies
static BEARER_TOKEN_RE: OnceLock<Regex> = OnceLock::new();

fn get_bearer_token_re() -> &'static Regex {
    BEARER_TOKEN_RE.get_or_init(|| {
        Regex::new(r"(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+")
            .expect("bearer token regex pattern is valid")
    })
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
struct WebSearchEntry {
    title: String,
    url: String,
    snippet: Option<String>,
}

pub struct WebSearchTool;

#[async_trait]
impl ToolSpec for WebSearchTool {
    fn name(&self) -> &'static str {
        "web_search"
    }

    fn model_visible(&self) -> bool {
        false
    }

    fn description(&self) -> &'static str {
        "Search the web and return ranked results with URLs, snippets, session-scoped ref_ids, and an execution receipt. Open a result ref_id with `web.run` when the short summary is not enough; fetch only the few sources needed. When the exact active route reports a documented first-party server-side search tool, it is tried first; otherwise keyless Firecrawl is the default. Configured API backends visibly degrade directly to keyless Bing when unavailable, and every hop is recorded. Configuration and network-policy errors fail closed. Explicit Bing and private DuckDuckGo-compatible routes do not cross providers. Set `[search] provider = \"firecrawl\" | \"bing\" | \"duckduckgo\" | \"tavily\" | \"bocha\" | \"metaso\" | \"searxng\" | \"baidu\" | \"volcengine\" | \"sofya\"` in config.toml. Firecrawl Cloud works keyless with a bounded quota. For a known canonical URL, prefer the Web tool with action=fetch."
    }

    fn input_schema(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "query": {
                    "type": "string",
                    "description": "Search query. Compatibility aliases: q, or search_query[0].q."
                },
                "q": {
                    "type": "string",
                    "description": "Search query."
                },
                "search_query": {
                    "type": "array",
                    "description": "Array form for advanced queries: [{\"q\":\"...\", \"max_results\": 5}]",
                    "items": {
                        "type": "object",
                        "properties": {
                            "q": { "type": "string" },
                            "query": { "type": "string" },
                            "max_results": { "type": "integer" },
                            "recency": {
                                "oneOf": [
                                    { "type": "string", "enum": ["day", "week", "month", "year"] },
                                    { "type": "integer", "minimum": 1, "maximum": 3650 }
                                ]
                            },
                            "domains": { "type": "array", "items": { "type": "string" } },
                            "locale": { "type": "string" }
                        }
                    }
                },
                "max_results": {
                    "type": "integer",
                    "description": "Maximum number of results to return (default: 5, max: 10)"
                },
                "timeout_ms": {
                    "type": "integer",
                    "description": "Configured/local search timeout in milliseconds (default: 15000, max: 60000). Model-backed provider-native search has a separate bounded minimum before fallback."
                },
                "recency": {
                    "oneOf": [
                        { "type": "string", "enum": ["day", "week", "month", "year"] },
                        { "type": "integer", "minimum": 1, "maximum": 3650 }
                    ],
                    "description": "Requested freshness window. Unsupported backends report it as degraded instead of silently ignoring it."
                },
                "domains": {
                    "type": "array",
                    "items": { "type": "string" },
                    "description": "Restrict returned results to these domains. Backends without native support report post-filtering."
                },
                "locale": {
                    "type": "string",
                    "description": "Requested result locale as a BCP 47-style tag such as zh-CN or ja-JP; malformed values are ignored. The keyless Bing scrape honors it via its mkt/setlang market parameters; the keyless DuckDuckGo scrape maps a fixed region list and reports malformed or unmapped regions as ignored; unsupported backends report it as degraded."
                }
            }
        })
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        vec![ToolCapability::ReadOnly, ToolCapability::Network]
    }

    fn approval_requirement(&self) -> ApprovalRequirement {
        ApprovalRequirement::Auto
    }

    fn supports_parallel(&self) -> bool {
        true
    }

    async fn execute(&self, input: Value, context: &ToolContext) -> Result<ToolResult, ToolError> {
        let query = search_query_from_input(&input)?;
        let timeout_ms = optional_u64(&input, "timeout_ms", DEFAULT_SEARCH_TIMEOUT_MS)?
            .min(MAX_SEARCH_TIMEOUT_MS);
        let response = execute_search(query, timeout_ms, context).await?;
        ToolResult::json(&response).map_err(|error| ToolError::execution_failed(error.to_string()))
    }
}

impl WebSearchTool {
    async fn run_firecrawl_search(
        &self,
        query: &str,
        max_results: usize,
        timeout_ms: u64,
        context: &ToolContext,
    ) -> Result<(Vec<WebSearchEntry>, String), ToolError> {
        let env_key = std::env::var("FIRECRAWL_API_KEY").ok();
        self.run_firecrawl_search_at(
            FIRECRAWL_ENDPOINT,
            query,
            max_results,
            timeout_ms,
            context.search_api_key.as_deref().or(env_key.as_deref()),
        )
        .await
    }

    async fn run_firecrawl_search_at(
        &self,
        endpoint: &str,
        query: &str,
        max_results: usize,
        timeout_ms: u64,
        api_key: Option<&str>,
    ) -> Result<(Vec<WebSearchEntry>, String), ToolError> {
        let client = crate::tls::reqwest_client_builder()
            .timeout(Duration::from_millis(timeout_ms))
            .user_agent(USER_AGENT)
            .build()
            .map_err(|e| {
                ToolError::execution_failed(format!("Failed to build HTTP client: {e}"))
            })?;
        let api_key = api_key.map(str::trim).filter(|key| !key.is_empty());
        let payload = json!({
            "query": query,
            "limit": max_results,
            "sources": [{"type": "web"}],
        });
        let mut request = client.post(endpoint).json(&payload);
        if let Some(key) = api_key {
            request = request.bearer_auth(key);
        }
        let response = request.send().await.map_err(|e| {
            ToolError::execution_failed(format!("Firecrawl search request failed: {e}"))
        })?;
        let status = response.status();
        let body = response.text().await.map_err(|e| {
            ToolError::execution_failed(format!("Failed to read Firecrawl response: {e}"))
        })?;
        if !status.is_success() {
            let message = match status.as_u16() {
                401 | 403 if api_key.is_none() => "Firecrawl rejected keyless search; set `[search] api_key` or FIRECRAWL_API_KEY".to_string(),
                401 | 403 => "Firecrawl authentication was rejected; check `[search] api_key` or FIRECRAWL_API_KEY".to_string(),
                429 if api_key.is_none() => "Firecrawl keyless quota is exhausted; retry later or set `[search] api_key` / FIRECRAWL_API_KEY".to_string(),
                429 => "Firecrawl quota is exhausted; retry later or check the configured account limits".to_string(),
                code => format!("Firecrawl search failed: HTTP {code} — {}", truncate_error_body(&body)),
            };
            return Err(ToolError::execution_failed(message));
        }
        let parsed: Value = serde_json::from_str(&body).map_err(|e| {
            ToolError::execution_failed(format!("Failed to parse Firecrawl response: {e}"))
        })?;
        if parsed.get("success").and_then(Value::as_bool) == Some(false) {
            let detail = first_non_empty_string(&parsed, &["error", "message"])
                .unwrap_or_else(|| "unknown API error".to_string());
            return Err(ToolError::execution_failed(format!(
                "Firecrawl search failed: {detail}"
            )));
        }
        let mode = if api_key.is_some() {
            "authenticated"
        } else {
            "keyless"
        };
        Ok((
            parse_firecrawl_results(&parsed, max_results),
            format!("Firecrawl {mode}"),
        ))
    }

    /// Search a configured SearXNG JSON API; no public instance is assumed.
    async fn run_searxng_search(
        &self,
        query: &str,
        max_results: usize,
        timeout_ms: u64,
        context: &ToolContext,
    ) -> Result<(Vec<WebSearchEntry>, String), ToolError> {
        let (url, host) = searxng_search_url(context.search_base_url.as_deref(), query)?;
        check_policy(context.network_policy.as_ref(), &host)?;

        let client = crate::tls::reqwest_client_builder()
            .timeout(Duration::from_millis(timeout_ms))
            .user_agent(USER_AGENT)
            .build()
            .map_err(|e| {
                ToolError::execution_failed(format!("Failed to build HTTP client: {e}"))
            })?;

        let resp = client
            .get(&url)
            .header("Accept", "application/json")
            .send()
            .await
            .map_err(|e| {
                ToolError::execution_failed(format!("SearXNG search request to {host} failed: {e}"))
            })?;

        let status = resp.status();
        let body = resp.text().await.map_err(|e| {
            ToolError::execution_failed(format!("Failed to read SearXNG response from {host}: {e}"))
        })?;

        if !status.is_success() {
            let truncated = truncate_error_body(&body);
            let msg = match status.as_u16() {
                403 => format!(
                    "SearXNG search failed: HTTP 403 from {host}. Check that JSON output is enabled and this instance permits API access. {truncated}"
                ),
                429 => format!(
                    "SearXNG search failed: HTTP 429 from {host}. The configured instance is rate-limiting requests; use a trusted/self-hosted instance or retry later. {truncated}"
                ),
                code => format!("SearXNG search failed: HTTP {code} from {host}. {truncated}"),
            };
            return Err(ToolError::execution_failed(msg));
        }

        let parsed: serde_json::Value = serde_json::from_str(&body).map_err(|e| {
            ToolError::execution_failed(format!(
                "Failed to parse SearXNG JSON response from {host}: {e}. Ensure the instance supports format=json and JSON output is enabled."
            ))
        })?;

        Ok((parse_searxng_results(&parsed, max_results), host))
    }

    /// Search via Tavily AI Search API (<https://tavily.com>).
    async fn run_tavily_search(
        &self,
        query: &str,
        max_results: usize,
        timeout_ms: u64,
        context: &ToolContext,
    ) -> Result<Vec<WebSearchEntry>, ToolError> {
        let api_key = context
            .search_api_key
            .as_deref()
            .ok_or_else(|| {
                ToolError::execution_failed(
                    "Tavily search requires an API key. Set `[search] api_key = \"tvly-...\"` in config.toml.",
                )
            })?;

        let client = crate::tls::reqwest_client_builder()
            .timeout(Duration::from_millis(timeout_ms))
            .build()
            .map_err(|e| {
                ToolError::execution_failed(format!("Failed to build HTTP client: {e}"))
            })?;

        let payload = json!({
            "api_key": api_key, // noqa: api-key-in-body
            "query": query,
            "search_depth": "basic",
            "max_results": max_results,
        });

        let resp = client
            .post(TAVILY_ENDPOINT)
            .header("Content-Type", "application/json")
            .json(&payload)
            .send()
            .await
            .map_err(|e| {
                ToolError::execution_failed(format!("Tavily search request failed: {e}"))
            })?;

        let status = resp.status();
        let body = resp.text().await.map_err(|e| {
            ToolError::execution_failed(format!("Failed to read Tavily response: {e}"))
        })?;

        if !status.is_success() {
            let truncated = truncate_error_body(&body);
            return Err(ToolError::execution_failed(format!(
                "Tavily search failed: HTTP {} — {truncated}",
                status.as_u16()
            )));
        }

        let parsed: serde_json::Value = serde_json::from_str(&body).map_err(|e| {
            ToolError::execution_failed(format!("Failed to parse Tavily response: {e}"))
        })?;

        Ok(parse_tavily_results(&parsed, max_results))
    }

    /// Search Sofya; it returns extracted content and accepts `SOFYA_API_KEY`.
    async fn run_sofya_search(
        &self,
        query: &str,
        max_results: usize,
        timeout_ms: u64,
        context: &ToolContext,
    ) -> Result<Vec<WebSearchEntry>, ToolError> {
        let env_key = std::env::var("SOFYA_API_KEY").ok();
        let api_key = context
            .search_api_key
            .as_deref()
            .or(env_key.as_deref())
            .ok_or_else(|| {
                ToolError::execution_failed(
                    "Sofya search requires an API key. Set `[search] api_key = \"ay_live_...\"` in config.toml or the SOFYA_API_KEY env var.",
                )
            })?;

        let client = crate::tls::reqwest_client_builder()
            .timeout(Duration::from_millis(timeout_ms))
            .build()
            .map_err(|e| {
                ToolError::execution_failed(format!("Failed to build HTTP client: {e}"))
            })?;

        let payload = json!({
            "query": query,
            "max_results": max_results,
        });

        let resp = client
            .post(SOFYA_ENDPOINT)
            .header("Content-Type", "application/json")
            .bearer_auth(api_key)
            .json(&payload)
            .send()
            .await
            .map_err(|e| {
                ToolError::execution_failed(format!("Sofya search request failed: {e}"))
            })?;

        let status = resp.status();
        let body = resp.text().await.map_err(|e| {
            ToolError::execution_failed(format!("Failed to read Sofya response: {e}"))
        })?;

        if !status.is_success() {
            let truncated = truncate_error_body(&body);
            return Err(ToolError::execution_failed(format!(
                "Sofya search failed: HTTP {} — {truncated}",
                status.as_u16()
            )));
        }

        let parsed: serde_json::Value = serde_json::from_str(&body).map_err(|e| {
            ToolError::execution_failed(format!("Failed to parse Sofya response: {e}"))
        })?;

        Ok(parse_sofya_results(&parsed, max_results))
    }

    /// Search via Bocha AI Search API (<https://bochaai.com>).
    async fn run_bocha_search(
        &self,
        query: &str,
        max_results: usize,
        timeout_ms: u64,
        context: &ToolContext,
    ) -> Result<Vec<WebSearchEntry>, ToolError> {
        let api_key = context
            .search_api_key
            .as_deref()
            .ok_or_else(|| {
                ToolError::execution_failed(
                    "Bocha search requires an API key. Set `[search] api_key = \"sk-...\"` in config.toml.",
                )
            })?;

        let client = crate::tls::reqwest_client_builder()
            .timeout(Duration::from_millis(timeout_ms))
            .build()
            .map_err(|e| {
                ToolError::execution_failed(format!("Failed to build HTTP client: {e}"))
            })?;

        let payload = json!({
            "query": query,
            "freshness": "noLimit",
            "count": max_results,
        });

        let resp = client
            .post(BOCHA_ENDPOINT)
            .header("Content-Type", "application/json")
            .header("Authorization", format!("Bearer {api_key}"))
            .json(&payload)
            .send()
            .await
            .map_err(|e| {
                ToolError::execution_failed(format!("Bocha search request failed: {e}"))
            })?;

        let status = resp.status();
        let body = resp.text().await.map_err(|e| {
            ToolError::execution_failed(format!("Failed to read Bocha response: {e}"))
        })?;

        if !status.is_success() {
            let truncated = truncate_error_body(&body);
            return Err(ToolError::execution_failed(format!(
                "Bocha search failed: HTTP {} — {truncated}",
                status.as_u16()
            )));
        }

        let parsed: serde_json::Value = serde_json::from_str(&body).map_err(|e| {
            ToolError::execution_failed(format!("Failed to parse Bocha response: {e}"))
        })?;

        if let Some(error) = bocha_error_message(&parsed) {
            return Err(ToolError::execution_failed(error));
        }

        Ok(parse_bocha_results(&parsed, max_results))
    }

    /// Search via Metaso AI Search API (<https://metaso.cn>). Falls back to
    /// `METASO_API_KEY` when no config key is set.
    async fn run_metaso_search(
        &self,
        query: &str,
        max_results: usize,
        timeout_ms: u64,
        context: &ToolContext,
    ) -> Result<Vec<WebSearchEntry>, ToolError> {
        let env_key = std::env::var("METASO_API_KEY").ok();
        let api_key = context
            .search_api_key
            .as_deref()
            .or(env_key.as_deref())
            .ok_or_else(|| {
                ToolError::execution_failed(
                    "Metaso search requires an API key. Set `METASO_API_KEY` or `[search] api_key` in config.toml.",
                )
            })?;

        let client = crate::tls::reqwest_client_builder()
            .timeout(Duration::from_millis(timeout_ms))
            .build()
            .map_err(|e| {
                ToolError::execution_failed(format!("Failed to build HTTP client: {e}"))
            })?;

        let size = max_results.clamp(1, 100);
        let payload = json!({
            "q": query,
            "scope": "webpage",
            "size": size,
        });

        let resp = client
            .post(format!("{METASO_ENDPOINT}/search"))
            .header("Content-Type", "application/json")
            .header("Authorization", format!("Bearer {api_key}"))
            .json(&payload)
            .send()
            .await
            .map_err(|e| {
                ToolError::execution_failed(format!("Metaso search request failed: {e}"))
            })?;

        let status = resp.status();
        let body = resp.text().await.map_err(|e| {
            ToolError::execution_failed(format!("Failed to read Metaso response: {e}"))
        })?;

        if !status.is_success() {
            let msg = match status.as_u16() {
                401 | 403 => "Metaso API key rejected — check METASO_API_KEY or set `[search] api_key` in config.toml, or get one at https://metaso.cn/search-api/playground".to_string(),
                429 => "Metaso rate-limited — wait and retry, or get your own API key at https://metaso.cn/search-api/playground".to_string(),
                _ => {
                    let truncated = truncate_error_body(&body);
                    format!("Metaso server error (HTTP {status}) — {truncated}")
                }
            };
            return Err(ToolError::execution_failed(msg));
        }

        let parsed: serde_json::Value = serde_json::from_str(&body).map_err(|e| {
            ToolError::execution_failed(format!("Failed to parse Metaso response: {e}"))
        })?;

        // Check business-logic error codes in the response body.
        if let Some(code) = parsed.get("code").and_then(|v| v.as_i64())
            && code != 0
        {
            let msg = parsed
                .get("message")
                .and_then(|v| v.as_str())
                .unwrap_or("unknown error");
            return Err(ToolError::execution_failed(match code {
                3003 => "Metaso: daily search limit reached — set METASO_API_KEY or get one at https://metaso.cn/search-api/playground".to_string(),
                2005 => "Metaso API key rejected — check METASO_API_KEY or set `[search] api_key` in config.toml".to_string(),
                _ => format!("Metaso API error (code {code}: {msg})"),
            }));
        }

        Ok(parse_metaso_results(&parsed, size))
    }

    /// Search via Baidu AI Search API (<https://qianfan.baidubce.com>).
    async fn run_baidu_search(
        &self,
        query: &str,
        max_results: usize,
        timeout_ms: u64,
        context: &ToolContext,
    ) -> Result<Vec<WebSearchEntry>, ToolError> {
        let env_key = std::env::var("BAIDU_SEARCH_API_KEY").ok();
        let api_key = context
            .search_api_key
            .as_deref()
            .or(env_key.as_deref())
            .ok_or_else(|| {
                ToolError::execution_failed(
                    "Baidu search requires an API key. Set `BAIDU_SEARCH_API_KEY` or `[search] api_key` in config.toml.",
                )
            })?;

        let client = crate::tls::reqwest_client_builder()
            .timeout(Duration::from_millis(timeout_ms))
            .build()
            .map_err(|e| {
                ToolError::execution_failed(format!("Failed to build HTTP client: {e}"))
            })?;

        let payload = baidu_search_payload(query, max_results);

        // Baidu's AI Search endpoint accepts conversational messages rather
        // than an index-only query. Treat the entire request/response decode
        // as model-backed for attached-run ownership; a false negative here
        // could overlap Runtime Chat, while the conservative read lease only
        // serializes work that already belongs to the same interactive run.
        let _inference = acquire_model_backed_search_inference_participant().await;

        let resp = client
            .post(BAIDU_ENDPOINT)
            .header("Authorization", format!("Bearer {api_key}"))
            .json(&payload)
            .send()
            .await
            .map_err(|e| {
                ToolError::execution_failed(format!("Baidu search request failed: {e}"))
            })?;

        let status = resp.status();
        let body = resp.text().await.map_err(|e| {
            ToolError::execution_failed(format!("Failed to read Baidu response: {e}"))
        })?;

        if !status.is_success() {
            let msg = match status.as_u16() {
                401 | 403 => "Baidu search API key rejected — check BAIDU_SEARCH_API_KEY or `[search] api_key` in config.toml".to_string(),
                429 => "Baidu search rate-limited — wait and retry, or check your Baidu AI Search quota".to_string(),
                _ => {
                    let truncated = truncate_error_body(&body);
                    format!("Baidu search failed: HTTP {} — {truncated}", status.as_u16())
                }
            };
            return Err(ToolError::execution_failed(msg));
        }

        let parsed: serde_json::Value = serde_json::from_str(&body).map_err(|e| {
            ToolError::execution_failed(format!("Failed to parse Baidu response: {e}"))
        })?;

        if let Some(error) = baidu_error_message(&parsed) {
            return Err(ToolError::execution_failed(error));
        }

        Ok(parse_baidu_results(&parsed, max_results))
    }

    /// Search via Volcengine Ark; it needs a 90s floor and retries transport failures.
    async fn run_volcengine_search(
        &self,
        query: &str,
        max_results: usize,
        timeout_ms: u64,
        context: &ToolContext,
    ) -> Result<Vec<WebSearchEntry>, ToolError> {
        let volc_key = std::env::var("VOLCENGINE_API_KEY").ok();
        let volc_ark_key = std::env::var("VOLCENGINE_ARK_API_KEY").ok();
        let ark_key = std::env::var("ARK_API_KEY").ok();
        let api_key = context
            .search_api_key
            .as_deref()
            .or(volc_key.as_deref())
            .or(volc_ark_key.as_deref())
            .or(ark_key.as_deref())
            .ok_or_else(|| {
                ToolError::execution_failed(
                    "Volcengine search requires an API key. Set `[search] api_key`, \
                     or VOLCENGINE_API_KEY / VOLCENGINE_ARK_API_KEY / ARK_API_KEY env var.",
                )
            })?;

        let effective_timeout = timeout_ms.max(90_000);

        let client = crate::tls::reqwest_client_builder()
            .connect_timeout(Duration::from_secs(15))
            .timeout(Duration::from_millis(effective_timeout))
            .tcp_keepalive(Some(Duration::from_secs(30)))
            .http2_keep_alive_interval(Some(Duration::from_secs(15)))
            .http2_keep_alive_timeout(Duration::from_secs(20))
            .user_agent(USER_AGENT)
            .build()
            .map_err(|e| {
                ToolError::execution_failed(format!("Failed to build HTTP client: {e}"))
            })?;

        let payload = volcengine_search_payload(query, max_results);

        // Unlike the ordinary index-search backends, Volcengine's Responses
        // endpoint runs a named model and returns model-generated text. Keep
        // that provider lifecycle inside the attached CWC run's shared read
        // ownership through retries and response decoding, so an isolated
        // Runtime Chat turn cannot be projected alongside it.
        let _inference = acquire_model_backed_search_inference_participant().await;

        let mut last_err: Option<ToolError> = None;
        for attempt in 0..3 {
            if attempt > 0 {
                tokio::time::sleep(Duration::from_millis(1000 * (1 << (attempt - 1)))).await;
            }

            match client
                .post(VOLCENGINE_RESPONSES_ENDPOINT)
                .header("Authorization", format!("Bearer {api_key}"))
                .json(&payload)
                .send()
                .await
            {
                Ok(resp) => {
                    let status = resp.status();
                    let body = resp.text().await.map_err(|e| {
                        ToolError::execution_failed(format!(
                            "Failed to read Volcengine response: {e}"
                        ))
                    })?;

                    if !status.is_success() {
                        let msg = match status.as_u16() {
                            401 | 403 => "Volcengine API key rejected — check `[search] api_key` in config.toml or VOLCENGINE_API_KEY / VOLCENGINE_ARK_API_KEY / ARK_API_KEY".to_string(),
                            429 => "Volcengine API rate-limited — wait and retry, or check your quota".to_string(),
                            _ => {
                                let truncated = truncate_error_body(&body);
                                format!("Volcengine search failed: HTTP {} — {truncated}", status.as_u16())
                            }
                        };
                        return Err(ToolError::execution_failed(msg));
                    }

                    let parsed: serde_json::Value = serde_json::from_str(&body).map_err(|e| {
                        ToolError::execution_failed(format!(
                            "Failed to parse Volcengine response: {e}"
                        ))
                    })?;

                    if let Some(error) = volcengine_error_message(&parsed) {
                        return Err(ToolError::execution_failed(error));
                    }

                    let response_text = volcengine_extract_text(&parsed).ok_or_else(|| {
                        ToolError::execution_failed("Volcengine response contains no output text")
                    })?;

                    return Ok(parse_volcengine_results(&response_text, max_results));
                }
                Err(e) => {
                    let is_transient = e.is_timeout() || e.is_connect();
                    if !is_transient || attempt == 2 {
                        return Err(ToolError::execution_failed(format!(
                            "Volcengine search request failed: {e}"
                        )));
                    }
                    last_err = Some(ToolError::execution_failed(format!(
                        "Volcengine search request failed (attempt {}/3): {e}",
                        attempt + 1
                    )));
                }
            }
        }

        // Unreachable — the final iteration always returns above.
        Err(last_err.unwrap_or_else(|| {
            ToolError::execution_failed("Volcengine search: unexpected retry exit")
        }))
    }
}

pub(crate) async fn execute_search(
    query: SearchQuery,
    timeout_ms: u64,
    context: &ToolContext,
) -> Result<SearchResponse, ToolError> {
    if configured_search_base_url(context.search_base_url.as_deref()).is_some()
        && !matches!(
            context.search_provider,
            SearchProvider::DuckDuckGo | SearchProvider::Searxng
        )
    {
        return Err(ToolError::invalid_input(format!(
            "[search].base_url is only supported with provider = \"duckduckgo\" or \"searxng\"; current provider is \"{}\"",
            context.search_provider.as_str()
        )));
    }

    let chain = SearchBackendChain::from_context(context);
    let initial_backend = chain.initial_backend();
    if initial_backend != BackendId::ProviderNative {
        debug_assert_eq!(initial_backend.as_str(), context.search_provider.as_str());
        preflight_search_provider(context)?;
    }
    let cache_scope = if initial_backend == BackendId::ProviderNative {
        context
            .provider_native_search
            .as_ref()
            .map(crate::client::ProviderNativeSearchClient::cache_identity)
    } else {
        normalized_search_base_url(context.search_base_url.as_deref())
    };

    if let Some(mut cached) = cache::get_search(
        &context.state_namespace,
        initial_backend,
        cache_scope.as_deref(),
        &query,
    ) {
        validate_cached_search_policy(&cached, context)?;
        register_search_citations(&mut cached, context);
        cached.receipt.cache_hit = true;
        cached.receipt.latency_ms = 0;
        return Ok(cached);
    }

    let started = Instant::now();
    let requested_timeout = Duration::from_millis(timeout_ms.max(1));
    let provider_native_timeout_floor = context
        .provider_native_search
        .as_ref()
        .and_then(provider_native_timeout_floor);
    let (total_timeout, first_attempt_budget, fallback_budget_after_first) = search_timeout_budgets(
        initial_backend,
        requested_timeout,
        provider_native_timeout_floor,
    );
    let deadline = started + total_timeout;
    let chained = chain
        .search(
            &query,
            deadline,
            first_attempt_budget,
            fallback_budget_after_first,
        )
        .await?;
    let mut response =
        finalize_search_response(query.clone(), chained.capabilities, chained.raw, started);
    register_search_citations(&mut response, context);
    cache::insert_search(
        &context.state_namespace,
        initial_backend,
        cache_scope.as_deref(),
        &query,
        response.clone(),
    );
    Ok(response)
}

fn search_timeout_budgets(
    initial_backend: BackendId,
    requested_timeout: Duration,
    provider_native_timeout_floor: Option<Duration>,
) -> (Duration, Option<Duration>, Option<Duration>) {
    match initial_backend {
        BackendId::Volcengine => {
            let provider_budget = Duration::from_millis(VOLCENGINE_MIN_TIMEOUT_MS);
            (
                provider_budget + requested_timeout,
                Some(provider_budget),
                None,
            )
        }
        BackendId::ProviderNative => {
            // Provider-native search performs a model-backed request. Give it
            // a dedicated minimum without donating unused time to the
            // configured/local fallback selected by the caller.
            let provider_budget = requested_timeout.max(
                provider_native_timeout_floor
                    .unwrap_or(Duration::from_millis(PROVIDER_NATIVE_MIN_TIMEOUT_MS)),
            );
            (
                provider_budget.saturating_add(requested_timeout),
                Some(provider_budget),
                Some(requested_timeout),
            )
        }
        _ => (requested_timeout, None, None),
    }
}

fn provider_native_timeout_floor(
    client: &crate::client::ProviderNativeSearchClient,
) -> Option<Duration> {
    crate::config::is_exact_direct_moonshot_k3_route(
        client.provider(),
        client.base_url(),
        client.model(),
    )
    .then_some(Duration::from_millis(KIMI_K3_FORMULA_MIN_TIMEOUT_MS))
}

fn register_search_citations(response: &mut SearchResponse, context: &ToolContext) {
    let mut seen = std::collections::HashSet::new();
    response.results.retain_mut(|result| {
        let Some(citation) = super::web::citations::register(
            &context.state_namespace,
            &result.url,
            Some(&result.title),
        ) else {
            return false;
        };
        result.ref_id = citation.ref_id;
        result.url = citation.url;
        seen.insert(result.ref_id.clone())
    });
    if response.count != response.results.len() {
        rerank(&mut response.results);
        response.count = response.results.len();
        response.message = if response.count == 0 {
            "No usable web citations found".to_string()
        } else {
            format!("Found {} result(s)", response.count)
        };
    }
}

/// Reject misconfiguration before cache lookup or network access.
fn preflight_search_provider(context: &ToolContext) -> Result<(), ToolError> {
    let configured_key = context
        .search_api_key
        .as_deref()
        .is_some_and(|key| !key.trim().is_empty());
    let env_key = |name: &str| std::env::var_os(name).is_some_and(|value| !value.is_empty());
    let not_configured = |message: &str| Err(ToolError::invalid_input(message));

    match context.search_provider {
        SearchProvider::Tavily if !configured_key => not_configured(
            "Tavily search is not configured: it requires an API key. Set `[search] api_key = \"tvly-...\"` in config.toml.",
        ),
        SearchProvider::Bocha if !configured_key => not_configured(
            "Bocha search is not configured: it requires an API key. Set `[search] api_key = \"sk-...\"` in config.toml.",
        ),
        SearchProvider::Metaso if !configured_key && !env_key("METASO_API_KEY") => not_configured(
            "Metaso search is not configured: it requires an API key. Set `METASO_API_KEY` or `[search] api_key` in config.toml.",
        ),
        SearchProvider::Baidu if !configured_key && !env_key("BAIDU_SEARCH_API_KEY") => {
            not_configured(
                "Baidu search is not configured: it requires an API key. Set `BAIDU_SEARCH_API_KEY` or `[search] api_key` in config.toml.",
            )
        }
        SearchProvider::Volcengine
            if !configured_key
                && !env_key("VOLCENGINE_API_KEY")
                && !env_key("VOLCENGINE_ARK_API_KEY")
                && !env_key("ARK_API_KEY") =>
        {
            not_configured(
                "Volcengine search is not configured: it requires an API key. Set `[search] api_key`, or VOLCENGINE_API_KEY / VOLCENGINE_ARK_API_KEY / ARK_API_KEY env var.",
            )
        }
        SearchProvider::Sofya if !configured_key && !env_key("SOFYA_API_KEY") => not_configured(
            "Sofya search is not configured: it requires an API key. Set `[search] api_key = \"ay_live_...\"` in config.toml or the SOFYA_API_KEY env var.",
        ),
        SearchProvider::Searxng
            if configured_search_base_url(context.search_base_url.as_deref()).is_none() =>
        {
            not_configured(
                "SearXNG search requires [search] base_url = \"https://your-searxng.example\"; no public instance is used by default.",
            )
        }
        _ => Ok(()),
    }
}

fn normalized_search_base_url(base_url: Option<&str>) -> Option<String> {
    let raw = configured_search_base_url(base_url)?;
    let Ok(mut url) = reqwest::Url::parse(raw) else {
        return Some(raw.to_string());
    };
    url.set_fragment(None);
    Some(url.to_string())
}

fn validate_cached_search_policy(
    response: &SearchResponse,
    context: &ToolContext,
) -> Result<(), ToolError> {
    let host = response
        .receipt
        .backend_detail
        .as_deref()
        .or_else(|| default_backend_host(response.receipt.backend))
        .ok_or_else(|| {
            ToolError::execution_failed("cached search receipt did not identify its backend host")
        })?;
    check_policy(context.network_policy.as_ref(), host)
}

const fn default_backend_host(backend: BackendId) -> Option<&'static str> {
    match backend {
        BackendId::ProviderNative => None,
        BackendId::Bing => Some(BING_HOST),
        BackendId::DuckDuckGo => Some("html.duckduckgo.com"),
        BackendId::Firecrawl => Some("api.firecrawl.dev"),
        BackendId::Tavily => Some("api.tavily.com"),
        BackendId::Bocha => Some("api.bochaai.com"),
        BackendId::Metaso => Some("metaso.cn"),
        BackendId::Searxng => None,
        BackendId::Baidu => Some("qianfan.baidubce.com"),
        BackendId::Volcengine => Some("ark.cn-beijing.volces.com"),
        BackendId::Sofya => Some("sofya.co"),
    }
}

fn finalize_search_response(
    query: SearchQuery,
    capabilities: super::web::contract::QueryCapabilities,
    mut raw: BackendSearch,
    started: Instant,
) -> SearchResponse {
    let mut honored = HonoredQueryCapabilities {
        max_results: matches!(
            capabilities.max_results,
            super::web::contract::CapabilityState::Supported
        ),
        ..HonoredQueryCapabilities::default()
    };

    if query.recency.is_some() {
        if matches!(
            capabilities.recency,
            super::web::contract::CapabilityState::Supported
        ) {
            honored.recency = true;
        } else {
            raw.degraded.push(DegradedReason::KnobIgnored {
                knob: QueryKnob::Recency,
            });
        }
    }
    if !query.domains.is_empty() {
        // The backend chain applies this before deciding whether an attempt
        // produced usable results. Keep finalization defensive for cached or
        // directly constructed responses; the helper is idempotent.
        apply_domain_constraints(&query, capabilities, &mut raw);
        honored.domains = true;
    }
    if query.locale.is_some() {
        // The scrapes can flag an explicit locale as ignored before this
        // point (malformed value, or a DuckDuckGo region outside the
        // verified `kl` list). In that case the knob was not honored and
        // the receipt must not claim it was — the degraded entry already
        // says so.
        let locale_flagged_ignored = raw.degraded.iter().any(|reason| {
            matches!(
                reason,
                DegradedReason::KnobIgnored {
                    knob: QueryKnob::Locale,
                }
            )
        });
        if matches!(
            capabilities.locale,
            super::web::contract::CapabilityState::Supported
        ) && !locale_flagged_ignored
        {
            honored.locale = true;
        } else if !locale_flagged_ignored {
            raw.degraded.push(DegradedReason::KnobIgnored {
                knob: QueryKnob::Locale,
            });
        }
    }

    raw.results.truncate(usize::from(query.max_results));
    rerank(&mut raw.results);
    let latency_ms = u32::try_from(started.elapsed().as_millis()).unwrap_or(u32::MAX);
    let receipt = SearchReceipt {
        backend: raw.backend,
        backend_detail: raw.backend_detail,
        requested: query.clone(),
        capabilities,
        honored,
        degraded: raw.degraded,
        latency_ms,
        cache_hit: false,
    };
    let count = raw.results.len();
    let message = match (count, raw.note.as_deref()) {
        (0, Some(note)) => format!("No results found. {note}"),
        (0, None) => "No results found".to_string(),
        (_, Some(note)) => format!("Found {count} result(s). {note}"),
        (_, None) => format!("Found {count} result(s)"),
    };

    SearchResponse {
        query: query.query,
        source: raw.source,
        count,
        message,
        results: raw.results,
        receipt,
    }
}

pub(crate) fn apply_domain_constraints(
    query: &SearchQuery,
    capabilities: super::web::contract::QueryCapabilities,
    raw: &mut BackendSearch,
) {
    if query.domains.is_empty() {
        return;
    }

    let before = raw.results.len();
    raw.results
        .retain(|result| domain_matches(&result.url, &query.domains));
    rerank(&mut raw.results);
    let provider_honored = matches!(
        capabilities.domains,
        crate::tools::web::contract::CapabilityState::Supported
    );
    let filtered_any = raw.results.len() != before;
    if raw.backend == BackendId::ProviderNative && (!provider_honored || filtered_any) {
        // Post-filtering constrains returned citations but cannot prove that a
        // provider-generated answer did not rely on a removed source.
        raw.note = None;
    }
    let already_recorded = raw.degraded.iter().any(|reason| {
        matches!(
            reason,
            DegradedReason::PostFiltered {
                knob: QueryKnob::Domains
            }
        )
    });
    if (!provider_honored || filtered_any) && !already_recorded {
        raw.degraded.push(DegradedReason::PostFiltered {
            knob: QueryKnob::Domains,
        });
    }
}

pub(crate) async fn run_backend_search(
    provider: SearchProvider,
    query: &SearchQuery,
    deadline: Instant,
    context: &ToolContext,
) -> Result<BackendSearch, ToolError> {
    let timeout_ms = u64::try_from(
        deadline
            .saturating_duration_since(Instant::now())
            .as_millis()
            .max(1),
    )
    .unwrap_or(u64::MAX);
    let max_results = usize::from(query.max_results);
    let tool = WebSearchTool;
    let simple = |backend, entries: Vec<WebSearchEntry>| BackendSearch {
        backend,
        source: backend.as_str().to_string(),
        backend_detail: None,
        results: normalize_entries(entries),
        degraded: Vec::new(),
        note: None,
    };

    match provider {
        SearchProvider::Firecrawl => {
            check_policy(context.network_policy.as_ref(), "api.firecrawl.dev")?;
            let (results, note) = tool
                .run_firecrawl_search(&query.query, max_results, timeout_ms, context)
                .await?;
            Ok(BackendSearch {
                backend: BackendId::Firecrawl,
                source: "firecrawl".to_string(),
                backend_detail: Some("api.firecrawl.dev".to_string()),
                results: normalize_entries(results),
                degraded: Vec::new(),
                note: Some(note),
            })
        }
        SearchProvider::Tavily => {
            check_policy(context.network_policy.as_ref(), "api.tavily.com")?;
            Ok(simple(
                BackendId::Tavily,
                tool.run_tavily_search(&query.query, max_results, timeout_ms, context)
                    .await?,
            ))
        }
        SearchProvider::Bocha => {
            check_policy(context.network_policy.as_ref(), "api.bochaai.com")?;
            Ok(simple(
                BackendId::Bocha,
                tool.run_bocha_search(&query.query, max_results, timeout_ms, context)
                    .await?,
            ))
        }
        SearchProvider::Metaso => {
            check_policy(context.network_policy.as_ref(), "metaso.cn")?;
            Ok(simple(
                BackendId::Metaso,
                tool.run_metaso_search(&query.query, max_results, timeout_ms, context)
                    .await?,
            ))
        }
        SearchProvider::Searxng => {
            let (entries, host) = tool
                .run_searxng_search(&query.query, max_results, timeout_ms, context)
                .await?;
            let note = format!("Backend: searxng at {host}");
            Ok(BackendSearch {
                backend: BackendId::Searxng,
                source: "searxng".to_string(),
                backend_detail: Some(host),
                results: normalize_entries(entries),
                degraded: Vec::new(),
                note: Some(note),
            })
        }
        SearchProvider::Baidu => {
            check_policy(context.network_policy.as_ref(), "qianfan.baidubce.com")?;
            Ok(simple(
                BackendId::Baidu,
                tool.run_baidu_search(&query.query, max_results, timeout_ms, context)
                    .await?,
            ))
        }
        SearchProvider::Volcengine => {
            check_policy(context.network_policy.as_ref(), "ark.cn-beijing.volces.com")?;
            let mut response = simple(
                BackendId::Volcengine,
                tool.run_volcengine_search(&query.query, max_results, timeout_ms, context)
                    .await?,
            );
            response.degraded.push(DegradedReason::SynthesizedResults);
            Ok(response)
        }
        SearchProvider::Sofya => {
            check_policy(context.network_policy.as_ref(), "sofya.co")?;
            Ok(simple(
                BackendId::Sofya,
                tool.run_sofya_search(&query.query, max_results, timeout_ms, context)
                    .await?,
            ))
        }
        SearchProvider::Bing | SearchProvider::DuckDuckGo => {
            run_scrape_search(provider, query, timeout_ms, context).await
        }
    }
}

#[derive(Clone, Copy)]
struct ScrapeEndpoints<'a> {
    bing: &'a str,
    allow_bing_fallback: Option<bool>,
}

impl Default for ScrapeEndpoints<'static> {
    fn default() -> Self {
        Self {
            bing: BING_ENDPOINT,
            allow_bing_fallback: None,
        }
    }
}

async fn run_scrape_search(
    provider: SearchProvider,
    query: &SearchQuery,
    timeout_ms: u64,
    context: &ToolContext,
) -> Result<BackendSearch, ToolError> {
    let fallback_context = (provider == SearchProvider::DuckDuckGo
        && context.search_provider != SearchProvider::DuckDuckGo)
        .then(|| {
            let mut cloned = context.clone();
            cloned.search_base_url = None;
            cloned
        });
    let context = fallback_context.as_ref().unwrap_or(context);
    run_scrape_search_with_endpoints(
        provider,
        query,
        timeout_ms,
        context,
        ScrapeEndpoints::default(),
    )
    .await
}

async fn run_scrape_search_with_endpoints(
    provider: SearchProvider,
    query: &SearchQuery,
    timeout_ms: u64,
    context: &ToolContext,
    endpoints: ScrapeEndpoints<'_>,
) -> Result<BackendSearch, ToolError> {
    let decider = context.network_policy.as_ref();
    let client = crate::tls::reqwest_client_builder()
        .timeout(Duration::from_millis(timeout_ms))
        .user_agent(USER_AGENT)
        .build()
        .map_err(|error| {
            ToolError::execution_failed(format!("Failed to build HTTP client: {error}"))
        })?;
    let max_results = usize::from(query.max_results);
    let mut degraded = Vec::new();

    if provider == SearchProvider::Bing {
        check_policy(decider, BING_HOST)?;
        if bing_locale_was_ignored(query.locale.as_deref()) {
            degraded.push(DegradedReason::KnobIgnored {
                knob: QueryKnob::Locale,
            });
        }
        let results = run_bing_search(
            &client,
            &query.query,
            query.locale.as_deref(),
            max_results,
            endpoints.bing,
        )
        .await?;
        return Ok(BackendSearch {
            backend: BackendId::Bing,
            source: "bing".to_string(),
            backend_detail: None,
            results: normalize_entries(results),
            degraded,
            note: None,
        });
    }

    let market = scrape_market(query.locale.as_deref(), &query.query);
    if ddg_locale_was_ignored(query.locale.as_deref(), market.as_deref()) {
        degraded.push(DegradedReason::KnobIgnored {
            knob: QueryKnob::Locale,
        });
    }
    let (url, duckduckgo_host) = duckduckgo_search_url(
        context.search_base_url.as_deref(),
        &query.query,
        market.as_deref(),
    )?;
    let allow_bing_fallback = endpoints
        .allow_bing_fallback
        .unwrap_or_else(|| duckduckgo_allows_bing_fallback(context.search_base_url.as_deref()));
    check_policy(decider, &duckduckgo_host)?;
    let resp = client
        .get(&url)
        .header(
            "Accept",
            "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
        )
        .header("Accept-Language", scrape_accept_language(market.as_deref()))
        .send()
        .await
        .map_err(|error| {
            ToolError::execution_failed(format!("Web search request failed: {error}"))
        })?;
    let status = resp.status();
    let body = resp.text().await.map_err(|error| {
        ToolError::execution_failed(format!("Failed to read response: {error}"))
    })?;
    if !status.is_success() {
        return Err(ToolError::execution_failed(format!(
            "Web search failed: HTTP {}",
            status.as_u16()
        )));
    }

    let results = parse_duckduckgo_results(&body, max_results);
    let blocked = is_duckduckgo_challenge(&body);
    if !results.is_empty() {
        return Ok(BackendSearch {
            backend: BackendId::DuckDuckGo,
            source: if allow_bing_fallback {
                "duckduckgo".to_string()
            } else {
                duckduckgo_host.clone()
            },
            backend_detail: (!allow_bing_fallback).then_some(duckduckgo_host),
            results: normalize_entries(results),
            degraded,
            note: None,
        });
    }
    if blocked {
        degraded.push(DegradedReason::ChallengeDetected {
            backend: BackendId::DuckDuckGo,
        });
    }
    if !allow_bing_fallback {
        if blocked {
            return Err(ToolError::execution_failed(format!(
                "DuckDuckGo-compatible search endpoint at {duckduckgo_host} returned a bot challenge; check the private search service, credentials, or network policy"
            )));
        }
        return Ok(BackendSearch {
            backend: BackendId::DuckDuckGo,
            source: duckduckgo_host.clone(),
            backend_detail: Some(duckduckgo_host),
            results: Vec::new(),
            degraded,
            note: None,
        });
    }

    check_policy(decider, BING_HOST)?;
    match run_bing_search(
        &client,
        &query.query,
        query.locale.as_deref(),
        max_results,
        endpoints.bing,
    )
    .await
    {
        Ok(results) if !results.is_empty() => {
            degraded.push(DegradedReason::ScrapeFallback {
                from: BackendId::DuckDuckGo,
                to: BackendId::Bing,
            });
            prune_fallback_locale_ignored(&mut degraded, query.locale.as_deref());
            Ok(BackendSearch {
                backend: BackendId::Bing,
                source: "bing".to_string(),
                backend_detail: None,
                results: normalize_entries(results),
                degraded,
                note: Some(if blocked {
                    "DuckDuckGo returned a bot challenge; used Bing fallback".to_string()
                } else {
                    "DuckDuckGo returned no parseable results; used Bing fallback".to_string()
                }),
            })
        }
        Ok(_) if blocked => Err(ToolError::execution_failed(
            "DuckDuckGo returned a bot challenge and Bing fallback returned no results",
        )),
        Err(error) if blocked => Err(ToolError::execution_failed(format!(
            "DuckDuckGo returned a bot challenge and Bing fallback failed: {error}"
        ))),
        Ok(_) | Err(_) => Ok(BackendSearch {
            backend: BackendId::DuckDuckGo,
            source: "duckduckgo".to_string(),
            backend_detail: None,
            results: Vec::new(),
            degraded,
            note: None,
        }),
    }
}

fn normalize_entries(entries: Vec<WebSearchEntry>) -> Vec<SearchResult> {
    entries
        .into_iter()
        .enumerate()
        .map(|(index, entry)| {
            SearchResult::new(index + 1, entry.title, entry.url, entry.snippet, None)
        })
        .collect()
}

fn rerank(results: &mut [SearchResult]) {
    for (index, result) in results.iter_mut().enumerate() {
        result.rank = u8::try_from(index + 1).unwrap_or(u8::MAX);
    }
}

pub(crate) fn domain_matches(url: &str, domains: &[String]) -> bool {
    if domains.is_empty() {
        return true;
    }
    let Ok(parsed) = reqwest::Url::parse(url) else {
        return false;
    };
    let Some(host) = parsed.host_str() else {
        return false;
    };
    let host = host.trim_start_matches("www.").to_ascii_lowercase();
    domains.iter().any(|domain| {
        let domain = domain.trim_start_matches("www.").to_ascii_lowercase();
        host == domain || host.ends_with(&format!(".{domain}"))
    })
}

fn truncate_error_body(body: &str) -> String {
    let stripped = sanitize_error_body(body);
    if stripped.len() <= ERROR_BODY_PREVIEW_BYTES {
        stripped
    } else {
        let mut end = ERROR_BODY_PREVIEW_BYTES;
        while !stripped.is_char_boundary(end) {
            end -= 1;
        }
        format!("{}...", &stripped[..end])
    }
}

static TAG_RE: OnceLock<Regex> = OnceLock::new();

fn get_tag_re() -> &'static Regex {
    TAG_RE.get_or_init(|| Regex::new(r"<[^>]+>").expect("tag regex pattern is valid"))
}

fn strip_html_tags(text: &str) -> String {
    get_tag_re().replace_all(text, "").to_string()
}

fn sanitize_error_body(body: &str) -> String {
    let stripped = strip_html_tags(body);
    let visible: String = stripped
        .chars()
        .filter(|c| !c.is_control() || c.is_ascii_whitespace())
        .collect();
    get_bearer_token_re()
        .replace_all(&visible, "Bearer [REDACTED]")
        .to_string()
}

fn parse_tavily_results(parsed: &Value, max_results: usize) -> Vec<WebSearchEntry> {
    parsed
        .get("results")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|item| {
            let title = item.get("title")?.as_str()?.trim();
            let url = item.get("url")?.as_str()?.trim();
            if title.is_empty() || url.is_empty() {
                return None;
            }
            Some(WebSearchEntry {
                title: title.to_string(),
                url: url.to_string(),
                snippet: first_non_empty_string(item, &["content", "snippet"]),
            })
        })
        .take(max_results)
        .collect()
}

fn parse_firecrawl_results(parsed: &Value, max_results: usize) -> Vec<WebSearchEntry> {
    parsed
        .pointer("/data/web")
        .or_else(|| parsed.get("data"))
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|item| {
            let title = item.get("title")?.as_str()?.trim();
            let url = item.get("url")?.as_str()?.trim();
            (!title.is_empty() && !url.is_empty()).then(|| WebSearchEntry {
                title: title.to_string(),
                url: url.to_string(),
                snippet: first_non_empty_string(item, &["description", "markdown", "content"])
                    .map(|value| value.chars().take(1_000).collect()),
            })
        })
        .take(max_results)
        .collect()
}

fn parse_metaso_results(parsed: &Value, max_results: usize) -> Vec<WebSearchEntry> {
    parsed
        .get("webpages")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|item| {
            let title = item.get("title")?.as_str()?.trim();
            let url = item.get("link")?.as_str()?.trim();
            if title.is_empty() || url.is_empty() {
                return None;
            }
            Some(WebSearchEntry {
                title: title.to_string(),
                url: url.to_string(),
                snippet: first_non_empty_string(item, &["snippet", "summary"]),
            })
        })
        .take(max_results)
        .collect()
}

fn parse_bocha_results(parsed: &Value, max_results: usize) -> Vec<WebSearchEntry> {
    parsed
        .get("data")
        .and_then(|d| {
            d.get("webPages")
                .and_then(|w| w.get("value"))
                .or_else(|| d.get("pages"))
        })
        .or_else(|| parsed.get("pages"))
        .and_then(|v| v.as_array())
        .into_iter()
        .flat_map(|arr| arr.iter())
        .filter_map(|item| {
            let title = item
                .get("name")
                .or_else(|| item.get("title"))
                .and_then(|s| s.as_str())?
                .trim();
            let url = item
                .get("url")
                .or_else(|| item.get("link"))
                .and_then(|s| s.as_str())?
                .trim();
            if title.is_empty() || url.is_empty() {
                return None;
            }
            let snippet = item
                .get("summary")
                .or_else(|| item.get("snippet"))
                .or_else(|| item.get("description"))
                .and_then(|s| s.as_str())
                .map(str::trim)
                .filter(|s| !s.is_empty())
                .map(ToString::to_string);
            Some(WebSearchEntry {
                title: title.to_string(),
                url: url.to_string(),
                snippet,
            })
        })
        .take(max_results)
        .collect()
}

fn bocha_error_message(parsed: &Value) -> Option<String> {
    let code = parsed.get("code").and_then(|v| v.as_i64())?;
    if code == 0 || code == 200 {
        return None;
    }
    let message = parsed
        .get("msg")
        .or_else(|| parsed.get("message"))
        .and_then(|v| v.as_str())
        .unwrap_or("unknown error");
    Some(format!("Bocha search API error (code {code}: {message})"))
}

fn parse_baidu_results(parsed: &Value, max_results: usize) -> Vec<WebSearchEntry> {
    parsed
        .get("references")
        .and_then(|v| v.as_array())
        .into_iter()
        .flat_map(|arr| arr.iter())
        .filter_map(|item| {
            let title = item
                .get("title")
                .or_else(|| item.get("name"))
                .and_then(|s| s.as_str())?
                .trim();
            let url = item
                .get("url")
                .or_else(|| item.get("link"))
                .and_then(|s| s.as_str())?
                .trim();
            if title.is_empty() || url.is_empty() {
                return None;
            }
            let snippet = item
                .get("content")
                .or_else(|| item.get("snippet"))
                .or_else(|| item.get("summary"))
                .and_then(|s| s.as_str())
                .map(str::trim)
                .filter(|s| !s.is_empty())
                .map(ToString::to_string);
            Some(WebSearchEntry {
                title: title.to_string(),
                url: url.to_string(),
                snippet,
            })
        })
        .take(max_results)
        .collect()
}

fn parse_searxng_results(parsed: &Value, max_results: usize) -> Vec<WebSearchEntry> {
    parsed
        .get("results")
        .and_then(|v| v.as_array())
        .into_iter()
        .flat_map(|arr| arr.iter())
        .filter_map(|item| {
            let title = item.get("title").and_then(Value::as_str)?.trim();
            let url = item.get("url").and_then(Value::as_str)?.trim();
            if title.is_empty() || url.is_empty() {
                return None;
            }
            let snippet = first_non_empty_string(item, &["content", "snippet"]);
            Some(WebSearchEntry {
                title: title.to_string(),
                url: url.to_string(),
                snippet,
            })
        })
        .take(max_results)
        .collect()
}

fn baidu_error_message(parsed: &Value) -> Option<String> {
    let code = parsed
        .get("error_code")
        .or_else(|| parsed.get("code"))
        .and_then(|v| v.as_i64())?;
    if code == 0 {
        return None;
    }
    let message = parsed
        .get("error_msg")
        .or_else(|| parsed.get("message"))
        .and_then(|v| v.as_str())
        .unwrap_or("unknown error");
    Some(format!("Baidu search API error (code {code}: {message})"))
}

async fn acquire_model_backed_search_inference_participant()
-> crate::client::RemoteControlInferencePermit {
    crate::client::acquire_remote_control_inference_participant().await
}

fn parse_sofya_results(parsed: &Value, max_results: usize) -> Vec<WebSearchEntry> {
    parsed
        .get("results")
        .and_then(|v| v.as_array())
        .into_iter()
        .flat_map(|arr| arr.iter())
        .filter_map(|item| {
            let title = item.get("title")?.as_str()?.to_string();
            let url = item.get("url")?.as_str()?.to_string();
            let snippet = first_non_empty_string(item, &["content", "description"]);
            Some(WebSearchEntry {
                title,
                url,
                snippet,
            })
        })
        .take(max_results)
        .collect()
}

fn first_non_empty_string(item: &Value, keys: &[&str]) -> Option<String> {
    keys.iter().find_map(|key| {
        item.get(*key)
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .map(str::to_string)
    })
}

fn baidu_search_payload(query: &str, max_results: usize) -> Value {
    json!({
        "messages": [
            {
                "role": "user",
                "content": query,
            }
        ],
        "search_source": "baidu_search_v2",
        "resource_type_filter": [
            {
                "type": "web",
                "top_k": max_results,
            }
        ],
    })
}

fn volcengine_search_payload(query: &str, max_results: usize) -> Value {
    json!({
        "model": "doubao-seed-2-0-lite-260428",
        "stream": false,
        "tools": [{"type": "web_search"}],
        "input": [{
            "role": "user",
            "content": [{
                "type": "input_text",
                "text": format!(
                    "Search the web for: {query}\n\n\
                     CRITICAL: Respond ONLY with a valid JSON object. No markdown, no explanation.\n\
                     Schema: {{\"results\":[{{\"title\":\"...\",\"url\":\"https://...\",\"snippet\":\"...\"}}]}}\n\
                     - results: 1-{max_results} most relevant pages\n\
                     - title: page title (required)\n\
                     - url: full URL starting with https:// (required)\n\
                     - snippet: 1-2 sentence factual summary (required)\n\
                     - If zero results: {{\"results\":[]}}\n\
                     - Your entire response must be valid, parseable JSON."
                )
            }]
        }]
    })
}

/// Extracts the model's text response from a Volcengine Responses API output.
fn volcengine_extract_text(parsed: &Value) -> Option<String> {
    parsed
        .get("output")
        .and_then(|v| v.as_array())
        .into_iter()
        .flat_map(|arr| arr.iter().rev())
        .find(|item| item.get("type").and_then(|t| t.as_str()) == Some("message"))
        .and_then(|msg| msg.get("content").and_then(|c| c.as_array()))
        .and_then(|content| {
            content
                .iter()
                .find(|c| c.get("text").and_then(|t| t.as_str()).is_some())
        })
        .and_then(|c| c.get("text").and_then(|t| t.as_str()))
        .map(|s| s.to_string())
}

/// Checks for business-logic errors in a Volcengine Responses API response.
fn volcengine_error_message(parsed: &Value) -> Option<String> {
    let error = parsed.get("error")?;
    let code = error
        .get("code")
        .and_then(|v| v.as_str())
        .unwrap_or("unknown");
    let message = error
        .get("message")
        .and_then(|v| v.as_str())
        .unwrap_or("no details");
    Some(format!("Volcengine API error (code {code}: {message})"))
}

/// Parses Volcengine model-generated JSON results into `WebSearchEntry` items.
fn parse_volcengine_results(response_text: &str, max_results: usize) -> Vec<WebSearchEntry> {
    let json_text = extract_json_block(response_text).unwrap_or(response_text);

    let parsed: Value = match serde_json::from_str(json_text) {
        Ok(v) => v,
        Err(_) => return Vec::new(),
    };

    parsed
        .get("results")
        .and_then(|v| v.as_array())
        .into_iter()
        .flat_map(|arr| arr.iter())
        .filter_map(|item| {
            let title = item.get("title").and_then(|s| s.as_str())?.trim();
            let url = item.get("url").and_then(|s| s.as_str())?.trim();
            if title.is_empty() || url.is_empty() {
                return None;
            }
            let snippet = item
                .get("snippet")
                .and_then(|s| s.as_str())
                .map(str::trim)
                .filter(|s| !s.is_empty())
                .map(ToString::to_string);
            Some(WebSearchEntry {
                title: title.to_string(),
                url: url.to_string(),
                snippet,
            })
        })
        .take(max_results)
        .collect()
}

/// Attempts to extract a JSON block from text that may be wrapped in
/// markdown fences (```json ... ```) or contain surrounding commentary.
fn extract_json_block(text: &str) -> Option<&str> {
    if let Some(start) = text.find("```json") {
        let inner = &text[start + 7..];
        if let Some(end) = inner.find("```") {
            return Some(inner[..end].trim());
        }
    }
    if let Some(start) = text.find('{')
        && let Some(end) = text.rfind('}')
    {
        return Some(&text[start..=end]);
    }
    None
}

fn extract_search_query(input: &Value) -> Result<String, ToolError> {
    for key in ["query", "q"] {
        if let Some(value) = input.get(key) {
            let Some(query) = value.as_str() else {
                return Err(ToolError::invalid_input(format!(
                    "Field '{key}' must be a string"
                )));
            };
            let query = query.trim();
            if !query.is_empty() {
                return Ok(query.to_string());
            }
        }
    }

    for item in search_query_items(input) {
        for key in ["q", "query"] {
            if let Some(value) = item.get(key) {
                let Some(query) = value.as_str() else {
                    return Err(ToolError::invalid_input(format!(
                        "Field 'search_query[].{key}' must be a string"
                    )));
                };
                let query = query.trim();
                if !query.is_empty() {
                    return Ok(query.to_string());
                }
            }
        }
    }

    Err(ToolError::missing_field("query"))
}

fn optional_search_max_results(input: &Value) -> u64 {
    if let Some(value) = input.get("max_results").and_then(Value::as_u64) {
        return value;
    }
    search_query_items(input)
        .filter_map(|item| item.get("max_results").and_then(Value::as_u64))
        .next()
        .unwrap_or(DEFAULT_SEARCH_RESULTS as u64)
}

fn search_query_from_input(input: &Value) -> Result<SearchQuery, ToolError> {
    let query = extract_search_query(input)?;
    if query.is_empty() {
        return Err(ToolError::invalid_input("Query cannot be empty"));
    }
    let max_results = usize::try_from(optional_search_max_results(input))
        .unwrap_or(DEFAULT_SEARCH_RESULTS)
        .clamp(1, usize::from(MAX_SEARCH_RESULTS));
    let recency = search_option(input, "recency")
        .map(parse_recency)
        .transpose()?;
    let domains = match search_option(input, "domains") {
        Some(value) => value
            .as_array()
            .ok_or_else(|| ToolError::invalid_input("Field 'domains' must be an array"))?
            .iter()
            .map(|value| {
                value.as_str().map(str::to_string).ok_or_else(|| {
                    ToolError::invalid_input("Every 'domains' entry must be a string")
                })
            })
            .collect::<Result<Vec<_>, _>>()?,
        None => Vec::new(),
    };
    let locale = search_option(input, "locale")
        .map(|value| {
            value
                .as_str()
                .map(str::to_string)
                .ok_or_else(|| ToolError::invalid_input("Field 'locale' must be a string"))
        })
        .transpose()?;

    Ok(SearchQuery::new(
        query,
        max_results,
        recency,
        domains,
        locale,
    ))
}

fn search_option<'a>(input: &'a Value, key: &str) -> Option<&'a Value> {
    input
        .get(key)
        .or_else(|| search_query_items(input).find_map(|item| item.get(key)))
}

fn parse_recency(value: &Value) -> Result<Recency, ToolError> {
    if let Some(days) = value.as_u64() {
        let days = u16::try_from(days)
            .ok()
            .filter(|days| (1..=3650).contains(days))
            .ok_or_else(|| {
                ToolError::invalid_input("Field 'recency' must be between 1 and 3650 days")
            })?;
        return Ok(Recency::Days(days));
    }
    match value.as_str() {
        Some("day") => Ok(Recency::Day),
        Some("week") => Ok(Recency::Week),
        Some("month") => Ok(Recency::Month),
        Some("year") => Ok(Recency::Year),
        _ => Err(ToolError::invalid_input(
            "Field 'recency' must be day, week, month, year, or an integer day count",
        )),
    }
}

fn search_query_items(input: &Value) -> impl Iterator<Item = &Value> {
    input
        .get("search_query")
        .and_then(Value::as_array)
        .into_iter()
        .flat_map(|items| items.iter())
}

/// Whether `query` contains Han ideographs (unified ideographs plus
/// Extension A). Kana and Hangul are deliberately excluded: this heuristic
/// exists only to pick a Chinese market for the keyless Bing/DuckDuckGo
/// scrapes when the model omits `locale`, and forcing Japanese or Korean
/// queries into the zh-CN market would be worse than sending no market
/// signal at all.
fn query_contains_han(query: &str) -> bool {
    query
        .chars()
        .any(|c| matches!(c, '\u{4E00}'..='\u{9FFF}' | '\u{3400}'..='\u{4DBF}'))
}

/// Whether `query` carries kana or hangul — the scripts that make a
/// Han-bearing query Japanese or Korean rather than Chinese. Real Japanese
/// queries almost always contain kana alongside kanji, so a Han-bearing
/// query with any kana or hangul must keep the no-market request instead of
/// being pushed onto the zh-CN market (the inverse of the drift the
/// fallback exists to fix). Only a purely Han-script query — rare for
/// search and indistinguishable from Chinese — still takes the fallback,
/// alongside unmarked Traditional Chinese (the accepted cost).
fn query_contains_kana_or_hangul(query: &str) -> bool {
    query.chars().any(|c| {
        matches!(
            c,
            '\u{3040}'..='\u{30FF}' // hiragana + katakana
                | '\u{31F0}'..='\u{31FF}' // katakana phonetic extensions
                | '\u{AC00}'..='\u{D7AF}' // hangul syllables
                | '\u{1100}'..='\u{11FF}' // hangul jamo
                | '\u{3130}'..='\u{318F}' // hangul compatibility jamo
        )
    })
}

/// Market tag the scrape backends should request, or `None` to keep the
/// historical no-market-signal request. An explicit locale wins; otherwise a
/// Han-script query without kana or hangul falls back to zh-CN because
/// without any market hint (and with an English `Accept-Language`) Bing
/// serves unrelated Japanese results for Chinese queries. The model-supplied
/// locale is shape-checked first so a malformed value cannot become a broken
/// market tag or header.
fn scrape_market(locale: Option<&str>, query: &str) -> Option<String> {
    locale
        .map(str::trim)
        .filter(|tag| is_plausible_locale_tag(tag))
        .map(|tag| tag.replace('_', "-"))
        .or_else(|| {
            (query_contains_han(query) && !query_contains_kana_or_hangul(query))
                .then(|| "zh-CN".to_string())
        })
}

/// Light shape check for a model-supplied `locale`: an alphabetic primary
/// language subtag followed by alphanumeric subtags separated by `-`/`_`,
/// each at most 8 characters, with no empty or separator-only edges. The
/// alphabetic primary matters for receipt honesty: `12345` is not a locale,
/// and transmitting it (with `honored.locale=true`) would contradict the
/// schema text promising that malformed values are ignored. Anything else
/// (e.g. `-CN`, `zh CN`, an injection attempt) is treated as no locale and
/// keeps the historical request shape.
fn is_plausible_locale_tag(tag: &str) -> bool {
    if tag.is_empty() || tag.len() > 35 {
        return false;
    }
    let mut subtags = tag.split(['-', '_']);
    let primary = subtags.next().unwrap_or_default();
    if primary.is_empty() || !primary.chars().all(|c| c.is_ascii_alphabetic()) {
        return false;
    }
    subtags.all(|subtag| {
        !subtag.is_empty() && subtag.len() <= 8 && subtag.chars().all(|c| c.is_ascii_alphanumeric())
    })
}

/// `Accept-Language` matching [`scrape_market`]. With no market resolved the
/// English default is kept — the value the Bing path has always sent
/// (`en-US,en;q=0.9`); the DuckDuckGo path previously sent `q=0.5` and now
/// shares this one rule.
fn scrape_accept_language(market: Option<&str>) -> String {
    match market {
        None => "en-US,en;q=0.9".to_string(),
        Some(market) => {
            let primary = market
                .split(['-', '_'])
                .next()
                .filter(|tag| !tag.is_empty())
                .unwrap_or("en");
            let mut value = if market.eq_ignore_ascii_case(primary) {
                format!("{market};q=0.9")
            } else {
                format!("{market},{primary};q=0.9")
            };
            // The generic `en` fallback duplicates the primary tag for
            // English markets (`en-US,en;q=0.9,en;q=0.8`), where two q-values
            // for one range have no defined precedence — only add it when it
            // contributes a distinct range.
            if !primary.eq_ignore_ascii_case("en") {
                value.push_str(",en;q=0.8");
            }
            value
        }
    }
}

/// Bing request adjustments for the resolved market: the `mkt`/`setlang`
/// query parameters and the `Accept-Language` header value.
fn scrape_locale_params(locale: Option<&str>, query: &str) -> (Vec<(String, String)>, String) {
    let market = scrape_market(locale, query);
    let accept_language = scrape_accept_language(market.as_deref());
    let Some(market) = market else {
        return (Vec::new(), accept_language);
    };
    let primary = market
        .split(['-', '_'])
        .next()
        .filter(|tag| !tag.is_empty())
        .unwrap_or("en");
    // Bing expects `setlang` to carry a script tag for Chinese (a bare `zh`
    // is invalid and silently defaults to `en`). An explicit script subtag
    // wins (`zh-Hant-TW` must not degrade to Simplified); otherwise the
    // script is picked from the market's region subtag, defaulting to
    // Simplified because the heuristic that reaches this branch without an
    // explicit region is Han-driven.
    let setlang = if primary.eq_ignore_ascii_case("zh") {
        let segments: Vec<&str> = market.split(['-', '_']).collect();
        if segments
            .iter()
            .any(|segment| segment.eq_ignore_ascii_case("hant"))
        {
            "zh-Hant"
        } else if segments
            .iter()
            .any(|segment| segment.eq_ignore_ascii_case("hans"))
        {
            "zh-Hans"
        } else {
            match segments.get(1) {
                Some(region)
                    if matches!(region.to_ascii_lowercase().as_str(), "tw" | "hk" | "mo") =>
                {
                    "zh-Hant"
                }
                _ => "zh-Hans",
            }
        }
    } else {
        primary
    };
    (
        vec![
            ("mkt".to_string(), market.clone()),
            ("setlang".to_string(), setlang.to_string()),
        ],
        accept_language,
    )
}

/// True when an explicit `locale` was supplied but cannot shape a market
/// signal for the Bing scrape (`mkt`/`setlang`): the value is malformed, so
/// the request proceeds with no locale signal at all and the receipt must
/// say so instead of claiming the knob was honored.
fn bing_locale_was_ignored(locale: Option<&str>) -> bool {
    locale.is_some_and(|tag| !is_plausible_locale_tag(tag.trim()))
}

/// The Bing fallback resolves the locale through Bing's own market mapping,
/// so a `KnobIgnored` flag pushed for the DuckDuckGo leg (whose verified
/// `kl` list is narrow) no longer describes the backend that produced the
/// results: drop it and re-derive from Bing's rule, where anything
/// shape-valid was sent as `mkt`/`setlang` and only an implausible tag
/// stays ignored.
fn prune_fallback_locale_ignored(degraded: &mut Vec<DegradedReason>, locale: Option<&str>) {
    degraded.retain(|reason| {
        !matches!(
            reason,
            DegradedReason::KnobIgnored {
                knob: QueryKnob::Locale,
            }
        )
    });
    if bing_locale_was_ignored(locale) {
        degraded.push(DegradedReason::KnobIgnored {
            knob: QueryKnob::Locale,
        });
    }
}

/// True when an explicit `locale` was supplied but the DuckDuckGo scrape
/// honors no part of it as a region signal: the value is malformed (the
/// shape check fails, independently of how [`scrape_market`] resolved the
/// market — a pure-Han query still falls back to the `zh-CN` market for a
/// malformed locale) or the resolved market is outside the verified `kl`
/// region list. In the unmapped case no `kl` parameter is sent, but the
/// `Accept-Language` header still carries the locale (see
/// [`scrape_accept_language`]); only the missing region signal is
/// receipted, so the schema's "malformed values are ignored" promise — and
/// parity with the Bing leg — holds either way.
fn ddg_locale_was_ignored(locale: Option<&str>, market: Option<&str>) -> bool {
    locale.is_some_and(|tag| {
        !is_plausible_locale_tag(tag.trim()) || market.and_then(ddg_region_param).is_none()
    })
}

async fn run_bing_search(
    client: &reqwest::Client,
    query: &str,
    locale: Option<&str>,
    max_results: usize,
    endpoint: &str,
) -> Result<Vec<WebSearchEntry>, ToolError> {
    let mut url = reqwest::Url::parse(endpoint)
        .map_err(|error| ToolError::invalid_input(format!("Invalid Bing endpoint: {error}")))?;
    let (extra_params, accept_language) = scrape_locale_params(locale, query);
    {
        let mut pairs = url.query_pairs_mut();
        pairs.append_pair("q", query);
        for (key, value) in &extra_params {
            pairs.append_pair(key, value);
        }
    }
    let resp = client
        .get(url)
        .header(
            "Accept",
            "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
        )
        .header("Accept-Language", accept_language)
        .send()
        .await
        .map_err(|e| ToolError::execution_failed(format!("Bing search request failed: {e}")))?;

    let status = resp.status();
    let body = resp.text().await.map_err(|e| {
        ToolError::execution_failed(format!("Failed to read Bing search response: {e}"))
    })?;

    if !status.is_success() {
        return Err(ToolError::execution_failed(format!(
            "Bing search failed: HTTP {}",
            status.as_u16()
        )));
    }

    Ok(parse_bing_results(&body, max_results))
}

fn parse_duckduckgo_results(html: &str, max_results: usize) -> Vec<WebSearchEntry> {
    scrape_duckduckgo_results(html, max_results)
        .into_iter()
        .map(web_search_entry_from_scraped)
        .collect()
}

fn parse_bing_results(html: &str, max_results: usize) -> Vec<WebSearchEntry> {
    scrape_bing_results(html, max_results)
        .into_iter()
        .map(web_search_entry_from_scraped)
        .collect()
}

fn web_search_entry_from_scraped(entry: ScrapedSearchResult) -> WebSearchEntry {
    WebSearchEntry {
        title: entry.title,
        url: entry.url,
        snippet: entry.snippet,
    }
}

/// Translate a BCP 47-style market tag into DuckDuckGo's `kl` region value.
/// DuckDuckGo's HTML endpoints take `kl` from a fixed, non-systematic list
/// (`cn-zh`, `us-en`, `jp-jp`, `kr-kr`, `tw-tzh`, `hk-tzh`, ...), so a
/// mechanical reversal of a BCP 47 tag produces off-list junk for most
/// non-English locales. Only verified pairs are translated; anything else
/// sends no `kl` at all — exactly the no-region request the scrape made
/// before this knob existed. Custom DuckDuckGo-compatible services typically
/// ignore `kl` entirely.
fn ddg_region_param(market: &str) -> Option<String> {
    let normalized = market.to_ascii_lowercase().replace('_', "-");
    // Script+region Chinese tags reduce to their region pair: the schema
    // teaches BCP 47, where the script subtag is best practice for Chinese,
    // and the verified list maps the region form.
    let region = match normalized.as_str() {
        "zh-cn" | "zh-hans-cn" => "cn-zh",
        "en-us" => "us-en",
        "ja-jp" => "jp-jp",
        "ko-kr" => "kr-kr",
        "zh-tw" | "zh-hant-tw" => "tw-tzh",
        "zh-hk" | "zh-hant-hk" => "hk-tzh",
        _ => return None,
    };
    Some(region.to_string())
}

fn duckduckgo_search_url(
    base_url: Option<&str>,
    query: &str,
    market: Option<&str>,
) -> Result<(String, String), ToolError> {
    let raw = configured_search_base_url(base_url).unwrap_or(DUCKDUCKGO_ENDPOINT);
    let mut url = reqwest::Url::parse(raw).map_err(|err| {
        ToolError::invalid_input(format!(
            "Invalid DuckDuckGo-compatible search base_url: {err}"
        ))
    })?;
    {
        let mut pairs = url.query_pairs_mut();
        pairs.append_pair("q", query);
        // DuckDuckGo HTML endpoints take the market hint as `kl`; see
        // [`ddg_region_param`] for the verified region-language mapping.
        // Custom DDG-compatible services simply ignore the extra parameter.
        if let Some(region) = market.and_then(ddg_region_param) {
            pairs.append_pair("kl", &region);
        }
    }
    let host = url.host_str().ok_or_else(|| {
        ToolError::invalid_input("DuckDuckGo-compatible search base_url must include a host")
    })?;
    Ok((url.to_string(), host.to_string()))
}

fn searxng_search_url(base_url: Option<&str>, query: &str) -> Result<(String, String), ToolError> {
    let raw = configured_search_base_url(base_url).ok_or_else(|| {
        ToolError::invalid_input(
            "SearXNG search requires [search] base_url = \"https://your-searxng.example\"; no public instance is used by default.",
        )
    })?;
    let mut url = reqwest::Url::parse(raw).map_err(|err| {
        ToolError::invalid_input(format!("Invalid SearXNG search base_url: {err}"))
    })?;
    let host = url
        .host_str()
        .ok_or_else(|| ToolError::invalid_input("SearXNG search base_url must include a host"))?
        .to_string();

    let path = url.path().trim_end_matches('/');
    if path.is_empty() {
        url.set_path("search");
    } else if path != "/search" && !path.ends_with("/search") {
        url.set_path(&format!("{path}/search"));
    }
    url.query_pairs_mut()
        .append_pair("q", query)
        .append_pair("format", "json");

    Ok((url.to_string(), host))
}

fn configured_search_base_url(base_url: Option<&str>) -> Option<&str> {
    base_url.map(str::trim).filter(|value| !value.is_empty())
}

fn duckduckgo_allows_bing_fallback(base_url: Option<&str>) -> bool {
    configured_search_base_url(base_url).is_none()
}

#[cfg(test)]
mod tests {
    use super::{
        ERROR_BODY_PREVIEW_BYTES, KIMI_K3_FORMULA_MIN_TIMEOUT_MS, ScrapeEndpoints,
        SearchProbeTargetError, WebSearchTool, acquire_model_backed_search_inference_participant,
        baidu_search_payload, bocha_error_message, domain_matches, duckduckgo_search_url,
        extract_search_query, finalize_search_response, optional_search_max_results,
        parse_baidu_results, parse_bocha_results, parse_metaso_results, parse_searxng_results,
        parse_sofya_results, parse_tavily_results, parse_volcengine_results,
        register_search_citations, rerank, run_scrape_search_with_endpoints, sanitize_error_body,
        search_probe_target, search_timeout_budgets, searxng_search_url, truncate_error_body,
        volcengine_extract_text,
    };
    use crate::config::SearchProvider;
    use crate::tools::web::contract::{
        BackendId, BackendSearch, CapabilityState, DegradedReason, QueryCapabilities, QueryKnob,
        Recency, SearchQuery, SearchResult,
    };
    use crate::tools::web::scrape::{decode_html_entities, normalize_bing_url};
    use serde_json::json;
    use std::time::{Duration, Instant};

    #[test]
    fn provider_native_receives_dedicated_budget_without_extending_fallback() {
        let requested = Duration::from_millis(15_000);
        let (total, first, fallback) =
            search_timeout_budgets(BackendId::ProviderNative, requested, None);

        assert_eq!(total, Duration::from_millis(60_000));
        assert_eq!(first, Some(Duration::from_millis(45_000)));
        assert_eq!(fallback, Some(requested));

        let (total, first, fallback) = search_timeout_budgets(
            BackendId::ProviderNative,
            requested,
            Some(Duration::from_millis(KIMI_K3_FORMULA_MIN_TIMEOUT_MS)),
        );
        assert_eq!(total, Duration::from_millis(195_000));
        assert_eq!(first, Some(Duration::from_millis(180_000)));
        assert_eq!(fallback, Some(requested));
    }

    #[test]
    fn doctor_search_probe_targets_cover_every_builtin_provider() {
        let cases = [
            (SearchProvider::Bing, "https://www.bing.com/search"),
            (
                SearchProvider::DuckDuckGo,
                "https://html.duckduckgo.com/html/",
            ),
            (
                SearchProvider::Firecrawl,
                "https://api.firecrawl.dev/v2/search",
            ),
            (SearchProvider::Tavily, "https://api.tavily.com/search"),
            (
                SearchProvider::Bocha,
                "https://api.bochaai.com/v1/web-search",
            ),
            (SearchProvider::Metaso, "https://metaso.cn/api/v1"),
            (
                SearchProvider::Baidu,
                "https://qianfan.baidubce.com/v2/ai_search/web_search",
            ),
            (
                SearchProvider::Volcengine,
                "https://ark.cn-beijing.volces.com/api/v3/responses",
            ),
            (SearchProvider::Sofya, "https://sofya.co/v1/search"),
        ];

        for (provider, expected) in cases {
            let target = search_probe_target(provider, None).expect("built-in target");
            assert_eq!(target.url.as_str(), expected, "{provider:?}");
            assert_eq!(target.host, target.url.host_str().unwrap(), "{provider:?}");
        }
    }

    #[test]
    fn doctor_search_probe_strips_every_secret_capable_custom_url_component() {
        let target = search_probe_target(
            SearchProvider::Searxng,
            Some(
                "https://URL-USER:URL-PASSWORD@search.example:8443/private/URL-PATH?URL-QUERY=secret#URL-FRAGMENT",
            ),
        )
        .expect("credential-free target");

        assert_eq!(target.url.as_str(), "https://search.example:8443/");
        assert_eq!(target.host, "search.example");
    }

    #[test]
    fn doctor_search_probe_rejects_configuration_that_runtime_cannot_use() {
        assert_eq!(
            search_probe_target(SearchProvider::Searxng, None),
            Err(SearchProbeTargetError::Missing)
        );
        assert_eq!(
            search_probe_target(SearchProvider::Tavily, Some("https://ignored.example")),
            Err(SearchProbeTargetError::Unsupported)
        );
        assert_eq!(
            search_probe_target(SearchProvider::DuckDuckGo, Some("file:///tmp/search")),
            Err(SearchProbeTargetError::Invalid)
        );
    }

    #[test]
    fn bing_ckurl_with_html_entities_decodes_real_url() {
        let href = "https://www.bing.com/ck/a?!&amp;&amp;p=abc&amp;u=a1aHR0cHM6Ly9ydXN0LWxhbmcub3JnLw&amp;ntb=1";
        assert_eq!(normalize_bing_url(href), "https://rust-lang.org/");
    }

    #[test]
    fn decode_html_entities_handles_named_entities() {
        assert_eq!(decode_html_entities("&amp;"), "&");
        assert_eq!(decode_html_entities("&lt;"), "<");
        assert_eq!(decode_html_entities("&gt;"), ">");
        assert_eq!(decode_html_entities("&quot;"), "\"");
        assert_eq!(decode_html_entities("&apos;"), "'");
        assert_eq!(decode_html_entities("&nbsp;"), " ");
        assert_eq!(decode_html_entities("&copy;"), "\u{00A9}");
        assert_eq!(decode_html_entities("&mdash;"), "\u{2014}");
    }

    #[test]
    fn decode_html_entities_handles_decimal_numeric_references() {
        assert_eq!(decode_html_entities("&#65;"), "A");
        assert_eq!(decode_html_entities("&#60;"), "<");
        assert_eq!(decode_html_entities("&#8211;"), "\u{2013}");
    }

    #[test]
    fn decode_html_entities_handles_hex_numeric_references() {
        assert_eq!(decode_html_entities("&#x41;"), "A");
        assert_eq!(decode_html_entities("&#x3C;"), "<");
        assert_eq!(decode_html_entities("&#x2014;"), "\u{2014}");
    }

    #[test]
    fn decode_html_entities_passthrough_unknown() {
        assert_eq!(decode_html_entities("&unknown;"), "&unknown;");
    }

    #[test]
    fn decode_html_entities_mixed_content() {
        let input = "Hello &amp; welcome to &quot;Rust&apos;s world&quot; &mdash; enjoy!";
        let expected = "Hello & welcome to \"Rust's world\" \u{2014} enjoy!";
        assert_eq!(decode_html_entities(input), expected);
    }

    #[test]
    fn extract_search_query_accepts_legacy_query() {
        let query =
            extract_search_query(&json!({"query": " deepseek v4 "})).expect("query should parse");
        assert_eq!(query, "deepseek v4");
    }

    #[test]
    fn extract_search_query_accepts_q_alias() {
        let query =
            extract_search_query(&json!({"q": "deepseek v4 pro"})).expect("q alias should parse");
        assert_eq!(query, "deepseek v4 pro");
    }

    #[test]
    fn extract_search_query_accepts_array_form() {
        let input = json!({"search_query": [{"q": "deepseek api", "max_results": 3}]});
        let query = extract_search_query(&input).expect("array form should parse");
        assert_eq!(query, "deepseek api");
        assert_eq!(optional_search_max_results(&input), 3);
    }

    #[test]
    fn extract_search_query_rejects_missing_query() {
        let err = extract_search_query(&json!({"max_results": 2}))
            .expect_err("missing query should fail");
        assert!(format!("{err}").contains("missing required field 'query'"));
    }

    #[test]
    fn optional_max_results_prefers_top_level_value() {
        assert_eq!(
            optional_search_max_results(
                &json!({"query": "x", "max_results": 8, "search_query": [{"q": "y", "max_results": 2}]})
            ),
            8,
        );
    }

    #[test]
    fn optional_max_results_falls_back_to_array_form() {
        assert_eq!(
            optional_search_max_results(&json!({"search_query": [{"q": "y", "max_results": 3}]})),
            3,
        );
    }

    #[test]
    fn optional_max_results_uses_default_when_neither_set() {
        assert_eq!(optional_search_max_results(&json!({"query": "x"})), 5);
        assert_eq!(
            optional_search_max_results(&json!({"search_query": [{"q": "y"}]})),
            5,
        );
    }

    #[test]
    fn optional_max_results_only_reads_first_array_entry() {
        assert_eq!(
            optional_search_max_results(
                &json!({"search_query": [{"q": "first", "max_results": 1}, {"q": "second", "max_results": 9}]})
            ),
            1,
        );
    }

    #[test]
    fn extract_search_query_trims_whitespace_from_array_form_q_alias() {
        let q = extract_search_query(&json!({"search_query": [{"q": "  deepseek tui  "}]}))
            .expect("array form should parse with trim");
        assert_eq!(q, "deepseek tui");
    }

    #[test]
    fn extract_search_query_rejects_empty_query() {
        for body in [json!({"query": ""}), json!({"q": "   "}), json!({})] {
            let err = extract_search_query(&body).expect_err("empty query must reject");
            let msg = format!("{err}");
            assert!(
                msg.contains("missing required field 'query'") || msg.contains("Query"),
                "expected query-missing error, got `{msg}`"
            );
        }
    }

    #[test]
    fn truncate_error_body_truncates_long_body() {
        let body = "a".repeat(ERROR_BODY_PREVIEW_BYTES + 100);
        let truncated = truncate_error_body(&body);
        assert!(truncated.len() <= ERROR_BODY_PREVIEW_BYTES + 3);
        assert!(truncated.ends_with("..."));
    }

    #[test]
    fn truncate_error_body_keeps_short_body_intact() {
        let body = "short error";
        assert_eq!(truncate_error_body(body), body);
    }

    #[test]
    fn sanitize_error_body_strips_html_and_control_chars() {
        let body = "<p>error</p>\x00\x01\x02";
        let sanitized = sanitize_error_body(body);
        assert_eq!(sanitized, "error");
    }

    #[test]
    fn sanitize_error_body_redacts_bearer_tokens() {
        let body = r#"{"error":"bad token","authorization":"Bearer test-token/with+chars="}"#;

        let sanitized = sanitize_error_body(body);

        assert!(!sanitized.contains("test-token/with+chars="));
        assert!(sanitized.contains("Bearer [REDACTED]"));
    }

    #[test]
    fn parse_bocha_web_pages_value_extracts_ranked_results() {
        let body = json!({
            "code": 200,
            "msg": null,
            "data": {
                "webPages": {
                    "value": [
                        {
                            "name": "广州天气",
                            "url": "https://bocha.cn/share/weather",
                            "snippet": "广州今日雷阵雨转晴。"
                        },
                        {
                            "name": "中央气象台",
                            "url": "https://www.weather.com.cn/",
                            "summary": "天气实况。"
                        }
                    ]
                }
            }
        });

        let results = parse_bocha_results(&body, 10);

        assert_eq!(results.len(), 2);
        assert_eq!(results[0].title, "广州天气");
        assert_eq!(results[0].url, "https://bocha.cn/share/weather");
        assert_eq!(results[0].snippet.as_deref(), Some("广州今日雷阵雨转晴。"));
        assert_eq!(results[1].title, "中央气象台");
    }

    #[test]
    fn parse_bocha_keeps_legacy_pages_shape() {
        let body = json!({
            "code": 200,
            "data": {
                "pages": [
                    {
                        "title": "Legacy title",
                        "link": "https://example.com/legacy",
                        "description": "Legacy description"
                    }
                ]
            }
        });

        let results = parse_bocha_results(&body, 5);

        assert_eq!(results.len(), 1);
        assert_eq!(results[0].title, "Legacy title");
        assert_eq!(results[0].url, "https://example.com/legacy");
        assert_eq!(results[0].snippet.as_deref(), Some("Legacy description"));
    }

    #[test]
    fn bocha_error_message_flags_non_success_business_code() {
        let body = json!({"code": 401, "msg": "invalid api key"});

        let error = bocha_error_message(&body).expect("non-success code should error");

        assert!(error.contains("Bocha"));
        assert!(error.contains("401"));
        assert!(error.contains("invalid api key"));
    }

    #[test]
    fn parse_baidu_references_extracts_ranked_results() {
        let body = json!({
            "references": [
                {
                    "title": "Rust 官方文档",
                    "url": "https://www.rust-lang.org/",
                    "content": "Rust 是一门注重性能和可靠性的语言。"
                },
                {
                    "title": "Cargo Book",
                    "url": "https://doc.rust-lang.org/cargo/",
                    "snippet": "Cargo is Rust's package manager."
                }
            ]
        });

        let results = parse_baidu_results(&body, 10);

        assert_eq!(results.len(), 2);
        assert_eq!(results[0].title, "Rust 官方文档");
        assert_eq!(results[0].url, "https://www.rust-lang.org/");
        assert_eq!(
            results[0].snippet.as_deref(),
            Some("Rust 是一门注重性能和可靠性的语言。")
        );
        assert_eq!(results[1].title, "Cargo Book");
        assert_eq!(results[1].url, "https://doc.rust-lang.org/cargo/");
        assert_eq!(
            results[1].snippet.as_deref(),
            Some("Cargo is Rust's package manager.")
        );
    }

    #[test]
    fn parse_baidu_references_skips_incomplete_entries() {
        let body = json!({
            "references": [
                {"title": "No URL", "content": "missing url"},
                {"url": "https://example.com/no-title", "content": "missing title"},
                {"title": "Valid", "url": "https://example.com/valid"}
            ]
        });

        let results = parse_baidu_results(&body, 10);

        assert_eq!(results.len(), 1);
        assert_eq!(results[0].title, "Valid");
        assert_eq!(results[0].url, "https://example.com/valid");
        assert_eq!(results[0].snippet, None);
    }

    #[test]
    fn baidu_search_payload_uses_official_search_source() {
        let payload = baidu_search_payload("Rust cargo workspace", 3);

        assert_eq!(
            payload.get("search_source").and_then(|v| v.as_str()),
            Some("baidu_search_v2")
        );
        assert_eq!(
            payload
                .get("messages")
                .and_then(|v| v.as_array())
                .and_then(|messages| messages.first())
                .and_then(|message| message.get("content"))
                .and_then(|v| v.as_str()),
            Some("Rust cargo workspace")
        );
        assert_eq!(
            payload
                .get("resource_type_filter")
                .and_then(|v| v.as_array())
                .and_then(|filters| filters.first())
                .and_then(|filter| filter.get("top_k"))
                .and_then(|v| v.as_u64()),
            Some(3)
        );
    }

    #[test]
    fn parse_sofya_results_falls_back_to_description_for_empty_content() {
        let body = json!({
            "results": [
                {
                    "title": "Full content",
                    "url": "https://example.com/full",
                    "content": "full extracted page content",
                    "description": "unused description"
                },
                {
                    "title": "Null content",
                    "url": "https://example.com/null",
                    "content": null,
                    "description": "description for null content"
                },
                {
                    "title": "Empty content",
                    "url": "https://example.com/empty",
                    "content": "",
                    "description": "description for empty content"
                },
                {
                    "title": "Whitespace content",
                    "url": "https://example.com/blank",
                    "content": "   ",
                    "description": "description for blank content"
                },
                {
                    "title": "No snippet",
                    "url": "https://example.com/no-snippet"
                }
            ]
        });

        let results = parse_sofya_results(&body, 10);

        assert_eq!(results.len(), 5);
        assert_eq!(
            results[0].snippet.as_deref(),
            Some("full extracted page content")
        );
        assert_eq!(
            results[1].snippet.as_deref(),
            Some("description for null content")
        );
        assert_eq!(
            results[2].snippet.as_deref(),
            Some("description for empty content")
        );
        assert_eq!(
            results[3].snippet.as_deref(),
            Some("description for blank content")
        );
        assert_eq!(results[4].snippet, None);
    }

    #[test]
    fn tavily_metaso_and_volcengine_payloads_use_normalized_entry_shape() {
        let tavily = parse_tavily_results(
            &json!({"results": [{
                "title": " Tavily result ",
                "url": "https://tavily.example/result",
                "content": " content "
            }]}),
            5,
        );
        let metaso = parse_metaso_results(
            &json!({"webpages": [{
                "title": " Metaso result ",
                "link": "https://metaso.example/result",
                "summary": " summary "
            }]}),
            5,
        );
        let volcengine = parse_volcengine_results(
            r#"{"results":[{"title":"Volcengine result","url":"https://volc.example/result","snippet":"summary"}]}"#,
            5,
        );

        for (entries, title, snippet) in [
            (tavily, "Tavily result", "content"),
            (metaso, "Metaso result", "summary"),
            (volcengine, "Volcengine result", "summary"),
        ] {
            assert_eq!(entries.len(), 1);
            assert_eq!(entries[0].title, title);
            assert_eq!(entries[0].snippet.as_deref(), Some(snippet));
        }
    }

    #[tokio::test]
    async fn firecrawl_keyless_request_is_headerless_and_keyed_request_is_explicit() {
        use wiremock::matchers::{method, path};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v2/search"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "success": true,
                "data": {"web": [{
                    "title": "Firecrawl result",
                    "url": "https://example.com/firecrawl",
                    "description": "x".repeat(1_200)
                }]}
            })))
            .mount(&server)
            .await;
        let endpoint = format!("{}/v2/search", server.uri());
        let (entries, mode) = WebSearchTool
            .run_firecrawl_search_at(&endpoint, "codewhale", 5, 5_000, None)
            .await
            .expect("keyless Firecrawl search");
        WebSearchTool
            .run_firecrawl_search_at(&endpoint, "codewhale", 5, 5_000, Some("fc-secret"))
            .await
            .expect("authenticated Firecrawl search");
        let requests = server.received_requests().await.expect("recorded requests");
        let payload: serde_json::Value =
            serde_json::from_slice(&requests[0].body).expect("request JSON");

        assert_eq!(entries[0].title, "Firecrawl result");
        assert_eq!(entries[0].url, "https://example.com/firecrawl");
        assert_eq!(
            entries[0].snippet.as_deref().unwrap().chars().count(),
            1_000
        );
        assert_eq!(mode, "Firecrawl keyless");
        assert!(requests[0].headers.get("authorization").is_none());
        assert_eq!(requests[1].headers["authorization"], "Bearer fc-secret");
        assert!(payload.get("integration").is_none());
        assert_eq!(payload["sources"][0]["type"], "web");
    }

    #[tokio::test]
    async fn firecrawl_keyless_rate_limit_is_actionable() {
        use wiremock::matchers::{method, path};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v2/search"))
            .respond_with(ResponseTemplate::new(429))
            .mount(&server)
            .await;
        let error = WebSearchTool
            .run_firecrawl_search_at(
                &format!("{}/v2/search", server.uri()),
                "quota",
                5,
                5_000,
                None,
            )
            .await
            .expect_err("429 must be actionable");
        assert!(error.to_string().contains("keyless quota is exhausted"));
        assert!(error.to_string().contains("FIRECRAWL_API_KEY"));
    }

    #[test]
    fn volcengine_extract_text_skips_non_text_content_blocks() {
        let body = json!({
            "output": [
                {
                    "type": "message",
                    "content": [
                        {"type": "reasoning", "summary": "thinking first"},
                        {"type": "output_text", "text": "{\"results\":[]}"}
                    ]
                }
            ]
        });

        assert_eq!(
            volcengine_extract_text(&body).as_deref(),
            Some("{\"results\":[]}")
        );
    }

    #[tokio::test]
    async fn volcengine_model_search_waits_for_runtime_chat_ownership() {
        let ownership = crate::client::acquire_runtime_chat_inference_ownership().await;
        let mut participant =
            tokio::spawn(async { acquire_model_backed_search_inference_participant().await });
        assert!(
            tokio::time::timeout(std::time::Duration::from_millis(40), &mut participant)
                .await
                .is_err(),
            "model-backed web search must wait behind Runtime Chat ownership"
        );
        drop(ownership);
        let permit = tokio::time::timeout(std::time::Duration::from_secs(1), participant)
            .await
            .expect("model-backed search resumes after relay settlement")
            .expect("model-backed search participant task");
        drop(permit);
    }

    #[tokio::test]
    async fn baidu_provider_without_api_key_surfaces_clear_error_not_silent_fallback() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolSpec};

        let prev = std::env::var_os("BAIDU_SEARCH_API_KEY");
        unsafe { std::env::remove_var("BAIDU_SEARCH_API_KEY") };

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Baidu;
        ctx.search_api_key = None;
        let err = WebSearchTool
            .execute(json!({"query": "anything"}), &ctx)
            .await
            .expect_err("missing api_key must surface as ToolError");

        match prev {
            Some(value) => unsafe { std::env::set_var("BAIDU_SEARCH_API_KEY", value) },
            None => unsafe { std::env::remove_var("BAIDU_SEARCH_API_KEY") },
        }

        let msg = err.to_string();
        assert!(
            msg.contains("Baidu") && msg.contains("API key"),
            "error must name the provider and missing key; got `{msg}`"
        );
    }

    #[tokio::test]
    #[allow(clippy::await_holding_lock)]
    async fn sofya_provider_without_api_key_surfaces_clear_error_not_silent_fallback() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolSpec};

        let _guard = crate::test_support::lock_test_env();
        let prev = std::env::var_os("SOFYA_API_KEY");
        unsafe { std::env::remove_var("SOFYA_API_KEY") };

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Sofya;
        ctx.search_api_key = None;
        let err = WebSearchTool
            .execute(json!({"query": "anything"}), &ctx)
            .await
            .expect_err("missing api_key must surface as ToolError");

        match prev {
            Some(value) => unsafe { std::env::set_var("SOFYA_API_KEY", value) },
            None => unsafe { std::env::remove_var("SOFYA_API_KEY") },
        }

        let msg = err.to_string();
        assert!(
            msg.contains("Sofya") && msg.contains("API key"),
            "error must name the provider and missing key; got `{msg}`"
        );
    }

    #[tokio::test]
    #[allow(clippy::await_holding_lock)]
    async fn volcengine_provider_without_api_key_lists_supported_env_fallbacks() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolSpec};

        let _guard = crate::test_support::lock_test_env();
        let prev_volc = std::env::var_os("VOLCENGINE_API_KEY");
        let prev_volc_ark = std::env::var_os("VOLCENGINE_ARK_API_KEY");
        let prev_ark = std::env::var_os("ARK_API_KEY");
        unsafe {
            std::env::remove_var("VOLCENGINE_API_KEY");
            std::env::remove_var("VOLCENGINE_ARK_API_KEY");
            std::env::remove_var("ARK_API_KEY");
        }

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Volcengine;
        ctx.search_api_key = None;
        let err = WebSearchTool
            .execute(json!({"query": "anything"}), &ctx)
            .await
            .expect_err("missing api_key must surface as ToolError");

        match prev_volc {
            Some(value) => unsafe { std::env::set_var("VOLCENGINE_API_KEY", value) },
            None => unsafe { std::env::remove_var("VOLCENGINE_API_KEY") },
        }
        match prev_volc_ark {
            Some(value) => unsafe { std::env::set_var("VOLCENGINE_ARK_API_KEY", value) },
            None => unsafe { std::env::remove_var("VOLCENGINE_ARK_API_KEY") },
        }
        match prev_ark {
            Some(value) => unsafe { std::env::set_var("ARK_API_KEY", value) },
            None => unsafe { std::env::remove_var("ARK_API_KEY") },
        }

        let msg = err.to_string();
        assert!(msg.contains("Volcengine") && msg.contains("API key"));
        assert!(msg.contains("VOLCENGINE_API_KEY"));
        assert!(msg.contains("VOLCENGINE_ARK_API_KEY"));
        assert!(msg.contains("ARK_API_KEY"));
        assert!(!msg.contains("DEEPSEEK_SEARCH_API_KEY"));
    }

    #[tokio::test]
    #[allow(clippy::await_holding_lock)]
    async fn metaso_provider_without_api_key_fails_closed_before_fallback() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolSpec};

        let _guard = crate::test_support::lock_test_env();
        let previous = std::env::var_os("METASO_API_KEY");
        unsafe { std::env::remove_var("METASO_API_KEY") };

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Metaso;
        ctx.search_api_key = None;
        let error = WebSearchTool
            .execute(json!({"query": "anything"}), &ctx)
            .await
            .expect_err("missing Metaso key must fail before the fallback chain");

        match previous {
            Some(value) => unsafe { std::env::set_var("METASO_API_KEY", value) },
            None => unsafe { std::env::remove_var("METASO_API_KEY") },
        }

        let message = error.to_string();
        assert!(
            message.contains("Metaso")
                && message.contains("API key")
                && message.contains("METASO_API_KEY"),
            "got `{message}`"
        );
        assert!(
            !message.contains("duckduckgo"),
            "missing configuration must not cross providers: `{message}`"
        );
    }

    #[test]
    fn duckduckgo_compatible_url_uses_custom_base_url_and_preserves_query() {
        let (url, host) = duckduckgo_search_url(
            Some("https://search.internal.example/html/?region=us"),
            "rust async",
            None,
        )
        .expect("custom duckduckgo-compatible url");

        assert_eq!(host, "search.internal.example");
        assert_eq!(
            url,
            "https://search.internal.example/html/?region=us&q=rust+async"
        );
    }

    #[test]
    fn bing_locale_params_follow_explicit_locale() {
        let (params, accept_language) = super::scrape_locale_params(Some("zh-CN"), "rust async");
        assert_eq!(
            params,
            vec![
                ("mkt".to_string(), "zh-CN".to_string()),
                ("setlang".to_string(), "zh-Hans".to_string()),
            ]
        );
        assert_eq!(accept_language, "zh-CN,zh;q=0.9,en;q=0.8");

        let (params, accept_language) = super::scrape_locale_params(Some("en-US"), "rust async");
        assert_eq!(
            params,
            vec![
                ("mkt".to_string(), "en-US".to_string()),
                ("setlang".to_string(), "en".to_string()),
            ]
        );
        assert_eq!(accept_language, "en-US,en;q=0.9");

        // Bing's setlang table keys the Chinese script off the region:
        // Taiwan/Hong Kong/Macao are Traditional, everything else (including
        // the Han-heuristic default) is Simplified.
        let (params, _) = super::scrape_locale_params(Some("zh-TW"), "rust async");
        assert_eq!(params[1].1, "zh-Hant");
        let (params, _) = super::scrape_locale_params(Some("zh-HK"), "rust async");
        assert_eq!(params[1].1, "zh-Hant");
    }

    #[test]
    fn bing_locale_params_fall_back_to_china_market_for_han_queries() {
        // Without this fallback a locale-less Chinese query used to get no
        // market hint plus an English Accept-Language, and Bing served
        // unrelated Japanese results.
        let (params, accept_language) = super::scrape_locale_params(None, "凹语言 编程");
        assert_eq!(
            params,
            vec![
                ("mkt".to_string(), "zh-CN".to_string()),
                ("setlang".to_string(), "zh-Hans".to_string()),
            ]
        );
        assert_eq!(accept_language, "zh-CN,zh;q=0.9,en;q=0.8");
    }

    #[test]
    fn bing_locale_params_keep_english_default_without_locale_signal() {
        let (params, accept_language) = super::scrape_locale_params(None, "rust async");
        assert!(params.is_empty());
        assert_eq!(accept_language, "en-US,en;q=0.9");
    }

    #[test]
    fn bing_locale_params_normalize_underscore_separators() {
        // The model often writes `zh_CN`; the underscore must not leak into
        // the `mkt` parameter or the `Accept-Language` ranges.
        let (params, accept_language) = super::scrape_locale_params(Some("zh_CN"), "rust async");
        assert_eq!(
            params,
            vec![
                ("mkt".to_string(), "zh-CN".to_string()),
                ("setlang".to_string(), "zh-Hans".to_string()),
            ]
        );
        assert_eq!(accept_language, "zh-CN,zh;q=0.9,en;q=0.8");
    }

    #[test]
    fn bing_setlang_keeps_an_explicit_script_subtag() {
        // `zh-Hant-TW` must not degrade to Simplified: the explicit script
        // subtag wins over the region-derived default.
        let (params, _) = super::scrape_locale_params(Some("zh-Hant-TW"), "rust async");
        assert_eq!(params[1].1, "zh-Hant");
        let (params, _) = super::scrape_locale_params(Some("zh-Hans-CN"), "rust async");
        assert_eq!(params[1].1, "zh-Hans");
        let (params, _) = super::scrape_locale_params(Some("zh-Hant"), "rust async");
        assert_eq!(params[1].1, "zh-Hant");
    }

    #[test]
    fn ddg_locale_ignored_only_when_an_explicit_locale_yields_no_region() {
        // Mapped regions are honored.
        assert!(!super::ddg_locale_was_ignored(Some("zh-CN"), Some("zh-CN")));
        // Unmapped but well-formed regions send no `kl`: the receipt must
        // not claim the knob was honored.
        assert!(super::ddg_locale_was_ignored(Some("fr-FR"), Some("fr-FR")));
        // Malformed values resolve no market at all.
        assert!(super::ddg_locale_was_ignored(Some("zh CN"), None));
        // Malformed values stay ignored even when a pure-Han query makes
        // scrape_market fall back to the zh-CN market — the shape check is
        // independent of the market resolution, matching the Bing leg.
        assert!(super::ddg_locale_was_ignored(Some("zh CN"), Some("zh-CN")));
        assert!(super::ddg_locale_was_ignored(Some("中文"), Some("zh-CN")));
        // No explicit locale: the Han fallback is the tool's own choice,
        // never a dropped knob.
        assert!(!super::ddg_locale_was_ignored(None, Some("zh-CN")));
        assert!(!super::ddg_locale_was_ignored(None, None));
    }

    #[test]
    fn forkguard_ddg_malformed_locale_with_han_query_receipt_stays_ignored() {
        // End-to-end across the DDG leg: a malformed locale plus a pure-Han
        // query resolves the zh-CN market via the Han heuristic, but the
        // receipt must keep KnobIgnored (honored.locale=false), exactly what
        // the Bing leg reports for the same input.
        let locale = Some("中文");
        let query = "凹语言 编程";
        let market = super::scrape_market(locale, query);
        assert_eq!(market.as_deref(), Some("zh-CN"));
        let degraded: Vec<DegradedReason> =
            if super::ddg_locale_was_ignored(locale, market.as_deref()) {
                vec![DegradedReason::KnobIgnored {
                    knob: QueryKnob::Locale,
                }]
            } else {
                Vec::new()
            };
        let raw = BackendSearch {
            backend: BackendId::DuckDuckGo,
            source: "duckduckgo".to_string(),
            backend_detail: None,
            results: Vec::new(),
            degraded,
            note: None,
        };
        let query = SearchQuery::new(
            query.to_string(),
            5,
            None,
            Vec::new(),
            locale.map(str::to_string),
        );
        let capabilities = crate::tools::web::contract::QueryCapabilities {
            max_results: crate::tools::web::contract::CapabilityState::Supported,
            recency: crate::tools::web::contract::CapabilityState::Unsupported,
            domains: crate::tools::web::contract::CapabilityState::Unsupported,
            locale: crate::tools::web::contract::CapabilityState::Supported,
            published_date: crate::tools::web::contract::CapabilityState::Unknown,
        };
        let response = finalize_search_response(query, capabilities, raw, Instant::now());
        assert!(
            !response.receipt.honored.locale,
            "a malformed locale must be receipted as ignored even when the \
             Han heuristic supplies a market"
        );
        assert!(response.receipt.degraded.iter().any(|reason| matches!(
            reason,
            DegradedReason::KnobIgnored {
                knob: QueryKnob::Locale
            }
        )));

        // fr-FR (well-formed, unmapped on the verified kl list) stays
        // ignored on the DDG leg, as before the fix.
        let market = super::scrape_market(Some("fr-FR"), "rust async");
        assert_eq!(market.as_deref(), Some("fr-FR"));
        assert!(super::ddg_locale_was_ignored(
            Some("fr-FR"),
            market.as_deref()
        ));

        // A mapped, well-formed value (ja-JP) is honored.
        let market = super::scrape_market(Some("ja-JP"), "rust async");
        assert_eq!(market.as_deref(), Some("ja-JP"));
        assert!(!super::ddg_locale_was_ignored(
            Some("ja-JP"),
            market.as_deref()
        ));
    }

    #[test]
    fn bing_locale_ignored_only_for_malformed_explicit_values() {
        assert!(!super::bing_locale_was_ignored(Some("zh-CN")));
        assert!(!super::bing_locale_was_ignored(None));
        assert!(super::bing_locale_was_ignored(Some("zh CN")));
        assert!(super::bing_locale_was_ignored(Some("-CN")));
    }

    #[test]
    fn han_detection_covers_han_only_and_ignores_other_cjk_scripts() {
        assert!(super::query_contains_han("学 rust"));
        assert!(super::query_contains_han("\u{3400}")); // Extension A
        assert!(!super::query_contains_han("rust async 123"));
        assert!(!super::query_contains_han("ルスト programming")); // Katakana
        assert!(!super::query_contains_han("러스트 programming")); // Hangul
    }

    #[test]
    fn duckduckgo_url_adds_kl_for_resolved_market() {
        let (url, _) =
            duckduckgo_search_url(None, "凹语言 编程", Some("zh-CN")).expect("duckduckgo url");
        let parsed = reqwest::Url::parse(&url).expect("valid url");
        assert_eq!(
            parsed.query_pairs().find(|(key, _)| key == "kl").unwrap().1,
            "cn-zh"
        );

        // A locale with no verified kl pair sends no kl at all rather than
        // an off-list guess.
        let (url, _) =
            duckduckgo_search_url(None, "rust async", Some("fr-FR")).expect("duckduckgo url");
        let parsed = reqwest::Url::parse(&url).expect("valid url");
        assert!(parsed.query_pairs().all(|(key, _)| key != "kl"));

        let (url, _) = duckduckgo_search_url(None, "rust async", None).expect("duckduckgo url");
        let parsed = reqwest::Url::parse(&url).expect("valid url");
        assert!(parsed.query_pairs().all(|(key, _)| key != "kl"));
    }

    #[test]
    fn ddg_region_param_translates_only_verified_pairs() {
        // DuckDuckGo's kl list is fixed and non-systematic; only pairs
        // verified against that list are translated (China, US, Japan,
        // Korea, Taiwan, Hong Kong). A mechanical reversal of a BCP 47 tag
        // produced off-list junk such as `jp-ja` (the real value is
        // `jp-jp`), so anything unverified sends no kl at all.
        assert_eq!(super::ddg_region_param("zh-CN").as_deref(), Some("cn-zh"));
        assert_eq!(super::ddg_region_param("en_US").as_deref(), Some("us-en"));
        assert_eq!(super::ddg_region_param("ja-JP").as_deref(), Some("jp-jp"));
        assert_eq!(super::ddg_region_param("ko-KR").as_deref(), Some("kr-kr"));
        assert_eq!(super::ddg_region_param("zh-TW").as_deref(), Some("tw-tzh"));
        assert_eq!(super::ddg_region_param("zh-HK").as_deref(), Some("hk-tzh"));
        // Unverified locales and script/bare tags send no region hint —
        // the no-kl request is the historical shape and strictly safer
        // than guessing an off-list value.
        assert_eq!(super::ddg_region_param("fr-FR"), None);
        assert_eq!(super::ddg_region_param("zh-Hans"), None);
        assert_eq!(super::ddg_region_param("zh"), None);
        // Script+region Chinese tags reduce to their verified region pair:
        // the schema teaches BCP 47 where the script subtag is best
        // practice for Chinese.
        assert_eq!(
            super::ddg_region_param("zh-Hans-CN").as_deref(),
            Some("cn-zh")
        );
        assert_eq!(
            super::ddg_region_param("zh-Hant-TW").as_deref(),
            Some("tw-tzh")
        );
        assert_eq!(
            super::ddg_region_param("zh-Hant-HK").as_deref(),
            Some("hk-tzh")
        );
    }

    #[test]
    fn bing_fallback_rederives_the_locale_flag_from_bings_rule() {
        // A shape-valid locale the DuckDuckGo leg could not map (no kl
        // pair) is carried by the Bing fallback through mkt/setlang, so the
        // stale DDG-leg flag must not keep the receipt claiming the knob
        // was ignored.
        let mut degraded = vec![DegradedReason::KnobIgnored {
            knob: QueryKnob::Locale,
        }];
        super::prune_fallback_locale_ignored(&mut degraded, Some("fr-FR"));
        assert!(
            !degraded.iter().any(|reason| matches!(
                reason,
                DegradedReason::KnobIgnored {
                    knob: QueryKnob::Locale,
                }
            )),
            "the DDG-leg ignored flag must not survive the Bing fallback: \
             {degraded:?}"
        );
        // An implausible tag stays ignored on the fallback too.
        let mut degraded = vec![DegradedReason::KnobIgnored {
            knob: QueryKnob::Locale,
        }];
        super::prune_fallback_locale_ignored(&mut degraded, Some("zh CN"));
        assert!(degraded.iter().any(|reason| matches!(
            reason,
            DegradedReason::KnobIgnored {
                knob: QueryKnob::Locale,
            }
        )));
    }

    #[test]
    fn ddg_accept_language_unifies_with_bing_rule() {
        assert_eq!(super::scrape_accept_language(None), "en-US,en;q=0.9");
        assert_eq!(
            super::scrape_accept_language(Some("zh-CN")),
            "zh-CN,zh;q=0.9,en;q=0.8"
        );
        // A bare-language market (reachable through an explicit `locale: en`)
        // emits a single range and, like every English market, never appends
        // a duplicate `en` fallback.
        assert_eq!(super::scrape_accept_language(Some("en")), "en;q=0.9");
    }

    #[test]
    fn implausible_locale_values_are_treated_as_no_locale() {
        // The locale reaches URLs and the Accept-Language header, so a
        // malformed model-supplied value keeps the historical no-market
        // request instead of becoming a broken tag or header value.
        assert_eq!(super::scrape_market(Some("-CN"), "rust async"), None);
        assert_eq!(super::scrape_market(Some("zh CN"), "rust async"), None);
        assert_eq!(super::scrape_market(Some(""), "rust async"), None);
        assert_eq!(super::scrape_market(Some("zh=CN"), "rust async"), None);
        assert_eq!(super::scrape_market(Some("  "), "rust async"), None);
        // Digits are not a language: the primary subtag must be alphabetic,
        // or the value would be transmitted (and receipted as honored)
        // against schema text promising malformed values are ignored.
        assert_eq!(super::scrape_market(Some("12345"), "rust async"), None);
        assert_eq!(
            super::scrape_market(Some("en-123456789"), "rust async"),
            None
        );
        // Well-formed tags survive the shape check verbatim.
        assert_eq!(
            super::scrape_market(Some("zh-TW"), "rust async").as_deref(),
            Some("zh-TW")
        );
        // The Han fallback is untouched by the shape check.
        assert_eq!(
            super::scrape_market(None, "凹语言 编程").as_deref(),
            Some("zh-CN")
        );
    }

    #[test]
    fn han_fallback_vetoes_kana_and_hangul_queries() {
        // A Han-bearing query with kana is Japanese: pushing it onto the
        // zh-CN market would be the inverse of the drift the fallback
        // exists to fix, so it keeps the no-market request.
        assert_eq!(super::scrape_market(None, "東京の天気"), None);
        assert_eq!(super::scrape_market(None, "日本の地図"), None);
        // Hangul-bearing queries likewise never take the fallback.
        assert_eq!(super::scrape_market(None, "러스트 프로그래밍"), None);
        // Purely Han queries (indistinguishable from Chinese) and an
        // explicit locale keep the existing behavior.
        assert_eq!(
            super::scrape_market(None, "中国 的 首都").as_deref(),
            Some("zh-CN")
        );
        assert_eq!(
            super::scrape_market(Some("ja-JP"), "東京の天気").as_deref(),
            Some("ja-JP")
        );
    }

    #[test]
    fn custom_duckduckgo_endpoint_disables_public_bing_fallback() {
        assert!(super::duckduckgo_allows_bing_fallback(None));
        assert!(super::duckduckgo_allows_bing_fallback(Some("   ")));
        assert!(!super::duckduckgo_allows_bing_fallback(Some(
            "https://search.internal.example/html/"
        )));
    }

    #[test]
    fn searxng_url_uses_search_path_and_json_format() {
        let (url, host) =
            searxng_search_url(Some("https://search.example/"), "rust async").expect("searxng url");
        let parsed = reqwest::Url::parse(&url).expect("valid url");
        assert_eq!(host, "search.example");
        assert_eq!(parsed.path(), "/search");
        assert_eq!(
            parsed.query_pairs().find(|(key, _)| key == "q").unwrap().1,
            "rust async"
        );
        assert_eq!(
            parsed
                .query_pairs()
                .find(|(key, _)| key == "format")
                .unwrap()
                .1,
            "json"
        );

        let (subpath_url, _) = searxng_search_url(
            Some("https://search.example/searxng?language=en"),
            "codewhale",
        )
        .expect("searxng subpath url");
        let parsed = reqwest::Url::parse(&subpath_url).expect("valid subpath url");
        assert_eq!(parsed.path(), "/searxng/search");
        assert_eq!(
            parsed
                .query_pairs()
                .find(|(key, _)| key == "language")
                .unwrap()
                .1,
            "en"
        );

        let (search_url, _) =
            searxng_search_url(Some("https://search.example/searxng/search"), "codewhale")
                .expect("searxng search endpoint");
        assert_eq!(
            reqwest::Url::parse(&search_url)
                .expect("valid search url")
                .path(),
            "/searxng/search"
        );
    }

    #[test]
    fn searxng_parser_normalizes_results() {
        let parsed = json!({
            "results": [
                {
                    "title": " Rust async ",
                    "url": " https://example.com/rust ",
                    "content": " Result content "
                },
                {
                    "title": "Empty snippet",
                    "url": "https://example.com/empty",
                    "content": "   ",
                    "snippet": " Fallback snippet "
                },
                {
                    "title": "",
                    "url": "https://example.com/missing-title",
                    "content": "ignored"
                },
                {
                    "title": "Missing URL",
                    "content": "ignored"
                }
            ]
        });

        let results = parse_searxng_results(&parsed, 10);
        assert_eq!(results.len(), 2);
        assert_eq!(results[0].title, "Rust async");
        assert_eq!(results[0].url, "https://example.com/rust");
        assert_eq!(results[0].snippet.as_deref(), Some("Result content"));
        assert_eq!(results[1].snippet.as_deref(), Some("Fallback snippet"));
    }

    #[tokio::test]
    async fn searxng_provider_requires_base_url() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolSpec};

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Searxng;
        ctx.search_base_url = None;

        let err = WebSearchTool
            .execute(json!({"query": "rust async"}), &ctx)
            .await
            .expect_err("searxng requires explicit base_url");
        let msg = err.to_string();
        assert!(
            matches!(err, crate::tools::spec::ToolError::InvalidInput { .. }),
            "missing base_url is a configuration gap, not a transport failure: {err:?}"
        );
        assert!(
            msg.contains("SearXNG")
                && msg.contains("base_url")
                && msg.contains("no public instance"),
            "got `{msg}`"
        );
    }

    #[tokio::test]
    async fn missing_provider_key_fails_closed_as_not_configured() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolError, ToolSpec};

        for provider in [SearchProvider::Tavily, SearchProvider::Bocha] {
            let tmp = tempfile::tempdir().expect("tempdir");
            let mut ctx = ToolContext::new(tmp.path().to_path_buf());
            ctx.search_provider = provider;
            ctx.search_api_key = None;

            let error = WebSearchTool
                .execute(json!({"query": "needs configuration"}), &ctx)
                .await
                .expect_err("a keyed provider without an API key must fail closed");
            assert!(
                matches!(error, ToolError::InvalidInput { .. }),
                "config gaps must stay distinguishable from transport failures: {error:?}"
            );
            let message = error.to_string();
            assert!(message.contains("is not configured"), "got `{message}`");
            assert!(message.contains("api_key"), "got `{message}`");
        }
    }

    #[tokio::test]
    async fn searxng_search_returns_json_results() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolSpec};
        use wiremock::matchers::{method, path, query_param};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/search"))
            .and(query_param("q", "rust async"))
            .and(query_param("format", "json"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "results": [
                    {
                        "title": "Rust async",
                        "url": "https://example.com/rust",
                        "content": "Async Rust result"
                    }
                ]
            })))
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Searxng;
        ctx.search_base_url = Some(server.uri());

        let result = WebSearchTool
            .execute(json!({"query": "rust async"}), &ctx)
            .await
            .expect("searxng endpoint should return results");
        let value: serde_json::Value =
            serde_json::from_str(&result.content).expect("web search json response");

        assert_eq!(value["source"].as_str(), Some("searxng"));
        assert_eq!(value["count"].as_u64(), Some(1));
        assert_eq!(value["results"][0]["rank"].as_u64(), Some(1));
        assert_eq!(value["results"][0]["domain"], "example.com");
        assert_eq!(value["receipt"]["backend"], "searxng");
        assert_eq!(
            value["receipt"]["backend_detail"].as_str(),
            Some("127.0.0.1")
        );
        assert!(
            value["message"]
                .as_str()
                .expect("message")
                .contains("Backend: searxng at")
        );
    }

    #[tokio::test]
    async fn unsupported_knobs_are_visible_and_domains_are_post_filtered() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolSpec};
        use wiremock::matchers::{method, path, query_param};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/search"))
            .and(query_param("q", "fresh rust"))
            .and(query_param("format", "json"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "results": [
                    {"title": "Keep", "url": "https://docs.example.com/rust", "content": "kept"},
                    {"title": "Drop", "url": "https://other.test/rust", "content": "dropped"}
                ]
            })))
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Searxng;
        ctx.search_base_url = Some(server.uri());

        let result = WebSearchTool
            .execute(
                json!({
                    "query": "fresh rust",
                    "recency": "week",
                    "domains": ["example.com"],
                    "locale": "en-US"
                }),
                &ctx,
            )
            .await
            .expect("structured query should execute");
        let value: serde_json::Value =
            serde_json::from_str(&result.content).expect("web search json response");

        assert_eq!(value["count"], 1);
        assert_eq!(value["results"][0]["domain"], "docs.example.com");
        assert_eq!(value["receipt"]["honored"]["domains"], true);
        assert_eq!(value["receipt"]["honored"]["recency"], false);
        assert_eq!(value["receipt"]["honored"]["locale"], false);
        let degraded = value["receipt"]["degraded"]
            .as_array()
            .expect("degraded receipt array");
        assert!(
            degraded
                .iter()
                .any(|item| { item["kind"] == "post_filtered" && item["knob"] == "domains" })
        );
        assert!(
            degraded
                .iter()
                .any(|item| { item["kind"] == "knob_ignored" && item["knob"] == "recency" })
        );
        assert!(
            degraded
                .iter()
                .any(|item| { item["kind"] == "knob_ignored" && item["knob"] == "locale" })
        );
    }

    #[test]
    fn provider_native_domain_filter_is_reported_as_provider_honored() {
        let query = SearchQuery::new(
            "current release".to_string(),
            3,
            Some(Recency::Week),
            vec!["example.com".to_string()],
            None,
        );
        let raw = BackendSearch {
            backend: BackendId::ProviderNative,
            source: "provider-native/xai/grok-4.5".to_string(),
            backend_detail: Some("api.x.ai".to_string()),
            results: vec![SearchResult::new(
                1,
                "Exact source".to_string(),
                "https://docs.example.com/release".to_string(),
                None,
                None,
            )],
            degraded: Vec::new(),
            note: Some("Grounded answer.".to_string()),
        };
        let response = finalize_search_response(
            query,
            QueryCapabilities {
                max_results: CapabilityState::Supported,
                recency: CapabilityState::Unsupported,
                domains: CapabilityState::Supported,
                locale: CapabilityState::Unsupported,
                published_date: CapabilityState::Unknown,
            },
            raw,
            Instant::now(),
        );

        assert!(response.receipt.honored.max_results);
        assert!(response.receipt.honored.domains);
        assert!(!response.receipt.honored.recency);
        assert!(response.receipt.degraded.iter().any(|reason| matches!(
            reason,
            DegradedReason::KnobIgnored {
                knob: QueryKnob::Recency
            }
        )));
        assert!(!response.receipt.degraded.iter().any(|reason| matches!(
            reason,
            DegradedReason::PostFiltered {
                knob: QueryKnob::Domains
            }
        )));
        assert!(response.message.contains("Grounded answer."));
    }

    #[test]
    fn provider_native_discards_answer_when_domain_filter_removes_a_source() {
        let query = SearchQuery::new(
            "current release".to_string(),
            3,
            None,
            vec!["example.com".to_string()],
            None,
        );
        let raw = BackendSearch {
            backend: BackendId::ProviderNative,
            source: "provider-native/xai/grok-4.5".to_string(),
            backend_detail: Some("api.x.ai".to_string()),
            results: vec![
                SearchResult::new(
                    1,
                    "Allowed source".to_string(),
                    "https://docs.example.com/release".to_string(),
                    None,
                    None,
                ),
                SearchResult::new(
                    2,
                    "Leaked source".to_string(),
                    "https://outside.test/release".to_string(),
                    None,
                    None,
                ),
            ],
            degraded: Vec::new(),
            note: Some("Answer synthesized from both sources.".to_string()),
        };
        let response = finalize_search_response(
            query,
            QueryCapabilities {
                max_results: CapabilityState::Supported,
                recency: CapabilityState::Unsupported,
                domains: CapabilityState::Supported,
                locale: CapabilityState::Unsupported,
                published_date: CapabilityState::Unknown,
            },
            raw,
            Instant::now(),
        );

        assert_eq!(response.count, 1);
        assert_eq!(response.results[0].domain, "docs.example.com");
        assert_eq!(response.message, "Found 1 result(s)");
        assert!(response.receipt.degraded.iter().any(|reason| matches!(
            reason,
            DegradedReason::PostFiltered {
                knob: QueryKnob::Domains
            }
        )));
    }

    #[test]
    fn search_results_receive_session_scoped_refs_and_sanitize_credential_urls() {
        let query = SearchQuery::new("sources".to_string(), 5, None, Vec::new(), None);
        let raw = BackendSearch {
            backend: BackendId::DuckDuckGo,
            source: "duckduckgo".to_string(),
            backend_detail: None,
            results: vec![
                SearchResult::new(
                    1,
                    "Valid".to_string(),
                    "https://example.com/source#section".to_string(),
                    None,
                    None,
                ),
                SearchResult::new(
                    2,
                    "Protected".to_string(),
                    "https://example.com/protected?access_token=sensitive&view=full".to_string(),
                    None,
                    None,
                ),
            ],
            degraded: Vec::new(),
            note: None,
        };
        let mut response =
            finalize_search_response(query, QueryCapabilities::count_only(), raw, Instant::now());
        let context = crate::tools::spec::ToolContext::new(std::path::PathBuf::from("."))
            .with_state_namespace("search-citation-session");

        register_search_citations(&mut response, &context);

        assert_eq!(response.count, 2);
        assert_eq!(response.results[0].url, "https://example.com/source");
        assert_eq!(
            response.results[1].url,
            "https://example.com/protected?view=full"
        );
        assert!(!response.results[1].url.contains("sensitive"));
        assert!(response.results[0].ref_id.starts_with("web_"));
        assert!(
            crate::tools::web::citations::resolve(
                "search-citation-session",
                &response.results[0].ref_id
            )
            .is_some()
        );
        assert!(
            crate::tools::web::citations::resolve(
                "foreign-search-citation-session",
                &response.results[0].ref_id
            )
            .is_none()
        );
    }

    #[tokio::test]
    async fn searxng_empty_results_report_backend() {
        use crate::config::SearchProvider;
        use crate::tools::spec::ToolContext;
        use wiremock::matchers::{method, path, query_param};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/search"))
            .and(query_param("q", "empty"))
            .and(query_param("format", "json"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({"results": []})))
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Searxng;
        ctx.search_base_url = Some(server.uri());

        let (results, host) = WebSearchTool
            .run_searxng_search("empty", 5, 5_000, &ctx)
            .await
            .expect("empty SearXNG adapter response should be successful");
        let expected_host = reqwest::Url::parse(&server.uri())
            .expect("mock URL")
            .host_str()
            .expect("mock host")
            .to_string();

        assert!(results.is_empty());
        assert_eq!(host, expected_host);
    }

    #[tokio::test]
    async fn searxng_http_errors_are_actionable() {
        use crate::config::SearchProvider;
        use crate::tools::spec::ToolContext;
        use wiremock::matchers::{method, path, query_param};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/search"))
            .and(query_param("q", "blocked"))
            .and(query_param("format", "json"))
            .respond_with(ResponseTemplate::new(403).set_body_string("json disabled"))
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Searxng;
        ctx.search_base_url = Some(server.uri());

        let err = WebSearchTool
            .run_searxng_search("blocked", 5, 5_000, &ctx)
            .await
            .expect_err("403 should be actionable");
        let msg = err.to_string();
        assert!(
            msg.contains("HTTP 403")
                && msg.contains("JSON output")
                && msg.contains("permits API access"),
            "got `{msg}`"
        );
    }

    #[tokio::test]
    async fn searxng_rate_limit_error_mentions_configured_instance() {
        use crate::config::SearchProvider;
        use crate::tools::spec::ToolContext;
        use wiremock::matchers::{method, path, query_param};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/search"))
            .and(query_param("q", "later"))
            .and(query_param("format", "json"))
            .respond_with(ResponseTemplate::new(429).set_body_string("too many requests"))
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Searxng;
        ctx.search_base_url = Some(server.uri());

        let err = WebSearchTool
            .run_searxng_search("later", 5, 5_000, &ctx)
            .await
            .expect_err("429 should be actionable");
        let msg = err.to_string();
        assert!(
            msg.contains("HTTP 429")
                && msg.contains("rate-limiting")
                && msg.contains("trusted/self-hosted instance"),
            "got `{msg}`"
        );
    }

    #[tokio::test]
    async fn searxng_invalid_json_is_actionable() {
        use crate::config::SearchProvider;
        use crate::tools::spec::ToolContext;
        use wiremock::matchers::{method, path, query_param};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/search"))
            .and(query_param("q", "html"))
            .and(query_param("format", "json"))
            .respond_with(ResponseTemplate::new(200).set_body_string("<html>not json</html>"))
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Searxng;
        ctx.search_base_url = Some(server.uri());

        let err = WebSearchTool
            .run_searxng_search("html", 5, 5_000, &ctx)
            .await
            .expect_err("invalid JSON should be actionable");
        let msg = err.to_string();
        assert!(
            msg.contains("Failed to parse SearXNG JSON response")
                && msg.contains("format=json")
                && msg.contains("JSON output"),
            "got `{msg}`"
        );
    }

    #[tokio::test]
    async fn custom_duckduckgo_results_report_custom_host_source() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolSpec};
        use wiremock::matchers::{method, path, query_param};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/html/"))
            .and(query_param("q", "rust async"))
            .respond_with(ResponseTemplate::new(200).set_body_string(
                r#"
                <html><body>
                  <a class="result__a" href="https://example.com/rust">Rust async</a>
                  <div class="result__snippet">Async Rust result</div>
                </body></html>
                "#,
            ))
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::DuckDuckGo;
        let base_url = format!("{}/html/", server.uri());
        let expected_host = reqwest::Url::parse(&base_url)
            .expect("mock server url")
            .host_str()
            .expect("mock server host")
            .to_string();
        ctx.search_base_url = Some(base_url);

        let result = WebSearchTool
            .execute(json!({"query": "rust async"}), &ctx)
            .await
            .expect("custom endpoint should return results");
        let value: serde_json::Value =
            serde_json::from_str(&result.content).expect("web search json response");

        assert_eq!(value["source"].as_str(), Some(expected_host.as_str()));
        assert_eq!(value["count"].as_u64(), Some(1));
    }

    #[tokio::test]
    async fn repeated_search_uses_session_cache_and_marks_receipt() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolSpec};
        use crate::tools::web::cache;
        use wiremock::matchers::{method, path, query_param};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        cache::reset_search();
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/html/"))
            .and(query_param("q", "session cache receipt"))
            .respond_with(ResponseTemplate::new(200).set_body_string(
                r#"
                <html><body>
                  <a class="result__a" href="https://example.com/cached">Cached result</a>
                  <div class="result__snippet">Fetched once.</div>
                </body></html>
                "#,
            ))
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut context = ToolContext::new(tmp.path().to_path_buf())
            .with_state_namespace("web-search-query-cache");
        context.search_provider = SearchProvider::DuckDuckGo;
        context.search_base_url = Some(format!("{}/html/", server.uri()));

        let first = WebSearchTool
            .execute(json!({"query": "session cache receipt"}), &context)
            .await
            .expect("first search should succeed");
        let second = WebSearchTool
            .execute(json!({"query": "session cache receipt"}), &context)
            .await
            .expect("second search should hit cache");
        let first: serde_json::Value =
            serde_json::from_str(&first.content).expect("first response json");
        let second: serde_json::Value =
            serde_json::from_str(&second.content).expect("second response json");
        let requests = server.received_requests().await.expect("recorded requests");

        assert_eq!(requests.len(), 1);
        assert_eq!(first["receipt"]["cache_hit"], false);
        assert_eq!(second["receipt"]["cache_hit"], true);
        assert_eq!(second["receipt"]["latency_ms"], 0);
        assert_eq!(second["results"], first["results"]);

        use crate::network_policy::{Decision, NetworkPolicy, NetworkPolicyDecider};
        let denied_host = reqwest::Url::parse(&server.uri())
            .expect("mock server URL")
            .host_str()
            .expect("mock server host")
            .to_string();
        let policy = NetworkPolicy {
            default: Decision::Allow.into(),
            allow: Vec::new(),
            deny: vec![denied_host],
            proxy: Vec::new(),
            proxy_fake_ip_cidrs: Vec::new(),
            audit: false,
        };
        let blocked = context
            .clone()
            .with_network_policy(NetworkPolicyDecider::new(policy, None));
        let error = WebSearchTool
            .execute(json!({"query": "session cache receipt"}), &blocked)
            .await
            .expect_err("tightened policy must win over the query cache");
        assert!(error.to_string().contains("blocked by network policy"));
        assert_eq!(
            server
                .received_requests()
                .await
                .expect("recorded requests")
                .len(),
            1
        );
    }

    #[tokio::test]
    async fn explicit_bing_does_not_fall_back_to_duckduckgo() {
        use crate::config::SearchProvider;
        use crate::tools::spec::ToolContext;
        use wiremock::matchers::{method, path, query_param};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/bing"))
            .and(query_param("q", "one way fallback"))
            .respond_with(ResponseTemplate::new(200).set_body_string("<html></html>"))
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut context = ToolContext::new(tmp.path().to_path_buf());
        context.search_provider = SearchProvider::Bing;
        context.search_base_url = Some(format!("{}/must-not-be-used", server.uri()));
        let query = SearchQuery::new("one way fallback".to_string(), 5, None, Vec::new(), None);
        let raw = run_scrape_search_with_endpoints(
            SearchProvider::Bing,
            &query,
            5_000,
            &context,
            ScrapeEndpoints {
                bing: &format!("{}/bing", server.uri()),
                allow_bing_fallback: Some(true),
            },
        )
        .await
        .expect("empty Bing response is a successful empty search");
        let requests = server.received_requests().await.expect("recorded requests");

        assert_eq!(raw.backend, BackendId::Bing);
        assert!(raw.results.is_empty());
        assert!(raw.degraded.is_empty());
        assert_eq!(requests.len(), 1);
        assert_eq!(requests[0].url.path(), "/bing");
    }

    #[tokio::test]
    async fn custom_duckduckgo_challenge_returns_actionable_error() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolSpec};
        use wiremock::matchers::{method, path, query_param};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/html/"))
            .and(query_param("q", "rust async"))
            .respond_with(ResponseTemplate::new(200).set_body_string(
                r#"<html><body><div class="anomaly-modal">Unfortunately, bots use DuckDuckGo too</div></body></html>"#,
            ))
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::DuckDuckGo;
        ctx.search_base_url = Some(format!("{}/html/", server.uri()));

        let err = WebSearchTool
            .execute(json!({"query": "rust async"}), &ctx)
            .await
            .expect_err("custom endpoint challenge should error");
        let msg = err.to_string();
        assert!(
            msg.contains("DuckDuckGo-compatible search endpoint")
                && msg.contains("bot challenge")
                && msg.contains("private search service"),
            "got `{msg}`"
        );
    }

    #[tokio::test]
    async fn duckduckgo_challenge_to_bing_success_populates_fallback_receipt() {
        use crate::config::SearchProvider;
        use crate::tools::spec::ToolContext;
        use std::time::Instant;
        use wiremock::matchers::{method, path, query_param};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/html/"))
            .and(query_param("q", "fallback receipt"))
            .respond_with(ResponseTemplate::new(200).set_body_string(
                r#"<html><body><div class="anomaly-modal">Unfortunately, bots use DuckDuckGo too</div></body></html>"#,
            ))
            .mount(&server)
            .await;
        Mock::given(method("GET"))
            .and(path("/bing"))
            .and(query_param("q", "fallback receipt"))
            .respond_with(ResponseTemplate::new(200).set_body_string(
                r#"
                <ol><li class="b_algo">
                  <h2><a href="https://example.com/fallback">Fallback result</a></h2>
                  <div class="b_caption"><p>Bing result after challenge.</p></div>
                </li></ol>
                "#,
            ))
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut context = ToolContext::new(tmp.path().to_path_buf());
        context.search_provider = SearchProvider::DuckDuckGo;
        context.search_base_url = Some(format!("{}/html/", server.uri()));
        let query = SearchQuery::new("fallback receipt".to_string(), 5, None, Vec::new(), None);
        let started = Instant::now();
        let raw = run_scrape_search_with_endpoints(
            SearchProvider::DuckDuckGo,
            &query,
            5_000,
            &context,
            ScrapeEndpoints {
                bing: &format!("{}/bing", server.uri()),
                allow_bing_fallback: Some(true),
            },
        )
        .await
        .expect("Bing fallback should succeed");
        let response =
            finalize_search_response(query, QueryCapabilities::count_only(), raw, started);
        let value = serde_json::to_value(&response).expect("response serializes");

        assert_eq!(value["source"], "bing");
        assert_eq!(value["count"], 1);
        assert_eq!(value["receipt"]["backend"], "bing");
        assert_eq!(
            value["receipt"]["degraded"][0],
            json!({"kind": "challenge_detected", "backend": "duckduckgo"})
        );
        assert_eq!(
            value["receipt"]["degraded"][1],
            json!({"kind": "scrape_fallback", "from": "duckduckgo", "to": "bing"})
        );
        assert!(
            response
                .receipt
                .warning()
                .expect("warning")
                .contains("used bing fallback")
        );
    }

    #[tokio::test]
    async fn search_base_url_with_non_duckduckgo_provider_is_explicit_error() {
        use crate::config::SearchProvider;
        use crate::tools::spec::{ToolContext, ToolSpec};

        let tmp = tempfile::tempdir().expect("tempdir");
        let mut ctx = ToolContext::new(tmp.path().to_path_buf());
        ctx.search_provider = SearchProvider::Tavily;
        ctx.search_base_url = Some("https://search.internal.example/html/".to_string());

        let err = WebSearchTool
            .execute(json!({"query": "rust async"}), &ctx)
            .await
            .expect_err("non-duckduckgo provider with base_url should error");
        let msg = err.to_string();
        assert!(
            msg.contains("[search].base_url")
                && msg.contains("provider = \"duckduckgo\" or \"searxng\"")
                && msg.contains("tavily"),
            "got `{msg}`"
        );
    }

    #[test]
    fn rerank_assigns_sequential_ranks_starting_at_one() {
        // Simulates the post-dedup path: ranks may be non-contiguous after a
        // result is dropped; rerank must restore a clean 1..N sequence.
        let mut results = vec![
            SearchResult::new(
                5,
                "C".to_string(),
                "https://c.example.com/".to_string(),
                None,
                None,
            ),
            SearchResult::new(
                3,
                "A".to_string(),
                "https://a.example.com/".to_string(),
                None,
                None,
            ),
            SearchResult::new(
                1,
                "B".to_string(),
                "https://b.example.com/".to_string(),
                None,
                None,
            ),
        ];
        rerank(&mut results);
        assert_eq!(results[0].rank, 1);
        assert_eq!(results[1].rank, 2);
        assert_eq!(results[2].rank, 3);
    }

    #[test]
    fn rerank_on_empty_slice_is_a_no_op() {
        let mut results: Vec<SearchResult> = Vec::new();
        rerank(&mut results); // must not panic
    }

    #[test]
    fn register_search_citations_deduplicates_results_with_same_canonical_url() {
        let namespace = "dedup-test-session-fragments";
        let query = SearchQuery::new("deduplicate".to_string(), 5, None, Vec::new(), None);
        let raw = BackendSearch {
            backend: BackendId::DuckDuckGo,
            source: "duckduckgo".to_string(),
            backend_detail: None,
            results: vec![
                SearchResult::new(
                    1,
                    "First occurrence".to_string(),
                    "https://dedup.example.com/page#section-a".to_string(),
                    Some("first snippet".to_string()),
                    None,
                ),
                SearchResult::new(
                    2,
                    "Unique result".to_string(),
                    "https://other.dedup.example.com/different".to_string(),
                    None,
                    None,
                ),
                SearchResult::new(
                    3,
                    "Duplicate of first".to_string(),
                    "https://dedup.example.com/page#section-b".to_string(),
                    Some("duplicate snippet".to_string()),
                    None,
                ),
            ],
            degraded: Vec::new(),
            note: None,
        };
        let mut response =
            finalize_search_response(query, QueryCapabilities::count_only(), raw, Instant::now());
        let context = crate::tools::spec::ToolContext::new(std::path::PathBuf::from("."))
            .with_state_namespace(namespace);

        register_search_citations(&mut response, &context);

        assert_eq!(
            response.count, 2,
            "duplicate canonical URL must reduce the result count"
        );
        assert_eq!(response.results.len(), 2);
        assert_eq!(response.results[0].rank, 1);
        assert_eq!(response.results[1].rank, 2);
        assert_eq!(response.results[0].url, "https://dedup.example.com/page");
        assert_eq!(
            response.results[1].url,
            "https://other.dedup.example.com/different"
        );
        assert_ne!(
            response.results[0].ref_id, response.results[1].ref_id,
            "surviving results must have distinct ref_ids"
        );
        assert!(response.message.contains('2'), "{}", response.message);
    }

    #[test]
    fn register_search_citations_preserves_title_url_and_ref_id_metadata() {
        let namespace = "citation-metadata-test-session";
        let query = SearchQuery::new("docs".to_string(), 5, None, Vec::new(), None);
        let raw = BackendSearch {
            backend: BackendId::DuckDuckGo,
            source: "duckduckgo".to_string(),
            backend_detail: None,
            results: vec![SearchResult::new(
                1,
                "Official Docs".to_string(),
                "https://docs.citation-meta.example.com/reference".to_string(),
                Some("Comprehensive reference documentation.".to_string()),
                None,
            )],
            degraded: Vec::new(),
            note: None,
        };
        let mut response =
            finalize_search_response(query, QueryCapabilities::count_only(), raw, Instant::now());
        let context = crate::tools::spec::ToolContext::new(std::path::PathBuf::from("."))
            .with_state_namespace(namespace);

        register_search_citations(&mut response, &context);

        assert_eq!(response.count, 1);
        let result = &response.results[0];
        assert!(
            result.ref_id.starts_with("web_"),
            "ref_id must use web_ prefix; got `{}`",
            result.ref_id
        );
        assert_eq!(result.title, "Official Docs");
        assert_eq!(
            result.url,
            "https://docs.citation-meta.example.com/reference"
        );
        assert_eq!(result.rank, 1);
        let citation = crate::tools::web::citations::resolve(namespace, &result.ref_id)
            .expect("citation must be registered and resolvable in its session");
        assert_eq!(citation.ref_id, result.ref_id);
        assert_eq!(citation.url, result.url);
        assert_eq!(citation.title.as_deref(), Some("Official Docs"));
        assert!(
            !citation.retrieved_at.is_empty(),
            "retrieved_at must be set to the retrieval timestamp"
        );
        assert!(
            crate::tools::web::citations::resolve("other-session", &result.ref_id).is_none(),
            "citation must not leak to foreign sessions"
        );
    }

    #[test]
    fn finalize_search_response_truncates_to_max_results_and_reranks() {
        let query = SearchQuery::new("truncate me".to_string(), 2, None, Vec::new(), None);
        let raw = BackendSearch {
            backend: BackendId::DuckDuckGo,
            source: "duckduckgo".to_string(),
            backend_detail: None,
            results: vec![
                SearchResult::new(
                    1,
                    "A".to_string(),
                    "https://a.trunc.example.com/".to_string(),
                    None,
                    None,
                ),
                SearchResult::new(
                    2,
                    "B".to_string(),
                    "https://b.trunc.example.com/".to_string(),
                    None,
                    None,
                ),
                SearchResult::new(
                    3,
                    "C".to_string(),
                    "https://c.trunc.example.com/".to_string(),
                    None,
                    None,
                ),
            ],
            degraded: Vec::new(),
            note: None,
        };

        let response =
            finalize_search_response(query, QueryCapabilities::count_only(), raw, Instant::now());

        assert_eq!(response.count, 2, "must be truncated to max_results");
        assert_eq!(response.results.len(), 2);
        assert_eq!(response.results[0].rank, 1);
        assert_eq!(response.results[1].rank, 2);
        assert_eq!(response.results[0].title, "A");
        assert_eq!(response.results[1].title, "B");
        assert!(response.message.contains('2'), "{}", response.message);
    }

    #[test]
    fn finalize_search_response_does_not_claim_an_ignored_locale_was_honored() {
        // A scrape flagged the explicit locale as ignored (here: an unmapped
        // DuckDuckGo region): the receipt must keep `honored.locale` false
        // instead of claiming the knob was honored.
        let query = SearchQuery::new(
            "query".to_string(),
            5,
            None,
            Vec::new(),
            Some("fr-FR".to_string()),
        );
        let raw = BackendSearch {
            backend: BackendId::DuckDuckGo,
            source: "duckduckgo".to_string(),
            backend_detail: None,
            results: Vec::new(),
            degraded: vec![DegradedReason::KnobIgnored {
                knob: QueryKnob::Locale,
            }],
            note: None,
        };
        let capabilities = crate::tools::web::contract::QueryCapabilities {
            max_results: crate::tools::web::contract::CapabilityState::Supported,
            recency: crate::tools::web::contract::CapabilityState::Unsupported,
            domains: crate::tools::web::contract::CapabilityState::Unsupported,
            locale: crate::tools::web::contract::CapabilityState::Supported,
            published_date: crate::tools::web::contract::CapabilityState::Unknown,
        };
        let response = finalize_search_response(query, capabilities, raw, Instant::now());
        assert!(
            !response.receipt.honored.locale,
            "an ignored locale must not be reported as honored"
        );
        assert!(
            response.receipt.degraded.iter().any(|reason| matches!(
                reason,
                DegradedReason::KnobIgnored {
                    knob: QueryKnob::Locale
                }
            )),
            "the degraded receipt must carry the locale flag exactly once"
        );
        assert_eq!(response.receipt.degraded.len(), 1);

        // A mapped locale with no scrape flag stays honored.
        let query = SearchQuery::new(
            "query".to_string(),
            5,
            None,
            Vec::new(),
            Some("zh-CN".to_string()),
        );
        let raw = BackendSearch {
            backend: BackendId::DuckDuckGo,
            source: "duckduckgo".to_string(),
            backend_detail: None,
            results: Vec::new(),
            degraded: Vec::new(),
            note: None,
        };
        let response = finalize_search_response(query, capabilities, raw, Instant::now());
        assert!(response.receipt.honored.locale);
    }

    #[test]
    fn domain_matches_handles_subdomains_www_prefix_and_empty_list() {
        assert!(
            domain_matches("https://any.example.com/page", &[]),
            "empty domain list must accept all URLs"
        );
        assert!(domain_matches(
            "https://example.com/page",
            &["example.com".to_string()]
        ));
        assert!(domain_matches(
            "https://docs.example.com/page",
            &["example.com".to_string()]
        ));
        assert!(domain_matches(
            "https://www.example.com/page",
            &["example.com".to_string()]
        ));
        assert!(domain_matches(
            "https://example.com/page",
            &["www.example.com".to_string()]
        ));
        assert!(!domain_matches(
            "https://other.com/page",
            &["example.com".to_string()]
        ));
        assert!(!domain_matches(
            "https://notexample.com/page",
            &["example.com".to_string()]
        ));
    }

    #[test]
    fn finalize_search_response_domain_post_filter_reranks_survivors() {
        let query = SearchQuery::new(
            "domain filter".to_string(),
            5,
            None,
            vec!["keep.example.com".to_string()],
            None,
        );
        let raw = BackendSearch {
            backend: BackendId::DuckDuckGo,
            source: "duckduckgo".to_string(),
            backend_detail: None,
            results: vec![
                SearchResult::new(
                    1,
                    "Drop this".to_string(),
                    "https://other.example.com/page".to_string(),
                    None,
                    None,
                ),
                SearchResult::new(
                    2,
                    "Keep this".to_string(),
                    "https://keep.example.com/page".to_string(),
                    None,
                    None,
                ),
                SearchResult::new(
                    3,
                    "Also drop".to_string(),
                    "https://unrelated.example.com/page".to_string(),
                    None,
                    None,
                ),
            ],
            degraded: Vec::new(),
            note: None,
        };

        let response =
            finalize_search_response(query, QueryCapabilities::count_only(), raw, Instant::now());

        assert_eq!(response.count, 1, "only the matching domain must survive");
        assert_eq!(
            response.results[0].rank, 1,
            "survivor must be re-ranked to 1"
        );
        assert_eq!(response.results[0].title, "Keep this");
        assert!(
            response.receipt.degraded.iter().any(|reason| matches!(
                reason,
                DegradedReason::PostFiltered {
                    knob: QueryKnob::Domains
                }
            )),
            "post-filtered degraded reason must be present"
        );
    }

    #[test]
    fn fallback_receipt_carries_full_backend_chain_history() {
        // Verifies that the machine-readable degraded vec records every hop in
        // the fallback chain so callers can audit exactly what happened.
        let receipt = crate::tools::web::contract::SearchReceipt {
            backend: BackendId::Bing,
            backend_detail: None,
            requested: SearchQuery::new("fallback chain".to_string(), 5, None, Vec::new(), None),
            capabilities: QueryCapabilities::count_only(),
            honored: crate::tools::web::contract::HonoredQueryCapabilities {
                max_results: true,
                ..Default::default()
            },
            degraded: vec![
                DegradedReason::ChallengeDetected {
                    backend: BackendId::DuckDuckGo,
                },
                DegradedReason::ScrapeFallback {
                    from: BackendId::DuckDuckGo,
                    to: BackendId::Bing,
                },
            ],
            latency_ms: 42,
            cache_hit: false,
        };

        let value = serde_json::to_value(&receipt).expect("receipt must serialize");
        assert_eq!(value["backend"], "bing");
        assert_eq!(value["degraded"].as_array().unwrap().len(), 2);
        assert_eq!(value["degraded"][0]["kind"], "challenge_detected");
        assert_eq!(value["degraded"][0]["backend"], "duckduckgo");
        assert_eq!(value["degraded"][1]["kind"], "scrape_fallback");
        assert_eq!(value["degraded"][1]["from"], "duckduckgo");
        assert_eq!(value["degraded"][1]["to"], "bing");

        let warning = receipt
            .warning()
            .expect("degraded receipt must produce a warning");
        assert!(warning.contains("bot challenge"), "{warning}");
        assert!(warning.contains("used bing fallback"), "{warning}");
    }
}
