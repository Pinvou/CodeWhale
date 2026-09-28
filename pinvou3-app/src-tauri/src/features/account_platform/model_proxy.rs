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
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::{Digest, Sha256};
use tauri::{AppHandle, Emitter};
use tokio::sync::{OnceCell, RwLock};
use tokio::time::sleep;

const MAX_CHAT_BODY_BYTES: usize = 4 * 1024 * 1024;
const IDEMPOTENCY_PREFIX: &str = "pinvou-desktop-";
const LOGICAL_TURN_PREFIX: &str = "pinvou-turn-";
const LOGICAL_TURN_HEADER: &str = "X-Pinvou-Logical-Turn-ID";
const CLIENT_CAPABILITIES_HEADER: &str = "X-Pinvou-Client-Capabilities";
const BILLING_NOTICE_CAPABILITY: &str = "billing-notice-v1";
const BILLING_NOTICE_EVENT: &str = "chat:billing_notice";

#[derive(Clone, Default)]
pub(super) struct ModelGatewayProxy {
    server: Arc<OnceCell<ProxyServer>>,
    app: Option<AppHandle>,
}

struct ProxyServer {
    origin: String,
    state: ProxyState,
}

#[derive(Clone)]
struct ProxyState {
    client: reqwest::Client,
    registry: Arc<RwLock<ProxyRegistry>>,
    app: Option<AppHandle>,
}

#[derive(Default)]
struct ProxyRegistry {
    session_routes: HashMap<String, String>,
    targets: HashMap<String, ProxyTarget>,
}

#[derive(Clone)]
struct ProxyTarget {
    session_id: String,
    upstream: Url,
    gateway_model_id: String,
    turn_generation: u64,
    maximum_output_tokens: u32,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
struct BillingNotice {
    schema_version: u32,
    code: String,
    stage: String,
    turn_outcome: String,
    settlement_status: String,
    action: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    context_type: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    organization_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    usage_request_id: String,
}

impl BillingNotice {
    fn is_valid(&self) -> bool {
        self.schema_version == 1
            && match self.code.as_str() {
                "WALLET_INSUFFICIENT" => match (self.context_type.as_str(), self.action.as_str()) {
                    ("PERSONAL", "OPEN_RECHARGE") => self.organization_id.is_empty(),
                    ("ORGANIZATION", "CONTACT_ORG_ADMIN")
                    | ("ORGANIZATION", "OPEN_ORGANIZATION_RECHARGE") => {
                        uuid::Uuid::parse_str(&self.organization_id).is_ok()
                    }
                    _ => false,
                },
                "MEMBER_QUOTA_INSUFFICIENT" => {
                    self.context_type == "ORGANIZATION"
                        && self.action == "CONTACT_ORG_ADMIN"
                        && uuid::Uuid::parse_str(&self.organization_id).is_ok()
                }
                _ => false,
            }
            && uuid::Uuid::parse_str(&self.usage_request_id).is_ok()
    }
}

#[derive(Clone)]
struct BillingNoticeEmitter {
    app: AppHandle,
    session_id: String,
    turn_generation: u64,
}

impl BillingNoticeEmitter {
    fn emit(&self, notice: BillingNotice) {
        if let Ok(persisted) = serde_json::to_value(&notice) {
            crate::features::assistant::timing::record_billing_notice(&self.session_id, persisted);
        }
        let _ = self.app.emit(
            BILLING_NOTICE_EVENT,
            json!({
                "session_id": self.session_id,
                "turn_generation": self.turn_generation,
                "billing_notice": notice,
            }),
        );
    }
}

impl ModelGatewayProxy {
    pub(super) fn new(app: AppHandle) -> Self {
        Self {
            server: Arc::new(OnceCell::new()),
            app: Some(app),
        }
    }

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
        let server = self
            .server
            .get_or_try_init(|| start_proxy_server(self.app.clone()))
            .await?;
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
                    target.session_id = session_id.to_string();
                    target.upstream = upstream.clone();
                    target.gateway_model_id = gateway_model_id.to_string();
                    target.maximum_output_tokens = maximum_output_tokens;
                })
                .or_insert(ProxyTarget {
                    session_id: session_id.to_string(),
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

async fn start_proxy_server(app: Option<AppHandle>) -> Result<ProxyServer> {
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
        app,
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
        builder = builder.header(CLIENT_CAPABILITIES_HEADER, BILLING_NOTICE_CAPABILITY);
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
    let notice_emitter = state.app.clone().map(|app| BillingNoticeEmitter {
        app,
        session_id: target.session_id.clone(),
        turn_generation: target.turn_generation,
    });
    if let Some(notice) = billing_notice_from_headers(&upstream_headers) {
        if let Some(emitter) = &notice_emitter {
            emitter.emit(notice);
        }
    }
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
    if status == StatusCode::PAYMENT_REQUIRED {
        let payload = match upstream_response.bytes().await {
            Ok(payload) => payload,
            Err(_) => return proxy_error(StatusCode::BAD_GATEWAY, "invalid gateway response"),
        };
        if let Some(notice) = billing_notice_from_body(&payload) {
            if let Some(emitter) = &notice_emitter {
                emitter.emit(notice);
            }
        }
        return response
            .body(Body::from(payload))
            .unwrap_or_else(|_| proxy_error(StatusCode::BAD_GATEWAY, "invalid gateway response"));
    }
    let upstream_stream = upstream_response.bytes_stream();
    let body = if upstream_is_sse {
        Body::from_stream(require_sse_done_with_notice(
            upstream_stream,
            notice_emitter,
        ))
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
    require_sse_done_with_notice(source, None)
}

fn require_sse_done_with_notice<S, E>(
    source: S,
    notice_emitter: Option<BillingNoticeEmitter>,
) -> impl Stream<Item = Result<Bytes, io::Error>>
where
    S: Stream<Item = Result<Bytes, E>> + Send + 'static,
    E: std::fmt::Display + Send + 'static,
{
    struct State<S> {
        source: std::pin::Pin<Box<S>>,
        line: Vec<u8>,
        line_overflow: bool,
        saw_done: bool,
        saw_terminal_error: bool,
        emitted_terminal_error: bool,
        event_name: String,
        notice_emitter: Option<BillingNoticeEmitter>,
    }

    stream::unfold(
        State {
            source: Box::pin(source),
            line: Vec::with_capacity(64),
            line_overflow: false,
            saw_done: false,
            saw_terminal_error: false,
            emitted_terminal_error: false,
            event_name: String::new(),
            notice_emitter,
        },
        |mut state| async move {
            match state.source.next().await {
                Some(Ok(bytes)) => {
                    let notices = observe_sse_lines(
                        &mut state.line,
                        &mut state.line_overflow,
                        &mut state.saw_done,
                        &mut state.saw_terminal_error,
                        &mut state.event_name,
                        &bytes,
                    );
                    if let Some(emitter) = &state.notice_emitter {
                        for notice in notices {
                            emitter.emit(notice);
                        }
                    }
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
                None if state.saw_done
                    || state.saw_terminal_error
                    || state.emitted_terminal_error =>
                {
                    None
                }
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

fn observe_sse_lines(
    line: &mut Vec<u8>,
    overflow: &mut bool,
    saw_done: &mut bool,
    saw_terminal_error: &mut bool,
    event_name: &mut String,
    bytes: &[u8],
) -> Vec<BillingNotice> {
    const MAX_TERMINAL_LINE_BYTES: usize = 4096;
    let mut notices = Vec::new();
    for byte in bytes {
        if *byte == b'\n' {
            let candidate = line.strip_suffix(b"\r").unwrap_or(line.as_slice());
            if !*overflow {
                if candidate.is_empty() {
                    event_name.clear();
                } else if let Some(value) = candidate.strip_prefix(b"event:") {
                    *event_name = String::from_utf8_lossy(value).trim().to_ascii_lowercase();
                } else if let Some(value) = candidate.strip_prefix(b"data:") {
                    let data = value.strip_prefix(b" ").unwrap_or(value);
                    if data == b"[DONE]" {
                        *saw_done = true;
                    } else if event_name == "billing_notice" {
                        if let Ok(notice) = serde_json::from_slice::<BillingNotice>(data) {
                            if notice.is_valid() {
                                notices.push(notice);
                            }
                        }
                    } else if serde_json::from_slice::<serde_json::Value>(data)
                        .ok()
                        .is_some_and(|value| value.get("error").is_some())
                    {
                        *saw_terminal_error = true;
                    }
                }
            }
            line.clear();
            *overflow = false;
        } else if line.len() < MAX_TERMINAL_LINE_BYTES {
            line.push(*byte);
        } else {
            *overflow = true;
        }
    }
    notices
}

fn billing_notice_from_headers(headers: &reqwest::header::HeaderMap) -> Option<BillingNotice> {
    let value = |name: &str| {
        headers
            .get(name)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .trim()
            .to_string()
    };
    let code = value("x-pinvou-billing-code");
    if code != "WALLET_INSUFFICIENT" && code != "MEMBER_QUOTA_INSUFFICIENT" {
        return None;
    }
    let notice = BillingNotice {
        schema_version: 1,
        code,
        stage: value("x-pinvou-billing-stage"),
        turn_outcome: value("x-pinvou-billing-turn-outcome"),
        settlement_status: value("x-pinvou-billing-settlement-status"),
        action: value("x-pinvou-billing-action"),
        context_type: value("x-pinvou-billing-context-type"),
        organization_id: value("x-pinvou-billing-organization-id"),
        usage_request_id: value("x-pinvou-billing-usage-request-id"),
    };
    notice.is_valid().then_some(notice)
}

fn billing_notice_from_body(body: &[u8]) -> Option<BillingNotice> {
    let payload = serde_json::from_slice::<serde_json::Value>(body).ok()?;
    let notice = payload.get("billing_notice")?;
    let notice = serde_json::from_value::<BillingNotice>(notice.clone()).ok()?;
    notice.is_valid().then_some(notice)
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
        ModelGatewayProxy, billing_notice_from_body, billing_notice_from_headers, logical_turn_id,
        observe_sse_lines, prepare_chat_body, request_idempotency_key, require_sse_done,
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

        let explicit_error = stream::iter(vec![Ok::<_, io::Error>(Bytes::from_static(
            b"event: error\ndata: {\"error\":{\"code\":\"SETTLEMENT_PENDING\"}}\n\n",
        ))]);
        let explicit_error = require_sse_done(explicit_error).collect::<Vec<_>>().await;
        assert_eq!(explicit_error.len(), 1);
        assert!(explicit_error[0].is_ok());
    }

    #[test]
    fn billing_notice_is_parsed_only_from_the_versioned_contract() {
        let body = br#"{"code":"WALLET_INSUFFICIENT","billing_notice":{"schema_version":1,"code":"WALLET_INSUFFICIENT","stage":"INITIAL_RESERVATION","turn_outcome":"REJECTED","settlement_status":"NOT_STARTED","action":"OPEN_RECHARGE","context_type":"PERSONAL","usage_request_id":"0199a100-0001-7000-8000-000000000106"}}"#;
        let notice = billing_notice_from_body(body).expect("structured body notice");
        assert_eq!(notice.stage, "INITIAL_RESERVATION");
        assert_eq!(notice.turn_outcome, "REJECTED");

        let mut headers = HeaderMap::new();
        headers.insert(
            "x-pinvou-billing-code",
            "WALLET_INSUFFICIENT".parse().unwrap(),
        );
        headers.insert(
            "x-pinvou-billing-stage",
            "ADDITIONAL_RESERVATION".parse().unwrap(),
        );
        headers.insert(
            "x-pinvou-billing-turn-outcome",
            "COMPLETED".parse().unwrap(),
        );
        headers.insert(
            "x-pinvou-billing-settlement-status",
            "SETTLED".parse().unwrap(),
        );
        headers.insert("x-pinvou-billing-action", "OPEN_RECHARGE".parse().unwrap());
        headers.insert("x-pinvou-billing-context-type", "PERSONAL".parse().unwrap());
        headers.insert(
            "x-pinvou-billing-usage-request-id",
            "0199a100-0001-7000-8000-000000000106".parse().unwrap(),
        );
        let notice = billing_notice_from_headers(&headers).expect("structured header notice");
        assert_eq!(notice.settlement_status, "SETTLED");
        assert_eq!(notice.action, "OPEN_RECHARGE");

        let organization = billing_notice_from_body(br#"{"code":"WALLET_INSUFFICIENT","billing_notice":{"schema_version":1,"code":"WALLET_INSUFFICIENT","stage":"INITIAL_RESERVATION","turn_outcome":"REJECTED","settlement_status":"NOT_STARTED","action":"OPEN_ORGANIZATION_RECHARGE","context_type":"ORGANIZATION","organization_id":"0199a100-0001-7000-8000-000000000201","usage_request_id":"0199a100-0001-7000-8000-000000000106"}}"#).expect("organization purchase notice");
        assert_eq!(organization.action, "OPEN_ORGANIZATION_RECHARGE");
        assert_eq!(
            organization.organization_id,
            "0199a100-0001-7000-8000-000000000201"
        );

        assert!(billing_notice_from_body(br#"{"message":"insufficient balance"}"#).is_none());
        assert!(billing_notice_from_body(br#"{"billing_notice":{"schema_version":1,"code":"WALLET_INSUFFICIENT","stage":"INITIAL_RESERVATION","turn_outcome":"REJECTED","settlement_status":"NOT_STARTED","action":"OPEN_RECHARGE","context_type":"ORGANIZATION","usage_request_id":"0199a100-0001-7000-8000-000000000106"}}"#).is_none());

        let member = billing_notice_from_body(br#"{"code":"MEMBER_QUOTA_INSUFFICIENT","billing_notice":{"schema_version":1,"code":"MEMBER_QUOTA_INSUFFICIENT","stage":"INITIAL_RESERVATION","turn_outcome":"REJECTED","settlement_status":"NOT_STARTED","action":"CONTACT_ORG_ADMIN","context_type":"ORGANIZATION","organization_id":"0199a100-0001-7000-8000-000000000201","usage_request_id":"0199a100-0001-7000-8000-000000000106"}}"#).expect("member quota body notice");
        assert_eq!(member.code, "MEMBER_QUOTA_INSUFFICIENT");
        assert!(billing_notice_from_body(br#"{"billing_notice":{"schema_version":1,"code":"MEMBER_QUOTA_INSUFFICIENT","stage":"INITIAL_RESERVATION","turn_outcome":"REJECTED","settlement_status":"NOT_STARTED","action":"OPEN_ORGANIZATION_RECHARGE","context_type":"ORGANIZATION","organization_id":"0199a100-0001-7000-8000-000000000201","usage_request_id":"0199a100-0001-7000-8000-000000000106"}}"#).is_none());

        headers.insert(
            "x-pinvou-billing-code",
            "MEMBER_QUOTA_INSUFFICIENT".parse().unwrap(),
        );
        headers.insert(
            "x-pinvou-billing-action",
            "CONTACT_ORG_ADMIN".parse().unwrap(),
        );
        headers.insert(
            "x-pinvou-billing-context-type",
            "ORGANIZATION".parse().unwrap(),
        );
        headers.insert(
            "x-pinvou-billing-organization-id",
            "0199a100-0001-7000-8000-000000000201".parse().unwrap(),
        );
        let member_header =
            billing_notice_from_headers(&headers).expect("member quota header notice");
        assert_eq!(member_header.code, "MEMBER_QUOTA_INSUFFICIENT");

        let mut line = Vec::new();
        let mut overflow = false;
        let mut saw_done = false;
        let mut saw_terminal_error = false;
        let mut event_name = String::new();
        let sse = b"event: billing_notice\ndata: {\"schema_version\":1,\"code\":\"MEMBER_QUOTA_INSUFFICIENT\",\"stage\":\"ADDITIONAL_RESERVATION\",\"turn_outcome\":\"COMPLETED\",\"settlement_status\":\"SETTLED\",\"action\":\"CONTACT_ORG_ADMIN\",\"context_type\":\"ORGANIZATION\",\"organization_id\":\"0199a100-0001-7000-8000-000000000201\",\"usage_request_id\":\"0199a100-0001-7000-8000-000000000106\"}\n\n";
        let notices = observe_sse_lines(
            &mut line,
            &mut overflow,
            &mut saw_done,
            &mut saw_terminal_error,
            &mut event_name,
            sse,
        );
        assert_eq!(notices.len(), 1);
        assert_eq!(notices[0].code, "MEMBER_QUOTA_INSUFFICIENT");
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

        let missing = prepare_chat_body(
            Bytes::from_static(br#"{"messages":[]}"#),
            4096,
            "stable-model-id",
        )
        .expect("request without explicit limit");
        let missing: serde_json::Value = serde_json::from_slice(&missing).unwrap();
        assert!(missing.get("max_completion_tokens").is_none());
        assert!(missing.get("max_tokens").is_none());
    }

    #[tokio::test]
    async fn proxy_injects_stable_key_and_preserves_sse_response() {
        let captures = Arc::new(Mutex::new(Vec::<(
            String,
            String,
            String,
            String,
            u64,
            String,
        )>::new()));
        let upstream = Router::new()
            .route(
                "/v1/chat/completions",
                post(
                    |State(captures): State<
                        Arc<Mutex<Vec<(String, String, String, String, u64, String)>>>,
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
                        let capabilities = headers
                            .get("x-pinvou-client-capabilities")
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
                            capabilities,
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
        assert!(captures.iter().all(
            |(_, authorization, logical_turn, capabilities, maximum_output, model)| authorization
                == "Bearer oidc-access-token"
                && logical_turn.starts_with("pinvou-turn-")
                && capabilities == "billing-notice-v1"
                && *maximum_output == 4096
                && model == "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c"
        ));
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
