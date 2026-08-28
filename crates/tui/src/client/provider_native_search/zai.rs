//! Z.AI structured Web Search API adapter.

use anyhow::Result;
use serde_json::{Value, json};

use super::{
    ProviderNativeSearchClient, ProviderNativeSearchRequest, ProviderNativeSearchResponse,
    citation_from_url, push_citation,
};
use crate::client::api_url;

pub(super) async fn search(
    client: &ProviderNativeSearchClient,
    request: &ProviderNativeSearchRequest,
) -> Result<ProviderNativeSearchResponse> {
    let body = build_body(request);
    let payload = client
        .post_json(&api_url(&client.inner.base_url, "web_search"), &body, &[])
        .await?;
    Ok(parse(&payload))
}

fn build_body(request: &ProviderNativeSearchRequest) -> Value {
    json!({
        "search_engine": "search-prime",
        "search_query": request.query,
        "count": request.max_results,
    })
}

fn parse(payload: &Value) -> ProviderNativeSearchResponse {
    let mut citations = Vec::new();
    if let Some(results) = payload.get("search_result").and_then(Value::as_array) {
        for result in results {
            let Some(url) = result.get("link").and_then(Value::as_str) else {
                continue;
            };
            let title = result
                .get("title")
                .and_then(Value::as_str)
                .map(str::to_string);
            let snippet = result
                .get("content")
                .and_then(Value::as_str)
                .map(str::to_string);
            let published = result
                .get("publish_date")
                .and_then(Value::as_str)
                .map(str::to_string);
            push_citation(
                &mut citations,
                citation_from_url(url, title, snippet, published),
            );
        }
    }
    ProviderNativeSearchResponse {
        answer: None,
        citations,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn request_uses_search_prime_without_unproven_filter_fields() {
        let request = ProviderNativeSearchRequest {
            query: "current release".to_string(),
            max_results: 3,
            domains: vec!["example.com".to_string()],
        };
        let body = build_body(&request);
        assert_eq!(body["search_engine"], "search-prime");
        assert_eq!(body["search_query"], "current release");
        assert_eq!(body["count"], 3);
        assert!(body.get("search_domain_filter").is_none());
    }

    #[test]
    fn parses_structured_search_results() {
        let parsed = parse(&json!({
            "search_result": [{
                "title": "Release",
                "content": "Release notes",
                "link": "https://example.com/release",
                "publish_date": "2026-08-28"
            }]
        }));
        assert_eq!(parsed.citations.len(), 1);
        assert_eq!(parsed.citations[0].title, "Release");
        assert_eq!(parsed.citations[0].published.as_deref(), Some("2026-08-28"));
    }
}
