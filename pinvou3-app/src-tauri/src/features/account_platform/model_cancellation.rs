//! Cancellation follows the frozen invocation identity; no client-side money decision.
use axum::body::Bytes;
use futures_util::{Stream, StreamExt, stream};
use reqwest::{Client, Url};
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone, Default)]
pub(super) struct Calls(Arc<Mutex<Registry>>);
#[derive(Default)]
struct Registry {
    // Active deliveries and detached identities share the same 4096-call bound.
    entries: HashMap<uuid::Uuid, Arc<Invocation>>,
    detached: HashMap<uuid::Uuid, Arc<Invocation>>,
    incarnations: HashMap<String, Arc<Incarnation>>,
}
pub(super) struct Incarnation {
    id: uuid::Uuid,
    session: String,
    retired: AtomicBool,
    cancelled_through: AtomicU64,
    latest_generation: AtomicU64,
}
impl Incarnation {
    pub(super) fn is_retired(&self) -> bool {
        self.retired.load(Ordering::Acquire)
    }
}
struct Invocation {
    id: uuid::Uuid,
    session: String,
    incarnation: Arc<Incarnation>,
    generation: u64,
    origin: Url,
    bearer: String,
    authorization: Option<Arc<dyn super::ModelAuthorization>>,
    operation_key: String,
    cancel_key: String,
    usage_id: Mutex<Option<uuid::Uuid>>,
    stopped: CancellationToken,
    cancellation_started: AtomicBool,
    initial_recovery_finished: AtomicBool,
    late_recovery_started: AtomicBool,
    late_identity_observed: AtomicBool,
    client: Client,
}
pub(super) struct Delivery {
    calls: Calls,
    invocation: Arc<Invocation>,
    completed: bool,
}
impl Calls {
    pub(super) fn incarnation(&self, session: &str) -> Arc<Incarnation> {
        self.0
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .incarnations
            .entry(session.into())
            .or_insert_with(|| {
                Arc::new(Incarnation {
                    id: uuid::Uuid::new_v4(),
                    session: session.into(),
                    retired: AtomicBool::new(false),
                    cancelled_through: AtomicU64::new(0),
                    latest_generation: AtomicU64::new(0),
                })
            })
            .clone()
    }
    pub(super) fn forget_session(&self, session: &str) {
        let entries = {
            let mut registry = self
                .0
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if let Some(incarnation) = registry.incarnations.remove(session) {
                incarnation.retired.store(true, Ordering::Release);
            }
            let entries = registry
                .entries
                .values()
                .chain(registry.detached.values())
                .filter(|entry| entry.session == session)
                .cloned()
                .collect::<Vec<_>>();
            registry
                .detached
                .retain(|_, entry| entry.session != session);
            entries
        };
        for entry in entries {
            entry.detach();
        }
    }
    #[cfg(test)]
    pub(super) fn track(
        &self,
        incarnation: &Arc<Incarnation>,
        generation: u64,
        origin: Url,
        bearer: &str,
        key: &str,
        client: Client,
    ) -> anyhow::Result<Delivery> {
        self.track_authorized(incarnation, generation, origin, bearer, key, client, None)
    }

    pub(super) fn track_authorized(
        &self,
        incarnation: &Arc<Incarnation>,
        generation: u64,
        origin: Url,
        bearer: &str,
        key: &str,
        client: Client,
        authorization: Option<Arc<dyn super::ModelAuthorization>>,
    ) -> anyhow::Result<Delivery> {
        let mut registry = self
            .0
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if incarnation.retired.load(Ordering::Acquire)
            || generation < incarnation.latest_generation.load(Ordering::Acquire)
            || generation <= incarnation.cancelled_through.load(Ordering::Acquire)
        {
            anyhow::bail!("model invocation is cancelled or tracking capacity is exhausted");
        }
        incarnation
            .latest_generation
            .fetch_max(generation, Ordering::AcqRel);
        // A new live turn retires only old local cancellation identities. It
        // cannot create a user cancellation or change the old server execution.
        registry.detached.retain(|_, entry| {
            !Arc::ptr_eq(&entry.incarnation, incarnation) || entry.generation >= generation
        });
        let predecessor = registry
            .detached
            .values()
            .find(|entry| {
                Arc::ptr_eq(&entry.incarnation, incarnation)
                    && entry.generation == generation
                    && entry.operation_key == key
                    && entry.origin == origin
            })
            .cloned();
        if predecessor.as_ref().is_some_and(|previous| {
            !match (&previous.authorization, &authorization) {
                (Some(previous), Some(current)) => match (previous.scope(), current.scope()) {
                    (Some(previous), Some(current)) => previous == current,
                    _ => Arc::ptr_eq(previous, current),
                },
                // No renewable authority exists in this test/legacy seam.
                (None, None) => previous.bearer == bearer,
                _ => false,
            }
        }) {
            anyhow::bail!("model retry authorization changed");
        }
        if registry.entries.len() + registry.detached.len() >= 4096 && predecessor.is_none() {
            anyhow::bail!("model invocation is cancelled or tracking capacity is exhausted");
        }
        if let Some(previous) = &predecessor {
            registry.detached.remove(&previous.id);
        }
        let session = &incarnation.session;
        let digest = Sha256::digest(format!("{session}\n{generation}\n{key}"));
        let cancel_key = format!(
            "pinvou-cancel-{}",
            crate::platform::encoding::hex_lower(&digest)
        );
        let invocation = Arc::new(Invocation {
            id: uuid::Uuid::new_v4(),
            session: session.to_string(),
            incarnation: incarnation.clone(),
            generation,
            origin,
            bearer: predecessor
                .as_ref()
                .map_or_else(|| bearer.into(), |entry| entry.bearer.clone()),
            authorization: predecessor
                .as_ref()
                .map_or(authorization, |entry| entry.authorization.clone()),
            operation_key: key.into(),
            cancel_key,
            usage_id: Mutex::new(predecessor.as_ref().and_then(|entry| {
                *entry
                    .usage_id
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner)
            })),
            stopped: CancellationToken::new(),
            cancellation_started: AtomicBool::new(false),
            initial_recovery_finished: AtomicBool::new(false),
            late_recovery_started: AtomicBool::new(false),
            late_identity_observed: AtomicBool::new(false),
            client,
        });
        registry.entries.insert(invocation.id, invocation.clone());
        Ok(Delivery {
            calls: self.clone(),
            invocation,
            completed: false,
        })
    }
    pub(super) fn reclaim_session(&self, session: &str, preserved: Option<u64>) {
        let entries = {
            let mut registry = self
                .0
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            let entries = registry
                .entries
                .values()
                .chain(registry.detached.values())
                .filter(|entry| entry.session == session && Some(entry.generation) != preserved)
                .cloned()
                .collect::<Vec<_>>();
            registry
                .detached
                .retain(|_, entry| entry.session != session || Some(entry.generation) == preserved);
            for entry in &entries {
                entry
                    .incarnation
                    .cancelled_through
                    .fetch_max(entry.generation, Ordering::AcqRel);
            }
            entries
        };
        for entry in entries {
            entry.detach();
        }
    }
    pub(super) fn cancellation_scope(&self, session: &str) -> Option<String> {
        self.0
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .incarnations
            .get(session)
            .map(|incarnation| incarnation.id.to_string())
    }
    #[cfg(test)]
    pub(super) fn cancel_turn(&self, session: &str, generation: u64) {
        if let Some(scope) = self.cancellation_scope(session) {
            self.cancel_turn_bound(session, generation, &scope);
        }
    }
    pub(super) fn cancel_turn_bound(&self, session: &str, generation: u64, scope: &str) {
        let entries = {
            let mut registry = self
                .0
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            let Some(incarnation) = registry.incarnations.get(session).cloned() else {
                return;
            };
            if incarnation.id.to_string() != scope {
                return;
            }
            incarnation
                .cancelled_through
                .fetch_max(generation, Ordering::AcqRel);
            let entries = registry
                .entries
                .values()
                .chain(registry.detached.values())
                .filter(|entry| {
                    Arc::ptr_eq(&entry.incarnation, &incarnation) && entry.generation == generation
                })
                .cloned()
                .collect::<Vec<_>>();
            registry.detached.retain(|_, entry| {
                !Arc::ptr_eq(&entry.incarnation, &incarnation) || entry.generation != generation
            });
            entries
        };
        for entry in entries {
            entry.stop();
        }
    }
}
impl Invocation {
    // A transport/lifecycle boundary stops local delivery only (TOK-40).
    fn detach(&self) {
        self.stopped.cancel();
    }
    // Only an explicit turn cancellation may create the TOK-38 command.
    fn stop(self: &Arc<Self>) {
        if self.cancellation_started.swap(true, Ordering::AcqRel) {
            return;
        }
        self.detach();
        let invocation = self.clone();
        tokio::spawn(async move {
            let initially_known = *invocation
                .usage_id
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if !invocation.reconcile_cancel().await {
                // Never expose a bearer, original key, URL, payload, or provider error.
                log::warn!("account model cancellation remains unconfirmed");
                invocation
                    .initial_recovery_finished
                    .store(true, Ordering::Release);
                // Headers may arrive after the bounded read-only lookup ended.
                // A newly learned identity gets its own bounded cancellation,
                // without issuing another model invocation.
                if initially_known.is_none()
                    && invocation.late_identity_observed.load(Ordering::Acquire)
                    && invocation
                        .usage_id
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner)
                        .is_some()
                {
                    invocation.recover_late_identity();
                }
            }
        });
    }
    fn recover_late_identity(self: &Arc<Self>) {
        if self.late_recovery_started.swap(true, Ordering::AcqRel) {
            return;
        }
        let invocation = self.clone();
        tokio::spawn(async move {
            if !invocation.reconcile_cancel().await {
                log::warn!("account model cancellation remains unconfirmed");
            }
        });
    }
    fn endpoint(&self, resource: &str) -> anyhow::Result<Url> {
        Ok(Url::parse(&format!(
            "{}/{}",
            self.origin.as_str().trim_end_matches('/'),
            resource
        ))?)
    }
    async fn reconcile_cancel(&self) -> bool {
        // Missing headers and lost ACKs are observations, not evidence of no effect.
        for _ in 0..8 {
            let known = *self
                .usage_id
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            let id = match known {
                Some(id) => Some(id),
                None => self
                    .read_usage(
                        "usage-requests/by-idempotency-key",
                        Some(&self.operation_key),
                    )
                    .await
                    .and_then(|body| {
                        body.get("usage_request_id")
                            .and_then(|v| v.as_str())
                            .and_then(|v| uuid::Uuid::parse_str(v).ok())
                    }),
            };
            if let Some(id) = id {
                let matches_binding = {
                    let mut bound = self
                        .usage_id
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner);
                    if bound.is_some_and(|saved| saved != id) {
                        false
                    } else {
                        *bound = Some(id);
                        true
                    }
                };
                if !matches_binding {
                    return false;
                }
                let Ok(endpoint) = self.endpoint(&format!("usage-requests/{id}/cancel")) else {
                    return false;
                };
                let response = self
                    .authorized_request(reqwest::Method::POST, endpoint, Some(&self.cancel_key))
                    .await;
                if let Some(response) = response {
                    if response.status().is_success() {
                        // The reply alone is not a local settlement or release decision.
                        let receipt = self
                            .read_usage(
                                "usage-requests/by-idempotency-key",
                                Some(&self.operation_key),
                            )
                            .await;
                        if let Some(body) = receipt {
                            if body.get("usage_request_id").and_then(|v| v.as_str())
                                == Some(id.to_string().as_str())
                                && body
                                    .get("cancel_status")
                                    .and_then(|v| v.as_str())
                                    .is_some_and(|s| {
                                        matches!(
                                            s,
                                            "LOCAL_CONFIRMED"
                                                | "PROVIDER_CONFIRMED"
                                                | "UNSUPPORTED"
                                        )
                                    })
                            {
                                return true;
                            }
                        }
                    }
                }
            }
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
        false
    }
    async fn read_usage(&self, resource: &str, key: Option<&str>) -> Option<serde_json::Value> {
        let response = self
            .authorized_request(reqwest::Method::GET, self.endpoint(resource).ok()?, key)
            .await?;
        if !response.status().is_success() {
            return None;
        }
        if response.content_length().is_some_and(|size| size > 65536) {
            return None;
        }
        let mut bytes = Vec::new();
        let mut source = response.bytes_stream();
        while let Some(chunk) = source.next().await {
            let chunk = chunk.ok()?;
            if bytes.len() + chunk.len() > 65536 {
                return None;
            }
            bytes.extend_from_slice(&chunk)
        }
        serde_json::from_slice(&bytes).ok()
    }

    async fn authorized_request(
        &self,
        method: reqwest::Method,
        url: Url,
        key: Option<&str>,
    ) -> Option<reqwest::Response> {
        let mut bearer = self.bearer.clone();
        for attempt in 0..2 {
            if let Some(supplier) = &self.authorization {
                bearer = tokio::time::timeout(Duration::from_secs(5), supplier.bearer(None))
                    .await
                    .ok()?
                    .ok()?;
            }
            let mut request = self
                .client
                .request(method.clone(), url.clone())
                .header("Authorization", &bearer)
                .timeout(Duration::from_secs(5));
            if let Some(key) = key {
                request = request.header("Idempotency-Key", key);
            }
            let response = request.send().await.ok()?;
            if response.status() == reqwest::StatusCode::UNAUTHORIZED && attempt == 0 {
                if let Some(supplier) = &self.authorization {
                    tokio::time::timeout(Duration::from_secs(5), supplier.bearer(Some(&bearer)))
                        .await
                        .ok()?
                        .ok()?;
                    continue;
                }
            }
            return Some(response);
        }
        None
    }
}
impl Delivery {
    pub(super) fn observe_usage(&self, header: Option<&str>) {
        if let Some(id) = header.and_then(|value| uuid::Uuid::parse_str(value).ok()) {
            let newly_bound = {
                let mut bound = self
                    .invocation
                    .usage_id
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner);
                if bound.is_some() {
                    false
                } else {
                    *bound = Some(id);
                    true
                }
            };
            if newly_bound && self.invocation.cancellation_started.load(Ordering::Acquire) {
                self.invocation
                    .late_identity_observed
                    .store(true, Ordering::Release);
                if self
                    .invocation
                    .initial_recovery_finished
                    .load(Ordering::Acquire)
                {
                    self.invocation.recover_late_identity();
                }
            }
        }
    }
    pub(super) fn stopped(&self) -> bool {
        self.invocation.stopped.is_cancelled()
    }
    pub(super) async fn cancelled(&self) {
        self.invocation.stopped.cancelled().await;
    }
    pub(super) fn cancellation_token(&self) -> CancellationToken {
        self.invocation.stopped.clone()
    }
    fn complete(&mut self) {
        self.completed = true;
    }
}
impl Drop for Delivery {
    fn drop(&mut self) {
        let mut registry = self
            .calls
            .0
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        registry.entries.remove(&self.invocation.id);
        if !self.completed {
            self.invocation.detach();
            let incarnation = &self.invocation.incarnation;
            // Retain only the current turn's original cancellation identity,
            // never a stream/body. Explicit Stop can still find the call in the
            // transparent retry gap; reclaim/retirement must not become Stop.
            if !incarnation.retired.load(Ordering::Acquire)
                && self.invocation.generation
                    == incarnation.latest_generation.load(Ordering::Acquire)
                && self.invocation.generation
                    > incarnation.cancelled_through.load(Ordering::Acquire)
            {
                registry
                    .detached
                    .insert(self.invocation.id, self.invocation.clone());
            }
        }
    }
}

pub(super) fn track_delivery<S, E>(
    source: S,
    delivery: Delivery,
) -> impl Stream<Item = Result<Bytes, std::io::Error>>
where
    S: Stream<Item = Result<Bytes, E>> + Send + 'static,
    E: std::fmt::Display + Send + 'static,
{
    stream::unfold(
        (Box::pin(source), delivery),
        |(mut source, mut delivery)| async move {
            if delivery.stopped() {
                return None;
            }
            tokio::select! {
                biased;
                _=delivery.invocation.stopped.cancelled()=>None,
                chunk=source.next()=>match chunk {
                    Some(Ok(bytes))=>Some((Ok(bytes),(source,delivery))),
                    Some(Err(_error))=>{delivery.invocation.detach();Some((Err(std::io::Error::other("model delivery failed")),(source,delivery)))},
                    None=>{delivery.complete();None},
                }
            }
        },
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{
        Json, Router,
        extract::{Path, State},
        http::{HeaderMap, StatusCode},
        routing::get,
    };
    use serde_json::json;

    #[tokio::test]
    async fn poisoned_cancellation_locks_preserve_frozen_call_and_usage_identity() {
        let calls = Calls::default();
        let incarnation = calls.incarnation("poisoned-cancellation");
        let mut delivery = calls
            .track(
                &incarnation,
                1,
                Url::parse("https://example.invalid/gateway/v1/").unwrap(),
                "Bearer frozen-user",
                "original-stable-invocation-key",
                Client::new(),
            )
            .unwrap();
        let invocation = delivery.invocation.clone();
        let usage_id = uuid::Uuid::new_v4();
        *invocation.usage_id.lock().unwrap() = Some(usage_id);

        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                let _guard = calls.0.lock().unwrap();
                panic!("simulate registry lock poisoning");
            }))
            .is_err()
        );
        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                let _guard = invocation.usage_id.lock().unwrap();
                panic!("simulate usage identity lock poisoning");
            }))
            .is_err()
        );

        assert!(Arc::ptr_eq(
            &incarnation,
            &calls.incarnation("poisoned-cancellation")
        ));
        assert_eq!(
            calls.cancellation_scope("poisoned-cancellation"),
            Some(incarnation.id.to_string())
        );
        assert!(Arc::ptr_eq(
            &calls
                .0
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .entries[&invocation.id],
            &invocation
        ));
        delivery.observe_usage(Some(&uuid::Uuid::new_v4().to_string()));
        assert_eq!(
            *invocation
                .usage_id
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner),
            Some(usage_id)
        );
        assert_eq!(
            invocation
                .endpoint("usage-requests/by-idempotency-key")
                .unwrap()
                .path(),
            "/gateway/v1/usage-requests/by-idempotency-key"
        );

        delivery.complete();
        drop(delivery);
        assert!(
            calls
                .0
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .entries
                .is_empty()
        );
        assert!(!invocation.stopped.is_cancelled());
        calls.forget_session("poisoned-cancellation");
        assert!(incarnation.is_retired());
        assert!(!Arc::ptr_eq(
            &incarnation,
            &calls.incarnation("poisoned-cancellation")
        ));
    }

    #[derive(Default)]
    struct Captures {
        posts: Vec<(String, String, String)>,
        reads: usize,
        lookups: usize,
    }
    #[derive(Clone)]
    struct Fixture {
        captures: Arc<Mutex<Captures>>,
        id: uuid::Uuid,
        found: bool,
    }
    async fn lookup(
        State(f): State<Fixture>,
        headers: HeaderMap,
    ) -> (StatusCode, Json<serde_json::Value>) {
        assert_eq!(headers["authorization"], "Bearer frozen-user");
        assert_eq!(headers["idempotency-key"], "original-stable-invocation-key");
        f.captures.lock().unwrap().lookups += 1;
        if !f.found && f.captures.lock().unwrap().posts.is_empty() {
            return (
                StatusCode::NOT_FOUND,
                Json(json!({"code":"TOKEN_NOT_FOUND"})),
            );
        }
        (
            StatusCode::OK,
            Json(
                json!({"usage_request_id":f.id,"cancel_status":if f.captures.lock().unwrap().posts.len()>=2 {"LOCAL_CONFIRMED"} else {"NOT_REQUESTED"}}),
            ),
        )
    }
    async fn read(
        State(f): State<Fixture>,
        Path(id): Path<String>,
        headers: HeaderMap,
    ) -> Json<serde_json::Value> {
        assert_eq!(headers["authorization"], "Bearer frozen-user");
        assert_eq!(id, f.id.to_string());
        f.captures.lock().unwrap().reads += 1;
        Json(
            json!({"usage_request_id":f.id,"cancel_status":"LOCAL_CONFIRMED","settlement_status":"RELEASED"}),
        )
    }
    async fn cancel(
        State(f): State<Fixture>,
        Path(id): Path<String>,
        headers: HeaderMap,
    ) -> StatusCode {
        let mut captures = f.captures.lock().unwrap();
        captures.posts.push((
            id,
            headers["idempotency-key"].to_str().unwrap().into(),
            headers["authorization"].to_str().unwrap().into(),
        ));
        // First result is lost after the server accepted cancellation. The
        // caller retries the same cancellation key, never a model invocation.
        if captures.posts.len() == 1 {
            StatusCode::SERVICE_UNAVAILABLE
        } else {
            StatusCode::OK
        }
    }
    async fn fixture(found: bool) -> (Fixture, Url, tokio::task::JoinHandle<()>) {
        let f = Fixture {
            captures: Arc::new(Mutex::new(Captures::default())),
            id: uuid::Uuid::new_v4(),
            found,
        };
        let router = Router::new()
            .route("/v1/usage-requests/by-idempotency-key", get(lookup))
            .route("/v1/usage-requests/{id}", get(read))
            .route(
                "/v1/usage-requests/{id}/cancel",
                axum::routing::post(cancel),
            )
            .with_state(f.clone());
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let task = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
        (
            f,
            Url::parse(&format!("http://{address}/v1")).unwrap(),
            task,
        )
    }
    fn track(calls: &Calls, origin: Url, generation: u64) -> Delivery {
        calls
            .track(
                &calls.incarnation("session"),
                generation,
                origin,
                "Bearer frozen-user",
                "original-stable-invocation-key",
                Client::builder()
                    .redirect(reqwest::redirect::Policy::none())
                    .build()
                    .unwrap(),
            )
            .unwrap()
    }
    #[tokio::test]
    async fn genuine_transport_lost_cancel_ack_retries_same_identity() {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let id = uuid::Uuid::new_v4();
        let server = tokio::spawn(async move {
            let mut keys = Vec::new();
            for index in 0..3 {
                let (mut socket, _) = listener.accept().await.unwrap();
                let mut request = Vec::new();
                while !request.windows(4).any(|part| part == b"\r\n\r\n") {
                    let mut buffer = [0u8; 1024];
                    let n = socket.read(&mut buffer).await.unwrap();
                    assert!(n > 0);
                    request.extend_from_slice(&buffer[..n]);
                    assert!(request.len() < 65536);
                }
                let text = String::from_utf8(request).unwrap();
                assert!(
                    text.to_ascii_lowercase()
                        .contains("authorization: bearer frozen-user")
                );
                if index < 2 {
                    assert!(text.starts_with(&format!("POST /v1/usage-requests/{id}/cancel ")));
                    keys.push(
                        text.lines()
                            .find_map(|line| {
                                line.split_once(':')
                                    .filter(|(name, _)| {
                                        name.eq_ignore_ascii_case("idempotency-key")
                                    })
                                    .map(|(_, value)| value.trim().to_string())
                            })
                            .unwrap(),
                    );
                    if index == 0 {
                        drop(socket);
                        continue;
                    }
                    socket
                        .write_all(
                            b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                        )
                        .await
                        .unwrap();
                } else {
                    assert!(text.starts_with("GET /v1/usage-requests/by-idempotency-key "));
                    assert!(
                        text.to_ascii_lowercase()
                            .contains("idempotency-key: original-stable-invocation-key")
                    );
                    let body=json!({"usage_request_id":id,"cancel_status":"LOCAL_CONFIRMED","settlement_status":"RELEASED"}).to_string();
                    let response = format!(
                        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                        body.len()
                    );
                    socket.write_all(response.as_bytes()).await.unwrap();
                }
            }
            assert_eq!(keys.len(), 2);
            assert_eq!(keys[0], keys[1]);
        });
        let calls = Calls::default();
        let mut delivery = track(
            &calls,
            Url::parse(&format!("http://{address}/v1")).unwrap(),
            1,
        );
        delivery.observe_usage(Some(&id.to_string()));
        assert!(delivery.invocation.reconcile_cancel().await);
        delivery.complete();
        drop(delivery);
        tokio::time::timeout(Duration::from_secs(5), server)
            .await
            .unwrap()
            .unwrap();
    }

    #[tokio::test]
    async fn known_usage_cancel_lost_ack_keeps_key_and_frozen_identity() {
        let (f, origin, task) = fixture(true).await;
        let calls = Calls::default();
        let delivery = track(&calls, origin, 1);
        delivery.observe_usage(Some(&f.id.to_string()));
        assert!(delivery.invocation.reconcile_cancel().await);
        delivery.invocation.stopped.cancel();
        let mut delivery = delivery;
        delivery.complete();
        drop(delivery);
        let captures = f.captures.lock().unwrap();
        assert_eq!(captures.posts.len(), 2);
        assert_eq!(captures.lookups, 1);
        assert!(
            captures
                .posts
                .iter()
                .all(|(id, key, auth)| id == &f.id.to_string()
                    && auth == "Bearer frozen-user"
                    && key == &captures.posts[0].1)
        );
        task.abort();
    }
    #[tokio::test]
    async fn missing_usage_header_recovers_only_by_reading_original_key() {
        let (f, origin, task) = fixture(true).await;
        let calls = Calls::default();
        let mut delivery = track(&calls, origin, 1);
        assert!(delivery.invocation.reconcile_cancel().await);
        delivery.complete();
        drop(delivery);
        let captures = f.captures.lock().unwrap();
        assert_eq!(captures.lookups, 2);
        assert_eq!(captures.posts.len(), 2);
        task.abort();
    }
    #[tokio::test]
    async fn unknown_lookup_never_becomes_release_or_model_replay() {
        let (f, origin, task) = fixture(false).await;
        let calls = Calls::default();
        let mut delivery = track(&calls, origin, 1);
        assert!(!delivery.invocation.reconcile_cancel().await);
        delivery.complete();
        drop(delivery);
        let captures = f.captures.lock().unwrap();
        assert_eq!(captures.lookups, 8);
        assert_eq!(captures.posts.len(), 0);
        assert_eq!(captures.reads, 0);
        task.abort();
    }
    #[tokio::test]
    async fn late_identity_after_lookup_budget_gets_one_bounded_recovery() {
        let (f, origin, task) = fixture(false).await;
        let calls = Calls::default();
        let mut delivery = track(&calls, origin, 1);
        calls.cancel_turn("session", 1);
        tokio::time::timeout(Duration::from_secs(4), async {
            while !delivery
                .invocation
                .initial_recovery_finished
                .load(Ordering::Acquire)
            {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap();
        assert_eq!(f.captures.lock().unwrap().posts.len(), 0);
        delivery.observe_usage(Some(&f.id.to_string()));
        // Repeated or conflicting headers cannot launch extra recovery tasks
        // or change the already captured server identity.
        delivery.observe_usage(Some(&uuid::Uuid::new_v4().to_string()));
        delivery.observe_usage(Some(&f.id.to_string()));
        tokio::time::timeout(Duration::from_secs(4), async {
            while f.captures.lock().unwrap().posts.len() < 2 {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap();
        assert_eq!(*delivery.invocation.usage_id.lock().unwrap(), Some(f.id));
        assert!(
            delivery
                .invocation
                .late_recovery_started
                .load(Ordering::Acquire)
        );
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(f.captures.lock().unwrap().posts.len(), 2);
        delivery.complete();
        drop(delivery);
        task.abort();
    }
    #[tokio::test]
    async fn aborted_generation_stops_delivery_without_touching_new_turn() {
        let (f, origin, task) = fixture(true).await;
        let calls = Calls::default();
        let old = track(&calls, origin.clone(), 1);
        let mut new = track(&calls, origin.clone(), 2);
        old.observe_usage(Some(&f.id.to_string()));
        calls.reclaim_session("session", Some(2));
        assert!(!new.stopped());
        assert!(
            calls
                .track(
                    &calls.incarnation("session"),
                    1,
                    origin,
                    "Bearer frozen-user",
                    "another-stable-invocation-key",
                    Client::new()
                )
                .is_err()
        );
        let source =
            stream::once(async { Ok::<_, std::io::Error>(Bytes::from_static(b"late result")) });
        assert!(Box::pin(track_delivery(source, old)).next().await.is_none());
        new.complete();
        drop(new);
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(f.captures.lock().unwrap().posts.is_empty());
        assert_eq!(f.captures.lock().unwrap().lookups, 0);
        task.abort();
    }
    #[tokio::test]
    async fn delivery_observations_do_not_fabricate_user_cancel_intent() {
        for observation in [
            "drop-before-poll",
            "drop-after-chunk",
            "read-error",
            "normal-eof",
        ] {
            let (f, origin, task) = fixture(true).await;
            let calls = Calls::default();
            let delivery = track(&calls, origin.clone(), 1);
            delivery.observe_usage(Some(&f.id.to_string()));
            let invocation = delivery.invocation.clone();
            let source = stream::iter(vec![
                Ok::<_, std::io::Error>(Bytes::from_static(b"partial-model-result")),
                Err(std::io::Error::other("controlled transport interruption")),
            ]);
            if observation == "normal-eof" {
                let source = stream::iter(vec![Ok::<_, std::io::Error>(Bytes::from_static(
                    b"complete-result",
                ))]);
                let result = track_delivery(source, delivery).collect::<Vec<_>>().await;
                assert_eq!(result.len(), 1);
                assert!(result[0].is_ok());
            } else {
                let mut tracked = Box::pin(track_delivery(source, delivery));
                if observation != "drop-before-poll" {
                    assert!(tracked.next().await.unwrap().is_ok());
                }
                if observation == "read-error" {
                    assert!(tracked.next().await.unwrap().is_err());
                    assert!(tracked.next().await.is_none());
                }
                drop(tracked);
            }
            assert!(
                calls.0.lock().unwrap().entries.is_empty(),
                "local tracking must be reclaimed"
            );
            assert!(
                !invocation.cancellation_started.load(Ordering::Acquire),
                "{observation} manufactured a user command"
            );
            assert_eq!(*invocation.usage_id.lock().unwrap(), Some(f.id));
            // A current-turn interrupted delivery retains only its original
            // cancellation identity: Stop still works in the retry gap.
            calls.cancel_turn("session", 1);
            assert_eq!(
                invocation.cancellation_started.load(Ordering::Acquire),
                observation != "normal-eof"
            );
            assert!(
                calls
                    .track(
                        &calls.incarnation("session"),
                        1,
                        origin.clone(),
                        "Bearer frozen-user",
                        "original-stable-invocation-key",
                        Client::new()
                    )
                    .is_err()
            );
            if observation != "normal-eof" {
                tokio::time::timeout(Duration::from_secs(3), async {
                    while f.captures.lock().unwrap().posts.len() < 2 {
                        tokio::time::sleep(Duration::from_millis(10)).await;
                    }
                })
                .await
                .unwrap();
                let captures = f.captures.lock().unwrap();
                assert_eq!(captures.posts.len(), 2);
                assert_eq!(captures.posts[0], captures.posts[1]);
                assert_eq!(captures.posts[0].0, f.id.to_string());
            } else {
                assert!(f.captures.lock().unwrap().posts.is_empty());
                assert_eq!(f.captures.lock().unwrap().lookups, 0);
            }
            let mut next = track(&calls, origin, 2);
            calls.cancel_turn("session", 1);
            assert!(!next.stopped());
            next.complete();
            drop(next);
            assert!(calls.0.lock().unwrap().detached.is_empty());
            task.abort();
        }
    }

    #[tokio::test]
    async fn explicit_stop_survives_later_delivery_drop_with_original_usage_and_key() {
        let (f, origin, task) = fixture(true).await;
        let calls = Calls::default();
        let delivery = track(&calls, origin, 1);
        delivery.observe_usage(Some(&f.id.to_string()));
        let invocation = delivery.invocation.clone();
        calls.cancel_turn("session", 1);
        drop(delivery);
        assert!(calls.0.lock().unwrap().entries.is_empty());
        assert!(invocation.cancellation_started.load(Ordering::Acquire));
        tokio::time::timeout(Duration::from_secs(3), async {
            while f.captures.lock().unwrap().posts.len() < 2 {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap();
        let captures = f.captures.lock().unwrap();
        assert_eq!(captures.posts.len(), 2);
        assert_eq!(captures.posts[0], captures.posts[1]);
        assert_eq!(captures.posts[0].0, f.id.to_string());
        assert_eq!(*invocation.usage_id.lock().unwrap(), Some(f.id));
        task.abort();
    }
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn explicit_stop_racing_delivery_drop_never_changes_usage_or_targets_next_turn() {
        for _ in 0..12 {
            let (f, origin, task) = fixture(true).await;
            let calls = Calls::default();
            let delivery = track(&calls, origin.clone(), 1);
            delivery.observe_usage(Some(&f.id.to_string()));
            let invocation = delivery.invocation.clone();
            let barrier = Arc::new(tokio::sync::Barrier::new(3));
            let dropping = {
                let barrier = barrier.clone();
                tokio::spawn(async move {
                    barrier.wait().await;
                    drop(delivery);
                })
            };
            let stopping = {
                let barrier = barrier.clone();
                let calls = calls.clone();
                tokio::spawn(async move {
                    barrier.wait().await;
                    calls.cancel_turn("session", 1);
                })
            };
            barrier.wait().await;
            dropping.await.unwrap();
            stopping.await.unwrap();
            assert!(calls.0.lock().unwrap().entries.is_empty());
            assert!(
                invocation.cancellation_started.load(Ordering::Acquire),
                "explicit Stop lost its original invocation in the Drop race"
            );
            if invocation.cancellation_started.load(Ordering::Acquire) {
                tokio::time::timeout(Duration::from_secs(3), async {
                    while f.captures.lock().unwrap().posts.len() < 2 {
                        tokio::time::sleep(Duration::from_millis(10)).await;
                    }
                })
                .await
                .unwrap();
                let captures = f.captures.lock().unwrap();
                assert_eq!(captures.posts.len(), 2);
                assert_eq!(captures.posts[0], captures.posts[1]);
                assert_eq!(captures.posts[0].0, f.id.to_string());
            } else {
                assert!(f.captures.lock().unwrap().posts.is_empty());
            }
            let mut next = track(&calls, origin, 2);
            calls.cancel_turn("session", 1);
            assert!(!next.stopped());
            assert_eq!(*invocation.usage_id.lock().unwrap(), Some(f.id));
            next.complete();
            drop(next);
            task.abort();
        }
    }
    #[tokio::test]
    async fn transparent_retry_inherits_original_identity_without_cancelling_or_reinvoking() {
        let (f, origin, task) = fixture(true).await;
        let calls = Calls::default();
        let old = track(&calls, origin.clone(), 3);
        old.observe_usage(Some(&f.id.to_string()));
        let original = old.invocation.clone();
        drop(old);
        assert_eq!(calls.0.lock().unwrap().detached.len(), 1);
        let retry = track(&calls, origin, 3);
        assert_ne!(retry.invocation.id, original.id);
        assert_eq!(*retry.invocation.usage_id.lock().unwrap(), Some(f.id));
        assert_eq!(retry.invocation.operation_key, original.operation_key);
        assert_eq!(retry.invocation.cancel_key, original.cancel_key);
        assert_eq!(retry.invocation.origin, original.origin);
        assert_eq!(retry.invocation.bearer, original.bearer);
        assert!(!original.cancellation_started.load(Ordering::Acquire));
        assert!(!retry.stopped());
        assert!(calls.0.lock().unwrap().detached.is_empty());
        calls.cancel_turn("session", 3);
        drop(retry);
        tokio::time::timeout(Duration::from_secs(3), async {
            while f.captures.lock().unwrap().posts.len() < 2 {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap();
        let captures = f.captures.lock().unwrap();
        assert_eq!(captures.posts.len(), 2);
        assert_eq!(captures.posts[0], captures.posts[1]);
        assert_eq!(captures.posts[0].0, f.id.to_string());
        task.abort();
    }

    #[tokio::test]
    async fn reclaim_retirement_and_new_generation_discard_only_local_retry_identities() {
        for boundary in ["reclaim", "retire", "new-generation"] {
            let (f, origin, task) = fixture(true).await;
            let calls = Calls::default();
            let old = track(&calls, origin.clone(), 1);
            let original = old.invocation.clone();
            old.observe_usage(Some(&f.id.to_string()));
            let old_scope = calls.cancellation_scope("session").unwrap();
            drop(old);
            assert_eq!(calls.0.lock().unwrap().detached.len(), 1);
            let mut next = match boundary {
                "retire" => {
                    calls.forget_session("session");
                    track(&calls, origin, 1)
                }
                "reclaim" => {
                    calls.reclaim_session("session", None);
                    track(&calls, origin, 2)
                }
                _ => track(&calls, origin, 2),
            };
            calls.cancel_turn_bound("session", 1, &old_scope);
            assert!(calls.0.lock().unwrap().detached.is_empty());
            assert!(!original.cancellation_started.load(Ordering::Acquire));
            assert!(!next.stopped());
            next.complete();
            drop(next);
            assert!(f.captures.lock().unwrap().posts.is_empty());
            assert_eq!(f.captures.lock().unwrap().lookups, 0);
            task.abort();
        }
    }

    #[tokio::test]
    async fn detached_retry_identities_share_existing_capacity_and_expire_with_the_turn() {
        let calls = Calls::default();
        let incarnation = calls.incarnation("bounded-session");
        let origin = Url::parse("http://127.0.0.1:1/v1").unwrap();
        let client = Client::new();
        for index in 0..4096 {
            let delivery = calls
                .track(
                    &incarnation,
                    1,
                    origin.clone(),
                    "Bearer frozen-user",
                    &format!("frozen-operation-{index}"),
                    client.clone(),
                )
                .unwrap();
            drop(delivery);
        }
        assert_eq!(calls.0.lock().unwrap().detached.len(), 4096);
        assert!(
            calls
                .track(
                    &incarnation,
                    1,
                    origin.clone(),
                    "Bearer frozen-user",
                    "one-too-many-operations",
                    client.clone()
                )
                .is_err()
        );
        let mut retry = calls
            .track(
                &incarnation,
                1,
                origin.clone(),
                "Bearer frozen-user",
                "frozen-operation-0",
                client.clone(),
            )
            .unwrap();
        assert_eq!(calls.0.lock().unwrap().detached.len(), 4095);
        retry.complete();
        drop(retry);
        let mut next = calls
            .track(
                &incarnation,
                2,
                origin.clone(),
                "Bearer frozen-user",
                "new-turn-operation",
                client.clone(),
            )
            .unwrap();
        assert!(calls.0.lock().unwrap().detached.is_empty());
        assert!(
            calls
                .track(
                    &incarnation,
                    1,
                    origin,
                    "Bearer frozen-user",
                    "frozen-operation-1",
                    client
                )
                .is_err()
        );
        next.complete();
        drop(next);
        assert!(calls.0.lock().unwrap().entries.is_empty());
        // A rejected new generation also retires older local identities.
        let other = calls.incarnation("other-capacity-session");
        let client = Client::new();
        let origin = Url::parse("http://127.0.0.1:1/v1").unwrap();
        let mut occupied = Vec::new();
        for index in 0..4095 {
            occupied.push(
                calls
                    .track(
                        &other,
                        1,
                        origin.clone(),
                        "Bearer frozen-user",
                        &format!("other-operation-{index}"),
                        client.clone(),
                    )
                    .unwrap(),
            );
        }
        let blocked = calls.incarnation("blocked-turn");
        let old = calls
            .track(
                &blocked,
                1,
                origin.clone(),
                "Bearer frozen-user",
                "old-blocked-operation",
                client.clone(),
            )
            .unwrap();
        assert!(
            calls
                .track(
                    &blocked,
                    2,
                    origin,
                    "Bearer frozen-user",
                    "new-blocked-operation",
                    client
                )
                .is_err()
        );
        drop(old);
        assert!(calls.0.lock().unwrap().detached.is_empty());
        for mut entry in occupied {
            entry.complete();
        }
        assert!(calls.0.lock().unwrap().entries.is_empty());
    }
    #[tokio::test]
    async fn retry_handoff_requires_exact_operation_origin_and_original_authority() {
        struct Authority {
            scope: &'static str,
            token: Arc<Mutex<String>>,
        }
        #[async_trait::async_trait]
        impl super::super::ModelAuthorization for Authority {
            fn scope(&self) -> Option<&str> {
                Some(self.scope)
            }
            async fn bearer(&self, _: Option<&str>) -> Result<String, String> {
                Ok(self.token.lock().unwrap().clone())
            }
        }
        let calls = Calls::default();
        let incarnation = calls.incarnation("bound-retry");
        let origin = Url::parse("http://127.0.0.1:1/v1").unwrap();
        let token = Arc::new(Mutex::new("Bearer original-token".to_string()));
        let authorization: Arc<dyn super::super::ModelAuthorization> = Arc::new(Authority {
            scope: "original-login",
            token: token.clone(),
        });
        let old = calls
            .track_authorized(
                &incarnation,
                1,
                origin.clone(),
                "Bearer original-token",
                "original-operation",
                Client::new(),
                Some(authorization.clone()),
            )
            .unwrap();
        let usage = uuid::Uuid::new_v4();
        old.observe_usage(Some(&usage.to_string()));
        drop(old);
        for (key, target) in [
            ("different-operation", origin.clone()),
            (
                "original-operation",
                Url::parse("http://127.0.0.1:2/v1").unwrap(),
            ),
        ] {
            let mut other = calls
                .track_authorized(
                    &incarnation,
                    1,
                    target,
                    "Bearer original-token",
                    key,
                    Client::new(),
                    Some(authorization.clone()),
                )
                .unwrap();
            assert_eq!(*other.invocation.usage_id.lock().unwrap(), None);
            other.complete();
            drop(other);
        }
        let wrong: Arc<dyn super::super::ModelAuthorization> = Arc::new(Authority {
            scope: "different-login",
            token: token.clone(),
        });
        assert!(
            calls
                .track_authorized(
                    &incarnation,
                    1,
                    origin.clone(),
                    "Bearer new-token",
                    "original-operation",
                    Client::new(),
                    Some(wrong)
                )
                .is_err()
        );
        *token.lock().unwrap() = "Bearer refreshed-token".into();
        let same_login: Arc<dyn super::super::ModelAuthorization> = Arc::new(Authority {
            scope: "original-login",
            token,
        });
        let mut retry = calls
            .track_authorized(
                &incarnation,
                1,
                origin,
                "Bearer refreshed-token",
                "original-operation",
                Client::new(),
                Some(same_login),
            )
            .unwrap();
        assert_eq!(*retry.invocation.usage_id.lock().unwrap(), Some(usage));
        assert_eq!(
            retry
                .invocation
                .authorization
                .as_ref()
                .unwrap()
                .bearer(None)
                .await
                .unwrap(),
            "Bearer refreshed-token"
        );
        assert!(Arc::ptr_eq(
            retry.invocation.authorization.as_ref().unwrap(),
            &authorization
        ));
        retry.complete();
        drop(retry);
        assert!(calls.0.lock().unwrap().detached.is_empty());
    }
}
