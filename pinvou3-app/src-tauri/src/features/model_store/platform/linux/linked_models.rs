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
    aliases: Vec<String>,
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
            || row.is_builtin_account_platform()
            || !row.is_operator_owned_endpoint()
        {
            continue;
        }
        let Some(base) = chat_base(&row.base_url) else {
            continue;
        };
        let route = if let Some(card) = row.linked_management_card() {
            let mut matches = routes.iter().filter(|route| {
                route.card_id == card || route.aliases.iter().any(|alias| alias == card)
            });
            let first = matches.next();
            if matches.next().is_some() {
                continue;
            }
            first
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
        // A row that already names its store card upgrades that card's route
        // (explicit path); one without model_store_card_id only gets here via
        // the exact legacy-route match below. The cmsd binding carry rule at
        // the bottom applies to the legacy upgrade only.
        let legacy_upgrade = row.model_store_card_id.is_none();
        if card_id.is_some_and(|card| card != route.card_id) {
            continue;
        }
        if !route.ready {
            if row.model_store_card_id.is_none() {
                row.model_store_card_id = Some(route.card_id.clone());
                // An identity-known row carries the cmsd binding along; a pure
                // legacy repair only acquires the store-source marker.
                if row.store_card_id.is_some() || row.provider_kind.is_some() {
                    row.store_card_id = Some(route.card_id.clone());
                }
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
        // Carry rule for the cmsd binding: a legacy upgrade only carries it
        // when the row's protocol identity is known or the binding already
        // exists (old physical aliases included) — a pure legacy metadata
        // repair never mints the marker. An explicit card link that swaps to
        // a new card must let an existing binding follow (r3 评审 P2-4):
        // leaving it on the old card makes normalize_store_link treat the
        // pair as a mismatch at the next prefs load and silently detach the
        // whole store association, defeating the recovery closed loop.
        if row.store_card_id.is_some() || (legacy_upgrade && row.provider_kind.is_some()) {
            row.store_card_id = Some(route.card_id.clone());
        }
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
                aliases: rec
                    .runtime_card_id
                    .iter()
                    .cloned()
                    .chain(
                        versions
                            .iter()
                            .filter(|v| v.card_id == rec.card_id)
                            .map(|v| v.runtime_card_id.clone()),
                    )
                    .collect(),
                previous,
            })
        })
        .collect())
}

pub(super) fn sync(card_id: Option<&str>) -> Result<Vec<String>, String> {
    sync_inner(card_id, true)
}

/// r36 P1-2 裁决落地（显式重启意图放行）：这张卡是否有待执行的历史上下文修复。
/// enable 预检的 rec.running 拒绝与「端点仍存活」拒绝两臂都以它放行（修复语义
/// 恰恰要求在服务存活时再启用完成一次性修复/接管）；普通启用的双实例防护
/// （r19 安全不变量）不变。读不到偏好/路由时按无修复待办处理——fail-closed
/// 仍是默认，放行必须被证实。**不看 route.ready**：enable 本身会把路由带向
/// ready，预检要回答的是「有没有等着的修复」，不是「现在能不能执行修复」。
/// 启动期后台对账入口（r36 P2-1）：run_startup_refresh 删除时连带丢掉的
/// sync_linked_models(None) 在这里恢复——fire-and-forget，失败由调用方记日志。
pub(crate) fn linked_models_startup_sync() -> Result<(), String> {
    sync(None).map(|_| ())
}

pub(super) fn repair_due(card_id: &str) -> bool {
    let Ok(prefs) = UserPrefs::load_read_only_model_snapshot() else {
        return false;
    };
    let Ok(routes) = routes() else {
        return false;
    };
    prefs.advanced.saved_models.iter().any(|row| {
        context_repair::pending_route_gate(&prefs, row, &routes, Some(card_id)).is_some()
    })
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
        && !row.is_builtin_account_platform()
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

/// A successful enable invalidates probes even when the wire route stays equal.
/// This nonce grants no execution authority. Concurrent enables may coalesce;
/// each observed change refreshes all linked consumers in the desktop.
pub(super) fn mark_runtime_refresh(card_id: &str) -> Result<(), String> {
    if current_linked_ids(card_id).is_empty() {
        return Ok(());
    }
    crate::platform::atomic_write_private(
        &super::config::data_dir().join("runtime-refresh.revision"),
        uuid::Uuid::new_v4().to_string().as_bytes(),
    )
    .map_err(|e| format!("model_refresh_persist_failed: {e}"))
}

fn runtime_refresh_revision() -> Result<Option<uuid::Uuid>, String> {
    let bytes = match crate::platform::filesystem::read_private_data_file_bytes_within(
        &super::config::data_dir().join("runtime-refresh.revision"),
        64,
    ) {
        Ok(bytes) => bytes,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(format!("model_refresh_read_failed: {error}")),
    };
    let value = std::str::from_utf8(&bytes).map_err(|_| "model_refresh_invalid")?;
    uuid::Uuid::parse_str(value)
        .map(Some)
        .map_err(|_| "model_refresh_invalid".into())
}

/// Process-local observations are scoped by profile and manager endpoint.
/// They are invalidation hints, never ownership or saved context caps.
#[derive(Default)]
pub(super) struct RuntimeFacts {
    scope: String,
    confirmed:
        std::collections::BTreeMap<String, std::collections::BTreeMap<String, serde_json::Value>>,
}

pub(super) fn runtime_scope() -> String {
    super::script_runner::sha256_hex(
        &serde_json::json!([
            crate::platform::paths::pinvou3_home(),
            crate::features::megacube::socket_path(),
        ])
        .to_string(),
    )
}

impl RuntimeFacts {
    /// Return logical and known physical IDs whose linked consumers still need
    /// a positive current deployment/window observation. This is not authority.
    pub(super) fn pending_cards(payload: &serde_json::Value) -> Vec<String> {
        let cards = UserPrefs::load()
            .advanced
            .saved_models
            .iter()
            .filter(|row| {
                row.model_store_card_id.is_some()
                    && !row.model_store_link_detached
                    && !row.is_builtin_account_platform()
                    && row.is_operator_owned_endpoint()
            })
            .filter_map(|row| row.linked_management_card().map(str::to_owned))
            .collect();
        Self::with_aliases(cards, payload)
    }

    fn with_aliases(
        mut cards: std::collections::BTreeSet<String>,
        payload: &serde_json::Value,
    ) -> Vec<String> {
        if let Some(models) = payload["models"].as_array() {
            let aliases: Vec<_> = models
                .iter()
                .filter(|model| {
                    model["card_id"]
                        .as_str()
                        .is_some_and(|card| cards.contains(card))
                })
                .filter_map(|model| model["runtime_card_id"].as_str())
                .filter(|card| !card.is_empty())
                .map(str::to_owned)
                .collect();
            cards.extend(aliases);
        }
        cards.into_iter().collect()
    }

    pub(super) fn observe(&mut self, payload: &serde_json::Value) -> Vec<String> {
        let scope = runtime_scope();
        // A proxy must not consume another host profile/manager's observations.
        if payload["linked_runtime_scope"].as_str() != Some(scope.as_str()) {
            return Self::pending_cards(payload);
        }
        self.select_scope(scope);
        let Some(models) = payload["models"].as_array() else {
            return Self::pending_cards(payload);
        };
        let prefs = UserPrefs::load();
        let mut pending: std::collections::BTreeSet<String> = prefs
            .advanced
            .saved_models
            .iter()
            .filter(|row| {
                row.model_store_card_id.is_some()
                    && !row.model_store_link_detached
                    && !row.is_builtin_account_platform()
                    && row.is_operator_owned_endpoint()
            })
            .filter_map(|row| row.linked_management_card().map(str::to_owned))
            .collect();
        for row in &prefs.advanced.saved_models {
            let Some(card) = row.linked_management_card() else {
                continue;
            };
            if row.model_store_link_detached
                || row.is_builtin_account_platform()
                || !row.is_operator_owned_endpoint()
            {
                continue;
            }
            let mut matches = models.iter().filter(|model| model["card_id"] == card);
            let Some(model) = matches.next() else {
                continue;
            };
            if matches.next().is_some() {
                continue;
            }
            let cmsd = &model["cmsd"];
            if model["status"] != "deployed"
                || model["running"] != true
                || model["card_update_pending"] == true
                || cmsd["managed"] != true
                || cmsd["status"] != "running"
                || cmsd["healthy"] != true
                || cmsd["degraded"] == true
                || cmsd["modelId"].as_str() != Some(row.model.as_str())
                || model["endpoint_model"].as_str() != Some(row.model.as_str())
                || model["endpoint_url"].as_str().and_then(chat_base) != chat_base(&row.base_url)
                || chat_base(&row.base_url).is_none()
            {
                continue;
            }
            let text = |value: &serde_json::Value| {
                value
                    .as_str()
                    .filter(|value| !value.is_empty() && value.len() <= 256)
                    .map(str::to_owned)
            };
            let window = cmsd["maxModelLen"].as_u64().filter(|window| *window > 0);
            let deployment = text(&cmsd["deploymentRevision"]);
            // A confirmed deployment can change before /models reports a window.
            // Neither missing dimension may erase its last confirmed value.
            if window.is_none() && deployment.is_none() {
                continue;
            }
            let identity = serde_json::json!([card, row.base_url, row.model]);
            let fact = self.confirmed.entry(row.id.clone()).or_default();
            if fact.get("identity") != Some(&identity) {
                fact.clear();
                fact.insert("identity".into(), identity);
            }
            if let Some(deployment) = deployment.as_ref() {
                fact.insert("deployment".into(), deployment.clone().into());
            }
            if let Some(window) = window {
                fact.insert("window".into(), window.into());
                if let Some(deployment) = fact.get("deployment").cloned() {
                    fact.insert("window_deployment".into(), deployment);
                }
            }
            for (key, value) in [
                ("version", &cmsd["versionId"]),
                ("deployed", &model["deployed_at"]),
                ("active_version", &model["active_version_id"]),
                ("runtime_card", &model["runtime_card_id"]),
                ("profile", &model["active_profile_id"]),
                ("script", &model["deployed_script_sha256"]),
            ] {
                if let Some(value) = text(value) {
                    fact.insert(key.into(), value.into());
                }
            }
            // A new deployment can already invalidate engines while window
            // confirmation remains pending. Retain the old window's generation;
            // the same numeric window confirmed for the new generation must
            // still refresh a runtime that could have probed only a fallback.
            if window.is_some()
                || deployment.as_ref().is_some_and(|deployment| {
                    fact.get("window_deployment")
                        .and_then(serde_json::Value::as_str)
                        == Some(deployment.as_str())
                })
            {
                pending.remove(card);
            }
        }
        Self::with_aliases(pending, payload)
    }

    fn select_scope(&mut self, scope: String) {
        if self.scope != scope {
            self.scope = scope;
            self.confirmed.clear();
        }
    }

    pub(super) fn revision(&mut self, ids: &[String]) -> String {
        self.select_scope(runtime_scope());
        // Keep observations bounded by the current linked settings rows.
        self.confirmed.retain(|id, _| ids.contains(id));
        super::script_runner::sha256_hex(
            &serde_json::json!([self.scope, self.confirmed]).to_string(),
        )
    }
}

/// A desktop watches the shared settings snapshot even when the durable host
/// committed a route before this process polled. No credentials enter the revision.
pub(super) fn snapshot() -> Result<(String, Vec<String>), String> {
    let prefs = UserPrefs::load();
    let rows: Vec<_> = prefs
        .advanced
        .saved_models
        .iter()
        .filter(|row| row.linked_management_card().is_some())
        .map(|row| {
            (
                &row.id,
                &row.base_url,
                &row.model,
                row.context_window_tokens,
                row.linked_management_card(),
            )
        })
        .collect();
    let revision = super::script_runner::sha256_hex(
        &serde_json::to_string(&(&rows, runtime_refresh_revision()?)).map_err(|e| e.to_string())?,
    );
    let ids = rows.iter().map(|row| row.0.clone()).collect();
    Ok((revision, ids))
}

#[cfg(test)]
mod tests;
