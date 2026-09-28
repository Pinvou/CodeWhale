//! Sidecar persistence for per-session auxiliary state.
//!
//! The durable `SavedSession` cannot grow new fields without changing the
//! upstream schema, so four independent JSON sidecars under
//! `~/.pinvou3/sessions/` capture cross-restart runtime state that must
//! survive a process bounce:
//!
//! - `_session_models.json` — session_id -> SavedModel.id override.
//! - `_pinned_sessions.json` — pinned conversation id list with timestamps.
//! - `_hidden_sessions.json` — collapsed conversation id list with timestamps.
//!
//! Mode / plan-phase remain in-memory only by design.

use std::collections::HashMap;
use std::io::ErrorKind;

use super::SessionStore;
use crate::platform::prefs::ModelSelectionRef;
use anyhow::{Context, Result};
use chrono::Utc;

const SESSION_MODELS_FILE: &str = "_session_models.json";
const PINNED_SESSIONS_FILE: &str = "_pinned_sessions.json";
const HIDDEN_SESSIONS_FILE: &str = "_hidden_sessions.json";

/// Shared save core for the pinned / hidden sidecars: both files carry the
/// same shape (an array of `{ id, <ts_key> }` objects sorted by id) and the
/// same "empty map deletes the file" semantics. Failures are silently ignored
/// (best-effort persistence, matching the historical per-file implementations).
fn save_timestamped_id_map(map: &HashMap<String, String>, file_name: &str, ts_key: &str) {
    let file = crate::platform::paths::sessions_root().join(file_name);
    if map.is_empty() {
        let _ = std::fs::remove_file(&file);
        return;
    }
    let mut out: Vec<_> = map
        .iter()
        .map(|(id, timestamp)| {
            serde_json::json!({
                "id": id,
                (ts_key): timestamp,
            })
        })
        .collect();
    out.sort_by(|a, b| {
        a.get("id")
            .and_then(|v| v.as_str())
            .cmp(&b.get("id").and_then(|v| v.as_str()))
    });
    if let Ok(json) = serde_json::to_string_pretty(&out) {
        let _ = deepseek_tui::utils::write_atomic(&file, json.as_bytes());
    }
}

/// Shared load core for the pinned / hidden sidecars. `None` = nothing to load
/// (missing / unreadable file or invalid shape, the latter logged with
/// `label` so the historical per-file diagnostics stay unchanged). Bare string
/// entries are re-stamped with the current time, matching the legacy format
/// that stored a plain id list.
fn load_timestamped_id_map(
    file_name: &str,
    ts_key: &str,
    label: &str,
) -> Option<HashMap<String, String>> {
    let file = crate::platform::paths::sessions_root().join(file_name);
    if !file.exists() {
        return None;
    }
    let content = match std::fs::read_to_string(&file) {
        Ok(c) => c,
        Err(_) => return None,
    };
    match serde_json::from_str::<serde_json::Value>(&content) {
        Ok(serde_json::Value::Array(items)) => {
            let mut parsed = HashMap::new();
            for item in items {
                match item {
                    serde_json::Value::String(id) => {
                        parsed.insert(id, Utc::now().to_rfc3339());
                    }
                    serde_json::Value::Object(mut obj) => {
                        let id = obj
                            .remove("id")
                            .and_then(|v| v.as_str().map(str::to_string));
                        let timestamp = obj
                            .remove(ts_key)
                            .and_then(|v| v.as_str().map(str::to_string))
                            .unwrap_or_else(|| Utc::now().to_rfc3339());
                        if let Some(id) = id {
                            parsed.insert(id, timestamp);
                        }
                    }
                    _ => {}
                }
            }
            Some(parsed)
        }
        Ok(_) => {
            eprintln!("[sessions] {label} failed: invalid shape");
            None
        }
        Err(e) => {
            eprintln!("[sessions] {label} failed: {e}");
            None
        }
    }
}

impl SessionStore {
    pub fn session_model_id(&self, id: &str) -> Option<String> {
        self.session_model_ref(id)
            .map(|selection| selection.runtime_saved_model_id().to_string())
    }

    /// 只读取用户在对话输入栏里选择的模型，不包含定时运行创建时的模型回退。
    pub fn session_model_override(&self, id: &str) -> Option<String> {
        self.session_model_ref_override(id)
            .map(|selection| selection.runtime_saved_model_id().to_string())
    }

    /// Source-aware selection shown by the composer. Scheduled sessions fall
    /// back to the immutable local model captured by their run profile.
    pub fn session_model_ref(&self, id: &str) -> Option<ModelSelectionRef> {
        self.session_model_ref_override(id).or_else(|| {
            self.scheduled_profile(id)
                .and_then(|profile| profile.model_id)
                .map(ModelSelectionRef::local)
        })
    }

    pub fn session_model_ref_override(&self, id: &str) -> Option<ModelSelectionRef> {
        self.session_models.read().get(id).cloned()
    }

    /// 设/清该 session 的模型 id 并落盘。`None` = 清除(回退全局默认)。
    pub fn set_session_model_id(&self, id: &str, model_id: Option<String>) -> Result<()> {
        self.set_session_model_ref(id, model_id.map(ModelSelectionRef::local))
    }

    /// Set/clear a typed session selection transactionally. `None` restores
    /// the global default for ordinary sessions.
    pub fn set_session_model_ref(
        &self,
        id: &str,
        model_ref: Option<ModelSelectionRef>,
    ) -> Result<()> {
        let mut models = self.session_models.write();
        let previous = models.get(id).cloned();
        match model_ref {
            Some(selection) => {
                models.insert(id.to_string(), selection);
            }
            None => {
                models.remove(id);
            }
        }
        if let Err(error) = Self::persist_session_models(&models) {
            match previous {
                Some(previous) => {
                    models.insert(id.to_string(), previous);
                }
                None => {
                    models.remove(id);
                }
            }
            return Err(error);
        }
        Ok(())
    }

    /// 持久化 per-session 模型绑定到 `~/.pinvou3/sessions/_session_models.json`。
    pub(crate) fn persist_session_models(
        models: &HashMap<String, ModelSelectionRef>,
    ) -> Result<()> {
        let file = crate::platform::paths::sessions_root().join(SESSION_MODELS_FILE);
        if models.is_empty() {
            return match std::fs::remove_file(&file) {
                Ok(()) => Ok(()),
                Err(error) if error.kind() == ErrorKind::NotFound => Ok(()),
                Err(error) => Err(error).with_context(|| format!("remove {}", file.display())),
            };
        }
        let payload =
            serde_json::to_vec_pretty(models).context("serialize per-session model bindings")?;
        deepseek_tui::utils::write_atomic(&file, &payload)
            .with_context(|| format!("persist per-session model bindings to {}", file.display()))
    }

    /// 尽力持久化由清理流程批量修改的模型绑定；交互式设置使用
    /// [`Self::set_session_model_id`] 的可失败事务路径。
    pub fn save_session_models(&self) {
        if let Err(error) = Self::persist_session_models(&self.session_models.read()) {
            eprintln!("[sessions] save_session_models failed: {error:#}");
        }
    }

    /// 启动时从磁盘恢复 per-session 模型绑定。
    pub fn load_session_models(&self) {
        let file = crate::platform::paths::sessions_root().join(SESSION_MODELS_FILE);
        if !file.exists() {
            return;
        }
        let content = match std::fs::read_to_string(&file) {
            Ok(c) => c,
            Err(_) => return,
        };
        match serde_json::from_str::<HashMap<String, ModelSelectionRef>>(&content) {
            Ok(mut map) => {
                let legacy_wire_format = serde_json::from_str::<serde_json::Value>(&content)
                    .ok()
                    .and_then(|value| value.as_object().cloned())
                    .is_some_and(|values| values.values().any(serde_json::Value::is_string));
                let mut dropped_ambiguous_platform_ref = false;
                for (session_id, selection) in &mut map {
                    if let ModelSelectionRef::AccountPlatform { model_id } = selection {
                        if model_id.trim().is_empty() {
                            if let Ok(session) = self.load(session_id) {
                                *model_id = session.metadata.model.trim().to_string();
                            }
                            if model_id.is_empty() {
                                dropped_ambiguous_platform_ref = true;
                            }
                        }
                    }
                }
                map.retain(|_, selection| {
                    !matches!(selection, ModelSelectionRef::AccountPlatform { model_id } if model_id.is_empty())
                });
                *self.session_models.write() = map;
                if legacy_wire_format || dropped_ambiguous_platform_ref {
                    self.save_session_models();
                }
            }
            Err(e) => eprintln!("[sessions] load_session_models failed: {e}"),
        }
    }

    // ===================== 历史对话置顶 =====================

    pub fn is_pinned(&self, id: &str) -> bool {
        self.pinned_sessions.read().contains_key(id)
    }

    pub fn pinned_at(&self, id: &str) -> Option<String> {
        self.pinned_sessions.read().get(id).cloned()
    }

    pub fn set_pinned(&self, id: &str, pinned: bool) {
        {
            let mut pins = self.pinned_sessions.write();
            if pinned {
                pins.insert(id.to_string(), Utc::now().to_rfc3339());
            } else {
                pins.remove(id);
            }
        }
        self.save_pinned_sessions();
    }

    pub fn save_pinned_sessions(&self) {
        let pins = self.pinned_sessions.read();
        save_timestamped_id_map(&pins, PINNED_SESSIONS_FILE, "pinned_at");
    }

    pub fn load_pinned_sessions(&self) {
        if let Some(pins) =
            load_timestamped_id_map(PINNED_SESSIONS_FILE, "pinned_at", "load_pinned_sessions")
        {
            *self.pinned_sessions.write() = pins;
        }
    }

    pub fn is_hidden(&self, id: &str) -> bool {
        self.hidden_sessions.read().contains_key(id)
    }

    pub fn hidden_at(&self, id: &str) -> Option<String> {
        self.hidden_sessions.read().get(id).cloned()
    }

    pub fn set_hidden(&self, id: &str, hidden: bool) {
        {
            let mut hidden_sessions = self.hidden_sessions.write();
            if hidden {
                hidden_sessions.insert(id.to_string(), Utc::now().to_rfc3339());
            } else {
                hidden_sessions.remove(id);
            }
        }
        if hidden {
            self.set_pinned(id, false);
        }
        self.save_hidden_sessions();
    }

    pub fn save_hidden_sessions(&self) {
        let hidden_sessions = self.hidden_sessions.read();
        save_timestamped_id_map(&hidden_sessions, HIDDEN_SESSIONS_FILE, "hidden_at");
    }

    pub fn load_hidden_sessions(&self) {
        if let Some(hidden_sessions) =
            load_timestamped_id_map(HIDDEN_SESSIONS_FILE, "hidden_at", "load_hidden_sessions")
        {
            *self.hidden_sessions.write() = hidden_sessions;
        }
    }
}
