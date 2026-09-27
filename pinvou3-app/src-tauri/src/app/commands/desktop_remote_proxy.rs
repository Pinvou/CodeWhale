//! Thin, allowlisted WebView projection of SessionStore RemoteProxy history.
//! No peer, remote ref, B mapping, pending ID, fingerprint, or credentials
//! cross this boundary. Delete means durable UI soft-hide, not record removal.

use serde_json::{Value, json};
use tauri::State;

use crate::features::sessions::SessionStore;

fn list_visible(store: &SessionStore) -> Vec<Value> {
    store
        .list_remote_proxy_history()
        .into_iter()
        .filter(|item| !item.ui.hidden)
        .map(|item| {
            json!({
                "localSessionId": item.local_session_id,
                "title": item.ui.title,
                "pinned": item.ui.pinned,
                "lastOpenedAt": item.ui.last_opened_at,
                "projectedEntryCount": item.projected_entry_count,
            })
        })
        .collect()
}

fn read_visible(store: &SessionStore, local_session_id: &str) -> Result<Value, String> {
    let view = store
        .read_remote_proxy_history(local_session_id)
        .filter(|view| !view.ui.hidden)
        .ok_or_else(|| "remote_proxy_not_found".to_owned())?;
    let entries: Vec<_> = view
        .projection
        .entries
        .into_iter()
        .map(|entry| {
            let attachments: Vec<_> = entry
                .attachments
                .into_iter()
                .map(|attachment| {
                    json!({
                        "id": attachment.id,
                        "displayName": attachment.display_name,
                        "contentType": attachment.content_type,
                        "byteLength": attachment.byte_length,
                        "checksumSha256": attachment.checksum_sha256,
                    })
                })
                .collect();
            json!({
                "journalEpoch": entry.journal_epoch,
                "sequence": entry.sequence,
                "eventId": entry.event_id,
                "kind": entry.kind,
                "taskId": entry.task_id,
                "body": entry.body,
                "attachments": attachments,
            })
        })
        .collect();
    let cursor = view
        .authenticated_cursor
        .map(|cursor| json!({"journalEpoch": cursor.journal_epoch, "sequence": cursor.sequence}));
    Ok(json!({
        "localSessionId": view.local_session_id,
        "title": view.ui.title,
        "pinned": view.ui.pinned,
        "lastOpenedAt": view.ui.last_opened_at,
        "entries": entries,
        "authenticatedCursor": cursor,
        "pendingUnknownCount": view.pending_unknown_count,
    }))
}

#[tauri::command]
pub(crate) fn desktop_remote_proxy_history_list(store: State<'_, SessionStore>) -> Vec<Value> {
    list_visible(&store)
}

#[tauri::command]
pub(crate) fn desktop_remote_proxy_history_read(
    store: State<'_, SessionStore>,
    local_session_id: String,
) -> Result<Value, String> {
    read_visible(&store, &local_session_id)
}

#[tauri::command]
pub(crate) fn desktop_remote_proxy_history_delete(
    store: State<'_, SessionStore>,
    local_session_id: String,
) -> Result<(), String> {
    if store.read_remote_proxy_history(&local_session_id).is_none() {
        return Err("remote_proxy_not_found".to_owned());
    }
    store
        .hide_remote_proxy_history(&local_session_id)
        .map_err(|_| "remote_proxy_delete_failed".to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;

    fn live() -> LiveRemoteContext {
        LiveRemoteContext {
            peer_id: "peer-1".into(),
            resume_boundary_id: "resume-1".into(),
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "epoch-1".into(),
            relay_epoch: "relay-1".into(),
            generation: "generation-1".into(),
            control_connected: true,
            encrypted_channel_connected: true,
        }
    }

    #[test]
    fn history_ui_allowlist_and_soft_delete_visibility() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let opened = store.open_remote_proxy(None, &live()).unwrap();
        let listed = list_visible(&store);
        assert_eq!(listed.len(), 1);
        assert_eq!(listed[0]["localSessionId"], opened.local_session_id);
        let before = read_visible(&store, &opened.local_session_id).unwrap();
        assert!(before.get("entries").is_some());
        for forbidden in [
            "peerId",
            "remoteSessionRef",
            "controlledSessionId",
            "pending",
            "retiredRequests",
        ] {
            assert!(before.get(forbidden).is_none());
            assert!(listed[0].get(forbidden).is_none());
        }
        store
            .hide_remote_proxy_history(&opened.local_session_id)
            .unwrap();
        assert!(list_visible(&store).is_empty());
        assert_eq!(
            read_visible(&store, &opened.local_session_id).unwrap_err(),
            "remote_proxy_not_found"
        );
        assert!(
            store
                .remote_proxy_snapshot(&opened.local_session_id)
                .is_some()
        );
    }

    #[test]
    fn unsafe_legacy_webview_handlers_are_not_registered() {
        let root = include_str!("../../lib.rs");
        assert!(!root.contains("commands::desktop_remote_controller_session::desktop_remote_controller_apply_capability_snapshot,"));
        assert!(!root.contains(
            "commands::desktop_remote_controller_session::desktop_remote_controller_task_send,"
        ));
        assert!(!root.contains(
            "commands::desktop_remote_controller_session::desktop_remote_controller_task_intent,"
        ));
        assert!(!root.contains(
            "commands::desktop_remote_controller_session::desktop_remote_controller_task_cancel_intent,"
        ));
        assert!(!root.contains("commands::desktop_remote_task::desktop_remote_task_submit,"));
        assert_eq!(
            root.matches(
                "commands::desktop_remote_controller_session::desktop_remote_controller_task_cancel_send,"
            )
            .count(),
            1
        );
        for command in [
            "commands::desktop_remote_proxy::desktop_remote_proxy_history_list,",
            "commands::desktop_remote_proxy::desktop_remote_proxy_history_read,",
            "commands::desktop_remote_proxy::desktop_remote_proxy_history_delete,",
        ] {
            assert_eq!(root.matches(command).count(), 1);
        }
    }
}
