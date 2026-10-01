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

  var CONTROLLED_TERMINAL = { disabled: true, stopped: true, error: true };
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
      status: "disabled",
      generation: null,
      statusIntent: null,
      pairingCode: null,
      expiresAtMs: null,
      deviceName: null,
      controllerDeviceName: null,
      workspaceCount: 0,
      codeModeAllowed: false,
      workspaces: [],
      lastError: null,
    };
  }

  function createControllerConnection() {
    return {
      status: "idle",
      connectionId: null,
      leaseEpoch: null,
      claimRequestId: null,
      controllerLeaseId: null,
      relayEpoch: null,
      remoteControlGeneration: null,
      deviceName: null,
      lastError: null,
      // E2E-03：A 端远程会话身份与能力版本由 Rust 构造后投影到这里，视图不再
      // 自行拼装协议字段。
      sessionRef: null,
      sessionId: null,
      sessionMode: null,
      capabilityRevision: null,
      capabilityStatus: "idle",
      capabilityError: null,
      capabilityRequestId: null,
      capabilityValidForMs: null,
      temporaryModelAvailable: false,
    };
  }

  function createState() {
    return {
      controlledDevice: createControlledDevice(),
      controlledSession: {
        sessionId: null,
        sessionRef: null,
        generation: null,
      },
      controllerConnection: createControllerConnection(),
      remoteCapabilities: null,
      remoteModels: [],
      remoteHistories: [],
      tasks: [],
      transcript: [],
      controlledTasks: [],
      controlledTranscript: [],
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
    // Normalize legacy spellings before applying the same generation guards.
    if (status === "registering") status = "reserving";
    if (status === "waiting_for_pairing") status = "waiting_connection";
    var replaceBinding = payload.replaceBinding === true && payload.intent !== undefined;
    var statusIntent = payload.statusIntent === undefined ? null : String(payload.statusIntent);
    if (statusIntent && !/^(0|[1-9][0-9]*)$/.test(statusIntent)) return state;
    if (statusIntent && current.statusIntent && BigInt(statusIntent) < BigInt(current.statusIntent)) return state;
    if (current.generation && !generation && !replaceBinding) return state;
    var starting = status === "reserving" || status === "waiting_connection";
    if (
      generation &&
      current.generation &&
      generation !== current.generation &&
      !replaceBinding &&
      !(current.status === "refreshing" && status === "reserving")
    ) {
      return state;
    }
    if (!replaceBinding && CONTROLLED_TERMINAL[current.status] && current.generation &&
        !CONTROLLED_TERMINAL[status]) return state;
    if (generation === current.generation && current.status === "waiting_connection" && status === "reserving") return state;
    if (generation === current.generation &&
        (current.status === "connected" || current.status === "reconnecting" ||
         current.status === "safety_handshake" || current.status === "refreshing") && starting) return state;
    var next = shallowCopy(state);
    var startsNewGeneration = starting
      && generation
      && current.generation
      && generation !== current.generation;
    if (startsNewGeneration) {
      next.controlledSession = { sessionId: null, sessionRef: null, generation: generation };
      next.controlledTasks = [];
      next.controlledTranscript = [];
    }
    // Partial waiting updates may retain a code only within the same binding.
    // Explicit null, a new status intent or a terminal status clears it.
    var retainPairing = status === "waiting_connection" && current.status === status &&
      generation === current.generation && statusIntent === current.statusIntent && !replaceBinding;
    var expiry = payload.pairingExpiresAtMs === undefined ? payload.expiresAtMs : payload.pairingExpiresAtMs;
    next.controlledDevice = {
      status: status,
      generation: generation || (starting ? current.generation : null),
      statusIntent: statusIntent || current.statusIntent,
      pairingCode: CONTROLLED_TERMINAL[status] ? null :
        (payload.pairingCode === undefined && retainPairing ? current.pairingCode : payload.pairingCode || null),
      expiresAtMs: CONTROLLED_TERMINAL[status] ? null :
        (expiry === undefined && retainPairing ? current.expiresAtMs : expiry || null),
      deviceName: payload.deviceName || null,
      controllerDeviceName: CONTROLLED_TERMINAL[status] ? null : payload.controllerDeviceName || null,
      workspaceCount: Number(payload.workspaceCount) || 0,
      codeModeAllowed: payload.codeModeAllowed === true,
      workspaces: Array.isArray(payload.workspaces) ? payload.workspaces.slice(0, 32) : current.workspaces,
      lastError: payload.errorCode || null,
    };
    if (CONTROLLED_TERMINAL[status]) {
      next.controlledSession = { sessionId: null, sessionRef: null, generation: null };
      next.controlledTasks = [];
      next.controlledTranscript = [];
    }
    next.revision = state.revision + 1;
    return next;
  }

  /** Bind B's visible composer and transcript to the authoritative session
   * created for the current controlled generation. */
  function applyControlledSession(state, envelope) {
    var payload = envelope || {};
    var sessionId = payload.sessionId ? String(payload.sessionId) : "";
    var sessionRef = payload.sessionRef ? String(payload.sessionRef) : "";
    var generation = payload.generation ? String(payload.generation) : "";
    var currentGeneration = String((state.controlledDevice && state.controlledDevice.generation) || "");
    var status = state.controlledDevice && state.controlledDevice.status;
    if (!sessionId || !sessionRef || !generation) return state;
    if (status !== "connected" && status !== "reconnecting") return state;
    if (!currentGeneration || generation !== currentGeneration) return state;
    var current = state.controlledSession || {};
    if (
      current.sessionId === sessionId
      && current.sessionRef === sessionRef
      && current.generation === generation
    ) return state;
    var next = shallowCopy(state);
    next.controlledSession = {
      sessionId: sessionId,
      sessionRef: sessionRef,
      generation: generation,
    };
    next.controlledTasks = [];
    next.controlledTranscript = [];
    next.revision = state.revision + 1;
    return next;
  }

  function applyControllerStatus(state, envelope) {
    var payload = envelope || {};
    if (!isCurrentIntent(state, "controller", payload.intent)) return state;
    var status = payload.status ? String(payload.status) : null;
    if (!status) return state;
    var lease = payload.lease || {};
    var connectionId = lease.connectionId ? String(lease.connectionId) : null;
    var leaseEpoch = lease.leaseEpoch === 0 || lease.leaseEpoch ? String(lease.leaseEpoch) : null;
    if (leaseEpoch && !/^(0|[1-9][0-9]*)$/.test(leaseEpoch)) return state;
    var claimRequestId = payload.claimRequestId || null;
    var current = state.controllerConnection;
    var newClaim = claimRequestId && claimRequestId !== current.claimRequestId;
    var replaceBinding = payload.replaceBinding === true && payload.intent !== undefined;
    if (newClaim && current.claimRequestId && !replaceBinding) return state;
    if (!newClaim && CONTROLLER_TERMINAL[current.status] && current.claimRequestId &&
        !CONTROLLER_TERMINAL[status]) return state;
    // Only a fresh explicit connect intent can replace the previous binding.
    if (
      connectionId &&
      current.connectionId &&
      connectionId !== current.connectionId &&
      !(newClaim && replaceBinding)
    ) {
      return state;
    }
    if (
      leaseEpoch &&
      current.leaseEpoch &&
      connectionId === current.connectionId &&
      BigInt(leaseEpoch) < BigInt(current.leaseEpoch)
    ) {
      return state;
    }
    if (!newClaim && (current.status === "connected" || current.status === "reconnecting") &&
        (status === "connecting" || status === "safety_handshake")) return state;
    if (!newClaim && current.status === "safety_handshake" && status === "connecting") return state;
    if (!newClaim && connectionId && current.connectionId &&
        ["controllerLeaseId", "relayEpoch", "remoteControlGeneration"].some(function (key) {
          return String(lease[key] || "") !== String(current[key] || "");
        })) return state;
    var retained = status === "reconnecting" && !newClaim ? current : {};
    var next = shallowCopy(state);
    next.controllerConnection = {
      status: status,
      connectionId: connectionId || retained.connectionId || null,
      leaseEpoch: leaseEpoch || retained.leaseEpoch || null,
      claimRequestId: claimRequestId,
      controllerLeaseId: lease.controllerLeaseId || retained.controllerLeaseId || null,
      relayEpoch: lease.relayEpoch || retained.relayEpoch || null,
      remoteControlGeneration: lease.remoteControlGeneration || retained.remoteControlGeneration || null,
      deviceName: payload.deviceDisplayName || null,
      lastError: payload.errorCode || null,
      sessionRef: current.sessionRef,
      sessionId: current.sessionId,
      sessionMode: current.sessionMode,
      capabilityRevision: current.capabilityRevision,
      capabilityStatus: current.capabilityStatus,
      capabilityError: current.capabilityError,
      capabilityRequestId: current.capabilityRequestId,
      capabilityValidForMs: current.capabilityValidForMs,
      temporaryModelAvailable: current.temporaryModelAvailable === true,
    };
    if (CONTROLLER_TERMINAL[status] || newClaim ||
        (leaseEpoch && current.leaseEpoch && leaseEpoch !== current.leaseEpoch)) {
      next.remoteCapabilities = null;
    }
    if (CONTROLLER_TERMINAL[status] || newClaim ||
        (leaseEpoch && current.leaseEpoch && leaseEpoch !== current.leaseEpoch)) {
      next.tasks = [];
      // 终端状态清空会话身份：旧 sessionId 不得被下一次连接复用。
      next.controllerConnection.sessionRef = null;
      next.controllerConnection.sessionId = null;
      next.controllerConnection.sessionMode = null;
      next.controllerConnection.capabilityRevision = null;
      next.controllerConnection.capabilityStatus = "idle";
      next.controllerConnection.capabilityError = null;
      next.controllerConnection.capabilityRequestId = null;
      next.controllerConnection.capabilityValidForMs = null;
      next.controllerConnection.temporaryModelAvailable = false;
    }
    next.revision = state.revision + 1;
    return next;
  }

  /**
   * Applies the A-side remote session projection (E2E-03). The projection is
   * produced by Rust and carries the session identity bound to the live lease;
   * the full lease tuple must match exactly while connected. Reconnecting keeps
   * the existing session but cannot adopt an in-flight projection.
   */
  function applyControllerSession(state, envelope) {
    var payload = envelope || {};
    var sessionRef = payload.sessionRef ? String(payload.sessionRef) : "";
    var sessionId = payload.sessionId ? String(payload.sessionId) : "";
    if (!sessionRef || !sessionId) return state;
    var current = state.controllerConnection;
    if (current.status !== "connected") return state;
    if (["connectionId", "controllerLeaseId", "leaseEpoch", "relayEpoch", "remoteControlGeneration"].some(function (key) {
      return !payload[key] || !current[key] || String(payload[key]) !== String(current[key]);
    })) return state;
    var revision = payload.capabilityRevision == null ? null : Number(payload.capabilityRevision);
    var next = shallowCopy(state);
    next.controllerConnection = shallowCopy(current);
    next.controllerConnection.sessionRef = sessionRef;
    next.controllerConnection.sessionId = sessionId;
    next.controllerConnection.sessionMode = payload.mode ? String(payload.mode) : current.sessionMode;
    next.controllerConnection.capabilityRevision = revision;
    next.controllerConnection.capabilityError = payload.capabilityError
      ? String(payload.capabilityError)
      : null;
    next.controllerConnection.capabilityRequestId = payload.capabilityRequestId
      ? String(payload.capabilityRequestId)
      : null;
    next.controllerConnection.capabilityValidForMs = payload.capabilityValidForMs == null
      ? null
      : Number(payload.capabilityValidForMs);
    next.controllerConnection.temporaryModelAvailable = payload.temporaryModelAvailable === true;
    next.controllerConnection.capabilityStatus = next.controllerConnection.capabilityError
      ? "error"
      : (revision != null ? "ready" : "idle");
    next.revision = state.revision + 1;
    return next;
  }

  function applyCapabilityPending(state, envelope) {
    var payload = envelope || {};
    var current = state.controllerConnection;
    if (!current.sessionRef || String(payload.sessionRef || "") !== String(current.sessionRef)) return state;
    var next = shallowCopy(state);
    next.controllerConnection = shallowCopy(current);
    next.controllerConnection.capabilityStatus = "preparing";
    next.controllerConnection.capabilityError = null;
    next.revision = state.revision + 1;
    return next;
  }

  function applyCapabilityFailure(state, envelope) {
    var payload = envelope || {};
    var current = state.controllerConnection;
    if (!current.sessionRef || String(payload.sessionRef || "") !== String(current.sessionRef)) return state;
    var next = shallowCopy(state);
    next.controllerConnection = shallowCopy(current);
    next.controllerConnection.capabilityRevision = null;
    next.controllerConnection.capabilityStatus = "error";
    next.controllerConnection.capabilityError = String(payload.code || "capability_snapshot_unavailable");
    next.controllerConnection.capabilityRequestId = payload.requestId ? String(payload.requestId) : null;
    next.controllerConnection.capabilityValidForMs = null;
    next.remoteCapabilities = null;
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
    if (["controllerLeaseId", "relayEpoch", "remoteControlGeneration"].some(function (key) {
      return String(payload[key] || "") !== String(connection[key] || "");
    })) return state;
    var revision = Number(payload.capabilityRevision);
    if (!isFinite(revision) || revision <= 0) return state;
    if (state.remoteCapabilities && revision <= Number(state.remoteCapabilities.capabilityRevision)) {
      return state;
    }
    var next = shallowCopy(state);
    next.remoteCapabilities = {
      connectionId: payload.connectionId,
      controllerLeaseId: payload.controllerLeaseId,
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

  /** transcript 条目上限：只保留最近正文，避免无限增长。 */
  var MAX_TRANSCRIPT_ENTRIES = 200;

  /**
   * E2E-05/E2E-07: projects one authoritative journal envelope (event or
   * snapshot) into the A-side transcript. Entries are deduplicated by
   * session+epoch+sequence and every payload is treated as plain text.
   */
  function applyJournalEventTo(state, envelope, transcriptField) {
    var payload = envelope || {};
    var transcript = state[transcriptField] || [];
    if (payload.snapshot) {
      var snapshot = payload.snapshot;
      var events = Array.isArray(snapshot.events) ? snapshot.events : [];
      var sessionId = events.length ? String(events[0].sessionId || "") : "";
      if (!sessionId) return state;
      var entries = events.map(toTranscriptEntry).filter(Boolean);
      var kept = transcript.filter(function (entry) {
        return entry.sessionId !== sessionId;
      });
      var next = shallowCopy(state);
      next[transcriptField] = kept.concat(entries).slice(-MAX_TRANSCRIPT_ENTRIES);
      next.revision = state.revision + 1;
      return next;
    }
    var entry = toTranscriptEntry(payload.event);
    if (!entry) return state;
    var current = transcript;
    for (var index = 0; index < current.length; index += 1) {
      if (
        current[index].sessionId === entry.sessionId &&
        current[index].epoch === entry.epoch &&
        current[index].sequence === entry.sequence
      ) {
        return state;
      }
    }
    var list = current.concat([entry]);
    if (list.length > MAX_TRANSCRIPT_ENTRIES) {
      list = list.slice(list.length - MAX_TRANSCRIPT_ENTRIES);
    }
    var updated = shallowCopy(state);
    updated[transcriptField] = list;
    updated.revision = state.revision + 1;
    return updated;
  }

  function applyJournalEvent(state, envelope) {
    return applyJournalEventTo(state, envelope, "transcript");
  }

  function applyControlledJournalEvent(state, envelope) {
    var event = envelope && envelope.event;
    var session = state.controlledSession || {};
    if (!event || !session.sessionId || String(event.sessionId || "") !== String(session.sessionId)) {
      return state;
    }
    return applyJournalEventTo(state, envelope, "controlledTranscript");
  }

  /** User, assistant and reasoning text become transcript entries; everything
   * else keeps its existing task/status projection. */
  function toTranscriptEntry(event) {
    if (!event || typeof event !== "object") return null;
    var kind = event.kind ? String(event.kind) : "";
    var role = kind === "user_message"
      ? "user"
      : (kind === "assistant_delta" ? "assistant" : (kind === "reasoning" ? "reasoning" : ""));
    if (!role) return null;
    var text = event.payload && event.payload.text ? String(event.payload.text) : "";
    if (!text) return null;
    return {
      sessionId: String(event.sessionId || ""),
      epoch: String(event.epoch || ""),
      sequence: Number(event.sequence || 0),
      taskId: event.taskId ? String(event.taskId) : null,
      role: role,
      text: text.slice(0, 8192),
    };
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
  function applyTaskEventTo(state, event, tasksField) {
    var payload = event || {};
    var taskIdValue = payload.taskId || payload.task_id;
    var statusValue = payload.status || payload.taskStatus || payload.task_status;
    var taskId = taskIdValue ? String(taskIdValue) : "";
    var status = statusValue ? String(statusValue) : "";
    if (!taskId || !status) return state;
    var connection = state.controllerConnection;
    var connectionId = payload.connectionId || payload.connection_id;
    // A08/A11 must add the full lease tuple to RemoteTaskEvent. Until then,
    // this local projection cannot identify old unbound events delivered after
    // reconnect; it can reject the offline window, not claim wire isolation.
    if (tasksField === "tasks" && connection.status !== "connected") return state;
    if (tasksField === "tasks" && connection.connectionId && connectionId && String(connectionId) !== String(connection.connectionId)) {
      return state;
    }
    var tasks = (state[tasksField] || []).slice();
    var index = -1;
    for (var i = 0; i < tasks.length; i += 1) {
      if (tasks[i].taskId === taskId) {
        index = i;
        break;
      }
    }
    if (index >= 0 && TASK_TERMINAL[tasks[index].status]) return state;
    // An admitted acknowledgement may arrive after the executor's running
    // event on a multi-thread runtime. Keep the newer lifecycle state while
    // still accepting prompt text that only the admitted projection carries.
    if (index >= 0 && status === "admitted" && tasks[index].status !== "admitted") {
      var lateText = payload.text ? String(payload.text).slice(0, 8192) : null;
      if (!lateText || tasks[index].text) return state;
      tasks[index] = Object.assign({}, tasks[index], { text: lateText });
      var promptUpdated = shallowCopy(state);
      promptUpdated[tasksField] = tasks;
      promptUpdated.revision = state.revision + 1;
      return promptUpdated;
    }
    var entry = {
      taskId: taskId,
      sessionId: (payload.sessionId || payload.session_id)
        ? String(payload.sessionId || payload.session_id)
        : (index >= 0 ? tasks[index].sessionId : null),
      status: status,
      text: payload.text
        ? String(payload.text).slice(0, 8192)
        : (index >= 0 ? tasks[index].text || null : null),
      messageIndex: Number.isInteger(payload.messageIndex)
        ? payload.messageIndex
        : (index >= 0 ? tasks[index].messageIndex : null),
      assistantText: index >= 0 ? tasks[index].assistantText || null : null,
      errorText: index >= 0 ? tasks[index].errorText || null : null,
      contentEventIds: index >= 0 ? (tasks[index].contentEventIds || []).slice() : [],
      outcomeUnknown: !!(payload.outcomeUnknown || payload.outcome_unknown),
      terminalReason: (payload.terminalReason || payload.terminal_reason)
        ? String(payload.terminalReason || payload.terminal_reason)
        : null,
      resultDigest: (payload.resultDigest || payload.result_digest)
        ? String(payload.resultDigest || payload.result_digest)
        : null,
    };
    if (index >= 0) {
      if (
        tasks[index].status === status
        && tasks[index].text === entry.text
        && tasks[index].messageIndex === entry.messageIndex
        && tasks[index].assistantText === entry.assistantText
        && tasks[index].errorText === entry.errorText
        && tasks[index].outcomeUnknown === entry.outcomeUnknown
        && tasks[index].terminalReason === entry.terminalReason
        && tasks[index].resultDigest === entry.resultDigest
      ) return state;
      tasks[index] = entry;
    } else {
      if (tasks.length >= 64) tasks.shift();
      tasks.push(entry);
    }
    var next = shallowCopy(state);
    next[tasksField] = tasks;
    next.revision = state.revision + 1;
    return next;
  }

  function applyTaskEvent(state, event) {
    return applyTaskEventTo(state, event, "tasks");
  }

  function applyControlledTaskEvent(state, event) {
    var payload = event || {};
    var session = state.controlledSession || {};
    var sessionId = payload.sessionId || payload.session_id;
    if (!session.sessionId || String(sessionId || "") !== String(session.sessionId)) {
      return state;
    }
    return applyTaskEventTo(state, payload, "controlledTasks");
  }

  /**
   * Projects authoritative B-side task text into the same durable task entry
   * used by status events. Admission text is visible immediately; assistant
   * output remains visible after the task reaches a terminal state.
   */
  function applyTaskContent(state, event) {
    var payload = event || {};
    var taskId = payload.taskId ? String(payload.taskId) : "";
    var role = payload.role ? String(payload.role) : "";
    var text = payload.text ? String(payload.text).slice(0, 8192) : "";
    var eventId = payload.eventId ? String(payload.eventId) : "";
    if (!taskId || !eventId || !text || (role !== "user" && role !== "assistant" && role !== "error")) {
      return state;
    }
    var session = state.controlledSession || {};
    if (!session.sessionId || String(payload.sessionId || "") !== String(session.sessionId)) {
      return state;
    }
    var tasks = (state.controlledTasks || []).slice();
    var index = tasks.findIndex(function (task) { return task && task.taskId === taskId; });
    if (index < 0) return state;
    var current = tasks[index];
    if (payload.sessionId && current.sessionId && String(payload.sessionId) !== current.sessionId) {
      return state;
    }
    var nextEntry = shallowCopy(current);
    var contentEventIds = (current.contentEventIds || []).slice();
    if (contentEventIds.indexOf(eventId) >= 0) return state;
    if (contentEventIds.length >= 32) contentEventIds.shift();
    contentEventIds.push(eventId);
    nextEntry.contentEventIds = contentEventIds;
    if (role === "user") {
      nextEntry.text = text;
    } else if (role === "assistant") {
      nextEntry.assistantText = String(current.assistantText || "") + text;
    } else {
      nextEntry.errorText = String(current.errorText || "") + text;
    }
    tasks[index] = nextEntry;
    var next = shallowCopy(state);
    next.controlledTasks = tasks;
    next.revision = state.revision + 1;
    return next;
  }

  /** True when the envelope still belongs to the live connection context. */
  function matchesLiveContext(state, envelope) {
    var payload = envelope || {};
    var connection = state.controllerConnection;
    if (!connection.connectionId || connection.status !== "connected") return false;
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
    applyControlledSession: applyControlledSession,
    applyControllerStatus: applyControllerStatus,
    applyControllerSession: applyControllerSession,
    applyCapabilityPending: applyCapabilityPending,
    applyCapabilityFailure: applyCapabilityFailure,
    applyCapabilities: applyCapabilities,
    applyJournalEvent: applyJournalEvent,
    applyControlledJournalEvent: applyControlledJournalEvent,
    applyModels: applyModels,
    applyHistories: applyHistories,
    applyTaskEvent: applyTaskEvent,
    applyControlledTaskEvent: applyControlledTaskEvent,
    applyTaskContent: applyTaskContent,
    matchesLiveContext: matchesLiveContext,
    isTaskTerminal: function (status) {
      return !!TASK_TERMINAL[status];
    },
  };
})(typeof window !== "undefined" ? window : globalThis);
