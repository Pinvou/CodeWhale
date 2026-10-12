use crate::features::assistant::runtime_model::RuntimeTurnAuthority;
use std::collections::HashMap;
use std::io;
use std::sync::{Arc, Mutex as StdMutex, OnceLock};
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
use tokio::sync::OnceCell;
use tokio::time::sleep;

const MAX_CHAT_BODY_BYTES: usize = 4 * 1024 * 1024;
const LOGICAL_TURN_PREFIX: &str = "pinvou-turn-";
const LOGICAL_TURN_HEADER: &str = "X-Pinvou-Logical-Turn-ID";
const CLIENT_CAPABILITIES_HEADER: &str = "X-Pinvou-Client-Capabilities";
const BILLING_NOTICE_CAPABILITY: &str = "billing-notice-v1";
const BILLING_NOTICE_EVENT: &str = "chat:billing_notice";

#[derive(Clone, Default)]
pub(super) struct ModelGatewayProxy {
    server: Arc<OnceCell<ProxyServer>>,
    app: Option<AppHandle>,
    turn_authority: Arc<OnceLock<Arc<dyn RuntimeTurnAuthority>>>,
}

struct ProxyServer {
    origin: String,
    state: ProxyState,
}

#[derive(Clone)]
struct ProxyState {
    client: reqwest::Client,
    registry: Arc<StdMutex<ProxyRegistry>>,
    turn_authority: Arc<dyn RuntimeTurnAuthority>,
    app: Option<AppHandle>,
    calls: super::model_cancellation::Calls,
}

#[derive(Default)]
struct ProxyRegistry {
    session_routes: HashMap<String, String>,
    targets: HashMap<String, ProxyTarget>,
}

#[derive(Clone)]
struct ProxyTarget {
    session_id: String,
    incarnation: Arc<super::model_cancellation::Incarnation>,
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
    turn_authority: Arc<dyn RuntimeTurnAuthority>,
    cancellation: Option<tokio_util::sync::CancellationToken>,
    incarnation: Arc<super::model_cancellation::Incarnation>,
}

impl BillingNoticeEmitter {
    fn stopped(&self) -> bool {
        self.incarnation.is_retired()
            || self
                .cancellation
                .as_ref()
                .is_some_and(|token| token.is_cancelled())
    }
    fn emit(&self, notice: BillingNotice) {
        if self.stopped() {
            return;
        }
        // Only the in-memory timing update runs under the lifecycle lock.
        // Event delivery occurs afterwards with the same fixed epoch, so a
        // synchronous event listener cannot re-enter the held lifecycle lock.
        let current = self.turn_authority.with_generation(
            &self.session_id,
            self.turn_generation,
            &mut || {
                if self.stopped() {
                    return;
                }
                if let Ok(persisted) = serde_json::to_value(&notice) {
                    crate::features::assistant::timing::record_billing_notice(
                        &self.session_id,
                        persisted,
                    );
                }
            },
        );
        if !current || self.stopped() {
            return;
        }
        let _ = self.app.emit(
            BILLING_NOTICE_EVENT,
            json!({ "session_id": self.session_id, "turn_generation": self.turn_generation, "billing_notice": notice }),
        );
    }
}

impl ModelGatewayProxy {
    pub(super) fn new(app: AppHandle) -> Self {
        Self {
            server: Arc::new(OnceCell::new()),
            app: Some(app),
            turn_authority: Arc::new(OnceLock::new()),
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
        let authority = self
            .turn_authority
            .get()
            .cloned()
            .context("model proxy turn authority is unavailable")?;
        let server = self
            .server
            .get_or_try_init(|| start_proxy_server(self.app.clone(), authority))
            .await?;
        let route = {
            let mut registry = server.state.registry.lock().unwrap();
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
                    incarnation: server.state.calls.incarnation(session_id),
                    upstream,
                    gateway_model_id: gateway_model_id.to_string(),
                    turn_generation: 0,
                    maximum_output_tokens,
                });
            route
        };
        Ok(format!("{}/{route}/v1", server.origin))
    }

    #[cfg(test)]
    pub(super) fn cancel_turn(&self, session_id: &str, generation: u64) {
        if let Some(server) = self.server.get() {
            server.state.calls.cancel_turn(session_id, generation);
        }
    }
    pub(super) fn cancellation_scope(&self, session_id: &str) -> Option<String> {
        self.server
            .get()
            .and_then(|server| server.state.calls.cancellation_scope(session_id))
    }
    pub(super) fn cancel_turn_bound(&self, session_id: &str, generation: u64, scope: Option<&str>) {
        if let (Some(server), Some(scope)) = (self.server.get(), scope) {
            server
                .state
                .calls
                .cancel_turn_bound(session_id, generation, scope);
        }
    }

    pub(super) fn forget_session(&self, session_id: &str) {
        if let Some(server) = self.server.get() {
            let mut registry = server.state.registry.lock().unwrap();
            if let Some(route) = registry.session_routes.remove(session_id) {
                registry.targets.remove(&route);
            }
            server.state.calls.forget_session(session_id);
        }
    }

    pub(super) fn reclaim_session(&self, session_id: &str, preserved_generation: Option<u64>) {
        if let Some(server) = self.server.get() {
            server
                .state
                .calls
                .reclaim_session(session_id, preserved_generation);
        }
    }

    pub(super) fn bind_turn_authority(
        &self,
        authority: Arc<dyn RuntimeTurnAuthority>,
    ) -> Result<()> {
        self.turn_authority
            .set(authority)
            .map_err(|_| anyhow::anyhow!("model proxy turn authority is already bound"))
    }
}

async fn start_proxy_server(
    app: Option<AppHandle>,
    turn_authority: Arc<dyn RuntimeTurnAuthority>,
) -> Result<ProxyServer> {
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
        registry: Arc::new(StdMutex::new(ProxyRegistry::default())),
        calls: super::model_cancellation::Calls::default(),
        turn_authority,
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
    let Some(mut target) = state.registry.lock().unwrap().targets.get(&route).cloned() else {
        return proxy_error(StatusCode::NOT_FOUND, "unknown model proxy route");
    };
    let Some(generation) = state
        .turn_authority
        .current_generation(&target.session_id)
        .filter(|value| *value > 0)
    else {
        return proxy_error(StatusCode::CONFLICT, "model turn is not active");
    };
    target.turn_generation = generation;
    let body = match prepare_chat_body(body, target.maximum_output_tokens, &target.gateway_model_id)
    {
        Ok(body) => body,
        Err(_) => return proxy_error(StatusCode::BAD_REQUEST, "invalid chat request body"),
    };
    let expects_sse = chat_request_expects_sse(&body);
    let idempotency_key = match request_idempotency_key(&parts.headers) {
        Some(key) => key,
        None => {
            return proxy_error(
                StatusCode::BAD_REQUEST,
                "model invocation identity is missing or invalid",
            );
        }
    };
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
    let target = state.registry.lock().unwrap().targets.get(&route).cloned();
    let Some(target) = target else {
        return proxy_error(StatusCode::NOT_FOUND, "unknown model proxy route");
    };
    forward(
        &state,
        target,
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
    if target.incarnation.is_retired() {
        return proxy_error(
            StatusCode::CONFLICT,
            "model transport incarnation is retired",
        );
    }
    let delivery = if let Some(key) = idempotency_key {
        let Some(bearer) = headers
            .get(header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
        else {
            return proxy_error(
                StatusCode::UNAUTHORIZED,
                "model invocation authorization is missing",
            );
        };
        match state.calls.track(
            &target.incarnation,
            target.turn_generation,
            target.upstream.clone(),
            bearer,
            key,
            state.client.clone(),
        ) {
            Ok(delivery) => Some(delivery),
            Err(_) => {
                return proxy_error(
                    StatusCode::CONFLICT,
                    "model invocation is cancelled or unavailable",
                );
            }
        }
    } else {
        None
    };
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
        if delivery.as_ref().is_some_and(|delivery| delivery.stopped()) {
            return proxy_error(StatusCode::CONFLICT, "model delivery cancelled");
        }
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
        let result = if let Some(delivery) = &delivery {
            tokio::select! {
                biased;
                _ = delivery.cancelled() => {
                    return proxy_error(StatusCode::CONFLICT, "model delivery cancelled");
                }
                result = builder.send() => result,
            }
        } else {
            builder.send().await
        };
        match result {
            Ok(response) => {
                upstream_response = Some(response);
                break;
            }
            Err(error) => {
                log::warn!(
                    "account platform model proxy transport failed: attempt={attempt} kind={}",
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
    if let Some(delivery) = &delivery {
        delivery.observe_usage(
            upstream_headers
                .get("x-pinvou-usage-request-id")
                .and_then(|value| value.to_str().ok()),
        );
        if delivery.stopped() {
            return proxy_error(StatusCode::CONFLICT, "model delivery cancelled");
        }
    }

    let notice_emitter = state.app.clone().map(|app| BillingNoticeEmitter {
        app,
        session_id: target.session_id.clone(),
        turn_generation: target.turn_generation,
        turn_authority: state.turn_authority.clone(),
        cancellation: delivery
            .as_ref()
            .map(|delivery| delivery.cancellation_token()),
        incarnation: target.incarnation.clone(),
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
        let payload = if let Some(delivery) = &delivery {
            tokio::select! {
                biased;
                _ = delivery.cancelled() => {
                    return proxy_error(StatusCode::CONFLICT, "model delivery cancelled");
                }
                result = upstream_response.bytes() => result,
            }
        } else {
            upstream_response.bytes().await
        };
        let payload = match payload {
            Ok(payload) => payload,
            Err(_) => return proxy_error(StatusCode::BAD_GATEWAY, "invalid gateway response"),
        };
        if delivery.as_ref().is_some_and(|delivery| delivery.stopped()) {
            return proxy_error(StatusCode::CONFLICT, "model delivery cancelled");
        }
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
    let body = match (upstream_is_sse, delivery) {
        (true, Some(delivery)) => Body::from_stream(super::model_cancellation::track_delivery(
            require_sse_done_with_notice(upstream_stream, notice_emitter),
            delivery,
        )),
        (false, Some(delivery)) => Body::from_stream(super::model_cancellation::track_delivery(
            upstream_stream,
            delivery,
        )),
        (true, None) => Body::from_stream(require_sse_done_with_notice(
            upstream_stream,
            notice_emitter,
        )),
        (false, None) => Body::from_stream(upstream_stream),
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

fn request_idempotency_key(headers: &reqwest::header::HeaderMap) -> Option<String> {
    // CodeWhale mints one operation ID and preserves it through every retry.
    // Do not derive a replacement from the body or the enclosing user turn:
    // separate invocations can have identical bodies in the same turn.
    let mut values = headers.get_all("Idempotency-Key").iter();
    let value = values.next()?.to_str().ok()?.trim();
    if values.next().is_some() || !(16..=128).contains(&value.len()) {
        return None;
    }
    Some(value.to_owned())
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
    url.has_host()
        && !url.cannot_be_a_base()
        && url.username().is_empty()
        && url.password().is_none()
        && url.query().is_none()
        && url.fragment().is_none()
        && (url.scheme() == "https"
            || (url.scheme() == "http"
                && matches!(url.host_str(), Some("127.0.0.1" | "localhost" | "::1"))))
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

    #[test]
    fn upstream_origin_rejects_credentials_and_ambiguous_targets() {
        for target in [
            "https://user:secret@example.test/",
            "https://example.test/?redirect=other",
            "https://example.test/#fragment",
            "http://example.test/",
            "file:///local",
        ] {
            assert!(!super::upstream_is_allowed(
                &reqwest::Url::parse(target).unwrap()
            ));
        }
        assert!(super::upstream_is_allowed(
            &reqwest::Url::parse("https://example.test/").unwrap()
        ));
        assert!(super::upstream_is_allowed(
            &reqwest::Url::parse("http://127.0.0.1:3000/").unwrap()
        ));
    }

    #[derive(Default)]
    pub(super) struct TestTurnAuthority(std::sync::Mutex<Option<u64>>);
    impl TestTurnAuthority {
        pub(super) fn set(&self, generation: Option<u64>) {
            *self.0.lock().unwrap() = generation;
        }
    }
    impl crate::features::assistant::runtime_model::RuntimeTurnAuthority for TestTurnAuthority {
        fn current_generation(&self, _: &str) -> Option<u64> {
            *self.0.lock().unwrap()
        }
        fn with_generation(&self, _: &str, generation: u64, callback: &mut dyn FnMut()) -> bool {
            let current = self.0.lock().unwrap();
            if generation == 0 || *current != Some(generation) {
                return false;
            }
            callback();
            true
        }
    }

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
    fn model_operation_id_is_preserved_and_missing_or_ambiguous_identity_is_rejected() {
        let mut headers = reqwest::header::HeaderMap::new();
        assert!(request_idempotency_key(&headers).is_none());
        headers.insert("Idempotency-Key", "operation-call-a-001".parse().unwrap());
        assert_eq!(
            request_idempotency_key(&headers).as_deref(),
            Some("operation-call-a-001")
        );
        assert_eq!(
            request_idempotency_key(&headers),
            request_idempotency_key(&headers)
        );
        headers.insert("Idempotency-Key", "operation-call-b-001".parse().unwrap());
        assert_eq!(
            request_idempotency_key(&headers).as_deref(),
            Some("operation-call-b-001")
        );
        headers.append("Idempotency-Key", "operation-call-a-001".parse().unwrap());
        assert!(request_idempotency_key(&headers).is_none());
        headers.clear();
        headers.insert("Idempotency-Key", "short".parse().unwrap());
        assert!(request_idempotency_key(&headers).is_none());
        headers.insert("Idempotency-Key", "x".repeat(129).parse().unwrap());
        assert!(request_idempotency_key(&headers).is_none());
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
        let authority = Arc::new(TestTurnAuthority::default());
        authority.set(Some(2)); // A prior local turn is already complete.
        proxy
            .bind_turn_authority(authority.clone())
            .expect("bind authority");
        let base_url = proxy
            .register_session(
                "session-a",
                &format!("http://{upstream_address}/v1"),
                "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c",
                4096,
            )
            .await
            .expect("register proxy route");
        authority.set(Some(2));
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
                authority.set(Some(3));
            }
            let response = client
                .post(format!("{base_url}/chat/completions"))
                .header(
                    "Idempotency-Key",
                    if index < 2 {
                        "operation-call-a-001"
                    } else if index == 2 {
                        "operation-call-b-001"
                    } else {
                        "operation-call-c-001"
                    },
                )
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
        let route = base_url.trim_end_matches("/v1").rsplit('/').next().unwrap();
        assert_eq!(captures[0].2, logical_turn_id(route, 2));
        assert_eq!(captures[2].2, logical_turn_id(route, 3));
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
        let authority = Arc::new(TestTurnAuthority::default());
        authority.set(Some(2)); // A prior local turn is already complete.
        proxy
            .bind_turn_authority(authority.clone())
            .expect("bind authority");
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
                .header("Idempotency-Key", "operation-response-case-001")
                .bearer_auth("frozen-user")
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

#[cfg(test)]
mod admission_tests {
    use super::tests::TestTurnAuthority;
    use super::*;
    #[tokio::test]
    async fn proxy_missing_idle_and_zero_authority_fail_before_upstream() {
        let proxy = ModelGatewayProxy::default();
        assert!(
            proxy
                .register_session("session", "http://127.0.0.1:1/v1", "public-model", 100)
                .await
                .is_err()
        );
        let authority = Arc::new(TestTurnAuthority::default());
        proxy.bind_turn_authority(authority.clone()).unwrap();
        let base = proxy
            .register_session("session", "http://127.0.0.1:1/v1", "public-model", 100)
            .await
            .unwrap();
        for epoch in [None, Some(0)] {
            authority.set(epoch);
            let result = reqwest::Client::new()
                .post(format!("{base}/chat/completions"))
                .header("Idempotency-Key", "operation-test-001")
                .json(&json!({"messages":[]}))
                .send()
                .await
                .unwrap();
            assert_eq!(result.status(), reqwest::StatusCode::CONFLICT);
        }
        assert!(
            proxy.bind_turn_authority(authority).is_err(),
            "authority cannot be rebound"
        );
    }
}

#[cfg(test)]
mod cancellation_integration_tests {
    use super::tests::TestTurnAuthority;
    use super::*;
    use std::sync::Mutex;
    #[derive(Clone)]
    struct Fixture {
        id: uuid::Uuid,
        posts: Arc<Mutex<Vec<String>>>,
        reads: Arc<Mutex<usize>>,
        delay_headers: bool,
        accepted: Arc<tokio::sync::Notify>,
        expected_bearer: &'static str,
        expected_key: &'static str,
    }
    async fn chat(
        State(f): State<Fixture>,
        headers: axum::http::HeaderMap,
        Json(body): Json<serde_json::Value>,
    ) -> Response<Body> {
        assert_eq!(headers[header::AUTHORIZATION], f.expected_bearer);
        assert_eq!(headers["idempotency-key"], f.expected_key);
        assert_eq!(body["model"], "public-model-id");
        f.accepted.notify_one();
        if f.delay_headers {
            sleep(Duration::from_secs(3)).await;
        }
        let source = stream::once(async {
            Ok::<_, io::Error>(Bytes::from_static(b"data: {\"text\":\"first\"}\n\n"))
        })
        .chain(stream::once(async {
            sleep(Duration::from_secs(2)).await;
            Ok::<_, io::Error>(Bytes::from_static(b"data: late-result\n\ndata: [DONE]\n\n"))
        }));
        Response::builder()
            .status(StatusCode::OK)
            .header(header::CONTENT_TYPE, "text/event-stream")
            .header("X-Pinvou-Usage-Request-ID", f.id.to_string())
            .body(Body::from_stream(source))
            .unwrap()
    }
    async fn cancel(
        State(f): State<Fixture>,
        Path(id): Path<String>,
        headers: axum::http::HeaderMap,
    ) -> StatusCode {
        assert_eq!(id, f.id.to_string());
        assert_eq!(headers[header::AUTHORIZATION], f.expected_bearer);
        let mut keys = f.posts.lock().unwrap();
        keys.push(headers["idempotency-key"].to_str().unwrap().into());
        if keys.len() == 1 {
            StatusCode::SERVICE_UNAVAILABLE
        } else {
            StatusCode::OK
        }
    }
    async fn read(
        State(f): State<Fixture>,
        headers: axum::http::HeaderMap,
    ) -> Json<serde_json::Value> {
        assert_eq!(headers[header::AUTHORIZATION], f.expected_bearer);
        assert_eq!(headers["idempotency-key"], f.expected_key);
        *f.reads.lock().unwrap() += 1;
        Json(
            json!({"usage_request_id":f.id,"cancel_status":"UNSUPPORTED","settlement_status":"PENDING"}),
        )
    }
    #[tokio::test]
    async fn real_proxy_abort_and_reclaim_cancel_frozen_usage_and_drop_late_delivery() {
        for reclaim in [false, true] {
            let f = Fixture {
                id: uuid::Uuid::new_v4(),
                posts: Arc::new(Mutex::new(Vec::new())),
                reads: Arc::new(Mutex::new(0)),
                delay_headers: false,
                accepted: Arc::new(tokio::sync::Notify::new()),
                expected_bearer: "Bearer frozen-user",
                expected_key: "model-invocation-frozen-key",
            };
            let router = Router::new()
                .route("/v1/chat/completions", post(chat))
                .route("/v1/usage-requests/{id}/cancel", post(cancel))
                // Active organization / cancel-only identities cannot use the
                // personal detail endpoint, but can confirm their own key.
                .route("/v1/usage-requests/{id}", get(|| async { StatusCode::FORBIDDEN }))
                .route("/v1/usage-requests/by-idempotency-key", get(read))
                .with_state(f.clone());
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let address = listener.local_addr().unwrap();
            let task = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
            let proxy = ModelGatewayProxy::default();
            let authority = Arc::new(TestTurnAuthority::default());
            authority.set(Some(11));
            proxy.bind_turn_authority(authority.clone()).unwrap();
            let base = proxy
                .register_session(
                    "session",
                    &format!("http://{address}/v1"),
                    "public-model-id",
                    128,
                )
                .await
                .unwrap();
            let response = reqwest::Client::new()
                .post(format!("{base}/chat/completions"))
                .bearer_auth("frozen-user")
                .header("Idempotency-Key", "model-invocation-frozen-key")
                .json(&json!({"model":"engine-visible-model","messages":[],"stream":true}))
                .send()
                .await
                .unwrap();
            assert_eq!(response.status(), StatusCode::OK);
            let mut source = response.bytes_stream();
            assert!(source.next().await.unwrap().unwrap().starts_with(b"data:"));
            if reclaim {
                proxy.reclaim_session("session", Some(12));
            } else {
                proxy.cancel_turn("session", 11);
            }
            let late = tokio::time::timeout(Duration::from_secs(1), source.next())
                .await
                .unwrap();
            assert!(
                late.is_none() || late.is_some_and(|chunk| chunk.is_err()),
                "late results escaped cancellation"
            );
            tokio::time::timeout(Duration::from_secs(3), async {
                loop {
                    if *f.reads.lock().unwrap() > 0 {
                        break;
                    }
                    sleep(Duration::from_millis(10)).await
                }
            })
            .await
            .unwrap();
            let keys = f.posts.lock().unwrap();
            assert_eq!(keys.len(), 2);
            assert_eq!(keys[0], keys[1]);
            // Unsupported provider cancellation preserves server PENDING funds;
            // the client only stops delivery and queries the authoritative state.
            task.abort();
        }
    }

    async fn slow_payment_required(State(f): State<Fixture>) -> Response<Body> {
        let accepted = f.accepted.clone();
        let source = stream::once(async move {
            accepted.notify_one();
            Ok::<_, io::Error>(Bytes::from_static(b"{\"error\":"))
        })
        .chain(stream::once(async {
            sleep(Duration::from_secs(3)).await;
            Ok::<_, io::Error>(Bytes::from_static(b"{\"code\":\"WALLET_INSUFFICIENT\"}}"))
        }));
        Response::builder()
            .status(StatusCode::PAYMENT_REQUIRED)
            .header(header::CONTENT_TYPE, "application/json")
            .header("X-Pinvou-Usage-Request-ID", f.id.to_string())
            .body(Body::from_stream(source))
            .unwrap()
    }

    async fn spawn_reset_fixture(f: Fixture) -> (String, tokio::task::JoinHandle<()>) {
        let router = Router::new()
            .route("/v1/chat/completions", post(chat))
            .route("/v1/usage-requests/{id}/cancel", post(cancel))
            .route("/v1/usage-requests/by-idempotency-key", get(read))
            .with_state(f);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let task = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
        (format!("http://{address}/v1"), task)
    }

    #[tokio::test]
    async fn real_proxy_reset_retires_old_route_and_cached_target_but_allows_new_epoch_one() {
        for pre_cancel in [false, true] {
            let fixture = |bearer, key| Fixture {
                id: uuid::Uuid::new_v4(),
                posts: Arc::new(Mutex::new(Vec::new())),
                reads: Arc::new(Mutex::new(0)),
                delay_headers: false,
                accepted: Arc::new(tokio::sync::Notify::new()),
                expected_bearer: bearer,
                expected_key: key,
            };
            let old = fixture("Bearer frozen-user", "model-invocation-frozen-key");
            let new = fixture("Bearer new-user", "new-model-invocation-key");
            let (old_origin, old_task) = spawn_reset_fixture(old.clone()).await;
            let (new_origin, new_task) = spawn_reset_fixture(new.clone()).await;
            let proxy = ModelGatewayProxy::default();
            let authority = Arc::new(TestTurnAuthority::default());
            authority.set(Some(1));
            proxy.bind_turn_authority(authority).unwrap();
            let old_base = proxy
                .register_session("aux-session", &old_origin, "public-model-id", 128)
                .await
                .unwrap();
            let old_route = old_base.split('/').nth(3).unwrap();
            let old_scope = proxy.cancellation_scope("aux-session");
            let mut cached = proxy
                .server
                .get()
                .unwrap()
                .state
                .registry
                .lock()
                .unwrap()
                .targets[old_route]
                .clone();
            cached.turn_generation = 1;
            let body = json!({"model":"engine-model","messages":[],"stream":true});
            let old_response = reqwest::Client::new()
                .post(format!("{old_base}/chat/completions"))
                .header("Authorization", old.expected_bearer)
                .header("Idempotency-Key", old.expected_key)
                .json(&body)
                .send()
                .await
                .unwrap();
            let mut old_stream = old_response.bytes_stream();
            assert!(old_stream.next().await.unwrap().is_ok());
            if pre_cancel {
                proxy.cancel_turn("aux-session", 1);
            }
            proxy.forget_session("aux-session");
            let new_base = proxy
                .register_session("aux-session", &new_origin, "public-model-id", 128)
                .await
                .unwrap();
            assert_ne!(old_base, new_base);
            assert_ne!(old_scope, proxy.cancellation_scope("aux-session"));
            let stale = reqwest::Client::new()
                .post(format!("{old_base}/chat/completions"))
                .header("Authorization", old.expected_bearer)
                .header("Idempotency-Key", old.expected_key)
                .json(&body)
                .send()
                .await
                .unwrap();
            assert_eq!(stale.status(), StatusCode::NOT_FOUND);
            let mut headers = axum::http::HeaderMap::new();
            headers.insert(header::AUTHORIZATION, old.expected_bearer.parse().unwrap());
            let stale_cached = super::forward(
                &proxy.server.get().unwrap().state,
                cached,
                "chat/completions",
                reqwest::Method::POST,
                &headers,
                Some(Bytes::from(body.to_string())),
                Some(old.expected_key),
                None,
                true,
            )
            .await;
            assert_eq!(stale_cached.status(), StatusCode::CONFLICT);
            let fresh = reqwest::Client::new()
                .post(format!("{new_base}/chat/completions"))
                .header("Authorization", new.expected_bearer)
                .header("Idempotency-Key", new.expected_key)
                .json(&body)
                .send()
                .await
                .unwrap();
            assert_eq!(fresh.status(), StatusCode::OK);
            let mut fresh_stream = fresh.bytes_stream();
            assert!(fresh_stream.next().await.unwrap().is_ok());
            proxy.cancel_turn_bound("aux-session", 1, old_scope.as_deref());
            let chunks = fresh_stream.collect::<Vec<_>>().await;
            assert!(chunks.iter().all(|chunk| chunk.is_ok()));
            assert!(chunks.iter().any(|chunk| {
                chunk
                    .as_ref()
                    .unwrap()
                    .windows(6)
                    .any(|part| part == b"[DONE]")
            }));
            tokio::time::timeout(Duration::from_secs(3), async {
                while old.posts.lock().unwrap().len() < 2 {
                    sleep(Duration::from_millis(10)).await;
                }
            })
            .await
            .unwrap();
            assert_eq!(old.posts.lock().unwrap().len(), 2);
            assert!(new.posts.lock().unwrap().is_empty());
            old_task.abort();
            new_task.abort();
        }
    }

    #[tokio::test]
    async fn real_proxy_cancel_interrupts_pending_headers_and_payment_error_body() {
        for payment_body in [false, true] {
            let f = Fixture {
                id: uuid::Uuid::new_v4(),
                posts: Arc::new(Mutex::new(Vec::new())),
                reads: Arc::new(Mutex::new(0)),
                delay_headers: true,
                accepted: Arc::new(tokio::sync::Notify::new()),
                expected_bearer: "Bearer frozen-user",
                expected_key: "model-invocation-frozen-key",
            };
            let router = Router::new()
                .route(
                    "/v1/chat/completions",
                    if payment_body {
                        post(slow_payment_required)
                    } else {
                        post(chat)
                    },
                )
                .route("/v1/usage-requests/{id}/cancel", post(cancel))
                .route("/v1/usage-requests/by-idempotency-key", get(read))
                .with_state(f.clone());
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let address = listener.local_addr().unwrap();
            let task = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
            let proxy = ModelGatewayProxy::default();
            let authority = Arc::new(TestTurnAuthority::default());
            authority.set(Some(11));
            proxy.bind_turn_authority(authority).unwrap();
            let base = proxy
                .register_session(
                    "session",
                    &format!("http://{address}/v1"),
                    "public-model-id",
                    128,
                )
                .await
                .unwrap();
            let request = tokio::spawn(async move {
                reqwest::Client::new()
                    .post(format!("{base}/chat/completions"))
                    .bearer_auth("frozen-user")
                    .header("Idempotency-Key", "model-invocation-frozen-key")
                    .json(&json!({"model":"engine-visible-model","messages":[],"stream":true}))
                    .send()
                    .await
                    .unwrap()
            });
            tokio::time::timeout(Duration::from_secs(2), f.accepted.notified())
                .await
                .unwrap();
            if payment_body {
                // Let the first partial error body reach the real proxy before stop.
                sleep(Duration::from_millis(100)).await;
            }
            proxy.cancel_turn("session", 11);
            let response = tokio::time::timeout(Duration::from_secs(1), request)
                .await
                .unwrap()
                .unwrap();
            assert_eq!(response.status(), StatusCode::CONFLICT);
            tokio::time::timeout(Duration::from_secs(3), async {
                while f.posts.lock().unwrap().len() < 2 {
                    sleep(Duration::from_millis(10)).await;
                }
            })
            .await
            .unwrap();
            assert!(*f.reads.lock().unwrap() >= 1);
            let keys = f.posts.lock().unwrap();
            assert_eq!(keys[0], keys[1]);
            task.abort();
        }
    }
}
