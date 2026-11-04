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
const MODEL_REJECTION_HEADER: &str = "x-pinvou-model-rejection-v1";

fn model_rejection_from_headers(
    status: StatusCode,
    headers: &reqwest::header::HeaderMap,
    model_id: &str,
) -> Option<crate::features::assistant::timing::ModelRejection> {
    use crate::features::assistant::timing::{ModelRejection, ModelRejectionCode};
    if status != StatusCode::CONFLICT {
        return None;
    }
    let values = headers.get_all(MODEL_REJECTION_HEADER);
    let mut values = values.iter();
    let value = values.next()?.to_str().ok()?;
    if values.next().is_some() {
        return None;
    }
    let code = match value {
        "MODEL_UNAVAILABLE" => ModelRejectionCode::Unavailable,
        "MODEL_DEPRECATED" => ModelRejectionCode::Deprecated,
        _ => return None,
    };
    ModelRejection::new(code, model_id)
}

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
    authorization: Option<Arc<dyn super::ModelAuthorization>>,
    session_id: String,
    incarnation: Arc<super::model_cancellation::Incarnation>,
    upstream: Url,
    gateway_model_id: String,
    turn_generation: u64,
    auxiliary: bool,
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

    pub(super) fn capture_timing_owner(&self, session_id: &str) -> Option<(u64, String)> {
        let authority = self.turn_authority.get()?;
        let generation = authority.current_generation(session_id)?;
        let mut timing_id = None;
        authority.with_generation(session_id, generation, &mut || {
            timing_id = crate::features::assistant::timing::current_turn_id(session_id);
        });
        timing_id.map(|id| (generation, id))
    }

    pub(super) fn record_prepare_rejection(
        &self,
        session_id: &str,
        owner: &(u64, String),
        rejection: crate::features::assistant::timing::ModelRejection,
    ) {
        if let Some(authority) = self.turn_authority.get() {
            authority.with_generation(session_id, owner.0, &mut || {
                crate::features::assistant::timing::record_model_rejection_for_turn(
                    session_id,
                    &owner.1,
                    rejection.clone(),
                );
            });
        }
    }

    #[cfg(test)]
    pub(super) async fn register_session(
        &self,
        session_id: &str,
        upstream_base_url: &str,
        gateway_model_id: &str,
        maximum_output_tokens: u32,
    ) -> Result<String> {
        self.register_authorized_session(
            session_id,
            upstream_base_url,
            gateway_model_id,
            maximum_output_tokens,
            None,
        )
        .await
    }

    pub(super) async fn register_authorized_session(
        &self,
        session_id: &str,
        upstream_base_url: &str,
        gateway_model_id: &str,
        maximum_output_tokens: u32,
        authorization: Option<Arc<dyn super::ModelAuthorization>>,
    ) -> Result<String> {
        self.register_authorized_runtime(
            session_id,
            false,
            upstream_base_url,
            gateway_model_id,
            maximum_output_tokens,
            authorization,
        )
        .await
    }

    /// Auxiliary scopes have their own opaque route/incarnation, never the
    /// original chat session's cancellation, generation, or timing owner.
    pub(super) async fn register_authorized_runtime(
        &self,
        session_id: &str,
        auxiliary: bool,
        upstream_base_url: &str,
        gateway_model_id: &str,
        maximum_output_tokens: u32,
        authorization: Option<Arc<dyn super::ModelAuthorization>>,
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
            let mut registry = server
                .state
                .registry
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            let changed_authority = registry
                .session_routes
                .get(session_id)
                .and_then(|route| registry.targets.get(route))
                .is_some_and(|target| match (&target.authorization, &authorization) {
                    (Some(previous), Some(current)) => match (previous.scope(), current.scope()) {
                        (Some(previous), Some(current)) => previous != current,
                        _ => !Arc::ptr_eq(previous, current),
                    },
                    (None, None) => false,
                    _ => true,
                });
            if changed_authority {
                if let Some(route) = registry.session_routes.remove(session_id) {
                    registry.targets.remove(&route);
                }
                // An old Engine may still retain its URL or an admitted target.
                // A new login never upgrades either handle to a new principal.
                server.state.calls.forget_session(session_id);
            }
            let route = registry
                .session_routes
                .entry(session_id.to_string())
                .or_insert_with(random_route_token)
                .clone();
            registry
                .targets
                .entry(route.clone())
                .and_modify(|target| {
                    target.authorization = authorization.clone();
                    target.session_id = session_id.to_string();
                    target.upstream = upstream.clone();
                    target.gateway_model_id = gateway_model_id.to_string();
                    target.maximum_output_tokens = maximum_output_tokens;
                    target.auxiliary = auxiliary;
                })
                .or_insert(ProxyTarget {
                    authorization,
                    session_id: session_id.to_string(),
                    incarnation: server.state.calls.incarnation(session_id),
                    upstream,
                    gateway_model_id: gateway_model_id.to_string(),
                    turn_generation: 0,
                    auxiliary,
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
            let mut registry = server
                .state
                .registry
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
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
    let Some(mut target) = state
        .registry
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .targets
        .get(&route)
        .cloned()
    else {
        return proxy_error(StatusCode::NOT_FOUND, "unknown model proxy route");
    };
    let generation = if target.auxiliary {
        // Admission exists only while the command owns its private scope.
        if target.incarnation.is_retired() {
            return proxy_error(StatusCode::CONFLICT, "model auxiliary delivery is retired");
        }
        1
    } else {
        let Some(generation) = state
            .turn_authority
            .current_generation(&target.session_id)
            .filter(|value| *value > 0)
        else {
            return proxy_error(StatusCode::CONFLICT, "model turn is not active");
        };
        generation
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
    let target = state
        .registry
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .targets
        .get(&route)
        .cloned();
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
    // Capture the timing owner before authorization may wait for token refresh.
    let timing_turn_id = (!target.auxiliary)
        .then(|| crate::features::assistant::timing::current_turn_id(&target.session_id))
        .flatten();
    let mut bearer = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_string();
    let delivery = if let Some(key) = idempotency_key {
        if bearer.is_empty() {
            return proxy_error(
                StatusCode::UNAUTHORIZED,
                "model invocation authorization is missing",
            );
        }
        match state.calls.track_authorized(
            &target.incarnation,
            target.turn_generation,
            target.upstream.clone(),
            &bearer,
            key,
            state.client.clone(),
            target.authorization.clone(),
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
    // naturally idempotent. A 401 gets one refresh of this route's original
    // authenticated session; other application responses are never retried.
    let mut transport_failures = 0;
    let mut refreshed = false;
    for attempt in 1..=3 {
        if delivery.as_ref().is_some_and(|delivery| delivery.stopped()) {
            return proxy_error(StatusCode::CONFLICT, "model delivery cancelled");
        }
        if let Some(supplier) = &target.authorization {
            let authorization = if let Some(delivery) = &delivery {
                tokio::select! {
                    biased;
                    _ = delivery.cancelled() => return proxy_error(StatusCode::CONFLICT, "model delivery cancelled"),
                    result = supplier.bearer(None) => result,
                }
            } else {
                supplier.bearer(None).await
            };
            match authorization {
                Ok(current) => bearer = current,
                Err(_) => {
                    return proxy_error(
                        StatusCode::UNAUTHORIZED,
                        "model authorization expired or changed",
                    );
                }
            }
        }
        let mut builder = model_request_budget(
            state.client.request(method.clone(), target_url.clone()),
            resource,
            &method,
        );
        for name in [header::CONTENT_TYPE, header::ACCEPT, header::USER_AGENT] {
            if let Some(value) = headers.get(&name) {
                builder = builder.header(name, value);
            }
        }
        builder = builder.header(header::AUTHORIZATION, &bearer);
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
                if response.status() == StatusCode::UNAUTHORIZED && !refreshed {
                    if let Some(supplier) = &target.authorization {
                        refreshed = true;
                        let authorization = if let Some(delivery) = &delivery {
                            tokio::select! {
                                biased;
                                _ = delivery.cancelled() => return proxy_error(StatusCode::CONFLICT, "model delivery cancelled"),
                                result = supplier.bearer(Some(&bearer)) => result,
                            }
                        } else {
                            supplier.bearer(Some(&bearer)).await
                        };
                        match authorization {
                            Ok(_) => continue,
                            Err(_) => {
                                return proxy_error(
                                    StatusCode::UNAUTHORIZED,
                                    "model authorization expired or changed",
                                );
                            }
                        }
                    }
                }
                upstream_response = Some(response);
                break;
            }
            Err(error) => {
                log::warn!(
                    "account platform model proxy transport failed: attempt={attempt} kind={}",
                    transport_error_kind(&error)
                );
                last_error = Some(error);
                transport_failures += 1;
                if transport_failures == 1 {
                    sleep(Duration::from_millis(150)).await;
                } else {
                    break;
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

    if let (Some(turn_id), Some(rejection)) = (
        timing_turn_id,
        model_rejection_from_headers(status, &upstream_headers, &target.gateway_model_id),
    ) {
        let stopped = || {
            target.incarnation.is_retired()
                || delivery.as_ref().is_some_and(|delivery| delivery.stopped())
        };
        if !stopped() {
            state.turn_authority.with_generation(
                &target.session_id,
                target.turn_generation,
                &mut || {
                    if !stopped() {
                        crate::features::assistant::timing::record_model_rejection_for_turn(
                            &target.session_id,
                            &turn_id,
                            rejection.clone(),
                        );
                    }
                },
            );
        }
    }

    let notice_emitter =
        state
            .app
            .clone()
            .filter(|_| !target.auxiliary)
            .map(|app| BillingNoticeEmitter {
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
                result = super::bounded_response_bytes(upstream_response) => result,
            }
        } else {
            super::bounded_response_bytes(upstream_response).await
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
    let notice_emitter =
        notice_emitter.filter(|_| accepts_sse_billing_notice(status, &upstream_headers));
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

// AccountPlatformState already applies this read budget to token GET /v1/models.
// The same catalog through the engine proxy must not wait without a budget.
// Chat has its existing cancellation/operation contract; no new total deadline.
fn model_request_budget(
    builder: reqwest::RequestBuilder,
    resource: &str,
    method: &reqwest::Method,
) -> reqwest::RequestBuilder {
    if resource == "models" && *method == reqwest::Method::GET {
        builder.timeout(super::ACCOUNT_API_REQUEST_TIMEOUT)
    } else {
        builder
    }
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
                        .is_some_and(|value| {
                            value.get("error").is_some_and(|error| !error.is_null())
                        })
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

fn accepts_sse_billing_notice(status: StatusCode, headers: &reqwest::header::HeaderMap) -> bool {
    // Gateway framing writes this exact type. Raw supplier MIME aliases must not
    // gain platform authority; ordinary SSE transport keeps its broader detection.
    status.is_success()
        && headers
            .get(header::CONTENT_TYPE)
            .is_some_and(|value| value.as_bytes() == b"text/event-stream")
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
    use axum::body::{Body, Bytes};
    use axum::extract::State;
    use axum::http::{HeaderMap, Response, StatusCode, header};
    use axum::routing::post;
    use futures_util::{StreamExt, stream};
    use tokio::sync::Mutex;

    use super::{
        ModelGatewayProxy, accepts_sse_billing_notice, billing_notice_from_body,
        billing_notice_from_headers, logical_turn_id, observe_sse_lines, prepare_chat_body,
        request_idempotency_key, require_sse_done,
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

    #[tokio::test]
    async fn gateway_lifecycle_response_is_durable_for_its_current_owner_only() {
        use crate::features::assistant::timing;
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let tmp = std::env::temp_dir().join(format!(
            "pinvou-model-proxy-evidence-{}",
            uuid::Uuid::new_v4()
        ));
        let previous_home = std::env::var_os("PINVOU3_HOME");
        // SAFETY: ENV_LOCK serializes process environment writes in tests.
        unsafe { std::env::set_var("PINVOU3_HOME", &tmp) };
        let id = "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c";
        for (index, (status, marker, expected, boundary)) in [
            (
                StatusCode::CONFLICT,
                Some("MODEL_UNAVAILABLE"),
                true,
                "current",
            ),
            (
                StatusCode::CONFLICT,
                Some("MODEL_DEPRECATED"),
                true,
                "current",
            ),
            (
                StatusCode::FORBIDDEN,
                Some("MODEL_UNAVAILABLE"),
                false,
                "current",
            ),
            (StatusCode::CONFLICT, None, false, "current"),
            (
                StatusCode::CONFLICT,
                Some("MODEL_REMOVED"),
                false,
                "current",
            ),
            (
                StatusCode::CONFLICT,
                Some("MODEL_UNAVAILABLE"),
                false,
                "retired",
            ),
            (
                StatusCode::CONFLICT,
                Some("MODEL_UNAVAILABLE"),
                false,
                "cancelled",
            ),
            (
                StatusCode::CONFLICT,
                Some("MODEL_UNAVAILABLE"),
                false,
                "generation",
            ),
            (
                StatusCode::CONFLICT,
                Some("MODEL_UNAVAILABLE"),
                false,
                "timing-owner",
            ),
        ]
        .into_iter()
        .enumerate()
        {
            let entered = Arc::new(tokio::sync::Notify::new());
            let release = Arc::new(tokio::sync::Notify::new());
            let server_entered = entered.clone();
            let server_release = release.clone();
            let upstream = Router::new().route("/v1/chat/completions", post(move || {
                let entered = server_entered.clone();
                let release = server_release.clone();
                async move {
                entered.notify_one();
                release.notified().await;
                let mut response = Response::builder().status(status).header(header::CONTENT_TYPE, "application/json");
                if let Some(value) = marker { response = response.header(super::MODEL_REJECTION_HEADER, value); }
                response.body(Body::from(r#"{"error":{"code":"MODEL_UNAVAILABLE","model_id":"body-imposter"}}"#)).unwrap()
            }}));
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let address = listener.local_addr().unwrap();
            let server = tokio::spawn(async move {
                axum::serve(listener, upstream).await.unwrap();
            });
            let sid = format!("model-proxy-evidence-{index}");
            let proxy = ModelGatewayProxy::default();
            let authority = Arc::new(TestTurnAuthority::default());
            authority.set(Some(1));
            proxy.bind_turn_authority(authority.clone()).unwrap();
            let base = proxy
                .register_session(&sid, &format!("http://{address}/v1"), id, 4096)
                .await
                .unwrap();
            let mut owner = timing::start_turn(&sid);
            let request = reqwest::Client::new()
                .post(format!("{base}/chat/completions"))
                .bearer_auth("test-only-opaque-authorization")
                .header(
                    "Idempotency-Key",
                    format!("model-evidence-operation-{index}"),
                )
                .header(header::CONTENT_TYPE, "application/json")
                .body(
                    r#"{"model":"m","stream":false,"messages":[{"role":"user","content":"test"}]}"#,
                );
            let pending = tokio::spawn(async move { request.send().await.unwrap() });
            entered.notified().await;
            match boundary {
                "retired" => proxy.forget_session(&sid),
                "cancelled" => {
                    let scope = proxy.cancellation_scope(&sid);
                    proxy.cancel_turn_bound(&sid, 1, scope.as_deref());
                }
                "generation" => authority.set(Some(2)),
                "timing-owner" => {
                    timing::clear_session(&sid);
                    owner = timing::start_turn(&sid);
                }
                _ => {}
            }
            release.notify_one();
            let response = pending.await.unwrap();
            assert_eq!(
                response.status(),
                if matches!(boundary, "retired" | "cancelled") {
                    StatusCode::CONFLICT
                } else {
                    status
                }
            );
            timing::finish_turn(&sid, "Failed", Some("generic model request rejected"));
            let terminal = timing::read_timeline(&sid)
                .unwrap()
                .into_iter()
                .find(|event| event.event == "assistant_done")
                .unwrap();
            assert_eq!(terminal.turn_id, owner);
            assert_eq!(terminal.model_rejection.is_some(), expected);
            if let Some(value) = terminal.model_rejection {
                assert_eq!(value.model_id, id);
            }

            // Prepared-model observation shares the production lifecycle authority.
            authority.set(Some(1));
            let first = timing::start_turn(&sid);
            let captured = proxy.capture_timing_owner(&sid).unwrap();
            assert_eq!(captured.1, first);
            timing::clear_session(&sid);
            let next = timing::start_turn(&sid);
            let rejection =
                timing::ModelRejection::new(timing::ModelRejectionCode::Unavailable, id).unwrap();
            proxy.record_prepare_rejection(&sid, &captured, rejection.clone());
            timing::finish_turn(&sid, "send_error", None);
            let terminal = timing::read_timeline(&sid)
                .unwrap()
                .into_iter()
                .find(|event| event.turn_id == next && event.event == "assistant_done")
                .unwrap();
            assert!(
                terminal.model_rejection.is_none(),
                "same SID and generation cannot reuse an old timing owner"
            );
            let next = timing::start_turn(&sid);
            let captured = proxy.capture_timing_owner(&sid).unwrap();
            authority.set(None);
            proxy.record_prepare_rejection(&sid, &captured, rejection);
            timing::finish_turn(&sid, "send_error", None);
            let terminal = timing::read_timeline(&sid)
                .unwrap()
                .into_iter()
                .find(|event| event.turn_id == next && event.event == "assistant_done")
                .unwrap();
            assert!(
                terminal.model_rejection.is_none(),
                "cancelled generation cannot record prepare rejection"
            );
            timing::clear_session(&sid);
            proxy.forget_session(&sid);
            server.abort();
        }
        unsafe {
            match previous_home {
                Some(value) => std::env::set_var("PINVOU3_HOME", value),
                None => std::env::remove_var("PINVOU3_HOME"),
            }
        }
        let _ = std::fs::remove_dir_all(tmp);
    }

    #[test]
    fn lifecycle_marker_requires_exact_status_header_and_frozen_route_identity() {
        let id = "01a08b7a-654d-7e8d-8a7c-87b4506f2c7c";
        let mut headers = reqwest::header::HeaderMap::new();
        for code in ["MODEL_UNAVAILABLE", "MODEL_DEPRECATED"] {
            headers.insert(super::MODEL_REJECTION_HEADER, code.parse().unwrap());
            let marker =
                super::model_rejection_from_headers(StatusCode::CONFLICT, &headers, id).unwrap();
            assert_eq!(marker.model_id, id);
            for status in [
                StatusCode::OK,
                StatusCode::UNAUTHORIZED,
                StatusCode::FORBIDDEN,
                StatusCode::INTERNAL_SERVER_ERROR,
            ] {
                assert!(super::model_rejection_from_headers(status, &headers, id).is_none());
            }
        }
        for code in [
            "MODEL_REMOVED",
            " MODEL_UNAVAILABLE",
            "MODEL_UNAVAILABLE bogus",
            "model_unavailable",
        ] {
            headers.insert(super::MODEL_REJECTION_HEADER, code.parse().unwrap());
            assert!(
                super::model_rejection_from_headers(StatusCode::CONFLICT, &headers, id).is_none()
            );
        }
        headers.insert(
            super::MODEL_REJECTION_HEADER,
            "MODEL_UNAVAILABLE".parse().unwrap(),
        );
        assert!(
            super::model_rejection_from_headers(StatusCode::CONFLICT, &headers, "invalid")
                .is_none()
        );
        headers.remove(super::MODEL_REJECTION_HEADER);
        assert!(
            super::model_rejection_from_headers(StatusCode::CONFLICT, &headers, id).is_none(),
            "body error text cannot produce evidence"
        );
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
        for data in [
            b"data: {\"choices\":[],\"error\":null}\n\n".as_slice(),
            b"data: {\"choices\":[],\"error\":null}\r\n\r\n".as_slice(),
        ] {
            let chunks = require_sse_done(stream::iter(vec![Ok::<_, io::Error>(
                Bytes::copy_from_slice(data),
            )]))
            .collect::<Vec<_>>()
            .await;
            assert_eq!(chunks.len(), 2);
            assert_eq!(
                chunks[1].as_ref().unwrap_err().kind(),
                io::ErrorKind::UnexpectedEof
            );
        }
    }

    #[test]
    fn catalog_proxy_uses_existing_account_read_budget_without_shortening_chat() {
        let client = reqwest::Client::new();
        for (resource, method, expected) in [
            (
                "models",
                reqwest::Method::GET,
                Some(super::super::ACCOUNT_API_REQUEST_TIMEOUT),
            ),
            ("chat/completions", reqwest::Method::POST, None),
        ] {
            let request = super::model_request_budget(
                client.request(method.clone(), "http://127.0.0.1/v1/models"),
                resource,
                &method,
            )
            .build()
            .unwrap();
            assert_eq!(request.timeout().copied(), expected);
        }
    }

    #[tokio::test]
    async fn real_proxy_payment_body_enforces_existing_bound_and_completeness() {
        let notice = br#"{"code":"WALLET_INSUFFICIENT","billing_notice":{"schema_version":1,"code":"WALLET_INSUFFICIENT","stage":"INITIAL_RESERVATION","turn_outcome":"REJECTED","settlement_status":"NOT_STARTED","action":"OPEN_RECHARGE","context_type":"PERSONAL","usage_request_id":"0199a100-0001-7000-8000-000000000106"}}"#;
        let bound = super::super::MAX_RESPONSE_BYTES;
        for (size, chunked, broken, expected) in [
            (notice.len(), false, false, StatusCode::PAYMENT_REQUIRED),
            (bound, false, false, StatusCode::PAYMENT_REQUIRED),
            (bound + 1, false, false, StatusCode::BAD_GATEWAY),
            (bound + 1, true, false, StatusCode::BAD_GATEWAY),
            (notice.len(), true, true, StatusCode::BAD_GATEWAY),
        ] {
            let mut payload = notice.to_vec();
            payload.resize(size, b' ');
            let expected_payload = payload.clone();
            let upstream = Router::new().route(
                "/v1/chat/completions",
                post(move || {
                    let payload = payload.clone();
                    async move {
                        let body = if broken {
                            Body::from_stream(stream::unfold(0, move |phase| {
                                let payload = payload.clone();
                                async move {
                                    match phase {
                                        0 => Some((Ok::<_, io::Error>(Bytes::from(payload)), 1)),
                                        1 => {
                                            tokio::time::sleep(std::time::Duration::from_millis(
                                                20,
                                            ))
                                            .await;
                                            Some((
                                                Err(io::Error::new(
                                                    io::ErrorKind::UnexpectedEof,
                                                    "controlled incomplete body",
                                                )),
                                                2,
                                            ))
                                        }
                                        _ => None,
                                    }
                                }
                            }))
                        } else if chunked {
                            let chunks = payload
                                .chunks(8192)
                                .map(Bytes::copy_from_slice)
                                .collect::<Vec<_>>();
                            Body::from_stream(stream::iter(
                                chunks.into_iter().map(Ok::<_, io::Error>),
                            ))
                        } else {
                            Body::from(payload)
                        };
                        Response::builder()
                            .status(StatusCode::PAYMENT_REQUIRED)
                            .header(header::CONTENT_TYPE, "application/json")
                            .body(body)
                            .unwrap()
                    }
                }),
            );
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let address = listener.local_addr().unwrap();
            let task = tokio::spawn(async move { axum::serve(listener, upstream).await.unwrap() });
            let proxy = ModelGatewayProxy::default();
            let authority = Arc::new(TestTurnAuthority::default());
            authority.set(Some(1));
            proxy.bind_turn_authority(authority).unwrap();
            let base = proxy
                .register_session(
                    "bounded-payment-session",
                    &format!("http://{address}/v1"),
                    "11111111-1111-4111-8111-111111111111",
                    128,
                )
                .await
                .unwrap();
            let response = reqwest::Client::new()
                .post(format!("{base}/chat/completions"))
                .bearer_auth("controlled-user")
                .header("Idempotency-Key", "bounded-payment-invocation-key")
                .json(&serde_json::json!({"model":"model","messages":[],"stream":false}))
                .send()
                .await
                .unwrap();
            assert_eq!(
                response.status(),
                expected,
                "size={size} chunked={chunked} broken={broken}"
            );
            let delivered = response.bytes().await.unwrap();
            if expected == StatusCode::PAYMENT_REQUIRED {
                assert_eq!(delivered.as_ref(), expected_payload.as_slice());
                assert!(billing_notice_from_body(&delivered).is_some());
            } else {
                assert_ne!(delivered.as_ref(), expected_payload.as_slice());
                assert!(billing_notice_from_body(&delivered).is_none());
            }
            task.abort();
        }
    }

    #[test]
    fn sse_billing_notice_requires_gateway_framed_success() {
        for (status, media_type, expected) in [
            (StatusCode::OK, "text/event-stream", true),
            (StatusCode::PARTIAL_CONTENT, "text/event-stream", true),
            (StatusCode::BAD_REQUEST, "text/event-stream", false),
            (StatusCode::PAYMENT_REQUIRED, "text/event-stream", false),
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                "text/event-stream",
                false,
            ),
            (StatusCode::OK, "Text/Event-Stream", false),
            (StatusCode::OK, " text/event-stream", false),
            (StatusCode::OK, "text/event-stream; charset=utf-8", false),
            (StatusCode::OK, "text/event-stream-extra", false),
            (StatusCode::OK, "application/json", false),
        ] {
            let mut headers = HeaderMap::new();
            headers.insert(header::CONTENT_TYPE, media_type.parse().unwrap());
            assert_eq!(accepts_sse_billing_notice(status, &headers), expected);
        }
        assert!(!accepts_sse_billing_notice(
            StatusCode::OK,
            &HeaderMap::new()
        ));
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
    async fn real_proxy_explicit_stop_cancels_but_reclaim_only_drops_late_delivery() {
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
                .route(
                    "/v1/usage-requests/{id}",
                    get(|| async { StatusCode::FORBIDDEN }),
                )
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
            if reclaim {
                sleep(Duration::from_millis(50)).await;
                assert_eq!(*f.reads.lock().unwrap(), 0);
                assert!(f.posts.lock().unwrap().is_empty());
            } else {
                tokio::time::timeout(Duration::from_secs(3), async {
                    while *f.reads.lock().unwrap() == 0 {
                        sleep(Duration::from_millis(10)).await;
                    }
                })
                .await
                .unwrap();
                let keys = f.posts.lock().unwrap();
                assert_eq!(keys.len(), 2);
                assert_eq!(keys[0], keys[1]);
            }
            // Unsupported provider cancellation preserves server PENDING funds;
            // the client only stops delivery and queries the authoritative state.
            task.abort();
        }
    }

    #[tokio::test]
    async fn real_proxy_transport_loss_and_sse_completion_do_not_request_cancel() {
        #[derive(Clone)]
        struct DeliveryFixture {
            id: uuid::Uuid,
            mode: &'static str,
            release: Arc<tokio::sync::Notify>,
            posts: Arc<Mutex<usize>>,
            models: Arc<Mutex<Vec<String>>>,
        }
        async fn deliver(
            State(f): State<DeliveryFixture>,
            headers: axum::http::HeaderMap,
        ) -> Response<Body> {
            assert_eq!(headers[header::AUTHORIZATION], "Bearer frozen-user");
            let key = headers["idempotency-key"].to_str().unwrap().to_owned();
            assert_eq!(key, "original-transport-operation-key");
            f.models.lock().unwrap().push(key);
            let id = f.id;
            let body = stream::unfold((f, 0), |(f, stage)| async move {
                if stage == 0 {
                    let first = if matches!(f.mode, "complete" | "done-marker-drop") {
                        Bytes::from_static(b"data: [DONE]\n\n")
                    } else {
                        Bytes::from_static(b"data: {\"text\":\"partial\"}\n\n")
                    };
                    return Some((Ok::<_, io::Error>(first), (f, 1)));
                }
                if stage != 1 || f.mode == "complete" {
                    return None;
                }
                f.release.notified().await;
                match f.mode {
                    "read-error" => Some((
                        Err(io::Error::other("controlled upstream read failure")),
                        (f, 2),
                    )),
                    "missing-final" => None,
                    _ => Some((Ok(Bytes::from_static(b"data: [DONE]\n\n")), (f, 2))),
                }
            });
            Response::builder()
                .status(StatusCode::OK)
                .header(header::CONTENT_TYPE, "text/event-stream")
                .header("X-Pinvou-Usage-Request-ID", id.to_string())
                .body(Body::from_stream(body))
                .unwrap()
        }
        async fn unwanted_cancel(
            State(f): State<DeliveryFixture>,
            Path(id): Path<String>,
        ) -> Json<serde_json::Value> {
            assert_eq!(id, f.id.to_string());
            *f.posts.lock().unwrap() += 1;
            Json(json!({"usage_request_id": f.id, "cancel_status": "LOCAL_CONFIRMED"}))
        }
        async fn observed_usage(State(f): State<DeliveryFixture>) -> Json<serde_json::Value> {
            Json(
                json!({"usage_request_id": f.id, "cancel_status": "LOCAL_CONFIRMED", "settlement_status": "PENDING"}),
            )
        }
        for mode in [
            "read-error",
            "missing-final",
            "consumer-drop",
            "done-marker-drop",
            "complete",
        ] {
            let f = DeliveryFixture {
                id: uuid::Uuid::new_v4(),
                mode,
                release: Arc::new(tokio::sync::Notify::new()),
                posts: Arc::new(Mutex::new(0)),
                models: Arc::new(Mutex::new(Vec::new())),
            };
            let router = Router::new()
                .route("/v1/chat/completions", post(deliver))
                .route("/v1/usage-requests/{id}/cancel", post(unwanted_cancel))
                .route("/v1/usage-requests/by-idempotency-key", get(observed_usage))
                .with_state(f.clone());
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let origin = format!("http://{}/v1", listener.local_addr().unwrap());
            let task = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
            let proxy = ModelGatewayProxy::default();
            let authority = Arc::new(TestTurnAuthority::default());
            authority.set(Some(11));
            proxy.bind_turn_authority(authority).unwrap();
            let base = proxy
                .register_session("transport-session", &origin, "public-model-id", 128)
                .await
                .unwrap();
            let response = reqwest::Client::new()
                .post(format!("{base}/chat/completions"))
                .bearer_auth("frozen-user")
                .header("Idempotency-Key", "original-transport-operation-key")
                .json(&json!({"model":"engine-visible-model", "messages":[], "stream":true}))
                .send()
                .await
                .unwrap();
            assert_eq!(response.status(), StatusCode::OK);
            let mut source = response.bytes_stream();
            let first = source.next().await.unwrap().unwrap();
            assert!(first.starts_with(b"data:"));
            if matches!(mode, "consumer-drop" | "done-marker-drop") {
                drop(source); // A real HTTP consumer disconnect, not a Stop command.
            } else {
                f.release.notify_one();
                let rest = source.collect::<Vec<_>>().await;
                if mode == "complete" {
                    assert!(rest.iter().all(Result::is_ok));
                } else {
                    assert!(
                        rest.iter().any(Result::is_err),
                        "{mode} must actually fail delivery"
                    );
                }
            }
            sleep(Duration::from_millis(200)).await;
            assert_eq!(
                *f.posts.lock().unwrap(),
                0,
                "{mode} created a TOK-38 command"
            );
            assert_eq!(
                *f.models.lock().unwrap(),
                vec!["original-transport-operation-key"]
            );
            if mode == "consumer-drop" {
                // The actual HTTP Delivery is already gone. Explicit Stop in
                // the transparent retry gap must still address the original.
                proxy.cancel_turn("transport-session", 11);
                tokio::time::timeout(Duration::from_secs(3), async {
                    while *f.posts.lock().unwrap() == 0 {
                        sleep(Duration::from_millis(10)).await;
                    }
                })
                .await
                .unwrap();
                assert_eq!(*f.posts.lock().unwrap(), 1);
                let retry = reqwest::Client::new()
                    .post(format!("{base}/chat/completions"))
                    .bearer_auth("frozen-user")
                    .header("Idempotency-Key", "original-transport-operation-key")
                    .json(&json!({"model":"engine-visible-model", "messages":[], "stream":true}))
                    .send()
                    .await
                    .unwrap();
                assert_eq!(retry.status(), StatusCode::CONFLICT);
                assert_eq!(
                    f.models.lock().unwrap().len(),
                    1,
                    "Stop must fence model redispatch"
                );
            }
            f.release.notify_one();
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
            if pre_cancel {
                tokio::time::timeout(Duration::from_secs(3), async {
                    while old.posts.lock().unwrap().len() < 2 {
                        sleep(Duration::from_millis(10)).await;
                    }
                })
                .await
                .unwrap();
                assert_eq!(old.posts.lock().unwrap().len(), 2);
            } else {
                assert!(old.posts.lock().unwrap().is_empty());
                assert_eq!(*old.reads.lock().unwrap(), 0);
            }
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
                // Recording the retry POST precedes its response and receipt GET.
                // Wait for both observations before asserting either contract.
                while f.posts.lock().unwrap().len() < 2 || *f.reads.lock().unwrap() == 0 {
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

#[cfg(test)]
mod authorization_retry_tests {
    use super::tests::TestTurnAuthority;
    use super::*;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    struct WaitingAuthorization(tokio::sync::Notify);
    #[async_trait::async_trait]
    impl super::super::ModelAuthorization for WaitingAuthorization {
        async fn bearer(&self, _: Option<&str>) -> Result<String, String> {
            self.0.notify_one();
            std::future::pending().await
        }
    }

    #[tokio::test]
    async fn local_stop_interrupts_authorization_refresh_before_upstream() {
        let proxy = ModelGatewayProxy::default();
        let authority = Arc::new(TestTurnAuthority::default());
        authority.set(Some(7));
        proxy.bind_turn_authority(authority).unwrap();
        let supplier = Arc::new(WaitingAuthorization(tokio::sync::Notify::new()));
        let base = proxy
            .register_authorized_session(
                "waiting-session",
                "http://127.0.0.1:1/v1",
                "public-model",
                128,
                Some(supplier.clone()),
            )
            .await
            .unwrap();
        let pending = tokio::spawn(async move {
            reqwest::Client::new()
                .post(format!("{base}/chat/completions"))
                .bearer_auth("expired-engine-token")
                .header("Idempotency-Key", "operation-waiting-authorization")
                .json(&json!({"messages":[],"stream":true}))
                .send()
                .await
                .unwrap()
        });
        tokio::time::timeout(Duration::from_secs(2), supplier.0.notified())
            .await
            .unwrap();
        proxy.cancel_turn("waiting-session", 7);
        let response = tokio::time::timeout(Duration::from_secs(1), pending)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(response.status(), StatusCode::CONFLICT);
    }

    struct RotatingAuthorization {
        token: AtomicUsize,
        retired: AtomicBool,
        fail_refresh: AtomicBool,
    }

    struct ScopedAuthorization(&'static str);
    #[async_trait::async_trait]
    impl super::super::ModelAuthorization for ScopedAuthorization {
        fn scope(&self) -> Option<&str> {
            Some(self.0)
        }
        async fn bearer(&self, _: Option<&str>) -> Result<String, String> {
            Ok(format!("Bearer {}", self.0))
        }
    }

    #[tokio::test]
    async fn registering_new_login_retires_old_url_cached_target_and_cancel_scope() {
        let captures = Arc::new(StdMutex::new(Vec::new()));
        let router = Router::new().route(
            "/v1/chat/completions",
            post({
                let captures = captures.clone();
                move |headers: axum::http::HeaderMap| {
                    let captures = captures.clone();
                    async move {
                        captures
                            .lock()
                            .unwrap()
                            .push(headers[header::AUTHORIZATION].to_str().unwrap().to_string());
                        (
                            [(header::CONTENT_TYPE, "text/event-stream")],
                            "data: [DONE]\n\n",
                        )
                    }
                }
            }),
        );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let upstream = format!("http://{}/v1", listener.local_addr().unwrap());
        let peer = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
        let proxy = ModelGatewayProxy::default();
        let authority = Arc::new(TestTurnAuthority::default());
        authority.set(Some(7));
        proxy.bind_turn_authority(authority).unwrap();
        let old = proxy
            .register_authorized_session(
                "same-session",
                &upstream,
                "public-model",
                128,
                Some(Arc::new(ScopedAuthorization("user-a-session-1"))),
            )
            .await
            .unwrap();
        let old_scope = proxy.cancellation_scope("same-session");
        let mut cached = proxy
            .server
            .get()
            .unwrap()
            .state
            .registry
            .lock()
            .unwrap()
            .targets[old.split('/').nth(3).unwrap()]
        .clone();
        cached.turn_generation = 7;
        let new = proxy
            .register_authorized_session(
                "same-session",
                &upstream,
                "public-model",
                128,
                Some(Arc::new(ScopedAuthorization("user-b-session-2"))),
            )
            .await
            .unwrap();
        assert_ne!(old, new);
        assert_ne!(old_scope, proxy.cancellation_scope("same-session"));
        // Token rotation within this original login does not retire its route.
        let rotated = proxy
            .register_authorized_session(
                "same-session",
                &upstream,
                "public-model",
                128,
                Some(Arc::new(ScopedAuthorization("user-b-session-2"))),
            )
            .await
            .unwrap();
        assert_eq!(rotated, new);
        let body = json!({"messages":[],"stream":true});
        let client = reqwest::Client::new();
        let stale = client
            .post(format!("{old}/chat/completions"))
            .bearer_auth("old-engine-token")
            .header("Idempotency-Key", "operation-old-login")
            .json(&body)
            .send()
            .await
            .unwrap();
        assert_eq!(stale.status(), StatusCode::NOT_FOUND);
        let mut headers = axum::http::HeaderMap::new();
        headers.insert(
            header::AUTHORIZATION,
            "Bearer old-engine-token".parse().unwrap(),
        );
        let stale_cached = forward(
            &proxy.server.get().unwrap().state,
            cached,
            "chat/completions",
            reqwest::Method::POST,
            &headers,
            Some(Bytes::from(body.to_string())),
            Some("operation-old-login"),
            None,
            true,
        )
        .await;
        assert_eq!(stale_cached.status(), StatusCode::CONFLICT);
        proxy.cancel_turn_bound("same-session", 7, old_scope.as_deref());
        let fresh = client
            .post(format!("{new}/chat/completions"))
            .bearer_auth("new-engine-token")
            .header("Idempotency-Key", "operation-new-login")
            .json(&body)
            .send()
            .await
            .unwrap();
        assert_eq!(fresh.status(), StatusCode::OK);
        assert!(fresh.text().await.unwrap().contains("[DONE]"));
        assert_eq!(*captures.lock().unwrap(), vec!["Bearer user-b-session-2"]);
        peer.abort();
    }
    #[async_trait::async_trait]
    impl super::super::ModelAuthorization for RotatingAuthorization {
        async fn bearer(&self, rejected: Option<&str>) -> Result<String, String> {
            if self.retired.load(Ordering::SeqCst)
                || (rejected.is_some() && self.fail_refresh.load(Ordering::SeqCst))
            {
                return Err("authentication_required".into());
            }
            if rejected.is_some() {
                self.token.fetch_add(1, Ordering::SeqCst);
            }
            Ok(format!(
                "Bearer original-user-token-{}",
                self.token.load(Ordering::SeqCst)
            ))
        }
    }

    #[tokio::test]
    async fn cancellation_refreshes_original_authority_and_preserves_both_keys() {
        let captures = Arc::new(StdMutex::new(Vec::new()));
        let usage_id = uuid::Uuid::new_v4();
        let router = Router::new().fallback({
            let captures = captures.clone();
            move |request: Request<Body>| {
                let captures = captures.clone();
                async move {
                    let bearer = request.headers()[header::AUTHORIZATION].to_str().unwrap().to_string();
                    let key = request.headers()["idempotency-key"].to_str().unwrap().to_string();
                    let method = request.method().clone();
                    captures.lock().unwrap().push((method.clone(), bearer.clone(), key));
                    if bearer.ends_with("-2") { return StatusCode::UNAUTHORIZED.into_response(); }
                    assert_eq!(bearer, "Bearer original-user-token-3");
                    if method == reqwest::Method::POST { StatusCode::OK.into_response() }
                    else { Json(json!({"usage_request_id":usage_id,"cancel_status":"PROVIDER_CONFIRMED"})).into_response() }
                }
            }
        });
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let upstream =
            Url::parse(&format!("http://{}/v1", listener.local_addr().unwrap())).unwrap();
        let peer = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
        let supplier = Arc::new(RotatingAuthorization {
            token: AtomicUsize::new(1),
            retired: AtomicBool::new(false),
            fail_refresh: AtomicBool::new(false),
        });
        let calls = super::super::model_cancellation::Calls::default();
        let incarnation = calls.incarnation("cancel-session");
        let delivery = calls
            .track_authorized(
                &incarnation,
                7,
                upstream,
                "Bearer expired-engine-snapshot",
                "original-operation-key",
                reqwest::Client::new(),
                Some(supplier.clone()),
            )
            .unwrap();
        delivery.observe_usage(Some(&usage_id.to_string()));
        // A tool/approval wait outlives the token originally injected into Engine.
        supplier.token.store(2, Ordering::SeqCst);
        calls.cancel_turn("cancel-session", 7);
        tokio::time::timeout(Duration::from_secs(3), async {
            loop {
                if captures.lock().unwrap().len() >= 3 {
                    break;
                }
                sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        let requests = captures.lock().unwrap();
        assert_eq!(requests.len(), 3);
        assert_eq!(requests[0].0, reqwest::Method::POST);
        assert_eq!(requests[1].0, reqwest::Method::POST);
        assert_eq!(requests[0].2, requests[1].2);
        assert_eq!(requests[2].2, "original-operation-key");
        assert_eq!(requests[0].1, "Bearer original-user-token-2");
        assert_eq!(requests[1].1, "Bearer original-user-token-3");
        assert_eq!(requests[2].1, "Bearer original-user-token-3");
        peer.abort();
    }
    // A real HTTP peer consumes the entire operation, then loses the first ACK.
    // Request bytes prove that retry does not mint a second operation or turn.
    #[tokio::test]
    async fn transport_retry_and_401_keep_operation_body_and_original_authority() {
        for fail_refresh in [false, true] {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let upstream = format!("http://{}/v1", listener.local_addr().unwrap());
            let captured = Arc::new(StdMutex::new(Vec::<String>::new()));
            let captured_server = captured.clone();
            let peer = tokio::spawn(async move {
                for reply in [None, Some("401 Unauthorized"), Some("200 OK")] {
                    if fail_refresh && reply == Some("200 OK") {
                        break;
                    }
                    let (mut socket, _) = listener.accept().await.unwrap();
                    let mut data = Vec::new();
                    loop {
                        let mut buffer = [0; 4096];
                        let count = socket.read(&mut buffer).await.unwrap();
                        assert_ne!(count, 0);
                        data.extend_from_slice(&buffer[..count]);
                        if let Some(end) = data.windows(4).position(|v| v == b"\r\n\r\n") {
                            let header = String::from_utf8_lossy(&data[..end]);
                            let length = header
                                .lines()
                                .find_map(|line| {
                                    line.to_ascii_lowercase()
                                        .strip_prefix("content-length: ")
                                        .and_then(|v| v.parse::<usize>().ok())
                                })
                                .unwrap();
                            if data.len() >= end + 4 + length {
                                break;
                            }
                        }
                    }
                    captured_server
                        .lock()
                        .unwrap()
                        .push(String::from_utf8(data).unwrap());
                    if let Some(status) = reply {
                        let body = if status.starts_with("200") {
                            "data: [DONE]\n\n"
                        } else {
                            "{}"
                        };
                        let response = format!(
                            "HTTP/1.1 {status}\r\nContent-Type: text/event-stream\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                            body.len()
                        );
                        socket.write_all(response.as_bytes()).await.unwrap();
                    }
                }
            });
            let proxy = ModelGatewayProxy::default();
            let authority = Arc::new(TestTurnAuthority::default());
            authority.set(Some(7));
            proxy.bind_turn_authority(authority).unwrap();
            let supplier = Arc::new(RotatingAuthorization {
                token: AtomicUsize::new(1),
                retired: AtomicBool::new(false),
                fail_refresh: AtomicBool::new(fail_refresh),
            });
            let base = proxy
                .register_authorized_session(
                    "refresh-session",
                    &upstream,
                    "public-model",
                    128,
                    Some(supplier.clone()),
                )
                .await
                .unwrap();
            let client = reqwest::Client::new();
            let body = json!({"model":"engine-model","messages":[],"stream":true});
            let response = client
                .post(format!("{base}/chat/completions"))
                .bearer_auth("expired-engine-snapshot")
                .header("Idempotency-Key", "operation-stable-on-retry")
                .json(&body)
                .send()
                .await
                .unwrap();
            assert_eq!(
                response.status(),
                if fail_refresh {
                    StatusCode::UNAUTHORIZED
                } else {
                    StatusCode::OK
                }
            );
            let _ = response.text().await.unwrap();
            peer.await.unwrap();
            let requests = captured.lock().unwrap();
            assert_eq!(requests.len(), if fail_refresh { 2 } else { 3 });
            let payload = requests[0].split("\r\n\r\n").nth(1).unwrap();
            let turn = requests[0]
                .lines()
                .find(|line| {
                    line.to_ascii_lowercase()
                        .starts_with("x-pinvou-logical-turn-id:")
                })
                .unwrap();
            for (index, request) in requests.iter().enumerate() {
                assert_eq!(request.split("\r\n\r\n").nth(1).unwrap(), payload);
                assert!(request.contains("idempotency-key: operation-stable-on-retry"));
                assert!(request.contains(turn));
                assert!(request.contains(&format!(
                    "authorization: Bearer original-user-token-{}",
                    if index == 2 { 2 } else { 1 }
                )));
                assert!(!request.contains("expired-engine-snapshot"));
            }
            drop(requests);
            supplier.retired.store(true, Ordering::SeqCst);
            let denied = client
                .post(format!("{base}/chat/completions"))
                .bearer_auth("new-user-token")
                .header("Idempotency-Key", "operation-after-login-change")
                .json(&body)
                .send()
                .await
                .unwrap();
            assert_eq!(denied.status(), StatusCode::UNAUTHORIZED);
            assert_eq!(
                captured.lock().unwrap().len(),
                if fail_refresh { 2 } else { 3 }
            );
        }
    }
}
