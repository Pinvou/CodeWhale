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
      status: "idle",
      generation: null,
      pairingCode: null,
      expiresAtMs: null,
      deviceName: null,
      controllerDeviceName: null,
      workspaces: [],
      lastError: null,
    };
  }

  function createControllerConnection() {
    return {
      status: "idle",
      connectionId: null,
      controllerLeaseId: null,
      leaseEpoch: null,
      relayEpoch: null,
      generation: null,
      deviceName: null,
      lastError: null,
      // E2E-03：A 端远程会话身份与能力版本由 Rust 构造后投影到这里，视图不再
      // 自行拼装协议字段。
      sessionRef: null,
      localSessionId: null,
      mappingConfirmed: false,
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
      remoteProxyHistories: [],
      remoteProxyHistory: null,
      remoteProxyNotice: null,
      remoteProxyCapabilityRevisions: {},
      retiredControlledGenerations: [],
      retiredControllerLeases: [],
      controlledFenced: false,
      controllerFenced: false,
      controlledOpenIntent: null,
      controllerOpenIntent: null,
      controlledTerminatedOpenIntent: null,
      controllerTerminatedOpenIntent: null,
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

  function beginControlledIntent(state, openNewGeneration) {
    var next = bump(state, "controlled");
    if (openNewGeneration === true) {
      next.controlledOpenIntent = next.intents.controlled;
      next.controlledTerminatedOpenIntent = null;
    }
    return next;
  }

  function beginControllerIntent(state, openNewConnection) {
    var next = bump(state, "controller");
    if (openNewConnection === true) {
      next.controllerOpenIntent = next.intents.controller;
      next.controllerTerminatedOpenIntent = null;
    }
    return next;
  }

  function retireIdentity(items, identity) {
    if (!identity || items.indexOf(identity) >= 0) return items;
    return items.concat([identity]).slice(-32);
  }

  function leaseIdentity(connectionId, leaseEpoch) {
    return connectionId && leaseEpoch ? String(connectionId) + "\u0000" + String(leaseEpoch) : null;
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
    if (!["disabled", "reserving", "waiting_connection", "safety_handshake",
      "connected", "reconnecting", "refreshing", "stopped", "error"].includes(status)) return state;
    var generation = payload.generation ? String(payload.generation) : null;
    if (!CONTROLLED_TERMINAL[status] && !generation) return state;
    var current = state.controlledDevice;
    // Only the current Rust ControlledStatus enum may open a generation.
    var starting = status === "reserving" || status === "waiting_connection";
    var waiting = status === "waiting_connection";
    var pendingOpen = state.controlledOpenIntent === state.intents.controlled;
    if (CONTROLLED_TERMINAL[status] && !state.controlledFenced
      && current.generation && !generation && payload.intent == null) return state;
    if (state.controlledFenced && CONTROLLED_TERMINAL[status] && payload.intent == null && !pendingOpen) return state;
    if (payload.intent != null && payload.intent === state.controlledTerminatedOpenIntent) return state;
    if (generation && state.retiredControlledGenerations.indexOf(generation) >= 0) return state;
    if (state.controlledFenced && !CONTROLLED_TERMINAL[status]) {
      if (state.controlledOpenIntent !== state.intents.controlled
        || payload.intent !== state.controlledOpenIntent) return state;
    }
    if (generation && current.generation && generation !== current.generation) {
      if (!starting || payload.intent == null || payload.intent !== state.controlledOpenIntent) return state;
    }
    var next = shallowCopy(state);
    var startsNewGeneration = starting
      && generation
      && current.generation
      && generation !== current.generation;
    if (startsNewGeneration) {
      next.retiredControlledGenerations = retireIdentity(state.retiredControlledGenerations, current.generation);
      next.controlledSession = { sessionId: null, sessionRef: null, generation: generation };
      next.controlledTasks = [];
      next.controlledTranscript = [];
    }
    next.controlledDevice = {
      status: status,
      generation: generation || (starting ? current.generation : null),
      pairingCode: payload.pairingCode || (waiting ? current.pairingCode : null),
      expiresAtMs: payload.expiresAtMs || (waiting ? current.expiresAtMs : null),
      deviceName: payload.deviceName || current.deviceName,
      controllerDeviceName: payload.controllerDeviceName || current.controllerDeviceName,
      workspaces: Array.isArray(payload.workspaces) ? payload.workspaces.slice(0, 32) : current.workspaces,
      lastError: payload.error || null,
    };
    if (generation && !CONTROLLED_TERMINAL[status]) {
      next.controlledFenced = false;
      if (payload.intent != null && payload.intent === state.controlledOpenIntent) {
        next.controlledOpenIntent = null;
      }
    }
    if (CONTROLLED_TERMINAL[status]) {
      if (pendingOpen) next.controlledTerminatedOpenIntent = state.controlledOpenIntent;
      next.retiredControlledGenerations = retireIdentity(
        next.retiredControlledGenerations, current.generation || generation,
      );
      next.controlledFenced = true;
      next.controlledOpenIntent = null;
      next.controlledSession = { sessionId: null, sessionRef: null, generation: null };
      next.controlledTasks = [];
      next.controlledTranscript = [];
    }
    next.revision = state.revision + 1;
    return next;
  }

  function reconcileControlledOpenStatus(state, intent, envelope, expectedGeneration) {
    var payload = envelope || {};
    var generation = payload.generation ? String(payload.generation) : "";
    if (!state.controlledFenced || state.controlledTerminatedOpenIntent !== intent
      || state.intents.controlled !== intent || !generation
      || generation !== String(expectedGeneration || "")
      || state.retiredControlledGenerations.indexOf(generation) >= 0
      || ["reserving", "waiting_connection", "safety_handshake", "connected", "reconnecting", "refreshing"].indexOf(payload.status) < 0) {
      return state;
    }
    var candidate = shallowCopy(state);
    candidate.controlledOpenIntent = intent;
    candidate.controlledTerminatedOpenIntent = null;
    var applied = applyControlledStatus(candidate, Object.assign({}, payload, { intent: intent }));
    return applied === candidate ? state : applied;
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
    var connectionId = payload.connectionId ? String(payload.connectionId) : null;
    var leaseEpoch = payload.leaseEpoch === 0 || payload.leaseEpoch ? String(payload.leaseEpoch) : null;
    var controllerLeaseId = payload.controllerLeaseId ? String(payload.controllerLeaseId) : null;
    var relayEpoch = payload.relayEpoch ? String(payload.relayEpoch) : null;
    var generation = payload.generation ? String(payload.generation) : null;
    var current = state.controllerConnection;
    var identity = leaseIdentity(connectionId, leaseEpoch);
    var pendingOpen = state.controllerOpenIntent === state.intents.controller;
    if (CONTROLLER_TERMINAL[status] && current.connectionId && payload.intent == null
      && payload.authoritativeRead !== true
      && (!connectionId || !controllerLeaseId || !leaseEpoch || !relayEpoch || !generation)) return state;
    if (state.controllerFenced && CONTROLLER_TERMINAL[status] && payload.intent == null && !pendingOpen) return state;
    if (payload.intent != null && payload.intent === state.controllerTerminatedOpenIntent) return state;
    if (identity && state.retiredControllerLeases.indexOf(identity) >= 0) return state;
    if (state.controllerFenced && !CONTROLLER_TERMINAL[status]) {
      if (state.controllerOpenIntent !== state.intents.controller
        || payload.intent !== state.controllerOpenIntent) return state;
    }
    var liveStatus = status === "connected" || status === "reconnecting";
    if (liveStatus && (!connectionId || !controllerLeaseId || !leaseEpoch || !relayEpoch || !generation)) {
      return state;
    }
    if (!CONTROLLER_TERMINAL[status] && !liveStatus && current.connectionId) return state;
    if (
      connectionId &&
      current.connectionId &&
      connectionId !== current.connectionId
    ) {
      return state;
    }
    if (
      leaseEpoch &&
      current.leaseEpoch &&
      connectionId === current.connectionId &&
      leaseEpoch !== current.leaseEpoch
    ) {
      return state;
    }
    if (controllerLeaseId && current.controllerLeaseId && controllerLeaseId !== current.controllerLeaseId) return state;
    if (relayEpoch && current.relayEpoch && relayEpoch !== current.relayEpoch) return state;
    if (connectionId && connectionId === current.connectionId && leaseEpoch === current.leaseEpoch
      && current.generation && generation && generation !== current.generation) return state;
    var next = shallowCopy(state);
    next.controllerConnection = {
      status: status,
      connectionId: connectionId || current.connectionId,
      controllerLeaseId: controllerLeaseId || current.controllerLeaseId,
      leaseEpoch: leaseEpoch || current.leaseEpoch,
      relayEpoch: relayEpoch || current.relayEpoch,
      generation: generation || current.generation,
      deviceName: payload.deviceName || current.deviceName,
      lastError: payload.error || null,
      sessionRef: current.sessionRef,
      localSessionId: current.localSessionId,
      mappingConfirmed: current.mappingConfirmed === true,
      sessionId: current.sessionId,
      sessionMode: current.sessionMode,
      capabilityRevision: current.capabilityRevision,
      capabilityStatus: current.capabilityStatus,
      capabilityError: current.capabilityError,
      capabilityRequestId: current.capabilityRequestId,
      capabilityValidForMs: current.capabilityValidForMs,
      temporaryModelAvailable: current.temporaryModelAvailable === true,
    };
    if (identity && !CONTROLLER_TERMINAL[status]) {
      next.controllerFenced = false;
      if (payload.intent != null && payload.intent === state.controllerOpenIntent) {
        next.controllerOpenIntent = null;
      }
    }
    if (CONTROLLER_TERMINAL[status]) {
      if (pendingOpen) next.controllerTerminatedOpenIntent = state.controllerOpenIntent;
      next.retiredControllerLeases = retireIdentity(
        next.retiredControllerLeases,
        leaseIdentity(current.connectionId, current.leaseEpoch) || identity,
      );
      next.controllerFenced = true;
      next.controllerOpenIntent = null;
      next.remoteCapabilities = null;
      next.remoteProxyNotice = null;
      next.remoteProxyCapabilityRevisions = {};
      next.controllerConnection.connectionId = null;
      next.controllerConnection.controllerLeaseId = null;
      next.controllerConnection.leaseEpoch = null;
      next.controllerConnection.relayEpoch = null;
      next.controllerConnection.generation = null;
      // 终端状态清空会话身份：旧 sessionId 不得被下一次连接复用。
      next.controllerConnection.sessionRef = null;
      next.controllerConnection.localSessionId = null;
      next.controllerConnection.mappingConfirmed = false;
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

  function reconcileControllerOpenStatus(state, intent, envelope) {
    var payload = envelope || {};
    if (!state.controllerFenced || state.controllerTerminatedOpenIntent !== intent
      || state.intents.controller !== intent
      || (payload.status !== "connected" && payload.status !== "reconnecting")
      || !payload.connectionId || !payload.controllerLeaseId || !payload.leaseEpoch
      || !payload.relayEpoch || !payload.generation) return state;
    var candidate = shallowCopy(state);
    candidate.controllerOpenIntent = intent;
    candidate.controllerTerminatedOpenIntent = null;
    var applied = applyControllerStatus(candidate, Object.assign({}, payload, { intent: intent }));
    return applied === candidate ? state : applied;
  }

  /** Rust's authenticated projection event is an invalidation notice only.
   * Journal contents must be read from SessionStore by the RemoteProxy API;
   * transport events never become display text directly. */
  function applyRemoteProxyNotice(state, envelope) {
    var item = envelope && typeof envelope === "object" ? envelope : {};
    var live = state.controllerConnection;
    if (live.status !== "connected" && live.status !== "reconnecting") return state;
    var fields = ["connectionId", "controllerLeaseId", "leaseEpoch", "relayEpoch", "generation"];
    if (fields.some(function (field) {
      return !live[field] || !item[field] || String(item[field]) !== String(live[field]);
    })) return state;
    if (!/^remote-[A-Za-z0-9._:-]+$/.test(item.localSessionId || "")
      || !item.remoteSessionRef || !item.controlledSessionId
      || ["journal", "snapshot", "terminal", "capability"].indexOf(item.kind) < 0) return state;
    if (item.localSessionId !== live.localSessionId
      || item.remoteSessionRef !== live.sessionRef) return state;
    var detail = item.fields && typeof item.fields === "object" ? item.fields : {};
    var revision = Number(detail.capabilityRevision);
    if (item.kind === "capability" && (!Number.isSafeInteger(revision) || revision <= 0)) return state;
    var knownRevision = (state.remoteProxyCapabilityRevisions || {})[item.localSessionId] || 0;
    if (item.kind === "capability" && revision <= knownRevision) return state;
    var previous = state.remoteProxyNotice;
    if (previous && previous.localSessionId === item.localSessionId
      && previous.connectionId === item.connectionId
      && previous.leaseEpoch === String(item.leaseEpoch)
      && previous.kind === item.kind
      && previous.sequence === detail.sequence
      && previous.capabilityRevision === (item.kind === "capability" ? revision : null)) return state;
    var next = shallowCopy(state);
    next.controllerConnection = shallowCopy(live);
    next.controllerConnection.mappingConfirmed = true;
    next.remoteProxyNotice = {
      kind: item.kind,
      localSessionId: String(item.localSessionId),
      remoteSessionRef: String(item.remoteSessionRef),
      connectionId: String(item.connectionId),
      leaseEpoch: String(item.leaseEpoch),
      capabilityRevision: item.kind === "capability" ? revision : null,
      journalEpoch: typeof detail.journalEpoch === "string" ? detail.journalEpoch.slice(0, 128) : null,
      sequence: Number.isSafeInteger(detail.sequence) && detail.sequence >= 0 ? detail.sequence : null,
    };
    if (item.kind === "capability") {
      next.controllerConnection.capabilityRevision = revision;
      next.controllerConnection.capabilityStatus = "ready";
      next.controllerConnection.capabilityError = null;
      next.remoteProxyCapabilityRevisions = shallowCopy(state.remoteProxyCapabilityRevisions || {});
      next.remoteProxyCapabilityRevisions[item.localSessionId] = revision;
    }
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
    if (connection.controllerLeaseId && String(payload.controllerLeaseId || "") !== connection.controllerLeaseId) return state;
    if (String(payload.leaseEpoch || "") !== String(connection.leaseEpoch)) return state;
    if (connection.relayEpoch && String(payload.relayEpoch || "") !== connection.relayEpoch) return state;
    if (connection.generation && String(payload.remoteControlGeneration || "") !== connection.generation) return state;
    var revision = Number(payload.capabilityRevision);
    if (!Number.isSafeInteger(revision) || revision <= 0) return state;
    if (state.remoteCapabilities && revision <= Number(state.remoteCapabilities.capabilityRevision)) {
      return state;
    }
    var next = shallowCopy(state);
    next.remoteCapabilities = {
      connectionId: payload.connectionId,
      controllerLeaseId: payload.controllerLeaseId || null,
      leaseEpoch: payload.leaseEpoch,
      relayEpoch: payload.relayEpoch || null,
      remoteControlGeneration: payload.remoteControlGeneration || null,
      capabilityRevision: revision,
      modes: Array.isArray(payload.modes) ? payload.modes.filter(function (mode) {
        return mode === "chat" || mode === "code";
      }).slice(0, 2) : [],
      tools: capabilityDescriptors(payload.tools, "toolId", 256),
      skills: capabilityDescriptors(payload.skills, "skillId", 256),
      workspaces: capabilityDescriptors(payload.workspaces, "workspaceId", 16, true),
      expiresAtMs: typeof payload.expiresAtMs === "string" || Number.isSafeInteger(payload.expiresAtMs)
        ? payload.expiresAtMs : null,
    };
    next.revision = state.revision + 1;
    return next;
  }

  /** transcript 条目上限：只保留最近正文，避免无限增长。 */
  var MAX_TRANSCRIPT_ENTRIES = 200;

  /**
   * B-side UI projection for the authoritative local Session timeline.
   * A-side history is read only from SessionStore.RemoteProxy.
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
      var item = model && typeof model === "object" ? model : {};
      return {
        modelId: typeof item.modelId === "string" ? item.modelId.slice(0, 128) : "",
        displayName: typeof item.displayName === "string" ? item.displayName.slice(0, 256) : "",
        exportable: item.exportable === true,
        reason: typeof item.reason === "string" ? item.reason.slice(0, 128) : null,
        visionModelId: typeof item.visionModelId === "string" ? item.visionModelId.slice(0, 128) : null,
      };
    }).filter(function (item) { return item.modelId.length > 0; });
    next.revision = state.revision + 1;
    return next;
  }

  function capabilityDescriptors(entries, idField, limit, workspace) {
    if (!Array.isArray(entries)) return [];
    return entries.slice(0, limit).filter(function (entry) {
      return entry && typeof entry === "object"
        && typeof entry[idField] === "string" && !!entry[idField]
        && typeof entry.displayName === "string";
    }).map(function (entry) {
      var safe = { displayName: entry.displayName.slice(0, 256) };
      safe[idField] = entry[idField].slice(0, 128);
      if (workspace) safe.codeAllowed = entry.codeAllowed === true;
      return safe;
    });
  }

  function remoteProxyId(value) {
    return typeof value === "string" && /^remote-[A-Za-z0-9._:-]+$/.test(value)
      && value.length <= 128;
  }

  function projectionText(value, maximum) {
    return typeof value === "string" ? value.slice(0, maximum) : "";
  }

  function projectRemoteProxyList(state, records) {
    if (!Array.isArray(records)) return state;
    var next = shallowCopy(state);
    next.remoteProxyHistories = records.slice(0, 256).filter(function (item) {
      return item && remoteProxyId(item.localSessionId);
    }).map(function (item) {
      return {
        localSessionId: item.localSessionId,
        title: projectionText(item.title, 120),
        pinned: item.pinned === true,
        lastOpenedAt: projectionText(item.lastOpenedAt, 64) || null,
        projectedEntryCount: Number.isSafeInteger(item.projectedEntryCount)
          && item.projectedEntryCount >= 0 ? item.projectedEntryCount : 0,
      };
    });
    next.revision = state.revision + 1;
    return next;
  }

  function applyRemoteProxyBinding(state, envelope) {
    var payload = envelope || {};
    var current = state.controllerConnection;
    var localSessionId = String(payload.localSessionId || "");
    var remoteRef = String(payload.remoteSessionRef || "");
    if (current.status !== "connected" || state.controllerFenced
      || localSessionId.length > 128 || !/^remote-[A-Za-z0-9._:-]+$/.test(localSessionId)
      || !/^[A-Za-z0-9._:-]{1,128}$/.test(remoteRef)
      || !payload.sessionId || payload.mode !== "chat"
      || String(payload.connectionId || "") !== String(current.connectionId || "")
      || String(payload.controllerLeaseId || "") !== String(current.controllerLeaseId || "")
      || String(payload.leaseEpoch || "") !== String(current.leaseEpoch || "")
      || String(payload.relayEpoch || "") !== String(current.relayEpoch || "")
      || String(payload.remoteControlGeneration || "") !== String(current.generation || "")
      || !current.connectionId || !current.controllerLeaseId || !current.leaseEpoch
      || !current.relayEpoch || !current.generation) return state;
    if (current.sessionRef && current.sessionRef !== remoteRef) return state;
    var next = shallowCopy(state);
    next.controllerConnection = shallowCopy(current);
    next.controllerConnection.localSessionId = localSessionId;
    next.controllerConnection.sessionRef = remoteRef;
    next.controllerConnection.mappingConfirmed = payload.mappingConfirmed === true;
    next.controllerConnection.sessionId = String(payload.sessionId);
    next.controllerConnection.sessionMode = "chat";
    next.controllerConnection.capabilityRevision = Number.isSafeInteger(payload.capabilityRevision)
      && payload.capabilityRevision > 0 ? payload.capabilityRevision : null;
    next.controllerConnection.capabilityValidForMs = payload.capabilityValidForMs == null
      ? null : Number(payload.capabilityValidForMs);
    next.controllerConnection.capabilityError = payload.capabilityError || null;
    next.controllerConnection.capabilityRequestId = payload.capabilityRequestId || null;
    next.controllerConnection.capabilityStatus = next.controllerConnection.capabilityError
      ? "error" : (next.controllerConnection.capabilityRevision == null ? "idle" : "ready");
    next.controllerConnection.temporaryModelAvailable = payload.temporaryModelAvailable === true;
    next.revision = state.revision + 1;
    return next;
  }

  function matchesRemoteProxyCapabilityBinding(state, envelope) {
    var payload = envelope || {};
    var live = state.controllerConnection || {};
    return (live.status === "connected" || live.status === "reconnecting")
      && !!live.localSessionId && !!live.sessionRef && !!live.sessionId
      && !!live.connectionId && !!live.controllerLeaseId && !!live.leaseEpoch
      && !!live.relayEpoch && !!live.generation
      && String(payload.localSessionId || "") === live.localSessionId
      && String(payload.remoteSessionRef || "") === live.sessionRef
      && String(payload.sessionId || "") === live.sessionId
      && String(payload.connectionId || "") === live.connectionId
      && String(payload.controllerLeaseId || "") === live.controllerLeaseId
      && String(payload.leaseEpoch || "") === live.leaseEpoch
      && String(payload.relayEpoch || "") === live.relayEpoch
      && String(payload.remoteControlGeneration || "") === live.generation
      && /^[A-Za-z0-9._:-]{1,128}$/.test(String(payload.capabilityRequestId || ""));
  }

  function applyRemoteProxyCapabilityResult(state, envelope) {
    if (!matchesRemoteProxyCapabilityBinding(state, envelope)) return state;
    var payload = envelope;
    var current = state.controllerConnection;
    var errorCode = String(payload.capabilityError || "");
    var revision = Number(payload.capabilityRevision);
    var confirmed = payload.mappingConfirmed === true && !errorCode
      && Number.isSafeInteger(revision) && revision > 0;
    if (confirmed && current.capabilityRevision != null
      && revision < current.capabilityRevision) return state;
    var next = shallowCopy(state);
    next.controllerConnection = shallowCopy(current);
    // Mapping is a distinct B-authoritative fact. A renewal can fail after
    // the mapping was already confirmed; the capability still fails closed.
    next.controllerConnection.mappingConfirmed = payload.mappingConfirmed === true;
    next.controllerConnection.capabilityRequestId = payload.capabilityRequestId;
    next.controllerConnection.capabilityRevision = confirmed ? revision : null;
    next.controllerConnection.capabilityStatus = confirmed ? "ready" : "error";
    next.controllerConnection.capabilityError = confirmed
      ? null : (errorCode || "capability_snapshot_unavailable");
    next.controllerConnection.capabilityValidForMs = confirmed
      && Number.isSafeInteger(payload.capabilityValidForMs)
      && payload.capabilityValidForMs > 0 ? payload.capabilityValidForMs : null;
    next.controllerConnection.temporaryModelAvailable = confirmed
      && payload.temporaryModelAvailable === true;
    if (confirmed) {
      next.remoteProxyCapabilityRevisions = shallowCopy(state.remoteProxyCapabilityRevisions || {});
      next.remoteProxyCapabilityRevisions[payload.localSessionId] = Math.max(
        next.remoteProxyCapabilityRevisions[payload.localSessionId] || 0, revision,
      );
    }
    if (!confirmed) next.remoteCapabilities = null;
    next.revision = state.revision + 1;
    return next;
  }

  var PROJECTION_KINDS = {
    user_message: true, assistant_message: true, reasoning: true, tool: true,
    approval: true, status: true, error: true, attachment: true,
    artifact: true, snapshot_marker: true,
  };

  function projectRemoteProxyRead(state, record) {
    if (!record || !remoteProxyId(record.localSessionId)
      || !Array.isArray(record.entries) || record.entries.length > 512
      || !Number.isSafeInteger(record.pendingUnknownCount) || record.pendingUnknownCount < 0
      || !Array.isArray(record.unattemptedPending) || record.unattemptedPending.length > 128
      || record.unattemptedPending.some(function (item) {
        return !item || item.dispatchState !== "prepared_never_attempted"
          || !/^[A-Za-z0-9._:-]{1,128}$/.test(item.taskId || "")
          || !/^[A-Za-z0-9._:-]{1,128}$/.test(item.clientRequestId || "")
          || !/^[a-fA-F0-9]{64}$/.test(item.requestFingerprint || "");
      })) return state;
    var next = shallowCopy(state);
    next.remoteProxyHistory = {
      localSessionId: record.localSessionId,
      title: projectionText(record.title, 120),
      pinned: record.pinned === true,
      lastOpenedAt: projectionText(record.lastOpenedAt, 64) || null,
      authenticatedCursor: record.authenticatedCursor
        && typeof record.authenticatedCursor.journalEpoch === "string"
        && Number.isSafeInteger(record.authenticatedCursor.sequence)
        ? {
            journalEpoch: projectionText(record.authenticatedCursor.journalEpoch, 128),
            sequence: record.authenticatedCursor.sequence,
          } : null,
      pendingUnknownCount: record.pendingUnknownCount,
      unattemptedPending: record.unattemptedPending.map(function (item) {
        return {
          taskId: item.taskId,
          clientRequestId: item.clientRequestId,
          requestFingerprint: item.requestFingerprint,
          dispatchState: item.dispatchState,
        };
      }),
      entries: record.entries.filter(function (entry) {
        return entry && typeof entry.journalEpoch === "string"
          && Number.isSafeInteger(entry.sequence) && entry.sequence >= 0
          && typeof entry.eventId === "string" && PROJECTION_KINDS[entry.kind] === true;
      }).map(function (entry) {
        return {
          journalEpoch: projectionText(entry.journalEpoch, 128),
          sequence: entry.sequence,
          eventId: projectionText(entry.eventId, 128),
          kind: entry.kind,
          taskId: projectionText(entry.taskId, 128) || null,
          body: typeof entry.body === "string" ? entry.body.slice(0, 16384) : null,
          attachments: Array.isArray(entry.attachments) ? entry.attachments.slice(0, 16).map(function (attachment) {
            var value = attachment && typeof attachment === "object" ? attachment : {};
            return {
              id: projectionText(value.id, 128),
              displayName: projectionText(value.displayName, 256),
              contentType: projectionText(value.contentType, 128),
              byteLength: Number.isSafeInteger(value.byteLength) && value.byteLength >= 0 ? value.byteLength : 0,
              checksumSha256: /^[a-fA-F0-9]{64}$/.test(value.checksumSha256 || "")
                ? value.checksumSha256 : "",
            };
          }) : [],
        };
      }),
    };
    next.revision = state.revision + 1;
    return next;
  }

  function hideRemoteProxyHistory(state, localSessionId) {
    var next = shallowCopy(state);
    next.remoteProxyHistories = (state.remoteProxyHistories || []).filter(function (item) {
      return item.localSessionId !== localSessionId;
    });
    if (next.remoteProxyHistory && next.remoteProxyHistory.localSessionId === localSessionId) {
      next.remoteProxyHistory = null;
    }
    next.revision = state.revision + 1;
    return next;
  }

  /**
   * Task events are deduplicated by (taskId, status) and never regress a
   * committed terminal state.
   */
  function applyTaskEventTo(state, event, tasksField) {
    // Current B sources intentionally differ: the encrypted task_status
    // protocol uses snake_case, while local admitted/executor projections
    // emitted by the same Rust task service use camelCase.
    var payload = event || {};
    var taskIdValue = payload.taskId || payload.task_id;
    var statusValue = payload.status || payload.taskStatus || payload.task_status;
    var taskId = taskIdValue ? String(taskIdValue) : "";
    var status = statusValue ? String(statusValue) : "";
    if (!taskId || !status) return state;
    var connection = state.controllerConnection;
    var connectionId = payload.connectionId || payload.connection_id;
    if (connection.connectionId && connectionId && String(connectionId) !== String(connection.connectionId)) {
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
    reconcileControlledOpenStatus: reconcileControlledOpenStatus,
    applyControlledSession: applyControlledSession,
    applyControllerStatus: applyControllerStatus,
    reconcileControllerOpenStatus: reconcileControllerOpenStatus,
    applyRemoteProxyNotice: applyRemoteProxyNotice,
    applyRemoteProxyBinding: applyRemoteProxyBinding,
    matchesRemoteProxyCapabilityBinding: matchesRemoteProxyCapabilityBinding,
    applyRemoteProxyCapabilityResult: applyRemoteProxyCapabilityResult,
    applyCapabilityPending: applyCapabilityPending,
    applyCapabilityFailure: applyCapabilityFailure,
    applyCapabilities: applyCapabilities,
    applyControlledJournalEvent: applyControlledJournalEvent,
    applyModels: applyModels,
    projectRemoteProxyList: projectRemoteProxyList,
    projectRemoteProxyRead: projectRemoteProxyRead,
    hideRemoteProxyHistory: hideRemoteProxyHistory,
    applyControlledTaskEvent: applyControlledTaskEvent,
    applyTaskContent: applyTaskContent,
    matchesLiveContext: matchesLiveContext,
    isTaskTerminal: function (status) {
      return !!TASK_TERMINAL[status];
    },
  };
})(typeof window !== "undefined" ? window : globalThis);
