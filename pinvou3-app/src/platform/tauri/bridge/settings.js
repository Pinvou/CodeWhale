/**
 * settings feature for the Tauri bridge.
 * Registered before bridge.js builds the backwards-compatible facade.
 */
(function (root) {
  "use strict";
  var registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};
  registry["settings"] = function (context) {let pinvouSharedtauriSettingsCache = null;
function pinvouSharedtauriSettings() {
  if (!pinvouSharedtauriSettingsCache) pinvouSharedtauriSettingsCache = window.PinvouBridgeShared.create("tauriSettings", { state, invoke, notify, settingsWriteQueue: { get value() { return settingsWriteQueue; }, set value(v) { settingsWriteQueue = v; } }, modelsLoadSeq: { get value() { return modelsLoadSeq; }, set value(v) { modelsLoadSeq = v; } }, loadSessionModel, setActiveModel });
  return pinvouSharedtauriSettingsCache;
}

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
    return !!(status && (
      status.enabled
      || behaviorTelemetryControlPending(status)
      || (status.configured && status.user_enabled !== false)
    ));
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
      window.PinvouBridgeShared.publishSettings(state, Object.assign({}, state.settings, {
        pet: Object.assign({}, state.settings.pet || {}, {
          enabled: !!(e.payload && e.payload.enabled),
        }),
      }));
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

  async function loadSettings() { return pinvouSharedtauriSettings().loadSettings(); }

  // Store tasks finish independently of the visible page. Reload all model
  // consumers after a route commit without changing global/session selection.
  var modelsRefreshPromise = null;
  var modelsRefreshPending = false;
  function refreshChangedModels() {
    modelsRefreshPending = true;
    if (modelsRefreshPromise) return modelsRefreshPromise;
    modelsRefreshPromise = (async function () {
      do {
        modelsRefreshPending = false;
        await loadModels();
        await loadSettings();
        await loadEffectiveModelConfig();
      } while (modelsRefreshPending);
    })().finally(function () { modelsRefreshPromise = null; });
    return modelsRefreshPromise;
  }
  listen("models:changed", function () {
    return refreshChangedModels().catch(function () {
      console.warn("[models] linked model refresh failed");
    });
  });

  async function loadBehaviorTelemetryStatus() {
    try {
      state.behaviorTelemetry = await invoke("get_behavior_telemetry_status");
    } catch (_) {
      state.behaviorTelemetry = {
        configured: false,
        enabled: false,
        user_enabled: null,
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
  async function loadSelectedPet() { return pinvouSharedtauriSettings().loadSelectedPet(); }
  async function setSelectedPet(id) { return pinvouSharedtauriSettings().setSelectedPet(id); }
  async function loadEffectiveModelConfig(sessionId) {
    var requestedSessionId = arguments.length ? (sessionId || null) : (state.activeSessionId || null);
    var read = window.PinvouBridgeShared.beginModelRead(state, requestedSessionId);
    try {
      var config = await invoke("get_effective_model_config", { sessionId: requestedSessionId });
      if (!read.configIsCurrent()) return;
      state.effectiveModelConfig = modelAvailabilityError(requestedSessionId) ? null : config;
    } catch (e) {
      if (!read.configIsCurrent()) return;
      state.effectiveModelConfig = null;
    }
    notify();
  }
  var settingsWriteQueue = Promise.resolve();
  function enqueueSettingsWrite(write) { return pinvouSharedtauriSettings().enqueueSettingsWrite(write); }
  async function saveSettings(patch) {
    return enqueueSettingsWrite(async function () {
      try {
        const settings = await invoke("update_settings", { patch: patch });
        window.PinvouBridgeShared.publishSettings(state, settings);
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
        const settings = await invoke("update_search_settings", { search: search });
        window.PinvouBridgeShared.publishSettings(state, settings);
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

  async function submitFeedback(request) { return pinvouSharedtauriSettings().submitFeedback(request); }
  async function discoverLocalVllm(request) { return pinvouSharedtauriSettings().discoverLocalVllm(request); }

  // 当前有效模型的图片输入能力(普通会话选图即时警告用);后端按会话模型绑定解析。
  async function getImageInputCapability(sessionId) { return pinvouSharedtauriSettings().getImageInputCapability(sessionId); }

  // ── 模型列表(「添加模型」方案)─────────────────────────────────
  // 整表覆盖加载：保存/删除/切换链式 loadModels 并发时旧列表不得覆盖新列表
  // （审计 b）。请求序号后发者胜（同本文件请求序号模式）。
  var modelsLoadSeq = 0;
  async function loadModels() { return pinvouSharedtauriSettings().loadModels(); }
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
 async function revealModelApiKey(id) { return pinvouSharedtauriSettings().revealModelApiKey(id); }
 async function deleteModel(id) {
   await invoke("delete_model", { id: id });
   await loadModels();
   await loadSettings();
   await loadEffectiveModelConfig();
  }
 async function deleteModelIfStoreLink(id, cardId) {
   var removed = await invoke("delete_model_if_store_link", { id: id, cardId: cardId });
   await loadModels();
   await loadSettings();
   await loadEffectiveModelConfig();
   return removed === true;
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
    var read = window.PinvouBridgeShared.beginModelRead(state, requestedSessionId, true);
    var results = await Promise.all([
      requestedSessionId
        ? invoke("get_session_model_id", { sessionId: requestedSessionId }).catch(function () { return null; })
        : Promise.resolve(null),
      invoke("get_effective_model_config", { sessionId: requestedSessionId }).catch(function () { return null; }),
    ]);
    if (!read.selectionIsCurrent()) return;
    state.currentSessionModelId = results[0];
    if (read.configIsCurrent()) {
      state.effectiveModelConfig = modelAvailabilityError(requestedSessionId) ? null : results[1];
    }
    notify();
  }
  // 切当前会话模型(chip 热切)。无 session(草稿态)时改全局默认。
  async function switchModel(sessionId, modelId) { return pinvouSharedtauriSettings().switchModel(sessionId, modelId); }
  async function testModelConnection(baseUrl, apiKey, modelId) { return pinvouSharedtauriSettings().testModelConnection(baseUrl, apiKey, modelId); }
  // 测试图片输入能力(设计 §7.3):用当前表单的 model/base_url/key 发一张内置纯色图,
  // 仅由模型编辑弹窗主动点击触发,无任何启动/定时自动测试。
  async function testImageInputCapability(model, baseUrl, apiKey, modelId) { return pinvouSharedtauriSettings().testImageInputCapability(model, baseUrl, apiKey, modelId); }
  async function probeLocalServerKind(baseUrl, apiKey, modelId) {
    // 本地/内网 OpenAI 兼容端点的服务类型探测（vllm/ollama/lmstudio/generic）。
    // Rust 侧按 base_url TTL 缓存；命令失败（老版本桌面/命令被拒）在这里 reject，
    // 由消费方 catch 降级为「未知」——吞错伪造成 generic 会让 UI 误报
    // 「该端点不支持思考档位调节」（localProbeTiersForKind('generic') 为 null）。
    // apiKey/modelId follow testModelConnection: a freshly typed form key
    // wins, otherwise the saved credential is read — authenticated vLLM
    // (--api-key) 401s on /v1/models, so probing without credentials
    // misclassifies the authenticated endpoint as generic.
    return invoke("probe_local_server_kind", {
      baseUrl,
      apiKey: apiKey || null,
      modelId: modelId || null,
    });
  }
  async function testSearchProvider(provider, apiKey) {
    return await invoke("test_search_provider", { provider: provider, apiKey: apiKey || null });
  }

  // ── Global bring-to-front shortcut ────────────────────────────────
  // The dedicated Rust command set_global_shortcut_config exclusively owns
  // registration and persistence (update_settings is never used): success =
  // status "registered"/"disabled", and a committed save always reports the
  // accelerator disk now holds — including a disabled save, which may itself
  // have changed the stored combo (null remains possible on
  // "invalid"/"conflict", i.e. nothing was persisted). "invalid"/"conflict"
  // mean the registration was refused (conflict's message is technical
  // detail, console only; the UI shows localized copy). Hard failures (IPC
  // exceptions) resolve null so the caller can roll its toggle back and show
  // the generic failure copy.
  function setGlobalShortcutConfig(enabled, accelerator) {
    const sync = function () {
      return invoke("set_global_shortcut_config", { enabled: !!enabled, accelerator: String(accelerator || "") });
    };
    // Retry once on any invocation failure — a transient IPC error, or a
    // Rust-side persist failure (which rolls the registration back before
    // returning Err). The full re-apply is safe: apply_config is idempotent
    // per (enabled, accelerator) and disk stays authoritative (same shape as
    // setVoiceShortcutEnabled); on persistent failure return null so the
    // caller can roll its optimistic state back. On success the authoritative
    // response is mirrored into state.settings so a settings-page remount
    // reads the persisted combo instead of the startup snapshot.
    return sync().then(function (result) { return applyGlobalShortcutResult(result, enabled); }, function (error) {
      console.warn("[settings] global shortcut config sync failed, retrying once", error);
      return sync().then(function (result) { return applyGlobalShortcutResult(result, enabled); }, function (retryError) {
        console.warn("[settings] global shortcut config sync failed after retry", retryError);
        return null;
      });
    });
  }
  function applyGlobalShortcutResult(result, enabled) {
    if (!result || typeof result.status !== "string") return null;
    if (result.status !== "registered" && result.status !== "disabled") return result;
    if (state.settings) {
      // Same mechanics as the pet:enabled_changed listener above: the bridge
      // owns its settings slice; the React layer only reads it. Mirror the
      // accelerator whenever Rust reports one — a disabled save reports the
      // combo it just persisted, so the next toggle submits it instead of a
      // stale mirror. (A missing accelerator must never regress the snapshot
      // to the raw input.)
      const mirror = { global_shortcut_enabled: !!enabled };
      if (typeof result.accelerator === "string" && result.accelerator) {
        mirror.global_shortcut_accelerator = result.accelerator;
      }
      window.PinvouBridgeShared.publishSettings(state, Object.assign({}, state.settings, mirror));
      notify();
    }
    return result;
  }

  // ── Builtin feature toggles (docs/builtin-toolset-contract.md hook) ──────
  // list_builtin_features is consumed by ChatView (the session-mention feature
  // gate, PR #586); set_builtin_feature_enabled only wraps the command channel
  // for future per-feature settings pages — no consumer yet, no UI.
  async function listBuiltinFeatures() {
    return invoke("list_builtin_features");
  }
  // The Rust command takes `feature_id` (app/commands/builtin.rs); Tauri v2
  // converts camelCase JS keys to snake_case, so the payload must use
  // `featureId` — same convention as get_effective_model_config({ sessionId }).
  async function setBuiltinFeatureEnabled(id, enabled) {
    return invoke("set_builtin_feature_enabled", { featureId: id, enabled });
  }

    return {
      loadSettings: loadSettings,
      loadBehaviorTelemetryStatus: loadBehaviorTelemetryStatus,
      setBehaviorTelemetryEnabled: setBehaviorTelemetryEnabled,
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
      getImageInputCapability: getImageInputCapability,
      loadModels: loadModels,
      saveModel: saveModel,
      revealModelApiKey: revealModelApiKey,
      deleteModel: deleteModel,
      deleteModelIfStoreLink: deleteModelIfStoreLink,
      setActiveModel: setActiveModel,
      loadSessionModel: loadSessionModel,
      switchModel: switchModel,
      testModelConnection: testModelConnection,
      testImageInputCapability: testImageInputCapability,
      probeLocalServerKind: probeLocalServerKind,
      testSearchProvider: testSearchProvider,
      setGlobalShortcutConfig,
      listBuiltinFeatures,
      setBuiltinFeatureEnabled: setBuiltinFeatureEnabled,
    };
  };
})(window);
