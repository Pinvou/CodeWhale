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
  const selectedRefs = new Map([...selected].map(([id, model]) => [id, { source: 'LOCAL_CONFIG', saved_model_id: model }]));
  const calls = [];
  const selectRef = (sessionId, modelRef) => {
    selectedRefs.set(sessionId, structuredClone(modelRef));
    selected.set(sessionId, modelRef.source === 'LOCAL_CONFIG' ? modelRef.saved_model_id : 'builtin_account_platform');
  };
  let interceptConfig = null;
  let state;
  const invoke = async (command, args) => {
    calls.push({ command, args: structuredClone(args || {}) });
    if (command === 'list_models') return { models: [], active_model_id: 'A' };
    if (command === 'get_settings') return { advanced: {} };
    if (command === 'set_session_model') {
      selectRef(args.sessionId, { source: 'LOCAL_CONFIG', saved_model_id: args.modelId });
      return null;
    }
    if (command === 'account_platform_select_session_model') {
      selectRef(args.input.sessionId, { source: 'ACCOUNT_PLATFORM', model_id: args.input.modelId });
      return null;
    }
    if (command === 'get_session_model_ref') return structuredClone(selectedRefs.get(args.sessionId) || null);
    if (command === 'get_session_model_id') return selected.get(args.sessionId) || null;
    if (command === 'get_effective_model_config') {
      const ref = selectedRefs.get(args.sessionId);
      const config = { model: ref?.source === 'ACCOUNT_PLATFORM' ? ref.model_id : selected.get(args.sessionId) || 'A' };
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
    models, state, selected, selectedRefs, calls, selectRef,
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


const typedSelections = [
  { label: 'local', ref: { source: 'LOCAL_CONFIG', saved_model_id: 'B' }, legacyId: 'B', wireModel: 'B' },
  { label: 'account-platform', ref: { source: 'ACCOUNT_PLATFORM', model_id: 'catalog-B' }, legacyId: 'builtin_account_platform', wireModel: 'catalog-B' },
];
const plain = value => structuredClone(value);

for (const platform of ['tauri', 'web']) {
  for (const selection of typedSelections) {
    test(`${platform}: background refresh preserves a pending ${selection.label} ID/ref pair`, async () => {
      const harness = boot(platform);
      const initialRef = { source: 'LOCAL_CONFIG', saved_model_id: 'A' };
      harness.state.currentSessionModelRef = initialRef;
      const old = harness.holdNextConfig();
      const selecting = harness.models.switchModelRef('session-a', selection.ref);
      assert.equal((await old.entered).model, selection.wireModel);
      await harness.refresh();
      assert.equal(harness.state.currentSessionModelId, 'A');
      assert.deepEqual(plain(harness.state.currentSessionModelRef), initialRef);
      assert.equal(harness.state.effectiveModelConfig.model, selection.wireModel);
      old.resolve({ model: 'obsolete-selection-config' });
      await selecting;
      assert.equal(harness.state.currentSessionModelId, selection.legacyId);
      assert.deepEqual(plain(harness.state.currentSessionModelRef), selection.ref);
      assert.equal(harness.state.effectiveModelConfig.model, selection.wireModel);
      const expectedCommand = selection.ref.source === 'LOCAL_CONFIG'
        ? 'set_session_model' : 'account_platform_select_session_model';
      assert.equal(harness.calls.filter(call => call.command === expectedCommand).length, 1);
    });

    test(`${platform}: ${selection.label} selection publishes ID/ref on config failure and returns false`, async () => {
      const harness = boot(platform);
      const authoritative = { model: 'known-model', credential_state: 'missing' };
      harness.state.effectiveModelConfig = authoritative;
      harness.selectRef('session-a', selection.ref);
      const failed = harness.holdNextConfig();
      const reading = harness.models.loadSessionModel('session-a');
      await failed.entered;
      failed.reject(new Error('transient config failure'));
      assert.equal(await reading, false);
      assert.equal(harness.state.currentSessionModelId, selection.legacyId);
      assert.deepEqual(plain(harness.state.currentSessionModelRef), selection.ref);
      assert.equal(harness.state.effectiveModelConfig, authoritative);
    });

    test(`${platform}: draft ${selection.label} choice never changes the global default`, async () => {
      const harness = boot(platform);
      harness.state.activeSessionId = null;
      harness.state.activeModelId = 'A';
      const defaultRef = { source: 'LOCAL_CONFIG', saved_model_id: 'A' };
      harness.state.defaultModelRef = defaultRef;
      const start = harness.calls.length;
      if (selection.ref.source === 'LOCAL_CONFIG') {
        await harness.models.switchModel(null, selection.ref.saved_model_id);
      } else {
        await harness.models.switchModelRef(null, selection.ref);
      }
      assert.deepEqual(plain(harness.state.pendingDraftModelRef), selection.ref);
      assert.equal(harness.state.activeModelId, 'A');
      assert.equal(harness.state.defaultModelRef, defaultRef);
      assert.deepEqual(harness.calls.slice(start), [], 'draft choices must not invoke global/session mutation commands');
    });
  }

  for (const [earlier, latest] of [typedSelections, [...typedSelections].reverse()]) {
    test(`${platform}: late ${earlier.label} read cannot restore ID/ref over ${latest.label}`, async () => {
      const harness = boot(platform);
      const old = harness.holdNextConfig();
      const earlierChoice = harness.models.switchModelRef('session-a', earlier.ref);
      await old.entered;
      await harness.models.switchModelRef('session-a', latest.ref);
      old.resolve({ model: 'obsolete-read' });
      await earlierChoice;
      assert.equal(harness.state.currentSessionModelId, latest.legacyId);
      assert.deepEqual(plain(harness.state.currentSessionModelRef), latest.ref);
      assert.equal(harness.state.effectiveModelConfig.model, latest.wireModel);
    });
  }

  for (const credentialState of ['missing', 'unavailable']) {
    test(`${platform}: current config failure retains authoritative ${credentialState} credentials and returns false`, async () => {
      const harness = boot(platform);
      const authoritative = { model: 'known-model', credential_state: credentialState, requires_user_api_key: true };
      harness.state.effectiveModelConfig = authoritative;
      const failed = harness.holdNextConfig();
      const reading = harness.models.loadEffectiveModelConfig();
      await failed.entered;
      failed.reject(new Error('transient config failure'));
      assert.equal(await reading, false);
      assert.equal(harness.state.effectiveModelConfig, authoritative);
    });
  }

  test(`${platform}: authoritative config clearing wins over a late prior response`, async () => {
    const harness = boot(platform);
    harness.state.effectiveModelConfig = { model: 'before-clear' };
    const old = harness.holdNextConfig();
    const earlier = harness.models.loadEffectiveModelConfig();
    await old.entered;
    const clear = harness.holdNextConfig();
    const clearing = harness.models.loadEffectiveModelConfig();
    await clear.entered;
    clear.resolve(null);
    assert.equal(await clearing, true);
    assert.equal(harness.state.effectiveModelConfig, null);
    old.resolve({ model: 'obsolete-before-clear' });
    assert.equal(await earlier, false);
    assert.equal(harness.state.effectiveModelConfig, null);
  });
}

for (const lane of ['tauriSettings', 'web']) {
  test(`${lane}: shared config loader retains authority and reports transport failure`, async () => {
    const window = {};
    vm.runInNewContext(source('shared/bridge-shared-helpers.js'), { window });
    const authoritative = { model: 'known-model', credential_state: 'missing' };
    const state = { activeSessionId: 'session', effectiveModelConfig: authoritative };
    let notifications = 0;
    const models = window.PinvouBridgeShared.create(lane, {
      state, notify() { notifications += 1; },
      invoke: async () => { throw new Error('transient config failure'); },
    });
    assert.equal(await models.loadEffectiveModelConfig(), false);
    assert.equal(state.effectiveModelConfig, authoritative);
    assert.equal(notifications, 0, 'a failed read must not publish a replacement snapshot');
  });
}
