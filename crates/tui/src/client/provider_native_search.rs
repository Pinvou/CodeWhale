//! Narrow provider-native web-search client.
//!
//! This adapter reuses the active route's authenticated HTTP client without
//! exposing credentials to tool code. Route capability facts decide whether
//! the adapter is attached; this module speaks only documented first-party
//! wire contracts and leaves every unproven route on the configured fallback.

use anyhow::{Context, Result, bail};
use reqwest::header::{HeaderName, HeaderValue};
use serde_json::{Value, json};

use crate::config::ApiProvider;

use super::{DeepSeekClient, api_url, responses_api_url};

mod kimi;
mod mimo;
mod zai;

const MAX_NATIVE_ANSWER_CHARS: usize = 4_000;

#[derive(Clone)]
pub(crate) struct ProviderNativeSearchClient {
    pub(super) inner: DeepSeekClient,
}

#[derive(Clone)]
pub(crate) struct ProviderNativeSearchRequest {
    pub(crate) query: String,
    pub(crate) max_results: u8,
    pub(crate) domains: Vec<String>,
}

#[derive(Clone, PartialEq, Eq)]
pub(crate) struct ProviderNativeCitation {
    pub(crate) url: String,
    pub(crate) title: String,
    pub(crate) snippet: Option<String>,
    pub(crate) published: Option<String>,
}

#[derive(Clone, PartialEq, Eq)]
pub(crate) struct ProviderNativeSearchResponse {
    pub(crate) answer: Option<String>,
    pub(crate) citations: Vec<ProviderNativeCitation>,
}

impl ProviderNativeSearchClient {
    #[must_use]
    pub(crate) fn new(inner: DeepSeekClient) -> Option<Self> {
        matches!(
            inner.api_provider,
            ApiProvider::Openai
                | ApiProvider::Anthropic
                | ApiProvider::Xai
                | ApiProvider::Deepseek
                | ApiProvider::DeepseekCN
                | ApiProvider::Moonshot
                | ApiProvider::Zai
                | ApiProvider::XiaomiMimo
                | ApiProvider::ModelstudioTokenPlan
        )
        .then_some(Self { inner })
    }

    #[must_use]
    pub(crate) fn provider(&self) -> ApiProvider {
        self.inner.api_provider
    }

    #[must_use]
    pub(crate) fn model(&self) -> &str {
        &self.inner.default_model
    }

    #[must_use]
    pub(crate) fn host(&self) -> Option<String> {
        reqwest::Url::parse(&self.inner.base_url)
            .ok()
            .and_then(|url| url.host_str().map(str::to_ascii_lowercase))
    }

    #[must_use]
    pub(crate) fn cache_identity(&self) -> String {
        format!(
            "provider-native://{}/{}/{}",
            self.inner.api_provider.as_str(),
            self.host().as_deref().unwrap_or("unknown-host"),
            self.inner.default_model
        )
    }

    #[must_use]
    pub(crate) const fn maximum_domain_count(&self) -> Option<usize> {
        match self.inner.api_provider {
            ApiProvider::Xai => Some(5),
            ApiProvider::Openai => Some(100),
            _ => None,
        }
    }

    #[must_use]
    pub(crate) const fn supports_domain_filter(&self) -> bool {
        matches!(
            self.inner.api_provider,
            ApiProvider::Openai | ApiProvider::Anthropic | ApiProvider::Xai
        )
    }

    pub(crate) async fn search(
        &self,
        request: &ProviderNativeSearchRequest,
    ) -> Result<ProviderNativeSearchResponse> {
        let mut parsed = match self.inner.api_provider {
            ApiProvider::Openai
            | ApiProvider::Xai
            | ApiProvider::Deepseek
            | ApiProvider::DeepseekCN
            | ApiProvider::ModelstudioTokenPlan => {
                let dialect = match self.inner.api_provider {
                    ApiProvider::Openai => ResponsesSearchDialect::Openai,
                    ApiProvider::Xai => ResponsesSearchDialect::Xai,
                    ApiProvider::Deepseek | ApiProvider::DeepseekCN => {
                        ResponsesSearchDialect::Deepseek
                    }
                    ApiProvider::ModelstudioTokenPlan => ResponsesSearchDialect::ModelStudio,
                    _ => unreachable!("provider grouped above"),
                };
                let body = build_responses_search_body(&self.inner.default_model, request, dialect);
                let url = match self.inner.api_provider {
                    ApiProvider::Deepseek | ApiProvider::DeepseekCN => {
                        responses_api_url(&self.inner.base_url, self.inner.api_provider)
                    }
                    _ => api_url(&self.inner.base_url, "responses"),
                };
                let payload = self.post_json(&url, &body, &[]).await?;
                parse_responses_search(&payload)
            }
            ApiProvider::Anthropic => {
                let body = build_anthropic_search_body(&self.inner.default_model, request);
                let payload = self
                    .post_json(&anthropic_messages_url(&self.inner.base_url), &body, &[])
                    .await?;
                parse_anthropic_search(&payload)
            }
            ApiProvider::Moonshot => kimi::search(self, request).await?,
            ApiProvider::Zai => zai::search(self, request).await?,
            ApiProvider::XiaomiMimo => mimo::search(self, request).await?,
            _ => bail!("active provider has no native web-search adapter"),
        };
        parsed.citations.truncate(usize::from(request.max_results));
        Ok(parsed)
    }

    pub(super) async fn post_json(
        &self,
        url: &str,
        body: &Value,
        headers: &[(HeaderName, HeaderValue)],
    ) -> Result<Value> {
        let body_bytes = serde_json::to_vec(body)
            .context("failed to serialize provider-native web-search request")?;
        let headers = headers.to_vec();
        let response = self
            .inner
            .send_with_retry(|| {
                let mut request = self
                    .inner
                    .http_client
                    .post(url)
                    .header("Accept", "application/json")
                    .body(body_bytes.clone());
                for (name, value) in &headers {
                    request = request.header(name, value);
                }
                request
            })
            .await
            .context("provider-native web search request failed")?;
        response
            .json::<Value>()
            .await
            .context("provider-native web search returned invalid JSON")
    }
}

#[derive(Clone, Copy)]
enum ResponsesSearchDialect {
    Openai,
    Xai,
    Deepseek,
    ModelStudio,
}

fn search_prompt(request: &ProviderNativeSearchRequest) -> String {
    let mut prompt = format!(
        "Search the web for the following query and answer only from web sources. \
         Use concise prose with citations and prefer at most {} distinct sources.\n\n{}",
        request.max_results, request.query
    );
    if !request.domains.is_empty() {
        prompt.push_str("\n\nOnly use sources from these domains: ");
        prompt.push_str(&request.domains.join(", "));
    }
    prompt
}

fn build_responses_search_body(
    model: &str,
    request: &ProviderNativeSearchRequest,
    dialect: ResponsesSearchDialect,
) -> Value {
    let mut tool = json!({ "type": "web_search" });
    if !request.domains.is_empty()
        && matches!(
            dialect,
            ResponsesSearchDialect::Openai | ResponsesSearchDialect::Xai
        )
    {
        tool["filters"] = json!({ "allowed_domains": request.domains });
    }
    let mut body = json!({
        "model": model,
        "input": search_prompt(request),
        "tools": [tool],
    });
    match dialect {
        ResponsesSearchDialect::Openai => {
            body["tool_choice"] = json!("required");
            body["store"] = json!(false);
            body["include"] = json!(["web_search_call.action.sources"]);
        }
        ResponsesSearchDialect::Xai => {
            body["tool_choice"] = json!("required");
        }
        ResponsesSearchDialect::Deepseek | ResponsesSearchDialect::ModelStudio => {
            body["tool_choice"] = json!({ "type": "web_search" });
        }
    }
    body
}

fn build_anthropic_search_body(model: &str, request: &ProviderNativeSearchRequest) -> Value {
    let mut tool = json!({
        "type": "web_search_20250305",
        "name": "web_search",
        "max_uses": 1,
    });
    if !request.domains.is_empty() {
        tool["allowed_domains"] = json!(request.domains);
    }
    json!({
        "model": model,
        "max_tokens": 2048,
        "messages": [{ "role": "user", "content": search_prompt(request) }],
        "tools": [tool],
    })
}

fn anthropic_messages_url(base_url: &str) -> String {
    let base = base_url.trim_end_matches('/');
    if base.ends_with("/v1") {
        format!("{base}/messages")
    } else {
        format!("{base}/v1/messages")
    }
}

fn parse_responses_search(payload: &Value) -> ProviderNativeSearchResponse {
    let mut answer_parts = Vec::new();
    let mut citations = Vec::new();
    if let Some(output) = payload.get("output").and_then(Value::as_array) {
        for item in output {
            let item_type = item.get("type").and_then(Value::as_str);
            if item_type == Some("web_search_call")
                && let Some(action) = item.get("action")
            {
                // OpenAI/xAI expose a collected source list, while DeepSeek
                // records each opened page directly as `action.url`.
                if let Some(sources) = action.get("sources").and_then(Value::as_array) {
                    for source in sources {
                        push_citation(&mut citations, citation_from_value(source, None, None));
                    }
                }
                push_citation(&mut citations, citation_from_value(action, None, None));
            }

            // Responses payloads may carry provider reasoning items beside
            // the final message. Only user-visible message content belongs in
            // a search answer; forwarding reasoning would leak hidden chain
            // of thought and can also introduce ungrounded intermediate URLs.
            if item_type == Some("message")
                && let Some(content) = item.get("content").and_then(Value::as_array)
            {
                for block in content {
                    if matches!(
                        block.get("type").and_then(Value::as_str),
                        Some("output_text" | "text")
                    ) && let Some(text) = block.get("text").and_then(Value::as_str)
                        && !text.trim().is_empty()
                    {
                        answer_parts.push(text.trim().to_string());
                    }
                    if let Some(annotations) = block.get("annotations").and_then(Value::as_array) {
                        for annotation in annotations {
                            push_citation(
                                &mut citations,
                                citation_from_value(annotation, None, None),
                            );
                        }
                    }
                }
            }
        }
    }
    if answer_parts.is_empty()
        && let Some(output_text) = payload.get("output_text").and_then(Value::as_str)
        && !output_text.trim().is_empty()
    {
        answer_parts.push(output_text.trim().to_string());
    }
    for answer in &answer_parts {
        for citation in citations_from_text(answer) {
            push_citation(&mut citations, Some(citation));
        }
    }
    if let Some(top_level) = payload.get("citations").and_then(Value::as_array) {
        for citation in top_level {
            let parsed = citation
                .as_str()
                .and_then(|url| citation_from_url(url, None, None, None))
                .or_else(|| citation_from_value(citation, None, None));
            push_citation(&mut citations, parsed);
        }
    }
    ProviderNativeSearchResponse {
        answer: bounded_answer(answer_parts),
        citations,
    }
}

fn parse_anthropic_search(payload: &Value) -> ProviderNativeSearchResponse {
    let mut answer_parts = Vec::new();
    let mut citations = Vec::new();
    if let Some(content) = payload.get("content").and_then(Value::as_array) {
        for block in content {
            match block.get("type").and_then(Value::as_str) {
                Some("web_search_tool_result") => {
                    if let Some(results) = block.get("content").and_then(Value::as_array) {
                        for result in results {
                            let published = result
                                .get("page_age")
                                .and_then(Value::as_str)
                                .map(str::to_string);
                            push_citation(
                                &mut citations,
                                citation_from_value(result, None, published),
                            );
                        }
                    }
                }
                Some("text") => {
                    if let Some(text) = block.get("text").and_then(Value::as_str)
                        && !text.trim().is_empty()
                    {
                        answer_parts.push(text.trim().to_string());
                    }
                    if let Some(block_citations) = block.get("citations").and_then(Value::as_array)
                    {
                        for citation in block_citations {
                            let snippet = citation
                                .get("cited_text")
                                .and_then(Value::as_str)
                                .map(str::to_string);
                            push_citation(
                                &mut citations,
                                citation_from_value(citation, snippet, None),
                            );
                        }
                    }
                }
                _ => {}
            }
        }
    }
    ProviderNativeSearchResponse {
        answer: bounded_answer(answer_parts),
        citations,
    }
}

fn citation_from_value(
    value: &Value,
    snippet: Option<String>,
    published: Option<String>,
) -> Option<ProviderNativeCitation> {
    let url = value.get("url").and_then(Value::as_str)?.trim();
    let title = value
        .get("title")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|title| !title.is_empty())
        .map(str::to_string);
    citation_from_url(url, title, snippet, published)
}

fn citation_from_url(
    url: &str,
    title: Option<String>,
    snippet: Option<String>,
    published: Option<String>,
) -> Option<ProviderNativeCitation> {
    let parsed = reqwest::Url::parse(url).ok()?;
    if !matches!(parsed.scheme(), "http" | "https") || parsed.host_str().is_none() {
        return None;
    }
    Some(ProviderNativeCitation {
        url: url.to_string(),
        title: title.unwrap_or_else(|| fallback_title(url)),
        snippet,
        published,
    })
}

fn push_citation(
    citations: &mut Vec<ProviderNativeCitation>,
    candidate: Option<ProviderNativeCitation>,
) {
    let Some(candidate) = candidate else {
        return;
    };
    if let Some(existing) = citations
        .iter_mut()
        .find(|existing| existing.url == candidate.url)
    {
        if existing.title == fallback_title(&existing.url)
            && candidate.title != fallback_title(&candidate.url)
        {
            existing.title = candidate.title;
        }
        if existing.snippet.is_none() {
            existing.snippet = candidate.snippet;
        }
        if existing.published.is_none() {
            existing.published = candidate.published;
        }
        return;
    }
    citations.push(candidate);
}

fn citations_from_text(text: &str) -> Vec<ProviderNativeCitation> {
    let mut citations = Vec::new();
    let mut offset = 0;
    while offset < text.len() {
        let remaining = &text[offset..];
        let Some(relative_start) = remaining
            .find("https://")
            .or_else(|| remaining.find("http://"))
        else {
            break;
        };
        let start = offset + relative_start;
        let tail = &text[start..];
        let end = tail
            .char_indices()
            .find_map(|(index, ch)| {
                (index > 0
                    && (ch.is_whitespace()
                        || matches!(ch, ')' | ']' | '}' | '>' | '"' | '\'' | '`')))
                .then_some(index)
            })
            .unwrap_or(tail.len());
        let url = tail[..end].trim_end_matches(['.', ',', ';', ':', '!', '?']);
        push_citation(&mut citations, citation_from_url(url, None, None, None));
        offset = start + end.max(1);
    }
    citations
}

fn fallback_title(url: &str) -> String {
    reqwest::Url::parse(url)
        .ok()
        .and_then(|parsed| parsed.host_str().map(str::to_string))
        .unwrap_or_else(|| "Web source".to_string())
}

fn bounded_answer(parts: Vec<String>) -> Option<String> {
    let joined = parts.join("\n\n");
    let trimmed = joined.trim();
    if trimmed.is_empty() {
        return None;
    }
    if trimmed.chars().count() <= MAX_NATIVE_ANSWER_CHARS {
        return Some(trimmed.to_string());
    }
    let mut bounded = trimmed
        .chars()
        .take(MAX_NATIVE_ANSWER_CHARS.saturating_sub(1))
        .collect::<String>();
    bounded.push('…');
    Some(bounded)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::{Config, ProviderConfig, ProvidersConfig};
    use wiremock::matchers::{body_partial_json, header, method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    fn request() -> ProviderNativeSearchRequest {
        ProviderNativeSearchRequest {
            query: "current release".to_string(),
            max_results: 3,
            domains: vec!["example.com".to_string()],
        }
    }

    #[test]
    fn responses_payload_requires_search_and_keeps_domains_provider_side() {
        let body =
            build_responses_search_body("gpt-5.6", &request(), ResponsesSearchDialect::Openai);
        assert_eq!(body["tools"][0]["type"], "web_search");
        assert_eq!(
            body["tools"][0]["filters"]["allowed_domains"][0],
            "example.com"
        );
        assert_eq!(body["tool_choice"], "required");
        assert_eq!(body["include"][0], "web_search_call.action.sources");
    }

    #[test]
    fn deepseek_responses_payload_requires_search_without_unsupported_fields() {
        let body = build_responses_search_body(
            "deepseek-v4-flash",
            &request(),
            ResponsesSearchDialect::Deepseek,
        );
        assert_eq!(body["tools"][0]["type"], "web_search");
        assert!(body["tools"][0].get("filters").is_none());
        assert_eq!(body["tool_choice"]["type"], "web_search");
        assert!(body.get("include").is_none());
        assert!(body.get("store").is_none());
    }

    #[test]
    fn model_studio_payload_uses_minimal_responses_search_contract() {
        let body = build_responses_search_body(
            "qwen3.8-max",
            &request(),
            ResponsesSearchDialect::ModelStudio,
        );
        assert_eq!(body["tools"][0]["type"], "web_search");
        assert!(body["tools"][0].get("filters").is_none());
        assert_eq!(body["tool_choice"]["type"], "web_search");
        assert!(body.get("include").is_none());
    }

    #[test]
    fn anthropic_payload_uses_basic_direct_search_contract() {
        let body = build_anthropic_search_body("claude-opus-4-8", &request());
        assert_eq!(body["tools"][0]["type"], "web_search_20250305");
        assert_eq!(body["tools"][0]["max_uses"], 1);
        assert_eq!(body["tools"][0]["allowed_domains"][0], "example.com");
    }

    #[test]
    fn responses_parser_separates_answer_and_deduplicated_citations() {
        let payload = json!({
            "output": [
                {
                    "type": "web_search_call",
                    "action": { "sources": [
                        { "url": "https://example.com/a", "title": "Source A" }
                    ] }
                },
                {
                    "type": "message",
                    "content": [{
                        "type": "output_text",
                        "text": "Grounded answer.",
                        "annotations": [
                            { "type": "url_citation", "url": "https://example.com/a", "title": "Source A" },
                            { "type": "url_citation", "url": "https://example.org/b", "title": "Source B" }
                        ]
                    }]
                }
            ]
        });
        let parsed = parse_responses_search(&payload);
        assert_eq!(parsed.answer.as_deref(), Some("Grounded answer."));
        assert_eq!(parsed.citations.len(), 2);
        assert_eq!(parsed.citations[0].title, "Source A");
        assert_eq!(parsed.citations[1].url, "https://example.org/b");
    }

    #[test]
    fn deepseek_responses_parser_keeps_final_message_and_opened_pages_only() {
        let payload = json!({
            "output": [
                {
                    "type": "reasoning",
                    "content": [{
                        "type": "reasoning_text",
                        "text": "private analysis https://reasoning.example/ must stay hidden"
                    }]
                },
                {
                    "type": "web_search_call",
                    "action": { "type": "search", "queries": ["CodeWhale"] }
                },
                {
                    "type": "web_search_call",
                    "action": {
                        "type": "open_page",
                        "url": "https://github.com/Hmbown/CodeWhale"
                    }
                },
                {
                    "type": "message",
                    "content": [{
                        "type": "output_text",
                        "text": "Official repository: https://github.com/Hmbown/CodeWhale",
                        "annotations": []
                    }]
                }
            ]
        });

        let parsed = parse_responses_search(&payload);

        assert_eq!(
            parsed.answer.as_deref(),
            Some("Official repository: https://github.com/Hmbown/CodeWhale")
        );
        assert_eq!(parsed.citations.len(), 1);
        assert_eq!(
            parsed.citations[0].url,
            "https://github.com/Hmbown/CodeWhale"
        );
        assert!(
            parsed
                .citations
                .iter()
                .all(|citation| !citation.url.contains("reasoning.example"))
        );
    }

    #[test]
    fn anthropic_parser_keeps_result_metadata_and_cited_text_separate() {
        let payload = json!({
            "content": [
                {
                    "type": "web_search_tool_result",
                    "content": [{
                        "type": "web_search_result",
                        "url": "https://example.com/a",
                        "title": "Source A",
                        "page_age": "July 18, 2026"
                    }]
                },
                {
                    "type": "text",
                    "text": "Grounded answer.",
                    "citations": [{
                        "type": "web_search_result_location",
                        "url": "https://example.com/a",
                        "title": "Source A",
                        "cited_text": "Supporting passage"
                    }]
                }
            ]
        });
        let parsed = parse_anthropic_search(&payload);
        assert_eq!(parsed.answer.as_deref(), Some("Grounded answer."));
        assert_eq!(parsed.citations.len(), 1);
        assert_eq!(
            parsed.citations[0].published.as_deref(),
            Some("July 18, 2026")
        );
        assert_eq!(
            parsed.citations[0].snippet.as_deref(),
            Some("Supporting passage")
        );
    }

    #[test]
    fn non_http_citations_are_rejected() {
        let payload = json!({ "citations": ["javascript:alert(1)"] });
        assert!(parse_responses_search(&payload).citations.is_empty());
    }

    #[test]
    fn answer_links_are_recovered_without_accepting_trailing_punctuation() {
        let citations = citations_from_text(
            "Read [Example](https://example.com/a), then https://example.org/b.",
        );
        assert_eq!(citations.len(), 2);
        assert_eq!(citations[0].url, "https://example.com/a");
        assert_eq!(citations[1].url, "https://example.org/b");
    }

    #[tokio::test]
    async fn xai_adapter_reuses_active_authenticated_transport() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/responses"))
            .and(header("authorization", "Bearer xai-test-key"))
            .and(body_partial_json(json!({
                "model": "grok-4.5",
                "tools": [{
                    "type": "web_search",
                    "filters": { "allowed_domains": ["example.com"] }
                }],
                "tool_choice": "required"
            })))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "output_text": "Grounded answer.",
                "citations": ["https://example.com/source"]
            })))
            .expect(1)
            .mount(&server)
            .await;
        let config = Config {
            provider: Some("xai".to_string()),
            providers: Some(ProvidersConfig {
                xai: ProviderConfig {
                    api_key: Some("xai-test-key".to_string()),
                    base_url: Some(format!("{}/v1", server.uri())),
                    model: Some("grok-4.5".to_string()),
                    ..ProviderConfig::default()
                },
                ..ProvidersConfig::default()
            }),
            ..Config::default()
        };
        let inner = DeepSeekClient::new(&config).expect("test xAI client");
        let client = ProviderNativeSearchClient::new(inner).expect("xAI native adapter");
        let cache_identity = client.cache_identity();
        assert!(cache_identity.contains("provider-native://xai/"));
        assert!(cache_identity.ends_with("/grok-4.5"));
        assert!(!cache_identity.contains("xai-test-key"));

        let response = client.search(&request()).await.expect("native search");

        assert_eq!(response.answer.as_deref(), Some("Grounded answer."));
        assert_eq!(response.citations.len(), 1);
        assert_eq!(response.citations[0].url, "https://example.com/source");
    }
    #[tokio::test]
    async fn model_studio_adapter_uses_token_plan_responses_contract() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/responses"))
            .and(header("authorization", "Bearer modelstudio-test-key"))
            .and(body_partial_json(json!({
                "model": "qwen3.8-max",
                "tools": [{ "type": "web_search" }]
            })))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "output": [{
                    "type": "web_search_call",
                    "action": {
                        "sources": [{
                            "url": "https://example.com/qwen",
                            "title": "Qwen source"
                        }]
                    }
                }]
            })))
            .expect(1)
            .mount(&server)
            .await;
        let config = Config {
            provider: Some("modelstudio-token-plan".to_string()),
            providers: Some(ProvidersConfig {
                modelstudio_token_plan: ProviderConfig {
                    api_key: Some("modelstudio-test-key".to_string()),
                    base_url: Some(format!("{}/v1", server.uri())),
                    model: Some("qwen3.8-max".to_string()),
                    ..ProviderConfig::default()
                },
                ..ProvidersConfig::default()
            }),
            ..Config::default()
        };
        let inner = DeepSeekClient::new(&config).expect("test Model Studio client");
        let client = ProviderNativeSearchClient::new(inner).expect("Qwen native adapter");

        let response = client.search(&request()).await.expect("native search");

        assert_eq!(response.citations.len(), 1);
        assert_eq!(response.citations[0].url, "https://example.com/qwen");
    }

    #[tokio::test]
    async fn moonshot_adapter_declares_builtin_search_and_recovers_citations() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/chat/completions"))
            .and(header("authorization", "Bearer moonshot-test-key"))
            .and(body_partial_json(json!({
                "model": "kimi-k3",
                "tools": [{
                    "type": "builtin_function",
                    "function": { "name": "$web_search" }
                }]
            })))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "choices": [{
                    "finish_reason": "stop",
                    "message": {
                        "role": "assistant",
                        "content": "See https://example.com/kimi for the current result."
                    }
                }]
            })))
            .expect(1)
            .mount(&server)
            .await;
        let config = Config {
            provider: Some("moonshot".to_string()),
            providers: Some(ProvidersConfig {
                moonshot: ProviderConfig {
                    api_key: Some("moonshot-test-key".to_string()),
                    base_url: Some(format!("{}/v1", server.uri())),
                    model: Some("kimi-k3".to_string()),
                    ..ProviderConfig::default()
                },
                ..ProvidersConfig::default()
            }),
            ..Config::default()
        };
        let inner = DeepSeekClient::new(&config).expect("test Moonshot client");
        let client = ProviderNativeSearchClient::new(inner).expect("Kimi native adapter");

        let response = client.search(&request()).await.expect("native search");

        assert_eq!(response.citations.len(), 1);
        assert_eq!(response.citations[0].url, "https://example.com/kimi");
    }
}
