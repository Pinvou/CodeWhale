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
//! Mode / pinvou_review / plan-phase remain in-memory only by design.

use std::collections::HashMap;
use std::io::ErrorKind;

use super::SessionStore;
use crate::platform::prefs::ModelSelectionRef;
use anyhow::{Context, Result};
use chrono::Utc;

const SESSION_MODELS_FILE: &str = "_session_models.json";
const PINNED_SESSIONS_FILE: &str = "_pinned_sessions.json";
const HIDDEN_SESSIONS_FILE: &str = "_hidden_sessions.json";

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
        let file = crate::platform::paths::sessions_root().join("_session_models.json");
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
        let file = crate::platform::paths::sessions_root().join("_session_models.json");
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
        let file = crate::platform::paths::sessions_root().join("_pinned_sessions.json");
        let pins = self.pinned_sessions.read();
        if pins.is_empty() {
            let _ = std::fs::remove_file(&file);
            return;
        }
        let mut out: Vec<_> = pins
            .iter()
            .map(|(id, pinned_at)| {
                serde_json::json!({
                    "id": id,
                    "pinned_at": pinned_at,
                })
            })
            .collect();
        out.sort_by(|a, b| {
            a.get("id")
                .and_then(|v| v.as_str())
                .cmp(&b.get("id").and_then(|v| v.as_str()))
        });
        if let Ok(json) = serde_json::to_string_pretty(&out) {
            let _ = std::fs::write(file, json);
        }
    }

    pub fn load_pinned_sessions(&self) {
        let file = crate::platform::paths::sessions_root().join("_pinned_sessions.json");
        if !file.exists() {
            return;
        }
        let content = match std::fs::read_to_string(&file) {
            Ok(c) => c,
            Err(_) => return,
        };
        match serde_json::from_str::<serde_json::Value>(&content) {
            Ok(serde_json::Value::Array(items)) => {
                let mut pins = HashMap::new();
                for item in items {
                    match item {
                        serde_json::Value::String(id) => {
                            pins.insert(id, Utc::now().to_rfc3339());
                        }
                        serde_json::Value::Object(mut obj) => {
                            let id = obj
                                .remove("id")
                                .and_then(|v| v.as_str().map(str::to_string));
                            let pinned_at = obj
                                .remove("pinned_at")
                                .and_then(|v| v.as_str().map(str::to_string))
                                .unwrap_or_else(|| Utc::now().to_rfc3339());
                            if let Some(id) = id {
                                pins.insert(id, pinned_at);
                            }
                        }
                        _ => {}
                    }
                }
                *self.pinned_sessions.write() = pins;
            }
            Ok(_) => eprintln!("[sessions] load_pinned_sessions failed: invalid shape"),
            Err(e) => eprintln!("[sessions] load_pinned_sessions failed: {e}"),
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
        let file = crate::platform::paths::sessions_root().join("_hidden_sessions.json");
        let hidden_sessions = self.hidden_sessions.read();
        if hidden_sessions.is_empty() {
            let _ = std::fs::remove_file(&file);
            return;
        }
        let mut out: Vec<_> = hidden_sessions
            .iter()
            .map(|(id, hidden_at)| {
                serde_json::json!({
                    "id": id,
                    "hidden_at": hidden_at,
                })
            })
            .collect();
        out.sort_by(|a, b| {
            a.get("id")
                .and_then(|v| v.as_str())
                .cmp(&b.get("id").and_then(|v| v.as_str()))
        });
        if let Ok(json) = serde_json::to_string_pretty(&out) {
            let _ = std::fs::write(file, json);
        }
    }

    pub fn load_hidden_sessions(&self) {
        let file = crate::platform::paths::sessions_root().join("_hidden_sessions.json");
        if !file.exists() {
            return;
        }
        let content = match std::fs::read_to_string(&file) {
            Ok(c) => c,
            Err(_) => return,
        };
        match serde_json::from_str::<serde_json::Value>(&content) {
            Ok(serde_json::Value::Array(items)) => {
                let mut hidden_sessions = HashMap::new();
                for item in items {
                    match item {
                        serde_json::Value::String(id) => {
                            hidden_sessions.insert(id, Utc::now().to_rfc3339());
                        }
                        serde_json::Value::Object(mut obj) => {
                            let id = obj
                                .remove("id")
                                .and_then(|v| v.as_str().map(str::to_string));
                            let hidden_at = obj
                                .remove("hidden_at")
                                .and_then(|v| v.as_str().map(str::to_string))
                                .unwrap_or_else(|| Utc::now().to_rfc3339());
                            if let Some(id) = id {
                                hidden_sessions.insert(id, hidden_at);
                            }
                        }
                        _ => {}
                    }
                }
                *self.hidden_sessions.write() = hidden_sessions;
            }
            Ok(_) => eprintln!("[sessions] load_hidden_sessions failed: invalid shape"),
            Err(e) => eprintln!("[sessions] load_hidden_sessions failed: {e}"),
        }
    }
}
