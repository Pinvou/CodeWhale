import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

function install() {
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
    invoke: () => Promise.resolve({ status: 'idle' }),
    listen(name, callback) {
      const pending = Promise.withResolvers();
      const registration = { name, callback, pending, releases: 0 };
      registration.off = () => { registration.releases++; };
      registrations.push(registration);
      return pending.promise;
    },
  });
  return { feature, registrations, notifications: () => notifications };
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
