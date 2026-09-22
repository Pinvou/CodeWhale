/**
 * Windows desktop dual-end remote control bridge (DEV-20).
 *
 * This is a separate domain from the existing `webAccess` WebUI share:
 * `controlledDevice` (B), `controllerConnection` + `remoteCapabilities` +
 * `remoteModels` + `remoteHistories` + `transcript` (A) live here, and every async completion
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
    var capabilityWaiters = Object.create(null);
    var capabilityOutcomes = Object.create(null);
    var capabilityOutcomeOrder = [];
    var capabilityFlights = Object.create(null);
    var capabilityRenewalTimer = null;
    var CAPABILITY_RESPONSE_TIMEOUT_MS = 8_000;
    var CAPABILITY_MAX_ATTEMPTS = 3;
    var CAPABILITY_RENEWAL_LEAD_MS = 5_000;
    function isAvailable() {
      // Capabilities load after the bridge is installed, so this must stay
      // lazy instead of being captured once at install time.
      return typeof can === "function" && can("desktopRemoteControl") === true;
    }

    // 域切片（bridge.js 的 STATE_SLICE_FIELDS.desktopRemoteControl）读的是**顶层键**
    // （controlledDevice / controllerConnection / …），而本 feature 过去只把整份状态挂在
    // 嵌套键 state.desktopRemoteControl 上，导致 UI 的 bs.controlledDevice 恒为
    // undefined——后端已 waiting_connection 且带连接码，面板却显示未开启（2026-09-20 实测）。
    var STATE_SLICES = [
      "controlledDevice",
      "controlledSession",
      "controllerConnection",
      "remoteCapabilities",
      "remoteModels",
      "remoteHistories",
      "tasks",
      "transcript",
      "controlledTasks",
      "controlledTranscript",
      "remotePermissions",
    ];

    function mirrorSlices(next) {
      for (var i = 0; i < STATE_SLICES.length; i++) {
        var key = STATE_SLICES[i];
        if (next && Object.prototype.hasOwnProperty.call(next, key)) state[key] = next[key];
      }
    }

    function publish(next) {
      state.desktopRemoteControl = next;
      mirrorSlices(next);
      if (typeof notify === "function") notify("desktopRemoteControl");
      return next;
    }

    // 安装时先镜像一次初始状态，保证 UI 首次读取 bs.controlledDevice 就不是
    // undefined。注意必须放在 STATE_SLICES 初始化之后：var 提升下提前调用会拿到
    // undefined 并让整个 feature 安装失败（2026-09-20 实测：voice 域缺失）。
    mirrorSlices(current());

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
        clientLog(
          "event status=" + String(payload.status || "") + " code=" + (payload.pairingCode ? "yes" : "no")
          + " expires=" + String(payload.pairingExpiresAtMs || payload.expiresAtMs || "")
          + " gen=" + String(payload.generation || ""),
        );
        publish(logic.applyControlledStatus(current(), payload));
      }).then(function (off) {
        unlisten.push(off);
      }).catch(function () {});
      listen("desktop-remote-task-status", function (event) {
        var payload = event && event.payload ? event.payload : event || {};
        var state = current();
        var controlledSession = state.controlledSession || {};
        var sessionId = payload.sessionId || payload.session_id || "";
        publish(controlledSession.sessionId && String(controlledSession.sessionId) === String(sessionId)
          ? logic.applyControlledTaskEvent(state, payload)
          : logic.applyTaskEvent(state, payload));
      }).then(function (off) {
        unlisten.push(off);
      }).catch(function () {});
      listen("desktop-remote-task-event", function (event) {
        var payload = event && event.payload ? event.payload : event || {};
        publish(logic.applyControlledTaskEvent(current(), payload));
      }).then(function (off) {
        unlisten.push(off);
      }).catch(function () {});
      listen("desktop-remote-controlled-session", function (event) {
        var payload = event && event.payload ? event.payload : event || {};
        publish(logic.applyControlledSession(current(), payload));
      }).then(function (off) {
        unlisten.push(off);
      }).catch(function () {});
      listen("desktop-remote-controlled-journal", function (event) {
        var payload = event && event.payload ? event.payload : event || {};
        publish(logic.applyControlledJournalEvent(current(), payload));
      }).then(function (off) {
        unlisten.push(off);
      }).catch(function () {});
      listen("desktop-remote-task-content", function (event) {
        var payload = event && event.payload ? event.payload : event || {};
        publish(logic.applyTaskContent(current(), payload));
      }).then(function (off) {
        unlisten.push(off);
      }).catch(function () {});
      listen("desktop-remote-controller-session", function (event) {
        var payload = event && event.payload ? event.payload : event || {};
        applyControllerSession(payload);
        settleCapabilityResponse(payload);
      }).then(function (off) {
        unlisten.push(off);
      }).catch(function () {});
      // A 端状态事件：后台 keeper 会在 connecting / safety_handshake /
      // connected / reconnecting / error 之间推进。没有这条监听时，前端只能
      // 看到一次性命令返回值，窗口重载后会误以为 A 端仍然空闲。
      listen("desktop-remote-controller-status", function (event) {
        var payload = event && event.payload ? event.payload : event || {};
        publish(logic.applyControllerStatus(current(), controllerStatusPayload(payload)));
      }).then(function (off) {
        unlisten.push(off);
      }).catch(function () {});
      listen("desktop-remote-controller-journal", function (event) {
        var payload = event && event.payload ? event.payload : event || {};
        publish(logic.applyJournalEvent(current(), payload));
      }).then(function (off) {
        unlisten.push(off);
      }).catch(function () {});
    }

    function stop() {
      unlisten.splice(0).forEach(function (off) {
        try {
          off();
        } catch (_) {}
      });
      subscribed = false;
      if (capabilityRenewalTimer) clearTimeout(capabilityRenewalTimer);
      capabilityRenewalTimer = null;
      Object.keys(capabilityWaiters).forEach(function (requestId) {
        var waiter = capabilityWaiters[requestId];
        clearTimeout(waiter.timer);
        waiter.reject(asBoundedError("capability_snapshot_unavailable"));
        delete capabilityWaiters[requestId];
      });
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
      clientLog(
        "result status=" + String(payload.status || "") + " code=" + (payload.pairingCode ? "yes" : "no")
        + " expires=" + String(payload.pairingExpiresAtMs || payload.expiresAtMs || "")
        + " gen=" + String(payload.generation || ""),
      );
      publish(logic.applyControlledStatus(current(), {
        intent: intent,
        status: payload.kind || payload.status || "idle",
        generation: payload.remoteControlGeneration || payload.generation || null,
        pairingCode: payload.pairingCode || null,
        expiresAtMs: payload.expiresAtMs || payload.pairingExpiresAtMs || null,
        deviceName: payload.deviceName || null,
        controllerDeviceName: payload.controllerDeviceName || null,
        workspaces: payload.workspaces || null,
        error: payload.errorCode || payload.lastError || null,
      }));
      return result;
    }

    function applyController(intent, result) {
      var payload = controllerStatusPayload(result);
      publish(logic.applyControllerStatus(current(), {
        intent: intent,
        status: payload.status,
        connectionId: payload.connectionId,
        leaseEpoch: payload.leaseEpoch,
        deviceName: payload.deviceName,
        error: payload.error,
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
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function controlledRefresh() {
      ensureAvailable();
      subscribe();
      var intent = controlledIntent();
      return invoke("desktop_remote_controlled_refresh", {}).then(function (result) {
        return applyControlled(intent, result);
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function controlledStop() {
      ensureAvailable();
      var intent = controlledIntent();
      return invoke("desktop_remote_controlled_stop", {}).then(function (result) {
        return applyControlled(intent, result);
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function controlledDisconnectNow() {
      ensureAvailable();
      var intent = controlledIntent();
      return invoke("desktop_remote_controlled_disconnect_now", {}).then(function (result) {
        return applyControlled(intent, result);
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function controlledStatus() {
      ensureAvailable();
      subscribe();
      return invoke("desktop_remote_controlled_status", {})
        .catch(function (failure) { throw asBoundedError(failure); });
    }

    /**
     * 主动拉取一次被控端状态并投影到本地状态：事件或命令回包一旦丢失，
     * 面板仍能通过它拿回真实状态（例如后端已 waiting_connection 与连接码）。
     */
    function controlledSync() {
      ensureAvailable();
      subscribe();
      var intent = controlledIntent();
      return invoke("desktop_remote_controlled_status", {})
        .then(function (result) {
          return applyControlled(intent, result);
        })
        .catch(function (failure) { throw asBoundedError(failure); });
    }

    /// 前端排障日志：写入与 Rust 相同的日志文件，支持人员可直接读取。
    function clientLog(message) {
      if (!isAvailable()) return Promise.resolve();
      return invoke("desktop_remote_client_log", { message: String(message || "") })
        .catch(function () {});
    }

    /**
     * 随包内置的「服务端点」profile 列表（生产/测试）。用户不需要再配置
     * PINVOU_REMOTE_* 环境变量即可在应用内切换。
     */
    function endpointProfiles() {
      ensureAvailable();
      return invoke("desktop_remote_endpoint_profiles");
    }

    function setEndpointProfile(profileId) {
      ensureAvailable();
      return invoke("desktop_remote_set_endpoint_profile", {
        profile: String(profileId || ""),
      }).catch(function (failure) { throw asBoundedError(failure); });
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
        return applyController(intent, result);
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    /**
     * A 端命令以有界错误码失败；Tauri 把 `Err(String)` 变成只有 message 的错误，
     * 这里把码恢复成 `error.code`，UI 才能给出对应的三语文案而不是笼统兜底。
     */
    function asBoundedError(failure) {
      // Tauri 对 `Err(String)` 直接抛**字符串**（既没有 code 也没有 message），
      // 之前会被兜底成 desktop_remote_control_failed，真实原因就此丢失。
      var raw = typeof failure === "string"
        ? failure
        : (failure && (failure.code || failure.message)) || "";
      var text = String(raw || "desktop_remote_control_failed");
      var error = new Error(text);
      error.code = text;
      return error;
    }

    function controllerDisconnect() {
      ensureAvailable();
      subscribe();
      var intent = controllerIntent();
      return invoke("desktop_remote_controller_disconnect", {}).then(function (result) {
        return applyController(intent, result);
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function controllerStatus() {
      ensureAvailable();
      subscribe();
      return invoke("desktop_remote_controller_status", {}).then(function (result) {
        var payload = controllerStatusPayload(result);
        publish(logic.applyControllerStatus(current(), payload));
        return result;
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    /** Rust ControllingStatus -> pure-state controller envelope. */
    function controllerStatusPayload(result) {
      var wire = result && typeof result === "object" ? result : {};
      var lease = wire.lease && typeof wire.lease === "object" ? wire.lease : {};
      return {
        status: wire.status || "idle",
        connectionId: lease.connectionId || null,
        leaseEpoch: lease.leaseEpoch || null,
        deviceName: wire.deviceDisplayName || null,
        error: wire.errorCode || null,
      };
    }

    /**
     * E2E-03: the A-side remote session and task intent. The view passes only UI
     * fields (title, mode, text); every protocol field, identifier and the input
     * digest are built in Rust. The composed payload is returned for the
     * encrypted dispatch task (E2E-04) and is never assembled here.
     */
    function controllerSessionOpen(title, mode) {
      ensureAvailable();
      subscribe();
      // 复用当前连接上的会话引用：重复打开是幂等的，不会为同一连接不断创建
      // 新会话（Rust 侧按引用复用并重新校验 lease 绑定）。
      var snapshot = current();
      var existing = snapshot.controllerConnection && snapshot.controllerConnection.sessionRef;
      return invoke("desktop_remote_controller_session_open", {
        remoteRef: existing || null,
        title: title || "",
        mode: mode || "chat",
      }).then(function (result) {
        return applyControllerSession(result);
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function controllerSessionState(remoteRef) {
      ensureAvailable();
      return invoke("desktop_remote_controller_session_state", { remoteRef: remoteRef });
    }

    function controllerSessionClose(remoteRef) {
      ensureAvailable();
      return invoke("desktop_remote_controller_session_close", { remoteRef: remoteRef });
    }

    function controllerCapabilityRequest(remoteRef) {
      ensureAvailable();
      return invoke("desktop_remote_controller_capability_request", { remoteRef: remoteRef });
    }

    function rememberCapabilityOutcome(requestId, outcome) {
      capabilityOutcomes[requestId] = outcome;
      capabilityOutcomeOrder.push(requestId);
      while (capabilityOutcomeOrder.length > 16) {
        delete capabilityOutcomes[capabilityOutcomeOrder.shift()];
      }
    }

    function finishCapabilityWaiter(requestId, outcome) {
      var waiter = capabilityWaiters[requestId];
      if (!waiter) {
        rememberCapabilityOutcome(requestId, outcome);
        return;
      }
      clearTimeout(waiter.timer);
      delete capabilityWaiters[requestId];
      if (outcome.ok) waiter.resolve(outcome.payload);
      else waiter.reject(asBoundedError(outcome.code));
    }

    function settleCapabilityResponse(payload) {
      var requestId = payload && payload.capabilityRequestId
        ? String(payload.capabilityRequestId)
        : "";
      if (!requestId) return;
      var error = payload.capabilityError ? String(payload.capabilityError) : "";
      var outcome = {
        ok: !error && payload.capabilityRevision != null,
        code: error || "capability_snapshot_unavailable",
        payload: payload,
      };
      finishCapabilityWaiter(requestId, outcome);
      // A successful snapshot satisfies every retry already in flight for the
      // same session. Late failures from older attempts must not regress it.
      if (outcome.ok && payload.sessionId) {
        Object.keys(capabilityWaiters).forEach(function (pendingId) {
          var waiter = capabilityWaiters[pendingId];
          if (String(waiter.sessionId || "") === String(payload.sessionId)) {
            finishCapabilityWaiter(pendingId, outcome);
          }
        });
        scheduleCapabilityRenewal(payload);
      }
    }

    function waitForCapabilityResponse(intent, remoteRef) {
      var requestId = intent && intent.requestId ? String(intent.requestId) : "";
      var sessionId = intent && intent.sessionId ? String(intent.sessionId) : "";
      if (!requestId || !sessionId) return Promise.reject(asBoundedError("capability_snapshot_unavailable"));
      if (capabilityOutcomes[requestId]) {
        var remembered = capabilityOutcomes[requestId];
        delete capabilityOutcomes[requestId];
        return remembered.ok
          ? Promise.resolve(remembered.payload)
          : Promise.reject(asBoundedError(remembered.code));
      }
      return new Promise(function (resolve, reject) {
        var timer = setTimeout(function () {
          delete capabilityWaiters[requestId];
          var connection = current().controllerConnection || {};
          if (
            String(connection.sessionRef || "") === String(remoteRef || "") &&
            connection.capabilityRevision != null &&
            connection.capabilityStatus === "ready"
          ) {
            resolve(connection);
            return;
          }
          reject(asBoundedError("capability_snapshot_timeout"));
        }, CAPABILITY_RESPONSE_TIMEOUT_MS);
        capabilityWaiters[requestId] = {
          resolve: resolve,
          reject: reject,
          timer: timer,
          sessionId: sessionId,
        };
      });
    }

    function capabilityAttempt(remoteRef, attempt) {
      return invoke("desktop_remote_controller_capability_send", { remoteRef: remoteRef })
        .then(function (intent) {
          return waitForCapabilityResponse(intent, remoteRef);
        })
        .catch(function (failure) {
          var error = asBoundedError(failure);
          if (attempt >= CAPABILITY_MAX_ATTEMPTS) throw error;
          return new Promise(function (resolve) {
            setTimeout(resolve, attempt * 500);
          }).then(function () {
            publish(logic.applyCapabilityPending(current(), { sessionRef: remoteRef }));
            return capabilityAttempt(remoteRef, attempt + 1);
          });
        });
    }

    function scheduleCapabilityRenewal(payload) {
      if (capabilityRenewalTimer) clearTimeout(capabilityRenewalTimer);
      var validForMs = Number(payload.capabilityValidForMs || 30_000);
      var delay = Math.max(1_000, Math.min(25_000, validForMs - CAPABILITY_RENEWAL_LEAD_MS));
      var sessionRef = current().controllerConnection && current().controllerConnection.sessionRef;
      if (!sessionRef) return;
      capabilityRenewalTimer = setTimeout(function () {
        capabilityRenewalTimer = null;
        var connection = current().controllerConnection || {};
        if (connection.status !== "connected" || connection.sessionRef !== sessionRef) return;
        beginCapabilityHandshake(sessionRef, true).catch(function () {});
      }, delay);
    }

    function beginCapabilityHandshake(remoteRef, background) {
      var key = String(remoteRef || "");
      if (capabilityFlights[key]) return capabilityFlights[key];
      if (!background) publish(logic.applyCapabilityPending(current(), { sessionRef: remoteRef }));
      var flight = capabilityAttempt(remoteRef, 1)
        .catch(function (failure) {
          var error = asBoundedError(failure);
          publish(logic.applyCapabilityFailure(current(), {
            sessionRef: remoteRef,
            code: error.code,
          }));
          clientLog("capability handshake failed code=" + String(error.code || "unknown"));
          throw error;
        })
        .finally(function () {
          delete capabilityFlights[key];
        });
      capabilityFlights[key] = flight;
      return flight;
    }

    /** 构造并发送请求，等待 B 的真实回包；超时或失败最多尝试三次。 */
    function controllerCapabilitySend(remoteRef) {
      ensureAvailable();
      subscribe();
      return beginCapabilityHandshake(remoteRef, false);
    }

    function controllerApplyCapabilitySnapshot(remoteRef, payload) {
      ensureAvailable();
      var body = payload && typeof payload === "object" ? payload : {};
      return invoke("desktop_remote_controller_apply_capability_snapshot", {
        remoteRef: remoteRef,
        payload: body,
      }).then(function (result) {
        // 同一个快照同时进入能力域（revision/modes/描述符），字段名按 UI 侧
        // 约定映射；wire 字段名不会泄漏到状态。
        publish(logic.applyCapabilities(current(), capabilitySnapshotForUi(body)));
        return applyControllerSession(result);
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function descriptorList(value, idKey) {
      if (!Array.isArray(value)) return [];
      return value.slice(0, 256).map(function (item) {
        var entry = item && typeof item === "object" ? item : {};
        return {
          id: entry[idKey] || entry.id || "",
          displayName: entry.display_name || entry.displayName || "",
          codeAllowed: !!entry.code_allowed,
        };
      });
    }

    function capabilitySnapshotForUi(payload) {
      var wire = payload && typeof payload === "object" ? payload : {};
      var revision = Number(wire.capability_revision);
      return {
        connectionId: wire.connection_id || null,
        leaseEpoch: wire.lease_epoch || null,
        relayEpoch: wire.relay_epoch || null,
        remoteControlGeneration: wire.remote_control_generation || null,
        capabilityRevision: isFinite(revision) ? revision : 0,
        modes: Array.isArray(wire.modes) ? wire.modes.slice(0, 2) : [],
        tools: descriptorList(wire.tools, "tool_id"),
        skills: descriptorList(wire.skills, "skill_id"),
        workspaces: descriptorList(wire.workspaces, "workspace_id"),
        expiresAtMs: wire.expires_at_ms || null,
      };
    }

    function controllerTaskIntent(remoteRef, text) {
      ensureAvailable();
      return invoke("desktop_remote_controller_task_intent", {
        remoteRef: remoteRef,
        text: String(text || ""),
      });
    }

    function controllerTaskCancelIntent(remoteRef, taskId) {
      ensureAvailable();
      return invoke("desktop_remote_controller_task_cancel_intent", {
        remoteRef: remoteRef,
        taskId: taskId,
      });
    }

    /**
     * E2E-04: compose and send one encrypted task / cancel frame. The protocol
     * payload is built and sealed in Rust; the UI only supplies the session
     * reference plus the text or task id.
     */
    function controllerTaskSend(remoteRef, text, modelSource) {
      ensureAvailable();
      var visibleText = String(text || "").trim();
      return invoke("desktop_remote_controller_task_send", {
        remoteRef: remoteRef,
        text: visibleText,
        modelSource: modelSource || "controlled_device",
      }).catch(function (failure) { throw asBoundedError(failure); }).then(function (result) {
        // The encrypted task-status acknowledgement intentionally carries no
        // prompt. Retain the text from the successfully queued local intent so
        // ChatView can show the sent message immediately; the authoritative
        // journal entry replaces this fallback by task id when it arrives.
        var projection = result && typeof result === "object" ? result : {};
        var taskId = projection.taskId || projection.task_id || null;
        if (taskId) {
          publish(logic.applyTaskEvent(current(), {
            taskId: taskId,
            sessionId: projection.sessionId || projection.session_id || null,
            status: "admitted",
            text: visibleText,
          }));
        }
        return result;
      });
    }

    function controllerTaskCancelSend(remoteRef, taskId) {
      ensureAvailable();
      return invoke("desktop_remote_controller_task_cancel_send", {
        remoteRef: remoteRef,
        taskId: taskId,
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    /** B submits through the same authoritative remote task service as A. */
    function controlledTaskSend(sessionId, text) {
      ensureAvailable();
      var visibleText = String(text || "").trim();
      return invoke("desktop_remote_controlled_task_send", {
        sessionId: sessionId,
        text: visibleText,
      }).catch(function (failure) { throw asBoundedError(failure); }).then(function (result) {
        var projection = result && typeof result === "object" ? result : {};
        var taskId = projection.taskId || projection.task_id || null;
        if (taskId) {
          publish(logic.applyControlledTaskEvent(current(), {
            taskId: taskId,
            sessionId: projection.sessionId || projection.session_id || sessionId,
            status: projection.taskStatus || projection.task_status || "admitted",
            text: visibleText,
          }));
        }
        return result;
      });
    }

    function applyControllerSession(result) {
      var payload = result && typeof result === "object" ? result : {};
      publish(logic.applyControllerSession(current(), {
        sessionRef: payload.sessionRef || null,
        sessionId: payload.sessionId || null,
        mode: payload.mode || null,
        connectionId: payload.connectionId || null,
        leaseEpoch: payload.leaseEpoch || null,
        capabilityRevision: payload.capabilityRevision == null ? null : payload.capabilityRevision,
        capabilityValidForMs: payload.capabilityValidForMs == null ? null : payload.capabilityValidForMs,
        capabilityError: payload.capabilityError || null,
        capabilityRequestId: payload.capabilityRequestId || null,
      }));
      return result;
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
        publish(logic.applyControlledTaskEvent(current(), result || {}));
        return result;
      });
    }

    function cancelRemoteTask(taskId) {
      ensureAvailable();
      return invoke("desktop_remote_task_cancel", { taskId: taskId }).then(function (result) {
        publish(logic.applyControlledTaskEvent(current(), result || {}));
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

    // 窗口重载 / bridge 重建后，以 Rust 状态机为权威恢复 A 端连接状态；
    // 否则后端 connected 而前端 idle，下一次连接会被本地状态机拒绝。
    controllerStatus().catch(function () {});

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
      clientLog: clientLog,
      endpointProfiles: endpointProfiles,
      setEndpointProfile: setEndpointProfile,
      controlledListWorkspaces: controlledListWorkspaces,
      controlledAuthorizeWorkspace: controlledAuthorizeWorkspace,
      controllerConnect: controllerConnect,
      controllerDisconnect: controllerDisconnect,
      controllerStatus: controllerStatus,
      controllerSessionOpen: controllerSessionOpen,
      controllerSessionState: controllerSessionState,
      controllerSessionClose: controllerSessionClose,
      controllerCapabilityRequest: controllerCapabilityRequest,
      controllerCapabilitySend: controllerCapabilitySend,
      controllerApplyCapabilitySnapshot: controllerApplyCapabilitySnapshot,
      controllerTaskIntent: controllerTaskIntent,
      controllerTaskCancelIntent: controllerTaskCancelIntent,
      controllerTaskSend: controllerTaskSend,
      controllerTaskCancelSend: controllerTaskCancelSend,
      controlledTaskSend: controlledTaskSend,
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
