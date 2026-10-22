import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const source = relative => fs.readFileSync(new URL(`../src/${relative}`, import.meta.url), 'utf8');

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

function boot(platform) {
  const listeners = new Map();
  const selected = new Map([['session-a', 'A'], ['session-b', 'B']]);
  let interceptConfig = null;
  let state;
  const invoke = async (command, args) => {
    if (command === 'list_models') return { models: [], active_model_id: 'A' };
    if (command === 'get_settings') return { advanced: {} };
    if (command === 'set_session_model') { selected.set(args.sessionId, args.modelId); return null; }
    if (command === 'get_session_model_id') return selected.get(args.sessionId) || null;
    if (command === 'get_effective_model_config') {
      const config = { model: selected.get(args.sessionId) || 'A' };
      return interceptConfig ? interceptConfig(config, args.sessionId) : config;
    }
    return null;
  };
  const localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
  const window = {
    PinvouPlatform: { kind: 'web', isWeb: true, capabilities: {}, can: () => false, canInvoke: () => false },
    __TAURI__: {
      core: { invoke },
      event: { listen(name, handler) { listeners.set(name, handler); return Promise.resolve(() => {}); } },
      dialog: { open: async () => null },
    },
    localStorage, location: { search: '', href: 'https://example.test/' },
    performance: { now: () => 0 }, addEventListener() {},
    setTimeout, clearTimeout,
  };
  const context = vm.createContext({
    window, localStorage, console, setTimeout, clearTimeout,
    setInterval: () => 0, clearInterval() {},
    document: { readyState: 'loading', addEventListener() {} },
    navigator: {}, URL, URLSearchParams,
    // Retain the real Web bridge state only to seed navigation in this
    // transport fixture. Its public snapshots still use a real deep clone.
    structuredClone(value) { state = value; return structuredClone(value); },
  });
  vm.runInContext(source('shared/bridge-shared-helpers.js'), context);
  let models;
  if (platform === 'tauri') {
    state = { settings: {}, activeSessionId: 'session-a', currentSessionModelId: 'A' };
    vm.runInContext(source('platform/tauri/bridge/settings.js'), context);
    models = window.__PINVOU_TAURI_BRIDGE_FEATURES__.settings({
      state, invoke, notify() {}, listen(name, handler) { listeners.set(name, handler); },
    });
  } else {
    vm.runInContext(source('platform/web/bridge.js'), context);
    models = window.TauriBridge;
    models.getState();
    state.activeSessionId = 'session-a';
    state.currentSessionModelId = 'A';
  }
  return {
    models, state, selected,
    refresh: () => listeners.get('models:changed')({ payload: { model_ids: ['A'] } }),
    holdNextConfig() {
      const entered = deferred();
      const response = deferred();
      interceptConfig = (config) => {
        interceptConfig = null;
        entered.resolve(config);
        return response.promise;
      };
      return { entered: entered.promise, resolve: response.resolve, reject: response.reject };
    },
  };
}

for (const platform of ['tauri', 'web']) {
  for (const failure of [false, true]) {
    test(`${platform}: a delayed store refresh ${failure ? 'error' : 'response'} cannot replace a newer selection`, async () => {
      const harness = boot(platform);
      const old = harness.holdNextConfig();
      const refresh = harness.refresh();
      assert.equal((await old.entered).model, 'A');
      await harness.models.switchModel('session-a', 'B');
      assert.equal(harness.state.currentSessionModelId, 'B');
      assert.equal(harness.state.effectiveModelConfig.model, 'B');
      if (failure) old.reject(new Error('old transport failure'));
      else old.resolve({ model: 'A' });
      await refresh;
      assert.equal(harness.state.currentSessionModelId, 'B');
      assert.equal(harness.state.effectiveModelConfig.model, 'B');
    });
  }

  test(`${platform}: a newer background config does not discard a pending selection ID`, async () => {
    const harness = boot(platform);
    const old = harness.holdNextConfig();
    const selection = harness.models.switchModel('session-a', 'B');
    assert.equal((await old.entered).model, 'B');
    await harness.refresh();
    assert.equal(harness.state.effectiveModelConfig.model, 'B');
    old.resolve({ model: 'B-before-refresh' });
    await selection;
    assert.equal(harness.state.currentSessionModelId, 'B');
    assert.equal(harness.state.effectiveModelConfig.model, 'B');
  });

  test(`${platform}: an older session-model read cannot restore the previous selection`, async () => {
    const harness = boot(platform);
    const old = harness.holdNextConfig();
    const first = harness.models.switchModel('session-a', 'B');
    await old.entered;
    await harness.models.switchModel('session-a', 'C');
    old.resolve({ model: 'B' });
    await first;
    assert.equal(harness.state.currentSessionModelId, 'C');
    assert.equal(harness.state.effectiveModelConfig.model, 'C');
  });

  for (const returnToOriginal of [false, true]) {
    test(`${platform}: store refresh respects a session ${returnToOriginal ? 'round trip' : 'change'}`, async () => {
      const harness = boot(platform);
      const old = harness.holdNextConfig();
      const refresh = harness.refresh();
      await old.entered;
      harness.state.activeSessionId = 'session-b';
      await harness.models.loadSessionModel('session-b');
      if (returnToOriginal) {
        harness.state.activeSessionId = 'session-a';
        await harness.models.switchModel('session-a', 'C');
      }
      const expected = returnToOriginal ? 'C' : 'B';
      old.resolve({ model: 'A' });
      await refresh;
      assert.equal(harness.state.currentSessionModelId, expected);
      assert.equal(harness.state.effectiveModelConfig.model, expected);
    });
  }
}

test('the shared config loader observes the same ordering across bridge clusters', async () => {
  const window = {};
  const context = vm.createContext({ window });
  vm.runInContext(source('shared/bridge-shared-helpers.js'), context);
  const state = { activeSessionId: 'session' };
  const pending = deferred();
  const old = window.PinvouBridgeShared.create('tauriSettings', {
    state, notify() {}, invoke: () => pending.promise,
  });
  const latest = window.PinvouBridgeShared.create('web', {
    state, notify() {}, invoke: async () => ({ model: 'B' }),
  });
  const earlier = old.loadEffectiveModelConfig();
  await latest.loadEffectiveModelConfig();
  pending.reject(new Error('late failure'));
  await earlier;
  assert.equal(state.effectiveModelConfig.model, 'B');
});
