import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

// Rust's actual Store -> read_visible serializer verifies these same bytes
// (normalizing only its generated UUID). This is a JS consumer/IPC-mock test.
const before = JSON.parse(fs.readFileSync(new URL('./fixtures/remote_quarantine_before.json', import.meta.url)));
const after = JSON.parse(fs.readFileSync(new URL('./fixtures/remote_quarantine_after.json', import.meta.url)));
function install(invoke) {
  const root = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  const sandbox = { window: root, console, TextEncoder, setTimeout, clearTimeout };
  vm.createContext(sandbox);
  for (const source of ['shared/bridge-shared-helpers.js',
    'features/remote-control/remote-control-state.js', 'platform/tauri/bridge/desktop-remote-control.js']) {
    vm.runInContext(fs.readFileSync(new URL(`../src/${source}`, import.meta.url), 'utf8'), sandbox);
  }
  const logic = root.__PINVOU_REMOTE_CONTROL_STATE__;
  const state = { activeSessionId: before.localSessionId,
    desktopRemoteControl: logic.projectRemoteProxyRead(logic.createState(), before) };
  const calls = [];
  const api = root.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control']({
    state, can: () => true, notify() {}, listen: async () => () => {},
    invoke(command, args) { calls.push([command, args]); return invoke(command, args); },
  });
  // The production factory performs one controller-status bootstrap call.
  // Count subsequent user actions separately from that normal initialization.
  calls.length = 0;
  return { state, api, calls, logic };
}
const plain = value => JSON.parse(JSON.stringify(value));

test('actual bridge requires strict per-call confirmation and consumes the real producer quarantine DTO', async () => {
  const { state, api, calls } = install(async command => {
    if (command === 'desktop_remote_proxy_quarantine') return true;
    if (command === 'desktop_remote_proxy_history_read') return after;
    return [];
  });
  for (const confirmation of [undefined, false, 1, 'true']) {
    await assert.rejects(api.quarantineRemoteProxyHistory(before.localSessionId, confirmation),
      error => error.code === 'remote_proxy_quarantine_confirmation_required');
  }
  assert.equal(calls.length, 0);
  assert.equal(await api.quarantineRemoteProxyHistory(before.localSessionId, true), true);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(plain(calls[0]), ['desktop_remote_proxy_quarantine', { localSessionId: before.localSessionId, confirmed: true }]);
  assert.deepEqual(plain(state.desktopRemoteControl.remoteProxyHistory), after);
  await assert.rejects(api.quarantineRemoteProxyHistory(before.localSessionId, false));
  assert.equal(calls.filter(([command]) => command === 'desktop_remote_proxy_quarantine').length, 1);
});

test('a committed seal survives refresh IO failure and invalidates an older pending read', async () => {
  let completeOldRead;
  let firstRead = true;
  const { state, api, calls } = install(command => {
    if (command === 'desktop_remote_proxy_quarantine') return Promise.resolve(false);
    if (command === 'desktop_remote_proxy_history_read' && firstRead) {
      firstRead = false;
      return new Promise(resolve => { completeOldRead = resolve; });
    }
    return Promise.reject(new Error('permission_denied'));
  });
  const olderRead = api.readRemoteProxyHistory(before.localSessionId);
  await Promise.resolve();
  assert.equal(await api.quarantineRemoteProxyHistory(before.localSessionId, true), false);
  completeOldRead(before);
  assert.equal(await olderRead, null, 'an older DTO must be rejected before it can write back to the selected history');
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(plain(state.desktopRemoteControl.remoteProxyHistory), after);
  assert.equal(calls.filter(([command]) => command === 'desktop_remote_proxy_quarantine').length, 1);
});

test('a refused native seal preserves unknown facts and existing execution handles', async () => {
  for (const code of ['remote_proxy_archive_online', 'remote_proxy_archive_unavailable']) {
    const { state, api } = install(async () => { throw Object.assign(new Error(code), { code }); });
    await assert.rejects(api.quarantineRemoteProxyHistory(before.localSessionId, true), error => error.code === code);
    assert.deepEqual(plain(state.desktopRemoteControl.remoteProxyHistory), before);
  }
});

test('quarantine projection rejects a nonarchived or malformed frozen envelope', () => {
  const { logic } = install(async () => []);
  for (const invalid of [{ ...after, archived: false }, { ...after, frozenPreparedCount: 65 },
    { ...after, frozenPreparedCount: -1 }, { ...after, quarantined: 'true' }]) {
    const state = logic.createState();
    assert.equal(logic.projectRemoteProxyRead(state, invalid), state);
  }
});

test('production public facade exports the quarantine operation consumed by the real mounted panel', () => {
  const noopFeature = () => new Proxy({}, { get: () => function () {} });
  const registry = new Proxy({}, { get: (object, key) => Object.hasOwn(object, key) ? object[key] : noopFeature });
  const root = { __TAURI__: { core: { invoke: async () => null }, event: { listen: async () => () => {} }, dialog: {} },
    __PINVOU_TAURI_BRIDGE_FEATURES__: registry, location: { search: '' }, performance: { now: () => 0 }, setTimeout, clearTimeout };
  const sandbox = vm.createContext({ window: root, document: { readyState: 'loading', addEventListener() {} },
    console, setTimeout, clearTimeout, structuredClone, URL, Blob, TextEncoder });
  for (const source of ['shared/bridge-shared-helpers.js', 'features/remote-control/remote-control-state.js',
    'platform/tauri/bridge/desktop-remote-control.js', 'platform/tauri/bridge.js']) {
    vm.runInContext(fs.readFileSync(new URL(`../src/${source}`, import.meta.url), 'utf8'), sandbox, { filename: source });
  }
  const api = root.TauriBridge.desktopRemoteControl;
  assert.equal(typeof api.quarantineRemoteProxyHistory, 'function');
  assert.equal(typeof api.listRemoteProxyArchiveCandidates, 'function');
  assert.equal(typeof api.listRemoteProxyArchives, 'function');
});
