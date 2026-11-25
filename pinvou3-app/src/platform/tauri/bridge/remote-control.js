/**
 * Persistent Web access administration for the desktop Tauri bridge.
 */
(function (root) {
  "use strict";
  var registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};
  registry["remote-control"] = function (context) {
    var state = context.state;
    var notify = context.notify;
    var invoke = context.invoke;
    var listen = context.listen;
    var bt = context.bt;
    var desktopProxyStarted = false;
    var eventForwarders = {};
    var policyPromise = null;
    var bridgeGeneration = (function () {
      try {
        if (root.crypto && typeof root.crypto.randomUUID === "function") {
          return "webview_" + root.crypto.randomUUID().replace(/-/g, "_"); // safari14-ok: guarded above
        }
      } catch (_) {}
      return "webview_" + Date.now().toString(36) + "_" + Math.random().toString(36).slice(2);
    })();

    function loadAccessPolicy() {
      if (policyPromise) return policyPromise;
      var url = new URL("platform/web/access-policy.json", document.baseURI);
      policyPromise = fetch(url, { cache: "no-store" }).then(function (response) {
        if (!response.ok) throw new Error("Web access policy unavailable (" + response.status + ")");
        return response.json();
      }).then(function (policy) {
        return {
          commands: new Set(policy.allowed_commands || []),
          events: new Set(policy.allowed_events || []),
        };
      });
      return policyPromise;
    }

    function eventPayload(event) {
      return event && Object.prototype.hasOwnProperty.call(event, "payload") ? event.payload : (event || {});
    }

    function respondToWebAccess(requestId, ok, result, error, errorCode, errorCategory) {
      return invoke("web_access_rpc_respond", {
        requestId: requestId,
        generation: bridgeGeneration,
        ok: !!ok,
        result: result === undefined ? null : result,
        error: error ? String(error) : null,
        // Structured identity of desktop command errors (e.g. VoiceCommandError
        // {code, category, message}): the message text alone would leave the
        // browser lane's error-code → trilingual copy mapping unreachable.
        errorCode: errorCode || null,
        errorCategory: errorCategory || null,
      }).catch(function (respondError) {
        console.warn("[WebAccess] failed to send RPC response", respondError);
      });
    }

    async function startDesktopProxy() {
      if (desktopProxyStarted || typeof listen !== "function" || typeof fetch !== "function") return;
      desktopProxyStarted = true;
      var policyReady = loadAccessPolicy();

      // Install every allowlisted desktop-side forwarder before the bridge
      // readiness ACK so browser subscriptions cannot miss early events.
      var eventForwardersReady = policyReady.then(function (policy) {
        return Promise.all(Array.from(policy.events).map(function (name) {
          if (eventForwarders[name]) return Promise.resolve();
          return listen(name, function (appEvent) {
            invoke("web_access_publish_event", {
              event: name,
              payload: appEvent ? appEvent.payload : null,
            }).catch(function () {});
          }).then(function (unlisten) {
            eventForwarders[name] = unlisten;
          });
        }));
      });

      var rpcListenerReady = listen("web_access:rpc_request", async function (event) {
        var request = eventPayload(event);
        var requestId = request.request_id || request.requestId || request.id;
        var requestGeneration = request.bridge_generation || request.bridgeGeneration;
        if (!requestId || requestGeneration !== bridgeGeneration) return;

        var mayExecute = false;
        try {
          mayExecute = await invoke("web_access_rpc_begin", {
            requestId: requestId,
            generation: bridgeGeneration,
          });
        } catch (error) {
          console.warn("[WebAccess] RPC begin barrier failed", error);
          return;
        }
        if (!mayExecute) return;

        var policy;
        try {
          policy = await policyReady;
        } catch (error) {
          console.error("[WebAccess] policy load failed", error);
          await respondToWebAccess(requestId, false, null, error);
          return;
        }

        var command = String(request.command || "");
        if (!policy.commands.has(command)) {
          await respondToWebAccess(requestId, false, null, bt("remoteCmdNotAllowed")(command));
          return;
        }
        if (command === "__dialog_open") {
          await respondToWebAccess(requestId, false, null, bt("remoteDialogDesktop"));
          return;
        }

        try {
          var result = await invoke(command, request.args || {});
          await respondToWebAccess(requestId, true, result, null);
        } catch (error) {
          // Desktop command errors arrive as structured objects (e.g.
          // VoiceCommandError); keep their stable code/category for the
          // browser's trilingual error mapping instead of only the message.
          const structured = error && typeof error.code === "string";
          const errorCategory = error && typeof error.category === "string" ? error.category : null;
          await respondToWebAccess(
            requestId,
            false,
            null,
            error && error.message ? error.message : error,
            structured ? error.code : null,
            errorCategory
          );
        }
      });

      var subscribeListenerReady = listen("web_access:event_subscribe", async function (event) {
        var policy;
        try {
          policy = await policyReady;
        } catch (error) {
          console.error("[WebAccess] policy load failed", error);
          return;
        }
        var name = String(eventPayload(event).event || "");
        if (!name || !policy.events.has(name)) return;
        await eventForwardersReady;
      });

      // Keep the desktop indicator in sync with the actual browser connection.
      // The access endpoint is intentionally persistent, so `active` only means
      // that the QR/link remains valid; it does not mean a phone is connected.
      var statusListenerReady = listen("web_access:status", function (event) {
        state.webAccess = Object.assign({}, state.webAccess, eventPayload(event));
        notify();
      });

      try {
        await Promise.all([
          policyReady,
          eventForwardersReady,
          rpcListenerReady,
          subscribeListenerReady,
          statusListenerReady,
        ]);
        await invoke("web_access_bridge_ready", { generation: bridgeGeneration });
      } catch (error) {
        console.error("[WebAccess] desktop bridge readiness failed", error);
        throw error;
      }
    }

    // webAccess 状态写入无意图排序：start/stop/rotate 并发时后完成者会把
    // UI 指示写反（审计 a）。用户操作意图序号——陈旧响应作废，终态由最新
    // 意图的完成写入收敛：web_access:status 事件 payload 不含 starting，
    // 事件无法清理该标志，须由每个意图完成路径（含失败）显式兜底。
    var webAccessIntentSeq = 0;
    var webAccessStatusSeq = 0;

    async function refreshRemoteControlStatus(expectedIntentSeq) {
      var intentSeq = expectedIntentSeq === undefined ? webAccessIntentSeq : expectedIntentSeq;
      var statusSeq = ++webAccessStatusSeq;
      try {
        var status = await invoke("web_access_status");
        if (intentSeq !== webAccessIntentSeq || statusSeq !== webAccessStatusSeq) return status;
        state.webAccess = Object.assign({}, state.webAccess, status || {});
      } catch (error) {
        if (intentSeq !== webAccessIntentSeq || statusSeq !== webAccessStatusSeq) return;
        // 错误写入与码清理必须成对——刷新失败也要清掉旧恢复
        // 码,否则 registerHint 优先级会让旧的三语提示盖住本次真实错误。
        state.webAccess = Object.assign({}, state.webAccess, { last_error: String(error), last_error_code: null });
      }
      notify();
    }

    async function startRemoteControl(options) {
      var seq = ++webAccessIntentSeq;
      var wasActive = !!state.webAccess.active;
      // 乐观开始同样清掉旧恢复码(last_error 清理与码清理
      // 成对),否则新尝试期间横幅仍显示上一轮的旧恢复提示。
      state.webAccess = Object.assign({}, state.webAccess, { starting: true, last_error: null, last_error_code: null });
      notify();
      try {
        var info = await invoke("web_access_enable", {
          allowHostWorkspace: !!(options && options.allowHostWorkspace),
        });
        // A newer user action owns the state now; discard this stale result.
        if (seq !== webAccessIntentSeq) return info;
        state.webAccess = Object.assign({}, state.webAccess, info || {}, {
          // 乐观合并同时清掉旧恢复码,避免状态刷新落地的
          // 一个 IPC 窗口里,新近的成功动作之上仍显示旧的三语恢复横幅。
          active: true, starting: false, last_error: null, last_error_code: null,
        });
        notify();
        await refreshRemoteControlStatus(seq);
        return info;
      } catch (error) {
        if (seq !== webAccessIntentSeq) throw error;
        state.webAccess = Object.assign({}, state.webAccess, {
          active: wasActive, web_client_connected: wasActive && !!state.webAccess.web_client_connected, starting: false,
          status: "error", last_error: String(error), last_error_code: null,
        });
        notify();
        throw error;
      }
    }

    async function stopRemoteControl() {
      const seq = ++webAccessIntentSeq;
      // (#876 review P3) no useless initializer: the success branch always
      // assigns before any read and the failure branch rethrows, so a null
      // init was dead (no-useless-assignment).
      let outcome;
      try {
        outcome = await invoke("web_access_disable");
      } catch (error) {
        if (seq !== webAccessIntentSeq) return; // 陈旧失败不向调用者抛错：已有更新的用户操作接管状态（审计补充）
        // 停用失败的错误写入同样必须清掉旧恢复码,与
        // start/rotate/revoke 的失败分支同口径。
        state.webAccess = Object.assign({}, state.webAccess, { status: "error", last_error: String(error), last_error_code: null, starting: false });
        notify();
        throw error;
      }
      // Latent gap, currently unreachable: a disposition that settles after a
      // newer intent bumped the seq is discarded here without recording its
      // warning — unreachable because SettingsView serializes every web-access
      // action behind the single actionBusy flag.
      if (seq !== webAccessIntentSeq) return; // 已有更新的用户操作，不写反状态
      // Review P2 (#876, fix-delta follow-up): the local stop always
      // completes, and the returned disposition truthfully reports where the
      // old credentials' revocation intent went. On a full queue
      // (deferred_full_queue) the intent landed nowhere and is never
      // replayed, so it must surface as a persistent revocation_warning.
      // A queued disposition covers ONLY the endpoint stopped here: any
      // earlier endpoint whose intent was dropped stays orphaned (joinable
      // until expiry or manual cleanup), so queued/not_needed must never
      // clear an existing warning — auto-clearing would misreport the
      // still-orphaned endpoint as recovered. The match below is enumerated on
      // the literal "deferred_full_queue": a future second incomplete-
      // disposition variant would fall into the keep-existing-warning arm
      // until the bridge learns the new literal (the same enumerated-pin
      // blindness the Rust side discloses for its pins).
      const revocationWarning =
        outcome && outcome.revocation === "deferred_full_queue"
          ? "deferred_full_queue"
          : state.webAccess.revocation_warning || null;
      state.webAccess = Object.assign({}, state.webAccess, {
        revocation_warning: revocationWarning,
        active: false, endpoint_id: null, url: null, qr_data_url: null,
        web_client_connected: false, host_workspace_authorized: false, status: "stopped", starting: false,
        last_error: null, last_error_code: null,
      });
      notify();
      // (#876 fix-delta follow-up P2) Callers (the settings dialog) need the
      // outcome to decide whether the queue-full warning is live after this
      // stop — the dialog must NOT dismiss itself over an incomplete
      // revocation. `revocation` carries the effective post-stop warning: a
      // later queued/not_needed stop preserves a pre-existing warning and
      // keeps reporting deferred_full_queue, while a clean stop reports null
      // only when nothing was ever orphaned. `raw` carries the stop's own
      // disposition for callers that need the boundary value.
      return { revocation: revocationWarning, raw: outcome || {} };
    }

    async function refreshRemoteControlQr() {
      var seq = ++webAccessIntentSeq;
      try {
        var info = await invoke("web_access_rotate");
        if (seq !== webAccessIntentSeq) return info;
        // Review P2 (#876, fix-delta follow-up): the rotation disposition
        // describes only the endpoint replaced here — queued means THIS one
        // was durably queued, while earlier dropped-intent endpoints stay
        // orphaned, so the warning never auto-clears (deferred_full_queue
        // overwrites it; anything else keeps what is already shown). The
        // match is enumerated on the literal "deferred_full_queue": a future
        // second incomplete-disposition variant falls into the
        // keep-existing-warning arm until the bridge learns the new literal.
        state.webAccess = Object.assign({}, state.webAccess, info || {}, {
          active: true, web_client_connected: false, last_error: null, last_error_code: null, starting: false,
          revocation_warning:
            info && info.revocation_disposition === "deferred_full_queue"
              ? "deferred_full_queue"
              : state.webAccess.revocation_warning || null,
        });
        notify();
        await refreshRemoteControlStatus(seq);
        return info;
      } catch (error) {
        if (seq !== webAccessIntentSeq) throw error;
        state.webAccess = Object.assign({}, state.webAccess, { status: "error", last_error: String(error), last_error_code: null, starting: false });
        notify();
        throw error;
      }
    }

    // 撤销本机目录授权但不断开链接：桌面专属同意面，手机侧永不可达
    // （命令仅主 WebView 可调，且不在 Web 访问策略内）。
    async function revokeRemoteHostWorkspace() {
      var seq = ++webAccessIntentSeq;
      try {
        var info = await invoke("web_access_revoke_host_workspace");
        if (seq !== webAccessIntentSeq) return info;
        // endpoint 未激活时桌面端返回携带空 url 的非活跃
        // 信息(撤权仍已持久化成功,不再按失败上报);乐观合并按 url
        // 有无推导 active,不把非活跃状态标成在线。同时清掉旧恢复码。
        state.webAccess = Object.assign({}, state.webAccess, info || {}, {
          active: !!(info && info.url), starting: false,
          host_workspace_authorized: false, last_error: null, last_error_code: null,
        });
        notify();
        await refreshRemoteControlStatus(seq);
        return info;
      } catch (error) {
        if (seq !== webAccessIntentSeq) throw error;
        state.webAccess = Object.assign({}, state.webAccess, { last_error: String(error), last_error_code: null });
        notify();
        throw error;
      }
    }

    async function getWebRelaySettings() {
      return invoke("web_access_relay_settings");
    }

    async function setWebRelayAddress(address) {
      var seq = ++webAccessIntentSeq;
      var info = await invoke("web_access_set_relay", { address: address });
      if (seq !== webAccessIntentSeq) return info;
      await refreshRemoteControlStatus(seq);
      return info;
    }

    return {
      startDesktopProxy,
      refreshRemoteControlStatus,
      startRemoteControl,
      stopRemoteControl,
      refreshRemoteControlQr,
      revokeRemoteHostWorkspace,
      getWebRelaySettings,
      setWebRelayAddress,
    };
  };
})(window);
