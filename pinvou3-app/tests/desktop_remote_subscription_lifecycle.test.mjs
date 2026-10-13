import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

function install(invoke = () => Promise.resolve({ status: 'idle' }), timers = { setTimeout, clearTimeout }, onListen = () => {}) {
  const root = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  const sandbox = { window: root, TextEncoder, ...timers };
  vm.createContext(sandbox);
  for (const source of ['features/remote-control/remote-control-state.js',
    'platform/tauri/bridge/desktop-remote-control.js']) {
    vm.runInContext(fs.readFileSync(new URL('../src/' + source, import.meta.url), 'utf8'), sandbox);
  }
  const registrations = [];
  let notifications = 0;
  const state = { desktopRemoteControl: root.__PINVOU_REMOTE_CONTROL_STATE__.createState() };
  const feature = root.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control']({
    state,
    notify() { notifications++; },
    can: () => true,
    invoke,
    listen(name, callback) {
      const pending = Promise.withResolvers();
      const registration = { name, callback, pending, releases: 0 };
      registration.off = () => { registration.releases++; };
      registrations.push(registration);
      onListen(registration);
      return pending.promise;
    },
  });
  return { feature, state, registrations, notifications: () => notifications,
    logic: root.__PINVOU_REMOTE_CONTROL_STATE__ };
}

async function complete(registrations) {
  for (const entry of registrations) entry.pending.resolve(entry.off);
  await Promise.resolve();
  await Promise.resolve();
}

test('synchronous stop during listen retires the registration round without stranding later channels', async () => {
  let stopNext = false;
  const f = install(undefined, undefined, () => {
    if (stopNext) {
      stopNext = false;
      f.feature.stop();
    }
  });
  try {
    await complete(f.registrations.slice());
    f.feature.stop();
    stopNext = true;
    f.feature.subscribe();
    const retired = f.registrations.slice(9);
    assert.equal(retired.length, 1, 'a retired round must not register its remaining eight channels');
    await complete(retired);
    assert.equal(retired[0].releases, 1, 'its late cleanup belongs only to that retired round');
    f.feature.subscribe();
    f.feature.subscribe();
    const current = f.registrations.slice(10);
    assert.equal(current.length, 9, 'the next owner must register every channel exactly once');
    await complete(current);
    const before = f.notifications();
    retired[0].callback({ payload: { status: 'idle' } });
    assert.equal(f.notifications(), before, 'retired callbacks cannot publish');
    current[0].callback({ payload: { status: 'idle' } });
    assert.equal(f.notifications(), before + 1, 'the replacement callback remains live');
    f.feature.stop();
    assert.deepEqual(current.map(entry => entry.releases), Array(9).fill(1));
    assert.equal(retired[0].releases, 1);
  } finally {
    f.feature.stop();
    await complete(f.registrations);
  }
});

test('synchronous listen retirement and replacement cannot let the old round register into the new owner', async () => {
  let replaceNext = false;
  const f = install(undefined, undefined, () => {
    if (replaceNext) {
      replaceNext = false;
      f.feature.stop();
      f.feature.subscribe();
    }
  });
  try {
    await complete(f.registrations.slice());
    f.feature.stop();
    replaceNext = true;
    f.feature.subscribe();
    const retired = f.registrations[9];
    const current = f.registrations.slice(10);
    assert.equal(current.length, 9);
    f.feature.subscribe();
    assert.equal(f.registrations.length, 19, 'old continuation and explicit retry add no duplicate registration');
    await complete(current);
    await complete([retired]);
    assert.equal(retired.releases, 1);
    assert.deepEqual(current.map(entry => entry.releases), Array(9).fill(0));
    const before = f.notifications();
    retired.callback({ payload: { status: 'idle' } });
    assert.equal(f.notifications(), before);
    current[0].callback({ payload: { status: 'idle' } });
    assert.equal(f.notifications(), before + 1);
    f.feature.stop();
    assert.deepEqual(current.map(entry => entry.releases), Array(9).fill(1));
    assert.equal(retired.releases, 1);
  } finally {
    f.feature.stop();
    await complete(f.registrations);
  }
});

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

test('stop retires waiting timers before offs and preserves replacement capability requests and bounded retries', async () => {
  const scheduled = new Map();
  let nextTimer = 0;
  const timers = {
    setTimeout(callback, delay) {
      const id = ++nextTimer;
      scheduled.set(id, { callback, delay });
      return id;
    },
    clearTimeout(id) { scheduled.delete(id); },
  };
  const requests = [];
  const { feature, state, logic, registrations, notifications } = install((command, args) => {
    if (command === 'desktop_remote_controller_status') return Promise.resolve({ status: 'idle' });
    if (command === 'desktop_remote_client_event') return Promise.resolve();
    assert.equal(command, 'desktop_remote_controller_capability_send');
    const requestId = `request-${requests.length + 1}`;
    requests.push({ requestId, remoteRef: args.remoteRef });
    return Promise.resolve({ requestId, sessionId: args.remoteRef === 'ref-old' ? 'session-old' : 'session-new' });
  }, timers);
  const flush = () => new Promise(setImmediate);
  function bind(suffix) {
    state.desktopRemoteControl = logic.beginControllerIntent(state.desktopRemoteControl, true);
    state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
      intent: state.desktopRemoteControl.intents.controller, status: 'connected',
      claimRequestId: `claim-${suffix}`, replaceBinding: true,
      connectionId: `connection-${suffix}`, controllerLeaseId: `lease-${suffix}`, leaseEpoch: '1',
      relayEpoch: `relay-${suffix}`, generation: `generation-${suffix}`,
    });
    state.desktopRemoteControl = logic.applyRemoteProxyBinding(state.desktopRemoteControl, {
      localSessionId: `remote-${suffix}`, remoteSessionRef: `ref-${suffix}`, sessionId: `session-${suffix}`,
      mode: 'chat', connectionId: `connection-${suffix}`, controllerLeaseId: `lease-${suffix}`,
      leaseEpoch: '1', relayEpoch: `relay-${suffix}`, remoteControlGeneration: `generation-${suffix}`,
      mappingConfirmed: false,
    });
    assert.equal(state.desktopRemoteControl.controllerConnection.sessionRef, `ref-${suffix}`);
  }
  function reply(suffix, requestId) {
    return { payload: {
      localSessionId: `remote-${suffix}`, remoteSessionRef: `ref-${suffix}`, sessionId: `session-${suffix}`,
      connectionId: `connection-${suffix}`, controllerLeaseId: `lease-${suffix}`, leaseEpoch: '1',
      relayEpoch: `relay-${suffix}`, remoteControlGeneration: `generation-${suffix}`,
      capabilityRequestId: requestId, capabilityRevision: 1, capabilityValidForMs: 30000,
      capabilityError: null, mappingConfirmed: true, temporaryModelAvailable: true,
    } };
  }
  function fire(delay) {
    const entry = [...scheduled].find(([, timer]) => timer.delay === delay);
    assert.ok(entry, `a real bridge timer with delay ${delay} must exist`);
    scheduled.delete(entry[0]);
    entry[1].callback();
  }
  let replacement;
  let completed = false;
  let cleanupFailure;
  try {
    await flush();
    bind('old');
    const oldRequest = feature.controllerCapabilitySend('ref-old');
    const oldOutcome = oldRequest.then(() => ({ ok: true }), error => ({ ok: false, code: error.code }));
    await flush();
    const oldTimeout = [...scheduled].find(([, timer]) => timer.delay === 8000);
    assert.ok(oldTimeout, 'the legitimate old request reached its response waiter');
    const retired = registrations.slice();
    retired[0].off = () => {
      retired[0].releases++;
      try {
        assert.equal(scheduled.has(oldTimeout[0]), false, 'old waiter timer is cleared before external off');
        feature.subscribe();
        bind('new');
        replacement = feature.controllerCapabilitySend('ref-new');
      } catch (error) { cleanupFailure = error; }
    };
    await complete(retired);
    await flush();
    feature.stop();
    if (cleanupFailure) throw cleanupFailure;
    assert.equal(retired[0].releases, 1);
    assert.ok(replacement, 'actual retained off created a new-owner capability request');
    await complete(registrations.slice(9));
    await flush();
    assert.deepEqual(requests.map(request => request.remoteRef), ['ref-old', 'ref-new']);
    assert.ok([...scheduled.values()].some(timer => timer.delay === 500),
      'retired waiter rejected promptly and entered the existing bounded retry');
    const before = notifications();
    const oldEvent = retired.find(entry => entry.name === 'desktop-remote-proxy-capability');
    const newEvent = registrations.slice(9).find(entry => entry.name === 'desktop-remote-proxy-capability');
    oldEvent.callback(reply('old', 'request-1'));
    newEvent.callback(reply('old', 'request-1'));
    assert.equal(notifications(), before, 'old listener and wrong binding cannot settle the replacement');
    assert.equal(state.desktopRemoteControl.controllerConnection.mappingConfirmed, false);
    newEvent.callback(reply('new', 'request-2'));
    assert.equal((await replacement).capabilityRequestId, 'request-2');
    assert.equal(state.desktopRemoteControl.controllerConnection.mappingConfirmed, true);
    // stop retires renderer waiters, not the legitimate old capability flight.
    // Drive the existing 500/1000ms retry and 8s response timers without sleeps.
    fire(500);
    await flush();
    fire(8000);
    await flush();
    fire(1000);
    await flush();
    fire(8000);
    await flush();
    assert.deepEqual(await oldOutcome, { ok: false, code: 'capability_snapshot_timeout' });
    assert.deepEqual(requests.map(request => request.remoteRef), ['ref-old', 'ref-new', 'ref-old', 'ref-old']);
    assert.equal(state.desktopRemoteControl.controllerConnection.sessionRef, 'ref-new');
    assert.equal(state.desktopRemoteControl.controllerConnection.mappingConfirmed, true);
    assert.equal([...scheduled.values()].filter(timer => timer.delay === 8000).length, 0);
    assert.deepEqual(retired.map(entry => entry.releases), Array(9).fill(1));
    completed = true;
  } finally {
    feature.stop();
    await flush();
    if (completed) assert.equal(scheduled.size, 0, 'all response and renewal timers are retired');
    scheduled.clear(); // Discard only test-owned fake timers on a failing case.
  }
});
