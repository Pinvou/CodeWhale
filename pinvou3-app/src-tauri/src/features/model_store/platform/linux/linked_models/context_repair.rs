//! One-time repair for the store release that supplied 32K instead of 256K.
//! Successful deployment + a fresh exact-wire probe are required before raising a cap.
use super::{Route, chat_base, db};
use crate::platform::{
    credential_store::{CredentialStore, SystemCredentialStore},
    prefs::{MODEL_API_KEY_ENV_OVERRIDE, SavedModel, UserPrefs},
};
use std::{
    io::Read,
    sync::atomic::{AtomicUsize, Ordering},
    time::{Duration, Instant},
};

const LEGACY_WINDOW: u32 = 32768;
const VERIFIED_WINDOW: u32 = 262144;
const RESPONSE_LIMIT: u64 = 256 * 1024;
static NEXT_PROBE: AtomicUsize = AtomicUsize::new(0);

pub(super) fn deployment_stamp(rec: &db::StoreModelRecord) -> Option<String> {
    let supported = match rec.card_id.as_str() {
        "qwen38-27b-nvfp4-mtp" => rec
            .active_profile_id
            .as_deref()
            .is_none_or(|profile| profile == "production-sampling"),
        "qwen38-27b-sglang0519"
        | "qwen38-27b-nvfp4-tachyon"
        | "ornith15-35b-a3b-nvfp4"
        | "ornith-15-9b-nvfp4" => rec.active_profile_id.is_none(),
        _ => false,
    };
    if !supported
        || rec
            .deployed_script_sha256
            .as_ref()
            .zip(rec.scripts_content.get("deploy"))
            .is_some_and(|(deployed, current)| deployed != &current.sha256)
    {
        return None;
    }
    Some(
        serde_json::json!({
            "card": rec.card_id, "version": rec.active_version_id,
            "profile": rec.active_profile_id, "deployed": rec.deployed_at,
            "script": rec.deployed_script_sha256,
            "current_script": rec.scripts_content.get("deploy").map(|s| &s.sha256),
            "runtime": rec.runtime_card_id, "container": rec.container_name,
            "url": rec.endpoint_url, "model": rec.endpoint_model,
        })
        .to_string(),
    )
}

fn marker(route: &Route) -> String {
    format!("verified-256k-v1/{}", route.card_id)
}

fn pending_route<'a>(
    prefs: &UserPrefs,
    row: &SavedModel,
    routes: &'a [Route],
    card_id: Option<&str>,
) -> Option<&'a Route> {
    if row.model_store_link_detached
        || row.is_builtin_account_platform()
        || !row.is_operator_owned_endpoint()
        || prefs.has_explicit_model_context_selection(&row.id)
    {
        return None;
    }
    let card = row.model_store_card_id.as_deref()?;
    let mut matches = routes.iter().filter(|route| route.card_id == card);
    let route = matches.next()?;
    if matches.next().is_some()
        || !route.ready
        || route.context_repair_stamp.is_none()
        || card_id.is_some_and(|filter| filter != card)
        || row.model != route.model
        || chat_base(&row.base_url).as_deref() != Some(&route.base_url)
        || prefs.advanced.model_store_context_repairs.get(&row.id) == Some(&marker(route))
    {
        return None;
    }
    Some(route)
}

pub(super) struct Proof {
    model_id: String,
    stamp: String,
}

fn bearer(row: &SavedModel) -> Option<Option<String>> {
    if let Ok(key) = std::env::var(MODEL_API_KEY_ENV_OVERRIDE) {
        if !key.trim().is_empty() {
            return Some(Some(key.trim().to_string()));
        }
    }
    if let Some(reference) = row.credential_ref.as_ref() {
        // An unavailable credential must not cause the migration to be recorded
        // as complete. A later library refresh can retry after recovery.
        return SystemCredentialStore::new()
            .get(reference)
            .ok()?
            .filter(|key| !key.trim().is_empty())
            .map(Some);
    }
    Some((!row.api_key.trim().is_empty()).then(|| row.api_key.trim().to_string()))
}

fn reports_verified_window(
    client: &reqwest::blocking::Client,
    row: &SavedModel,
    timeout: Duration,
) -> bool {
    let Some(key) = bearer(row) else { return false };
    let Some(base_url) = chat_base(&row.base_url) else {
        return false;
    };
    let mut request = client.get(format!("{base_url}/models")).timeout(timeout);
    if let Some(key) = key {
        request = request.bearer_auth(key);
    }
    let Ok(response) = request.send() else {
        return false;
    };
    if !response.status().is_success() {
        return false;
    }
    let mut bytes = Vec::new();
    if response
        .take(RESPONSE_LIMIT + 1)
        .read_to_end(&mut bytes)
        .is_err()
        || bytes.len() as u64 > RESPONSE_LIMIT
    {
        return false;
    }
    let Ok(value) = serde_json::from_slice::<serde_json::Value>(&bytes) else {
        return false;
    };
    let Some(models) = value.get("data").and_then(|value| value.as_array()) else {
        return false;
    };
    let mut matches = models
        .iter()
        .filter(|model| model.get("id").and_then(|value| value.as_str()) == Some(&row.model));
    let Some(model) = matches.next() else {
        return false;
    };
    matches.next().is_none()
        && model.get("max_model_len").and_then(|value| value.as_u64())
            == Some(VERIFIED_WINDOW as u64)
}

pub(super) fn probe(prefs: &UserPrefs, routes: &[Route], card_id: Option<&str>) -> Vec<Proof> {
    let mut candidates: Vec<_> = prefs
        .advanced
        .saved_models
        .iter()
        .filter_map(|row| {
            (row.context_window_tokens == Some(LEGACY_WINDOW))
                .then(|| pending_route(prefs, row, routes, card_id))
                .flatten()
                .and_then(|route| {
                    route
                        .context_repair_stamp
                        .as_deref()
                        .map(|stamp| (row, stamp))
                })
        })
        .collect();
    if candidates.is_empty() {
        return Vec::new();
    }
    // Rotate retries so stale/offline rows cannot exhaust the per-pass budget
    // forever before another eligible deployment gets its capability check.
    let offset = NEXT_PROBE.fetch_add(1, Ordering::Relaxed) % candidates.len();
    candidates.rotate_left(offset);
    // Never use a proxy or follow a redirect with a saved credential. This is
    // only a loopback capability check, on a blocking worker, capped per pass.
    let Ok(client) = reqwest::blocking::Client::builder()
        .no_proxy()
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(Duration::from_millis(250))
        .timeout(Duration::from_secs(1))
        .build()
    else {
        return Vec::new();
    };
    let deadline = Instant::now() + Duration::from_secs(3);
    let mut proofs = Vec::new();
    for (row, stamp) in candidates {
        let Some(remaining) = deadline.checked_duration_since(Instant::now()) else {
            break;
        };
        if reports_verified_window(&client, row, remaining.min(Duration::from_secs(1))) {
            proofs.push(Proof {
                model_id: row.id.clone(),
                stamp: stamp.to_string(),
            });
        }
    }
    proofs
}

pub(super) fn apply(
    prefs: &mut UserPrefs,
    routes: &[Route],
    proofs: &[Proof],
    card_id: Option<&str>,
) -> Vec<String> {
    let mut changed = Vec::new();
    for index in 0..prefs.advanced.saved_models.len() {
        let row = &prefs.advanced.saved_models[index];
        let Some(route) = pending_route(prefs, row, routes, card_id) else {
            continue;
        };
        let receipt = marker(route);
        let id = row.id.clone();
        if row.context_window_tokens == Some(LEGACY_WINDOW) {
            if !proofs.iter().any(|proof| {
                proof.model_id == id && Some(&proof.stamp) == route.context_repair_stamp.as_ref()
            }) {
                continue;
            }
            // Persist the verified correction explicitly so an older background
            // host cannot refill it with its stale automatic-mode probe cache.
            prefs.advanced.saved_models[index].context_window_tokens = Some(VERIFIED_WINDOW);
        }
        // Remember unaffected automatic/custom limits too, so a future manual
        // 32K selection is not mistaken for a pre-upgrade default.
        prefs
            .advanced
            .model_store_context_repairs
            .insert(id.clone(), receipt);
        changed.push(id);
    }
    changed
}

#[cfg(test)]
mod tests;
