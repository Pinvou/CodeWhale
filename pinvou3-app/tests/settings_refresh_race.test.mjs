import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const source = (relative) => fs.readFileSync(new URL(`../src/${relative}`, import.meta.url), 'utf8');

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

/** Run the real lane and shared loaders; only the command transport is held. */
function boot(platform) {
  const listeners = new Map();
  const heldCommands = new Map();
  const failedCommands = new Set();
  const calls = [];
  let persisted = {
    sidebar: { date_grouping: true }, search: { provider: 'old' },
    pet: { enabled: false, selected_pet: 'kept' },
    computer_use: { enabled: false }, global_shortcut_enabled: false,
  };
  let state;
  const invoke = async (command, args) => {
    calls.push({ command, args });
    if (failedCommands.delete(command)) throw new Error('fixture write failed');
    let value;
    if (command === 'get_settings') value = structuredClone(persisted);
    else if (command === 'update_settings' || command === 'web_access_update_settings') {
      persisted = { ...persisted, ...args.patch };
      value = structuredClone(persisted);
    } else if (command === 'update_search_settings') {
      persisted = { ...persisted, search: args.search };
      value = structuredClone(persisted);
    } else if (command === 'set_global_shortcut_config') {
      value = { status: args.enabled ? 'registered' : 'disabled', accelerator: args.accelerator };
    } else if (command === 'list_models') value = { models: [], active_model_id: 'model' };
    else if (command === 'get_effective_model_config') value = { model: 'model' };
    else throw new Error(`Unexpected fixture command: ${command}`);
    const held = heldCommands.get(command)?.shift();
    if (!held) return value;
    held.entered.resolve(value);
    return held.response.promise;
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
    performance: { now: () => 0 }, addEventListener() {}, setTimeout, clearTimeout,
  };
  const context = vm.createContext({
    window, localStorage, console: { warn() {}, error() {}, log() {} }, setTimeout, clearTimeout,
    setInterval: () => 0, clearInterval() {},
    document: { readyState: 'loading', addEventListener() {} }, navigator: {}, URL, URLSearchParams,
    structuredClone(value) { state = value; return structuredClone(value); },
  });
  vm.runInContext(source('shared/bridge-shared-helpers.js'), context);
  let settings;
  if (platform === 'tauri') {
    state = { settings: structuredClone(persisted), computerUse: { enabled: false } };
    vm.runInContext(source('platform/tauri/bridge/settings.js'), context);
    settings = window.__PINVOU_TAURI_BRIDGE_FEATURES__.settings({
      state, invoke, notify() {}, listen(name, handler) { listeners.set(name, handler); },
    });
  } else {
    vm.runInContext(source('platform/web/bridge.js'), context);
    settings = window.TauriBridge;
    settings.getState();
    state.settings = structuredClone(persisted);
  }
  // A second cluster on the same real state must participate in read ordering.
  const other = window.PinvouBridgeShared.create(platform === 'tauri' ? 'web' : 'tauriSettings', {
    state, invoke, notify() {},
  });
  return {
    state, settings, calls, listeners,
    refresh: () => listeners.get('models:changed')({ payload: {} }),
    read: () => other.loadSettings(),
    writeCommand: platform === 'tauri' ? 'update_settings' : 'web_access_update_settings',
    searchCommand: platform === 'tauri' ? 'update_search_settings' : 'web_access_update_settings',
    failNext(command) { failedCommands.add(command); },
    holdNext(command = 'get_settings') {
      const entered = deferred();
      const response = deferred();
      const held = { entered, response };
      heldCommands.set(command, [...(heldCommands.get(command) || []), held]);
      return { entered: entered.promise, resolve: response.resolve, reject: response.reject };
    },
  };
}

for (const platform of ['tauri', 'web']) {
  for (const failure of [false, true]) {
    test(`${platform}: delayed model refresh settings ${failure ? 'error' : 'response'} preserves a newer saved sidebar preference`, async () => {
      const harness = boot(platform);
      const old = harness.holdNext();
      const refresh = harness.refresh();
      const previous = await old.entered;
      assert.equal(await harness.settings.saveSettings({ sidebar: { date_grouping: false } }), true,
        'a background settings read must not block the write queue');
      assert.equal(harness.state.settings.sidebar.date_grouping, false);
      if (failure) old.reject(new Error('old transport failure'));
      else old.resolve(previous);
      await refresh;
      assert.equal(harness.state.settings.sidebar.date_grouping, false);
    });
  }

  test(`${platform}: a successful search save also invalidates the old model refresh snapshot`, async () => {
    const harness = boot(platform);
    const old = harness.holdNext();
    const refresh = harness.refresh();
    const previous = await old.entered;
    assert.equal(await harness.settings.saveSearchSettings({ provider: 'new' }), true);
    old.resolve(previous);
    await refresh;
    assert.equal(harness.state.settings.search.provider, 'new');
  });

  for (const failure of [false, true]) {
    test(`${platform}: settings reads across clusters keep the newer ${failure ? 'fallback' : 'snapshot'}`, async () => {
      const harness = boot(platform);
      const first = harness.holdNext();
      const refresh = harness.refresh();
      const previous = await first.entered;
      const second = harness.holdNext();
      const read = harness.read();
      await second.entered;
      if (failure) second.reject(new Error('current transport failure'));
      else second.resolve({ sidebar: { date_grouping: false } });
      await read;
      const latest = harness.state.settings;
      first.resolve(previous);
      await refresh;
      assert.equal(harness.state.settings, latest);
      if (failure) assert.equal(latest.color_scheme, 'system', 'current failures keep the existing startup fallback');
      else assert.equal(latest.sidebar.date_grouping, false);
    });
  }

  test(`${platform}: an older settings failure cannot replace a newer successful read`, async () => {
    const harness = boot(platform);
    const old = harness.holdNext();
    const refresh = harness.refresh();
    await old.entered;
    const next = harness.holdNext();
    const read = harness.read();
    await next.entered;
    next.resolve({ sidebar: { date_grouping: false } });
    await read;
    old.reject(new Error('old transport failure'));
    await refresh;
    assert.equal(harness.state.settings.sidebar.date_grouping, false);
  });

  for (const method of ['saveSettings', 'saveSearchSettings']) {
    test(`${platform}: failed ${method} retains failure semantics and does not suppress a pending read`, async () => {
      const harness = boot(platform);
      const old = harness.holdNext();
      const refresh = harness.refresh();
      await old.entered;
      harness.failNext(method === 'saveSettings' ? harness.writeCommand : harness.searchCommand);
      assert.equal(await harness.settings[method]({}), false);
      old.resolve({ sidebar: { date_grouping: false } });
      await refresh;
      assert.equal(harness.state.settings.sidebar.date_grouping, false);
    });
  }

  test(`${platform}: settings writes remain serialized while background reads run independently`, async () => {
    const harness = boot(platform);
    const held = harness.holdNext(harness.writeCommand);
    const first = harness.settings.saveSettings({ sidebar: { date_grouping: false } });
    const response = await held.entered;
    const second = harness.settings.saveSettings({ sidebar: { date_grouping: true } });
    await harness.read();
    assert.equal(harness.calls.filter(({ command }) => command === harness.writeCommand).length, 1);
    held.resolve(response);
    assert.equal(await first, true);
    assert.equal(await second, true);
    assert.equal(harness.state.settings.sidebar.date_grouping, true);
  });
}

for (const failure of [false, true]) {
  test(`tauri: pet events protect the new partial setting from a delayed ${failure ? 'error' : 'response'}`, async () => {
    const harness = boot('tauri');
    const old = harness.holdNext();
    const refresh = harness.refresh();
    const previous = await old.entered;
    const original = harness.state.settings;
    harness.listeners.get('pet:enabled_changed')({ payload: { enabled: true } });
    if (failure) old.reject(new Error('old transport failure'));
    else old.resolve(previous);
    await refresh;
    assert.equal(harness.state.settings.pet.enabled, true);
    assert.equal(harness.state.settings.pet.selected_pet, 'kept');
    assert.equal(original.pet.enabled, false, 'event publication must not mutate an older snapshot');
  });
}

test('tauri: dedicated shortcut saves protect their partial settings mirror', async () => {
  const harness = boot('tauri');
  const old = harness.holdNext();
  const refresh = harness.refresh();
  const previous = await old.entered;
  const result = await harness.settings.setGlobalShortcutConfig(true, 'CommandOrControl+Shift+P');
  assert.equal(result.status, 'registered');
  old.resolve(previous);
  await refresh;
  assert.equal(harness.state.settings.global_shortcut_enabled, true);
  assert.equal(harness.state.settings.global_shortcut_accelerator, 'CommandOrControl+Shift+P');
  assert.equal(harness.state.settings.sidebar.date_grouping, true);
});
