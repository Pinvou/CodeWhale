use std::collections::HashMap;
use std::io;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use axum::body::{Body, Bytes, to_bytes};
use axum::extract::{Path, State};
use axum::http::{Request, Response, StatusCode, header};
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use futures_util::{Stream, StreamExt, stream};
use rand::Rng as _;
use reqwest::Url;
use serde_json::json;
use sha2::{Digest, Sha256};
use tokio::sync::{OnceCell, RwLock};
use tokio::time::sleep;

const MAX_CHAT_BODY_BYTES: usize = 4 * 1024 * 1024;
const IDEMPOTENCY_PREFIX: &str = "pinvou-desktop-";
const LOGICAL_TURN_PREFIX: &str = "pinvou-turn-";
const LOGICAL_TURN_HEADER: &str = "X-Pinvou-Logical-Turn-ID";

#[derive(Clone, Default)]
pub(super) struct ModelGatewayProxy {
    server: Arc<OnceCell<ProxyServer>>,
}

struct ProxyServer {
    origin: String,
    state: ProxyState,
}

#[derive(Clone)]
struct ProxyState {
    client: reqwest::Client,
    registry: Arc<RwLock<ProxyRegistry>>,
}

#[derive(Default)]
struct ProxyRegistry {
    session_routes: HashMap<String, String>,
    targets: HashMap<String, ProxyTarget>,
}

#[derive(Clone)]
struct ProxyTarget {
    upstream: Url,
    gateway_model_id: String,
    turn_generation: u64,
    maximum_output_tokens: u32,
}

impl ModelGatewayProxy {
    pub(super) async fn register_session(
        &self,
        session_id: &str,
        upstream_base_url: &str,
        gateway_model_id: &str,
        maximum_output_tokens: u32,
    ) -> Result<String> {
        let upstream = Url::parse(upstream_base_url)
            .context("account platform model gateway URL is invalid")?;
        if !upstream_is_allowed(&upstream) {
            anyhow::bail!("account platform model gateway must use HTTPS or loopback HTTP");
        }
        let gateway_model_id = gateway_model_id.trim();
        if gateway_model_id.is_empty() {
            anyhow::bail!("account platform gateway model ID is missing");
        }
        let server = self.server.get_or_try_init(start_proxy_server).await?;
        let route = {
            let mut registry = server.state.registry.write().await;
            let route = registry
                .session_routes
                .entry(session_id.to_string())
                .or_insert_with(random_route_token)
                .clone();
            registry
                .targets
                .entry(route.clone())
                .and_modify(|target| {
                    target.upstream = upstream.clone();
                    target.gateway_model_id = gateway_model_id.to_string();
                    target.maximum_output_tokens = maximum_output_tokens;
                })
                .or_insert(ProxyTarget {
                    upstream,
                    gateway_model_id: gateway_model_id.to_string(),
                    turn_generation: 0,
                    maximum_output_tokens,
                });
            route
        };
        Ok(format!("{}/{route}/v1", server.origin))
    }

    pub(super) async fn advance_session_turn(&self, session_id: &str) {
        let Some(server) = self.server.get() else {
            return;
        };
        let mut registry = server.state.registry.write().await;
        let Some(route) = registry.session_routes.get(session_id).cloned() else {
            return;
        };
        if let Some(target) = registry.targets.get_mut(&route) {
            target.turn_generation = target.turn_generation.saturating_add(1);
        }
    }
}

async fn start_proxy_server() -> Result<ProxyServer> {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .context("bind account platform model proxy")?;
    let address = listener
        .local_addr()
        .context("read account platform model proxy address")?;
    let state = ProxyState {
        client: reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(10))
            .build()
            .context("build account platform model proxy client")?,
        registry: Arc::new(RwLock::new(ProxyRegistry::default())),
    };
    let router = Router::new()
        .route("/{route}/v1/chat/completions", post(forward_chat))
        .route("/{route}/v1/models", get(forward_models))
        .with_state(state.clone());
    tokio::spawn(async move {
        if let Err(error) = axum::serve(listener, router).await {
            log::error!("account platform model proxy stopped: {error}");
        }
    });
    Ok(ProxyServer {
        origin: format!("http://{address}"),
        state,
    })
}

async fn forward_chat(
    Path(route): Path<String>,
    State(state): State<ProxyState>,
    request: Request<Body>,
) -> Response<Body> {
    let (parts, body) = request.into_parts();
    let body = match to_bytes(body, MAX_CHAT_BODY_BYTES).await {
        Ok(body) => body,
        Err(_) => return proxy_error(StatusCode::PAYLOAD_TOO_LARGE, "request body is too large"),
    };
    let Some(target) = state.registry.read().await.targets.get(&route).cloned() else {
        return proxy_error(StatusCode::NOT_FOUND, "unknown model proxy route");
    };
    let body = match prepare_chat_body(body, target.maximum_output_tokens, &target.gateway_model_id)
    {
        Ok(body) => body,
        Err(_) => return proxy_error(StatusCode::BAD_REQUEST, "invalid chat request body"),
    };
    let expects_sse = chat_request_expects_sse(&body);
    let idempotency_key = request_idempotency_key(&route, target.turn_generation, &body);
    let logical_turn_id = logical_turn_id(&route, target.turn_generation);
    forward(
        &state,
        target,
        "chat/completions",
        reqwest::Method::POST,
        &parts.headers,
        Some(body),
        Some(&idempotency_key),
        Some(&logical_turn_id),
        expects_sse,
    )
    .await
}

async fn forward_models(
    Path(route): Path<String>,
    State(state): State<ProxyState>,
    request: Request<Body>,
) -> Response<Body> {
    let (parts, _) = request.into_parts();
    forward(
        &state,
        match state.registry.read().await.targets.get(&route).cloned() {
            Some(target) => target,
            None => return proxy_error(StatusCode::NOT_FOUND, "unknown model proxy route"),
        },
        "models",
        reqwest::Method::GET,
        &parts.headers,
        None,
        None,
        None,
        false,
    )
    .await
}

async fn forward(
    state: &ProxyState,
    target: ProxyTarget,
    resource: &str,
    method: reqwest::Method,
    headers: &axum::http::HeaderMap,
    body: Option<Bytes>,
    idempotency_key: Option<&str>,
    logical_turn_id: Option<&str>,
    expects_sse: bool,
) -> Response<Body> {
    let target_url = match Url::parse(&format!(
        "{}/{resource}",
        target.upstream.as_str().trim_end_matches('/')
    )) {
        Ok(target) => target,
        Err(_) => return proxy_error(StatusCode::BAD_GATEWAY, "invalid upstream route"),
    };
    let mut last_error = None;
    let mut upstream_response = None;
    // A transport error can happen after the gateway accepted the request but
    // before response headers reach the desktop. Reusing the same generated
    // Idempotency-Key makes one bounded retry safe for chat requests; GET is
    // naturally idempotent. Application HTTP responses are never retried.
    for attempt in 1..=2 {
        let mut builder = state.client.request(method.clone(), target_url.clone());
        for name in [
            header::AUTHORIZATION,
            header::CONTENT_TYPE,
            header::ACCEPT,
            header::USER_AGENT,
        ] {
            if let Some(value) = headers.get(&name) {
                builder = builder.header(name, value);
            }
        }
        if let Some(value) = headers.get("x-pinvou-client-version") {
            builder = builder.header("x-pinvou-client-version", value);
        }
        if let Some(key) = idempotency_key {
            builder = builder.header("Idempotency-Key", key);
        }
        if let Some(turn_id) = logical_turn_id {
            builder = builder.header(LOGICAL_TURN_HEADER, turn_id);
        }
        if let Some(body) = body.clone() {
            builder = builder.body(body);
        }
        match builder.send().await {
            Ok(response) => {
                upstream_response = Some(response);
                break;
            }
            Err(error) => {
                log::warn!(
                    "account platform model proxy transport failed: attempt={attempt} kind={} error={error}",
                    transport_error_kind(&error)
                );
                last_error = Some(error);
                if attempt == 1 {
                    sleep(Duration::from_millis(150)).await;
                }
            }
        }
    }
    let Some(upstream_response) = upstream_response else {
        let _ = last_error;
        return proxy_error(StatusCode::BAD_GATEWAY, "model gateway request failed");
    };
    let status = upstream_response.status();
    let upstream_headers = upstream_response.headers().clone();
    let upstream_is_sse = upstream_headers
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| {
            value
                .trim_start()
                .to_ascii_lowercase()
                .starts_with("text/event-stream")
        });
    if expects_sse && status == StatusCode::ACCEPTED {
        return proxy_error(
            StatusCode::SERVICE_UNAVAILABLE,
            "model gateway request is still reconciling",
        );
    }
    if expects_sse && status.is_success() && !upstream_is_sse {
        return proxy_error(
            StatusCode::BAD_GATEWAY,
            "model gateway returned a non-stream response",
        );
    }
    let mut response = Response::builder().status(status);
    for name in [
        header::CONTENT_TYPE,
        header::CACHE_CONTROL,
        header::RETRY_AFTER,
    ] {
        if let Some(value) = upstream_headers.get(&name) {
            response = response.header(name, value);
        }
    }
    for name in ["x-request-id", "x-ratelimit-limit", "x-ratelimit-remaining"] {
        if let Some(value) = upstream_headers.get(name) {
            response = response.header(name, value);
        }
    }
    let upstream_stream = upstream_response.bytes_stream();
    let body = if upstream_is_sse {
        Body::from_stream(require_sse_done(upstream_stream))
    } else {
        Body::from_stream(upstream_stream)
    };
    response
        .body(body)
        .unwrap_or_else(|_| proxy_error(StatusCode::BAD_GATEWAY, "invalid gateway response"))
}

fn chat_request_expects_sse(body: &[u8]) -> bool {
    serde_json::from_slice::<serde_json::Value>(body)
        .ok()
        .and_then(|payload| payload.get("stream").and_then(serde_json::Value::as_bool))
        .unwrap_or(false)
}

fn require_sse_done<S, E>(source: S) -> impl Stream<Item = Result<Bytes, io::Error>>
where
    S: Stream<Item = Result<Bytes, E>> + Send + 'static,
    E: std::fmt::Display + Send + 'static,
{
    struct State<S> {
        source: std::pin::Pin<Box<S>>,
        line: Vec<u8>,
        line_overflow: bool,
        saw_done: bool,
        emitted_terminal_error: bool,
    }

    stream::unfold(
        State {
            source: Box::pin(source),
            line: Vec::with_capacity(64),
            line_overflow: false,
            saw_done: false,
            emitted_terminal_error: false,
        },
        |mut state| async move {
            match state.source.next().await {
                Some(Ok(bytes)) => {
                    observe_sse_lines(
                        &mut state.line,
                        &mut state.line_overflow,
                        &mut state.saw_done,
                        &bytes,
                    );
                    Some((Ok(bytes), state))
                }
                Some(Err(error)) => {
                    state.emitted_terminal_error = true;
                    Some((
                        Err(io::Error::other(format!(
                            "account platform stream failed: {error}"
                        ))),
                        state,
                    ))
                }
                None if state.saw_done || state.emitted_terminal_error => None,
                None => {
                    state.emitted_terminal_error = true;
                    Some((
                        Err(io::Error::new(
                            io::ErrorKind::UnexpectedEof,
                            "account platform stream ended before data: [DONE]",
                        )),
                        state,
                    ))
                }
            }
        },
    )
}

fn observe_sse_lines(line: &mut Vec<u8>, overflow: &mut bool, saw_done: &mut bool, bytes: &[u8]) {
    const MAX_TERMINAL_LINE_BYTES: usize = 256;
    for byte in bytes {
        if *byte == b'\n' {
            let candidate = line.strip_suffix(b"\r").unwrap_or(line.as_slice());
            if !*overflow && candidate == b"data: [DONE]" {
                *saw_done = true;
            }
            line.clear();
            *overflow = false;
        } else if line.len() < MAX_TERMINAL_LINE_BYTES {
            line.push(*byte);
        } else {
            *overflow = true;
        }
    }
}

fn transport_error_kind(error: &reqwest::Error) -> &'static str {
    if error.is_timeout() {
        "timeout"
    } else if error.is_connect() {
        "connect"
    } else if error.is_body() {
        "body"
    } else if error.is_request() {
        "request"
    } else {
        "unknown"
    }
}

fn request_idempotency_key(route: &str, turn_generation: u64, body: &[u8]) -> String {
    let mut digest = Sha256::new();
    digest.update(route.as_bytes());
    digest.update([0]);
    digest.update(turn_generation.to_be_bytes());
    digest.update([0]);
    digest.update(body);
    format!(
        "{IDEMPOTENCY_PREFIX}{}",
        crate::platform::encoding::hex_lower(&digest.finalize())
    )
}

fn logical_turn_id(route: &str, turn_generation: u64) -> String {
    let mut digest = Sha256::new();
    digest.update(route.as_bytes());
    digest.update([0]);
    digest.update(turn_generation.to_be_bytes());
    format!(
        "{LOGICAL_TURN_PREFIX}{}",
        crate::platform::encoding::hex_lower(&digest.finalize())
    )
}

fn prepare_chat_body(
    body: Bytes,
    maximum_output_tokens: u32,
    gateway_model_id: &str,
) -> serde_json::Result<Bytes> {
    let mut payload: serde_json::Value = serde_json::from_slice(&body)?;
    let Some(object) = payload.as_object_mut() else {
        return Ok(body);
    };
    object.insert(
        "model".to_string(),
        serde_json::Value::String(gateway_model_id.to_string()),
    );
    for field in ["max_tokens", "max_completion_tokens"] {
        let Some(value) = object.get_mut(field) else {
            continue;
        };
        if value
            .as_u64()
            .is_some_and(|tokens| tokens > u64::from(maximum_output_tokens))
        {
            *value = serde_json::Value::from(maximum_output_tokens);
        }
    }
    serde_json::to_vec(&payload).map(Bytes::from)
}

fn random_route_token() -> String {
    let mut bytes = [0_u8; 24];
    rand::rng().fill_bytes(&mut bytes);
    URL_SAFE_NO_PAD.encode(bytes)
}

fn upstream_is_allowed(url: &Url) -> bool {
    url.scheme() == "https"
        || (url.scheme() == "http"
            && matches!(url.host_str(), Some("127.0.0.1" | "localhost" | "::1")))
}

fn proxy_error(status: StatusCode, message: &'static str) -> Response<Body> {
    (status, Json(json!({ "error": { "message": message } }))).into_response()
}

#[cfg(test)]
mod tests {
    use std::io;
    use std::sync::Arc;

    use axum::Router;
    use axum::body::Bytes;
    use axum::extract::State;
    use axum::http::{HeaderMap, StatusCode, header};
    use axum::routing::post;
    use futures_util::{StreamExt, stream};
    use tokio::sync::Mutex;

    use super::{
        ModelGatewayProxy, logical_turn_id, prepare_chat_body, request_idempotency_key,
        require_sse_done,
    };

    #[tokio::test]
    async fn sse_guard_requires_explicit_done_terminal() {
        let complete = stream::iter(vec![
            Ok::<_, io::Error>(Bytes::from_static(b"data: {\"choices\":[]}\n\n")),
            Ok(Bytes::from_static(b"data: [DO")),
            Ok(Bytes::from_static(b"NE]\r\n\r\n")),
        ]);
        let complete = require_sse_done(complete).collect::<Vec<_>>().await;
        assert!(complete.iter().all(Result::is_ok));

        let incomplete = stream::iter(vec![Ok::<_, io::Error>(Bytes::from_static(
            b"data: {\"choices\":[]}\n\n",
        ))]);
        let incomplete = require_sse_done(incomplete).collect::<Vec<_>>().await;
        assert_eq!(incomplete.len(), 2);
        assert!(incomplete[0].is_ok());
        assert_eq!(
            incomplete[1].as_ref().unwrap_err().kind(),
            io::ErrorKind::UnexpectedEof
        );
    }

    #[test]
    fn idempotency_key_is_stable_bounded_and_scoped_to_session_route() {
        let first = request_idempotency_key("session-route-a", 1, br#"{"model":"m"}"#);
        let retry = request_idempotency_key("session-route-a", 1, br#"{"model":"m"}"#);
        let next_step = request_idempotency_key("session-route-a", 1, br#"{"model":"m","step":2}"#);
        let next_turn = request_idempotency_key("session-route-a", 2, br#"{"model":"m"}"#);
        let other_session = request_idempotency_key("session-route-b", 1, br#"{"model":"m"}"#);

        assert_eq!(first, retry);
        assert_ne!(first, next_step);
        assert_ne!(first, next_turn);
        assert_ne!(first, other_session);
        assert!((16..=128).contains(&first.len()));
        assert!(
            first
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        );
    }

    #[test]
    fn logical_turn_id_is_stable_within_a_turn_and_rotates_between_turns() {
        let first = logical_turn_id("session-route-a", 1);
        assert_eq!(first, logical_turn_id("session-route-a", 1));
        assert_ne!(first, logical_turn_id("session-route-a", 2));
        assert_ne!(first, logical_turn_id("session-route-b", 1));
        assert!((16..=128).contains(&first.len()));
    }

    #[test]
    fn output_limit_is_clamped_without_expanding_smaller_requests() {
        let clamped = prepare_chat_body(
            Bytes::from_static(
                br#"{"model":"deepseek-v4-flash","max_tokens":8192,"messages":[{"role":"assistant","content":"","reasoning_content":"kept reasoning","tool_calls":[]}]}"#,
            ),
            4096,
            "catalog-model-id",
        )
        .expect("clamped request");
        let clamped: serde_json::Value = serde_json::from_slice(&clamped).unwrap();
        assert_eq!(clamped["max_tokens"], 4096);
        assert_eq!(clamped["model"], "catalog-model-id");
        assert_eq!(
            clamped["messages"][0]["reasoning_content"],
            "kept reasoning"
        );

        let smaller = Bytes::from_static(br#"{"max_completion_tokens":1024,"messages":[]}"#);
        let smaller = prepare_chat_body(smaller, 4096, "stable-model-id").expect("smaller request");
        let smaller: serde_json::Value = serde_json::from_slice(&smaller).unwrap();
        assert_eq!(smaller["max_completion_tokens"], 1024);
        assert_eq!(smaller["model"], "stable-model-id");
    }

    #[tokio::test]
    async fn proxy_injects_stable_key_and_preserves_sse_response() {
        let captures = Arc::new(Mutex::new(
            Vec::<(String, String, String, u64, String)>::new(),
        ));
        let upstream = Router::new()
            .route(
                "/v1/chat/completions",
                post(
                    |State(captures): State<
                        Arc<Mutex<Vec<(String, String, String, u64, String)>>>,
                    >,
                     headers: HeaderMap,
                     body: Bytes| async move {
                        let key = headers
                            .get("idempotency-key")
                            .and_then(|value| value.to_str().ok())
                            .unwrap_or_default()
                            .to_string();
                        let authorization = headers
                            .get(header::AUTHORIZATION)
                            .and_then(|value| value.to_str().ok())
                            .unwrap_or_default()
                            .to_string();
                        let logical_turn = headers
                            .get("x-pinvou-logical-turn-id")
                            .and_then(|value| value.to_str().ok())
                            .unwrap_or_default()
                            .to_string();
                        let payload = serde_json::from_slice::<serde_json::Value>(&body)
                            .expect("forwarded JSON body");
                        let maximum_output = payload
                            .get("max_tokens")
                            .and_then(|value| value.as_u64())
                            .unwrap_or_default();
                        let model = payload
                            .get("model")
                            .and_then(|value| value.as_str())
                            .unwrap_or_default()
                            .to_string();
                        captures.lock().await.push((
                            key,
                            authorization,
                            logical_turn,
                            maximum_output,
                            model,
                        ));
                        (
                            [(header::CONTENT_TYPE, "text/event-stream")],
                            "data: [DONE]\n\n",
                        )
                    },
                ),
            )
            .with_state(captures.clone());
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind upstream");
        let upstream_address = listener.local_addr().expect("upstream address");
        tokio::spawn(async move {
            axum::serve(listener, upstream)
                .await
                .expect("serve upstream");
        });

        let proxy = ModelGatewayProxy::default();
        let base_url = proxy
            .register_session(
                "session-a",
                &format!("http://{upstream_address}/v1"),
                "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c",
                4096,
            )
            .await
            .expect("register proxy route");
        proxy.advance_session_turn("session-a").await;
        let client = reqwest::Client::new();
        for (index, body) in [
            r#"{"model":"m","stream":true,"max_tokens":8192}"#,
            r#"{"model":"m","stream":true,"max_tokens":8192}"#,
            r#"{"model":"m","stream":true,"max_tokens":8192}"#,
            r#"{"model":"m","stream":true,"max_tokens":8192,"step":2}"#,
        ]
        .into_iter()
        .enumerate()
        {
            if index == 2 {
                let next_turn_base = proxy
                    .register_session(
                        "session-a",
                        &format!("http://{upstream_address}/v1"),
                        "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c",
                        4096,
                    )
                    .await
                    .expect("advance proxy turn");
                assert_eq!(next_turn_base, base_url);
                proxy.advance_session_turn("session-a").await;
            }
            let response = client
                .post(format!("{base_url}/chat/completions"))
                .bearer_auth("oidc-access-token")
                .header(header::CONTENT_TYPE, "application/json")
                .body(body)
                .send()
                .await
                .expect("proxy request");
            assert_eq!(response.status(), reqwest::StatusCode::OK);
            assert_eq!(
                response
                    .headers()
                    .get(header::CONTENT_TYPE)
                    .and_then(|value| value.to_str().ok()),
                Some("text/event-stream")
            );
            assert_eq!(response.text().await.expect("SSE body"), "data: [DONE]\n\n");
        }
        let captures = captures.lock().await;
        assert_eq!(captures.len(), 4);
        assert_eq!(captures[0].0, captures[1].0);
        assert_ne!(captures[0].0, captures[2].0);
        assert_ne!(captures[2].0, captures[3].0);
        assert_eq!(captures[0].2, captures[1].2);
        assert_ne!(captures[0].2, captures[2].2);
        assert_eq!(captures[2].2, captures[3].2);
        assert!((16..=128).contains(&captures[0].0.len()));
        assert!(
            captures
                .iter()
                .all(
                    |(_, authorization, logical_turn, maximum_output, model)| authorization
                        == "Bearer oidc-access-token"
                        && logical_turn.starts_with("pinvou-turn-")
                        && *maximum_output == 4096
                        && model == "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c"
                )
        );
    }

    #[tokio::test]
    async fn streaming_request_rejects_successful_non_sse_responses() {
        let upstream = Router::new().route(
            "/v1/chat/completions",
            post(|body: Bytes| async move {
                let payload: serde_json::Value =
                    serde_json::from_slice(&body).expect("forwarded JSON body");
                let status = if matches!(payload["case"].as_str(), Some("pending" | "pending-sse"))
                {
                    StatusCode::ACCEPTED
                } else {
                    StatusCode::OK
                };
                let content_type = if payload["case"] == "pending-sse" {
                    "text/event-stream"
                } else {
                    "application/json"
                };
                (
                    status,
                    [(header::CONTENT_TYPE, content_type)],
                    r#"{"code":"USAGE_REVIEW"}"#,
                )
            }),
        );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind upstream");
        let upstream_address = listener.local_addr().expect("upstream address");
        tokio::spawn(async move {
            axum::serve(listener, upstream)
                .await
                .expect("serve upstream");
        });

        let proxy = ModelGatewayProxy::default();
        let base_url = proxy
            .register_session(
                "non-sse-session",
                &format!("http://{upstream_address}/v1"),
                "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c",
                4096,
            )
            .await
            .expect("register proxy route");
        let client = reqwest::Client::new();
        for (case, stream, expected_status, expected_message) in [
            (
                "pending",
                true,
                reqwest::StatusCode::SERVICE_UNAVAILABLE,
                "still reconciling",
            ),
            (
                "pending-sse",
                true,
                reqwest::StatusCode::SERVICE_UNAVAILABLE,
                "still reconciling",
            ),
            (
                "invalid-success",
                true,
                reqwest::StatusCode::BAD_GATEWAY,
                "non-stream response",
            ),
            (
                "ordinary-json",
                false,
                reqwest::StatusCode::OK,
                "USAGE_REVIEW",
            ),
        ] {
            let response = client
                .post(format!("{base_url}/chat/completions"))
                .header(header::CONTENT_TYPE, "application/json")
                .body(format!(
                    r#"{{"model":"m","stream":{stream},"case":"{case}"}}"#
                ))
                .send()
                .await
                .expect("proxy request");
            assert_eq!(response.status(), expected_status);
            assert!(
                response
                    .text()
                    .await
                    .expect("proxy response body")
                    .contains(expected_message)
            );
        }
    }
}
