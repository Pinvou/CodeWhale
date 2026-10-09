//! OpenAI Responses API bridge for the OpenAI Codex / ChatGPT provider.
//!
//! Implements a dedicated Responses API client that maps CodeWhale's internal
//! message/tool types to the Responses wire format and parses streaming SSE
//! events back into CodeWhale's `StreamEvent` / `MessageResponse` types.
//!
//! This is intentionally separate from the Chat Completions path
//! (`client/chat.rs`) to avoid protocol hacks.

use anyhow::{Context, Result};
use codewhale_config::provider::WireFormat;
use codewhale_config::provider_base_url_is_official;
use serde_json::{Value, json};
use std::collections::HashSet;

use crate::config::ApiProvider;
use crate::llm_client::StreamEventBox;
use crate::logging;
use crate::models::{
    ContentBlock, ContentBlockStart, Delta, MessageDelta, MessageRequest, MessageResponse,
    OpaqueReasoningState, StreamEvent, Tool, Usage,
};
use crate::tools::schema_sanitize;

use super::prepared::WireDialect;
use super::role_placement::{RolePlacement, role_placement};
use super::wire::{extract_sse_data_value, next_sse_line};
use super::{
    DeepSeekClient, ERROR_BODY_MAX_BYTES, bounded_error_text, from_api_tool_name,
    system_to_instructions, to_api_tool_name,
};

/// Base URL path for the Codex Responses endpoint.
pub(super) const CODEX_RESPONSES_PATH: &str = "/codex/responses";

/// Build the Responses API request body from a `MessageRequest`.
#[cfg(test)]
pub(super) fn build_responses_body(request: &MessageRequest) -> Value {
    build_responses_body_for_provider(
        request,
        ApiProvider::OpenaiCodex,
        ApiProvider::OpenaiCodex.as_str(),
        "fp-codex-endpoint",
        // The catalog Codex endpoint — the only URL fingerprint-less legacy
        // states may replay onto (see the gate in
        // `convert_messages_to_responses_input`).
        "https://chatgpt.com/backend-api",
    )
}

/// Whether a Responses route's request body carries
/// `include: ["reasoning.encrypted_content"]`: every Responses-wire provider
/// except DeepSeek (stateless, plain `reasoning_text`, no `include`) and
/// Concentrate (documented fields only). The capture gate in
/// `handle_responses_stream` derives from this same predicate, so include and
/// capture cover the same route set by construction.
fn responses_route_sends_encrypted_reasoning_include(provider: ApiProvider) -> bool {
    !matches!(
        provider,
        ApiProvider::Deepseek | ApiProvider::DeepseekCN | ApiProvider::Concentrate
    )
}

/// Whether a captured provider tag names endpoint-scoped Custom identity: the
/// legacy root table (`custom`) or a named table (`custom/<name>`), exactly
/// the tags `DeepSeekClient::reasoning_provider_tag` mints for Custom. These
/// are the tags whose endpoint can change while the tag stays put, so they
/// are the fingerprint-less (pre-fingerprint) states that always fail closed.
/// Built-in tags take the narrower rule at the replay gate: a fingerprint-less
/// state may only ride the provider's official endpoint — without a
/// fingerprint it is the one origin this gate still vouches for.
fn is_custom_reasoning_tag(tag: &str) -> bool {
    tag == "custom" || tag.starts_with("custom/")
}

/// Build a provider-aware Responses API request body.
///
/// DeepSeek-V4-Flash-0731 implements the Responses wire shape but is stateless
/// and exposes plain reasoning text rather than OpenAI encrypted summaries.
/// Keep those exact-route differences here instead of leaking them into the
/// provider-neutral message model.
///
/// `reasoning_provider_tag` is the tag the caller's capture side mints into
/// [`OpaqueReasoningState`] (see `DeepSeekClient::reasoning_provider_tag`);
/// the replay gate below only reattaches reasoning items whose state carries
/// that exact tag, so encrypted reasoning minted by one endpoint — a named
/// Custom table — is never replayed to another. `reasoning_endpoint_fingerprint`
/// is the same client's endpoint fingerprint (see
/// `DeepSeekClient::reasoning_endpoint_fingerprint`): the tag pins the table
/// name, not the URL behind it, so a state captured before the table's
/// `base_url` was edited stops replaying. States minted before fingerprints
/// existed carry no proof of origin: Custom endpoints can move under a
/// stable tag, so those fail closed, and built-in tags keep replaying only
/// while `current_base_url` is still the provider's official endpoint — a
/// client re-pointed by config has no way to prove where an old state was
/// captured, so it fails closed too.
///
/// `current_base_url` is the endpoint this request is about to be POSTed to
/// (the client's frozen base URL); it decides that legacy arm.
pub(super) fn build_responses_body_for_provider(
    request: &MessageRequest,
    provider: ApiProvider,
    reasoning_provider_tag: &str,
    reasoning_endpoint_fingerprint: &str,
    current_base_url: &str,
) -> Value {
    let is_deepseek = matches!(provider, ApiProvider::Deepseek | ApiProvider::DeepseekCN);
    // Concentrate documents `model`, `input`, `stream`, `max_output_tokens`,
    // `tools` / `tool_choice` / `parallel_tool_calls`, and `reasoning.effort`;
    // `store`, `include`, `instructions`, and `reasoning.summary` are absent
    // from its parameter reference, so this route sends only documented
    // fields and carries the system prompt as a leading `system` message item
    // (a documented input role) instead of `instructions`.
    // https://concentrate.ai/docs/api-reference/endpoint/request-parameters
    let is_concentrate = provider == ApiProvider::Concentrate;
    let model = &request.model;
    let mut body = json!({
        "model": model,
        "stream": true,
    });
    // One shared route set decides `store:false`, `include`, and capture —
    // see `responses_route_sends_encrypted_reasoning_include`. Keeping all
    // three on the same predicate is what makes capture symmetric with the
    // request.
    let captures_reasoning = responses_route_sends_encrypted_reasoning_include(provider);
    if captures_reasoning {
        body["store"] = json!(false);
    }
    // Every Responses route receives the same resolved request envelope as
    // Chat and Messages. Omitting this field let auxiliary Responses calls
    // escape the central route cap and made preview unable to prove the wire
    // allowance. The Codex OAuth backend is the exception: its Responses
    // endpoint rejects the field outright ("Unsupported parameter:
    // max_output_tokens"), so its requests carry no client-side output cap
    // instead of failing every call — the same lesson the Chat path learned
    // in `apply_provider_token_limit`.
    if request.max_tokens > 0 && provider != ApiProvider::OpenaiCodex {
        body["max_output_tokens"] = json!(request.max_tokens);
    }
    if is_deepseek {
        if let Some(temperature) = request.temperature {
            body["temperature"] = json!(temperature);
        }
        if let Some(top_p) = request.top_p {
            body["top_p"] = json!(top_p);
        }
    }

    // Instructions (system prompt). The Codex Responses backend rejects
    // requests without instructions, so fall back to a minimal system
    // prompt when the caller did not supply one.
    let instructions = system_to_instructions(request.system.clone())
        .filter(|text| !text.trim().is_empty())
        .unwrap_or_else(|| "You are a helpful assistant.".to_string());

    // Convert messages to Responses input items.
    let mut input = convert_messages_to_responses_input(
        request,
        provider,
        reasoning_provider_tag,
        reasoning_endpoint_fingerprint,
        current_base_url,
    );
    if is_concentrate {
        input.insert(
            0,
            json!({
                "type": "message",
                "role": "system",
                "content": [{ "type": "input_text", "text": instructions }],
            }),
        );
    } else {
        body["instructions"] = json!(instructions);
    }
    body["input"] = json!(input);

    // Convert tools to Responses function tools.
    if let Some(tools) = request.tools.as_ref() {
        let responses_tools: Vec<Value> = tools.iter().map(tool_to_responses_function).collect();
        if !responses_tools.is_empty() {
            body["tools"] = json!(responses_tools);
            body["tool_choice"] = json!("auto");
            body["parallel_tool_calls"] = json!(true);
        }
    }

    // Reasoning configuration. The Codex Responses backend accepts
    // low/medium/high/xhigh, so provider-aware callers normalize inherited
    // DeepSeek-only values before request construction: "off" becomes
    // "low", and CodeWhale's "auto" falls back to "medium". DeepSeek's
    // Responses API documents `reasoning.effort: "none"` to disable
    // thinking, so its branch sends "none" for the off tier instead of
    // collapsing it into low (see `responses_reasoning_effort`).
    if let Some(raw) = request.reasoning_effort.as_deref()
        && let Some(effort) = responses_reasoning_effort(raw, is_deepseek)
    {
        body["reasoning"] = if is_deepseek || is_concentrate {
            json!({ "effort": effort })
        } else {
            json!({
                "effort": effort,
                "summary": "auto",
            })
        };
    }

    // Every Responses route that receives this builder can replay encrypted
    // reasoning. The include predicate is the same one the capture gate in
    // `handle_responses_stream` derives from, so include and capture cover
    // the same route set by construction — a future Responses-wire provider
    // cannot silently start sending `include` without capturing.
    if responses_route_sends_encrypted_reasoning_include(provider) {
        body["include"] = json!(["reasoning.encrypted_content"]);
    }

    body
}

impl DeepSeekClient {
    /// Handle a streaming Responses API request for the OpenAI Codex provider.
    pub(super) async fn handle_responses_stream(
        &self,
        prepared: &super::PreparedOutboundRequest,
    ) -> Result<StreamEventBox> {
        // Body, endpoint, and route shape all come from the shared
        // prepared-request seam (`prepare_outbound_request`).
        let body = &prepared.body;
        let is_codex = prepared.endpoint.shape == super::RouteShape::CodexResponses;
        let url = prepared.endpoint.url.clone();
        // The synthetic MessageStart below is emitted from inside the stream
        // closure, which outlives `prepared`. Clone the wire model — the id
        // actually placed on the body by the shared seam, after route
        // remapping — rather than borrowing the request that no longer exists
        // at this layer.
        let wire_model = prepared.wire_model.clone();
        // Encrypted-reasoning capture applies to every Responses route whose
        // request carries `include: ["reasoning.encrypted_content"]` — the
        // same predicate the body builder uses to send the include — and
        // replays by the endpoint-scoped provider tag from
        // `reasoning_provider_tag` plus the endpoint fingerprint from
        // `reasoning_endpoint_fingerprint`. The transport-wire half keeps
        // Chat-wire Custom tables (and any other dialect) excluded.
        let reasoning_origin = (self.wire_format == WireFormat::Responses
            && responses_route_sends_encrypted_reasoning_include(self.api_provider))
        .then(|| {
            (
                self.reasoning_provider_tag(),
                self.reasoning_endpoint_fingerprint(),
                wire_model.clone(),
            )
        });

        // The bearer Authorization header is already installed as a default
        // header on both the dual and the HTTP/1.1 twin client (resolved from
        // the Codex OAuth access token), so it must not be set again here or
        // it would be duplicated. The ChatGPT backend additionally requires
        // the account id and the experimental Responses beta opt-in.
        //
        // The open itself goes through the shared stream-entry transport
        // policy: bounded header wait, policy-selected client, and at most
        // one HTTP/1.1 fallback retry on a classified H2 header stall. The
        // pre-existing provider retry loop (rate limit / transient upstream)
        // stays inside each open attempt, before any stream body exists.
        let account_id = self.codex_account_id.clone();
        let request_body =
            serde_json::to_vec(&body).context("Failed to serialize Responses API request body")?;
        let open_req = super::stream_entry::StreamOpenRequest::new(
            super::stream_entry::stream_open_timeout(),
            self.stream_idle_timeout,
        );
        let response = super::stream_entry::open_sse_response(&open_req, |policy| {
            let url = url.clone();
            let account_id = account_id.clone();
            let request_body = request_body.clone();
            async move {
                let client = super::stream_entry::client_for_policy(
                    &self.http_client,
                    self.http1_fallback_client(),
                    policy,
                );
                // Stream open: no per-request total — the response body
                // outlives the open, and a total would ride on it and
                // truncate the stream.
                self.send_stream_open_with_retry(|| {
                    let mut builder = client
                        .post(&url)
                        .header("Content-Type", "application/json")
                        .header("Accept", "text/event-stream");
                    if is_codex {
                        builder = builder
                            .header("OpenAI-Beta", "responses=experimental")
                            .header("originator", "codex_cli_rs");
                        if let Some(account_id) = &account_id {
                            builder = builder.header("chatgpt-account-id", account_id);
                        }
                    }
                    builder.body(request_body.clone())
                })
                .await
                .context("Responses API request failed")
            }
        })
        .await?;

        let status = response.status();
        crate::client::record_provider_response(self.api_provider, status.as_u16());
        if !status.is_success() {
            let raw = bounded_error_text(response, ERROR_BODY_MAX_BYTES).await;
            anyhow::bail!("Responses API error (HTTP {status}): {raw}");
        }

        let stream_idle_timeout = self.stream_idle_timeout;
        let byte_stream = response.bytes_stream();

        let stream = async_stream::stream! {
            use futures_util::StreamExt;

            // Emit synthetic MessageStart.
            yield Ok(StreamEvent::MessageStart {
                message: MessageResponse {
                    id: String::new(),
                    r#type: "message".to_string(),
                    role: "assistant".to_string(),
                    content: vec![],
                    model: wire_model.clone(),
                    stop_reason: None,
                    stop_sequence: None,
                    container: None,
                    usage: Usage::default(),
                },
            });

            let mut current_block_index: Option<u32> = None;
            // Whether reasoning text has already been emitted for the current
            // reasoning block. Used to insert a paragraph break between
            // consecutive summary parts, which the wire protocol delivers
            // back-to-back with no separator.
            let mut reasoning_text_emitted = false;
            let mut saw_tool_call = false;
            let mut usage_data: Option<Usage> = None;
            // Raw byte buffer: decode only COMPLETE lines (or the stream-end
            // tail) via the shared take_sse_line / flush_sse_line helpers so a
            // multi-byte UTF-8 char split across HTTP/2 DATA is never
            // corrupted to U+FFFD. Genuine invalid bytes fail closed.
            let mut buffer: Vec<u8> = Vec::new();
            let mut done = false;
            let mut ended = false;
            let mut content_block_counter: u32 = 0;
            let stream_start = std::time::Instant::now();
            let mut last_chunk_at = std::time::Instant::now();
            let mut bytes_received: usize = 0;

            tokio::pin!(byte_stream);

            while !done {
                if !ended {
                    match tokio::time::timeout(stream_idle_timeout, byte_stream.next()).await {
                        Ok(Some(Ok(chunk))) => {
                            bytes_received += chunk.len();
                            last_chunk_at = std::time::Instant::now();
                            buffer.extend_from_slice(&chunk);
                        }
                        Ok(Some(Err(e))) => {
                            yield Err(anyhow::anyhow!("Stream read error: {e}"));
                            return;
                        }
                        Ok(None) => ended = true,
                        Err(_) => {
                            yield Err(anyhow::anyhow!(super::stream_entry::idle_timeout_message(
                                stream_idle_timeout,
                                bytes_received,
                                stream_start.elapsed(),
                                last_chunk_at.elapsed(),
                            )));
                            return;
                        }
                    }
                }

                // Process complete SSE lines, and the unterminated tail at stream end.
                loop {
                    let line = match next_sse_line(&mut buffer, ended) {
                        Ok(Some(line)) => line,
                        Ok(None) => break,
                        Err(err) => {
                            yield Err(anyhow::anyhow!("{err}"));
                            return;
                        }
                    };

                    if line.is_empty() || line.starts_with(':') {
                        continue;
                    }

                    if let Some(data) = extract_sse_data_value(&line) {
                        if data == "[DONE]" {
                            done = true;
                            break;
                        }

                        let event: Value = match serde_json::from_str(data) {
                            Ok(v) => v,
                            Err(e) => {
                                logging::warn(format!(
                                    "Failed to parse Responses SSE event: {e}"
                                ));
                                continue;
                            }
                        };

                        let event_type =
                            event.get("type").and_then(|t| t.as_str()).unwrap_or("");

                        match event_type {
                            "response.output_item.added" => {
                                if let Some(item) = event.get("item") {
                                    let item_type = item
                                        .get("type")
                                        .and_then(|v| v.as_str())
                                        .unwrap_or("");

                                    match item_type {
                                        "message" => {
                                            content_block_counter += 1;
                                            yield Ok(StreamEvent::ContentBlockStart {
                                                index: content_block_counter - 1,
                                                content_block: ContentBlockStart::Text {
                                                    text: String::new(),
                                                },
                                            });
                                            current_block_index =
                                                Some(content_block_counter - 1);
                                        }
                                        "function_call" => {
                                            let call_id = item
                                                .get("call_id")
                                                .and_then(|v| v.as_str())
                                                .unwrap_or("")
                                                .to_string();
                                            let item_id = item
                                                .get("id")
                                                .and_then(|v| v.as_str())
                                                .unwrap_or("")
                                                .to_string();
                                            let name = item
                                                .get("name")
                                                .and_then(|v| v.as_str())
                                                .unwrap_or("")
                                                .to_string();
                                            saw_tool_call = true;
                                            // call_id and item_id are folded
                                            // into a composite tool-use id so
                                            // the function_call_output can be
                                            // routed back to the right call.
                                            let composite_id =
                                                format!("{call_id}|{item_id}");
                                            content_block_counter += 1;
                                            yield Ok(StreamEvent::ContentBlockStart {
                                                index: content_block_counter - 1,
                                                content_block:
                                                    ContentBlockStart::ToolUse {
                                                        id: composite_id,
                                                        name: from_api_tool_name(&name),
                                                        input: json!({}),
                                                        caller: None,
                                                    thought_signature: None,
                                                },
                                            });
                                            current_block_index =
                                                Some(content_block_counter - 1);
                                        }
                                        "reasoning" => {
                                            reasoning_text_emitted = false;
                                            content_block_counter += 1;
                                            yield Ok(StreamEvent::ContentBlockStart {
                                                index: content_block_counter - 1,
                                                content_block:
                                                    ContentBlockStart::Thinking {
                                                        thinking: String::new(),
                                                    },
                                            });
                                            current_block_index =
                                                Some(content_block_counter - 1);
                                        }
                                        // DeepSeek can run server-side web
                                        // search on this route, but Codewhale
                                        // does not yet replay `web_search_call`
                                        // items or their citations (the
                                        // offering keeps
                                        // `server_side_web_search: Unknown`).
                                        // Surface a visible notice instead of
                                        // dropping the item silently so the
                                        // user is not handed an ungrounded
                                        // answer with no explanation.
                                        "web_search_call" => {
                                            content_block_counter += 1;
                                            yield Ok(StreamEvent::ContentBlockStart {
                                                index: content_block_counter - 1,
                                                content_block:
                                                    ContentBlockStart::Text {
                                                        text: "[Web search ran server-side; results are not replayed on this route.]".to_string(),
                                                    },
                                            });
                                            current_block_index =
                                                Some(content_block_counter - 1);
                                        }
                                        _ => {}
                                    }
                                }
                            }
                            "response.output_text.delta" => {
                                if let Some(delta_text) =
                                    event.get("delta").and_then(|d| d.as_str())
                                    && let Some(idx) = current_block_index
                                {
                                    yield Ok(StreamEvent::ContentBlockDelta {
                                        index: idx,
                                        delta: Delta::TextDelta {
                                            text: delta_text.to_string(),
                                        },
                                    });
                                }
                            }
                            "response.function_call_arguments.delta" => {
                                if let Some(delta_text) =
                                    event.get("delta").and_then(|d| d.as_str())
                                    && let Some(idx) = current_block_index
                                {
                                    yield Ok(StreamEvent::ContentBlockDelta {
                                        index: idx,
                                        delta: Delta::InputJsonDelta {
                                            partial_json: delta_text.to_string(),
                                        },
                                    });
                                }
                            }
                            "response.reasoning_summary_text.delta"
                            | "response.reasoning_text.delta" => {
                                if let Some(delta_text) =
                                    event.get("delta").and_then(|d| d.as_str())
                                    && let Some(idx) = current_block_index
                                {
                                    if !delta_text.is_empty() {
                                        reasoning_text_emitted = true;
                                    }
                                    yield Ok(StreamEvent::ContentBlockDelta {
                                        index: idx,
                                        delta: Delta::ThinkingDelta {
                                            thinking: delta_text.to_string(),
                                        },
                                    });
                                }
                            }
                            "response.reasoning_summary_part.added" => {
                                // Consecutive summary parts arrive with no
                                // separator in the text deltas, so without a
                                // boundary they concatenate as
                                // "…done.**Next Phase**…". Insert a paragraph
                                // break before every part after the first.
                                if reasoning_text_emitted
                                    && let Some(idx) = current_block_index
                                {
                                    yield Ok(StreamEvent::ContentBlockDelta {
                                        index: idx,
                                        delta: Delta::ThinkingDelta {
                                            thinking: "\n\n".to_string(),
                                        },
                                    });
                                }
                            }
                            "response.output_item.done" => {
                                if let Some(idx) = current_block_index {
                                    if let (Some((provider, endpoint, model)), Some(item)) =
                                        (reasoning_origin.as_ref(), event.get("item"))
                                        && item.get("type").and_then(Value::as_str)
                                            == Some("reasoning")
                                    {
                                        match item
                                            .get("encrypted_content")
                                            .and_then(Value::as_str)
                                            .filter(|value| !value.is_empty())
                                        {
                                            Some(encrypted_content) => {
                                                yield Ok(StreamEvent::ContentBlockDelta {
                                                    index: idx,
                                                    delta: Delta::ReasoningStateDelta {
                                                        state: OpaqueReasoningState {
                                                            provider: provider.clone(),
                                                            api: "openai-responses".to_string(),
                                                            model: model.clone(),
                                                            id: item
                                                                .get("id")
                                                                .and_then(Value::as_str)
                                                                .map(str::to_string),
                                                            encrypted_content:
                                                                encrypted_content.to_string(),
                                                            endpoint: Some(endpoint.clone()),
                                                        },
                                                    },
                                                });
                                            }
                                            None => {
                                                // With `store:false` this item
                                                // cannot be replayed; its
                                                // paired tool calls would be
                                                // rejected on the next request.
                                                logging::warn(
                                                    "Responses reasoning item completed without encrypted_content; replay will omit it and its paired tool calls may be rejected by the provider",
                                                );
                                            }
                                        }
                                    }
                                    yield Ok(StreamEvent::ContentBlockStop { index: idx });
                                    current_block_index = None;
                                }
                            }
                            "response.completed" | "response.incomplete" => {
                                if let Some(resp) = event.get("response") {
                                    if let Some(usage_val) = resp.get("usage") {
                                        usage_data =
                                            Some(parse_responses_usage(usage_val));
                                    }
                                    let stop_reason = responses_stop_reason(resp, saw_tool_call);
                                    yield Ok(StreamEvent::MessageDelta {
                                        delta: MessageDelta {
                                            stop_reason: Some(stop_reason),
                                            stop_sequence: None,
                                        },
                                        usage: usage_data.take(),
                                    });
                                }
                                // DeepSeek terminates semantic Responses
                                // streams with this event and deliberately does
                                // not send `data: [DONE]`.
                                done = true;
                            }
                            "error" | "response.failed" => {
                                let (code, msg) = responses_event_error_details(&event);
                                yield Err(anyhow::anyhow!(
                                    "Responses API error [{code}]: {msg}"
                                ));
                                return;
                            }
                            _ => {
                                // Ignore unknown event types.
                            }
                        }
                    }
                }

                if ended {
                    break;
                }
            }

            // Emit MessageStop.
            yield Ok(StreamEvent::MessageStop);
        };

        Ok(Box::pin(stream))
    }

    /// Non-streaming Responses request: drive the streaming handler and fold
    /// its events into a single `MessageResponse`.
    ///
    /// The ChatGPT Codex backend only serves streaming responses, so the
    /// non-streaming entry point (`create_message`, used by `exec`) reuses the
    /// same wire path as the interactive stream rather than a second request
    /// shape.
    pub(super) async fn handle_responses_message(
        &self,
        prepared: &super::PreparedOutboundRequest,
    ) -> Result<MessageResponse> {
        use futures_util::StreamExt;

        let model = prepared.wire_model.clone();
        let mut stream = self.handle_responses_stream(prepared).await?;

        let mut response = MessageResponse {
            id: String::new(),
            r#type: "message".to_string(),
            role: "assistant".to_string(),
            content: Vec::new(),
            model,
            stop_reason: None,
            stop_sequence: None,
            container: None,
            usage: Usage::default(),
        };
        // Accumulated tool-call argument JSON, parallel to `response.content`.
        let mut tool_args: Vec<String> = Vec::new();

        while let Some(event) = stream.next().await {
            match event? {
                StreamEvent::MessageStart { message } => {
                    response.id = message.id;
                    response.usage = message.usage;
                }
                StreamEvent::ContentBlockStart { content_block, .. } => {
                    let block = match content_block {
                        ContentBlockStart::Text { text } => ContentBlock::Text {
                            text,
                            cache_control: None,
                        },
                        ContentBlockStart::Thinking { thinking } => ContentBlock::Thinking {
                            thinking,
                            signature: None,
                            state: None,
                            redacted_data: None,
                            reasoning_details: None,
                        },
                        // Redacted thinking does not occur on Responses
                        // routes (encrypted reasoning rides the reasoning
                        // item's `encrypted_content` instead); keep the
                        // match total without inventing a block shape.
                        ContentBlockStart::RedactedThinking { .. } => {
                            continue;
                        }
                        ContentBlockStart::ToolUse {
                            id,
                            name,
                            input,
                            caller,
                            thought_signature,
                        } => ContentBlock::ToolUse {
                            id,
                            name,
                            input,
                            caller,
                            thought_signature,
                        },
                        ContentBlockStart::ServerToolUse { id, name, input } => {
                            ContentBlock::ServerToolUse { id, name, input }
                        }
                    };
                    response.content.push(block);
                    tool_args.push(String::new());
                }
                StreamEvent::ContentBlockDelta { index, delta } => {
                    let i = index as usize;
                    match delta {
                        Delta::TextDelta { text } => {
                            if let Some(ContentBlock::Text { text: existing, .. }) =
                                response.content.get_mut(i)
                            {
                                existing.push_str(&text);
                            }
                        }
                        Delta::ThinkingDelta { thinking } => {
                            if let Some(ContentBlock::Thinking {
                                thinking: existing, ..
                            }) = response.content.get_mut(i)
                            {
                                existing.push_str(&thinking);
                            }
                        }
                        Delta::InputJsonDelta { partial_json } => {
                            if let Some(buf) = tool_args.get_mut(i) {
                                buf.push_str(&partial_json);
                            }
                        }
                        Delta::SignatureDelta { .. } => {
                            // Anthropic-native signature deltas never occur on
                            // the Responses bridge (#3014).
                        }
                        Delta::ToolThoughtSignatureDelta { .. } => {
                            // Google compat-route signatures never occur on
                            // the Responses bridge.
                        }
                        Delta::ReasoningStateDelta { state } => {
                            if let Some(ContentBlock::Thinking {
                                state: existing, ..
                            }) = response.content.get_mut(i)
                            {
                                *existing = Some(state);
                            }
                        }
                        Delta::ReasoningDetailsDelta { .. } => {
                            // OpenRouter chat-route details never occur on
                            // the Responses bridge.
                        }
                    }
                }
                StreamEvent::ContentBlockStop { index } => {
                    let i = index as usize;
                    if let Some(buf) = tool_args.get(i)
                        && !buf.trim().is_empty()
                        && let Ok(parsed) = serde_json::from_str::<Value>(buf)
                        && let Some(ContentBlock::ToolUse { input, .. }) =
                            response.content.get_mut(i)
                    {
                        *input = parsed;
                    }
                }
                StreamEvent::MessageDelta { delta, usage } => {
                    if let Some(stop_reason) = delta.stop_reason {
                        response.stop_reason = Some(stop_reason);
                    }
                    if let Some(usage) = usage {
                        response.usage = usage;
                    }
                }
                StreamEvent::MessageStop => break,
                _ => {}
            }
        }

        Ok(response)
    }
}

pub(super) fn responses_tool_output(content: &str, content_blocks: Option<&[Value]>) -> Value {
    let (image, omitted) = crate::image_attach::provider_tool_result_image_refs(content_blocks);
    let content = crate::image_attach::tool_result_text_with_omission(content, omitted);
    let Some((mime_type, data)) = image else {
        return json!(content);
    };
    let mut output = Vec::with_capacity(2);
    if !content.is_empty() {
        output.push(json!({ "type": "input_text", "text": content }));
    }
    output.push(json!({
        "type": "input_image",
        "image_url": format!("data:{mime_type};base64,{data}"),
        "detail": "auto",
    }));
    json!(output)
}

/// Whether an assistant message carries tool calls.
fn message_has_tool_calls(msg: &crate::models::Message) -> bool {
    msg.content
        .iter()
        .any(|block| matches!(block, ContentBlock::ToolUse { .. }))
}

/// Whether any thinking block of this message replays its encrypted
/// reasoning on this request: same endpoint-scoped provider tag, same
/// Responses api shape, same model, and the endpoint rules (fingerprint
/// match, or the fingerprint-less official-endpoint credit). This is exactly
/// the reattach gate inside [`convert_messages_to_responses_input`], kept in
/// one place so the chain-drop gate can never disagree with the gate that
/// actually reattaches the item.
fn message_has_replayable_reasoning(
    msg: &crate::models::Message,
    reasoning_provider_tag: &str,
    reasoning_endpoint_fingerprint: &str,
    fingerprintless_replay_official: bool,
    model: &str,
) -> bool {
    msg.content.iter().any(|block| {
        matches!(
            block,
            ContentBlock::Thinking {
                state: Some(state),
                redacted_data: None,
                ..
            } if state.provider == reasoning_provider_tag
                && state.api == "openai-responses"
                && state.model == model
                && match &state.endpoint {
                    None => {
                        !is_custom_reasoning_tag(&state.provider)
                            && fingerprintless_replay_official
                    }
                    Some(captured) => captured == reasoning_endpoint_fingerprint,
                }
        )
    })
}

/// Convert Codewhale messages to Responses API input items.
///
/// `reasoning_provider_tag` scopes opaque-reasoning replay to the endpoint
/// that minted the state; see [`build_responses_body_for_provider`].
/// Pairing integrity: with `store:false`, a `function_call` item needs the
/// encrypted `reasoning` item that produced it replayed ahead of it, and a
/// `reasoning` item needs the item(s) it produced to follow it — the docs'
/// rule is completeness plus original positions ("all items between the last
/// user message and your function call output are passed into the next
/// response untouched"), and the API rejects a reasoning item whose
/// following item is missing. When a message's reasoning is unusable on
/// this route (captured on a different model, endpoint tag or fingerprint,
/// or by a build that did not capture it), its whole tool chain — reasoning,
/// function calls, and their outputs — is dropped instead of shipping a
/// request the API rejects outright.
pub(super) fn convert_messages_to_responses_input(
    request: &MessageRequest,
    provider: ApiProvider,
    reasoning_provider_tag: &str,
    reasoning_endpoint_fingerprint: &str,
    current_base_url: &str,
) -> Vec<Value> {
    let is_deepseek = matches!(provider, ApiProvider::Deepseek | ApiProvider::DeepseekCN);
    // Fingerprint-less (pre-fingerprint) states carry no proof of which
    // endpoint minted them, so they may only replay where the endpoint cannot
    // have moved: a built-in on its official endpoint family. Custom is
    // excluded by its tag at the gate below (and
    // `provider_base_url_is_official` rejects it outright), and a built-in
    // re-pointed by config or env loses the credit — it has no way to prove
    // an old state came from the new URL.
    let fingerprintless_replay_official = provider
        .kind()
        .is_some_and(|kind| provider_base_url_is_official(kind, current_base_url));
    // Replay rides the same predicate that sends the `include`: a provider
    // whose body omits `include: ["reasoning.encrypted_content"]` (DeepSeek's
    // plain `reasoning_text`, Concentrate's documented-fields contract) must
    // never receive an encrypted replay item either, so the replay gate
    // cannot drift from the include gate.
    let replays_encrypted_reasoning = responses_route_sends_encrypted_reasoning_include(provider);
    // Hoisted: the reasoning-capability lookup lowercases and consults the
    // catalog, and the loop would otherwise repeat it per assistant message.
    let reasoning_capable_model = crate::models::model_supports_reasoning(&request.model);
    let mut items = Vec::new();
    let mut orphaned_call_ids: HashSet<String> = HashSet::new();
    let mut dropped_chains: usize = 0;

    for msg in &request.messages {
        // Channel selection lives in the shared placement table; this adapter
        // owns only the shape of each channel's items.
        let placement = role_placement(&msg.role, WireDialect::OpenAiResponses);
        match placement {
            RolePlacement::User => {
                let mut content_items = Vec::new();
                for block in &msg.content {
                    match block {
                        ContentBlock::Text { text, .. } => {
                            content_items.push(json!({
                                "type": "input_text",
                                "text": text,
                            }));
                        }
                        ContentBlock::ImageUrl { image_url } => {
                            content_items.push(json!({
                                "type": "input_image",
                                "image_url": image_url.url,
                            }));
                        }
                        ContentBlock::ToolResult {
                            tool_use_id,
                            content,
                            content_blocks,
                            ..
                        } => {
                            if !content_items.is_empty() {
                                items.push(json!({
                                    "type": "message",
                                    "role": "user",
                                    "content": content_items,
                                }));
                                content_items = Vec::new();
                            }
                            let (call_id, _item_id) = parse_tool_use_id(tool_use_id);
                            if orphaned_call_ids.contains(&call_id) {
                                continue;
                            }
                            items.push(json!({
                                "type": "function_call_output",
                                "call_id": call_id,
                                "output": responses_tool_output(content, content_blocks.as_deref()),
                            }));
                        }
                        _ => {}
                    }
                }
                if !content_items.is_empty() {
                    items.push(json!({
                        "type": "message",
                        "role": "user",
                        "content": content_items,
                    }));
                }
            }
            RolePlacement::Assistant | RolePlacement::InterruptedAssistant => {
                // Tool calls whose producing reasoning is not replayable on
                // this route would ship as orphaned `function_call` items —
                // an automatic 400 on reasoning models. Drop the whole chain
                // instead; the tool outputs are filtered out of the following
                // user/tool messages via `orphaned_call_ids`. Models that
                // never reason legitimately emit bare calls, so the gate only
                // applies to reasoning-capable request models — and only on
                // routes that capture at all: a route whose requests carry no
                // `include` can never satisfy the replay check, so gating
                // there would erase history the route itself accepts.
                let mut chain_dropped = false;
                if replays_encrypted_reasoning
                    && reasoning_capable_model
                    && message_has_tool_calls(msg)
                    && !message_has_replayable_reasoning(
                        msg,
                        reasoning_provider_tag,
                        reasoning_endpoint_fingerprint,
                        fingerprintless_replay_official,
                        &request.model,
                    )
                {
                    chain_dropped = true;
                    dropped_chains += 1;
                    for block in &msg.content {
                        if let ContentBlock::ToolUse { id, .. } = block {
                            let (call_id, _item_id) = parse_tool_use_id(id);
                            orphaned_call_ids.insert(call_id);
                        }
                    }
                }
                for block in &msg.content {
                    match block {
                        ContentBlock::Text { text, .. } => {
                            let text = if placement == RolePlacement::InterruptedAssistant {
                                format!(
                                    "{}{}",
                                    crate::models::INTERRUPTED_ASSISTANT_CONTEXT_PREFIX,
                                    text
                                )
                            } else {
                                text.clone()
                            };
                            items.push(json!({
                                "type": "message",
                                "role": "assistant",
                                "content": [{
                                    "type": "output_text",
                                    "text": text,
                                }],
                            }));
                        }
                        ContentBlock::ToolUse {
                            id, name, input, ..
                        } => {
                            if chain_dropped {
                                continue;
                            }
                            let (call_id, _item_id) = parse_tool_use_id(id);
                            items.push(json!({
                                "type": "function_call",
                                "call_id": call_id,
                                "name": to_api_tool_name(name),
                                "arguments": serde_json::to_string(input).unwrap_or_default(),
                            }));
                        }
                        ContentBlock::Thinking {
                            thinking,
                            state,
                            redacted_data,
                            ..
                        } => {
                            if chain_dropped || redacted_data.is_some() {
                                // Redacted Anthropic payloads have no
                                // Responses representation; skip rather than
                                // emit an empty reasoning item.
                                continue;
                            }
                            if let Some(state) = state {
                                // Endpoint-scoped replay: the tag must match
                                // the endpoint this request is bound to
                                // (provider slug, plus the table identity for
                                // Custom), alongside api shape, exact model,
                                // and the endpoint fingerprint — a table whose
                                // base_url was edited stops replaying the
                                // previous endpoint's blobs. Legacy states
                                // minted before fingerprints existed carry no
                                // proof of which endpoint produced them:
                                // Custom endpoints can move under a stable
                                // tag, so those fail closed, and a built-in
                                // keeps replaying only while the client still
                                // points at the provider's official endpoint
                                // (`fingerprintless_replay_official` above) —
                                // a re-pointed client gets no such credit.
                                let endpoint_matches = match &state.endpoint {
                                    None => {
                                        !is_custom_reasoning_tag(&state.provider)
                                            && fingerprintless_replay_official
                                    }
                                    Some(captured) => captured == reasoning_endpoint_fingerprint,
                                };
                                if replays_encrypted_reasoning
                                    && state.provider == reasoning_provider_tag
                                    && state.api == "openai-responses"
                                    && state.model == request.model
                                    && endpoint_matches
                                {
                                    let mut item = json!({
                                        "type": "reasoning",
                                        "summary": [],
                                        "encrypted_content": state.encrypted_content,
                                    });
                                    if let Some(id) = state.id.as_ref() {
                                        item["id"] = json!(id);
                                    }
                                    items.push(item);
                                }
                            } else if is_deepseek {
                                items.push(json!({
                                    "type": "reasoning",
                                    "content": [{
                                        "type": "reasoning_text",
                                        "text": thinking,
                                    }],
                                }));
                            }
                        }
                        _ => {}
                    }
                }
            }
            // `System` and `Developer` are typed placements for load-bearing
            // in-history context. `Omitted` also receives compatible transcript
            // spellings that predate the closed Role enum; preserve the
            // representable `tool`, `system`, and `developer` wire shapes
            // instead of silently deleting them.
            RolePlacement::System | RolePlacement::Developer | RolePlacement::Omitted => {
                match msg.role.as_str() {
                    "tool" => {
                        for block in &msg.content {
                            if let ContentBlock::ToolResult {
                                tool_use_id,
                                content,
                                content_blocks,
                                ..
                            } = block
                            {
                                let (call_id, _item_id) = parse_tool_use_id(tool_use_id);
                                if orphaned_call_ids.contains(&call_id) {
                                    // The call was chain-dropped with its
                                    // reasoning; shipping the bare output is
                                    // another pairing 400.
                                    continue;
                                }
                                items.push(json!({
                                    "type": "function_call_output",
                                    "call_id": call_id,
                                    "output": responses_tool_output(
                                        content,
                                        content_blocks.as_deref(),
                                    ),
                                }));
                            }
                        }
                    }
                    role @ ("system" | "developer") => {
                        let content_items: Vec<Value> = msg
                            .content
                            .iter()
                            .filter_map(|block| match block {
                                ContentBlock::Text { text, .. } => Some(json!({
                                    "type": "input_text",
                                    "text": text,
                                })),
                                _ => None,
                            })
                            .collect();
                        if !content_items.is_empty() {
                            items.push(json!({
                                "type": "message",
                                "role": role,
                                "content": content_items,
                            }));
                        }
                    }
                    other => {
                        logging::warn(format!(
                            "Responses adapter dropped a message with unsupported role {other:?}"
                        ));
                    }
                }
            }
            // The outbound seam refuses rejected pairs before body building;
            // keeping this arm empty is fail-closed defense in depth.
            RolePlacement::Rejected => {}
        }
    }

    if dropped_chains > 0 {
        // One summary per request instead of one line per affected message:
        // stale history re-fires the condition on every subsequent request
        // until compaction retires the turns, and per-message warns would
        // spam verbose logs for a single root cause.
        logging::warn(format!(
            "Responses replay dropped {dropped_chains} assistant tool chain(s) whose encrypted reasoning is unavailable for this route/model/endpoint (captured on a different model, endpoint tag or fingerprint, or by an older build); their tool calls and outputs are omitted to keep the request valid"
        ));
    }

    if !is_deepseek {
        // A `reasoning` item needs the item it produced to follow it (its
        // function_call, or the assistant message for text output) — the
        // pairing validator rejects a reasoning item whose following item is
        // missing ("was provided without its required following item"). An
        // orphaned reasoning item — in practice a turn that was interrupted
        // before producing any output — is dropped here rather than shipped
        // to fail the whole request.
        let mut keep = vec![true; items.len()];
        let mut dropped_orphans: usize = 0;
        for (index, item) in items.iter().enumerate() {
            if item.get("type").and_then(Value::as_str) != Some("reasoning") {
                continue;
            }
            let followed_by_produced_item = items.get(index + 1).is_some_and(|next| {
                next.get("type").and_then(Value::as_str) == Some("function_call")
                    || (next.get("type").and_then(Value::as_str) == Some("message")
                        && next.get("role").and_then(Value::as_str) == Some("assistant"))
            });
            if !followed_by_produced_item {
                keep[index] = false;
                dropped_orphans += 1;
            }
        }
        let mut paired = Vec::with_capacity(items.len());
        for (index, item) in items.into_iter().enumerate() {
            if keep[index] {
                paired.push(item);
            }
        }
        if dropped_orphans > 0 {
            logging::warn(format!(
                "Responses replay dropped {dropped_orphans} orphaned reasoning item(s) with no following output item"
            ));
        }
        items = paired;
    }

    items
}

/// Convert a CodeWhale tool definition to a Responses API function tool.
fn tool_to_responses_function(tool: &Tool) -> Value {
    let mut parameters = tool.input_schema.clone();
    let constraint_note = schema_sanitize::sanitize_for_responses(&mut parameters);
    let description = match constraint_note {
        Some(note) if tool.description.trim().is_empty() => note,
        Some(note) => format!("{}\n\n{}", tool.description.trim(), note),
        None => tool.description.clone(),
    };
    json!({
        "type": "function",
        "name": to_api_tool_name(&tool.name),
        "description": description,
        "parameters": parameters,
        "strict": false,
    })
}

fn codex_responses_reasoning_effort(raw: &str) -> Option<&'static str> {
    match raw.trim().to_ascii_lowercase().as_str() {
        "off" | "disabled" | "none" | "false" => Some("low"),
        "minimal" => Some("low"),
        "low" => Some("low"),
        "high" => Some("high"),
        "xhigh" | "max" | "maximum" | "ultra" | "ultracode" => Some("xhigh"),
        _ => Some("medium"),
    }
}

/// DeepSeek's Responses wire spelling of the shared tier table
/// (`client::deepseek_effort`), which is the single annotated source for the
/// tier ladder — including the documented `"none"` off tier, so the picker's
/// Off entry stays off instead of collapsing into a still-thinking low.
///
/// Unlike the Chat wire, this endpoint must send *some* documented label once
/// an effort is requested at all, so unknown/automatic tiers normalize to the
/// table's default tier rather than writing nothing.
pub(super) fn responses_reasoning_effort(raw: &str, is_deepseek: bool) -> Option<&'static str> {
    if !is_deepseek {
        return codex_responses_reasoning_effort(raw);
    }
    Some(super::deepseek_effort::deepseek_effort_tier_or_default(raw).responses_effort())
}

fn responses_event_error_details(event: &Value) -> (String, String) {
    let event_type = string_at(event, "/type").unwrap_or("error");
    let code = first_string_at(
        event,
        &[
            "/code",
            "/error/code",
            "/response/error/code",
            "/response/incomplete_details/reason",
            "/response/status",
        ],
    )
    .unwrap_or("unknown");
    let message = first_string_at(
        event,
        &[
            "/message",
            "/error/message",
            "/response/error/message",
            "/response/incomplete_details/reason",
        ],
    )
    .map_or_else(
        || format!("{event_type} event received"),
        |message| {
            if message == code && event_type == "response.incomplete" {
                format!("response incomplete: {message}")
            } else {
                message.to_string()
            }
        },
    );
    (code.to_string(), message)
}

fn responses_stop_reason(response: &Value, saw_tool_call: bool) -> String {
    match string_at(response, "/status").unwrap_or("completed") {
        "completed" if saw_tool_call => "tool_use".to_string(),
        "completed" => "end_turn".to_string(),
        "incomplete" => format!(
            "incomplete:{}",
            string_at(response, "/incomplete_details/reason").unwrap_or("max_tokens")
        ),
        _ => "end_turn".to_string(),
    }
}

fn first_string_at<'a>(value: &'a Value, paths: &[&str]) -> Option<&'a str> {
    paths.iter().find_map(|path| string_at(value, path))
}

fn string_at<'a>(value: &'a Value, path: &str) -> Option<&'a str> {
    value.pointer(path).and_then(Value::as_str).and_then(|s| {
        let trimmed = s.trim();
        (!trimmed.is_empty()).then_some(trimmed)
    })
}

/// Parse a composite tool_use_id back to (call_id, item_id).
/// Composite format: "call_id|item_id"
fn parse_tool_use_id(id: &str) -> (String, String) {
    if let Some(pipe_pos) = id.find('|') {
        (id[..pipe_pos].to_string(), id[pipe_pos + 1..].to_string())
    } else {
        (id.to_string(), String::new())
    }
}

/// Parse usage from a Responses API usage object.
fn parse_responses_usage(val: &Value) -> Usage {
    let input = val
        .get("input_tokens")
        .and_then(|v| v.as_u64())
        .map_or(0, super::saturating_u32);
    let output = val
        .get("output_tokens")
        .and_then(|v| v.as_u64())
        .map_or(0, super::saturating_u32);
    // Cache telemetry arrives in two dialects. DeepSeek's Responses payload
    // uses the same top-level `prompt_cache_hit_tokens` /
    // `prompt_cache_miss_tokens` fields as its Chat-Completions endpoint,
    // while OpenAI-style payloads nest `cached_tokens` under
    // `input_tokens_details`. Prefer the top-level hit, falling back to the
    // nested form when the payload only reports that.
    let nested_cached_tokens = val
        .get("input_tokens_details")
        .and_then(|d| d.get("cached_tokens"))
        .and_then(|v| v.as_u64());
    let prompt_cache_hit_tokens = val
        .get("prompt_cache_hit_tokens")
        .and_then(|v| v.as_u64())
        .or(nested_cached_tokens)
        .map(super::saturating_u32);
    // DeepSeek reports the miss explicitly; otherwise mirror the
    // Chat-Completions parser: derive the miss as input minus the cached hit
    // when the payload reported cached input tokens. Responses nests
    // reasoning under `output_tokens_details` (not `completion_tokens_details`).
    let prompt_cache_miss_tokens = val
        .get("prompt_cache_miss_tokens")
        .and_then(|v| v.as_u64())
        .map(super::saturating_u32)
        .or_else(|| prompt_cache_hit_tokens.map(|hit| input.saturating_sub(hit)));
    // Cache-creation tokens, kept as their own class so pricing can apply the
    // write rate where the provider publishes one. DeepSeek-style payloads
    // nest these under `input_tokens_details`; accept a top-level spelling
    // too for providers that flatten the object.
    let prompt_cache_write_tokens = val
        .get("prompt_cache_write_tokens")
        .and_then(|v| v.as_u64())
        .or_else(|| {
            val.get("input_tokens_details")
                .and_then(|d| d.get("cache_write_tokens"))
                .and_then(|v| v.as_u64())
        })
        .map(super::saturating_u32);
    // `output_tokens` is already the total billable completion count, with
    // reasoning a subset of it. A payload reporting more reasoning than output
    // violates that, so the value is rejected as invalid telemetry rather than
    // being trusted or turned into extra billable output (#4318).
    let reasoning_tokens = val
        .get("output_tokens_details")
        .and_then(|d| d.get("reasoning_tokens"))
        .and_then(|v| v.as_u64())
        .map(super::saturating_u32)
        .filter(|reasoning| *reasoning <= output);
    // `input_tokens` stays the provider-reported *total* (cache-hit + miss +
    // write + uncategorized): `token_usage_for_pricing` partitions it into
    // billable classes and the context budget measures the window with it.
    // Reducing it here to miss-only would double-subtract at those surfaces.
    Usage {
        input_tokens: input,
        output_tokens: output,
        prompt_cache_hit_tokens,
        prompt_cache_miss_tokens,
        prompt_cache_write_tokens,
        reasoning_tokens,
        reasoning_replay_tokens: None,
        server_tool_use: None,
    }
}

#[cfg(test)]
mod tests;
