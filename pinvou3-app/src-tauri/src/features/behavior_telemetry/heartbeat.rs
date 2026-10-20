use super::*;

pub(super) const HEARTBEAT_INTERVAL: Duration = Duration::from_secs(120);

impl BehaviorTelemetry {
    pub(super) fn wake_heartbeat(&self) {
        if let Some(tx) = &self.heartbeat_tx {
            let _ = tx.try_send(());
        }
    }

    /// Presence must progress even when the behavior outbox is empty or retrying.
    /// Only public owners retain the sender, so dropping them stops this worker.
    pub(super) async fn run_heartbeat_worker(mut rx: mpsc::Receiver<()>, telemetry: Self) {
        let mut interval = tokio::time::interval(HEARTBEAT_INTERVAL);
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            tokio::select! {
                biased;
                signal = rx.recv() => {
                    if signal.is_none() {
                        break;
                    }
                    // An enable signal sends immediately and starts a fresh cadence.
                    interval.reset();
                }
                _ = interval.tick() => {}
            }
            if let Err(error) = telemetry.send_heartbeat().await {
                log::debug!("[pinvou3][behavior] installation heartbeat deferred: {error:#}");
            }
        }
    }

    pub(super) async fn send_heartbeat(&self) -> Result<()> {
        // Serialize with consent, deletion and endpoint/credential changes. A
        // completed stop cannot be followed by a heartbeat from an old snapshot.
        // Each HTTP request is bounded to 12 seconds. Expired credentials can
        // add register + collection/start under this gate (up to 36 seconds of
        // network waits in total, in addition to local persistence work).
        let _control_guard = self.control_gate.lock().await;
        if !self.runtime.enabled || !self.state_healthy.load(Ordering::Acquire) {
            return Ok(());
        }
        let Some(lease) = self.current_lease() else {
            return Ok(());
        };
        let snapshot = self.state.lock().await.clone();
        let Some(state) = snapshot else {
            return Ok(());
        };
        if !endpoint_matches_runtime(&state, &self.runtime)
            || !state.control.enabled
            || state.control.user_enabled == Some(false)
            || state.control.collection_blocked
            || state.has_pending_control()
            || state.control.generation != lease.generation
            || state.control.collection_period_id.as_deref()
                != Some(lease.collection_period_id.as_str())
        {
            return Ok(());
        }
        // Heartbeats never enroll a device or implicitly start collection.
        let Some(credentials) = persisted_credentials_from_state(&state) else {
            return Ok(());
        };
        let response = self
            .client
            .post(format!(
                "{}/v1/heartbeat",
                state.telemetry_base_url.trim_end_matches('/')
            ))
            .timeout(Duration::from_secs(12))
            .bearer_auth(&credentials.device_token)
            .json(&json!({
                "device_id": credentials.device_id,
                "app_version": self.runtime.app_version,
                "platform": self.runtime.platform,
                "arch": self.runtime.arch,
                "state": "online",
            }))
            .send()
            .await
            .context("send installation heartbeat")?;
        if response.status() == reqwest::StatusCode::GONE {
            match parse_control_gone(response, "installation heartbeat").await? {
                GoneReason::DeviceCredentialsExpired => {
                    self.recover_expired_credentials_locked(&credentials)
                        .await?;
                }
                GoneReason::DeviceDataDeleted => {
                    self.handle_authoritative_deletion_locked(&credentials)
                        .await?;
                }
            }
        } else if !response.status().is_success() {
            anyhow::bail!("installation heartbeat returned {}", response.status());
        }
        Ok(())
    }
}
