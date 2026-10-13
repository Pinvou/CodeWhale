/**
 * Windows desktop dual-end remote control bridge (DEV-20).
 *
 * This is a separate domain from the existing `webAccess` WebUI share:
 * `controlledDevice` (B), `controllerConnection` + `remoteCapabilities` +
 * `remoteModels` and lease/task status (A) live here; durable history is only
 * the SessionStore RemoteProxy projection. Every async completion
 * is gated by the intent sequence and generation/lease epoch held in
 * `features/remote-control/remote-control-state.js`.
 *
 * A07 exposes lifecycle/status, A-local in-memory history and B-local stop adapters. Model export,
 * permission decisions and authenticated B-history mutation are absent from the
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
    var channelRegistrations = Object.create(null);
    var capabilityWaiters = Object.create(null);
    var capabilityOutcomes = Object.create(null);
    var capabilityOutcomeOrder = [];
    var capabilityFlights = Object.create(null);
    var capabilityRenewalTimer = null;
    var modelListIntent = 0;
    var capabilityReadIntent = 0;
    var controlledTerminalSequence = 0;
    var controllerTerminalSequence = 0;
    var proxyListIntent = 0;
    var proxyReadIntents = Object.create(null);
    var proxyReadEpoch = 0;
    var proxyOpenIntent = 0;
    var proxyRefreshTimer = null;
    var remoteActionSequence = 0;
    var uncertainActionRequests = Object.create(null);
    var uncertainActionOrder = [];
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
    // undefined——后端已 waiting_for_pairing 且带连接码，面板却显示未开启（2026-09-20 实测）。
    var STATE_SLICES = [
      "controlledDevice",
      "controlledSession",
      "controllerConnection",
      "remoteCapabilities",
      "remoteModels",
      "remoteProxyHistories",
      "remoteProxyHistory",
      "remoteProxyNotice",
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
      if (typeof notify === "function") notify(["desktopRemoteControl"]);
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

    function subscribeChannel(name, start) {
      if (channelRegistrations[name]) return;
      var attempt = {};
      channelRegistrations[name] = attempt;
      function registrationFailed() {
        // A late failure only releases its own registration attempt, never a
        // replacement created after stop. This is registration bookkeeping;
        // callback/off lifecycle ownership remains a separate contract.
        if (channelRegistrations[name] === attempt) delete channelRegistrations[name];
      }
      try {
        Promise.resolve(start()).then(function (off) {
          unlisten.push(off);
        }, registrationFailed);
      } catch (_) {
        registrationFailed();
      }
    }

    function subscribe() {
      if (typeof listen !== "function") return;
      subscribeChannel("desktop-remote-controlled-status", function () {
        return listen("desktop-remote-controlled-status", function (event) {
          var payload = event && event.payload ? event.payload : event || {};
          noteControlledTerminal(payload);
          clientEvent("controlled_status", controlledDiagnosticDetail(payload.status));
          publish(logic.applyControlledStatus(current(), controlledStatusPayload(payload)));
        });
      });
      subscribeChannel("desktop-remote-controller-status", function () {
        return listen("desktop-remote-controller-status", function (event) {
          var payload = event && event.payload ? event.payload : event || {};
          noteControllerTerminal(payload);
          publish(logic.applyControllerStatus(current(), payload));
        });
      });
      subscribeChannel("desktop-remote-task-status", function () {
        return listen("desktop-remote-task-status", function (event) {
          var payload = event && event.payload ? event.payload : event || {};
          var state = current();
          var controlledSession = state.controlledSession || {};
          // This event is the current frozen task_status protocol projection.
          // The separate local task-event channel uses camelCase.
          var sessionId = payload.sessionId || payload.session_id || "";
          if (controlledSession.sessionId && String(controlledSession.sessionId) === String(sessionId)) {
            publish(logic.applyControlledTaskEvent(state, payload));
          }
        });
      });
      subscribeChannel("desktop-remote-task-event", function () {
        return listen("desktop-remote-task-event", function (event) {
          var payload = event && event.payload ? event.payload : event || {};
          publish(logic.applyControlledTaskEvent(current(), payload));
        });
      });
      subscribeChannel("desktop-remote-controlled-session", function () {
        return listen("desktop-remote-controlled-session", function (event) {
          var payload = event && event.payload ? event.payload : event || {};
          publish(logic.applyControlledSession(current(), payload));
        });
      });
      subscribeChannel("desktop-remote-controlled-journal", function () {
        return listen("desktop-remote-controlled-journal", function (event) {
          var payload = event && event.payload ? event.payload : event || {};
          publish(logic.applyControlledJournalEvent(current(), payload));
        });
      });
      subscribeChannel("desktop-remote-task-content", function () {
        return listen("desktop-remote-task-content", function (event) {
          var payload = event && event.payload ? event.payload : event || {};
          publish(logic.applyTaskContent(current(), payload));
        });
      });
      // A 端状态事件：后台 keeper 会在 connecting / safety_handshake /
      // connected / reconnecting / error 之间推进。没有这条监听时，前端只能
      // 看到一次性命令返回值，窗口重载后会误以为 A 端仍然空闲。
      // Rust authenticates this narrow result against the live RemoteProxy.
      // The WebView still checks its exact local/ref/session/lease binding.
      subscribeChannel("desktop-remote-proxy-capability", function () {
        return listen("desktop-remote-proxy-capability", function (event) {
          var payload = event && event.payload ? event.payload : event || {};
          settleCapabilityResponse(payload);
        });
      });
      subscribeChannel("desktop_remote_proxy", function () {
        return listen("desktop_remote_proxy", function (event) {
          var payload = event && event.payload ? event.payload : event || {};
          var before = current();
          var after = logic.applyRemoteProxyNotice(before, payload);
          if (after !== before) {
            publish(after);
            scheduleRemoteProxyRefresh(after.remoteProxyNotice);
          }
        });
      });
    }

    function stop() {
      unlisten.splice(0).forEach(function (off) {
        try {
          off();
        } catch (_) {}
      });
      channelRegistrations = Object.create(null);
      if (proxyRefreshTimer) clearTimeout(proxyRefreshTimer);
      proxyRefreshTimer = null;
      ++proxyListIntent;
      ++proxyReadEpoch;
      proxyReadIntents = Object.create(null);
      if (capabilityRenewalTimer) clearTimeout(capabilityRenewalTimer);
      capabilityRenewalTimer = null;
      Object.keys(capabilityWaiters).forEach(function (requestId) {
        var waiter = capabilityWaiters[requestId];
        clearTimeout(waiter.timer);
        waiter.reject(asBoundedError("capability_snapshot_unavailable"));
        delete capabilityWaiters[requestId];
      });
      uncertainActionRequests = Object.create(null);
      uncertainActionOrder = [];
    }

    function controlledIntent(openNewGeneration) {
      var next = logic.beginControlledIntent(current(), openNewGeneration);
      publish(next);
      return next.intents.controlled;
    }

    function controllerIntent(openNewConnection) {
      var next = logic.beginControllerIntent(current(), openNewConnection);
      publish(next);
      return next.intents.controller;
    }

    function applyControlled(intent, result, replaceBinding) {
      var payload = result && typeof result === "object" ? result : {};
      noteControlledTerminal(payload);
      clientEvent("controlled_result", controlledDiagnosticDetail(payload.status));
      publish(logic.applyControlledStatus(current(), Object.assign(
        controlledStatusPayload(payload), { intent: intent, replaceBinding: replaceBinding === true },
      )));
      return result;
    }

    /** The sole Rust ControlledStatus DTO has camelCase fields. */
    function controlledStatusPayload(result) {
      var wire = result && typeof result === "object" ? result : {};
      return shallow(wire);
    }

    function applyController(intent, result, replaceBinding) {
      var payload = controllerStatusPayload(result);
      noteControllerTerminal(payload);
      publish(logic.applyControllerStatus(current(), {
        intent: intent,
        status: payload.status,
        connectionId: payload.connectionId,
        controllerLeaseId: payload.controllerLeaseId,
        leaseEpoch: payload.leaseEpoch,
        relayEpoch: payload.relayEpoch,
        generation: payload.generation,
        claimRequestId: payload.claimRequestId,
        lease: payload.lease,
        deviceDisplayName: payload.deviceDisplayName,
        errorCode: payload.errorCode,
        replaceBinding: replaceBinding === true,
        deviceName: payload.deviceName,
        error: payload.error,
      }));
      return result;
    }

    function controlledStart(deviceName, workspaces) {
      ensureAvailable();
      subscribe();
      var intent = controlledIntent(true);
      return invoke("desktop_remote_controlled_start", {
        deviceName: deviceName || null,
        workspaces: Array.isArray(workspaces) ? workspaces : [],
      }).then(function (result) {
        if (current().controlledTerminatedOpenIntent === intent) {
          var terminalSequence = controlledTerminalSequence;
          return invoke("desktop_remote_controlled_status", {}).then(function (authoritative) {
            if (terminalSequence !== controlledTerminalSequence) return result;
            publish(logic.reconcileControlledOpenStatus(
              current(), intent, controlledStatusPayload(authoritative), result && result.generation,
            ));
            return result;
          });
        }
        return applyControlled(intent, result, true);
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function controlledRefresh() {
      ensureAvailable();
      subscribe();
      var intent = controlledIntent(true);
      return invoke("desktop_remote_controlled_refresh", {}).then(function (result) {
        return applyControlled(intent, result, true);
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
      var intent = current().intents.controlled;
      var revision = current().revision;
      return invoke("desktop_remote_controlled_status", {}).then(function (result) {
        if (current().revision !== revision) return result;
        return applyControlled(intent, result);
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    /**
     * 主动拉取一次被控端状态并投影到本地状态：事件或命令回包一旦丢失，
     * 面板仍能通过它拿回真实状态（例如后端已 waiting_for_pairing 与连接码）。
     */
    function controlledSync() {
      // A read must not supersede an in-flight start/refresh command intent.
      return controlledStatus();
    }

    function controlledDiagnosticDetail(status) {
      switch (status) {
        case "registering": case "waiting_for_pairing": case "pairing_reserved":
        case "handshake_pending": case "connected": case "reconnecting":
        case "stopped": case "error": return status;
        default: return "unknown";
      }
    }

    /// Frontend diagnostics use fixed semantic categories only.
    function clientEvent(event, detail) {
      if (!isAvailable()) return Promise.resolve();
      return invoke("desktop_remote_client_event", { event: event, detail: detail })
        .catch(function () {});
    }

    /**
     * 随包内置的「服务端点」profile 列表（生产/测试），与后端切换能力对应。
     * 该桥接能力无需 PINVOU_REMOTE_* 环境变量；当前 React 界面有意不消费
     * 端点列表和切换入口，不代表用户切换界面已交付。
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

    function controllerConnect(code) {
      ensureAvailable();
      subscribe();
      var intent = controllerIntent(true);
      return invoke("desktop_remote_controller_connect", { code: code }).then(function (result) {
        if (current().controllerTerminatedOpenIntent === intent) {
          var terminalSequence = controllerTerminalSequence;
          return invoke("desktop_remote_controller_status", {}).then(function (authoritative) {
            if (terminalSequence !== controllerTerminalSequence) return result;
            publish(logic.reconcileControllerOpenStatus(
              current(), intent, controllerStatusPayload(authoritative),
            ));
            return result;
          });
        }
        return applyController(intent, result, true);
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
      var before = current();
      return invoke("desktop_remote_controller_status", {}).then(function (result) {
        if (!sameControllerView(before, current())) return result;
        var payload = controllerStatusPayload(result);
        noteControllerTerminal(payload);
        publish(logic.applyControllerStatus(current(), Object.assign({}, payload, { authoritativeRead: true })));
        return result;
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    /** Rust ControllingStatus -> pure-state controller envelope. */
    function controllerStatusPayload(result) {
      var wire = result && typeof result === "object" ? result : {};
      var lease = wire.lease && typeof wire.lease === "object" ? wire.lease : {};
      return Object.assign({}, wire, {
        status: wire.status || "idle",
        connectionId: lease.connectionId || null,
        controllerLeaseId: lease.controllerLeaseId || null,
        leaseEpoch: lease.leaseEpoch || null,
        relayEpoch: lease.relayEpoch || null,
        generation: lease.remoteControlGeneration || null,
        claimRequestId: wire.claimRequestId || null,
        deviceName: wire.deviceDisplayName || null,
        error: wire.errorCode || null,
      });
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
      if (!requestId || !logic.matchesRemoteProxyCapabilityBinding(current(), payload)) return;
      var error = payload.capabilityError ? String(payload.capabilityError) : "";
      var outcome = {
        ok: !error && payload.mappingConfirmed === true
          && Number.isSafeInteger(payload.capabilityRevision) && payload.capabilityRevision > 0,
        code: error || "capability_snapshot_unavailable",
        payload: payload,
      };
      var waiter = capabilityWaiters[requestId];
      if (!waiter) {
        rememberCapabilityOutcome(requestId, outcome);
        return;
      }
      if (waiter.sessionId !== payload.sessionId || waiter.remoteRef !== payload.remoteSessionRef) return;
      var before = current();
      var after = logic.applyRemoteProxyCapabilityResult(before, payload);
      if (after === before) return;
      publish(after);
      finishCapabilityWaiter(requestId, outcome);
      if (outcome.ok) scheduleCapabilityRenewal(payload);
    }

    function waitForCapabilityResponse(intent, remoteRef) {
      var requestId = intent && intent.requestId ? String(intent.requestId) : "";
      var sessionId = intent && intent.sessionId ? String(intent.sessionId) : "";
      if (!requestId || !sessionId) return Promise.reject(asBoundedError("capability_snapshot_unavailable"));
      if (capabilityOutcomes[requestId]) {
        var remembered = capabilityOutcomes[requestId];
        delete capabilityOutcomes[requestId];
        if (remembered.payload.sessionId !== sessionId
          || remembered.payload.remoteSessionRef !== remoteRef
          || !logic.matchesRemoteProxyCapabilityBinding(current(), remembered.payload)) {
          return Promise.reject(asBoundedError("capability_snapshot_unavailable"));
        }
        var before = current();
        var after = logic.applyRemoteProxyCapabilityResult(before, remembered.payload);
        if (after === before) return Promise.reject(asBoundedError("capability_snapshot_unavailable"));
        publish(after);
        if (remembered.ok) scheduleCapabilityRenewal(remembered.payload);
        return remembered.ok
          ? Promise.resolve(remembered.payload)
          : Promise.reject(asBoundedError(remembered.code));
      }
      return new Promise(function (resolve, reject) {
        var timer = setTimeout(function () {
          delete capabilityWaiters[requestId];
          reject(asBoundedError("capability_snapshot_timeout"));
        }, CAPABILITY_RESPONSE_TIMEOUT_MS);
        capabilityWaiters[requestId] = {
          resolve: resolve,
          reject: reject,
          timer: timer,
          sessionId: sessionId,
          remoteRef: remoteRef,
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
          clientEvent("capability_handshake", "failed");
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

    function invalidControlledTask() {
      var error = new Error("controlled_task_input_invalid");
      error.code = "controlled_task_input_invalid";
      return error;
    }

    /** B submits through the same authoritative remote task service as A. */
    async function controlledTaskSend(sessionId, text, readyAttachments, skillIds) {
      ensureAvailable();
      var visibleText = String(text || "").trim();
      if (!requiredOpaqueId(sessionId) || !Array.isArray(readyAttachments)
        || readyAttachments.length > 8 || !Array.isArray(skillIds)
        || skillIds.length > 32 || new Set(skillIds).size !== skillIds.length
        || skillIds.some(function (id) { return !requiredOpaqueId(id); })) {
        throw invalidControlledTask();
      }
      var sources = [];
      var consumedIds = [];
      for (var index = 0; index < readyAttachments.length; index++) {
        var attachment = readyAttachments[index];
        var result = attachment && attachment.status === "ready" && attachment.result;
        if (!result) throw invalidControlledTask();
        var uploadId = result.__pinvouManagedDraftAttachmentId;
        if (uploadId) {
          result = await invoke("adopt_draft_attachment", {
            sessionId: sessionId,
            uploadId: uploadId,
          });
          Object.defineProperty(result, "__pinvouManagedAttachmentSessionId", {
            configurable: true,
            value: sessionId,
            enumerable: false,
          });
          attachment.result = result;
          attachment.basename = result.basename || attachment.basename;
        }
        if (!result || typeof result.path !== "string" || !result.path) {
          throw invalidControlledTask();
        }
        // Paths are B-local IPC input only. The authoritative task payload and
        // every network/journal projection contain Rust-minted opaque handles.
        sources.push({ path: result.path });
        consumedIds.push(attachment.id);
      }
      if (!visibleText && sources.length === 0) throw invalidControlledTask();
      var result = await invoke("desktop_remote_controlled_task_send", {
        sessionId: sessionId,
        text: visibleText,
        skillIds: skillIds.slice(),
        attachments: sources,
      }).catch(function (failure) { throw asBoundedError(failure); });
      var projection = result && typeof result === "object" ? result : {};
      // The B Command returns the frozen task_status protocol projection.
      var taskId = projection.task_id || null;
      if (taskId && projection.session_id === sessionId && projection.task_status) {
        publish(logic.applyControlledTaskEvent(current(), {
          taskId: taskId,
          sessionId: projection.session_id,
          generation: projection.generation,
          modelSource: projection.modelSource,
          status: projection.task_status,
          text: visibleText,
        }));
      }
      if (consumedIds.length) {
        state.attachments = (state.attachments || []).filter(function (attachment) {
          return consumedIds.indexOf(attachment.id) < 0;
        });
        if (typeof notify === "function") notify(["chat"]);
      }
      return result;
    }

    function controlledCapabilitiesSnapshot(sessionId) {
      ensureAvailable();
      subscribe();
      if (!requiredOpaqueId(sessionId)) {
        return Promise.reject(asBoundedError("capability_snapshot_unavailable"));
      }
      return invoke("desktop_remote_controlled_capabilities_snapshot", { sessionId: sessionId })
        .then(function (snapshot) {
          snapshot = snapshot && typeof snapshot === "object" ? snapshot : {};
          var descriptors = function (items, idKey, kind) {
            return (Array.isArray(items) ? items : []).map(function (item) {
              var id = item && item[idKey];
              if (!requiredOpaqueId(id)) return null;
              var projected = {};
              projected[idKey] = id;
              projected.displayName = String(item.displayName || id).slice(0, 256);
              if (kind === "workspace") projected.codeAllowed = item.codeAllowed === true;
              if (kind === "skill") {
                var scopes = Array.isArray(item.scopes) ? item.scopes.filter(function (scope, index, all) {
                  return (scope === "chat" || scope === "code") && all.indexOf(scope) === index;
                }) : [];
                if (!scopes.length || scopes.length !== item.scopes.length) return null;
                projected.scopes = scopes;
              }
              return projected;
            }).filter(Boolean);
          };
          var expiresAtMs = Number(snapshot.expiresAtMs);
          if (snapshot.sessionId !== sessionId
            || (snapshot.sessionLane !== "chat" && snapshot.sessionLane !== "code")
            || !requiredOpaqueId(snapshot.connectionId)
            || !requiredOpaqueId(snapshot.controllerLeaseId)
            || !requiredOpaqueId(snapshot.leaseEpoch)
            || !requiredOpaqueId(snapshot.relayEpoch)
            || !requiredOpaqueId(snapshot.remoteControlGeneration)
            || !Number.isSafeInteger(expiresAtMs) || expiresAtMs <= 0) {
            throw asBoundedError("capability_snapshot_unavailable");
          }
          return {
            connectionId: snapshot.connectionId,
            controllerLeaseId: snapshot.controllerLeaseId,
            leaseEpoch: snapshot.leaseEpoch,
            relayEpoch: snapshot.relayEpoch,
            remoteControlGeneration: snapshot.remoteControlGeneration,
            sessionId: snapshot.sessionId,
            sessionLane: snapshot.sessionLane,
            capabilityRevision: requiredOpaqueId(snapshot.capabilityRevision)
              ? snapshot.capabilityRevision : null,
            expiresAtMs: expiresAtMs,
            modes: (Array.isArray(snapshot.modes) ? snapshot.modes : [])
              .filter(function (mode) { return mode === "chat" || mode === "code"; }),
            tools: descriptors(snapshot.tools, "toolId", "tool"),
            skills: descriptors(snapshot.skills, "skillId", "skill"),
            workspaces: descriptors(snapshot.workspaces, "workspaceId", "workspace"),
          };
        })
        .catch(function (failure) { throw asBoundedError(failure); });
    }

    function capabilitiesSnapshot(remoteRef) {
      ensureAvailable();
      var intent = ++capabilityReadIntent;
      var before = current();
      var connection = before.controllerConnection || {};
      if (!requiredOpaqueId(remoteRef) || connection.status !== "connected"
        || connection.sessionRef !== remoteRef || !connection.connectionId
        || !connection.controllerLeaseId || !connection.leaseEpoch
        || !connection.relayEpoch || !connection.generation) {
        return Promise.reject(asBoundedError("capability_snapshot_unavailable"));
      }
      return invoke("desktop_remote_controller_capability_snapshot", { remoteRef: remoteRef }).then(function (snapshot) {
        if (intent !== capabilityReadIntent || !sameControllerView(before, current())) return null;
        var after = logic.applyCapabilities(current(), snapshot);
        if (after === current()) return null;
        publish(after);
        return after.remoteCapabilities;
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function noteControlledTerminal(payload) {
      var status = payload && payload.status;
      if (status === "stopped" || status === "error" || status === "disabled") {
        controlledTerminalSequence += 1;
      }
    }

    function noteControllerTerminal(payload) {
      var status = payload && payload.status;
      if (status === "idle" || status === "disconnected" || status === "error") {
        controllerTerminalSequence += 1;
      }
    }

    function sameControllerView(before, after) {
      var left = before.controllerConnection || {};
      var right = after.controllerConnection || {};
      return before.intents.controller === after.intents.controller
        && left.status === right.status
        && left.connectionId === right.connectionId
        && left.controllerLeaseId === right.controllerLeaseId
        && left.leaseEpoch === right.leaseEpoch
        && left.relayEpoch === right.relayEpoch
        && left.generation === right.generation
        && left.sessionRef === right.sessionRef
        && left.capabilityRevision === right.capabilityRevision;
    }

    function listExportableModels() {
      ensureAvailable();
      var intent = ++modelListIntent;
      var before = current();
      return invoke("desktop_remote_model_list_exportable", {}).then(function (models) {
        if (intent !== modelListIntent || !sameControllerView(before, current())) return [];
        var next = logic.applyModels(current(), models);
        if (next !== current()) publish(next);
        return next.remoteModels;
      });
    }

    function controlledStopTask(taskId) {
      ensureAvailable();
      var before = current();
      var device = before.controlledDevice || {};
      var session = before.controlledSession || {};
      var target = (before.controlledTasks || []).find(function (task) {
        return task && task.taskId === taskId && task.sessionId === session.sessionId;
      });
      if (!requiredOpaqueId(taskId) || ["connected", "reconnecting",
        "resume_provisional", "resume_activated_pending_liveness"].indexOf(device.status) < 0
        || !device.generation || session.generation !== device.generation || !target) {
        return Promise.reject(asBoundedError("task_binding_stale"));
      }
      return invoke("desktop_remote_controlled_task_stop", { taskId: taskId })
        .catch(function (failure) { throw asBoundedError(failure); })
        .then(function (result) {
          var latest = current();
          var active = latest.controlledSession || {};
          if (["connected", "reconnecting", "resume_provisional",
            "resume_activated_pending_liveness"].indexOf(latest.controlledDevice.status) >= 0
            && latest.controlledDevice.generation === device.generation
            && active.sessionId === session.sessionId
            && active.generation === session.generation
            && result && result.sessionId === session.sessionId
            && result.generation === session.generation) {
            publish(logic.applyControlledTaskEvent(latest, result));
          }
          return result;
        });
    }

    function listRemoteProxyHistory() {
      ensureAvailable();
      var intent = ++proxyListIntent;
      return invoke("desktop_remote_proxy_history_list", {}).then(function (records) {
        if (intent !== proxyListIntent) return [];
        var before = current();
        var after = logic.projectRemoteProxyList(before, records);
        if (after === before) throw asBoundedError("remote_proxy_projection_invalid");
        if (after !== before) publish(after);
        return after.remoteProxyHistories;
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function openLiveRemoteProxy(existingLocalSessionId) {
      ensureAvailable();
      subscribe();
      if (existingLocalSessionId != null
        && (!requiredOpaqueId(existingLocalSessionId) || !existingLocalSessionId.startsWith("remote-"))) {
        return Promise.reject(invalidTaskIntent());
      }
      var intent = ++proxyOpenIntent;
      return invoke("desktop_remote_proxy_open_live", {
        existingLocalSessionId: existingLocalSessionId || null,
      }).then(function (result) {
        if (intent !== proxyOpenIntent) throw asBoundedError("remote_proxy_binding_mismatch");
        if (existingLocalSessionId && result.localSessionId !== existingLocalSessionId) {
          throw asBoundedError("remote_proxy_binding_mismatch");
        }
        var before = current();
        var after = logic.applyRemoteProxyBinding(before, result);
        if (after === before) throw asBoundedError("remote_proxy_binding_mismatch");
        publish(after);
        return {
          localSessionId: result.localSessionId,
          remoteRef: result.remoteSessionRef,
        };
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function readRemoteProxyHistory(localSessionId, options) {
      ensureAvailable();
      if (!requiredOpaqueId(localSessionId) || !localSessionId.startsWith("remote-")) {
        return Promise.reject(invalidTaskIntent());
      }
      var intent = (proxyReadIntents[localSessionId] || 0) + 1;
      proxyReadIntents[localSessionId] = intent;
      var epoch = proxyReadEpoch;
      var navigationRead = options && options.navigation === true;
      return invoke("desktop_remote_proxy_history_read", { localSessionId: localSessionId })
        .then(function (record) {
          if (epoch !== proxyReadEpoch || intent !== proxyReadIntents[localSessionId]
            || !record || record.localSessionId !== localSessionId) return null;
          var before = current();
          var after = logic.projectRemoteProxyRead(before, record);
          if (after === before) throw asBoundedError("remote_proxy_projection_invalid");
          // Navigation consumes this verified value directly and publishes it
          // only after sessions adopts the ID. A background read of another
          // proxy must never replace the selected ChatView projection.
          if (!navigationRead && state.activeSessionId === localSessionId) publish(after);
          return after.remoteProxyHistory;
        }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function selectRemoteProxyHistory(record) {
      if (!record || state.activeSessionId !== record.localSessionId) return false;
      var before = current();
      var after = logic.projectRemoteProxyRead(before, record);
      if (after === before) return false;
      publish(after);
      return true;
    }

    function deleteRemoteProxyHistory(localSessionId) {
      ensureAvailable();
      if (!requiredOpaqueId(localSessionId) || !localSessionId.startsWith("remote-")) {
        return Promise.reject(invalidTaskIntent());
      }
      return invoke("desktop_remote_proxy_history_delete", { localSessionId: localSessionId })
        .then(function () {
          ++proxyListIntent;
          proxyReadIntents[localSessionId] = (proxyReadIntents[localSessionId] || 0) + 1;
          publish(logic.hideRemoteProxyHistory(current(), localSessionId));
          listRemoteProxyHistory().catch(function () {});
        }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function listRemoteProxyArchiveCandidates() {
      ensureAvailable();
      return invoke("desktop_remote_proxy_archive_candidates", {})
        .then(function (records) {
          if (!Array.isArray(records) || records.length > 256 || records.some(function (record) {
            return !record || !requiredOpaqueId(record.localSessionId)
              || !record.localSessionId.startsWith("remote-") || typeof record.hidden !== "boolean";
          })) throw asBoundedError("remote_proxy_projection_invalid");
          // This management-only result must not publish hidden entries into
          // the ordinary history list or select a live execution target.
          return records;
        }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function listRemoteProxyArchives(before) {
      ensureAvailable();
      return invoke("desktop_remote_proxy_archive_list", { before: before || null })
        .then(function (records) {
          if (!Array.isArray(records) || records.length > 256 || records.some(function (record) {
            return !record || !requiredOpaqueId(record.localSessionId)
              || !record.localSessionId.startsWith("remote-") || record.archived !== true
              || typeof record.archiveCursor !== "string" || !/^[1-9][0-9]{0,18}$/.test(record.archiveCursor);
          })) throw asBoundedError("remote_proxy_projection_invalid");
          return records;
        })
        .catch(function (failure) { throw asBoundedError(failure); });
    }

    function archiveRemoteProxyHistory(localSessionId) {
      ensureAvailable();
      if (!requiredOpaqueId(localSessionId) || !localSessionId.startsWith("remote-")) {
        return Promise.reject(invalidTaskIntent());
      }
      return invoke("desktop_remote_proxy_archive", { localSessionId: localSessionId })
        .then(function (cleaned) {
          ++proxyListIntent;
          proxyReadIntents[localSessionId] = (proxyReadIntents[localSessionId] || 0) + 1;
          var selected = current().remoteProxyHistory;
          if (selected && selected.localSessionId === localSessionId) {
            publish(logic.projectRemoteProxyRead(current(), Object.assign({}, selected, {
              archived: true, pendingUnknownCount: 0, unattemptedPending: [],
            })));
          }
          // The irreversible Rust commit succeeded. Refresh failure cannot
          // report it as an uncommitted archive or restore UI execution rights.
          Promise.resolve().then(function () { return listRemoteProxyHistory(); }).catch(function () {});
          Promise.resolve().then(function () { return readRemoteProxyHistory(localSessionId); }).catch(function () {});
          return cleaned;
        }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function scheduleRemoteProxyRefresh(notice) {
      if (proxyRefreshTimer) clearTimeout(proxyRefreshTimer);
      proxyRefreshTimer = setTimeout(function () {
        proxyRefreshTimer = null;
        if (current().remoteProxyNotice !== notice || !isAvailable()) return;
        var selected = current().remoteProxyHistory;
        listRemoteProxyHistory().catch(function () {});
        if (selected && selected.localSessionId === notice.localSessionId) {
          readRemoteProxyHistory(selected.localSessionId).catch(function () {});
        }
      }, 50);
    }

    function invalidTaskIntent() {
      var error = new Error("remote_task_intent_invalid");
      error.code = "remote_task_intent_invalid";
      return error;
    }

    function requiredOpaqueId(value) {
      return typeof value === "string" && value.length > 0 && value.length <= 128
        && /^[A-Za-z0-9._:-]+$/.test(value);
    }

    /** A-side intent only. Rust owns IDs, digest, binding and encrypted wire bytes. */
    function reserveRemoteTask(localSessionId, remoteRef, intent) {
      ensureAvailable();
      if (!requiredOpaqueId(localSessionId) || !requiredOpaqueId(remoteRef) || !intent || typeof intent !== "object") {
        return Promise.reject(invalidTaskIntent());
      }
      var body;
      if (intent.kind === "new") {
        if (typeof intent.text !== "string" || !intent.text.trim()
          || new TextEncoder().encode(intent.text).length > 65536
          || (intent.mode !== "chat" && intent.mode !== "code")
          || (intent.modelSource !== "controlled_device" && intent.modelSource !== "controller_temporary")) {
          return Promise.reject(invalidTaskIntent());
        }
        if ((intent.mode === "chat" && intent.workspaceId != null)
          || (intent.mode === "code" && !requiredOpaqueId(intent.workspaceId))) {
          return Promise.reject(invalidTaskIntent());
        }
        if (!Array.isArray(intent.skillIds) || intent.skillIds.length > 32
          || new Set(intent.skillIds).size !== intent.skillIds.length
          || intent.skillIds.some(function (id) { return !requiredOpaqueId(id); })) {
          return Promise.reject(invalidTaskIntent());
        }
        if (intent.modelSource === "controller_temporary" && !requiredOpaqueId(intent.selectedModelId)) {
          return Promise.reject(invalidTaskIntent());
        }
        if (intent.mode === "code" && intent.modelSource === "controller_temporary") {
          return Promise.reject(invalidTaskIntent());
        }
        if (intent.modelSource === "controlled_device" && intent.selectedModelId != null) {
          return Promise.reject(invalidTaskIntent());
        }
        if (Object.keys(intent).some(function (key) {
          return ["kind", "text", "mode", "workspaceId", "skillIds", "modelSource", "selectedModelId"].indexOf(key) < 0;
        })) return Promise.reject(invalidTaskIntent());
        body = {
          kind: "new",
          text: intent.text,
          mode: intent.mode,
          workspaceId: intent.mode === "code" ? intent.workspaceId : null,
          skillIds: intent.skillIds.slice(),
          modelSource: intent.modelSource,
          selectedModelId: intent.modelSource === "controller_temporary" ? intent.selectedModelId : null,
        };
      } else return Promise.reject(invalidTaskIntent());
      return invoke("desktop_remote_task_reserve_a", {
        localSessionId: localSessionId,
        remoteRef: remoteRef,
        intent: body,
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function validTaskHandle(handle) {
      return !!handle && typeof handle === "object"
        && requiredOpaqueId(handle.taskId) && requiredOpaqueId(handle.clientRequestId)
        && /^[a-fA-F0-9]{64}$/.test(handle.requestFingerprint || "")
        && !Object.keys(handle).some(function (key) {
          return ["taskId", "clientRequestId", "requestFingerprint"].indexOf(key) < 0;
        });
    }

    function commitRemoteTask(localSessionId, remoteRef, handle, attachmentIds) {
      ensureAvailable();
      var ids = Array.isArray(attachmentIds) ? attachmentIds : [];
      if (!requiredOpaqueId(localSessionId) || !requiredOpaqueId(remoteRef) || !validTaskHandle(handle)
        || ids.length > 8 || ids.some(function (id) { return !requiredOpaqueId(id); })
        || new Set(ids).size !== ids.length) return Promise.reject(invalidTaskIntent());
      return invoke("desktop_remote_task_commit_a", {
        localSessionId: localSessionId,
        remoteRef: remoteRef,
        handle: {
          taskId: handle.taskId,
          clientRequestId: handle.clientRequestId,
          requestFingerprint: handle.requestFingerprint,
        },
        attachmentIds: ids.slice(),
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function cancelRemoteTask(localSessionId, remoteRef, taskId) {
      ensureAvailable();
      if (!requiredOpaqueId(localSessionId) || !requiredOpaqueId(remoteRef) || !requiredOpaqueId(taskId)) {
        return Promise.reject(invalidTaskIntent());
      }
      return invoke("desktop_remote_task_cancel_a", {
        localSessionId: localSessionId, remoteRef: remoteRef, taskId: taskId,
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function abandonRemoteTask(localSessionId, clientRequestId, taskId) {
      ensureAvailable();
      if (!requiredOpaqueId(localSessionId) || !requiredOpaqueId(clientRequestId) || !requiredOpaqueId(taskId)) {
        return Promise.reject(invalidTaskIntent());
      }
      return invoke("desktop_remote_task_abandon_prepared_a", {
        localSessionId: localSessionId,
        clientRequestId: clientRequestId,
        taskId: taskId,
      }).catch(function (failure) { throw asBoundedError(failure); });
    }

    function newRemoteActionRequestId() {
      remoteActionSequence += 1;
      return "action-" + Date.now().toString(36) + "-" + remoteActionSequence.toString(36)
        + "-" + Math.floor(Math.random() * 0x100000000).toString(36);
    }

    function remoteActionKey(localSessionId, remoteRef, intent) {
      return [
        localSessionId, remoteRef, intent.operation, intent.taskId, intent.transferId,
        intent.chunkIndex, intent.chunkBytes, intent.permissionRequestId, intent.decision,
        intent.artifactId, intent.fileName, intent.mimeType, intent.totalBytes, intent.sha256,
      ].map(function (value) { return value == null ? "" : String(value); }).join("\u001f");
    }

    function rememberUncertainAction(key, requestId) {
      if (!uncertainActionRequests[key]) uncertainActionOrder.push(key);
      uncertainActionRequests[key] = requestId;
      while (uncertainActionOrder.length > 128) {
        var expired = uncertainActionOrder.shift();
        delete uncertainActionRequests[expired];
      }
    }

    function forgetUncertainAction(key) {
      delete uncertainActionRequests[key];
    }

    function invokeRemoteAction(localSessionId, remoteRef, intent, suppliedRequestId) {
      ensureAvailable();
      if (!requiredOpaqueId(localSessionId) || !requiredOpaqueId(remoteRef)) {
        return Promise.reject(invalidTaskIntent());
      }
      if (suppliedRequestId != null && !requiredOpaqueId(suppliedRequestId)) {
        return Promise.reject(invalidTaskIntent());
      }
      var key = remoteActionKey(localSessionId, remoteRef, intent);
      var requestId = suppliedRequestId || uncertainActionRequests[key] || newRemoteActionRequestId();
      function attempt(retryUncertain) {
        return invoke("desktop_remote_action_a", {
          localSessionId: localSessionId, remoteRef: remoteRef,
          requestId: requestId, intent: intent,
        }).then(function (body) {
          forgetUncertainAction(key);
          return body;
        }).catch(function (failure) {
          var error = asBoundedError(failure);
          error.requestId = requestId;
          if (error.code === "result_unknown") {
            rememberUncertainAction(key, requestId);
            if (retryUncertain) return attempt(false);
          } else {
            forgetUncertainAction(key);
          }
          throw error;
        });
      }
      return attempt(true);
    }

    function permissionList(localSessionId, remoteRef, taskId) {
      if (!requiredOpaqueId(taskId)) return Promise.reject(invalidTaskIntent());
      return invokeRemoteAction(localSessionId, remoteRef, {
        operation: "permission_list", taskId: taskId,
      }).then(function (body) {
        var permissions = body && body.permissions;
        var next = shallow(current());
        next.remotePermissions = Array.isArray(permissions)
          ? permissions.slice(0, 8).map(function (entry) {
              return {
                permissionRequestId: String(entry.permission_request_id || ""),
                taskId: String(entry.task_id || ""),
                operationType: String(entry.operation_type || ""),
                riskLevel: String(entry.risk_level || ""),
                targetScope: String(entry.target_scope || ""),
                preview: String(entry.preview || ""),
                state: String(entry.state || "pending"),
              };
            })
          : [];
        publish(next);
        return next.remotePermissions;
      });
    }

    function permissionDecide(localSessionId, remoteRef, taskId, permissionRequestId, decision) {
      if (!requiredOpaqueId(taskId) || !requiredOpaqueId(permissionRequestId)
        || ["allow", "deny"].indexOf(decision) < 0) return Promise.reject(invalidTaskIntent());
      return invokeRemoteAction(localSessionId, remoteRef, {
        operation: "permission_decide", taskId: taskId,
        permissionRequestId: permissionRequestId, decision: decision,
      }).then(function (result) {
        var next = shallow(current());
        next.remotePermissions = (next.remotePermissions || []).filter(function (entry) {
          return entry.permissionRequestId !== permissionRequestId;
        });
        publish(next);
        return result;
      });
    }

    // B's local, sanitized read-only projection. Never uses A's remote action
    // signature and never creates a decision or mutates A's permission mirror.
    function controlledPermissionList(taskId) {
      ensureAvailable();
      if (!requiredOpaqueId(taskId)) return Promise.reject(invalidTaskIntent());
      return invoke("desktop_remote_permission_list_b", { taskId: taskId });
    }

    function permissionDenyTask(taskId, reason) {
      ensureAvailable();
      return invoke("desktop_remote_permission_deny_task", { taskId: taskId, reason: reason || "b_stop" });
    }

    function attachmentBegin(localSessionId, remoteRef, taskId, fileName, mimeType, totalBytes, sha256) {
      if (!requiredOpaqueId(taskId) || typeof fileName !== "string" || typeof mimeType !== "string"
        || !fileName.trim() || Array.from(fileName).length > 200
        || !mimeType || mimeType.length > 120
        || !Number.isSafeInteger(totalBytes) || totalBytes <= 0 || totalBytes > 32 * 1024 * 1024
        || !/^[a-f0-9]{64}$/.test(sha256 || "")) return Promise.reject(invalidTaskIntent());
      return invokeRemoteAction(localSessionId, remoteRef, {
        operation: "attachment_begin", taskId: taskId, fileName: fileName,
        mimeType: mimeType, totalBytes: totalBytes, sha256: sha256,
      }).then(function (body) { return {
        transferId: body.transfer_id, chunkBytes: Number(body.chunk_bytes), totalBytes: Number(body.total_bytes),
      }; });
    }

    function attachmentChunk(localSessionId, remoteRef, taskId, transferId, chunkIndex, chunkB64, chunkBytes) {
      if (!requiredOpaqueId(taskId) || !requiredOpaqueId(transferId)
        || !Number.isSafeInteger(chunkIndex) || chunkIndex < 0
        || typeof chunkB64 !== "string" || !Number.isSafeInteger(chunkBytes)
        || chunkBytes <= 0 || chunkBytes > 256 * 1024) {
        return Promise.reject(invalidTaskIntent());
      }
      return invokeRemoteAction(localSessionId, remoteRef, {
        operation: "attachment_chunk", taskId: taskId, transferId: transferId,
        chunkIndex: chunkIndex, chunkB64: chunkB64, chunkBytes: chunkBytes,
      }).then(function (body) { return {
        state: body.state, receivedBytes: Number(body.received_bytes), sha256: body.sha256 || null,
      }; });
    }

    function attachmentAbort(localSessionId, remoteRef, taskId, transferId) {
      if (!requiredOpaqueId(taskId) || !requiredOpaqueId(transferId)) return Promise.reject(invalidTaskIntent());
      return invokeRemoteAction(localSessionId, remoteRef, {
        operation: "attachment_abort", taskId: taskId, transferId: transferId,
      });
    }

    function artifactIndex(localSessionId, remoteRef) {
      return invokeRemoteAction(localSessionId, remoteRef, { operation: "artifact_index" })
        .then(function (body) { return (body.artifacts || []).map(function (item) { return {
          artifactId: item.artifact_id, taskId: item.task_id, displayName: item.display_name,
          mimeType: item.mime_type, totalBytes: Number(item.total_bytes), sha256: item.sha256,
          previewKind: item.preview_kind,
        }; }); });
    }

    function artifactPreview(localSessionId, remoteRef, artifactId) {
      if (!requiredOpaqueId(artifactId)) return Promise.reject(invalidTaskIntent());
      return invokeRemoteAction(localSessionId, remoteRef, {
        operation: "artifact_preview", artifactId: artifactId,
      }).then(function (body) { return {
        artifactId: body.artifact_id, previewKind: body.preview_kind,
        truncated: body.truncated, text: body.text || null,
      }; });
    }

    function artifactDownloadBegin(localSessionId, remoteRef, taskId, artifactId) {
      if (!requiredOpaqueId(taskId) || !requiredOpaqueId(artifactId)) return Promise.reject(invalidTaskIntent());
      return invokeRemoteAction(localSessionId, remoteRef, {
        operation: "artifact_download_begin", taskId: taskId, artifactId: artifactId,
      }).then(function (body) { return {
        transferId: body.transfer_id, chunkBytes: Number(body.chunk_bytes), totalBytes: Number(body.total_bytes),
      }; });
    }

    function artifactDownloadChunk(localSessionId, remoteRef, taskId, artifactId, transferId, chunkIndex) {
      if (!requiredOpaqueId(taskId) || !requiredOpaqueId(artifactId) || !requiredOpaqueId(transferId)
        || !Number.isSafeInteger(chunkIndex) || chunkIndex < 0) return Promise.reject(invalidTaskIntent());
      return invokeRemoteAction(localSessionId, remoteRef, {
        operation: "artifact_download_chunk", taskId: taskId,
        artifactId: artifactId, transferId: transferId, chunkIndex: chunkIndex,
      }).then(function (body) { return {
        chunkIndex: Number(body.chunk_index), chunkB64: body.chunk_b64,
        chunkBytes: Number(body.chunk_bytes), receivedBytes: Number(body.received_bytes),
        nextChunk: Number(body.next_chunk), completed: body.completed,
      }; });
    }

    function artifactDownloadAbort(localSessionId, remoteRef, taskId, artifactId, transferId) {
      if (!requiredOpaqueId(taskId) || !requiredOpaqueId(artifactId) || !requiredOpaqueId(transferId)) {
        return Promise.reject(invalidTaskIntent());
      }
      return invokeRemoteAction(localSessionId, remoteRef, {
        operation: "artifact_download_abort", taskId: taskId,
        artifactId: artifactId, transferId: transferId,
      });
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
    if (isAvailable()) controllerStatus().catch(function () {});

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
      clientEvent: clientEvent,
      controlledSync: controlledSync,
      endpointProfiles: endpointProfiles,
      setEndpointProfile: setEndpointProfile,
      controlledListWorkspaces: controlledListWorkspaces,
      controllerConnect: controllerConnect,
      controllerDisconnect: controllerDisconnect,
      controllerStatus: controllerStatus,
      controllerCapabilitySend: controllerCapabilitySend,
      controlledTaskSend: controlledTaskSend,
      controlledCapabilitiesSnapshot: controlledCapabilitiesSnapshot,
      controlledStopTask: controlledStopTask,
      capabilitiesSnapshot: capabilitiesSnapshot,
      listExportableModels: listExportableModels,
      listRemoteProxyHistory: listRemoteProxyHistory,
      openLiveRemoteProxy: openLiveRemoteProxy,
      readRemoteProxyHistory: readRemoteProxyHistory,
      selectRemoteProxyHistory: selectRemoteProxyHistory,
      deleteRemoteProxyHistory: deleteRemoteProxyHistory,
      archiveRemoteProxyHistory: archiveRemoteProxyHistory,
      listRemoteProxyArchives: listRemoteProxyArchives,
      listRemoteProxyArchiveCandidates: listRemoteProxyArchiveCandidates,
      reserveRemoteTask: reserveRemoteTask,
      commitRemoteTask: commitRemoteTask,
      cancelRemoteTask: cancelRemoteTask,
      abandonRemoteTask: abandonRemoteTask,
      permissionList: permissionList,
      controlledPermissionList: controlledPermissionList,
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
    };
  };
})(typeof window !== "undefined" ? window : globalThis);
