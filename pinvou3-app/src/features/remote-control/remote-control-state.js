/**
 * Pure desktop remote-control state for the Tauri bridge (DEV-20).
 *
 * The bridge is a late-result boundary, not a source of truth: every async
 * desktop action captures an intent sequence and every asynchronous completion
 * is dropped unless it still matches the current intent *and* the current
 * generation / lease context. Task events never regress a committed terminal
 * state, and capability snapshots are bound to connection + lease + revision.
 *
 * Classic script (loaded from index.html) so the Tauri bridge feature can use
 * it synchronously; exposed on `window` for the node contract tests.
 */
(function (root) {
  "use strict";

  var CONTROLLED_TERMINAL = { stopped: true, error: true };
  var CONTROLLER_TERMINAL = { idle: true, disconnected: true, error: true };
  var TASK_TERMINAL = {
    succeeded: true,
    failed: true,
    cancelled: true,
    interrupted: true,
    result_unknown: true,
  };

  function createControlledDevice() {
    return {
      status: "idle",
      generation: null,
      pairingCode: null,
      expiresAtMs: null,
      deviceName: null,
      workspaces: [],
      lastError: null,
    };
  }

  function createControllerConnection() {
    return {
      status: "idle",
      connectionId: null,
      leaseEpoch: null,
      deviceName: null,
      lastError: null,
    };
  }

  function createState() {
    return {
      controlledDevice: createControlledDevice(),
      controllerConnection: createControllerConnection(),
      remoteCapabilities: null,
      remoteModels: [],
      remoteHistories: [],
      tasks: [],
      intents: { controlled: 0, controller: 0 },
      revision: 0,
    };
  }

  function bump(state, kind) {
    var next = shallowCopy(state);
    next.intents = shallowCopy(state.intents);
    next.intents[kind] = (next.intents[kind] || 0) + 1;
    next.revision = state.revision + 1;
    return next;
  }

  function beginControlledIntent(state) {
    return bump(state, "controlled");
  }

  function beginControllerIntent(state) {
    return bump(state, "controller");
  }

  function shallowCopy(value) {
    var copy = {};
    if (!value) return copy;
    Object.keys(value).forEach(function (key) {
      copy[key] = value[key];
    });
    return copy;
  }

  function isCurrentIntent(state, kind, intent) {
    if (intent === null || intent === undefined) return true;
    return state.intents[kind] === intent;
  }

  /**
   * Applies one controlled-device (B) status projection. Dropped when the
   * intent is stale or when the envelope carries a generation the current
   * state already replaced.
   */
  function applyControlledStatus(state, envelope) {
    var payload = envelope || {};
    if (!isCurrentIntent(state, "controlled", payload.intent)) return state;
    var status = payload.status ? String(payload.status) : null;
    if (!status) return state;
    var generation = payload.generation ? String(payload.generation) : null;
    var current = state.controlledDevice;
    var starting = status === "registering" || status === "waiting_for_pairing";
    if (
      generation &&
      current.generation &&
      generation !== current.generation &&
      !starting &&
      !CONTROLLED_TERMINAL[status]
    ) {
      return state;
    }
    var next = shallowCopy(state);
    next.controlledDevice = {
      status: status,
      generation: generation || (starting ? current.generation : null),
      pairingCode: payload.pairingCode || (status === "waiting_for_pairing" ? current.pairingCode : null),
      expiresAtMs: payload.expiresAtMs || payload.pairingExpiresAtMs || null,
      deviceName: payload.deviceName || current.deviceName,
      workspaces: Array.isArray(payload.workspaces) ? payload.workspaces.slice(0, 32) : current.workspaces,
      lastError: payload.error || null,
    };
    next.revision = state.revision + 1;
    return next;
  }

  function applyControllerStatus(state, envelope) {
    var payload = envelope || {};
    if (!isCurrentIntent(state, "controller", payload.intent)) return state;
    var status = payload.status ? String(payload.status) : null;
    if (!status) return state;
    var connectionId = payload.connectionId ? String(payload.connectionId) : null;
    var leaseEpoch = payload.leaseEpoch === 0 || payload.leaseEpoch ? String(payload.leaseEpoch) : null;
    var current = state.controllerConnection;
    if (
      connectionId &&
      current.connectionId &&
      connectionId !== current.connectionId &&
      !CONTROLLER_TERMINAL[status]
    ) {
      return state;
    }
    if (
      leaseEpoch &&
      current.leaseEpoch &&
      connectionId === current.connectionId &&
      Number(leaseEpoch) < Number(current.leaseEpoch) &&
      !CONTROLLER_TERMINAL[status]
    ) {
      return state;
    }
    var next = shallowCopy(state);
    next.controllerConnection = {
      status: status,
      connectionId: connectionId || current.connectionId,
      leaseEpoch: leaseEpoch || current.leaseEpoch,
      deviceName: payload.deviceName || current.deviceName,
      lastError: payload.error || null,
    };
    if (CONTROLLER_TERMINAL[status]) {
      next.remoteCapabilities = null;
      next.tasks = [];
    }
    next.revision = state.revision + 1;
    return next;
  }

  /**
   * Capability snapshots are only accepted for the live connection + lease and
   * only when the revision moves forward.
   */
  function applyCapabilities(state, snapshot) {
    var payload = snapshot || {};
    var connection = state.controllerConnection;
    if (!connection.connectionId || connection.status !== "connected") return state;
    if (String(payload.connectionId || "") !== String(connection.connectionId)) return state;
    if (String(payload.leaseEpoch || "") !== String(connection.leaseEpoch)) return state;
    var revision = Number(payload.capabilityRevision);
    if (!isFinite(revision) || revision <= 0) return state;
    if (state.remoteCapabilities && revision <= Number(state.remoteCapabilities.capabilityRevision)) {
      return state;
    }
    var next = shallowCopy(state);
    next.remoteCapabilities = {
      connectionId: payload.connectionId,
      leaseEpoch: payload.leaseEpoch,
      relayEpoch: payload.relayEpoch || null,
      remoteControlGeneration: payload.remoteControlGeneration || null,
      capabilityRevision: revision,
      modes: Array.isArray(payload.modes) ? payload.modes.slice(0, 2) : [],
      tools: Array.isArray(payload.tools) ? payload.tools.slice(0, 256) : [],
      skills: Array.isArray(payload.skills) ? payload.skills.slice(0, 256) : [],
      workspaces: Array.isArray(payload.workspaces) ? payload.workspaces.slice(0, 16) : [],
      expiresAtMs: payload.expiresAtMs || null,
    };
    next.revision = state.revision + 1;
    return next;
  }

  /** Model descriptors are opaque: only IDs, names and reasons reach JS. */
  function applyModels(state, models) {
    if (!Array.isArray(models)) return state;
    var next = shallowCopy(state);
    next.remoteModels = models.slice(0, 128).map(function (model) {
      return {
        modelId: String(model && model.modelId ? model.modelId : ""),
        displayName: String(model && model.displayName ? model.displayName : ""),
        exportable: !!(model && model.exportable),
        reason: model && model.reason ? String(model.reason) : null,
        visionModelId: model && model.visionModelId ? String(model.visionModelId) : null,
      };
    });
    next.revision = state.revision + 1;
    return next;
  }

  function applyHistories(state, histories) {
    if (!Array.isArray(histories)) return state;
    var next = shallowCopy(state);
    next.remoteHistories = histories.slice(0, 64).map(function (record) {
      return {
        historyId: String(record && record.historyId ? record.historyId : ""),
        title: String(record && record.title ? record.title : ""),
        deviceDisplayName: String(record && record.deviceDisplayName ? record.deviceDisplayName : ""),
        state: String(record && record.state ? record.state : "offline_copy"),
        lastSequence: Number(record && record.lastSequence ? record.lastSequence : 0),
        lastTerminalStatus: record && record.lastTerminalStatus ? String(record.lastTerminalStatus) : null,
      };
    });
    next.revision = state.revision + 1;
    return next;
  }

  /**
   * Task events are deduplicated by (taskId, status) and never regress a
   * committed terminal state.
   */
  function applyTaskEvent(state, event) {
    var payload = event || {};
    var taskId = payload.taskId ? String(payload.taskId) : "";
    var status = payload.status ? String(payload.status) : "";
    if (!taskId || !status) return state;
    var connection = state.controllerConnection;
    if (connection.connectionId && payload.connectionId && String(payload.connectionId) !== String(connection.connectionId)) {
      return state;
    }
    var tasks = (state.tasks || []).slice();
    var index = -1;
    for (var i = 0; i < tasks.length; i += 1) {
      if (tasks[i].taskId === taskId) {
        index = i;
        break;
      }
    }
    if (index >= 0 && TASK_TERMINAL[tasks[index].status]) return state;
    var entry = {
      taskId: taskId,
      sessionId: payload.sessionId ? String(payload.sessionId) : (index >= 0 ? tasks[index].sessionId : null),
      status: status,
      outcomeUnknown: !!payload.outcomeUnknown,
      terminalReason: payload.terminalReason ? String(payload.terminalReason) : null,
      resultDigest: payload.resultDigest ? String(payload.resultDigest) : null,
    };
    if (index >= 0) {
      if (tasks[index].status === status) return state;
      tasks[index] = entry;
    } else {
      if (tasks.length >= 64) tasks.shift();
      tasks.push(entry);
    }
    var next = shallowCopy(state);
    next.tasks = tasks;
    next.revision = state.revision + 1;
    return next;
  }

  /** True when the envelope still belongs to the live connection context. */
  function matchesLiveContext(state, envelope) {
    var payload = envelope || {};
    var connection = state.controllerConnection;
    if (!connection.connectionId) return false;
    if (payload.connectionId && String(payload.connectionId) !== String(connection.connectionId)) return false;
    if (
      payload.leaseEpoch !== undefined &&
      payload.leaseEpoch !== null &&
      String(payload.leaseEpoch) !== String(connection.leaseEpoch)
    ) {
      return false;
    }
    return true;
  }

  root.__PINVOU_REMOTE_CONTROL_STATE__ = {
    createState: createState,
    beginControlledIntent: beginControlledIntent,
    beginControllerIntent: beginControllerIntent,
    isCurrentIntent: isCurrentIntent,
    applyControlledStatus: applyControlledStatus,
    applyControllerStatus: applyControllerStatus,
    applyCapabilities: applyCapabilities,
    applyModels: applyModels,
    applyHistories: applyHistories,
    applyTaskEvent: applyTaskEvent,
    matchesLiveContext: matchesLiveContext,
    isTaskTerminal: function (status) {
      return !!TASK_TERMINAL[status];
    },
  };
})(typeof window !== "undefined" ? window : globalThis);
