//! Opt-in OpenAI LAN proxy. This owns listeners/requests, never model processes.
//! Store adapters supply current keys, routes and interface/isolation evidence.
use axum::{
    Json, Router,
    body::{Body, Bytes, to_bytes},
    extract::{ConnectInfo, State},
    http::{Request, StatusCode},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use futures_util::{StreamExt, stream};
use serde::Serialize;
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    net::{Ipv4Addr, SocketAddr},
    sync::Arc,
    time::Duration,
};
use tokio::{
    net::TcpListener,
    sync::{Mutex, mpsc, watch},
    task::JoinHandle,
};

#[derive(Clone, Debug, Serialize, PartialEq, Eq)]
pub struct Interface {
    pub address: Ipv4Addr,
    pub mask: Ipv4Addr,
    pub name: String,
}
impl Interface {
    pub fn accepts(&self, peer: std::net::IpAddr) -> bool {
        let std::net::IpAddr::V4(peer) = peer else {
            return false;
        };
        peer.is_private()
            && (u32::from(peer) & u32::from(self.mask))
                == (u32::from(self.address) & u32::from(self.mask))
    }
}
#[derive(Clone)]
pub struct Route {
    pub card_id: String,
    pub upstream: String,
    pub upstream_model: String,
}
impl Route {
    pub fn model_id(&self) -> String {
        format!("pinvou:{}", self.card_id)
    }
}

#[async_trait::async_trait]
pub trait Store: Send + Sync + 'static {
    fn interfaces(&self) -> Result<Vec<Interface>, String>;
    /// Legacy global keys must not silently acquire access to every shared model.
    async fn key_card(&self, key: &str) -> Result<Option<String>, String>;
    /// Must reject stopped models and unisolated raw engine ports.
    async fn route(&self, card: &str) -> Result<Route, String>;
}
struct SharedCard {
    stop: watch::Sender<bool>,
}
struct Session {
    interface: Interface,
    port: u16,
    cards: Arc<Mutex<HashMap<String, SharedCard>>>,
    stop: watch::Sender<bool>,
    task: JoinHandle<()>,
}
impl Drop for Session {
    fn drop(&mut self) {
        let _ = self.stop.send(true);
    }
}
#[derive(Default)]
pub struct Gateway {
    session: Mutex<Option<Session>>,
}
#[derive(Serialize)]
pub struct Status {
    pub enabled: bool,
    pub address: Option<Ipv4Addr>,
    pub port: u16,
    pub base_url: Option<String>,
    pub endpoint: Option<String>,
    pub model_id: String,
    pub interfaces: Vec<Interface>,
    pub available: bool,
    pub reason: Option<String>,
}
#[derive(Clone)]
struct App {
    store: Arc<dyn Store>,
    client: reqwest::Client,
    interface: Interface,
    cards: Arc<Mutex<HashMap<String, SharedCard>>>,
    stop: watch::Receiver<bool>,
    /// route 存活性检查的共享去重缓存（card -> 上次实查时刻）。监督循环按流
    /// 各起一个，K 条流不该变成 K 次/秒的 docker inspect——同秒内复用结论。
    route_checks: Arc<std::sync::Mutex<HashMap<String, std::time::Instant>>>,
}

impl Gateway {
    pub async fn status(&self, store: &Arc<dyn Store>, card: &str) -> Result<Status, String> {
        let interfaces = store.interfaces()?;
        // 快照在锁内拿全，慢检查一律在锁外（评审 P2）：route 验证含 docker inspect
        // 与上游 GET（各 ~3s），而前端 3s 一轮轮询 status——此前慢检查全程持
        // session 锁，set() 每次都要等一轮让路。
        let (enabled, address, port) = {
            let mut session = self.session.lock().await;
            if session.as_ref().is_some_and(|s| {
                *s.stop.borrow() || s.task.is_finished() || !interfaces.contains(&s.interface)
            }) && let Some(s) = session.take()
            {
                let _ = s.stop.send(true);
            }
            let enabled = match session.as_ref() {
                Some(s) => s.cards.lock().await.contains_key(card),
                None => false,
            };
            let (address, port) = session
                .as_ref()
                .map(|s| (Some(s.interface.address), s.port))
                .unwrap_or((None, 8090));
            (enabled, address, port)
        };
        let route = match store.route(card).await {
            Ok(route) => match local_client() {
                Ok(client) => verify_model(&client, &route)
                    .await
                    .map_err(|_| "lan_model_unavailable".to_string()),
                Err(e) => Err(e),
            },
            Err(e) => Err(e),
        };
        let base_url = if enabled {
            address.map(|ip| format!("http://{ip}:{port}/v1"))
        } else {
            None
        };
        Ok(Status {
            enabled,
            address,
            port,
            endpoint: base_url
                .as_ref()
                .map(|url| format!("{url}/chat/completions")),
            base_url,
            model_id: format!("pinvou:{card}"),
            interfaces,
            available: route.is_ok(),
            reason: route.err(),
        })
    }
    pub async fn set(
        &self,
        store: Arc<dyn Store>,
        card: String,
        enabled: bool,
        address: Option<String>,
        port: u16,
    ) -> Result<Status, String> {
        {
            let mut session = self.session.lock().await;
            if !enabled {
                if let Some(s) = session.as_ref() {
                    if let Some(c) = s.cards.lock().await.remove(&card) {
                        let _ = c.stop.send(true);
                    }
                    if s.cards.lock().await.is_empty()
                        && let Some(mut s) = session.take()
                    {
                        let _ = s.stop.send(true);
                        let _ = tokio::time::timeout(Duration::from_secs(2), &mut s.task).await;
                    }
                }
            } else {
                let ip: Ipv4Addr = address
                    .ok_or("lan_select_address")?
                    .parse()
                    .map_err(|_| "lan_select_address")?;
                if !ip.is_private() || port < 1024 {
                    return Err("lan_select_address".into());
                }
                let interface = store
                    .interfaces()?
                    .into_iter()
                    .find(|i| i.address == ip)
                    .ok_or("lan_address_changed")?;
                // r5 评审 P2-2：route+verify_model 最坏 ~6s（docker inspect + 上游
                // GET），此前全程持 session 锁——前端 3s 一轮的 status 轮询被卡住，
                // 启用/停用按钮看起来"死"了。慢验证在锁外做；锁内只留快校验与
                // 状态提交（下方第二段重新拿锁）。
                drop(session);
                let route = store.route(&card).await?;
                let client = local_client()?;
                verify_model(&client, &route)
                    .await
                    .map_err(|_| "lan_model_unavailable")?;
                let mut session = self.session.lock().await;
                if session
                    .as_ref()
                    .is_some_and(|s| *s.stop.borrow() || s.task.is_finished())
                {
                    session.take();
                }
                if let Some(s) = session.as_ref() {
                    if s.interface != interface || s.port != port {
                        return Err("lan_listener_in_use".into());
                    }
                    s.cards
                        .lock()
                        .await
                        .entry(card.clone())
                        .or_insert_with(|| SharedCard {
                            stop: watch::channel(false).0,
                        });
                } else {
                    let listener = TcpListener::bind((ip, port))
                        .await
                        .map_err(|_| "lan_bind_failed")?;
                    let cards = Arc::new(Mutex::new(HashMap::from([(
                        card.clone(),
                        SharedCard {
                            stop: watch::channel(false).0,
                        },
                    )])));
                    let (stop, receiver) = watch::channel(false);
                    let app = App {
                        store: store.clone(),
                        client,
                        interface: interface.clone(),
                        cards: cards.clone(),
                        stop: receiver.clone(),
                        route_checks: Arc::new(std::sync::Mutex::new(HashMap::new())),
                    };
                    let task = tokio::spawn(serve(listener, app, receiver, stop.clone()));
                    *session = Some(Session {
                        interface,
                        port,
                        cards,
                        stop,
                        task,
                    });
                }
            }
        }
        self.status(&store, &card).await
    }
}

fn local_client() -> Result<reqwest::Client, String> {
    reqwest::Client::builder()
        .no_proxy()
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(Duration::from_secs(3))
        .build()
        .map_err(|_| "lan_unavailable".into())
}

async fn stopped(mut receiver: watch::Receiver<bool>) {
    while !*receiver.borrow_and_update() {
        if receiver.changed().await.is_err() {
            break;
        }
    }
}
async fn serve(
    listener: TcpListener,
    app: App,
    stop: watch::Receiver<bool>,
    cancel: watch::Sender<bool>,
) {
    let server = Router::new()
        .route("/v1/models", get(models))
        .route("/v1/chat/completions", post(chat))
        .fallback(|| async {
            error(
                StatusCode::NOT_FOUND,
                "unsupported_endpoint",
                "Only /v1/models and /v1/chat/completions are supported.",
            )
        })
        .with_state(app.clone());
    let monitor = async move {
        loop {
            tokio::time::sleep(Duration::from_secs(1)).await;
            if !app
                .store
                .interfaces()
                .is_ok_and(|items| items.contains(&app.interface))
            {
                break;
            }
        }
    };
    let shutdown = async move {
        tokio::select! { _ = stopped(stop) => {}, _ = monitor => {} };
        let _ = cancel.send(true);
    };
    let _ = axum::serve(
        listener,
        server.into_make_service_with_connect_info::<SocketAddr>(),
    )
    .with_graceful_shutdown(shutdown)
    .await;
}

fn error(status: StatusCode, code: &str, message: &str) -> Response {
    (
        status,
        Json(json!({"error":{"message":message,"type":"invalid_request_error","code":code}})),
    )
        .into_response()
}

#[derive(Clone, Copy)]
struct ApiError {
    status: StatusCode,
    code: &'static str,
    message: &'static str,
}
impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        error(self.status, self.code, self.message)
    }
}
fn rejection(status: StatusCode, code: &'static str, message: &'static str) -> ApiError {
    ApiError {
        status,
        code,
        message,
    }
}

async fn authorize(
    app: &App,
    peer: SocketAddr,
    headers: &axum::http::HeaderMap,
    query: Option<&str>,
) -> Result<(String, String), ApiError> {
    if !app.interface.accepts(peer.ip()) {
        return Err(rejection(
            StatusCode::FORBIDDEN,
            "lan_only",
            "Use the selected local network.",
        ));
    }
    if query.is_some() || headers.contains_key("origin") {
        return Err(rejection(
            StatusCode::BAD_REQUEST,
            "invalid_request",
            "Use an API client with an Authorization header and no URL parameters.",
        ));
    }
    if *app.stop.borrow() {
        return Err(rejection(
            StatusCode::SERVICE_UNAVAILABLE,
            "sharing_disabled",
            "LAN sharing is disabled.",
        ));
    }
    let mut auth = headers.get_all("authorization").iter();
    let key = auth
        .next()
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .filter(|v| !v.is_empty() && v.len() <= 256);
    if auth.next().is_some() {
        return Err(rejection(
            StatusCode::UNAUTHORIZED,
            "invalid_api_key",
            "Supply a valid model API key.",
        ));
    }
    let key = key.ok_or_else(|| {
        rejection(
            StatusCode::UNAUTHORIZED,
            "invalid_api_key",
            "Supply a valid model API key.",
        )
    })?;
    let card = app
        .store
        .key_card(key)
        .await
        .map_err(|_| {
            rejection(
                StatusCode::SERVICE_UNAVAILABLE,
                "key_store_unavailable",
                "Key verification is unavailable.",
            )
        })?
        .ok_or_else(|| {
            rejection(
                StatusCode::UNAUTHORIZED,
                "invalid_api_key",
                "The key is invalid, revoked, or a legacy global key. Create a model key.",
            )
        })?;
    if !app.cards.lock().await.contains_key(&card) {
        return Err(rejection(
            StatusCode::FORBIDDEN,
            "sharing_disabled",
            "LAN sharing is disabled for this model.",
        ));
    }
    Ok((key.to_string(), card))
}
async fn verified_route(app: &App, card: &str) -> Result<Route, ApiError> {
    let route = app.store.route(card).await.map_err(|_| rejection(StatusCode::SERVICE_UNAVAILABLE, "model_unavailable", "Model is stopped, unavailable, or its raw port is not isolated. Check the model on the host."))?;
    verify_model(&app.client, &route).await.map_err(|_| {
        rejection(
            StatusCode::SERVICE_UNAVAILABLE,
            "model_unavailable",
            "The expected model is not available on the host.",
        )
    })?;
    Ok(route)
}
async fn verify_model(client: &reqwest::Client, route: &Route) -> Result<(), ()> {
    let response = client
        .get(format!("{}/models", route.upstream))
        .timeout(Duration::from_secs(3))
        .send()
        .await
        .map_err(|_| ())?;
    if !response.status().is_success() || response.content_length().is_some_and(|n| n > 1024 * 1024)
    {
        return Err(());
    }
    let mut body = Vec::new();
    let mut chunks = response.bytes_stream();
    while let Some(chunk) = chunks.next().await {
        let chunk = chunk.map_err(|_| ())?;
        if body.len() + chunk.len() > 1024 * 1024 {
            return Err(());
        }
        body.extend_from_slice(&chunk);
    }
    let value: Value = serde_json::from_slice(&body).map_err(|_| ())?;
    if value["data"].as_array().is_some_and(|items| {
        items
            .iter()
            .any(|m| m["id"].as_str() == Some(&route.upstream_model))
    }) {
        Ok(())
    } else {
        Err(())
    }
}
async fn models(
    State(app): State<App>,
    ConnectInfo(peer): ConnectInfo<SocketAddr>,
    request: Request<Body>,
) -> Response {
    let (_, card) = match authorize(&app, peer, request.headers(), request.uri().query()).await {
        Ok(v) => v,
        Err(r) => return r.into_response(),
    };
    let route = match verified_route(&app, &card).await {
        Ok(v) => v,
        Err(r) => return r.into_response(),
    };
    Json(json!({"object":"list", "data":[{"id":route.model_id(),"object":"model","created":0,"owned_by":"pinvou"}]})).into_response()
}

struct AbortOnDrop(JoinHandle<()>);
impl Drop for AbortOnDrop {
    fn drop(&mut self) {
        self.0.abort();
    }
}

fn sharing_disabled() -> ApiError {
    rejection(
        StatusCode::SERVICE_UNAVAILABLE,
        "sharing_disabled",
        "LAN sharing is disabled.",
    )
}

async fn recheck_key(app: &App, key: &str, card: &str) -> Result<(), ApiError> {
    match app.store.key_card(key).await {
        Ok(Some(current)) if current == card => Ok(()),
        Ok(_) => Err(rejection(
            StatusCode::UNAUTHORIZED,
            "invalid_api_key",
            "The model key is invalid or revoked.",
        )),
        Err(_) => Err(rejection(
            StatusCode::SERVICE_UNAVAILABLE,
            "key_store_unavailable",
            "Key verification is unavailable.",
        )),
    }
}

async fn cancelled(mut signal: watch::Receiver<Option<ApiError>>) -> ApiError {
    loop {
        if let Some(reason) = *signal.borrow_and_update() {
            return reason;
        }
        if signal.changed().await.is_err() {
            return sharing_disabled();
        }
    }
}

/// Request-owned supervision starts before body upload and survives HTTP
/// backpressure. Key and route clocks are independent: a slow route lookup must
/// not delay key revocation, nor may fast chunks restart either clock.
/// route 存活检查的共享去重（评审 P2-2）：1s 内已有任一流实查过这张卡，
/// 其余流直接认定仍存活。实查失败时**不写缓存**（失败结论立即广播给所有流）。
async fn route_check_fresh(app: &App, card: &str) -> bool {
    const FRESH_FOR: Duration = Duration::from_secs(1);
    {
        let cache = app.route_checks.lock().unwrap();
        if let Some(last) = cache.get(card) {
            if last.elapsed() < FRESH_FOR {
                return true;
            }
        }
    }
    if app.store.route(card).await.is_err() {
        return false;
    }
    app.route_checks
        .lock()
        .unwrap()
        .insert(card.to_string(), std::time::Instant::now());
    true
}

fn supervise(
    app: App,
    key: String,
    card: String,
    card_stop: watch::Receiver<bool>,
) -> (AbortOnDrop, watch::Receiver<Option<ApiError>>) {
    let (signal, receiver) = watch::channel(None);
    let task = tokio::spawn(async move {
        // r5 评审 P2-2：监督按流各起一个循环，K 条卡住的流 = K 次/秒的 docker
        // inspect（route）+ SQLite 连接（key），可饿坏共享 spawn_blocking 池。
        // 不能简单放宽间隔——撤销亚秒生效是 PR 正文承诺且有 3s 收敛测试钉住。
        // 修法：key 重检保持 1s（本地 SQLite 开销极小）；route 重检加**共享 1s
        // 去重缓存**——同一秒内已由任一流检查过的卡，其余流直接复用结论，
        // K 条流的合计成本从 K 次/秒降为 1 次/秒。
        let keys = async {
            let mut tick = tokio::time::interval(Duration::from_secs(1));
            tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
            loop {
                tick.tick().await;
                if let Err(reason) = recheck_key(&app, &key, &card).await {
                    return reason;
                }
            }
        };
        let routes = async {
            let mut tick = tokio::time::interval(Duration::from_secs(1));
            tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
            loop {
                tick.tick().await;
                if !route_check_fresh(&app, &card).await {
                    return rejection(
                        StatusCode::SERVICE_UNAVAILABLE,
                        "model_unavailable",
                        "Model is stopped or unavailable.",
                    );
                }
            }
        };
        let reason = tokio::select! {
            biased;
            _ = stopped(app.stop.clone()) => sharing_disabled(),
            _ = stopped(card_stop) => sharing_disabled(),
            reason = keys => reason,
            reason = routes => reason,
        };
        let _ = signal.send(Some(reason));
    });
    (AbortOnDrop(task), receiver)
}

async fn chat(
    State(app): State<App>,
    ConnectInfo(peer): ConnectInfo<SocketAddr>,
    request: Request<Body>,
) -> Response {
    let (key, card) = match authorize(&app, peer, request.headers(), request.uri().query()).await {
        Ok(v) => v,
        Err(r) => return r.into_response(),
    };
    let card_stop = match app.cards.lock().await.get(&card) {
        Some(c) => c.stop.subscribe(),
        None => {
            return error(
                StatusCode::FORBIDDEN,
                "sharing_disabled",
                "LAN sharing is disabled.",
            );
        }
    };
    let (monitor, cancellation) =
        supervise(app.clone(), key.clone(), card.clone(), card_stop.clone());
    let opened = tokio::select! {
        biased;
        reason = cancelled(cancellation.clone()) => return reason.into_response(),
        result = open_chat(&app, &key, &card, &card_stop, request) => result,
    };
    let (response, content_type) = match opened {
        Ok(value) => value,
        Err(reason) => return reason.into_response(),
    };

    // At most two 16 KiB chunks are queued, plus one pending send and the
    // current reqwest frame (HTTP/TCP buffers are owned by their libraries).
    // The producer owns upstream and is polled independently of the HTTP body.
    // Every await (including a full queue) remains cancellable. Dropping the body
    // aborts the producer, which in turn drops upstream and its monitor.
    let (sender, receiver) = mpsc::channel::<Result<Bytes, reqwest::Error>>(2);
    let producer = tokio::spawn(async move {
        let _monitor = monitor;
        let mut last_key_check = std::time::Instant::now();
        let pump = async {
            let mut bytes = response.bytes_stream();
            loop {
                let step = async {
                    let Some(item) = bytes.next().await else {
                        return false;
                    };
                    let chunk = match item {
                        Ok(chunk) => chunk,
                        Err(e) => {
                            let _ = sender.send(Err(e)).await;
                            return false;
                        }
                    };
                    // Retain the original per-upstream-chunk key check, but
                    // throttled (评审 P2): 撤销即时性已有 1s 时钟 + POST 前
                    // reauthorize 兜底，逐 chunk 的 SQLite 重开连接只是冗余放大。
                    if last_key_check.elapsed() >= Duration::from_millis(500) {
                        last_key_check = std::time::Instant::now();
                        if recheck_key(&app, &key, &card).await.is_err() {
                            return false;
                        }
                    }
                    for part in chunk.chunks(16 * 1024) {
                        if sender.send(Ok(Bytes::copy_from_slice(part))).await.is_err() {
                            return false;
                        }
                    }
                    true
                };
                if !matches!(
                    tokio::time::timeout(Duration::from_secs(120), step).await,
                    Ok(true)
                ) {
                    break;
                }
            }
        };
        tokio::select! {
            biased;
            _ = cancelled(cancellation) => {},
            _ = sender.closed() => {},
            _ = pump => {},
        }
    });
    let output = stream::unfold(
        (receiver, AbortOnDrop(producer)),
        |(mut receiver, owner)| async move {
            receiver.recv().await.map(|item| (item, (receiver, owner)))
        },
    );
    (
        [
            ("content-type", content_type),
            ("cache-control", "no-store"),
            ("x-accel-buffering", "no"),
        ],
        Body::from_stream(output),
    )
        .into_response()
}

async fn open_chat(
    app: &App,
    key: &str,
    card: &str,
    card_stop: &watch::Receiver<bool>,
    request: Request<Body>,
) -> Result<(reqwest::Response, &'static str), ApiError> {
    let bytes = match tokio::time::timeout(
        Duration::from_secs(10),
        to_bytes(request.into_body(), 4 * 1024 * 1024),
    )
    .await
    {
        Ok(Ok(v)) => v,
        _ => {
            return Err(rejection(
                StatusCode::BAD_REQUEST,
                "invalid_body",
                "Send a JSON body smaller than 4 MiB within 10 seconds.",
            ));
        }
    };
    let mut body: Value = match serde_json::from_slice(&bytes) {
        Ok(Value::Object(v)) => Value::Object(v),
        _ => {
            return Err(rejection(
                StatusCode::BAD_REQUEST,
                "invalid_json",
                "Send a JSON object.",
            ));
        }
    };
    if body["model"].as_str() != Some(format!("pinvou:{card}").as_str()) {
        return Err(rejection(
            StatusCode::NOT_FOUND,
            "model_not_found",
            "Use the model ID returned by /v1/models for this key.",
        ));
    }
    let route = verified_route(app, card).await?;
    body["model"] = Value::String(route.upstream_model);
    // Upload/probe may have waited for seconds since initial authorization.
    // Reauthorize immediately before POST; do not wait for the periodic check.
    recheck_key(app, key, card).await?;
    if *app.stop.borrow() || *card_stop.borrow() {
        return Err(sharing_disabled());
    }
    let upstream = app
        .client
        .post(format!("{}/chat/completions", route.upstream))
        .json(&body)
        .send();
    let response = match tokio::time::timeout(Duration::from_secs(120), upstream).await {
        Ok(Ok(value)) => value,
        _ => {
            return Err(rejection(
                StatusCode::BAD_GATEWAY,
                "upstream_unavailable",
                "Model request failed. Check the model and retry.",
            ));
        }
    };
    let status = response.status();
    if !status.is_success() {
        // Never relay model-server diagnostics (which can echo prompts/secrets).
        return Err(rejection(
            status,
            "upstream_error",
            "The model rejected the request. Check the request or the model on the host.",
        ));
    }
    if body["stream"] == true
        && !response
            .headers()
            .get("content-type")
            .and_then(|value| value.to_str().ok())
            .is_some_and(|value| value.starts_with("text/event-stream"))
    {
        return Err(rejection(
            StatusCode::BAD_GATEWAY,
            "upstream_protocol_error",
            "The model did not return an SSE stream. Check its streaming support on the host.",
        ));
    }
    let content_type = if body["stream"] == true {
        "text/event-stream"
    } else {
        "application/json"
    };
    Ok((response, content_type))
}
