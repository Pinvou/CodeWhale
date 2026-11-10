//! Endpoint startup commits. Lock order is lifecycle -> inner. Reader retirement
//! uses inner only; the exact pending sender fences it without taking lifecycle
//! from a reader callback. Filesystem commits never reenter Inner, and status
//! emission / reader dispatch always happen after releasing its guard.

use super::*;

pub(super) struct ControllerRelayStart {
    pub(super) sender: tokio::sync::mpsc::Sender<ControllerRelayFrame>,
    pub(super) receiver: tokio::sync::mpsc::Receiver<ControllerRelayFrame>,
    pub(super) owner: RelaySender,
    pub(super) binding: u64,
    pub(super) synthetic_registration: Option<Value>,
}

struct EndpointReader {
    owner: RelaySender,
    receiver: relay_client::RelayReceiver,
}

impl RemoteControlManager {
    // A synchronous boundary prevents a non-Send lifecycle guard crossing the
    // caller's synthetic-registration send await.
    pub(super) fn begin_controller_relay(
        &self,
        generation: &str,
    ) -> Result<ControllerRelayStart, String> {
        let _lifecycle = self.lifecycle.lock();
        let (sender, receiver) = tokio::sync::mpsc::channel(128);
        let synthetic_registration = {
            let mut inner = self.inner.lock();
            if inner.controller_inbound.is_some() {
                return Err("desktop_controller_relay_busy".to_owned());
            }
            inner.controller_relay_generation = Some(generation.to_owned());
            inner.controller_inbound = Some(sender.clone());
            inner.endpoint.as_ref().map(|endpoint| {
                json!({
                    "v": 2, "type": "desktop_endpoint_registered",
                    "endpoint_id": endpoint.config.endpoint_id,
                    "web_client_connected": endpoint.web_client_connected,
                    "lease_id": endpoint.lease_id,
                })
            })
        };
        #[cfg(test)]
        {
            let hook = self.controller_start_hook.lock().take();
            if let Some(hook) = hook {
                hook();
            }
        }
        if let Err(error) = self.start_with_owner(false, Some(&sender)) {
            // A reader may have retired this pending lane. Never clear a
            // replacement merely because it happens to use the same generation.
            let mut inner = self.inner.lock();
            if inner.controller_relay_generation.as_deref() == Some(generation)
                && inner
                    .controller_inbound
                    .as_ref()
                    .is_some_and(|active| active.same_channel(&sender))
            {
                if let Some(endpoint) = &inner.endpoint {
                    endpoint.sender.controller_authority.retire(generation);
                }
                inner.controller_relay_generation = None;
                inner.controller_binding = None;
                inner.controller_inbound = None;
            }
            return Err(error);
        }
        let mut inner = self.inner.lock();
        if !inner
            .controller_inbound
            .as_ref()
            .is_some_and(|active| active.same_channel(&sender))
        {
            return Err("desktop_controller_generation_stale".to_owned());
        }
        let Some(owner) = inner
            .endpoint
            .as_ref()
            .map(|endpoint| endpoint.sender.clone())
        else {
            inner.controller_relay_generation = None;
            inner.controller_inbound = None;
            return Err("desktop_controller_authority_unavailable".to_owned());
        };
        let Some(binding) = owner.controller_authority.bind(generation) else {
            inner.controller_relay_generation = None;
            inner.controller_inbound = None;
            return Err("desktop_controller_authority_unavailable".to_owned());
        };
        inner.controller_binding = Some(binding);
        let already_registered = inner
            .endpoint
            .as_ref()
            .is_some_and(|endpoint| endpoint.status != WebAccessStatusKind::ConnectingRelay);
        Ok(ControllerRelayStart {
            sender,
            receiver,
            owner,
            binding,
            synthetic_registration: synthetic_registration.filter(|_| already_registered),
        })
    }

    /// Caller holds lifecycle. The definitive pending-owner check and all
    /// config creation/persistence/endpoint installation share Inner with reader
    /// retirement. Cancellation cannot overtake a check and then revive config.
    pub(super) fn start_with_owner(
        &self,
        allow_host_workspace: bool,
        expected: Option<&tokio::sync::mpsc::Sender<ControllerRelayFrame>>,
    ) -> Result<WebAccessInfo, String> {
        self.ensure_policy()?;
        self.ensure_process_ownership()?;
        self.replay_pending_revocations()?;
        let (info, reader) = {
            let mut inner = self.inner.lock();
            if expected.is_some_and(|expected| {
                !inner
                    .controller_inbound
                    .as_ref()
                    .is_some_and(|active| active.same_channel(expected))
            }) {
                return Err("desktop_controller_generation_stale".to_owned());
            }
            let active_endpoint_id = inner
                .endpoint
                .as_ref()
                .map(|endpoint| endpoint.config.endpoint_id.clone());
            let (mut config, is_new) = match load_config()? {
                Some(config) => {
                    validate_config(&config)?;
                    (config, false)
                }
                None if active_endpoint_id.is_some() => {
                    return Err("Web access endpoint configuration is missing".to_owned());
                }
                None => (fresh_config(false), true),
            };
            if active_endpoint_id
                .as_ref()
                .is_some_and(|id| id != &config.endpoint_id)
            {
                return Err("Web access endpoint configuration changed".to_owned());
            }
            let authorization_changed = allow_host_workspace && !config.allow_host_workspace;
            config.allow_host_workspace |= allow_host_workspace;
            if is_new || authorization_changed {
                persist_config(&config)?;
            }
            if let Some(endpoint) = inner.endpoint.as_mut() {
                endpoint.config.allow_host_workspace = config.allow_host_workspace;
                (pairing_info(endpoint), None)
            } else {
                let reader =
                    Self::install_endpoint(&mut inner, apply_runtime_relay_override(config))?;
                let endpoint = inner
                    .endpoint
                    .as_ref()
                    .ok_or_else(|| "Web access endpoint failed to start".to_owned())?;
                let info = pairing_info(endpoint);
                (info, Some(reader))
            }
        };
        if let Some(reader) = reader {
            self.start_endpoint_reader(reader);
        }
        Ok(info)
    }

    pub(super) fn activate(&self, config: WebAccessConfig) -> Result<(), String> {
        let reader = {
            let mut inner = self.inner.lock();
            Self::install_endpoint(&mut inner, config)?
        };
        self.start_endpoint_reader(reader);
        Ok(())
    }

    fn install_endpoint(
        inner: &mut Inner,
        config: WebAccessConfig,
    ) -> Result<EndpointReader, String> {
        let rpc_ledger = load_or_initialize_rpc_ledger(&config.endpoint_id)?;
        let url = public_url(&config);
        let (sender, receiver) = relay_client::spawn(config.clone());
        let owner = sender.clone();
        inner.idle_error = None;
        inner.idle_status = WebAccessStatusKind::Idle;
        inner.rpc_ledger = rpc_ledger;
        inner.rpc_cache.clear();
        inner.rpc_order.clear();
        // Keep the first pending controller lane; only a replaced socket retires.
        if inner.endpoint.is_some()
            && let Some(previous) = take_retired_endpoint(inner)
        {
            let _ = previous.sender.try_send(RelayOutbound::Shutdown);
        }
        inner.endpoint = Some(ActiveEndpoint {
            config,
            url,
            status: WebAccessStatusKind::ConnectingRelay,
            web_client_connected: false,
            client_ready: false,
            lease_id: None,
            last_lease_id: None,
            fresh_baseline_seq: None,
            last_error: None,
            last_error_code: None,
            sender,
        });
        Ok(EndpointReader { owner, receiver })
    }

    fn start_endpoint_reader(&self, reader: EndpointReader) {
        self.emit_status();
        let manager = self.clone();
        tauri::async_runtime::spawn(async move {
            let EndpointReader {
                owner,
                mut receiver,
            } = reader;
            while let Some(inbound) = receiver.recv().await {
                match inbound {
                    RelayInbound::Message(text) => match serde_json::from_str::<Value>(&text) {
                        Ok(value) => manager.handle_relay_message(value, &owner),
                        Err(_) => crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                            "relay_message_rejected", "rejected",
                        ),
                    },
                    RelayInbound::ControllerMessage { text, receipt } => {
                        if let Ok(value) = serde_json::from_str::<Value>(&text) {
                            manager.handle_relay_message_with_receipt(value, &owner, Some(receipt));
                        }
                    }
                    RelayInbound::Connection { endpoint_id, connected, error } => {
                        manager.handle_connection(&endpoint_id, connected, error, &owner);
                    }
                }
            }
        });
    }
}
