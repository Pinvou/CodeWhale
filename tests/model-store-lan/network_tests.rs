use crate::lan_gateway::*;
use axum::{
    Json, Router,
    body::Body,
    http::StatusCode,
    response::IntoResponse,
    routing::{get, post},
};
use futures_util::{StreamExt, stream};
use serde_json::{Value, json};
use std::sync::{
    Mutex as StdMutex,
    atomic::{AtomicBool, AtomicUsize, Ordering},
};
use std::{
    collections::HashMap,
    net::{IpAddr, Ipv4Addr},
    sync::Arc,
    time::Duration,
};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::{net::TcpListener, task::JoinHandle};

async fn until(mut predicate: impl FnMut() -> bool) {
    tokio::time::timeout(Duration::from_secs(3), async {
        while !predicate() {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("condition did not converge within 3 seconds");
}

async fn headers(socket: &mut tokio::net::TcpStream) -> String {
    let mut bytes = Vec::new();
    while !bytes.ends_with(b"\r\n\r\n") {
        let byte = socket.read_u8().await.expect("headers closed early");
        bytes.push(byte);
        assert!(bytes.len() < 16384);
    }
    String::from_utf8(bytes).unwrap()
}

async fn upload(f: &Fixture, port: u16, text: &str) -> (tokio::net::TcpStream, Vec<u8>) {
    use std::os::fd::AsRawFd;
    let mut socket = tokio::net::TcpStream::connect((f.ip, port)).await.unwrap();
    let size: libc::c_int = 1024;
    // Keep the downstream open but force finite TCP backpressure quickly.
    assert_eq!(
        unsafe {
            libc::setsockopt(
                socket.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_RCVBUF,
                (&size as *const libc::c_int).cast(),
                std::mem::size_of_val(&size) as libc::socklen_t,
            )
        },
        0
    );
    let body = serde_json::to_vec(&json!({"model":"pinvou:a","stream":true,
        "messages":[{"role":"user","content":text}]}))
    .unwrap();
    socket.write_all(format!("POST /v1/chat/completions HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer test-a\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n", body.len()).as_bytes()).await.unwrap();
    socket.write_all(&body[..1]).await.unwrap();
    until(|| f.store.key_checks.load(Ordering::SeqCst) > 0).await;
    (socket, body[1..].to_vec())
}

async fn revoked_upload(complete: bool) {
    let f = Fixture::new().await;
    let port = f.free_port().await;
    f.enable("a", port).await;
    let (mut socket, rest) = upload(&f, port, "hello").await;
    crate::db::delete_api_key("test-a").unwrap();
    if complete {
        socket.write_all(&rest).await.unwrap();
    }
    let response = tokio::time::timeout(Duration::from_secs(3), headers(&mut socket))
        .await
        .expect("revoked upload was left waiting for the 10s body timeout");
    assert!(response.starts_with("HTTP/1.1 401"), "{response}");
    assert!(
        f.seen.lock().unwrap().is_empty(),
        "revoked upload started inference"
    );
}

#[tokio::test]
async fn revocation_cancels_an_incomplete_upload() {
    revoked_upload(false).await;
}

#[tokio::test]
async fn revocation_is_rechecked_before_sending_a_completed_upload() {
    revoked_upload(true).await;
}

#[tokio::test]
async fn revocation_during_delayed_headers_disconnects_upstream() {
    let f = Fixture::new().await;
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let started = Arc::new(AtomicBool::new(false));
    let disconnected = Arc::new(AtomicBool::new(false));
    let (seen, gone) = (started.clone(), disconnected.clone());
    let server = tokio::spawn(async move {
        loop {
            let (mut socket, _) = listener.accept().await.unwrap();
            let (seen, gone) = (seen.clone(), gone.clone());
            tokio::spawn(async move {
                let request = headers(&mut socket).await;
                if request.starts_with("GET ") {
                    let body = r#"{"data":[{"id":"actual-a"}]}"#;
                    socket.write_all(format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).as_bytes()).await.unwrap();
                } else {
                    let len: usize = request
                        .lines()
                        .find_map(|line| {
                            line.to_lowercase()
                                .strip_prefix("content-length:")
                                .map(|value| value.trim().parse().unwrap())
                        })
                        .unwrap();
                    let mut body = vec![0; len];
                    socket.read_exact(&mut body).await.unwrap();
                    seen.store(true, Ordering::SeqCst);
                    // No response headers; observe cancellation on the actual upstream socket.
                    let mut byte = [0];
                    if matches!(socket.read(&mut byte).await, Ok(0) | Err(_)) {
                        gone.store(true, Ordering::SeqCst);
                    }
                }
            });
        }
    });
    f.store
        .routes
        .lock()
        .unwrap()
        .get_mut("a")
        .unwrap()
        .upstream = format!("http://{address}/v1");
    let base = f.enable("a", f.free_port().await).await.base_url.unwrap();
    let client = f.client.clone();
    let request = tokio::spawn(async move {
        client
            .post(format!("{base}/chat/completions"))
            .bearer_auth("test-a")
            .json(&json!({"model":"pinvou:a","messages":[],"stream":true}))
            .send()
            .await
            .unwrap()
    });
    until(|| started.load(Ordering::SeqCst)).await;
    crate::db::delete_api_key("test-a").unwrap();
    let response = tokio::time::timeout(Duration::from_secs(3), request)
        .await
        .expect("revocation waited for the 120s header timeout")
        .unwrap();
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    until(|| disconnected.load(Ordering::SeqCst)).await;
    server.abort();
}

async fn stalled_downstream(action: &str) {
    let f = Fixture::new().await;
    let port = f.free_port().await;
    f.enable("a", port).await;
    let (mut socket, rest) = upload(&f, port, "flood").await;
    socket.write_all(&rest).await.unwrap();
    assert!(headers(&mut socket).await.starts_with("HTTP/1.1 200"));
    // Stop reading, retain the connection, and prove the upstream became blocked.
    tokio::time::timeout(Duration::from_secs(8), async {
        let mut previous = 0;
        let mut stable = 0;
        loop {
            tokio::time::sleep(Duration::from_millis(100)).await;
            let count = f.produced.load(Ordering::SeqCst);
            stable = if count > 2 && count == previous {
                stable + 1
            } else {
                0
            };
            if stable == 3 {
                break;
            }
            previous = count;
        }
    })
    .await
    .expect("fixture did not reach bounded backpressure");
    assert_eq!(f.drops.load(Ordering::SeqCst), 0);
    if action == "revoke" {
        crate::db::delete_api_key("test-a").unwrap();
    } else {
        f.disable("a").await;
    }
    until(|| f.drops.load(Ordering::SeqCst) > 0).await;
    // The socket remains live/unread until after upstream release is observed.
    drop(socket);
}

#[tokio::test]
async fn revocation_releases_upstream_with_downstream_connected_but_not_reading() {
    stalled_downstream("revoke").await;
}

#[tokio::test]
async fn disable_releases_upstream_with_downstream_connected_but_not_reading() {
    stalled_downstream("disable").await;
}

#[tokio::test]
async fn continuous_chunks_do_not_starve_route_rechecks() {
    let f = Fixture::new().await;
    let base = f.enable("a", f.free_port().await).await.base_url.unwrap();
    let response = f.chat(&base, "test-a", "pinvou:a", true, "hello").await;
    let mut output = response.bytes_stream();
    output.next().await.unwrap().unwrap();
    f.store.routes.lock().unwrap().remove("a");
    assert!(
        crate::db::find_api_key("test-a").unwrap().is_some(),
        "key remains valid"
    );
    tokio::time::timeout(Duration::from_secs(3), async {
        while output.next().await.is_some() {}
    })
    .await
    .expect("fast chunks starved route checks");
    until(|| f.drops.load(Ordering::SeqCst) > 0).await;
}

struct TestStore {
    interface: Interface,
    online: AtomicBool,
    key_checks: AtomicUsize,
    routes: StdMutex<HashMap<String, Route>>,
}
#[async_trait::async_trait]
impl Store for TestStore {
    fn interfaces(&self) -> Result<Vec<Interface>, String> {
        Ok(if self.online.load(Ordering::SeqCst) {
            vec![self.interface.clone()]
        } else {
            vec![]
        })
    }
    async fn key_card(&self, key: &str) -> Result<Option<String>, String> {
        self.key_checks.fetch_add(1, Ordering::SeqCst);
        crate::db::find_api_key(key)
            .map(|record| record.and_then(|r| r.card_id))
            .map_err(|e| e.to_string())
    }
    async fn route(&self, card: &str) -> Result<Route, String> {
        self.routes
            .lock()
            .unwrap()
            .get(card)
            .cloned()
            .ok_or("lan_model_unavailable".into())
    }
}
struct DropCount(Arc<AtomicUsize>);
impl Drop for DropCount {
    fn drop(&mut self) {
        self.0.fetch_add(1, Ordering::SeqCst);
    }
}
struct Fixture {
    gateway: Gateway,
    store: Arc<TestStore>,
    client: reqwest::Client,
    upstream: JoinHandle<()>,
    drops: Arc<AtomicUsize>,
    produced: Arc<AtomicUsize>,
    seen: Arc<StdMutex<Vec<Value>>>,
    ip: Ipv4Addr,
}
impl Drop for Fixture {
    fn drop(&mut self) {
        self.upstream.abort();
    }
}
impl Fixture {
    async fn new() -> Self {
        // Use only the local WSL virtual subnet; no device/model/firewall access.
        let socket = std::net::UdpSocket::bind("0.0.0.0:0").unwrap();
        socket.connect("192.0.2.1:9").unwrap(); // route lookup, sends no datagram
        let IpAddr::V4(ip) = socket.local_addr().unwrap().ip() else {
            panic!("IPv4 test network required")
        };
        assert!(ip.is_private());
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let drops = Arc::new(AtomicUsize::new(0));
        let seen = Arc::new(StdMutex::new(Vec::new()));
        let produced = Arc::new(AtomicUsize::new(0));
        let production = produced.clone();
        let dropped = drops.clone();
        let received = seen.clone();
        let server = Router::new().route("/v1/models", get(|| async { Json(json!({"data":[{"id":"actual-a"},{"id":"actual-b"}]})) }))
            .route("/v1/chat/completions", post(move |headers: axum::http::HeaderMap, Json(body): Json<Value>| {
                let dropped = dropped.clone(); let received = received.clone(); let production = production.clone();
                async move {
                    assert!(!headers.contains_key("authorization"), "LAN key must never reach upstream");
                    received.lock().unwrap().push(body.clone());
                    if body["messages"][0]["content"] == "bad-stream" { return Json(json!({"object":"chat.completion"})).into_response(); }
                    if body["messages"][0]["content"] == "error" { return (StatusCode::TOO_MANY_REQUESTS, "private upstream diagnostics").into_response(); }
                    if body["stream"] == true {
                        let flood = body["messages"][0]["content"] == "flood";
                        let output = stream::unfold((DropCount(dropped), 0, production), move |(guard, index, production)| async move {
                            tokio::time::sleep(Duration::from_millis(if flood { 1 } else { 30 })).await;
                            production.fetch_add(1, Ordering::SeqCst);
                            let data = if flood { vec![b'x'; 65536] } else { format!("data: {{\"choices\":[{{\"delta\":{{\"content\":\"{index}\"}}}}]}}\n\n").into_bytes() };
                            Some((Ok::<_, std::io::Error>(data), (guard, index+1, production)))
                        });
                        ([("content-type","text/event-stream")],Body::from_stream(output)).into_response()
                    } else { Json(json!({"id":"test","object":"chat.completion","model":body["model"],"choices":[{"message":{"role":"assistant","content":"ok"}}]})).into_response() }
                }
            }));
        let upstream = tokio::spawn(async move {
            axum::serve(listener, server).await.unwrap();
        });
        crate::db::fixtures();
        let store = Arc::new(TestStore {
            interface: Interface {
                address: ip,
                mask: Ipv4Addr::new(255, 255, 255, 0),
                name: "virtual-test".into(),
            },
            online: AtomicBool::new(true),
            key_checks: AtomicUsize::new(0),
            routes: StdMutex::new(HashMap::from([
                (
                    "a".into(),
                    Route {
                        card_id: "a".into(),
                        upstream: format!("http://{address}/v1"),
                        upstream_model: "actual-a".into(),
                    },
                ),
                (
                    "b".into(),
                    Route {
                        card_id: "b".into(),
                        upstream: format!("http://{address}/v1"),
                        upstream_model: "actual-b".into(),
                    },
                ),
            ])),
        });
        Self {
            gateway: Gateway::default(),
            store,
            client: reqwest::Client::builder().no_proxy().build().unwrap(),
            upstream,
            drops,
            produced,
            seen,
            ip,
        }
    }
    fn store(&self) -> Arc<dyn Store> {
        self.store.clone()
    }
    async fn enable(&self, card: &str, port: u16) -> Status {
        self.gateway
            .set(
                self.store(),
                card.into(),
                true,
                Some(self.ip.to_string()),
                port,
            )
            .await
            .unwrap()
    }
    async fn disable(&self, card: &str) {
        self.gateway
            .set(self.store(), card.into(), false, None, 8090)
            .await
            .unwrap();
    }
    async fn free_port(&self) -> u16 {
        TcpListener::bind((self.ip, 0))
            .await
            .unwrap()
            .local_addr()
            .unwrap()
            .port()
    }
    async fn models(&self, base: &str, key: &str) -> reqwest::Response {
        self.client
            .get(format!("{base}/models"))
            .bearer_auth(key)
            .send()
            .await
            .unwrap()
    }
    async fn chat(
        &self,
        base: &str,
        key: &str,
        model: &str,
        streaming: bool,
        text: &str,
    ) -> reqwest::Response {
        self.client.post(format!("{base}/chat/completions")).bearer_auth(key).json(&json!({"model":model,"messages":[{"role":"user","content":text}],"stream":streaming})).send().await.unwrap()
    }
}

#[tokio::test]
async fn default_off_auth_scope_routing_revoke_and_independent_models() {
    let f = Fixture::new().await;
    let before = f.gateway.status(&f.store(), "a").await.unwrap();
    assert!(!before.enabled);
    assert!(before.base_url.is_none());
    let port = f.free_port().await;
    assert!(
        f.client
            .get(format!("http://{}:{port}/v1/models", f.ip))
            .timeout(Duration::from_secs(1))
            .send()
            .await
            .is_err(),
        "no LAN listener before explicit activation"
    );
    let raw_port = reqwest::Url::parse(&f.store.routes.lock().unwrap()["a"].upstream)
        .unwrap()
        .port()
        .unwrap();
    assert!(
        f.client
            .get(format!("http://{}:{raw_port}/v1/models", f.ip))
            .timeout(Duration::from_secs(1))
            .send()
            .await
            .is_err(),
        "loopback engine must not be reachable through its raw LAN port"
    );
    let status = f.enable("a", port).await;
    let base = status.base_url.unwrap();
    assert_eq!(
        f.models(&base, "wrong").await.status(),
        StatusCode::UNAUTHORIZED
    );
    assert_eq!(
        f.models(&base, "legacy").await.status(),
        StatusCode::UNAUTHORIZED
    );
    let generated = crate::db::add_api_key("a", Some("separate caller")).unwrap();
    assert!(generated.key_value.starts_with("lmm-"));
    assert_eq!(
        f.models(&base, &generated.key_value).await.status(),
        StatusCode::OK
    );
    crate::db::delete_api_key(&generated.id).unwrap();
    assert_eq!(
        f.models(&base, &generated.key_value).await.status(),
        StatusCode::UNAUTHORIZED
    );
    assert_eq!(crate::db::list_api_keys_for_card("a").unwrap().len(), 3);
    assert_eq!(
        f.models(&base, "test-b").await.status(),
        StatusCode::FORBIDDEN
    );
    assert_eq!(f.models(&base, "").await.status(), StatusCode::UNAUTHORIZED);
    let list: Value = f.models(&base, "test-a").await.json().await.unwrap();
    assert_eq!(list["data"][0]["id"], "pinvou:a");
    assert_eq!(list["data"].as_array().unwrap().len(), 1);
    assert_eq!(
        f.chat(&base, "test-a", "pinvou:b", false, "hello")
            .await
            .status(),
        StatusCode::NOT_FOUND
    );
    let response: Value = f
        .chat(&base, "test-a", "pinvou:a", false, "hello")
        .await
        .json()
        .await
        .unwrap();
    assert_eq!(response["choices"][0]["message"]["content"], "ok");
    assert_eq!(f.seen.lock().unwrap()[0]["model"], "actual-a");
    f.enable("b", port).await;
    assert_eq!(
        f.chat(&base, "test-b", "pinvou:b", false, "hello")
            .await
            .status(),
        StatusCode::OK
    );
    crate::db::delete_api_key("test-a").unwrap();
    assert_eq!(
        f.models(&base, "test-a").await.status(),
        StatusCode::UNAUTHORIZED
    );
    assert_eq!(f.models(&base, "test-a2").await.status(), StatusCode::OK);
    f.disable("a").await;
    assert_eq!(
        f.models(&base, "test-a2").await.status(),
        StatusCode::FORBIDDEN
    );
    assert_eq!(f.models(&base, "test-b").await.status(), StatusCode::OK);
    f.disable("b").await;
    assert!(
        f.client
            .get(format!("{base}/models"))
            .timeout(Duration::from_secs(1))
            .send()
            .await
            .is_err()
    );
    assert!(
        f.client
            .get(format!(
                "{}/models",
                f.store.routes.lock().unwrap()["b"].upstream
            ))
            .send()
            .await
            .unwrap()
            .status()
            .is_success(),
        "closing sharing must leave the engine running"
    );
}

#[tokio::test]
async fn streaming_cancellation_revocation_and_disable_drop_upstream() {
    let f = Fixture::new().await;
    let port = f.free_port().await;
    let base = f.enable("a", port).await.base_url.unwrap();
    for action in ["disconnect", "revoke", "disable"] {
        crate::db::connect()
            .unwrap()
            .execute(
                "INSERT OR REPLACE INTO api_keys VALUES ('test-a','a',NULL,'test-a','test')",
                [],
            )
            .unwrap();
        f.enable("a", port).await;
        let response = f.chat(&base, "test-a", "pinvou:a", true, "hello").await;
        assert_eq!(response.headers()["content-type"], "text/event-stream");
        let mut stream = response.bytes_stream();
        let first = stream.next().await.unwrap().unwrap();
        assert!(String::from_utf8_lossy(&first).starts_with("data: "));
        let before = f.drops.load(Ordering::SeqCst);
        match action {
            "revoke" => {
                crate::db::delete_api_key("test-a").unwrap();
            }
            "disable" => f.disable("a").await,
            _ => {}
        }
        if action != "disconnect" {
            tokio::time::timeout(Duration::from_secs(2), async {
                while stream.next().await.is_some() {}
            })
            .await
            .unwrap();
        }
        drop(stream);
        tokio::time::timeout(Duration::from_secs(2), async {
            while f.drops.load(Ordering::SeqCst) == before {
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
        })
        .await
        .unwrap();
    }
}

#[tokio::test]
async fn address_change_conflicts_model_restart_errors_and_url_secrets() {
    let f = Fixture::new().await;
    for bad in ["0.0.0.0", "8.8.8.8", "127.0.0.1"] {
        assert!(
            f.gateway
                .set(f.store(), "a".into(), true, Some(bad.into()), 8090)
                .await
                .is_err()
        );
    }
    let occupied = TcpListener::bind((f.ip, 0)).await.unwrap();
    let port = occupied.local_addr().unwrap().port();
    assert_eq!(
        f.gateway
            .set(f.store(), "a".into(), true, Some(f.ip.to_string()), port)
            .await
            .err()
            .unwrap(),
        "lan_bind_failed"
    );
    drop(occupied);
    let base = f.enable("a", port).await.base_url.unwrap();
    assert_eq!(
        f.client
            .get(format!("{base}/models?api_key=never-log"))
            .bearer_auth("test-a")
            .send()
            .await
            .unwrap()
            .status(),
        StatusCode::BAD_REQUEST
    );
    assert_eq!(
        f.client
            .get(format!("{base}/models"))
            .header("origin", "https://example.com")
            .bearer_auth("test-a")
            .send()
            .await
            .unwrap()
            .status(),
        StatusCode::BAD_REQUEST
    );
    assert_eq!(
        f.chat(&base, "test-a", "pinvou:a", false, "error")
            .await
            .status(),
        StatusCode::TOO_MANY_REQUESTS
    );
    let error = f
        .chat(&base, "test-a", "pinvou:a", false, "error")
        .await
        .text()
        .await
        .unwrap();
    assert!(!error.contains("private upstream diagnostics"));
    assert_eq!(
        f.chat(&base, "test-a", "pinvou:a", true, "bad-stream")
            .await
            .status(),
        StatusCode::BAD_GATEWAY
    );
    let route = f.store.routes.lock().unwrap().remove("a").unwrap();
    assert_eq!(
        f.models(&base, "test-a").await.status(),
        StatusCode::SERVICE_UNAVAILABLE
    );
    let mut changed = route.clone();
    changed.upstream_model = "wrong-model".into();
    f.store.routes.lock().unwrap().insert("a".into(), changed);
    assert_eq!(
        f.models(&base, "test-a").await.status(),
        StatusCode::SERVICE_UNAVAILABLE
    );
    f.store.routes.lock().unwrap().insert("a".into(), route);
    assert_eq!(f.models(&base, "test-a").await.status(), StatusCode::OK);
    let replacement = Fixture::new().await;
    let replacement_route = replacement.store.routes.lock().unwrap()["a"].clone();
    f.upstream.abort();
    f.store
        .routes
        .lock()
        .unwrap()
        .insert("a".into(), replacement_route);
    assert_eq!(
        f.models(&base, "test-a").await.status(),
        StatusCode::OK,
        "requests must follow the current port, not a cached route"
    );
    assert_eq!(
        f.chat(&base, "test-a", "pinvou:a", false, "after restart")
            .await
            .status(),
        StatusCode::OK
    );
    assert_eq!(replacement.seen.lock().unwrap().len(), 1);
    f.store.online.store(false, Ordering::SeqCst);
    tokio::time::sleep(Duration::from_millis(1200)).await;
    assert!(
        f.client
            .get(format!("{base}/models"))
            .timeout(Duration::from_secs(1))
            .send()
            .await
            .is_err()
    );
    assert!(!f.gateway.status(&f.store(), "a").await.unwrap().enabled);
}

#[test]
fn peer_must_be_private_and_in_the_selected_subnet() {
    let i = Interface {
        address: Ipv4Addr::new(192, 168, 50, 2),
        mask: Ipv4Addr::new(255, 255, 255, 0),
        name: "test".into(),
    };
    assert!(i.accepts("192.168.50.3".parse().unwrap()));
    for peer in ["192.168.51.3", "8.8.8.8", "127.0.0.1", "::1"] {
        assert!(!i.accepts(peer.parse().unwrap()));
    }
}
