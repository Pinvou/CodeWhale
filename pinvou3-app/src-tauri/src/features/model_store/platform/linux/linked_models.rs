//! Reconcile existing Pinvou model routes after a store engine reports its actual endpoint.
//! Preserve user options except the explicitly versioned correction of erroneous store defaults.
use super::{db, version_store};
use crate::features::model_store::linked_model_route::chat_base;
use crate::platform::prefs::{ModelPreset, SavedModel, UserPrefs};

mod context_repair;

struct Route {
    card_id: String,
    base_url: String,
    model: String,
    ready: bool,
    context_repair_stamp: Option<String>,
    previous: Vec<(String, String)>,
}

fn identity(record: &db::StoreModelRecord) -> Option<(String, String)> {
    let base = chat_base(record.endpoint_url.as_deref()?)?;
    let model = record
        .endpoint_model
        .as_deref()
        .filter(|m| !m.is_empty())
        .unwrap_or(&record.model_name);
    (!model.is_empty()).then(|| (base, model.to_string()))
}

fn is_legacy_store_chat_route(row: &SavedModel) -> bool {
    // Every pre-provenance buildSavedModel emitted this metadata. Matching an
    // address and model alone cannot establish ownership of a user-configured
    // Responses/Messages route or another explicitly selected endpoint mode.
    row.preset == ModelPreset::OpenaiCompatible
        && row
            .provider_kind
            .as_deref()
            .is_none_or(|kind| kind == "custom")
        && row
            .vendor
            .as_deref()
            .is_none_or(|vendor| vendor.trim().is_empty())
        && row
            .endpoint_mode
            .as_deref()
            .is_none_or(|mode| mode.trim().is_empty())
        && crate::core::model_wire::model_route_wire_api(
            row.preset.as_str(),
            row.vendor.as_deref(),
            &row.model,
            &row.base_url,
        ) == "openai_chat_completions"
}

pub(crate) fn matches_current_model_route(card_id: &str, base_url: &str, model: &str) -> bool {
    let Ok(Some(rec)) = db::get_store_model(card_id) else {
        return false;
    };
    if !rec.running || rec.status != "deployed" || rec.card_update_pending {
        return false;
    }
    let Ok(version) = version_store::get(card_id, &version_store::active_id(&rec)) else {
        return false;
    };
    if version.is_some_and(|v| v.state == "deleting") {
        return false;
    }
    identity(&rec)
        .is_some_and(|(base, wire)| chat_base(base_url).as_deref() == Some(&base) && model == wire)
}

fn reconcile(models: &mut [SavedModel], routes: &[Route], card_id: Option<&str>) -> Vec<String> {
    let mut changed = Vec::new();
    for row in models {
        if row.model_store_link_detached
            || row.is_builtin_llmapi()
            || !row.is_operator_owned_endpoint()
        {
            continue;
        }
        let Some(base) = chat_base(&row.base_url) else {
            continue;
        };
        let route = if let Some(card) = row.model_store_card_id.as_deref() {
            routes.iter().find(|route| route.card_id == card)
        } else {
            if !is_legacy_store_chat_route(row) {
                continue;
            }
            // Migrate only an exact, unambiguous legacy route, including a version
            // archived before the wire ID changed. Same-name remote/other-port rows stay put.
            let mut matches = routes.iter().filter(|route| {
                route
                    .previous
                    .iter()
                    .any(|(url, model)| url == &base && model == &row.model)
            });
            let first = matches.next();
            if matches.next().is_some() {
                continue;
            }
            first
        };
        let Some(route) = route else { continue };
        if card_id.is_some_and(|card| card != route.card_id) {
            continue;
        }
        if !route.ready {
            if row.model_store_card_id.is_none() {
                row.model_store_card_id = Some(route.card_id.clone());
                changed.push(row.id.clone());
            }
            continue;
        }
        if row.base_url == route.base_url
            && row.model == route.model
            && row.model_store_card_id.as_deref() == Some(&route.card_id)
        {
            continue;
        }
        row.base_url.clone_from(&route.base_url);
        row.model.clone_from(&route.model);
        row.model_store_card_id = Some(route.card_id.clone());
        changed.push(row.id.clone());
    }
    changed
}

fn routes() -> Result<Vec<Route>, String> {
    let records = db::list_store_models().map_err(|e| e.to_string())?;
    if records.is_empty() {
        return Ok(Vec::new());
    }
    let versions = version_store::list(None)?;
    Ok(records
        .iter()
        .filter_map(|rec| {
            let (base_url, model) = identity(rec)?;
            let mut previous = vec![(base_url.clone(), model.clone())];
            previous.extend(
                versions
                    .iter()
                    .filter(|version| version.card_id == rec.card_id && version.state == "history")
                    .filter_map(|version| identity(version.record.as_ref()?)),
            );
            Some(Route {
                card_id: rec.card_id.clone(),
                base_url,
                model,
                ready: rec.running
                    && rec.status == "deployed"
                    && !rec.card_update_pending
                    && !versions.iter().any(|version| {
                        version.card_id == rec.card_id
                            && version.version_id == version_store::active_id(rec)
                            && version.state == "deleting"
                    }),
                context_repair_stamp: context_repair::deployment_stamp(rec),
                previous,
            })
        })
        .collect())
}

pub(super) fn sync(card_id: Option<&str>) -> Result<Vec<String>, String> {
    sync_inner(card_id, true)
}

/// Bind legacy routes before enable, but do not repair limits until enable succeeds.
pub(super) fn sync_routes(card_id: Option<&str>) -> Result<Vec<String>, String> {
    sync_inner(card_id, false)
}

fn sync_inner(card_id: Option<&str>, repair_context: bool) -> Result<Vec<String>, String> {
    // Automatic route repair must neither migrate unrelated credentials nor
    // mistake unreadable settings for an empty/default model list.
    let mut preview = UserPrefs::load_read_only_model_snapshot().map_err(str::to_owned)?;
    if preview.advanced.saved_models.is_empty() {
        return Ok(Vec::new());
    }
    let routes = routes()?;
    let mut changed = reconcile(&mut preview.advanced.saved_models, &routes, card_id);
    // Probe only eligible legacy 32K rows, outside the settings lock and on the
    // caller's blocking worker. Ordinary reads after repair perform no request.
    let proofs = if repair_context {
        context_repair::probe(&preview, &routes, card_id)
    } else {
        Vec::new()
    };
    if repair_context {
        changed.extend(context_repair::apply(
            &mut preview,
            &routes,
            &proofs,
            card_id,
        ));
    }
    if changed.is_empty() {
        return Ok(Vec::new());
    }
    // The probe ran without settings locks. Strictly re-read under both locks
    // so a concurrent truncation/removal cannot become a default-file save.
    UserPrefs::update_readable_model_transaction_with_post_commit(
        |prefs| {
            // A concurrent enable may have committed a different deployment while
            // this pass probed or waited for the settings lock. Revalidate its stamp.
            let routes = self::routes()?;
            changed = reconcile(&mut prefs.advanced.saved_models, &routes, card_id);
            if repair_context {
                changed.extend(context_repair::apply(prefs, &routes, &proofs, card_id));
            }
            changed.sort();
            changed.dedup();
            Ok(())
        },
        || (),
    )?;
    Ok(changed)
}

fn is_current_link(row: &SavedModel, card_id: &str) -> bool {
    row.model_store_card_id.as_deref() == Some(card_id)
        && !row.model_store_link_detached
        && !row.is_builtin_llmapi()
        && row.is_operator_owned_endpoint()
        && matches_current_model_route(card_id, &row.base_url, &row.model)
}

/// A deployment can change its context window while keeping the same wire route.
/// Rebuild its consumers after enable even when no saved field needed changing.
pub(super) fn current_linked_ids(card_id: &str) -> Vec<String> {
    UserPrefs::load()
        .advanced
        .saved_models
        .iter()
        .filter(|row| is_current_link(row, card_id))
        .map(|row| row.id.clone())
        .collect()
}

/// Explicitly restore automatic probing, without resaving a stale editor snapshot
/// or touching credentials, manual limits on other rows, or model selection.
pub(super) fn follow_deployment_context(card_id: &str, model_id: &str) -> Result<(), String> {
    UserPrefs::update_readable_model_transaction_with_post_commit(
        |prefs| {
            let row = prefs
                .advanced
                .saved_models
                .iter_mut()
                .find(|row| row.id == model_id && is_current_link(row, card_id))
                .ok_or(
                    "model store endpoint changed or is unavailable; refresh the card and retry",
                )?;
            // None is the existing automatic-mode representation. Probe results must
            // never be persisted here: that would turn a runtime fact into a fixed cap.
            row.context_window_tokens = None;
            Ok(())
        },
        || (),
    )
    .map(|_| ())
}

#[cfg(test)]
mod tests;
