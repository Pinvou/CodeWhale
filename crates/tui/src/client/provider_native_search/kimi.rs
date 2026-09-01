//! Moonshot/Kimi native search adapters.

use anyhow::{Context, Result, bail};
use reqwest::header::{HeaderName, HeaderValue};
use serde_json::{Map, Value, json};
use uuid::Uuid;

use super::{
    ProviderNativeSearchClient, ProviderNativeSearchRequest, ProviderNativeSearchResponse,
    bounded_answer, citation_from_url, citations_from_text, push_citation,
};
use crate::{
    client::api_url,
    config::{DEFAULT_KIMI_CODE_BASE_URL, MOONSHOT_KIMI_K3_MODEL},
};

const MAX_NATIVE_SEARCH_ROUNDS: usize = 4;
const MAX_NATIVE_SEARCH_TOOL_CALLS: usize = 8;
const WEB_SEARCH_FORMULA_URI: &str = "moonshot/web-search:latest";
const WEB_SEARCH_FORMULA_FUNCTION: &str = "web_search";

pub(super) async fn search(
    client: &ProviderNativeSearchClient,
    request: &ProviderNativeSearchRequest,
) -> Result<ProviderNativeSearchResponse> {
    if is_kimi_code_route(&client.inner.base_url) {
        search_kimi_code(client, request).await
    } else if client
        .inner
        .default_model
        .trim()
        .eq_ignore_ascii_case(MOONSHOT_KIMI_K3_MODEL)
    {
        search_formula(client, request).await
    } else {
        search_builtin(client, request).await
    }
}

fn is_kimi_code_route(base_url: &str) -> bool {
    base_url
        .trim()
        .trim_end_matches('/')
        .eq_ignore_ascii_case(DEFAULT_KIMI_CODE_BASE_URL)
}

async fn search_kimi_code(
    client: &ProviderNativeSearchClient,
    request: &ProviderNativeSearchRequest,
) -> Result<ProviderNativeSearchResponse> {
    let body = build_kimi_code_body(request);
    let call_id = HeaderValue::from_str(&Uuid::new_v4().to_string())
        .context("failed to build Kimi search call id")?;
    let url = format!("{}/search", client.inner.base_url.trim_end_matches('/'));
    let payload = client
        .post_json(
            &url,
            &body,
            &[(HeaderName::from_static("x-msh-tool-call-id"), call_id)],
        )
        .await?;
    Ok(parse_kimi_code(&payload))
}

fn build_kimi_code_body(request: &ProviderNativeSearchRequest) -> Value {
    json!({
        "text_query": request.query,
    })
}

async fn search_builtin(
    client: &ProviderNativeSearchClient,
    request: &ProviderNativeSearchRequest,
) -> Result<ProviderNativeSearchResponse> {
    let tools = builtin_search_tools();
    let mut messages = vec![json!({
        "role": "user",
        "content": super::search_prompt(request),
    })];
    let mut tool_calls_executed = 0;
    let url = api_url(&client.inner.base_url, "chat/completions");

    for _ in 0..MAX_NATIVE_SEARCH_ROUNDS {
        let body = build_builtin_chat_body(&client.inner.default_model, &messages, &tools);
        let payload = client.post_json(&url, &body, &[]).await?;
        let choice = payload
            .pointer("/choices/0")
            .context("Kimi web search response omitted choices[0]")?;
        let message = choice
            .get("message")
            .and_then(Value::as_object)
            .context("Kimi web search response omitted assistant message")?;
        let finish_reason = choice.get("finish_reason").and_then(Value::as_str);
        if finish_reason != Some("tool_calls") {
            return Ok(parse_final_message(message));
        }

        messages.push(Value::Object(message.clone()));
        let tool_calls = message
            .get("tool_calls")
            .and_then(Value::as_array)
            .context("Kimi returned tool_calls finish reason without tool calls")?;
        if tool_calls.is_empty() {
            bail!("Kimi returned an empty native web-search tool call list");
        }
        reserve_native_search_tool_calls(&mut tool_calls_executed, tool_calls.len())?;
        for tool_call in tool_calls {
            let name = tool_call.pointer("/function/name").and_then(Value::as_str);
            if name != Some("$web_search") {
                bail!("Kimi native search requested an unexpected tool");
            }
            let id = tool_call
                .get("id")
                .and_then(Value::as_str)
                .context("Kimi native web-search call omitted id")?;
            let arguments = tool_call
                .pointer("/function/arguments")
                .and_then(Value::as_str)
                .context("Kimi native web-search call omitted arguments")?;
            let _: Value = serde_json::from_str(arguments)
                .context("Kimi native web-search arguments were not valid JSON")?;
            messages.push(json!({
                "role": "tool",
                "tool_call_id": id,
                "name": "$web_search",
                "content": arguments,
            }));
        }
    }

    bail!("Kimi native web search exceeded the bounded tool-call loop")
}

fn build_builtin_chat_body(model: &str, messages: &[Value], tools: &Value) -> Value {
    json!({
        "model": model,
        "messages": messages,
        "tools": tools,
        "max_completion_tokens": 4_096,
        "stream": false,
        "thinking": { "type": "disabled" },
    })
}

async fn search_formula(
    client: &ProviderNativeSearchClient,
    request: &ProviderNativeSearchRequest,
) -> Result<ProviderNativeSearchResponse> {
    let formula_path = format!("formulas/{WEB_SEARCH_FORMULA_URI}");
    let tools_payload = client
        .get_json(&api_url(
            &client.inner.base_url,
            &format!("{formula_path}/tools"),
        ))
        .await?;
    let tools = formula_web_search_tools(&tools_payload)?;
    let mut messages = vec![json!({
        "role": "user",
        "content": super::search_prompt(request),
    })];
    let mut tool_calls_executed = 0;
    let chat_url = api_url(&client.inner.base_url, "chat/completions");
    let fiber_url = api_url(&client.inner.base_url, &format!("{formula_path}/fibers"));

    for _ in 0..MAX_NATIVE_SEARCH_ROUNDS {
        let body = json!({
            "model": client.inner.default_model,
            "messages": &messages,
            "tools": &tools,
            "stream": false,
        });
        let payload = client.post_json(&chat_url, &body, &[]).await?;
        let choice = payload
            .pointer("/choices/0")
            .context("Kimi Formula web search response omitted choices[0]")?;
        let message = choice
            .get("message")
            .and_then(Value::as_object)
            .context("Kimi Formula web search response omitted assistant message")?;
        let tool_calls = message.get("tool_calls").and_then(Value::as_array);
        if tool_calls.is_none_or(Vec::is_empty) {
            return Ok(parse_final_message(message));
        }

        let tool_calls = tool_calls.expect("checked above");
        reserve_native_search_tool_calls(&mut tool_calls_executed, tool_calls.len())?;
        messages.push(Value::Object(message.clone()));
        for tool_call in tool_calls {
            let id = tool_call
                .get("id")
                .and_then(Value::as_str)
                .context("Kimi Formula web-search call omitted id")?;
            let function = tool_call
                .get("function")
                .and_then(Value::as_object)
                .context("Kimi Formula web-search call omitted function")?;
            let name = function
                .get("name")
                .and_then(Value::as_str)
                .context("Kimi Formula web-search call omitted function name")?;
            if name != WEB_SEARCH_FORMULA_FUNCTION {
                bail!("Kimi Formula web search requested an unexpected tool");
            }
            let arguments = function
                .get("arguments")
                .and_then(Value::as_str)
                .context("Kimi Formula web-search call omitted arguments")?;
            let _: Value = serde_json::from_str(arguments)
                .context("Kimi Formula web-search arguments were not valid JSON")?;
            let fiber = client
                .post_json(
                    &fiber_url,
                    &json!({ "name": name, "arguments": arguments }),
                    &[],
                )
                .await?;
            let result = formula_fiber_result(&fiber)?;
            messages.push(json!({
                "role": "tool",
                "tool_call_id": id,
                "content": result,
            }));
        }
    }

    bail!("Kimi Formula web search exceeded the bounded tool-call loop")
}

fn reserve_native_search_tool_calls(executed: &mut usize, additional: usize) -> Result<()> {
    let total = executed
        .checked_add(additional)
        .context("Kimi native web search tool-call count overflowed")?;
    if total > MAX_NATIVE_SEARCH_TOOL_CALLS {
        bail!(
            "Kimi native web search exceeded the {MAX_NATIVE_SEARCH_TOOL_CALLS}-call safety limit"
        );
    }
    *executed = total;
    Ok(())
}

fn formula_web_search_tools(payload: &Value) -> Result<Value> {
    let tools = payload
        .get("tools")
        .and_then(Value::as_array)
        .context("Kimi web-search Formula omitted tools")?;
    if tools.len() != 1
        || tools[0].get("type").and_then(Value::as_str) != Some("function")
        || tools[0].pointer("/function/name").and_then(Value::as_str)
            != Some(WEB_SEARCH_FORMULA_FUNCTION)
    {
        bail!("Kimi web-search Formula returned an unexpected tool declaration");
    }
    Ok(Value::Array(tools.clone()))
}

fn formula_fiber_result(payload: &Value) -> Result<&str> {
    if payload.get("status").and_then(Value::as_str) != Some("succeeded") {
        bail!("Kimi web-search Formula fiber did not succeed");
    }
    payload
        .pointer("/context/output")
        .or_else(|| payload.pointer("/context/encrypted_output"))
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|result| !result.is_empty())
        .context("Kimi web-search Formula fiber omitted its result")
}

fn builtin_search_tools() -> Value {
    json!([{
        "type": "builtin_function",
        "function": { "name": "$web_search" }
    }])
}

fn parse_kimi_code(payload: &Value) -> ProviderNativeSearchResponse {
    let mut citations = Vec::new();
    if let Some(results) = payload.get("search_results").and_then(Value::as_array) {
        for result in results {
            let Some(url) = result.get("url").and_then(Value::as_str) else {
                continue;
            };
            let title = result
                .get("title")
                .and_then(Value::as_str)
                .map(str::to_string);
            let snippet = result
                .get("snippet")
                .and_then(Value::as_str)
                .map(str::to_string);
            let published = result
                .get("date")
                .and_then(Value::as_str)
                .filter(|value| !value.is_empty())
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

fn parse_final_message(message: &Map<String, Value>) -> ProviderNativeSearchResponse {
    let answer = message
        .get("content")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|text| !text.is_empty())
        .map(str::to_string);
    let citations = answer
        .as_deref()
        .map(citations_from_text)
        .unwrap_or_default();
    ProviderNativeSearchResponse {
        answer: bounded_answer(answer.into_iter().collect()),
        citations,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request() -> ProviderNativeSearchRequest {
        ProviderNativeSearchRequest {
            query: "current release".to_string(),
            max_results: 3,
            domains: vec![],
        }
    }

    #[test]
    fn kimi_code_request_uses_dedicated_search_contract() {
        let body = build_kimi_code_body(&request());
        assert_eq!(body["text_query"], "current release");
        assert_eq!(body.as_object().map(serde_json::Map::len), Some(1));
    }

    #[test]
    fn moonshot_request_declares_builtin_web_search() {
        let tools = builtin_search_tools();
        assert_eq!(tools[0]["type"], "builtin_function");
        assert_eq!(tools[0]["function"]["name"], "$web_search");
        let body = build_builtin_chat_body("kimi-k2.6", &[], &tools);
        assert_eq!(body["max_completion_tokens"], 4_096);
        assert_eq!(body["thinking"]["type"], "disabled");
    }

    #[test]
    fn parses_kimi_code_structured_results() {
        let parsed = parse_kimi_code(&json!({
            "search_results": [{
                "title": "Kimi",
                "url": "https://example.com/kimi",
                "snippet": "Summary",
                "date": "2026-08-28"
            }]
        }));
        assert_eq!(parsed.citations.len(), 1);
        assert_eq!(parsed.citations[0].snippet.as_deref(), Some("Summary"));
        assert_eq!(parsed.citations[0].published.as_deref(), Some("2026-08-28"));
    }

    #[test]
    fn native_search_tool_call_limit_is_total_not_per_round() {
        let mut executed = 0;
        reserve_native_search_tool_calls(&mut executed, 1).expect("first round");
        reserve_native_search_tool_calls(&mut executed, 3).expect("second round");
        reserve_native_search_tool_calls(&mut executed, 4).expect("third round");
        assert_eq!(executed, MAX_NATIVE_SEARCH_TOOL_CALLS);
        assert!(reserve_native_search_tool_calls(&mut executed, 1).is_err());
        assert_eq!(executed, MAX_NATIVE_SEARCH_TOOL_CALLS);
    }
}
