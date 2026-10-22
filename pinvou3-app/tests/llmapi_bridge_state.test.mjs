import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const bridgeRoot = path.join(root, 'src', 'platform', 'tauri', 'bridge');

function loadFactories() {
  const windowObject = {};
  const context = vm.createContext({ window: windowObject, console, setTimeout, clearTimeout });
  const sharedRoot = path.join(root, 'src', 'shared');
  // #566 共享模块移植：feature 工厂的共享函数体收敛在 bridge-shared-helpers.js，
  // 必须先于任何 feature 工厂加载（与 index.html 的脚本顺序一致）。
  vm.runInContext(fs.readFileSync(path.join(sharedRoot, 'bridge-shared-helpers.js'), 'utf8'), context, { filename: 'shared/bridge-shared-helpers.js' });
  for (const file of ['settings.js', 'chat.js']) {
    vm.runInContext(fs.readFileSync(path.join(bridgeRoot, file), 'utf8'), context, { filename: file });
  }
  return { factories: windowObject.__PINVOU_TAURI_BRIDGE_FEATURES__, windowObject };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function baseState() {
  return {
    activeSessionId: 'session-1',
    activeModelId: 'builtin_llmapi',
    currentSessionModelId: 'builtin_llmapi',
    effectiveModelConfig: { model: 'deepseek-v4-flash' },
    savedModels: [
      { id: 'builtin_llmapi', kind: 'builtin_llmapi' },
      { id: 'custom-model', kind: null },
    ],
    llmApiStatus: {
      backend_user_exists: true,
      backend_user_state: 'exists',
      stale: false,
      enabled: true,
      provisioning_status: 'ready',
    },
    llmApiModels: { available_models: ['deepseek-v4-flash'] },
  };
}

const { factories, windowObject } = loadFactories();

{
  const state = baseState();
  const settings = factories.settings({
    state,
    notify() {},
    listen() {},
    invoke: async () => null,
  });
  assert.equal(
    settings.observeInvokeError(new Error('send_user_message failed: service_disabled: user has been banned'), 'chat'),
    true,
  );
  assert.equal(state.llmApiStatus.backend_user_state, 'not_exists');
  assert.equal(state.llmApiModels, null);
  assert.equal(state.effectiveModelConfig, null);
  assert.match(settings.modelAvailabilityError('session-1'), /选择其他模型/);
  windowObject.PinvouTauriBridgeStrings = { builtinModelUnavailable: 'Select another model first.' };
  assert.equal(settings.modelAvailabilityError('session-1'), 'Select another model first.');
  delete windowObject.PinvouTauriBridgeStrings;
}

{
  const state = baseState();
  state.activeModelId = 'custom-model';
  state.currentSessionModelId = null;
  const settings = factories.settings({
    state,
    notify() {},
    listen() {},
    invoke: async () => null,
  });
  assert.equal(
    settings.observeInvokeError(new Error('service_disabled: unrelated domain'), 'submit_feedback'),
    false,
    'an unrelated command must not mutate LLM API account state when the effective model is custom',
  );
  assert.equal(state.llmApiStatus.backend_user_state, 'exists');
}

{
  const statusRequest = deferred();
  const state = baseState();
  const settings = factories.settings({
    state,
    notify() {},
    listen() {},
    invoke(command) {
      if (command === 'get_llmapi_status') return statusRequest.promise;
      return Promise.resolve(null);
    },
  });
  const refresh = settings.getLlmApiStatus();
  settings.observeInvokeError(new Error('service_disabled: account disabled'), 'chat');
  statusRequest.resolve({
    backend_user_exists: true,
    backend_user_state: 'exists',
    stale: false,
    enabled: true,
    provisioning_status: 'ready',
  });
  const returned = await refresh;
  assert.equal(returned.backend_user_state, 'not_exists');
  assert.equal(state.llmApiStatus.backend_user_state, 'not_exists');
}

{
  const state = baseState();
  const settings = factories.settings({
    state,
    notify() {},
    listen() {},
    invoke: async command => command === 'get_llmapi_status' ? {
      backend_user_exists: true,
      backend_user_state: 'exists',
      stale: true,
      auto_login_failed: true,
      enabled: true,
      provisioning_status: 'ready',
      quota: null,
    } : null,
  });
  const status = await settings.getLlmApiStatus();
  assert.equal(status.stale, true);
  assert.equal(status.auto_login_failed, true);
  assert.equal(state.llmApiStatus.backend_user_state, 'exists');
}

{
  const modelsRequest = deferred();
  const state = baseState();
  const settings = factories.settings({
    state,
    notify() {},
    listen() {},
    invoke(command) {
      if (command === 'get_llmapi_models') return modelsRequest.promise;
      return Promise.resolve(null);
    },
  });
  const refresh = settings.getLlmApiModels();
  settings.observeInvokeError(new Error('service_disabled: account disabled'), 'chat');
  modelsRequest.resolve({ available_models: ['stale-model'] });
  await refresh;
  assert.equal(state.llmApiModels, null, 'a model response started before the authoritative error must be ignored');
}

{
  const state = baseState();
  const invokedCommands = [];
  const settings = factories.settings({
    state,
    notify() {},
    listen() {},
    invoke: async command => {
      invokedCommands.push(command);
      if (command === 'get_llmapi_status') {
        return {
          backend_user_exists: true,
          backend_user_state: 'exists',
          stale: false,
          enabled: false,
          provisioning_status: 'disabled',
        };
      }
      return { available_models: ['must-not-be-used'] };
    },
  });
  const result = await settings.refreshLlmApi();
  assert.equal(result.status.enabled, false);
  assert.equal(invokedCommands.includes('get_llmapi_models'), false);
  assert.equal(state.llmApiModels, null);
  assert.equal(state.effectiveModelConfig, null);
}

{
  const state = baseState();
  state.llmApiStatus = {
    backend_user_exists: false,
    backend_user_state: 'not_exists',
    stale: false,
    enabled: false,
    provisioning_status: 'disabled',
  };
  const invokes = [];
  let notifications = 0;
  const settings = factories.settings({
    state,
    notify() {},
    listen() {},
    invoke: async () => null,
  });
  Object.assign(state, {
    chatItems: [],
    messages: [],
    attachments: [],
    busy: false,
  });
  const chat = factories.chat({
    state,
    invoke(command) { invokes.push(command); return Promise.resolve(null); },
    notify() { notifications += 1; },
    TAURI: null,
    sessionStates: {},
    turnUsageDirty: {},
    personaPlaceholderTitles: {},
    renderMarkdown: String,
    safeConsoleInfo() {},
    bt(key) { return key; },
    runSyncOnSession(_sessionId, action) { action(); },
    startThinking() {},
    stopThinking() {},
    ensureSessionBufferLoaded: async () => {},
    ensureSession: async () => {},
    getBuffer() { return null; },
    reconcileRemoteTurn: async () => true,
    markRemoteTurn() {},
    clearAttachments() {},
    isScheduledRunSession() { return false; },
    basename(value) { return String(value || ''); },
    extractArtifactPath() { return null; },
    parseScheduledTaskDraftFromText() { return null; },
    autoCreateScheduledTaskDraft() {},
    modelAvailabilityError: settings.modelAvailabilityError,
    itemIdSeq: 0,
    pendingAssistantText: '',
    pendingAssistantBlocks: [],
    currentStreamText: '',
    currentStreamId: 0,
  });
  assert.equal(
    await chat.doSendFor('session-1', 'interactive', 'interactive', [], null, false, false),
    false,
    'an interactive send should surface the visible error without rejecting its UI event handler',
  );
  await assert.rejects(
    chat.doSendFor('session-1', 'hello', 'hello', [], null, false, true),
    /选择其他模型/,
  );
  assert.equal(invokes.includes('chat'), false, 'the disabled hidden model must be blocked before invoking chat');
  assert.equal(state.chatItems.some(item => item.turnErrorNotice && /选择其他模型/.test(item.text)), true);
  assert.ok(notifications > 0);
}

console.log('LLM API bridge state and send guards passed');
