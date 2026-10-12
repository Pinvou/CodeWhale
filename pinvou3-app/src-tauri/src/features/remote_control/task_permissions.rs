//! B-owned remote task policy; local lane defaults never authorize remote execution.
use crate::core::mode_state::SerializableMode;
use crate::features::sessions::SessionStore;
use crate::platform::prefs::UserPrefs;

pub(crate) const REQUIRED: &str = "task_permission_mode_required";
pub(crate) const CHANGED: &str = "task_permission_mode_changed";

pub(crate) fn configured(code: bool) -> Result<SerializableMode, String> {
    let policy = UserPrefs::load().remote_task_permissions;
    (if code { policy.code } else { policy.chat }).ok_or_else(|| REQUIRED.to_owned())
}

pub(crate) fn for_session(
    store: &SessionStore,
    id: &str,
    code: bool,
) -> Result<SerializableMode, String> {
    let selected = configured(code)?;
    validate_frozen(
        selected,
        store.durable_mode_entry(id),
        store.mode_state(id).mode,
    )
}

fn validate_frozen(
    selected: SerializableMode,
    persisted: Option<SerializableMode>,
    current: SerializableMode,
) -> Result<SerializableMode, String> {
    if persisted != Some(selected) || current != selected {
        return Err(CHANGED.to_owned());
    }
    Ok(selected)
}

/// Contract of the pinned Codex ACP 1.6.2 adapter, not generic ACP mode semantics.
pub(crate) fn codex_mode(mode: SerializableMode) -> &'static str {
    match mode {
        SerializableMode::Plan => "read-only",
        SerializableMode::Yolo => "agent",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn local_defaults_do_not_authorize_remote_tasks() {
        let prefs: UserPrefs = serde_json::from_str(r#"{"mode_defaults":{"work":"yolo"},"code_permission":{"last_mode":"yolo","yolo_confirmed":true}}"#).unwrap();
        assert_eq!(prefs.remote_task_permissions.chat, None);
        assert_eq!(prefs.remote_task_permissions.code, None);
    }
    #[test]
    fn explicit_lanes_round_trip_without_cross_lane_fallback() {
        let prefs: UserPrefs =
            serde_json::from_str(r#"{"remote_task_permissions":{"chat":"plan"}}"#).unwrap();
        assert_eq!(
            prefs.remote_task_permissions.chat,
            Some(SerializableMode::Plan)
        );
        assert_eq!(prefs.remote_task_permissions.code, None);
        let saved = serde_json::to_string(&prefs).unwrap();
        let loaded: UserPrefs = serde_json::from_str(&saved).unwrap();
        assert_eq!(
            loaded.remote_task_permissions.chat,
            Some(SerializableMode::Plan)
        );
        assert_eq!(loaded.remote_task_permissions.code, None);
    }
    #[test]
    fn unsupported_values_do_not_upgrade_to_agent() {
        for value in ["agent-full-access", "auto", "", "Agent"] {
            let input = serde_json::json!({"mode_defaults":{"work":"plan"}, "remote_task_permissions":{"chat":"plan", "code":value}});
            let prefs = serde_json::from_value::<UserPrefs>(input).unwrap();
            assert_eq!(prefs.remote_task_permissions.code, None);
            assert_eq!(
                prefs.remote_task_permissions.chat,
                Some(SerializableMode::Plan)
            );
            assert_eq!(prefs.mode_defaults.work, Some(SerializableMode::Plan));
        }
    }
    #[test]
    fn malformed_remote_object_preserves_local_preferences() {
        for value in [
            serde_json::json!(false),
            serde_json::json!(42),
            serde_json::json!(["yolo"]),
            serde_json::json!("yolo"),
            serde_json::Value::Null,
        ] {
            let prefs: UserPrefs = serde_json::from_value(serde_json::json!({"mode_defaults":{"work":"plan"},"remote_task_permissions":value})).unwrap();
            assert_eq!(prefs.remote_task_permissions.chat, None);
            assert_eq!(prefs.remote_task_permissions.code, None);
            assert_eq!(prefs.mode_defaults.work, Some(SerializableMode::Plan));
        }
    }
    #[test]
    fn codex_mapping_never_selects_approval_never_full_access() {
        assert_eq!(codex_mode(SerializableMode::Plan), "read-only");
        assert_eq!(codex_mode(SerializableMode::Yolo), "agent");
    }
    #[test]
    fn changed_missing_or_locally_upgraded_session_cannot_execute() {
        use SerializableMode::{Plan, Yolo};
        assert_eq!(validate_frozen(Plan, Some(Plan), Plan).unwrap(), Plan);
        assert_eq!(validate_frozen(Yolo, Some(Yolo), Yolo).unwrap(), Yolo);
        for (selected, stored, current) in [
            (Yolo, Some(Plan), Plan),
            (Plan, Some(Yolo), Yolo),
            (Plan, None, Plan),
            (Plan, Some(Plan), Yolo),
        ] {
            assert_eq!(
                validate_frozen(selected, stored, current).unwrap_err(),
                CHANGED
            );
        }
    }
}
