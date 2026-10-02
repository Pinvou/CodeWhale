/**
 * aux-chat feature for the Tauri bridge.
 * Registered before bridge.js builds the backwards-compatible facade.
 *
 * "Aux chat" bridge: each task (taskId) gets one background aux chat session
 * (id derived as aux-<taskId>). Aux sessions are
 * filtered out of list_sessions
 * by the backend (they never enter state.sessions), so the chat domain's
 * sendMessageToSession (which requires sid ∈ state.sessions) cannot be reused.
 * This domain is a thin wrapper: session creation/loading reuses the sessions
 * domain's per-session buffer path, and turn events are still routed into the
 * buffer by chat-events keyed on session_id (this domain adds no event listeners).
 *
 * Official-lane note (community #433 port): upstream's M7 refactor moved the
 * lane-identical domain bodies into the shared cluster "auxChat" of
 * src/shared/bridge-shared-helpers.js (window.PinvouBridgeShared). Official
 * has no shared-cluster mechanism (no PinvouBridgeShared global and no
 * index.html loader for one), so the domain bodies live verbatim in this
 * module, mirroring Official's self-contained bridge/<feature>.js pattern.
 * Only the genuine lane difference stays here as well — the desktop send
 * dispatch goes through chat with an empty attachments list; Web goes through
 * web_access_chat with attachment handles (see platform/web/bridge.js).
 * The web-only session_turn_in_progress translation wrapper was dropped with
 * the upstream move (M7): send errors only reach console.warn and the panel's
 * static localized sendFailed copy on either lane, so the translated text
 * never reached the user.
 */
(function (root) {
  "use strict";
  var registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};
  registry.auxChat = function (context) {
    var state = context.state;
    var invoke = context.invoke;
    var bt = context.bt;
    var sessionStates = context.sessionStates;
    var ensureSessionBufferLoaded = context.ensureSessionBufferLoaded;
    // Passed bare like the web lane (round-32 review minor 12): a fallback
    // no-op would silently skip buffer eviction/LRU touches on a future
    // injection miss, while a missing function throws loudly at the first call.
    var purgeSessionBuffer = context.purgeSessionBuffer;
    var touchSessionBuffer = context.touchSessionBuffer;
    var isBusyFor = context.isBusyFor;

    // Lane-specific send dispatch (M7); every other line of the domain is
    // shared verbatim with the web lane.
    function auxChatDispatch(sid, message) {
      return invoke("chat", { message, attachments: [], sessionId: sid, restrictTools: true });
    }

    // Display-only and case-sensitive ON PURPOSE: it only gates ensure/send
    // inputs on ids the backend itself minted (always lowercase `aux-`);
    // identity and destructive paths decide in Rust, whose
    // is_aux_session_id is deliberately case-insensitive. Never reuse this
    // predicate for a security or destructive decision.
    function auxChatIsAuxSession(id) {
      return typeof id === "string" && id.indexOf("aux-") === 0;
    }
    function auxChatEmptySnapshot() {
      return { chatItems: [], busy: false, queued: [] };
    }
    async function auxChatEnsure(taskId) {
      const task = String(taskId || "").trim();
      if (!task) throw new Error(bt("targetSessionMissing"));
      const metadata = await invoke("get_or_create_aux_session", { sessionId: task });
      const auxId = metadata && typeof metadata.id === "string" ? metadata.id : "";
      if (!auxChatIsAuxSession(auxId)) throw new Error(bt("sessionDataInvalid"));
      // Aux sessions never become active: they use the background-buffer
      // load_session(setActive:false) path.
      await ensureSessionBufferLoaded(auxId);
      return auxId;
    }
    async function auxChatSend(auxId, text) {
      const sid = String(auxId || "").trim();
      const message = String(text || "").trim();
      if (!auxChatIsAuxSession(sid)) throw new Error(bt("targetSessionMissing"));
      if (!message) throw new Error(bt("replyContentEmpty"));
      await ensureSessionBufferLoaded(sid);
      const buf = sessionStates[sid];
      // Aux sessions never queue (queue is user-input semantics): reject
      // outright when busy or queued messages exist; the caller retries.
      if (isBusyFor(sid) || (buf && Array.isArray(buf.queued) && buf.queued.length > 0)) {
        throw new Error(bt("turnAlreadyInProgress"));
      }
      // 发送命令是 lane 真实差异（desktop: chat + attachments;Web:
      // web_access_chat + attachmentHandles）,本模块走 desktop 派发。
      return auxChatDispatch(sid, message);
    }
    // Synchronous snapshot: when not loaded (no buffer) returns an empty
    // structure — never throws and never triggers a load. Items are shallow-
    // copied one by one: streaming deltas mutate buffer items in place (the
    // item.text/html assignments in chat-events), so copying only the array
    // would share object references and a caller comparing field by field
    // could not detect changes; after copying, every poll gets fresh
    // references, making field comparison a true content comparison.
    function auxChatSnapshotItems(items) {
      return (Array.isArray(items) ? items : []).map(function (item) {
        return item && typeof item === "object" ? Object.assign({}, item) : item;
      });
    }
    function auxChatSnapshot(auxId) {
      const sid = String(auxId || "").trim();
      if (!sid) return auxChatEmptySnapshot();
      if (sid === state.activeSessionId) {
        return {
          chatItems: auxChatSnapshotItems(state.chatItems),
          busy: !!state.busy,
          queued: auxChatSnapshotItems(state.queued),
        };
      }
      const buf = sessionStates[sid];
      if (!buf) return auxChatEmptySnapshot();
      // An always-open panel polling snapshot() counts as "reading": refresh
      // LRU recency consistently with the getBuffer read paths, otherwise
      // after 32+ session switches the buffer is evicted by capacity and the
      // panel wrongly shows the empty state.
      touchSessionBuffer(sid, buf, false);
      return {
        chatItems: auxChatSnapshotItems(buf.chatItems),
        busy: !!buf.busy,
        queued: auxChatSnapshotItems(buf.queued),
      };
    }
    async function auxChatDiscard(taskId) {
      const task = String(taskId || "").trim();
      if (!task) throw new Error(bt("targetSessionMissing"));
      await invoke("discard_aux_session", { sessionId: task });
      // The aux id is a pure function of the task id (aux-<taskId>, round-30
      // B8), so the buffer purge derives it — the old per-task id map was
      // redundant state that was never pruned (M5). The session:deleted event
      // fired by backend deletion is handled by the sessions domain as a
      // fallback — belt and suspenders, and idempotent.
      purgeSessionBuffer(`aux-${task}`);
    }
    // Atomic restart (M6): one backend command discards the old aux session
    // (gated against its turns) and creates the fresh one — no two-invoke
    // window for an orphaned discard to land in. The aux id is derived
    // (aux-<taskId>, round-30 B8), so the stale buffer purge needs no
    // per-task id map; the backend's session:deleted also purges through
    // the sessions domain as a fallback.
    async function auxChatReset(taskId) {
      const task = String(taskId || "").trim();
      if (!task) throw new Error(bt("targetSessionMissing"));
      const metadata = await invoke("reset_aux_session", { sessionId: task });
      const auxId = metadata && typeof metadata.id === "string" ? metadata.id : "";
      if (!auxChatIsAuxSession(auxId)) throw new Error(bt("sessionDataInvalid"));
      purgeSessionBuffer(auxId);
      // Aux sessions never become active: they use the background-buffer
      // load_session(setActive:false) path.
      await ensureSessionBufferLoaded(auxId);
      return auxId;
    }

    // isAuxSession stays domain-private: both facades only validate their own
    // ensure/send inputs with it, and no feature ever needed it across the
    // bridge — the dead public export (pinned by the domain contract) is gone
    // instead of being maintained on both sides forever.
    return {
      ensure: auxChatEnsure,
      send: auxChatSend,
      snapshot: auxChatSnapshot,
      discard: auxChatDiscard,
      reset: auxChatReset,
    };
  };
})(typeof window === "undefined" ? globalThis : window);
