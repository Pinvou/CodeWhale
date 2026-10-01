//! Thin, allowlisted WebView projection of SessionStore RemoteProxy history.
//! No peer, remote ref, B mapping, credentials, or task body cross this boundary.
//! Never-attempted request IDs/fingerprints are exposed only for explicit
//! abandon after a restart. Delete means durable UI soft-hide, not removal.

use serde::Serialize;
use serde_json::{Value, json};
use tauri::State;

use crate::features::remote_control::controller::controlling::ControllingDeviceManager;
use crate::features::remote_control::controller::controlling_session::{
    ControllingSessionManager, ControllingSessionProjection,
};
use crate::features::remote_control::controller::encrypted_lane::ControllingLaneState;
use crate::features::sessions::SessionStore;
use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct LiveRemoteProxyOpen {
    local_session_id: String,
    remote_session_ref: String,
    session_id: String,
    mode: String,
    model_source: String,
    connection_id: String,
    controller_lease_id: String,
    lease_epoch: String,
    relay_epoch: String,
    remote_control_generation: String,
    capability_revision: Option<u64>,
    capability_valid_for_ms: Option<u64>,
    capability_error: Option<String>,
    capability_request_id: Option<String>,
    temporary_model_available: bool,
    modes: Vec<String>,
    workspace_count: usize,
    task_count: usize,
    mapping_confirmed: bool,
}

fn authenticated_live_context(
    manager: &ControllingDeviceManager,
    lane: &ControllingLaneState,
) -> Result<LiveRemoteContext, String> {
    if !manager.is_supported() || !lane.is_installed() || lane.is_suspended() {
        return Err("remote_proxy_channel_unavailable".into());
    }
    let (lease, peer_id, resume_boundary_id) = manager
        .authenticated_remote_proxy_identity()
        .map_err(|_| "remote_proxy_channel_unavailable".to_owned())?;
    Ok(LiveRemoteContext {
        peer_id,
        resume_boundary_id,
        connection_id: lease.connection_id,
        controller_lease_id: lease.controller_lease_id,
        lease_epoch: lease.lease_epoch,
        relay_epoch: lease.relay_epoch,
        generation: lease.remote_control_generation,
        control_connected: true,
        encrypted_channel_connected: true,
    })
}

fn open_live(
    store: &SessionStore,
    manager: &ControllingDeviceManager,
    lane: &ControllingLaneState,
    sessions: &ControllingSessionManager,
    existing_local_session_id: Option<&str>,
) -> Result<LiveRemoteProxyOpen, String> {
    let context = authenticated_live_context(manager, lane)?;
    let (lease, capabilities) = manager
        .live_runtime_context()
        .map_err(|_| "remote_proxy_channel_unavailable".to_owned())?;
    if lease.connection_id != context.connection_id
        || lease.controller_lease_id != context.controller_lease_id
        || lease.lease_epoch != context.lease_epoch
        || lease.relay_epoch != context.relay_epoch
        || lease.remote_control_generation != context.generation
    {
        return Err("remote_proxy_channel_changed".into());
    }
    let prepared = store
        .prepare_remote_proxy_open(existing_local_session_id, &context)
        .map_err(|_| "remote_proxy_open_failed".to_owned())?;
    let remote_ref = prepared.open.remote_session_ref.clone();
    let (projection, created_intent): (ControllingSessionProjection, bool) = match sessions
        .open_with_outcome(Some(&remote_ref), "remote", "chat", &lease, &capabilities)
    {
        Ok(outcome) => outcome,
        Err(_) => return Err("remote_proxy_session_unavailable".to_owned()),
    };
    let expected = (
        lease,
        context.peer_id.clone(),
        context.resume_boundary_id.clone(),
    );
    let opened =
        match manager.with_current_authenticated_remote_proxy_identity(lane, &expected, || {
            store
                .commit_prepared_remote_proxy_open(prepared, &context)
                .map_err(|_| "remote_proxy_open_failed".to_owned())
        }) {
            Ok(opened) => opened,
            Err(error) => {
                if created_intent {
                    let _ = sessions.close(&remote_ref);
                }
                return Err(error);
            }
        };
    let mapping_confirmed = store
        .require_live_remote_mutation(
            &opened.local_session_id,
            &opened.remote_session_ref,
            &context,
        )
        .is_ok();
    Ok(LiveRemoteProxyOpen {
        local_session_id: opened.local_session_id,
        remote_session_ref: opened.remote_session_ref,
        session_id: projection.session_id,
        mode: projection.mode,
        model_source: projection.model_source,
        connection_id: context.connection_id,
        controller_lease_id: context.controller_lease_id,
        lease_epoch: context.lease_epoch,
        relay_epoch: context.relay_epoch,
        remote_control_generation: context.generation,
        capability_revision: projection.capability_revision,
        capability_valid_for_ms: projection.capability_valid_for_ms,
        capability_error: projection.capability_error,
        capability_request_id: projection.capability_request_id,
        temporary_model_available: projection.temporary_model_available,
        modes: projection.modes,
        workspace_count: projection.workspace_count,
        task_count: projection.task_count,
        mapping_confirmed,
    })
}

#[tauri::command]
pub(crate) fn desktop_remote_proxy_open_live(
    store: State<'_, SessionStore>,
    manager: State<'_, ControllingDeviceManager>,
    lane: State<'_, ControllingLaneState>,
    sessions: State<'_, ControllingSessionManager>,
    existing_local_session_id: Option<String>,
) -> Result<LiveRemoteProxyOpen, String> {
    open_live(
        &store,
        &manager,
        &lane,
        &sessions,
        existing_local_session_id.as_deref(),
    )
}

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
    let unattempted_pending: Vec<_> = view
        .unattempted_pending
        .into_iter()
        .map(|pending| {
            json!({
                "taskId": pending.task_id,
                "clientRequestId": pending.client_request_id,
                "requestFingerprint": pending.request_fingerprint,
                "dispatchState": "prepared_never_attempted",
            })
        })
        .collect();
    Ok(json!({
        "localSessionId": view.local_session_id,
        "title": view.ui.title,
        "pinned": view.ui.pinned,
        "lastOpenedAt": view.ui.last_opened_at,
        "entries": entries,
        "authenticatedCursor": cursor,
        "pendingUnknownCount": view.pending_unknown_count,
        "unattemptedPending": unattempted_pending,
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
    hide_visible(&store, &local_session_id)
}

fn hide_visible(store: &SessionStore, local_session_id: &str) -> Result<(), String> {
    if store.read_remote_proxy_history(&local_session_id).is_none() {
        return Err("remote_proxy_not_found".to_owned());
    }
    store
        .hide_remote_proxy_history(local_session_id)
        .map_err(|error| {
            if error.to_string() == "remote_unattempted_pending_exists" {
                "remote_unattempted_pending_exists".to_owned()
            } else {
                "remote_proxy_delete_failed".to_owned()
            }
        })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::sessions::remote_proxy::binding::{
        AuthenticatedMapping, LiveRemoteContext,
    };
    use crate::features::sessions::remote_proxy::schema::{
        PendingDispatchState, PendingRemoteRequest,
    };

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
        let opened = store.test_commit_remote_proxy_open(None, &live()).unwrap();
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
    fn restarted_history_exposes_only_never_attempted_non_secret_abandon_handles() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = live();
        let opened = store.test_commit_remote_proxy_open(None, &live).unwrap();
        store
            .confirm_remote_mapping(
                &opened.local_session_id,
                &live,
                &AuthenticatedMapping {
                    remote_session_ref: opened.remote_session_ref.clone(),
                    controlled_session_id: "b-session-1".into(),
                },
            )
            .unwrap();
        for (client, task) in [
            ("request-prepared", "task-prepared"),
            ("request-attempted", "task-attempted"),
        ] {
            store
                .insert_remote_pending(
                    &opened.local_session_id,
                    &opened.remote_session_ref,
                    &live,
                    PendingRemoteRequest {
                        client_request_id: client.into(),
                        task_id: task.into(),
                        request_fingerprint: "a".repeat(64),
                        dispatch_state: PendingDispatchState::PreparedNeverAttempted,
                        reservation_fingerprint: None,
                    },
                )
                .unwrap();
        }
        store
            .mark_remote_dispatch_attempt(
                &opened.local_session_id,
                &opened.remote_session_ref,
                &live,
                "request-attempted",
                "task-attempted",
                &"a".repeat(64),
            )
            .unwrap();
        let restarted = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let history = read_visible(&restarted, &opened.local_session_id).unwrap();
        assert_eq!(history["pendingUnknownCount"], 1);
        assert_eq!(history["unattemptedPending"].as_array().unwrap().len(), 1);
        assert_eq!(history["unattemptedPending"][0]["taskId"], "task-prepared");
        assert_eq!(
            history["unattemptedPending"][0]["clientRequestId"],
            "request-prepared"
        );
        assert_eq!(
            history["unattemptedPending"][0]["requestFingerprint"],
            "a".repeat(64)
        );
        assert_eq!(
            history["unattemptedPending"][0]["dispatchState"],
            "prepared_never_attempted"
        );
        let encoded = serde_json::to_string(&history).unwrap();
        for forbidden in [
            "request-attempted",
            "task-attempted",
            "sk-secret",
            "temporary_model_package",
            "remoteSessionRef",
            "controlledSessionId",
        ] {
            assert!(!encoded.contains(forbidden));
        }
        let disk_path = temp.path().join("sessions/_remote_proxies.json");
        let before_hide = std::fs::read(&disk_path).unwrap();
        assert_eq!(
            hide_visible(&restarted, &opened.local_session_id).unwrap_err(),
            "remote_unattempted_pending_exists"
        );
        assert_eq!(std::fs::read(&disk_path).unwrap(), before_hide);
        assert!(read_visible(&restarted, &opened.local_session_id).is_ok());
        restarted
            .abandon_unattempted_remote_pending(
                &opened.local_session_id,
                "request-prepared",
                "task-prepared",
                &"a".repeat(64),
            )
            .unwrap();
        hide_visible(&restarted, &opened.local_session_id).unwrap();
        let record = restarted
            .remote_proxy_snapshot(&opened.local_session_id)
            .unwrap();
        assert!(record.ui.hidden);
        assert_eq!(record.pending.len(), 1);
        assert_eq!(
            record.pending[0].dispatch_state,
            PendingDispatchState::AttemptMayHaveOccurred
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
        assert!(!root.contains("commands::desktop_remote_controller_session::desktop_remote_controller_task_cancel_send,"));
        assert!(
            !root.contains("commands::desktop_remote_task::desktop_remote_task_abandon_lost_a,")
        );
        assert_eq!(
            root.matches("commands::desktop_remote_task::desktop_remote_task_abandon_prepared_a,")
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
