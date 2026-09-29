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
      // The computer_use master switch is persisted via the dedicated command through
      // prefs (not the update_settings patch); at cold start with no session yet, the
      // persisted value lands first (a session-less computer_use_get_status read fills
      // the rest of the slice from the settings page's mount refresh).
      // Note: this mirror runs on every settings reload (loadSettings is
      // re-invoked after model save/delete/switch too) — it stays idempotent
      // because the persisted value always matches the last accepted
      // computer_use_set_enabled, and the runtime toggle's optimistic flip is
      // reverted by the bridge on failure.
      const persistedEnabled = !!(state.settings && state.settings.computer_use && state.settings.computer_use.enabled);
      if (state.computerUse && state.computerUse.enabled !== persistedEnabled) {
        state.computerUse = Object.assign({}, state.computerUse, { enabled: persistedEnabled });
      }
    } catch (e) {
      // Backend unreachable = nothing to judge; fall back to following the
      // system for the color scheme (color_scheme: system).
      state.settings = { theme: "genesis", color_scheme: "system", language: "zh-Hans" };
    }
    notify();
  }
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
      state.effectiveModelConfig = config;
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

  async function submitFeedback(request) {
    return await invoke("submit_feedback", { request: request });
  }
  async function discoverLocalVllm(request) {
    return await invoke("discover_local_vllm", { request: request || null });
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
  // （审计 b）。请求序号后发者胜（同本文件请求序号模式）。
  var modelsLoadSeq = 0;
  async function loadModels() {
    var seq = ++modelsLoadSeq;
    try {
      var v = await invoke("list_models");
      if (seq !== modelsLoadSeq) return;
      state.savedModels = (v && v.models) || [];
      state.activeModelId = (v && v.active_model_id) || null;
      state.defaultModelRef = (v && v.default_model_ref) || null;
      state.modelsLoaded = true;
    } catch (e) {
      if (seq !== modelsLoadSeq) return;
      state.savedModels = []; state.activeModelId = null; state.defaultModelRef = null;
      state.modelsLoaded = false;
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
     if (item && item.kind === 'builtin_account_platform') return false;
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
  }
  // 读某会话当前绑定的模型 id(切会话时刷新 chip)。
  async function loadSessionModel(sessionId) {
    var requestedSessionId = sessionId || null;
    var results = await Promise.all([
      requestedSessionId
        ? invoke("get_session_model_id", { sessionId: requestedSessionId }).catch(function () { return null; })
        : Promise.resolve(null),
      requestedSessionId
        ? invoke("get_session_model_ref", { sessionId: requestedSessionId }).catch(function () { return null; })
        : Promise.resolve(null),
      invoke("get_effective_model_config", { sessionId: requestedSessionId }).catch(function () { return null; }),
    ]);
    if (requestedSessionId !== (state.activeSessionId || null)) return;
    state.currentSessionModelId = results[0];
    state.currentSessionModelRef = results[1];
    state.effectiveModelConfig = results[2];
    notify();
  }
  // 切当前会话模型；草稿态只寄存到首条消息物化，不改全局默认。
  async function switchModelRef(sessionId, modelRef) {
    if (!modelRef || !modelRef.source) throw new Error("model selection is required");
    if (!sessionId) {
      state.pendingDraftModelRef = modelRef;
      notify();
      return;
    }
    if (modelRef.source === "ACCOUNT_PLATFORM") {
      await invoke("account_platform_select_session_model", {
        input: { sessionId: sessionId, modelId: modelRef.model_id },
      });
    } else if (modelRef.source === "LOCAL_CONFIG") {
      await invoke("set_session_model", { sessionId: sessionId, modelId: modelRef.saved_model_id });
    } else {
      throw new Error("unsupported model selection source");
    }
    await loadSessionModel(sessionId);
  }
  async function switchModel(sessionId, modelId) {
    return switchModelRef(sessionId, { source: "LOCAL_CONFIG", saved_model_id: modelId });
  }
  async function testModelConnection(baseUrl, apiKey, modelId) {
    return await invoke("test_model_connection", { baseUrl: baseUrl, apiKey: apiKey, modelId: modelId || null });
  }
  // 测试图片输入能力(设计 §7.3):用当前表单的 model/base_url/key 发一张内置纯色图,
  // 仅由模型编辑弹窗主动点击触发,无任何启动/定时自动测试。
  async function testImageInputCapability(model, baseUrl, apiKey, modelId) {
    return await invoke("test_image_input_capability", { model: model, baseUrl: baseUrl, apiKey: apiKey, modelId: modelId || null });
  }
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

    return {
      loadSettings: loadSettings,
      loadBehaviorTelemetryStatus: loadBehaviorTelemetryStatus,
      setBehaviorTelemetryEnabled: setBehaviorTelemetryEnabled,
      loadSelectedPet: loadSelectedPet,
      setSelectedPet: setSelectedPet,
      loadEffectiveModelConfig: loadEffectiveModelConfig,
      saveSettings: saveSettings,
      saveSettingsAndRestart: saveSettingsAndRestart,
      saveSearchSettings: saveSearchSettings,
      saveSearchSettingsAndRestart: saveSearchSettingsAndRestart,
      submitFeedback: submitFeedback,
      discoverLocalVllm: discoverLocalVllm,
      getEffectiveModelConfig: getEffectiveModelConfig,
      getImageInputCapability: getImageInputCapability,
      loadModels: loadModels,
      saveModel: saveModel,
      revealModelApiKey: revealModelApiKey,
      deleteModel: deleteModel,
      setActiveModel: setActiveModel,
      loadSessionModel: loadSessionModel,
      switchModel: switchModel,
      switchModelRef: switchModelRef,
      testModelConnection: testModelConnection,
      testImageInputCapability: testImageInputCapability,
      probeLocalServerKind: probeLocalServerKind,
      testSearchProvider: testSearchProvider
    };
  };
})(window);
