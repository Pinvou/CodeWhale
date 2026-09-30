/**
 * Windows desktop dual-end remote control bridge (DEV-20).
 *
 * This is a separate domain from the existing `webAccess` WebUI share:
 * `controlledDevice` (B), `controllerConnection` + `remoteCapabilities` +
 * `remoteModels` + `remoteHistories` (A) live here, and every async completion
 * is gated by the intent sequence and generation/lease epoch held in
 * `features/remote-control/remote-control-state.js`.
 *
 * No API key, credential reference or `SavedModel` body ever crosses this file:
 * model listing returns opaque descriptors and task submission only passes the
 * selected model ID plus the task binding.
 */
(function (root) {
  "use strict";
  var registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};
  registry["desktop-remote-control"] = function (context) {
    var state = context.state;
    var notify = context.notify;
    var invoke = context.invoke;
    var listen = context.listen;
    var can = context.can;
    var logic = root.__PINVOU_REMOTE_CONTROL_STATE__;

    if (!logic) throw new Error("desktop remote control state logic not loaded");

    var unlisten = [];
    var subscribed = false;
    function isAvailable() {
      // Capabilities load after the bridge is installed, so this must stay
      // lazy instead of being captured once at install time.
      return typeof can === "function" && can("desktopRemoteControl") === true;
    }

    function publish(next) {
      state.desktopRemoteControl = next;
      if (typeof notify === "function") notify("desktopRemoteControl");
      return next;
    }

    function current() {
      return state.desktopRemoteControl || logic.createState();
    }

    function ensureAvailable() {
      if (!isAvailable()) {
        var error = new Error("desktop_remote_control_unsupported");
        error.code = "desktop_remote_control_unsupported";
        throw error;
      }
    }

    function subscribe() {
      if (subscribed || typeof listen !== "function") return;
      subscribed = true;
      listen("desktop-remote-controlled-status", function (event) {
        var payload = event && event.payload ? event.payload : event || {};
        publish(logic.applyControlledStatus(current(), payload));
      }).then(function (off) {
        unlisten.push(off);
      }).catch(function () {});
      ["desktop-remote-task-status", "desktop-remote-task-event"].forEach(function (name) {
        listen(name, function (event) {
          var payload = event && event.payload ? event.payload : event || {};
          publish(logic.applyTaskEvent(current(), payload));
        }).then(function (off) {
          unlisten.push(off);
        }).catch(function () {});
      });
    }

    function stop() {
      unlisten.splice(0).forEach(function (off) {
        try {
          off();
        } catch (_) {}
      });
      subscribed = false;
    }

    function controlledIntent() {
      var next = logic.beginControlledIntent(current());
      publish(next);
      return next.intents.controlled;
    }

    function controllerIntent() {
      var next = logic.beginControllerIntent(current());
      publish(next);
      return next.intents.controller;
    }

    function applyControlled(intent, result) {
      var payload = result && typeof result === "object" ? result : {};
      publish(logic.applyControlledStatus(current(), {
        intent: intent,
        status: payload.kind || payload.status || "idle",
        generation: payload.remoteControlGeneration || payload.generation || null,
        pairingCode: payload.pairingCode || null,
        expiresAtMs: payload.expiresAtMs || payload.pairingExpiresAtMs || null,
        deviceName: payload.deviceName || null,
        workspaces: payload.workspaces || null,
        error: payload.lastError || null,
      }));
      return result;
    }

    function applyController(intent, result) {
      var payload = result && typeof result === "object" ? result : {};
      publish(logic.applyControllerStatus(current(), {
        intent: intent,
        status: payload.kind || payload.status || "idle",
        connectionId: payload.connectionId || null,
        leaseEpoch: payload.leaseEpoch || null,
        deviceName: payload.deviceName || null,
        error: payload.lastError || null,
      }));
      return result;
    }

    function controlledStart(deviceName, workspaces) {
      ensureAvailable();
      subscribe();
      var intent = controlledIntent();
      return invoke("desktop_remote_controlled_start", {
        deviceName: deviceName || null,
        workspaces: Array.isArray(workspaces) ? workspaces : [],
      }).then(function (result) {
        return applyControlled(intent, result);
      });
    }

    function controlledRefresh() {
      ensureAvailable();
      subscribe();
      var intent = controlledIntent();
      return invoke("desktop_remote_controlled_refresh", {}).then(function (result) {
        return applyControlled(intent, result);
      });
    }

    function controlledStop() {
      ensureAvailable();
      var intent = controlledIntent();
      return invoke("desktop_remote_controlled_stop", {}).then(function (result) {
        return applyControlled(intent, result);
      });
    }

    function controlledDisconnectNow() {
      ensureAvailable();
      var intent = controlledIntent();
      return invoke("desktop_remote_controlled_disconnect_now", {}).then(function (result) {
        return applyControlled(intent, result);
      });
    }

    function controlledStatus() {
      ensureAvailable();
      subscribe();
      return invoke("desktop_remote_controlled_status", {});
    }

    function controlledListWorkspaces() {
      ensureAvailable();
      return invoke("desktop_remote_controlled_list_workspaces", {});
    }

    function controlledAuthorizeWorkspace(workspace) {
      ensureAvailable();
      return invoke("desktop_remote_controlled_authorize_workspace", { workspace: workspace });
    }

    function controllerConnect(code) {
      ensureAvailable();
      var intent = controllerIntent();
      return invoke("desktop_remote_controller_connect", { code: code }).then(function (result) {
        return applyController(intent, result);
      });
    }

    function controllerDisconnect() {
      ensureAvailable();
      var intent = controllerIntent();
      return invoke("desktop_remote_controller_disconnect", {}).then(function (result) {
        return applyController(intent, result);
      });
    }

    function controllerStatus() {
      ensureAvailable();
      return invoke("desktop_remote_controller_status", {});
    }

    function controllerListHistory() {
      ensureAvailable();
      return invoke("desktop_remote_controller_list_history", {}).then(function (result) {
        publish(logic.applyHistories(current(), result || []));
        return result;
      });
    }

    function controllerCreateHistory(title, deviceDisplayName, sessionRef) {
      ensureAvailable();
      return invoke("desktop_remote_controller_create_history", {
        title: title,
        deviceDisplayName: deviceDisplayName,
        sessionRef: sessionRef,
      });
    }

    function capabilitiesSnapshot() {
      ensureAvailable();
      return invoke("desktop_remote_capabilities_snapshot", {}).then(function (snapshot) {
        var before = current();
        var after = logic.applyCapabilities(before, snapshot);
        if (after === before) return null;
        publish(after);
        return snapshot;
      });
    }

    function listExportableModels() {
      ensureAvailable();
      return invoke("desktop_remote_model_list_exportable", {}).then(function (models) {
        publish(logic.applyModels(current(), models || []));
        return models || [];
      });
    }

    function openRemoteSession(remoteSessionRef, title, codeMode) {
      ensureAvailable();
      return invoke("desktop_remote_task_open_session", {
        remoteSessionRef: remoteSessionRef,
        title: title,
        codeMode: !!codeMode,
      });
    }

    function switchRemoteSession(remoteSessionRef, sessionId) {
      ensureAvailable();
      return invoke("desktop_remote_task_switch_session", {
        remoteSessionRef: remoteSessionRef,
        sessionId: sessionId,
      });
    }

    /**
     * Submits one task. `payload` must be the already-built protocol payload
     * (binding + input digest); `modelId` is only forwarded when the task uses
     * an A-side temporary model, because Rust resolves the credential itself.
     */
    function submitRemoteTask(payload, selectedModelId, permissionRequest) {
      ensureAvailable();
      var body = payload && typeof payload === "object" ? payload : {};
      if (!logic.matchesLiveContext(current(), body)) {
        var stale = new Error("task_binding_stale");
        stale.code = "task_binding_stale";
        return Promise.reject(stale);
      }
      return invoke("desktop_remote_task_submit", {
        payload: body,
        selectedModelId: selectedModelId || null,
        permissionRequest: permissionRequest || null,
      }).then(function (result) {
        publish(logic.applyTaskEvent(current(), result || {}));
        return result;
      });
    }

    function cancelRemoteTask(taskId) {
      ensureAvailable();
      return invoke("desktop_remote_task_cancel", { taskId: taskId }).then(function (result) {
        publish(logic.applyTaskEvent(current(), result || {}));
        return result;
      });
    }

    function remoteTaskStatus(taskId) {
      ensureAvailable();
      return invoke("desktop_remote_task_status", { taskId: taskId });
    }

    function permissionList(taskId) {
      ensureAvailable();
      return invoke("desktop_remote_permission_list", { taskId: taskId }).then(function (permissions) {
        var next = shallow(current());
        next.remotePermissions = Array.isArray(permissions)
          ? permissions.slice(0, 8).map(function (entry) {
              return {
                permissionRequestId: String(entry.permissionRequestId || ""),
                taskId: String(entry.taskId || ""),
                operationType: String(entry.operationType || ""),
                riskLevel: String(entry.riskLevel || ""),
                targetScope: String(entry.targetScope || ""),
                preview: String(entry.preview || ""),
                state: String(entry.state || "pending"),
              };
            })
          : [];
        publish(next);
        return permissions;
      });
    }

    function permissionDecide(taskId, permissionRequestId, decision) {
      ensureAvailable();
      return invoke("desktop_remote_permission_decide", {
        taskId: taskId,
        permissionRequestId: permissionRequestId,
        decision: decision,
      }).then(function (result) {
        var next = shallow(current());
        next.remotePermissions = (next.remotePermissions || []).filter(function (entry) {
          return entry.permissionRequestId !== permissionRequestId;
        });
        publish(next);
        return result;
      });
    }

    function permissionDenyTask(taskId, reason) {
      ensureAvailable();
      return invoke("desktop_remote_permission_deny_task", { taskId: taskId, reason: reason || "b_stop" });
    }

    function attachmentBegin(request) {
      ensureAvailable();
      return invoke("desktop_remote_attachment_begin", request || {});
    }

    function attachmentChunk(transferId, chunkIndex, chunkB64, chunkBytes) {
      ensureAvailable();
      return invoke("desktop_remote_attachment_chunk", {
        transferId: transferId,
        chunkIndex: chunkIndex,
        chunkB64: chunkB64,
        chunkBytes: chunkBytes,
      });
    }

    function attachmentAbort(transferId) {
      ensureAvailable();
      return invoke("desktop_remote_attachment_abort", { transferId: transferId });
    }

    function artifactIndex(sessionId) {
      ensureAvailable();
      return invoke("desktop_remote_artifact_index", { sessionId: sessionId });
    }

    function artifactPreview(sessionId, artifactId) {
      ensureAvailable();
      return invoke("desktop_remote_artifact_preview", { sessionId: sessionId, artifactId: artifactId });
    }

    function artifactDownloadBegin(sessionId, taskId, artifactId) {
      ensureAvailable();
      return invoke("desktop_remote_artifact_download_begin", {
        sessionId: sessionId,
        taskId: taskId,
        artifactId: artifactId,
      });
    }

    function artifactDownloadChunk(sessionId, artifactId, transferId, chunkIndex) {
      ensureAvailable();
      return invoke("desktop_remote_artifact_download_chunk", {
        sessionId: sessionId,
        artifactId: artifactId,
        transferId: transferId,
        chunkIndex: chunkIndex,
      });
    }

    function artifactDownloadAbort(transferId) {
      ensureAvailable();
      return invoke("desktop_remote_artifact_download_abort", { transferId: transferId });
    }

    function historyList() {
      ensureAvailable();
      return invoke("desktop_remote_history_list", {}).then(function (records) {
        publish(logic.applyHistories(current(), records || []));
        return records || [];
      });
    }

    function historyLoad(historyId) {
      ensureAvailable();
      return invoke("desktop_remote_history_load", { historyId: historyId });
    }

    function historyDelete(historyId) {
      ensureAvailable();
      return invoke("desktop_remote_history_delete", { historyId: historyId }).then(function (deleted) {
        if (deleted) {
          var next = shallow(current());
          next.remoteHistories = (next.remoteHistories || []).filter(function (record) {
            return record.historyId !== historyId;
          });
          publish(next);
        }
        return deleted;
      });
    }

    function historyMarkOffline(historyId) {
      ensureAvailable();
      return invoke("desktop_remote_history_mark_offline", { historyId: historyId }).then(function (record) {
        var next = shallow(current());
        next.remoteHistories = (next.remoteHistories || []).map(function (entry) {
          if (entry.historyId !== historyId) return entry;
          return {
            historyId: entry.historyId,
            title: entry.title,
            deviceDisplayName: entry.deviceDisplayName,
            state: "offline_copy",
            lastSequence: entry.lastSequence,
            lastTerminalStatus: entry.lastTerminalStatus,
          };
        });
        publish(next);
        return record;
      });
    }

    function journalReplay(epoch, sequence) {
      ensureAvailable();
      return invoke("desktop_remote_journal_replay", { epoch: epoch, sequence: sequence });
    }

    function journalSnapshot() {
      ensureAvailable();
      return invoke("desktop_remote_journal_snapshot", {});
    }

    function journalAcknowledge(sequence) {
      ensureAvailable();
      return invoke("desktop_remote_journal_acknowledge", { sequence: sequence });
    }

    function shallow(value) {
      var copy = {};
      Object.keys(value || {}).forEach(function (key) {
        copy[key] = value[key];
      });
      return copy;
    }

    publish(current());

    return {
      isAvailable: function () {
        return isAvailable();
      },
      subscribe: subscribe,
      stop: stop,
      controlledStart: controlledStart,
      controlledRefresh: controlledRefresh,
      controlledStop: controlledStop,
      controlledDisconnectNow: controlledDisconnectNow,
      controlledStatus: controlledStatus,
      controlledListWorkspaces: controlledListWorkspaces,
      controlledAuthorizeWorkspace: controlledAuthorizeWorkspace,
      controllerConnect: controllerConnect,
      controllerDisconnect: controllerDisconnect,
      controllerStatus: controllerStatus,
      controllerListHistory: controllerListHistory,
      controllerCreateHistory: controllerCreateHistory,
      capabilitiesSnapshot: capabilitiesSnapshot,
      listExportableModels: listExportableModels,
      openRemoteSession: openRemoteSession,
      switchRemoteSession: switchRemoteSession,
      submitRemoteTask: submitRemoteTask,
      cancelRemoteTask: cancelRemoteTask,
      remoteTaskStatus: remoteTaskStatus,
      permissionList: permissionList,
      permissionDecide: permissionDecide,
      permissionDenyTask: permissionDenyTask,
      attachmentBegin: attachmentBegin,
      attachmentChunk: attachmentChunk,
      attachmentAbort: attachmentAbort,
      artifactIndex: artifactIndex,
      artifactPreview: artifactPreview,
      artifactDownloadBegin: artifactDownloadBegin,
      artifactDownloadChunk: artifactDownloadChunk,
      artifactDownloadAbort: artifactDownloadAbort,
      historyList: historyList,
      historyLoad: historyLoad,
      historyDelete: historyDelete,
      historyMarkOffline: historyMarkOffline,
      journalReplay: journalReplay,
      journalSnapshot: journalSnapshot,
      journalAcknowledge: journalAcknowledge,
    };
  };
})(typeof window !== "undefined" ? window : globalThis);
