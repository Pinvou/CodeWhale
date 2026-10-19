//! B-owned remote task policy; local lane defaults never authorize remote execution.
mod frozen;

use crate::core::mode_state::SerializableMode;
use crate::features::sessions::SessionStore;
use crate::platform::prefs::{RemoteTaskPermissionPrefs, UserPrefs};

pub(crate) const REQUIRED: &str = "task_permission_mode_required";
pub(crate) const CHANGED: &str = "task_permission_mode_changed";

/// Enabling B is the explicit authorization boundary, not deserializing prefs.
/// Preserve an existing Plan choice and persist before starting the listener.
pub(crate) fn enable_defaults() -> Result<RemoteTaskPermissionPrefs, String> {
    UserPrefs::update_transaction(|prefs| {
        validate_existing_settings()?;
        initialize_defaults(&mut prefs.remote_task_permissions);
        Ok(())
    })
    .map(|prefs| prefs.remote_task_permissions)
    .map_err(|_| "remote_task_permissions_save_failed".to_owned())
}

fn initialize_defaults(policy: &mut RemoteTaskPermissionPrefs) {
    policy.chat.get_or_insert(SerializableMode::Yolo);
    policy.code.get_or_insert(SerializableMode::Yolo);
}

/// Called inside the settings transaction's locks. The normal loader tolerates
/// damaged settings; a permission change must not persist that fallback over
/// a user's existing configuration.
fn validate_existing_settings() -> Result<(), String> {
    let raw = match std::fs::read(crate::platform::paths::settings_path()) {
        Ok(raw) => raw,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(_) => return Err("remote_task_permissions_save_failed".into()),
    };
    let value: serde_json::Value = serde_json::from_slice(&raw)
        .map_err(|_| "remote_task_permissions_save_failed".to_owned())?;
    if !value.is_object() {
        return Err("remote_task_permissions_save_failed".into());
    }
    serde_json::from_value::<UserPrefs>(value)
        .map(|_| ())
        .map_err(|_| "remote_task_permissions_save_failed".to_owned())
}

pub(crate) fn save(policy: RemoteTaskPermissionPrefs) -> Result<RemoteTaskPermissionPrefs, String> {
    // The settings UI saves one lane at a time, including before first enable.
    // Persist an unconfigured sibling without authorizing it; configured()
    // still rejects that lane until selected or initialized by explicit enable.
    if policy.chat.is_none() && policy.code.is_none() {
        return Err(REQUIRED.to_owned());
    }
    UserPrefs::update_transaction(|prefs| {
        validate_existing_settings()?;
        prefs.remote_task_permissions = policy;
        Ok(())
    })
    .map(|prefs| prefs.remote_task_permissions)
    .map_err(|_| "remote_task_permissions_save_failed".to_owned())
}

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
    validate_session(store, id, code, selected)
}

/// Called only for a newly created session's first trusted claim. Freeze the
/// immutable fact before the mutable mode write; failure must not erase it.
pub(crate) fn initialize_new_session(
    store: &SessionStore,
    id: &str,
    code: bool,
    selected: SerializableMode,
) -> Result<(), String> {
    if store.session_kind(id).ok() != Some(crate::features::sessions::SessionKind::Chat) {
        return Err(CHANGED.to_owned());
    }
    // An existing fact is validation authority, never permission to reset a
    // user-changed mode, including a retry after a failed first mode write.
    if frozen::register_new(store, id, code, selected)? {
        store
            .set_mode_and_persist(id, selected)
            .map_err(|_| "task_session_backend_bind_failed".to_owned())?;
    }
    validate_session(store, id, code, selected).map(|_| ())
}

pub(crate) fn validate_session(
    store: &SessionStore,
    id: &str,
    code: bool,
    selected: SerializableMode,
) -> Result<SerializableMode, String> {
    if store.session_kind(id).ok() != Some(crate::features::sessions::SessionKind::Chat) {
        return Err(CHANGED.to_owned());
    }
    let fact = frozen::read(store, id)?.ok_or_else(|| CHANGED.to_owned())?;
    if fact.code != code || fact.mode != selected {
        return Err(CHANGED.to_owned());
    }
    validate_frozen(
        selected,
        store
            .strict_durable_mode_entry(id)
            .map_err(|_| CHANGED.to_owned())?,
        store.mode_state(id).mode,
    )
}

#[cfg(test)]
mod frozen_tests;

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
    fn enable_defaults_fills_both_lanes_and_preserves_plan() {
        let mut policy = RemoteTaskPermissionPrefs::default();
        initialize_defaults(&mut policy);
        assert_eq!(policy.chat, Some(SerializableMode::Yolo));
        assert_eq!(policy.code, Some(SerializableMode::Yolo));
        policy.chat = Some(SerializableMode::Plan);
        initialize_defaults(&mut policy);
        assert_eq!(policy.chat, Some(SerializableMode::Plan));
        assert_eq!(policy.code, Some(SerializableMode::Yolo));
    }
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
