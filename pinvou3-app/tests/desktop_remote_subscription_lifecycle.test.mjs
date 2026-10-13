import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

function install(invoke = () => Promise.resolve({ status: 'idle' })) {
  const root = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  const sandbox = { window: root, TextEncoder, setTimeout, clearTimeout };
  vm.createContext(sandbox);
  for (const source of ['features/remote-control/remote-control-state.js',
    'platform/tauri/bridge/desktop-remote-control.js']) {
    vm.runInContext(fs.readFileSync(new URL('../src/' + source, import.meta.url), 'utf8'), sandbox);
  }
  const registrations = [];
  let notifications = 0;
  const feature = root.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control']({
    state: { desktopRemoteControl: root.__PINVOU_REMOTE_CONTROL_STATE__.createState() },
    notify() { notifications++; },
    can: () => true,
    invoke,
    listen(name, callback) {
      const pending = Promise.withResolvers();
      const registration = { name, callback, pending, releases: 0 };
      registration.off = () => { registration.releases++; };
      registrations.push(registration);
      return pending.promise;
    },
  });
  return { feature, registrations, notifications: () => notifications,
    logic: root.__PINVOU_REMOTE_CONTROL_STATE__ };
}

async function complete(registrations) {
  for (const entry of registrations) entry.pending.resolve(entry.off);
  await Promise.resolve();
  await Promise.resolve();
}

test('stop releases listeners that finish registration late exactly once', async () => {
  const { feature, registrations } = install();
  feature.subscribe();
  assert.equal(registrations.length, 9);
  feature.stop();
  feature.stop();
  await complete(registrations);
  assert.deepEqual(registrations.map(entry => entry.releases), Array(9).fill(1));
  feature.stop();
  assert.deepEqual(registrations.map(entry => entry.releases), Array(9).fill(1));
});

test('old late registrations cannot attach to or publish into the replacement lifecycle', async () => {
  const { feature, registrations, notifications } = install();
  feature.subscribe();
  const old = registrations.slice();
  await complete(old.slice(0, 2));
  feature.stop();
  feature.subscribe();
  const current = registrations.slice(9);
  feature.subscribe();
  assert.equal(registrations.length, 18);
  const before = notifications();
  old[0].callback({ payload: { status: 'idle' } });
  old[1].callback({ payload: { status: 'idle' } });
  assert.equal(notifications(), before);
  await complete(old.slice(2));
  await complete(current);
  assert.deepEqual(old.map(entry => entry.releases), Array(9).fill(1));
  assert.deepEqual(current.map(entry => entry.releases), Array(9).fill(0));
  current[0].callback({ payload: { status: 'idle' } });
  assert.equal(notifications(), before + 1);
  feature.stop();
  assert.deepEqual(current.map(entry => entry.releases), Array(9).fill(1));
});

test('partial registration rejection does not retain successful late listeners after stop', async () => {
  const { feature, registrations } = install();
  feature.subscribe();
  registrations[0].pending.reject(new Error('registration unavailable'));
  registrations[1].off = () => { registrations[1].releases++; throw new Error('already removed'); };
  await complete(registrations.slice(1, 4));
  feature.stop();
  await complete(registrations.slice(4));
  assert.equal(registrations[0].releases, 0);
  assert.deepEqual(registrations.slice(1).map(entry => entry.releases), Array(8).fill(1));
  feature.stop();
  assert.deepEqual(registrations.slice(1).map(entry => entry.releases), Array(8).fill(1));
});

test('a cleanup callback can create a replacement without inheriting the retired channel attempts', async () => {
  const { feature, registrations, notifications } = install();
  registrations[0].off = () => {
    registrations[0].releases++;
    feature.subscribe();
  };
  try {
    const old = registrations.slice();
    await complete(old);
    feature.stop();
    assert.equal(registrations.length, 18, 'replacement registers all nine retired channels');
    const replacement = registrations.slice(9);
    await complete(replacement);
    const before = notifications();
    old[0].callback({ payload: { status: 'idle' } });
    assert.equal(notifications(), before);
    replacement[0].callback({ payload: { status: 'idle' } });
    assert.equal(notifications(), before + 1);
    assert.deepEqual(old.map(entry => entry.releases), Array(9).fill(1));
    feature.stop();
    assert.deepEqual(replacement.map(entry => entry.releases), Array(9).fill(1));
    assert.deepEqual(old.map(entry => entry.releases), Array(9).fill(1));
  } finally {
    feature.stop();
  }
});

test('old stop does not invalidate a new-owner proxy request started by a cleanup callback', async () => {
  const pending = Promise.withResolvers();
  const { feature, registrations, logic } = install(command => {
    if (command === 'desktop_remote_proxy_history_list') return pending.promise;
    assert.equal(command, 'desktop_remote_controller_status');
    return Promise.resolve({ status: 'idle' });
  });
  let projections = 0;
  const project = logic.projectRemoteProxyList;
  logic.projectRemoteProxyList = (...args) => { projections++; return project(...args); };
  let request;
  registrations[0].off = () => {
    registrations[0].releases++;
    feature.subscribe();
    request = feature.listRemoteProxyHistory();
  };
  try {
    await complete(registrations.slice());
    await new Promise(setImmediate);
    feature.stop();
    assert.equal(registrations[0].releases, 1, 'actual retained cleanup ran');
    assert.ok(request, 'replacement issued its real bridge request');
    pending.resolve([]);
    await request;
    assert.equal(projections, 1, 'retired stop cannot invalidate the new owner list intent');
    await complete(registrations.slice(9));
  } finally {
    feature.stop();
  }
});
