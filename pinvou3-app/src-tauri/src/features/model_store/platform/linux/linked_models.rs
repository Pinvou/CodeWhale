//! Reconcile existing Pinvou model routes after a store engine reports its actual endpoint.
//! Only route identity and card provenance are changed; credentials and user options stay intact.
use super::{db, lifecycle::endpoint_url_is_loopback, version_store};
use crate::platform::prefs::{SavedModel, UserPrefs};

struct Route {
    card_id: String,
    base_url: String,
    model: String,
    ready: bool,
    previous: Vec<(String, String)>,
}

fn chat_base(url: &str) -> Option<String> {
    if !endpoint_url_is_loopback(url) {
        return None;
    }
    let mut url = reqwest::Url::parse(url).ok()?;
    if !["http", "https"].contains(&url.scheme())
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return None;
    }
    match url.path().trim_end_matches('/') {
        "/v1" | "/v1/chat/completions" => url.set_path("/v1"),
        _ => return None,
    }
    Some(url.to_string())
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
                ready: rec.running && rec.status == "deployed" && !rec.card_update_pending,
                previous,
            })
        })
        .collect())
}

pub(super) fn sync(card_id: Option<&str>) -> Result<Vec<String>, String> {
    let prefs = UserPrefs::load();
    if prefs.advanced.saved_models.is_empty() {
        return Ok(Vec::new());
    }
    let routes = routes()?;
    // Most library reads require no write. Recompute under the prefs transaction so
    // concurrent model edits, selection changes and keyring writes are preserved.
    if reconcile(&mut prefs.advanced.saved_models.clone(), &routes, card_id).is_empty() {
        return Ok(Vec::new());
    }
    let mut changed = Vec::new();
    UserPrefs::update_transaction(|prefs| {
        // Read again under the settings lock: a concurrent enable may have
        // committed a newer endpoint while this pass waited for that lock.
        changed = reconcile(&mut prefs.advanced.saved_models, &self::routes()?, card_id);
        Ok(())
    })?;
    Ok(changed)
}

#[cfg(test)]
mod tests;
