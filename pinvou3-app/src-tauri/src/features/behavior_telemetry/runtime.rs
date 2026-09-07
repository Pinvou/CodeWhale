use super::*;

impl BehaviorTelemetry {
    pub fn new() -> Self {
        let runtime = Arc::new(RuntimeConfig::from_env());
        let (mut persisted, mut state_healthy) = match load_state() {
            Ok(state) => (state, true),
            Err(error) => {
                log::error!(
                    "[pinvou3][behavior] telemetry state is unreadable; collection is disabled to preserve identity and pending controls: {error:#}"
                );
                (None, false)
            }
        };
        if !runtime.enabled {
            if let Some(state) = persisted.as_mut() {
                if prepare_operational_stop(state) {
                    if let Err(error) = persist_state(state) {
                        log::error!(
                            "[pinvou3][behavior] failed to persist operational telemetry stop: {error:#}"
                        );
                        state_healthy = false;
                    }
                }
            }
        }
        let active_period = persisted.as_ref().and_then(|state| {
            (runtime.enabled
                && state.telemetry_base_url == runtime.telemetry_base_url
                && state.behavior_events_url == runtime.behavior_events_url
                && state.control.enabled
                && !state.control.collection_blocked)
                .then(|| state.control.collection_period_id.clone())
                .flatten()
        });
        let active_generation = persisted
            .as_ref()
            .map_or(0, |state| state.control.generation);
        let collecting = Arc::new(AtomicBool::new(
            runtime.enabled && state_healthy && active_period.is_some(),
        ));
        let collection_period_id = Arc::new(RwLock::new(active_period.clone()));
        let mut loaded_outbox = Outbox::load(outbox_path()).unwrap_or_else(|error| {
            log::debug!("[pinvou3][behavior] failed to load outbox: {error:#}");
            Outbox::empty(outbox_path())
        });
        if let Err(error) = loaded_outbox.retain_period(active_period.as_deref(), active_generation)
        {
            log::debug!("[pinvou3][behavior] failed to sanitize outbox period: {error:#}");
        }
        let outbox = Arc::new(StdMutex::new(loaded_outbox));
        let recovered_tasks = load_task_attempts()
            .into_iter()
            .filter(|(_, attempt)| {
                active_period.as_deref() == Some(attempt.collection_period_id.as_str())
                    && attempt.generation == active_generation
            })
            .collect::<HashMap<_, _>>();
        let tasks = Arc::new(StdMutex::new(recovered_tasks.clone()));
        let state = Arc::new(Mutex::new(persisted));
        let state_healthy = Arc::new(AtomicBool::new(state_healthy));
        let credential_gate = Arc::new(Mutex::new(()));
        let control_gate = Arc::new(Mutex::new(()));
        let (event_tx, event_rx) = if runtime.enabled {
            let (tx, rx) = mpsc::channel(EVENT_WAKE_CAPACITY);
            (Some(tx), Some(rx))
        } else {
            (None, None)
        };
        let (control_tx, control_rx) = if state_healthy.load(Ordering::Acquire) {
            let (tx, rx) = mpsc::channel(CONTROL_WAKE_CAPACITY);
            (Some(tx), Some(rx))
        } else {
            (None, None)
        };
        let telemetry = Self {
            client: reqwest::Client::builder()
                .connect_timeout(Duration::from_secs(5))
                .timeout(Duration::from_secs(12))
                .build()
                .unwrap_or_else(|_| reqwest::Client::new()),
            runtime,
            state,
            state_healthy,
            credential_gate,
            control_gate,
            collecting,
            authorization_generation: Arc::new(AtomicU64::new(active_generation)),
            credential_renewal_in_progress: Arc::new(AtomicBool::new(false)),
            collection_period_id,
            outbox,
            tasks,
            team_sessions: Arc::new(StdMutex::new(HashMap::new())),
            tool_calls: Arc::new(StdMutex::new(HashMap::new())),
            voice_submissions: Arc::new(StdMutex::new(HashMap::new())),
            artifacts: Arc::new(StdMutex::new(HashMap::new())),
            latest_turns: Arc::new(StdMutex::new(HashMap::new())),
            tool_install_terminals: ToolInstallTerminalCoordinator::default(),
            event_tx,
            control_tx,
        };
        if let Some(rx) = event_rx {
            tauri::async_runtime::spawn(Self::run_worker(rx, telemetry.worker_clone()));
            if telemetry
                .outbox
                .lock()
                .is_ok_and(|outbox| !outbox.records.is_empty())
            {
                if let Some(tx) = &telemetry.event_tx {
                    let _ = tx.try_send(());
                }
            }
        }
        if let Some(rx) = control_rx {
            let mut control_worker = telemetry.worker_clone();
            control_worker.control_tx = None;
            tauri::async_runtime::spawn(Self::run_control_worker(rx, control_worker));
            if telemetry.has_pending_control_sync() {
                telemetry.wake_control_worker();
            }
        }
        if telemetry.runtime.enabled && telemetry.event_tx.is_none() {
            log::debug!("[pinvou3][behavior] telemetry disabled: queue initialization failed");
        }
        if telemetry.collecting.load(Ordering::Acquire) {
            telemetry.track(
                BehaviorEvent::new("app_opened")
                    .string("launch_type", "cold")
                    .integer(
                        "startup_duration_ms",
                        crate::platform::startup::elapsed_ms().min(600_000),
                    ),
            );
        }
        for (session_id, attempt) in recovered_tasks {
            if let Some(turn_id) = attempt.turn_id.as_deref() {
                telemetry.track(
                    super::events::task_fields(
                        BehaviorEvent::new("task_finished"),
                        &session_id,
                        &attempt,
                    )
                    .turn(turn_id)
                    .status("interrupted")
                    .stage("shutdown")
                    .integer("duration_ms", now_ms().saturating_sub(attempt.submitted_at))
                    .string("error_category", "internal_error"),
                );
            } else {
                telemetry.track(
                    super::events::task_fields(
                        BehaviorEvent::new("task_rejected"),
                        &session_id,
                        &attempt,
                    )
                    .string("error_category", "internal_error")
                    .stage("shutdown"),
                );
            }
        }
        if let Ok(mut tasks) = telemetry.tasks.lock() {
            tasks.clear();
            let _ = persist_task_attempts(&tasks);
        }
        telemetry
    }

    pub fn start_default_collection(&self) {
        if !self.runtime.enabled || self.collecting.load(Ordering::Acquire) {
            return;
        }
        let telemetry = self.clone();
        tauri::async_runtime::spawn(async move {
            let mut retry_delay = Duration::from_secs(1);
            loop {
                if !telemetry.state_healthy.load(Ordering::Acquire)
                    || telemetry.collection_is_blocked().await
                {
                    return;
                }
                match telemetry.set_collection_enabled_inner(true, None).await {
                    Ok(status) => {
                        if status.enabled {
                            telemetry.track(
                                BehaviorEvent::new("app_opened")
                                    .string("launch_type", "cold")
                                    .integer(
                                        "startup_duration_ms",
                                        crate::platform::startup::elapsed_ms().min(600_000),
                                    ),
                            );
                        }
                        return;
                    }
                    Err(error) => {
                        log::debug!(
                            "[pinvou3][behavior] default collection start deferred: {error:#}"
                        );
                        if !telemetry.state_healthy.load(Ordering::Acquire)
                            || telemetry.collection_is_blocked().await
                        {
                            return;
                        }
                        let jitter = Duration::from_millis(now_ms() % 251);
                        tokio::time::sleep(retry_delay.saturating_add(jitter)).await;
                        retry_delay = retry_delay.saturating_mul(2).min(RETRY_MAX_DELAY);
                    }
                }
            }
        });
    }

    pub(super) async fn collection_is_blocked(&self) -> bool {
        self.state
            .lock()
            .await
            .as_ref()
            .is_some_and(|state| state.control.collection_blocked)
    }

    pub fn track(&self, event: BehaviorEvent) {
        let Some(lease) = self.current_lease() else {
            return;
        };
        self.track_for_lease(event, &lease);
    }

    pub(super) fn track_for_lease(&self, event: BehaviorEvent, lease: &CollectionLease) {
        let _ = self.try_track_for_lease(event, lease);
    }

    pub(super) fn try_track_for_lease(
        &self,
        event: BehaviorEvent,
        lease: &CollectionLease,
    ) -> bool {
        let record = match self.prepare_record(event, lease) {
            Ok(record) => record,
            Err(error) => {
                log::debug!("[pinvou3][behavior] event serialization skipped: {error:#}");
                return false;
            }
        };
        let enqueued = self
            .outbox
            .lock()
            .map_err(|_| anyhow::anyhow!("behavior outbox lock poisoned"))
            .and_then(|mut outbox| {
                if !self.lease_is_current(lease) {
                    return Ok(false);
                }
                outbox.enqueue(record)?;
                Ok(true)
            });
        match enqueued {
            Ok(true) => {}
            Ok(false) => return false,
            Err(error) => {
                log::debug!("[pinvou3][behavior] persistent enqueue skipped: {error:#}");
                return false;
            }
        }
        if let Some(tx) = &self.event_tx {
            let _ = tx.try_send(());
        }
        true
    }

    pub(super) fn current_lease(&self) -> Option<CollectionLease> {
        let generation = self.authorization_generation.load(Ordering::Acquire);
        if !self.collecting.load(Ordering::Acquire) {
            return None;
        }
        let collection_period_id = self.collection_period_id.read().ok()?.clone()?;
        let lease = CollectionLease {
            generation,
            collection_period_id,
        };
        self.lease_is_current(&lease).then_some(lease)
    }

    pub(super) fn lease_is_current(&self, lease: &CollectionLease) -> bool {
        lease_matches_authorization(
            &self.collecting,
            &self.authorization_generation,
            &self.collection_period_id,
            lease,
        )
    }

    pub(super) fn invalidate_generation_above(&self, floor: u64) -> Result<u64> {
        let mut observed = self.authorization_generation.load(Ordering::Acquire);
        loop {
            let generation = observed
                .max(floor)
                .checked_add(1)
                .context("behavior telemetry generation is exhausted")?;
            match self.authorization_generation.compare_exchange_weak(
                observed,
                generation,
                Ordering::AcqRel,
                Ordering::Acquire,
            ) {
                Ok(_) => {
                    self.tool_install_terminals.discard_stale(generation);
                    return Ok(generation);
                }
                Err(actual) => observed = actual,
            }
        }
    }

    pub(super) fn enter_generation_exhausted_fail_close(&self) {
        self.authorization_generation
            .store(u64::MAX, Ordering::Release);
        self.collecting.store(false, Ordering::Release);
        self.credential_renewal_in_progress
            .store(false, Ordering::Release);
        self.tool_install_terminals.clear_all();
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
    }

    pub(super) fn finish_generation_exhausted<T>(
        &self,
        context: &str,
        mut failures: Vec<String>,
    ) -> Result<T> {
        failures.extend(self.clear_local_transient_state());
        self.wake_control_worker();
        if failures.is_empty() {
            anyhow::bail!(
                "{context}: behavior telemetry generation exhausted; collection permanently blocked"
            );
        }
        anyhow::bail!(
            "{context}: behavior telemetry generation exhausted; fail-close cleanup incomplete: {}",
            failures.join("; ")
        )
    }

    pub(super) fn complete_tool_install_terminal(
        &self,
        operation_id: String,
        generation: u64,
        event: BehaviorEvent,
    ) {
        self.tool_install_terminals.submit(
            DeferredToolInstallTerminal {
                operation_id,
                generation,
                event,
            },
            &self.authorization_generation,
            &self.credential_renewal_in_progress,
            |event, expected_generation| {
                let Some(lease) = lease_for_generation(self.current_lease(), expected_generation)
                else {
                    return false;
                };
                self.try_track_for_lease(event.clone(), &lease)
            },
        );
    }

    pub(super) fn flush_tool_install_terminals(&self, generation: u64) {
        self.tool_install_terminals.flush(
            &self.authorization_generation,
            &self.credential_renewal_in_progress,
            generation,
            |event, expected_generation| {
                let Some(lease) = lease_for_generation(self.current_lease(), expected_generation)
                else {
                    return false;
                };
                self.try_track_for_lease(event.clone(), &lease)
            },
        );
    }

    pub(super) fn rebind_lifecycle_state(&self, generation: u64, period_id: &str) -> Result<()> {
        let rebind = |lease: &mut CollectionLease| {
            if lease.generation == generation {
                lease.collection_period_id = period_id.to_string();
            }
        };
        let mut tasks = self
            .tasks
            .lock()
            .map_err(|_| anyhow::anyhow!("behavior task state lock poisoned"))?;
        for attempt in tasks.values_mut() {
            if attempt.generation == generation {
                attempt.collection_period_id = period_id.to_string();
            }
        }
        persist_task_attempts(&tasks)?;
        drop(tasks);
        if let Ok(mut sessions) = self.team_sessions.lock() {
            sessions.values_mut().for_each(rebind);
        }
        if let Ok(mut calls) = self.tool_calls.lock() {
            calls
                .values_mut()
                .for_each(|operation| rebind(&mut operation.lease));
        }
        if let Ok(mut submissions) = self.voice_submissions.lock() {
            submissions.values_mut().for_each(rebind);
        }
        if let Ok(mut artifacts) = self.artifacts.lock() {
            artifacts
                .values_mut()
                .for_each(|record| rebind(&mut record.lease));
        }
        if let Ok(mut turns) = self.latest_turns.lock() {
            turns
                .values_mut()
                .for_each(|record| rebind(&mut record.lease));
        }
        Ok(())
    }

    pub(super) fn prepare_record(
        &self,
        event: BehaviorEvent,
        lease: &CollectionLease,
    ) -> Result<OutboxRecord> {
        validate_client_event_fields(&event)?;
        let event_id = event.event_id.clone();
        let mut value = serde_json::to_value(event).context("serialize behavior event")?;
        let object = value
            .as_object_mut()
            .context("behavior event must serialize as an object")?;
        object.insert("schema_version".to_string(), json!(1));
        object.insert("event_source".to_string(), json!("client"));
        object.insert(
            "collection_period_id".to_string(),
            json!(lease.collection_period_id.clone()),
        );
        object.insert("producer".to_string(), json!("pinvou_agent"));
        object.insert(
            "producer_version".to_string(),
            json!(self.runtime.app_version.clone()),
        );
        object.insert(
            "app_version".to_string(),
            json!(self.runtime.app_version.clone()),
        );
        object.insert("platform".to_string(), json!(self.runtime.platform.clone()));
        object.insert("arch".to_string(), json!(self.runtime.arch.clone()));
        object.insert("channel".to_string(), json!(self.runtime.channel.clone()));
        object.insert("locale".to_string(), json!(self.runtime.locale.clone()));
        Ok(OutboxRecord {
            event_id,
            collection_period_id: lease.collection_period_id.clone(),
            generation: lease.generation,
            enqueued_at: now_ms(),
            delivery_attempt_count: 0,
            last_reason_code: None,
            event: value,
        })
    }

    pub(super) fn worker_clone(&self) -> Self {
        Self {
            client: reqwest::Client::builder()
                .connect_timeout(Duration::from_secs(5))
                .timeout(Duration::from_secs(12))
                .build()
                .unwrap_or_else(|_| reqwest::Client::new()),
            runtime: self.runtime.clone(),
            state: self.state.clone(),
            state_healthy: self.state_healthy.clone(),
            credential_gate: self.credential_gate.clone(),
            control_gate: self.control_gate.clone(),
            collecting: self.collecting.clone(),
            authorization_generation: self.authorization_generation.clone(),
            credential_renewal_in_progress: self.credential_renewal_in_progress.clone(),
            collection_period_id: self.collection_period_id.clone(),
            outbox: self.outbox.clone(),
            tasks: self.tasks.clone(),
            team_sessions: self.team_sessions.clone(),
            tool_calls: self.tool_calls.clone(),
            voice_submissions: self.voice_submissions.clone(),
            artifacts: self.artifacts.clone(),
            latest_turns: self.latest_turns.clone(),
            tool_install_terminals: self.tool_install_terminals.clone(),
            event_tx: None,
            control_tx: self.control_tx.clone(),
        }
    }

    pub(super) fn has_pending_control_sync(&self) -> bool {
        self.state
            .try_lock()
            .ok()
            .and_then(|state| state.as_ref().map(PersistedState::has_pending_control))
            .unwrap_or(false)
    }

    pub(super) async fn has_pending_control(&self) -> bool {
        self.state
            .lock()
            .await
            .as_ref()
            .is_some_and(PersistedState::has_pending_control)
    }

    pub(super) fn wake_control_worker(&self) {
        if let Some(tx) = &self.control_tx {
            let _ = tx.try_send(());
        }
    }

    pub(super) async fn run_control_worker(mut rx: mpsc::Receiver<()>, telemetry: Self) {
        while rx.recv().await.is_some() {
            let mut retry_delay = Duration::from_secs(1);
            while telemetry.has_pending_control().await {
                match telemetry.retry_pending_control().await {
                    Ok(()) => retry_delay = Duration::from_secs(1),
                    Err(error) => {
                        log::debug!(
                            "[pinvou3][behavior] pending control retry deferred: {error:#}"
                        );
                        let jitter = Duration::from_millis(now_ms() % 251);
                        let wait = retry_delay.saturating_add(jitter);
                        retry_delay = retry_delay.saturating_mul(2).min(RETRY_MAX_DELAY);
                        tokio::select! {
                            _ = tokio::time::sleep(wait) => {}
                            signal = rx.recv() => {
                                if signal.is_none() {
                                    return;
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    pub(super) async fn run_worker(mut rx: mpsc::Receiver<()>, telemetry: Self) {
        let mut retry_delay = Duration::from_secs(1);
        loop {
            let has_events = telemetry
                .outbox
                .lock()
                .is_ok_and(|outbox| !outbox.records.is_empty());
            if !has_events || !telemetry.collecting.load(Ordering::Acquire) {
                if rx.recv().await.is_none() {
                    break;
                }
                let _ = tokio::time::timeout(EVENT_FLUSH_DEBOUNCE, rx.recv()).await;
                continue;
            }
            let batch = telemetry
                .outbox
                .lock()
                .map(|outbox| outbox.batch())
                .unwrap_or_default();
            if batch.is_empty() {
                let dropped = telemetry.outbox.lock().is_ok_and(|mut outbox| {
                    outbox.drop_oversized_front().unwrap_or_else(|error| {
                        log::debug!(
                            "[pinvou3][behavior] failed to drop oversized event: {error:#}"
                        );
                        false
                    })
                });
                if !dropped {
                    tokio::time::sleep(Duration::from_secs(1)).await;
                }
                continue;
            }
            match telemetry.send_events(&batch).await {
                Ok(results) => {
                    let retryable_remains = telemetry.outbox.lock().map_or(true, |mut outbox| {
                        outbox
                            .apply_results(&batch, &results)
                            .unwrap_or_else(|error| {
                                log::debug!(
                                    "[pinvou3][behavior] persist delivery results failed: {error:#}"
                                );
                                true
                            })
                    });
                    if retryable_remains {
                        let jitter = Duration::from_millis(now_ms() % 251);
                        tokio::time::sleep(retry_delay.saturating_add(jitter)).await;
                        retry_delay = retry_delay.saturating_mul(2).min(RETRY_MAX_DELAY);
                    } else {
                        retry_delay = Duration::from_secs(1);
                    }
                }
                Err(error) => {
                    log::debug!("[pinvou3][behavior] upload deferred: {error:#}");
                    if let Ok(mut outbox) = telemetry.outbox.lock() {
                        let _ = outbox.mark_retry(&batch, "network_unavailable");
                    }
                    let jitter = Duration::from_millis(now_ms() % 251);
                    tokio::time::sleep(retry_delay.saturating_add(jitter)).await;
                    retry_delay = retry_delay.saturating_mul(2).min(RETRY_MAX_DELAY);
                }
            }
        }
    }

    pub(super) async fn send_events(
        &self,
        records: &[OutboxRecord],
    ) -> Result<HashMap<String, (DeliveryStatus, Option<String>)>> {
        let events: Vec<&Value> = records.iter().map(|record| &record.event).collect();
        if events.is_empty() {
            return Ok(HashMap::new());
        }
        let credentials = self.ensure_credentials().await?;
        let body = json!({
            "batch_id": token("batch_", 18),
            "device_id": credentials.device_id.clone(),
            "events": events,
        });
        let response = self
            .client
            .post(&credentials.behavior_events_url)
            .bearer_auth(&credentials.device_token)
            .json(&body)
            .send()
            .await
            .context("send behavior events")?;
        if response.status() == reqwest::StatusCode::GONE {
            let error_code = telemetry_error_code(response).await;
            if error_code == "device_credentials_expired" {
                let _ = self.recover_expired_credentials(&credentials).await?;
                anyhow::bail!("behavior telemetry credentials expired and were renewed");
            }
            if error_code == "device_data_deleted" {
                let _ = self.handle_authoritative_deletion(&credentials).await?;
                anyhow::bail!("behavior telemetry data was deleted; local collection disabled");
            }
            anyhow::bail!("behavior telemetry credentials unavailable: {error_code}");
        }
        if !response.status().is_success() {
            anyhow::bail!("behavior events endpoint returned {}", response.status());
        }
        let response: EventUploadResponse = response
            .json()
            .await
            .context("parse behavior event delivery response")?;
        Ok(delivery_results(records, response))
    }

    pub(super) async fn recover_expired_credentials(&self, expired: &Credentials) -> Result<bool> {
        let _control_guard = self.control_gate.lock().await;
        self.recover_expired_credentials_locked(expired).await
    }

    pub(super) async fn handle_authoritative_deletion(
        &self,
        deleted: &Credentials,
    ) -> Result<bool> {
        let _control_guard = self.control_gate.lock().await;
        self.handle_authoritative_deletion_locked(deleted).await
    }

    pub(super) async fn handle_authoritative_deletion_locked(
        &self,
        deleted: &Credentials,
    ) -> Result<bool> {
        let mut guard = self.state.lock().await;
        let Some(state) = guard.as_ref() else {
            return Ok(false);
        };
        if !deleted_credentials_match(state, deleted) {
            return Ok(false);
        }

        let (generation, generation_exhausted) =
            match self.invalidate_generation_above(state.control.generation) {
                Ok(generation) => (generation, false),
                Err(_) => {
                    self.enter_generation_exhausted_fail_close();
                    (u64::MAX, true)
                }
            };
        self.collecting.store(false, Ordering::Release);
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
        let fresh = state_after_authoritative_deletion(state, &self.runtime, deleted, generation);

        let mut failures = Vec::new();
        if let Err(error) = persist_state(&fresh) {
            self.state_healthy.store(false, Ordering::Release);
            failures.push(format!("persist disabled telemetry state: {error:#}"));
        }
        *guard = Some(fresh);
        drop(guard);

        if generation_exhausted {
            return self.finish_generation_exhausted("authoritative device deletion", failures);
        }
        failures.extend(self.clear_local_transient_state());
        self.wake_control_worker();

        if failures.is_empty() {
            Ok(true)
        } else {
            anyhow::bail!(
                "authoritative deletion cleanup incomplete: {}",
                failures.join("; ")
            )
        }
    }

    pub(super) async fn handle_authoritative_installation_deletion_locked(
        &self,
        expected: &PersistedState,
    ) -> Result<bool> {
        let mut guard = self.state.lock().await;
        let Some(current) = guard.as_ref() else {
            return Ok(false);
        };
        if current.installation_id != expected.installation_id
            || current.registration_secret != expected.registration_secret
        {
            return Ok(false);
        }

        let (generation, generation_exhausted) =
            match self.invalidate_generation_above(current.control.generation) {
                Ok(generation) => (generation, false),
                Err(_) => {
                    self.enter_generation_exhausted_fail_close();
                    (u64::MAX, true)
                }
            };
        self.collecting.store(false, Ordering::Release);
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
        let blocked = state_after_authoritative_installation_deletion(
            current,
            expected,
            &self.runtime,
            generation,
        );
        let mut failures = Vec::new();
        if let Err(error) = persist_state(&blocked) {
            self.state_healthy.store(false, Ordering::Release);
            failures.push(format!(
                "persist authoritative installation deletion: {error:#}"
            ));
        }
        *guard = Some(blocked);
        drop(guard);

        if generation_exhausted {
            return self
                .finish_generation_exhausted("authoritative installation deletion", failures);
        }
        failures.extend(self.clear_local_transient_state());
        self.wake_control_worker();
        if failures.is_empty() {
            Ok(true)
        } else {
            anyhow::bail!(
                "authoritative installation deletion cleanup incomplete: {}",
                failures.join("; ")
            )
        }
    }

    pub(super) fn clear_local_transient_state(&self) -> Vec<String> {
        let mut failures = Vec::new();
        match self.outbox.lock() {
            Ok(mut outbox) => {
                if let Err(error) = outbox.clear() {
                    failures.push(format!("clear outbox: {error:#}"));
                }
            }
            Err(_) => failures.push("clear outbox: lock poisoned".to_string()),
        }
        match self.tasks.lock() {
            Ok(mut tasks) => {
                tasks.clear();
                if let Err(error) = persist_task_attempts(&tasks) {
                    failures.push(format!("clear task attempts: {error:#}"));
                }
            }
            Err(_) => failures.push("clear task attempts: lock poisoned".to_string()),
        }
        if let Ok(mut sessions) = self.team_sessions.lock() {
            sessions.clear();
        }
        if let Ok(mut calls) = self.tool_calls.lock() {
            calls.clear();
        }
        if let Ok(mut submissions) = self.voice_submissions.lock() {
            submissions.clear();
        }
        if let Ok(mut artifacts) = self.artifacts.lock() {
            artifacts.clear();
        }
        if let Ok(mut latest_turns) = self.latest_turns.lock() {
            latest_turns.clear();
        }
        self.tool_install_terminals.clear_all();
        failures
    }

    pub(super) async fn recover_expired_credentials_locked(
        &self,
        expired: &Credentials,
    ) -> Result<bool> {
        let snapshot = self
            .state
            .lock()
            .await
            .clone()
            .context("behavior telemetry state missing during credential renewal")?;
        if !endpoint_matches_runtime(&snapshot, &self.runtime)
            || snapshot.has_pending_control()
            || snapshot.device_id.as_deref() != Some(expired.device_id.as_str())
            || snapshot.device_token.as_deref() != Some(expired.device_token.as_str())
        {
            return Ok(false);
        }
        let enrollment_token = self
            .runtime
            .enrollment_token
            .as_deref()
            .context("PINVOU_TELEMETRY_ENROLLMENT_TOKEN is not configured")?;
        let response = self
            .client
            .post(format!(
                "{}/v1/register",
                snapshot.telemetry_base_url.trim_end_matches('/')
            ))
            .json(&json!({
                "enrollment_token": enrollment_token,
                "hardware_claim": format!("pinvou3-installation:{}", snapshot.installation_id),
                "registration_secret": snapshot.registration_secret,
                "hardware_source": "installation_id",
                "identity_quality": "installation_only",
                "app_version": self.runtime.app_version,
                "platform": self.runtime.platform,
                "arch": self.runtime.arch,
            }))
            .send()
            .await
            .context("renew expired behavior telemetry registration")?;
        if response.status() == reqwest::StatusCode::GONE {
            let error_code = telemetry_error_code(response).await;
            if error_code == "installation_data_deleted" {
                let handled = self
                    .handle_authoritative_installation_deletion_locked(&snapshot)
                    .await?;
                if !handled {
                    self.fail_close_unmatched_authoritative_deletion();
                }
                anyhow::bail!(
                    "behavior telemetry installation data was deleted; local collection disabled"
                );
            }
            anyhow::bail!("telemetry registration renewal unavailable: {error_code}");
        }
        if !response.status().is_success() {
            anyhow::bail!(
                "telemetry registration renewal returned {}",
                response.status()
            );
        }
        let registered: RegistrationResponse = response
            .json()
            .await
            .context("parse renewed telemetry registration response")?;
        let renewed = Credentials {
            device_id: registered.device_id,
            device_token: registered.device_token,
            behavior_events_url: self.runtime.behavior_events_url.clone(),
        };
        let request_id = token("control_", 18);
        let started_response = self
            .request_control(
                &renewed,
                "collection/start",
                json!({ "request_id": request_id }),
            )
            .await?;
        if started_response.status() == reqwest::StatusCode::GONE {
            match parse_control_gone(started_response, "renewed collection/start").await? {
                GoneReason::DeviceCredentialsExpired => {
                    anyhow::bail!("renewed behavior credentials expired before collection start");
                }
                GoneReason::DeviceDataDeleted => {
                    let handled = self
                        .handle_authoritative_installation_deletion_locked(&snapshot)
                        .await?;
                    if !handled {
                        self.fail_close_unmatched_authoritative_deletion();
                    }
                    anyhow::bail!(
                        "renewed behavior device data was deleted before collection start"
                    );
                }
            }
        }
        if !started_response.status().is_success() {
            anyhow::bail!(
                "renewed behavior collection start returned {}",
                started_response.status()
            );
        }
        let started: CollectionStartResponse = started_response
            .json()
            .await
            .context("parse renewed behavior collection response")?;

        let mut guard = self.state.lock().await;
        let state = guard
            .as_mut()
            .context("behavior telemetry state disappeared during credential renewal")?;
        if state.device_id.as_deref() != Some(expired.device_id.as_str())
            || state.device_token.as_deref() != Some(expired.device_token.as_str())
            || state.control.generation != snapshot.control.generation
            || state.control.collection_blocked
            || state.has_pending_control()
        {
            return Ok(false);
        }
        let generation = self.authorization_generation.load(Ordering::Acquire);
        let renewal_transition = CredentialRenewalTransition::begin(
            &self.credential_renewal_in_progress,
            &self.tool_install_terminals,
            generation,
        );
        self.collecting.store(false, Ordering::Release);
        self.outbox
            .lock()
            .map_err(|_| anyhow::anyhow!("behavior outbox lock poisoned"))?
            .rebind_period(&started.collection_period_id, generation)?;
        self.rebind_lifecycle_state(generation, &started.collection_period_id)?;
        state.device_id = Some(renewed.device_id);
        state.device_token = Some(renewed.device_token);
        state.control.enabled = true;
        state.control.collection_period_id = Some(started.collection_period_id.clone());
        state.control.generation = generation;
        state.control.pending_request = None;
        persist_state(state)?;
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = Some(started.collection_period_id);
        }
        self.collecting.store(true, Ordering::Release);
        renewal_transition.complete();
        self.flush_tool_install_terminals(generation);
        Ok(true)
    }

    pub async fn status(&self) -> BehaviorTelemetryStatus {
        let state = self.state.lock().await;
        let control = state.as_ref().map(|value| &value.control);
        let (pending_event_count, dropped_event_count) = self
            .outbox
            .lock()
            .map(|outbox| (outbox.records.len(), outbox.dropped_event_count))
            .unwrap_or_default();
        BehaviorTelemetryStatus {
            configured: self.runtime.enabled && self.state_healthy.load(Ordering::Acquire),
            enabled: self.collecting.load(Ordering::Acquire),
            user_enabled: control.and_then(|value| value.user_enabled),
            collection_period_id: control.and_then(|value| value.collection_period_id.clone()),
            withdrawal_pending: control.is_some_and(|value| {
                matches!(
                    value.pending_request.as_ref().map(|request| &request.kind),
                    Some(PendingControlKind::End)
                )
            }),
            deletion_pending: control.is_some_and(|value| {
                matches!(
                    value.pending_request.as_ref().map(|request| &request.kind),
                    Some(PendingControlKind::Delete)
                ) || !value.pending_deletions.is_empty()
            }),
            last_deletion_request_id: control
                .and_then(|value| value.last_deletion_request_id.clone()),
            retention_days: SERVER_LINKABLE_RETENTION_DAYS,
            pending_event_count,
            dropped_event_count,
        }
    }

    pub async fn set_user_collection_enabled(
        &self,
        enabled: bool,
    ) -> Result<BehaviorTelemetryStatus> {
        self.set_collection_enabled_inner(enabled, Some(enabled))
            .await
    }

    pub async fn set_collection_enabled(&self, enabled: bool) -> Result<BehaviorTelemetryStatus> {
        self.set_collection_enabled_inner(enabled, None).await
    }

    pub(super) async fn set_collection_enabled_inner(
        &self,
        enabled: bool,
        user_preference: Option<bool>,
    ) -> Result<BehaviorTelemetryStatus> {
        let _control_guard = self.control_gate.lock().await;
        if !self.state_healthy.load(Ordering::Acquire) {
            anyhow::bail!(
                "behavior telemetry state is unreadable; repair or delete the local state file before continuing"
            );
        }
        let default_start_blocked = {
            let mut guard = self.state.lock().await;
            let state = guard.get_or_insert_with(|| PersistedState::fresh(&self.runtime));
            if let Some(preference) = user_preference {
                state.control.user_enabled = Some(preference);
                persist_state(state).context("persist behavior telemetry user preference")?;
            }
            user_preference.is_none() && enabled && state.control.user_enabled == Some(false)
        };
        if default_start_blocked {
            return Ok(self.status().await);
        }
        if enabled {
            if !self.runtime.enabled {
                anyhow::bail!("behavior telemetry service is not configured");
            }
            if self.collecting.load(Ordering::Acquire) {
                return Ok(self.status().await);
            }
            if self.collection_is_blocked().await {
                anyhow::bail!(
                    "behavior telemetry collection is blocked after authoritative deletion"
                );
            }
            while self
                .state
                .lock()
                .await
                .as_ref()
                .is_some_and(PersistedState::has_pending_control)
            {
                self.retry_pending_control_locked().await?;
            }
            if self.collection_is_blocked().await {
                anyhow::bail!(
                    "behavior telemetry collection is blocked after authoritative deletion"
                );
            }
            self.migrate_endpoint_locked().await?;
            if self.collection_is_blocked().await {
                anyhow::bail!(
                    "behavior telemetry collection is blocked after authoritative deletion"
                );
            }
            let credentials = self.ensure_credentials().await?;
            let request_id = token("control_", 18);
            let response = self
                .start_collection_with_renewal_locked(&credentials, request_id)
                .await?;
            let mut guard = self.state.lock().await;
            let state = guard.get_or_insert_with(|| PersistedState::fresh(&self.runtime));
            let emit_cohort = !state.control.cohort_started;
            let generation = match self.invalidate_generation_above(state.control.generation) {
                Ok(generation) => generation,
                Err(_) => {
                    state.control.collection_period_id = Some(response.collection_period_id);
                    state.control.pending_request = None;
                    converge_generation_exhausted(state);
                    self.enter_generation_exhausted_fail_close();
                    let mut failures = Vec::new();
                    if let Err(error) = persist_state(state) {
                        self.state_healthy.store(false, Ordering::Release);
                        failures.push(format!(
                            "persist generation-exhausted collection start: {error:#}"
                        ));
                    }
                    drop(guard);
                    return self.finish_generation_exhausted("start behavior collection", failures);
                }
            };
            state.control.enabled = true;
            state.control.collection_period_id = Some(response.collection_period_id);
            state.control.generation = generation;
            state.control.pending_request = None;
            state.control.cohort_started = true;
            persist_state(state)?;
            if let Ok(mut period) = self.collection_period_id.write() {
                *period = state.control.collection_period_id.clone();
            }
            self.collecting.store(true, Ordering::Release);
            drop(guard);
            if emit_cohort {
                let previous_version_present =
                    paths::pinvou3_home().join("settings.json").is_file();
                self.track(
                    BehaviorEvent::new("analytics_cohort_started")
                        .string("eligibility_source", "first_open")
                        .string(
                            "install_source",
                            if previous_version_present {
                                "upgrade"
                            } else {
                                "direct"
                            },
                        )
                        .boolean("previous_version_present", previous_version_present),
                );
            }
        } else {
            let mut generation_exhausted = false;
            let mut exhaustion_failures = Vec::new();
            {
                let mut guard = self.state.lock().await;
                let state = guard.get_or_insert_with(|| PersistedState::fresh(&self.runtime));
                match self.invalidate_generation_above(state.control.generation) {
                    Ok(generation) => prepare_disabled_state(state, generation),
                    Err(_) => {
                        generation_exhausted = true;
                        converge_generation_exhausted(state);
                        self.enter_generation_exhausted_fail_close();
                    }
                }
                if let Err(error) = persist_state(state) {
                    self.state_healthy.store(false, Ordering::Release);
                    if generation_exhausted {
                        exhaustion_failures
                            .push(format!("persist generation-exhausted stop: {error:#}"));
                    } else {
                        return Err(error.context("persist disabled behavior telemetry state"));
                    }
                }
            }
            self.collecting.store(false, Ordering::Release);
            if let Ok(mut period) = self.collection_period_id.write() {
                *period = None;
            }
            if generation_exhausted {
                return self
                    .finish_generation_exhausted("stop behavior collection", exhaustion_failures);
            }
            self.wake_control_worker();
            let failures = self.clear_local_transient_state();
            if !failures.is_empty() {
                anyhow::bail!(
                    "stop behavior collection cleanup incomplete: {}",
                    failures.join("; ")
                );
            }
        }
        Ok(self.status().await)
    }

    pub async fn delete_local_data(&self) -> Result<BehaviorTelemetryStatus> {
        self.outbox
            .lock()
            .map_err(|_| anyhow::anyhow!("behavior outbox lock poisoned"))?
            .clear()?;
        if let Ok(mut tasks) = self.tasks.lock() {
            tasks.clear();
            persist_task_attempts(&tasks)?;
        }
        if let Ok(mut submissions) = self.voice_submissions.lock() {
            submissions.clear();
        }
        if let Ok(mut artifacts) = self.artifacts.lock() {
            artifacts.clear();
        }
        if let Ok(mut latest_turns) = self.latest_turns.lock() {
            latest_turns.clear();
        }
        Ok(self.status().await)
    }

    pub async fn delete_remote_data(&self) -> Result<BehaviorTelemetryStatus> {
        let _control_guard = self.control_gate.lock().await;
        let mut generation_exhausted = false;
        let mut failures = Vec::new();
        {
            let mut guard = self.state.lock().await;
            let state = guard.get_or_insert_with(|| PersistedState::fresh(&self.runtime));
            let generation = match self.invalidate_generation_above(state.control.generation) {
                Ok(generation) => generation,
                Err(_) => {
                    generation_exhausted = true;
                    self.enter_generation_exhausted_fail_close();
                    u64::MAX
                }
            };
            let mut deletion_targets = state.control.pending_deletions.clone();
            deletion_targets.extend(state.deletion_targets());
            state.control.enabled = false;
            state.control.collection_period_id = None;
            state.control.generation = generation;
            state.control.collection_blocked = true;
            state.control.pending_request = None;
            state.control.pending_deletions = deletion_targets;
            enqueue_retired_endpoint_deletions(state);
            if let Err(error) = persist_state(state) {
                self.state_healthy.store(false, Ordering::Release);
                if generation_exhausted {
                    failures.push(format!(
                        "persist generation-exhausted remote deletion: {error:#}"
                    ));
                } else {
                    return Err(error.context("persist remote behavior deletion"));
                }
            }
        }
        self.collecting.store(false, Ordering::Release);
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
        if generation_exhausted {
            return self.finish_generation_exhausted("delete remote behavior data", failures);
        }
        if self.has_pending_control().await {
            self.wake_control_worker();
        }
        self.delete_local_data().await
    }

    pub(super) async fn retry_pending_control(&self) -> Result<()> {
        let _control_guard = self.control_gate.lock().await;
        self.retry_pending_control_locked().await
    }

    pub(super) async fn retry_pending_control_locked(&self) -> Result<()> {
        let snapshot = self.state.lock().await.clone();
        let Some(snapshot) = snapshot else {
            return Ok(());
        };
        if let Some(pending) = snapshot.control.pending_request.clone() {
            let credentials = self
                .credentials_for_pending(&pending)
                .await
                .context("credentials unavailable for pending behavior control")?;
            return self.send_pending_control(&credentials, pending).await;
        }
        let Some(pending) = snapshot.control.pending_deletions.first().cloned() else {
            return Ok(());
        };
        self.send_pending_deletion(pending).await
    }

    pub(super) async fn send_pending_deletion(
        &self,
        pending: PendingEndpointDeletion,
    ) -> Result<()> {
        let credentials = Credentials {
            device_id: pending.device_id.clone(),
            device_token: pending.device_token.clone(),
            behavior_events_url: pending.behavior_events_url.clone(),
        };
        let response = self
            .request_control(
                &credentials,
                "deletion",
                json!({ "request_id": pending.request_id.clone() }),
            )
            .await?;
        let (deletion_request_id, authoritative_deleted) =
            if response.status() == reqwest::StatusCode::GONE {
                match parse_control_gone(response, "deletion").await? {
                    GoneReason::DeviceCredentialsExpired => (None, false),
                    GoneReason::DeviceDataDeleted => (None, true),
                }
            } else {
                if !response.status().is_success() {
                    anyhow::bail!("behavior control deletion returned {}", response.status());
                }
                (
                    Some(
                        response
                            .json::<DeletionResponse>()
                            .await
                            .context("parse behavior control deletion")?
                            .deletion_request_id,
                    ),
                    false,
                )
            };
        let mut guard = self.state.lock().await;
        let Some(state) = guard.as_mut() else {
            return Ok(());
        };
        if !state
            .control
            .pending_deletions
            .first()
            .is_some_and(|current| current.same_request(&pending))
        {
            return Ok(());
        }
        let mut generation_exhausted = false;
        let completion_generation = if authoritative_deleted
            || !state.control.collection_blocked
            || state.control.enabled
            || state.control.collection_period_id.is_some()
        {
            match self.invalidate_generation_above(state.control.generation) {
                Ok(generation) => generation,
                Err(_) => {
                    generation_exhausted = true;
                    self.enter_generation_exhausted_fail_close();
                    u64::MAX
                }
            }
        } else {
            self.authorization_generation.load(Ordering::Acquire)
        };
        self.collecting.store(false, Ordering::Release);
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
        state.control.pending_deletions.remove(0);
        state.control.enabled = false;
        state.control.collection_period_id = None;
        state.control.pending_request = None;
        state.control.collection_blocked = true;
        state.control.generation = state.control.generation.max(completion_generation);
        if deletion_request_id.is_some() {
            state.control.last_deletion_request_id = deletion_request_id;
        }
        state.retired_endpoints.retain(|retired| {
            retired.behavior_events_url != pending.behavior_events_url
                || retired.device_id != pending.device_id
                || retired.device_token != pending.device_token
        });
        if state.behavior_events_url == pending.behavior_events_url
            && state.device_id.as_deref() == Some(pending.device_id.as_str())
            && state.device_token.as_deref() == Some(pending.device_token.as_str())
        {
            state.device_id = None;
            state.device_token = None;
        }
        let mut failures = Vec::new();
        if let Err(error) = persist_state(state) {
            self.state_healthy.store(false, Ordering::Release);
            failures.push(format!("persist endpoint deletion: {error:#}"));
        }
        let has_more_deletions = !state.control.pending_deletions.is_empty();
        drop(guard);
        if has_more_deletions {
            self.wake_control_worker();
        }
        if generation_exhausted {
            return self.finish_generation_exhausted("endpoint deletion", failures);
        }
        failures.extend(self.clear_local_transient_state());
        if !failures.is_empty() {
            anyhow::bail!(
                "endpoint deletion cleanup incomplete: {}",
                failures.join("; ")
            );
        }
        Ok(())
    }

    pub(super) async fn send_pending_control(
        &self,
        credentials: &Credentials,
        pending: PendingControlRequest,
    ) -> Result<()> {
        match pending.kind {
            PendingControlKind::End => {
                let response = self
                    .request_control(
                        credentials,
                        "collection/end",
                        json!({
                            "request_id": pending.request_id.clone(),
                            "collection_period_id": pending.collection_period_id.clone(),
                        }),
                    )
                    .await?;
                if response.status() == reqwest::StatusCode::GONE {
                    match parse_control_gone(response, "collection/end").await? {
                        GoneReason::DeviceCredentialsExpired => {}
                        GoneReason::DeviceDataDeleted => {
                            let _ = self
                                .handle_authoritative_deletion_locked(credentials)
                                .await?;
                            anyhow::bail!(
                                "behavior telemetry data was deleted while ending collection"
                            );
                        }
                    }
                } else {
                    if !response.status().is_success() {
                        anyhow::bail!(
                            "behavior control collection/end returned {}",
                            response.status()
                        );
                    }
                    let _: CollectionEndResponse = response
                        .json()
                        .await
                        .context("parse behavior control collection/end")?;
                }
                let mut guard = self.state.lock().await;
                if let Some(state) = guard.as_mut() {
                    if !pending_request_matches(state, &pending) {
                        return Ok(());
                    }
                    state.control.collection_period_id = None;
                    state.control.pending_request = None;
                    persist_state(state)?;
                }
            }
            PendingControlKind::Delete => {
                let response = self
                    .request_control(
                        credentials,
                        "deletion",
                        json!({ "request_id": pending.request_id.clone() }),
                    )
                    .await?;
                let (deletion_request_id, authoritative_deleted) = if response.status()
                    == reqwest::StatusCode::GONE
                {
                    match parse_control_gone(response, "legacy deletion").await? {
                        GoneReason::DeviceCredentialsExpired => (None, false),
                        GoneReason::DeviceDataDeleted => (None, true),
                    }
                } else {
                    if !response.status().is_success() {
                        anyhow::bail!("behavior control deletion returned {}", response.status());
                    }
                    (
                        Some(
                            response
                                .json::<DeletionResponse>()
                                .await
                                .context("parse behavior control deletion")?
                                .deletion_request_id,
                        ),
                        false,
                    )
                };
                let mut guard = self.state.lock().await;
                let Some(state) = guard.as_mut() else {
                    return Ok(());
                };
                if !pending_request_matches(state, &pending) {
                    return Ok(());
                }
                let mut generation_exhausted = false;
                let generation = if authoritative_deleted {
                    match self.invalidate_generation_above(state.control.generation) {
                        Ok(generation) => generation,
                        Err(_) => {
                            generation_exhausted = true;
                            self.enter_generation_exhausted_fail_close();
                            u64::MAX
                        }
                    }
                } else {
                    self.authorization_generation.load(Ordering::Acquire)
                };
                converge_legacy_deletion(state, credentials, deletion_request_id, generation);
                self.collecting.store(false, Ordering::Release);
                if let Ok(mut period) = self.collection_period_id.write() {
                    *period = None;
                }
                let mut failures = Vec::new();
                if let Err(error) = persist_state(state) {
                    self.state_healthy.store(false, Ordering::Release);
                    failures.push(format!("persist legacy behavior deletion: {error:#}"));
                }
                let has_more_deletions = !state.control.pending_deletions.is_empty();
                drop(guard);
                if generation_exhausted {
                    return self.finish_generation_exhausted("legacy behavior deletion", failures);
                }
                failures.extend(self.clear_local_transient_state());
                if has_more_deletions {
                    self.wake_control_worker();
                }
                if !failures.is_empty() {
                    anyhow::bail!(
                        "legacy behavior deletion cleanup incomplete: {}",
                        failures.join("; ")
                    );
                }
            }
        }
        Ok(())
    }

    pub(super) async fn migrate_endpoint_locked(&self) -> Result<()> {
        let snapshot = self.state.lock().await.clone();
        let Some(snapshot) = snapshot else {
            return Ok(());
        };
        if endpoint_matches_runtime(&snapshot, &self.runtime) {
            return Ok(());
        }
        if snapshot.has_pending_control() {
            anyhow::bail!("pending behavior control must finish before endpoint migration");
        }
        let Some(period_id) = snapshot.control.collection_period_id.clone() else {
            return Ok(());
        };
        if persisted_credentials_from_state(&snapshot).is_none() {
            anyhow::bail!(
                "cannot end the previous behavior collection period because its credentials are unavailable"
            );
        }
        {
            let mut guard = self.state.lock().await;
            let state = guard
                .as_mut()
                .context("behavior telemetry state disappeared during endpoint migration")?;
            if endpoint_matches_runtime(state, &self.runtime) {
                return Ok(());
            }
            state.control.enabled = false;
            state.control.pending_request = Some(PendingControlRequest {
                kind: PendingControlKind::End,
                request_id: token("control_", 18),
                collection_period_id: Some(period_id),
                behavior_events_url: state.behavior_events_url.clone(),
            });
            persist_state(state)?;
        }
        self.retry_pending_control_locked().await
    }

    pub(super) async fn start_collection_with_renewal_locked(
        &self,
        credentials: &Credentials,
        request_id: String,
    ) -> Result<CollectionStartResponse> {
        let response = self
            .request_control(
                credentials,
                "collection/start",
                json!({ "request_id": request_id }),
            )
            .await?;
        if response.status() == reqwest::StatusCode::GONE {
            match parse_control_gone(response, "collection/start").await? {
                GoneReason::DeviceCredentialsExpired => {
                    if self.recover_expired_credentials_locked(credentials).await? {
                        let collection_period_id = self
                            .state
                            .lock()
                            .await
                            .as_ref()
                            .and_then(|state| state.control.collection_period_id.clone())
                            .context("renewed behavior collection period is unavailable")?;
                        return Ok(CollectionStartResponse {
                            collection_period_id,
                        });
                    }
                    anyhow::bail!("behavior collection credentials expired");
                }
                GoneReason::DeviceDataDeleted => {
                    let _ = self
                        .handle_authoritative_deletion_locked(credentials)
                        .await?;
                    anyhow::bail!("behavior telemetry data was deleted; local collection disabled");
                }
            }
        }
        if !response.status().is_success() {
            anyhow::bail!(
                "behavior control collection/start returned {}",
                response.status()
            );
        }
        response
            .json()
            .await
            .context("parse behavior control collection/start")
    }

    pub(super) async fn request_control(
        &self,
        credentials: &Credentials,
        path: &str,
        mut body: Value,
    ) -> Result<reqwest::Response> {
        if let Value::Object(ref mut object) = body {
            object.insert(
                "device_id".to_string(),
                Value::String(credentials.device_id.clone()),
            );
        }
        let base = credentials
            .behavior_events_url
            .trim_end_matches('/')
            .strip_suffix("/events")
            .unwrap_or(credentials.behavior_events_url.trim_end_matches('/'));
        self.client
            .post(format!("{base}/{path}"))
            .bearer_auth(&credentials.device_token)
            .json(&body)
            .send()
            .await
            .with_context(|| format!("send behavior control {path}"))
    }

    pub(super) async fn ensure_credentials(&self) -> Result<Credentials> {
        if !self.state_healthy.load(Ordering::Acquire) {
            anyhow::bail!(
                "behavior telemetry state is unreadable; refusing to replace the persisted installation identity"
            );
        }
        if let Some(credentials) = self.current_credentials().await {
            return Ok(credentials);
        }
        let _credential_guard = self.credential_gate.lock().await;
        if let Some(credentials) = self.current_credentials().await {
            return Ok(credentials);
        }
        let enrollment_token = self
            .runtime
            .enrollment_token
            .as_deref()
            .context("PINVOU_TELEMETRY_ENROLLMENT_TOKEN is not configured")?;
        let mut state = {
            let mut guard = self.state.lock().await;
            let state = guard.get_or_insert_with(|| PersistedState::fresh(&self.runtime));
            state.clone()
        };
        if !endpoint_matches_runtime(&state, &self.runtime) {
            if state.has_pending_control() || state.control.collection_period_id.is_some() {
                anyhow::bail!(
                    "previous behavior endpoint control state must be resolved before registration"
                );
            }
            state = state.rebind_endpoint(&self.runtime);
        }
        let body = json!({
            "enrollment_token": enrollment_token,
            "hardware_claim": format!("pinvou3-installation:{}", state.installation_id),
            "registration_secret": state.registration_secret,
            "hardware_source": "installation_id",
            "identity_quality": "installation_only",
            "app_version": self.runtime.app_version,
            "platform": self.runtime.platform,
            "arch": self.runtime.arch,
        });
        let response = self
            .client
            .post(format!(
                "{}/v1/register",
                state.telemetry_base_url.trim_end_matches('/')
            ))
            .json(&body)
            .send()
            .await
            .context("register behavior telemetry device")?;
        if response.status() == reqwest::StatusCode::GONE {
            let error_code = telemetry_error_code(response).await;
            if error_code == "installation_data_deleted" {
                let handled = self
                    .handle_authoritative_installation_deletion_locked(&state)
                    .await?;
                if !handled {
                    self.fail_close_unmatched_authoritative_deletion();
                }
                anyhow::bail!(
                    "behavior telemetry installation data was deleted; local collection disabled"
                );
            }
            anyhow::bail!("telemetry registration unavailable: {error_code}");
        }
        if !response.status().is_success() {
            anyhow::bail!("telemetry registration returned {}", response.status());
        }
        let registered: RegistrationResponse = response
            .json()
            .await
            .context("parse telemetry registration response")?;
        state.device_id = Some(registered.device_id);
        state.device_token = Some(registered.device_token);
        let mut guard = self.state.lock().await;
        let current = guard
            .as_ref()
            .context("behavior telemetry state disappeared during registration")?;
        if current.installation_id != state.installation_id
            || current.registration_secret != state.registration_secret
            || current.control.generation != state.control.generation
            || current.control.collection_blocked
            || current.has_pending_control()
        {
            anyhow::bail!("behavior telemetry state changed while registration was in flight");
        }
        persist_state(&state)?;
        *guard = Some(state);
        drop(guard);
        self.current_credentials()
            .await
            .context("telemetry credentials missing after registration")
    }

    pub(super) fn fail_close_unmatched_authoritative_deletion(&self) {
        self.state_healthy.store(false, Ordering::Release);
        self.collecting.store(false, Ordering::Release);
        if let Ok(mut period) = self.collection_period_id.write() {
            *period = None;
        }
    }

    pub(super) async fn current_credentials(&self) -> Option<Credentials> {
        let state = self.state.lock().await.clone()?;
        if !endpoint_matches_runtime(&state, &self.runtime) {
            return None;
        }
        persisted_credentials_from_state(&state)
    }

    pub(super) async fn credentials_for_pending(
        &self,
        pending: &PendingControlRequest,
    ) -> Option<Credentials> {
        let state = self.state.lock().await.clone()?;
        if !pending_request_matches(&state, pending) {
            return None;
        }
        let mut credentials = persisted_credentials_from_state(&state)?;
        if !pending.behavior_events_url.is_empty() {
            credentials.behavior_events_url = pending.behavior_events_url.clone();
        }
        Some(credentials)
    }
}
