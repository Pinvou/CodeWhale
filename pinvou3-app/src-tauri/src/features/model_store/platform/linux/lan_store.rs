//! Linux evidence adapter for LAN sharing; no deployment or firewall mutations.
use super::lan_network::{interfaces, isolation, normalize_upstream};
use super::{
    db,
    lan_gateway::{Interface, Route, Status, Store},
    service::ModelStoreService,
};
use std::{sync::Arc, time::Duration};

pub struct LocalStore;
pub fn local_store() -> Arc<dyn Store> {
    Arc::new(LocalStore)
}

#[async_trait::async_trait]
impl Store for LocalStore {
    fn interfaces(&self) -> Result<Vec<Interface>, String> {
        interfaces()
    }
    async fn key_card(&self, key: &str) -> Result<Option<String>, String> {
        let key = key.to_string();
        tokio::task::spawn_blocking(move || {
            db::find_api_key(&key)
                .map(|record| record.and_then(|r| r.card_id))
                .map_err(|_| "lan_key_store".into())
        })
        .await
        .map_err(|_| "lan_unavailable")?
    }
    async fn route(&self, card: &str) -> Result<Route, String> {
        let card = card.to_string();
        tokio::task::spawn_blocking(move || {
            let record = db::get_store_model(&card)
                .map_err(|_| "lan_unavailable")?
                .ok_or("lan_model_unavailable")?;
            if !record.running || record.status != "deployed" {
                return Err("lan_model_unavailable".into());
            }
            let upstream =
                normalize_upstream(record.endpoint_url.as_deref().ok_or("lan_unsupported")?)?;
            let upstream_model = record
                .endpoint_model
                .filter(|m| !m.trim().is_empty())
                .ok_or("lan_unsupported")?;
            let port = reqwest::Url::parse(&upstream)
                .map_err(|_| "lan_unsupported")?
                .port_or_known_default()
                .ok_or("lan_unsupported")?;
            isolation(port, record.container_name.as_deref(), |container| {
                let mut command = std::process::Command::new("docker");
                command.args(["inspect", "--type", "container", container]);
                let output =
                    crate::platform::process::output_with_timeout(command, Duration::from_secs(3))
                        .map_err(|_| "lan_isolation_unknown")?;
                if !output.status.success() {
                    return Err("lan_isolation_unknown".into());
                }
                serde_json::from_slice(&output.stdout).map_err(|_| "lan_isolation_unknown".into())
            })?;
            Ok(Route {
                card_id: card,
                upstream,
                upstream_model,
            })
        })
        .await
        .map_err(|_| "lan_unavailable")?
    }
}

pub async fn lan_status(
    service: &Arc<ModelStoreService>,
    card: String,
) -> Result<serde_json::Value, String> {
    serde_json::to_value(service.lan.status(&local_store(), &card).await?)
        .map_err(|_| "lan_unavailable".into())
}
pub async fn set_lan_sharing(
    service: &Arc<ModelStoreService>,
    card: String,
    enabled: bool,
    address: Option<String>,
    port: u16,
) -> Result<serde_json::Value, String> {
    let status: Status = service
        .lan
        .set(local_store(), card, enabled, address, port)
        .await?;
    serde_json::to_value(status).map_err(|_| "lan_unavailable".into())
}
pub fn revoke_api_key(id: String) -> Result<serde_json::Value, String> {
    db::delete_api_key(&id).map_err(|_| "lan_key_store")?;
    Ok(serde_json::json!({"ok":true}))
}
