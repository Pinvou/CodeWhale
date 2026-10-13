import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const CHANNELS = [
  'desktop-remote-controlled-status', 'desktop-remote-controller-status',
  'desktop-remote-task-status', 'desktop-remote-task-event',
  'desktop-remote-controlled-session', 'desktop-remote-controlled-journal',
  'desktop-remote-task-content', 'desktop-remote-proxy-capability', 'desktop_remote_proxy',
];
const source = path => readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8');
const flush = () => new Promise(resolve => setImmediate(resolve));

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function fixture(register) {
  const root = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  const sandbox = { window: root, TextEncoder, setTimeout, clearTimeout };
  vm.createContext(sandbox);
  vm.runInContext(source('features/remote-control/remote-control-state.js'), sandbox);
  vm.runInContext(source('platform/tauri/bridge/desktop-remote-control.js'), sandbox);
  const calls = [];
  const released = [];
  const state = { desktopRemoteControl: root.__PINVOU_REMOTE_CONTROL_STATE__.createState() };
  const feature = root.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control']({
    state, notify() {}, can: () => true,
    invoke: () => Promise.resolve({ status: 'connected', claimRequestId: 'claim-retry',
      lease: { connectionId: 'connection-retry', controllerLeaseId: 'lease-retry',
        leaseEpoch: '1', relayEpoch: 'relay-retry', remoteControlGeneration: 'generation-retry' },
    }),
    listen(name, callback) {
      const attempt = calls.filter(call => call.name === name).length + 1;
      const call = { name, attempt, callback };
      calls.push(call);
      const off = () => released.push(call);
      return register(call, off);
    },
  });
  return { feature, calls, released, state };
}

test('subscription retries only rejected channels and keeps successful callbacks active', async () => {
  const failed = new Set([CHANNELS[0], CHANNELS[8]]);
  const f = fixture((call, off) => failed.has(call.name) && call.attempt === 1
    ? Promise.reject(new Error('fake-registration-rejected')) : Promise.resolve(off));
  try {
    await flush();
    f.feature.subscribe();
    f.feature.subscribe();
    await flush();
    for (const name of CHANNELS) {
      assert.equal(f.calls.filter(call => call.name === name).length, failed.has(name) ? 2 : 1);
    }
    f.calls.find(call => call.name === CHANNELS[1]).callback({ payload: {
      status: 'reconnecting', claimRequestId: 'claim-retry',
      lease: { connectionId: 'connection-retry', controllerLeaseId: 'lease-retry',
        leaseEpoch: '1', relayEpoch: 'relay-retry', remoteControlGeneration: 'generation-retry' },
    } });
    assert.equal(f.state.desktopRemoteControl.controllerConnection.status, 'reconnecting');
    assert.equal(f.state.desktopRemoteControl.controllerConnection.generation, 'generation-retry');
  } finally {
    f.feature.stop();
  }
});

test('all rejected subscription channels can be explicitly retried once', async () => {
  const f = fixture((call, off) => call.attempt === 1
    ? Promise.reject(new Error('fake-registration-rejected')) : Promise.resolve(off));
  try {
    await flush();
    f.feature.subscribe();
    await flush();
    f.feature.subscribe();
    assert.equal(f.calls.length, CHANNELS.length * 2);
    for (const name of CHANNELS) assert.equal(f.calls.filter(call => call.name === name).length, 2);
  } finally {
    f.feature.stop();
  }
});

test('a synchronous registration throw does not block other channels or the retry', async () => {
  const f = fixture((call, off) => {
    if (call.name === CHANNELS[1] && call.attempt === 1) throw new Error('fake-registration-throw');
    return Promise.resolve(off);
  });
  try {
    await flush();
    assert.equal(f.calls.length, CHANNELS.length);
    f.feature.subscribe();
    await flush();
    assert.equal(f.calls.length, CHANNELS.length + 1);
    for (const name of CHANNELS) assert.equal(f.calls.filter(call => call.name === name).length,
      name === CHANNELS[1] ? 2 : 1);
  } finally {
    f.feature.stop();
  }
});

test('pending channel registration is single and its old rejection cannot erase a replacement', async () => {
  const pending = [];
  const f = fixture((call, off) => {
    if (call.name !== CHANNELS[1]) return Promise.resolve(off);
    const attempt = deferred();
    pending.push({ ...attempt, off });
    return attempt.promise;
  });
  try {
    await flush();
    f.feature.subscribe();
    f.feature.subscribe();
    assert.equal(pending.length, 1);
    f.feature.stop();
    assert.equal(f.released.length, CHANNELS.length - 1);
    f.feature.subscribe();
    assert.equal(pending.length, 2);
    pending[0].reject(new Error('fake-old-registration-rejected'));
    await flush();
    f.feature.subscribe();
    assert.equal(pending.length, 2, 'old rejection cannot make the new pending channel retry');
    pending[1].resolve(pending[1].off);
    await flush();
    f.feature.subscribe();
    assert.equal(pending.length, 2);
  } finally {
    f.feature.stop();
  }
});
