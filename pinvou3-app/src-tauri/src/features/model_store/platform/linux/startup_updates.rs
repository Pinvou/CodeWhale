//! One read-only catalogue pass per process. No scripts, engines or weights run.
use super::{
    card_update, config, db,
    store_client::{StoreClient, StoreSource},
};
use serde::Serialize;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

#[derive(Debug, Clone, Serialize, Default)]
pub struct Report {
    pub checked: usize,
    pub skipped: usize,
    pub updates: Vec<Update>,
}

#[derive(Debug, Clone, Serialize)]
pub struct Update {
    pub card_id: String,
    pub card_name: String,
    pub local_revision: String,
    pub revision: String,
}

pub struct StartupUpdates {
    started: AtomicBool,
    result: tokio::sync::watch::Sender<Option<Report>>,
}

impl Default for StartupUpdates {
    fn default() -> Self {
        Self {
            started: AtomicBool::new(false),
            result: tokio::sync::watch::channel(None).0,
        }
    }
}

impl StartupUpdates {
    pub async fn once(
        &self,
        available: bool,
        job: impl FnOnce() -> Report + Send + 'static,
    ) -> Report {
        if !available {
            return Report::default();
        }
        let mut receiver = self.result.subscribe();
        if self
            .started
            .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
            .is_ok()
        {
            // A webview/RPC disappearing must not cancel initialization and start
            // another network pass. Both success and failure are process-local.
            let result = self.result.clone();
            tokio::spawn(async move {
                let report = tokio::task::spawn_blocking(job)
                    .await
                    .unwrap_or_else(|_| Report {
                        skipped: 1,
                        ..Default::default()
                    });
                result.send_replace(Some(report));
            });
        }
        receiver
            .wait_for(|value| value.is_some())
            .await
            .ok()
            .and_then(|value| value.as_ref().cloned())
            .unwrap_or_default()
    }
}

pub fn check() -> Report {
    if !super::available() {
        return Report::default();
    }
    log::info!("[model-store] startup_update_check:start");
    let store = StoreClient::for_startup(&config::load_settings().store_url);
    let Ok(ids) = db::startup_card_ids() else {
        return Report {
            skipped: 1,
            ..Default::default()
        };
    };
    let report = check_cards(&store, &ids, Duration::from_secs(30));
    log::info!(
        "[model-store] startup_update_check:done checked={} skipped={} updates={}",
        report.checked,
        report.skipped,
        report.updates.len()
    );
    report
}

fn check_cards(store: &dyn StoreSource, ids: &[String], budget: Duration) -> Report {
    let mut report = Report::default();
    let started = Instant::now();
    for (index, id) in ids.iter().enumerate() {
        if started.elapsed() >= budget {
            report.skipped += ids.len() - index;
            break;
        }
        let Ok(Some(local)) = db::startup_card_manifest(id) else {
            report.skipped += 1;
            continue;
        };
        let Ok(card) = store.get_card(id) else {
            report.skipped += 1;
            continue;
        };
        if card.summary.id != *id
            || card_update::validate_inventory(
                &card.scripts,
                card.script_sha256.as_ref().unwrap_or(&Default::default()),
                &card.assets,
                card.asset_sha256.as_ref().unwrap_or(&Default::default()),
            )
            .is_err()
        {
            report.skipped += 1;
            continue;
        }
        // A concurrent manual upgrade/removal invalidates the captured baseline.
        if db::startup_card_manifest(id).ok().flatten().as_ref() != Some(&local) {
            report.skipped += 1;
            continue;
        }
        report.checked += 1;
        let remote = card_update::remote_manifest(&card);
        if remote != local {
            report.updates.push(Update {
                card_id: id.clone(),
                card_name: local["name"].as_str().unwrap_or(id).into(),
                local_revision: card_update::revision(&local),
                revision: card_update::revision(&remote),
            });
        }
    }
    report
}

#[cfg(test)]
mod tests;
