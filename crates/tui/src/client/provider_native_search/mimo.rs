//! Xiaomi MiMo Chat Completions web-search plugin.

use anyhow::Result;
use serde_json::{Value, json};

use super::{
    ProviderNativeSearchClient, ProviderNativeSearchRequest, ProviderNativeSearchResponse,
    bounded_answer, citation_from_url, citations_from_text, push_citation,
};
use crate::client::api_url;

pub(super) async fn search(
    client: &ProviderNativeSearchClient,
    request: &ProviderNativeSearchRequest,
) -> Result<ProviderNativeSearchResponse> {
    let body = build_body(&client.inner.default_model, request);
    let payload = client
        .post_json(
            &api_url(&client.inner.base_url, "chat/completions"),
            &body,
            &[],
        )
        .await?;
    Ok(parse(&payload))
}

fn build_body(model: &str, request: &ProviderNativeSearchRequest) -> Value {
    json!({
        "model": model,
        "messages": [{ "role": "user", "content": super::search_prompt(request) }],
        "tools": [{
            "type": "web_search",
            "max_keyword": 1,
            "force_search": true,
            "limit": request.max_results,
        }],
        "tool_choice": "auto",
        "max_completion_tokens": 2_048,
        "stream": false,
        "thinking": { "type": "disabled" },
    })
}

fn parse(payload: &Value) -> ProviderNativeSearchResponse {
    let message = payload.pointer("/choices/0/message");
    let answer = message
        .and_then(|value| value.get("content"))
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|text| !text.is_empty())
        .map(str::to_string);
    let mut citations = Vec::new();
    if let Some(annotations) = message
        .and_then(|value| value.get("annotations"))
        .and_then(Value::as_array)
        .or_else(|| payload.get("annotations").and_then(Value::as_array))
    {
        for annotation in annotations {
            let Some(url) = annotation.get("url").and_then(Value::as_str) else {
                continue;
            };
            let title = annotation
                .get("title")
                .and_then(Value::as_str)
                .map(str::to_string);
            let snippet = annotation
                .get("summary")
                .and_then(Value::as_str)
                .map(str::to_string);
            let published = annotation
                .get("publish_time")
                .and_then(Value::as_str)
                .map(str::to_string);
            push_citation(
                &mut citations,
                citation_from_url(url, title, snippet, published),
            );
        }
    }
    if citations.is_empty()
        && let Some(text) = answer.as_deref()
    {
        citations = citations_from_text(text);
    }
    ProviderNativeSearchResponse {
        answer: bounded_answer(answer.into_iter().collect()),
        citations,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn request_forces_mimo_web_search_plugin() {
        let request = ProviderNativeSearchRequest {
            query: "current release".to_string(),
            max_results: 3,
            domains: vec![],
        };
        let body = build_body("mimo-v2.5-pro", &request);
        assert_eq!(body["tools"][0]["type"], "web_search");
        assert_eq!(body["tools"][0]["force_search"], true);
        assert_eq!(body["tools"][0]["limit"], 3);
        assert_eq!(body["thinking"]["type"], "disabled");
    }

    #[test]
    fn parses_non_streaming_annotations() {
        let parsed = parse(&json!({
            "choices": [{
                "message": {
                    "content": "Grounded answer.",
                    "annotations": [{
                        "type": "url_citation",
                        "url": "https://example.com/weather",
                        "title": "Weather",
                        "summary": "Forecast",
                        "publish_time": "2026-08-28"
                    }]
                }
            }]
        }));
        assert_eq!(parsed.answer.as_deref(), Some("Grounded answer."));
        assert_eq!(parsed.citations.len(), 1);
        assert_eq!(parsed.citations[0].snippet.as_deref(), Some("Forecast"));
    }
}
