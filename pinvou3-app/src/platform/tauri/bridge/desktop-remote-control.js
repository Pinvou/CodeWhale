/**
 * Windows desktop dual-end remote control bridge (DEV-20).
 *
 * This is a separate domain from the existing `webAccess` WebUI share:
 * `controlledDevice` (B), `controllerConnection` + `remoteCapabilities` +
 * `remoteModels` + `remoteHistories` (A) live here, and every async completion
 * is gated by the intent sequence and generation/lease epoch held in
 * `features/remote-control/remote-control-state.js`.
 *
 * A07 exposes lifecycle/capability/status and B-local stop adapters. Model export,
 * permission decisions and history mutation are deliberately absent from the
 * renderer surface until A08 binds them to authenticated Rust transport.
 * Transfer inputs also stay closed until a bounded task/receipt consumer owns
 * completed files; a successful staging write alone is not task delivery.
 * Task/session admission is also closed until authenticated Rust dispatch.
 * These kernels are not claimed as end-to-end product functionality here.
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
      listen("desktop-remote-controller-status", function (event) {
        var payload = event && event.payload ? event.payload : event || {};
        publish(logic.applyControllerStatus(current(), payload));
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

    function applyControlled(intent, result, replaceBinding) {
      var payload = shallow(result);
      payload.intent = intent;
      payload.replaceBinding = replaceBinding === true;
      publish(logic.applyControlledStatus(current(), payload));
      return result;
    }

    function applyController(intent, result, replaceBinding) {
      var payload = shallow(result);
      payload.intent = intent;
      payload.replaceBinding = replaceBinding === true;
      publish(logic.applyControllerStatus(current(), payload));
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
        return applyControlled(intent, result, true);
      });
    }

    function controlledRefresh() {
      ensureAvailable();
      subscribe();
      var intent = controlledIntent();
      return invoke("desktop_remote_controlled_refresh", {}).then(function (result) {
        return applyControlled(intent, result, true);
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
      var intent = current().intents.controlled;
      var revision = current().revision;
      return invoke("desktop_remote_controlled_status", {}).then(function (result) {
        if (current().revision !== revision) return result;
        return applyControlled(intent, result);
      });
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
      subscribe();
      var intent = controllerIntent();
      return invoke("desktop_remote_controller_connect", { code: code }).then(function (result) {
        return applyController(intent, result, true);
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
      subscribe();
      var intent = current().intents.controller;
      var revision = current().revision;
      return invoke("desktop_remote_controller_status", {}).then(function (result) {
        if (current().revision !== revision) return result;
        return applyController(intent, result);
      });
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
      cancelRemoteTask: cancelRemoteTask,
      remoteTaskStatus: remoteTaskStatus,
      journalReplay: journalReplay,
      journalSnapshot: journalSnapshot,
      journalAcknowledge: journalAcknowledge,
    };
  };
})(typeof window !== "undefined" ? window : globalThis);
