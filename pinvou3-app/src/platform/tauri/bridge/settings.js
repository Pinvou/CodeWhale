/**
 * settings feature for the Tauri bridge.
 * Registered before bridge.js builds the backwards-compatible facade.
 */
(function (root) {
  "use strict";
  var registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};
  registry["settings"] = function (context) {
  var state = context.state;
  var notify = context.notify;
  var invoke = context.invoke;
  var listen = context.listen;
  var authoritativeAccountRevision = 0;
  var behaviorTelemetryStatusPollTimer = null;

  function behaviorTelemetryControlPending(status) {
    return !!(status && (status.withdrawal_pending || status.deletion_pending));
  }

  function behaviorTelemetryStatusNeedsPoll(status) {
    return !!(status && (status.enabled || behaviorTelemetryControlPending(status)));
  }

  function scheduleBehaviorTelemetryStatusPoll() {
    if (behaviorTelemetryStatusPollTimer || !behaviorTelemetryStatusNeedsPoll(state.behaviorTelemetry)) return;
    var delay = behaviorTelemetryControlPending(state.behaviorTelemetry) ? 1000 : 5000;
    behaviorTelemetryStatusPollTimer = setTimeout(async function pollBehaviorTelemetryStatus() {
      behaviorTelemetryStatusPollTimer = null;
      try {
        state.behaviorTelemetry = await invoke("get_behavior_telemetry_status");
        notify();
      } catch (_) {}
      if (behaviorTelemetryStatusNeedsPoll(state.behaviorTelemetry)) {
        scheduleBehaviorTelemetryStatusPoll();
      }
    }, delay);
  }

  function isBuiltinLlmApiModelId(modelId) {
    if (modelId === "builtin_llmapi") return true;
    return (state.savedModels || []).some(function (model) {
      return model && model.id === modelId && model.kind === "builtin_llmapi";
    });
  }

  function selectedModelIdForSession(sessionId) {
    if (!sessionId || sessionId === state.activeSessionId) {
      return state.currentSessionModelId || state.activeModelId || null;
    }
    return state.activeModelId || null;
  }

  function llmApiAccountIsAuthoritativelyUnavailable(status) {
    if (!status || status.stale === true) return false;
    return status.backend_user_state === "not_exists"
      || status.enabled === false
      || String(status.provisioning_status || "").trim().toLowerCase() === "disabled";
  }

  function modelAvailabilityError(sessionId) {
    if (!llmApiAccountIsAuthoritativelyUnavailable(state.llmApiStatus)) return null;
    if (!isBuiltinLlmApiModelId(selectedModelIdForSession(sessionId))) return null;
    var copy = root.PinvouTauriBridgeStrings || {};
    return copy.builtinModelUnavailable || "当前账户的内置模型已不可用，请先选择其他模型";
  }

  function authoritativeLlmApiErrorCode(error) {
    if (error && typeof error === "object" && (error.code === "service_disabled" || error.code === "user_not_found")) {
      return error.code;
    }
    var parts = [];
    if (typeof error === "string") parts.push(error);
    if (error && typeof error === "object") {
      if (error.message) parts.push(error.message);
      try { parts.push(JSON.stringify(error)); } catch (_) {}
    }
    var match = parts.join(" ").toLowerCase().match(/\b(service_disabled|user_not_found)\s*:/);
    return match ? match[1] : null;
  }

  function commandCanReportLlmApiAccountError(command) {
    if (String(command || "").indexOf("llmapi") >= 0) return true;
    return isBuiltinLlmApiModelId(selectedModelIdForSession(state.activeSessionId));
  }

  function applyAuthoritativeLlmApiError(error, command) {
    var code = authoritativeLlmApiErrorCode(error);
    if (!code || !commandCanReportLlmApiAccountError(command)) return false;
    authoritativeAccountRevision += 1;
    var previous = state.llmApiStatus || {};
    state.llmApiStatus = Object.assign({}, previous, {
      backend_user_exists: false,
      backend_user_state: "not_exists",
      stale: false,
      backend_username: null,
      backend_display_name: null,
      quota: null,
      auto_login_failed: true,
      last_error_code: code,
      last_error_message: String(error && error.message ? error.message : error || ""),
    });
    state.llmApiModels = null;
    if (isBuiltinLlmApiModelId(selectedModelIdForSession(state.activeSessionId))) {
      state.effectiveModelConfig = null;
    }
    notify();
    return true;
  }

  function observeInvokeError(error, command) {
    return applyAuthoritativeLlmApiError(error, command);
  }
  // ── Settings ─────────────────────────────────────────────────────
  // 桌宠开关由 Rust set_pet_enabled 直接写盘(设置页/宠物右键/快捷图标共用),
  // 这里同步进内存副本，保证设置界面立即反映专用命令返回的桌宠状态。
  listen("pet:enabled_changed", function (e) {
    if (state.settings) {
      state.settings.pet = Object.assign({}, state.settings.pet || {}, {
        enabled: !!(e.payload && e.payload.enabled),
      });
      notify();
    }
  });

  listen("pet:selected_changed", function (e) {
    var selectedPet = e.payload && e.payload.selected_pet;
    if (typeof selectedPet === "string") {
      state.selectedPet = selectedPet;
      notify();
    }
  });

  async function loadSettings() {
    try {
      state.settings = await invoke("get_settings");
    } catch (e) {
      state.settings = { theme: "genesis", language: "zh-Hans" };
    }
    notify();
  }
  async function loadBehaviorTelemetryStatus() {
    try {
      state.behaviorTelemetry = await invoke("get_behavior_telemetry_status");
    } catch (e) {
      state.behaviorTelemetry = {
        configured: false,
        enabled: false,
        collection_period_id: null,
        withdrawal_pending: false,
        deletion_pending: false,
        last_deletion_request_id: null,
        retention_days: 35,
        pending_event_count: 0,
        dropped_event_count: 0,
      };
    }
    notify();
    scheduleBehaviorTelemetryStatusPoll();
    return state.behaviorTelemetry;
  }
  async function setBehaviorTelemetryEnabled(enabled) {
    state.behaviorTelemetry = await invoke("set_behavior_telemetry_enabled", { enabled: !!enabled });
    notify();
    scheduleBehaviorTelemetryStatusPoll();
    return state.behaviorTelemetry;
  }
  async function deleteLocalBehaviorTelemetryData() {
    state.behaviorTelemetry = await invoke("delete_local_behavior_telemetry_data");
    notify();
    return state.behaviorTelemetry;
  }
  async function deleteRemoteBehaviorTelemetryData() {
    state.behaviorTelemetry = await invoke("delete_remote_behavior_telemetry_data");
    notify();
    scheduleBehaviorTelemetryStatusPoll();
    return state.behaviorTelemetry;
  }
  async function loadSelectedPet() {
    try {
      state.selectedPet = await invoke("get_selected_pet");
    } catch (e) {
      state.selectedPet = "lingling";
    }
    notify();
  }
  async function setSelectedPet(id) {
    return await invoke("set_selected_pet", { id: id });
  }
  async function loadEffectiveModelConfig(sessionId) {
    var requestedSessionId = arguments.length ? (sessionId || null) : (state.activeSessionId || null);
    try {
      var config = await invoke("get_effective_model_config", { sessionId: requestedSessionId });
      if (requestedSessionId !== (state.activeSessionId || null)) return;
      state.effectiveModelConfig = modelAvailabilityError(requestedSessionId) ? null : config;
    } catch (e) {
      state.effectiveModelConfig = null;
    }
    notify();
  }
  var settingsWriteQueue = Promise.resolve();
  function enqueueSettingsWrite(write) {
    var pending = settingsWriteQueue.then(write, write);
    settingsWriteQueue = pending.then(function () {}, function () {});
    return pending;
  }
  async function saveSettings(patch) {
    return enqueueSettingsWrite(async function () {
      try {
        state.settings = await invoke("update_settings", { patch: patch });
        await loadEffectiveModelConfig();
        notify();
        return true;
      } catch (e) {
        console.warn("save settings failed", e);
        return false;
      }
    });
  }
  async function saveSettingsAndRestart(patch) {
    return enqueueSettingsWrite(async function () {
      try {
        await invoke("save_settings_and_restart", { patch: patch });
        return true;
      } catch (e) {
        console.warn("save settings and restart failed", e);
        return false;
      }
    });
  }
  async function saveSearchSettings(search) {
    return enqueueSettingsWrite(async function () {
      try {
        state.settings = await invoke("update_search_settings", { search: search });
        await loadEffectiveModelConfig();
        notify();
        return true;
      } catch (e) {
        console.warn("save search settings failed", e);
        return false;
      }
    });
  }
  async function saveSearchSettingsAndRestart(search) {
    return enqueueSettingsWrite(async function () {
      try {
        await invoke("save_search_settings_and_restart", { search: search });
        return true;
      } catch (e) {
        console.warn("save search settings and restart failed", e);
        return false;
      }
    });
  }

  function llmApiBackendUserState(status) {
    if (!status) return "unknown";
    if (status.stale === true) return "unknown";
    if (status.backend_user_state === "exists" || status.backend_user_state === "not_exists" || status.backend_user_state === "unknown") {
      return status.backend_user_state;
    }
    return status.backend_user_exists ? "exists" : "not_exists";
  }

  function llmApiAccountKnownExists(status) {
    return llmApiBackendUserState(status) === "exists";
  }

  function llmApiAccountStateIsKnown(status) {
    var backendUserState = llmApiBackendUserState(status);
    return backendUserState === "exists" || backendUserState === "not_exists";
  }

  function llmApiBuiltinIsReady(status) {
    return llmApiAccountKnownExists(status)
      && status.stale !== true
      && status.enabled === true
      && String(status.provisioning_status || "").trim().toLowerCase() === "ready";
  }

  var LLMAPI_STARTUP_RETRY_DELAYS_MS = [2000, 5000, 10000, 30000];

  function llmApiStartupRetryDelay(attempt) {
    return LLMAPI_STARTUP_RETRY_DELAYS_MS[
      Math.min(attempt, LLMAPI_STARTUP_RETRY_DELAYS_MS.length - 1)
    ];
  }

  function waitForLlmApiStartupRetry(delayMs) {
    return new Promise(function (resolve) { setTimeout(resolve, delayMs); });
  }

  async function refreshLlmApiState(options) {
    options = options || {};
    var refreshModels = options.refreshModels !== false;
    var refreshSavedModels = !!options.refreshSavedModels;
    var status = null;
    var models = null;
    var refreshRevision = authoritativeAccountRevision;
    try {
      status = await invoke("get_llmapi_status");
      if (llmApiAccountIsAuthoritativelyUnavailable(status)) {
        authoritativeAccountRevision += 1;
        refreshRevision = authoritativeAccountRevision;
      } else if (refreshRevision !== authoritativeAccountRevision) {
        return { status: state.llmApiStatus, models: null, superseded: true };
      }
      // Preserve an established identity across transient failures, but expose
      // authentication failures so the account recovery form remains reachable.
      if (status.stale === true && status.auto_login_failed === true && llmApiAccountKnownExists(state.llmApiStatus)) {
        status = Object.assign({}, state.llmApiStatus, status, {
          backend_user_exists: true,
          backend_user_state: "exists",
        });
        state.llmApiStatus = status;
        state.effectiveModelConfig = null;
      } else if (llmApiAccountStateIsKnown(status) || !llmApiAccountStateIsKnown(state.llmApiStatus)) {
        state.llmApiStatus = status;
        if (llmApiAccountIsAuthoritativelyUnavailable(status)) {
          state.llmApiModels = null;
          state.effectiveModelConfig = null;
        } else if (modelAvailabilityError(state.activeSessionId)) {
          state.effectiveModelConfig = null;
        }
      }
    } catch (e) {
      console.warn("get llmapi status failed", e);
      // Keep the last known account state. A transport failure is not proof
      // that the backend account disappeared.
      notify();
      throw e;
    }
    if (refreshModels && llmApiBuiltinIsReady(status)) {
      try {
        var modelsRevision = authoritativeAccountRevision;
        models = await invoke("get_llmapi_models");
        if (modelsRevision === authoritativeAccountRevision && llmApiBuiltinIsReady(state.llmApiStatus)) {
          state.llmApiModels = models;
        }
      } catch (e) {
        console.warn("get llmapi models failed", e);
        // Preserve the last successfully synchronized model list.
        notify();
        throw e;
      }
    } else if (llmApiAccountIsAuthoritativelyUnavailable(status)) {
      state.llmApiModels = null;
    }
    if (refreshSavedModels) await loadModels();
    notify();
    return { status: status, models: models };
  }

  async function getLlmApiStatus() {
    var result = await refreshLlmApiState({ refreshModels: false });
    return result.status;
  }

  async function getLlmApiModels() {
    if (!llmApiBuiltinIsReady(state.llmApiStatus)) {
      state.llmApiModels = null;
      notify();
      return null;
    }
    var modelsRevision = authoritativeAccountRevision;
    var models = await invoke("get_llmapi_models");
    if (modelsRevision === authoritativeAccountRevision && llmApiBuiltinIsReady(state.llmApiStatus)) {
      state.llmApiModels = models;
    }
    notify();
    return models;
  }

  async function refreshLlmApi() {
    return await refreshLlmApiState({ refreshModels: true, refreshSavedModels: true });
  }

  async function refreshLlmApiOnStartup() {
    var retryAttempt = 0;
    while (true) {
      var status = null;
      try {
        status = await getLlmApiStatus();
      } catch (e) {
        console.warn("load llmapi account status failed", e);
      }

      if (llmApiAccountStateIsKnown(status)) {
        if (llmApiBuiltinIsReady(status)) {
          try {
            await getLlmApiModels();
          } catch (e) {
            console.warn("load llmapi models failed", e);
          }
        }
        try {
          await loadModels();
        } catch (e) {
          console.warn("reload saved models after llmapi account refresh failed", e);
        }
        return status;
      }

      var retryDelayMs = llmApiStartupRetryDelay(retryAttempt);
      retryAttempt += 1;
      console.warn("llmapi account status is unknown; retrying in " + retryDelayMs + "ms");
      await waitForLlmApiStartupRetry(retryDelayMs);
    }
  }

  async function setLlmApiDefaultModel(model) {
    var models = await invoke("set_llmapi_default_model", { model: model });
    state.llmApiModels = models;
    await loadSettings();
    await loadModels();
    notify();
    return models;
  }

  async function ensureLlmApiBinding() {
    var result = await invoke("ensure_llmapi_binding");
    await refreshLlmApiState({ refreshSavedModels: true });
    return result;
  }

  async function prepareBackendManagedModel(modelId) {
    if (!isBuiltinLlmApiModelId(modelId)) return;
    try {
      await ensureLlmApiBinding();
    } catch (error) {
      // 选择动作已经生效；发送消息时会再次执行权威校验并把可读错误返回给用户。
      console.warn("prepare built-in model credential after selection failed", error);
    }
  }

  async function loginLlmApiUser(username, password) {
    var result = await invoke("login_llmapi_user", { username: username, password: password });
    await refreshLlmApiState({ refreshSavedModels: true });
    return result;
  }

  async function saveLlmApiUserSession(userId, accessToken) {
    var result = await invoke("save_llmapi_user_session", { userId: userId, accessToken: accessToken });
    await refreshLlmApiState({ refreshSavedModels: true });
    return result;
  }

  async function retryLlmApiProvisioning(pinvouUserId, deviceBindingId) {
    var result = await invoke("retry_llmapi_provisioning", { pinvouUserId: pinvouUserId, deviceBindingId: deviceBindingId });
    await refreshLlmApiState({ refreshSavedModels: true });
    return result;
  }

  async function setLlmApiUserEnabled(pinvouUserId, enabled) {
    var result = await invoke("set_llmapi_user_enabled", { pinvouUserId: pinvouUserId, enabled: enabled });
    await refreshLlmApiState({ refreshSavedModels: true });
    return result;
  }

  async function getLlmApiAdminOverview(query, status, limit, offset) {
    return await invoke("get_llmapi_admin_overview", {
      query: query || null,
      status: status || null,
      limit: limit == null ? null : limit,
      offset: offset == null ? null : offset,
    });
  }

  async function submitFeedback(request) {
    return await invoke("submit_feedback", { request: request });
  }
  async function discoverLocalVllm(request) {
    return await invoke("discover_local_vllm", { request: request || null });
  }

  // ── MegaCube(GB10) 本地大模型一键引导 ────────────────────────────
  var vllmSetupPollTimer = null;
  var vllmSetupPollStartedAt = 0;
  var VLLM_SETUP_POLL_INTERVAL_MS = 3000;
  var VLLM_SETUP_POLL_TIMEOUT_MS = 12 * 60 * 1000;
  // 首屏检测「预装但未启用」状态;eligible 时前端弹引导框。
  // 开机加载中不弹框，每 3 秒静默复查；12 分钟后仍 starting 则恢复可重试入口。
  // autoPoll 只供内部定时器续接；用户手动检测会重置本轮截止时间。
  // 陈旧检测快照覆盖（审计）：检测与长任务引导（bootstrap_local_vllm）并发时，
  // 旧快照会把已就绪引擎覆盖回 starting。任何新检测与引导完成都递增序号，
  // 在途读取一律作废。社区版后端 detect 恒 stopped / bootstrap 恒 Err（厂商版
  // 语义的桩），此守卫为防御性：后端恢复真实探测时竞态即真实存在。
  var vllmDetectSeq = 0;
  async function detectLocalVllmSetup(options) {
    var autoPoll = !!(options && options.autoPoll);
    var seq = ++vllmDetectSeq;
    if (vllmSetupPollTimer) {
      clearTimeout(vllmSetupPollTimer);
      vllmSetupPollTimer = null;
    }
    if (!autoPoll) vllmSetupPollStartedAt = Date.now();
    try {
      var snapshot = await invoke("detect_local_vllm_setup");
      if (seq !== vllmDetectSeq) return state.vllmSetup; // 已作废的陈旧读取
      state.vllmSetup = snapshot;
    } catch (e) {
      if (seq !== vllmDetectSeq) return state.vllmSetup;
      state.vllmSetup = null; // 检测失败静默,不打扰(等同不弹)
      vllmSetupPollStartedAt = 0;
    }
    if (state.vllmSetup && state.vllmSetup.engine_state === 'starting' && state.vllmSetup.may_offer_setup !== false) {
      var elapsed = Date.now() - vllmSetupPollStartedAt;
      if (vllmSetupPollStartedAt > 0 && elapsed >= VLLM_SETUP_POLL_TIMEOUT_MS) {
        state.vllmSetup = Object.assign({}, state.vllmSetup, {
          engine_state: 'failed',
          eligible: !!state.vllmSetup.may_offer_setup,
          detection_timed_out: true,
        });
        vllmSetupPollStartedAt = 0;
      } else {
        vllmSetupPollTimer = setTimeout(function () {
          vllmSetupPollTimer = null;
          detectLocalVllmSetup({ autoPoll: true });
        }, VLLM_SETUP_POLL_INTERVAL_MS);
      }
    } else {
      vllmSetupPollStartedAt = 0;
    }
    notify();
    return state.vllmSetup; // 返回供设置页「检测本机 vLLM」判断 has_packages
  }
  // 用户点「启用」:后端一次 pkexec 拉起引擎+装 systemd 服务,轮询就绪后写模型配置。
  // 引擎首次载模型可能几分钟,全程 vllmBootstrapping 显示 spinner。
  async function bootstrapLocalVllm() {
    if (state.vllmBootstrapping) return;
    state.vllmBootstrapping = true;
    state.vllmBootstrapError = null;
    state.vllmBootstrapDone = null;
    state.vllmSetupPhase = 'authorizing'; // 后端事件到达前先本地置首阶段(pkexec 阻塞期也有步骤显示)
    state.vllmSetupAttempt = 0;
    notify();
    try {
      state.vllmBootstrapDone = await invoke("bootstrap_local_vllm");
    } catch (e) {
      state.vllmBootstrapError = String(e && e.message ? e.message : e);
    }
    vllmDetectSeq++; // 引导完成：作废在途的陈旧检测读取（审计）
    state.vllmBootstrapping = false;
    notify();
    // 作废在途读取会中断 autoPoll 续排链（被作废的检测不再续排定时器），
    // 引导完成后主动重检一次，让引擎就绪状态及时收敛（审计补充）。
    detectLocalVllmSetup({ autoPoll: true });
  }
  // 点「跳过」:仅本次会话内不再弹(不写持久标记,下次启动若仍未配好会再次友好提示)。
  function dismissVllmSetup() {
    state.vllmSetupDismissed = true;
    notify();
  }
  // 点「不再提醒 → 确认」:持久婉拒,开机引导框不再自动弹(仍可在设置→模型管理手动启用)。
  async function declineVllmSetup() {
    try { await invoke("decline_local_vllm_setup"); } catch (e) { /* 持久失败也先隐藏本会话,不阻断 */ }
    state.vllmSetupDismissed = true;
    notify();
  }
  async function getEffectiveModelConfig(sessionId) {
    return await invoke("get_effective_model_config", {
      sessionId: arguments.length ? (sessionId || null) : (state.activeSessionId || null),
    });
  }
  // 当前有效模型的图片输入能力(普通会话选图即时警告用);后端按会话模型绑定解析。
  async function getImageInputCapability(sessionId) {
    return await invoke("get_image_input_capability", {
      sessionId: arguments.length ? (sessionId || null) : (state.activeSessionId || null),
    });
  }

  // ── 模型列表(「添加模型」方案)─────────────────────────────────
  // 整表覆盖加载：保存/删除/切换链式 loadModels 并发时旧列表不得覆盖新列表
  // （审计 b）。请求序号后发者胜（同 vllmDetectSeq 模式）。
  var modelsLoadSeq = 0;
  async function loadModels() {
    var seq = ++modelsLoadSeq;
    try {
      var v = await invoke("list_models");
      if (seq !== modelsLoadSeq) return;
      state.savedModels = (v && v.models) || [];
      state.activeModelId = (v && v.active_model_id) || null;
    } catch (e) {
      if (seq !== modelsLoadSeq) return;
      state.savedModels = []; state.activeModelId = null;
    }
    notify();
  }
  // model 对象字段须是 snake_case(SavedModel serde):
  // {id,name,preset,context_window_tokens,max_output_tokens,model,base_url,api_key,credential_action,image_capability_override,vision_model_id}
 function analyticsOperationId(prefix) {
   var cryptoApi = globalThis.crypto;
   if (cryptoApi && typeof cryptoApi.getRandomValues === 'function') {
     var bytes = new Uint8Array(12);
     cryptoApi.getRandomValues(bytes);
     return `${prefix}_${Array.from(bytes, function (byte) { return byte.toString(16).padStart(2, '0'); }).join('')}`;
   }
   return `${prefix}_${Date.now().toString(36)}${Math.random().toString(36).slice(2)}${Math.random().toString(36).slice(2)}`;
 }
 function analyticsProviderKey(model) {
   const raw = String(model.vendor || model.preset || 'unknown').toLowerCase();
   const normalized = raw.replace(/[^a-z0-9_.-]+/g, '_').replace(/^[^a-z0-9]+/, '').slice(0, 64);
   return normalized || 'unknown';
 }
 function hasConfiguredUserModel() {
   return (state.savedModels || []).some(item => {
     if (item && item.kind === 'builtin_llmapi') return false;
     const credential = item.credential_state || (item.has_secret ? 'configured' : 'missing');
     return credential === 'configured' || credential === 'env_override';
   });
 }
 async function trackProviderSetup(request) {
   try { await invoke("track_behavior_event", { request }); } catch (_) { /* analytics is best effort */ }
 }
 async function saveModel(model) {
   const operationId = analyticsOperationId('provider');
   const providerKey = analyticsProviderKey(model);
   const startedAt = Date.now();
   const initialSetup = !hasConfiguredUserModel();
   if (initialSetup) {
     await trackProviderSetup({ eventName: 'onboarding_started', entryPoint: 'settings' });
   }
   await trackProviderSetup({
     eventName: 'provider_setup_started', providerKey, entryPoint: 'settings',
     operationId, attemptIndex: 1,
   });
   try {
     await invoke("save_model", { model: model });
     const durationMs = Math.max(0, Date.now() - startedAt);
     await trackProviderSetup({
       eventName: 'provider_setup_completed', providerKey, operationId,
       attemptIndex: 1, durationMs,
     });
     if (initialSetup) {
       await trackProviderSetup({
         eventName: 'onboarding_completed', durationMs, providerConfigured: true,
       });
       await trackProviderSetup({
         eventName: 'task_environment_ready', readinessSource: 'provider_setup', providerKey,
       });
     }
     await loadModels();
     await loadSettings();
     await loadEffectiveModelConfig();
   } catch (error) {
     await trackProviderSetup({
       eventName: 'provider_setup_failed', providerKey, operationId,
       attemptIndex: 1, errorCategory: 'validation_failed',
     });
     throw error;
   }
 }
 async function revealModelApiKey(id) {
   return await invoke("reveal_model_api_key", { id: id });
 }
 async function deleteModel(id) {
   await invoke("delete_model", { id: id });
   await loadModels();
   await loadSettings();
   await loadEffectiveModelConfig();
  }
  async function setActiveModel(id) {
    await invoke("set_active_model", { id: id });
    await loadModels();
    await loadSettings();
    await loadEffectiveModelConfig();
    await prepareBackendManagedModel(id);
  }
  // 读某会话当前绑定的模型 id(切会话时刷新 chip)。
  async function loadSessionModel(sessionId) {
    var requestedSessionId = sessionId || null;
    var results = await Promise.all([
      requestedSessionId
        ? invoke("get_session_model_id", { sessionId: requestedSessionId }).catch(function () { return null; })
        : Promise.resolve(null),
      invoke("get_effective_model_config", { sessionId: requestedSessionId }).catch(function () { return null; }),
    ]);
    if (requestedSessionId !== (state.activeSessionId || null)) return;
    state.currentSessionModelId = results[0];
    state.effectiveModelConfig = modelAvailabilityError(requestedSessionId) ? null : results[1];
    notify();
  }
  // 切当前会话模型(chip 热切)。无 session(草稿态)时改全局默认。
  async function switchModel(sessionId, modelId) {
    if (sessionId) {
      await invoke("set_session_model", { sessionId: sessionId, modelId: modelId });
      await loadSessionModel(sessionId);
      await prepareBackendManagedModel(modelId);
    } else {
      await setActiveModel(modelId);
    }
  }
  async function testModelConnection(baseUrl, apiKey, modelId) {
    return await invoke("test_model_connection", { baseUrl: baseUrl, apiKey: apiKey, modelId: modelId || null });
  }
  // 测试图片输入能力(设计 §7.3):用当前表单的 model/base_url/key 发一张内置纯色图,
  // 仅由模型编辑弹窗主动点击触发,无任何启动/定时自动测试。
  async function testImageInputCapability(model, baseUrl, apiKey, modelId) {
    return await invoke("test_image_input_capability", { model: model, baseUrl: baseUrl, apiKey: apiKey, modelId: modelId || null });
  }
  async function testSearchProvider(provider, apiKey) {
    return await invoke("test_search_provider", { provider: provider, apiKey: apiKey || null });
  }

    return {
      loadSettings: loadSettings,
      loadBehaviorTelemetryStatus: loadBehaviorTelemetryStatus,
      setBehaviorTelemetryEnabled: setBehaviorTelemetryEnabled,
      deleteLocalBehaviorTelemetryData: deleteLocalBehaviorTelemetryData,
      deleteRemoteBehaviorTelemetryData: deleteRemoteBehaviorTelemetryData,
      loadSelectedPet: loadSelectedPet,
      setSelectedPet: setSelectedPet,
      loadEffectiveModelConfig: loadEffectiveModelConfig,
      saveSettings: saveSettings,
      saveSettingsAndRestart: saveSettingsAndRestart,
      observeInvokeError: observeInvokeError,
      modelAvailabilityError: modelAvailabilityError,
      refreshLlmApiOnStartup: refreshLlmApiOnStartup,
      refreshLlmApi: refreshLlmApi,
      getLlmApiStatus: getLlmApiStatus,
      getLlmApiModels: getLlmApiModels,
      setLlmApiDefaultModel: setLlmApiDefaultModel,
      ensureLlmApiBinding: ensureLlmApiBinding,
      loginLlmApiUser: loginLlmApiUser,
      saveLlmApiUserSession: saveLlmApiUserSession,
      retryLlmApiProvisioning: retryLlmApiProvisioning,
      setLlmApiUserEnabled: setLlmApiUserEnabled,
      getLlmApiAdminOverview: getLlmApiAdminOverview,
      saveSearchSettings: saveSearchSettings,
      saveSearchSettingsAndRestart: saveSearchSettingsAndRestart,
      submitFeedback: submitFeedback,
      discoverLocalVllm: discoverLocalVllm,
      detectLocalVllmSetup: detectLocalVllmSetup,
      bootstrapLocalVllm: bootstrapLocalVllm,
      dismissVllmSetup: dismissVllmSetup,
      declineVllmSetup: declineVllmSetup,
      getEffectiveModelConfig: getEffectiveModelConfig,
      getImageInputCapability: getImageInputCapability,
      loadModels: loadModels,
      saveModel: saveModel,
      revealModelApiKey: revealModelApiKey,
      deleteModel: deleteModel,
      setActiveModel: setActiveModel,
      loadSessionModel: loadSessionModel,
      switchModel: switchModel,
      testModelConnection: testModelConnection,
      testImageInputCapability: testImageInputCapability,
      testSearchProvider: testSearchProvider
    };
  };
})(window);
