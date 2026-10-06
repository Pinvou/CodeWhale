// DEV-20 contract: the desktop remote-control bridge state must reject late
// results. Each rule here maps to a documented safety requirement, so a
// regression in the pure state module fails the lane instead of silently
// resurrecting a stale code/connection/capability/task.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const logicPath = path.join(root, 'src', 'features', 'remote-control', 'remote-control-state.js');
const context = { window: {} };
context.globalThis = context;
vm.createContext(context);
vm.runInContext(fs.readFileSync(logicPath, 'utf8'), context, { filename: logicPath });
const logic = context.window.__PINVOU_REMOTE_CONTROL_STATE__;
const fixtures = JSON.parse(fs.readFileSync(path.join(root, 'tests/fixtures/desktop_remote_status.json'), 'utf8'));

function connected(connectionId = 'connection-1', leaseEpoch = '2') {
  return {
    ...fixtures.controllerConnected,
    lease: { ...fixtures.controllerConnected.lease, connectionId, leaseEpoch },
  };
}

function makeBridge(responses = {}) {
  const callbacks = new Map();
  const host = { window: {} };
  vm.createContext(host);
  vm.runInContext(fs.readFileSync(logicPath, 'utf8'), host);
  vm.runInContext(fs.readFileSync(path.join(root, 'src/platform/tauri/bridge/desktop-remote-control.js'), 'utf8'), host);
  const state = {};
  const bridge = host.window.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control']({
    state,
    can: () => true,
    notify() {},
    listen(name, callback) {
      callbacks.set(name, callback);
      return Promise.resolve(() => callbacks.delete(name));
    },
    invoke(name) {
      return Promise.resolve(typeof responses[name] === 'function' ? responses[name]() : responses[name]);
    },
  });
  return {
    bridge,
    get state() { return state.desktopRemoteControl; },
    emit(name, payload) {
      assert.ok(callbacks.has(name), `missing subscription: ${name}`);
      callbacks.get(name)({ payload });
    },
  };
}

test('shared Rust fixtures project device names, errors, lease and pairing expiry', () => {
  let state = logic.applyControllerStatus(logic.createState(), fixtures.controllerConnected);
  assert.equal(state.controllerConnection.connectionId, 'connection-0001');
  assert.equal(state.controllerConnection.controllerLeaseId, 'lease-0001');
  assert.equal(state.controllerConnection.remoteControlGeneration, 'generation-0001');
  assert.equal(state.controllerConnection.deviceName, 'Studio PC');
  state = logic.applyControlledStatus(state, fixtures.controlledWaiting);
  assert.equal(state.controlledDevice.expiresAtMs, 600000);
  assert.equal(state.controlledDevice.workspaceCount, 1);
  assert.equal(state.controlledDevice.codeModeAllowed, true);
  state = logic.applyControlledStatus(state, fixtures.controlledError);
  assert.equal(state.controlledDevice.lastError, 'endpoint_offline');
  assert.equal(state.controlledDevice.pairingCode, null);
  state = logic.applyControllerStatus(state, fixtures.controllerError);
  assert.equal(state.controllerConnection.lastError, 'endpoint_offline');
  assert.equal(state.controllerConnection.connectionId, null);
  assert.equal(logic.matchesLiveContext(state, {}), false);
});

test('real bridge subscribes during A connect and applies handshake, connected and error events', async () => {
  const connecting = { ...fixtures.controllerConnected, status: 'connecting', lease: null, deviceDisplayName: null, hasPendingCode: true };
  const host = makeBridge({ desktop_remote_controller_connect: connecting });
  await host.bridge.controllerConnect('2345678');
  host.emit('desktop-remote-controller-status', { ...connecting, status: 'safety_handshake', hasPendingCode: false });
  assert.equal(host.state.controllerConnection.status, 'safety_handshake');
  host.emit('desktop-remote-controller-status', connecting);
  assert.equal(host.state.controllerConnection.status, 'safety_handshake');
  host.emit('desktop-remote-controller-status', fixtures.controllerConnected);
  assert.equal(host.state.controllerConnection.connectionId, 'connection-0001');
  host.emit('desktop-remote-controller-status', connecting);
  assert.equal(host.state.controllerConnection.status, 'connected', 'late results must not regress handshake');
  host.emit('desktop-remote-controller-status', fixtures.controllerError);
  assert.equal(host.state.controllerConnection.lastError, 'endpoint_offline');
  assert.equal(host.state.controllerConnection.connectionId, null);
  host.emit('desktop-remote-controller-status', fixtures.controllerConnected);
  assert.equal(host.state.controllerConnection.status, 'error', 'old connected event cannot revive a terminal claim');
  host.bridge.stop();
});

test('command responses and B events share the Rust projection and reject replaced generations', async () => {
  const host = makeBridge({ desktop_remote_controlled_start: fixtures.controlledWaiting });
  await host.bridge.controlledStart('Studio PC', []);
  assert.equal(host.state.controlledDevice.expiresAtMs, 600000);
  host.emit('desktop-remote-controlled-status', { ...fixtures.controlledWaiting, status: 'reserving' });
  assert.equal(host.state.controlledDevice.pairingCode, '2345678');
  host.emit('desktop-remote-controlled-status', fixtures.controlledError);
  assert.equal(host.state.controlledDevice.lastError, 'endpoint_offline');
  host.emit('desktop-remote-controlled-status', fixtures.controlledWaiting);
  assert.equal(host.state.controlledDevice.status, 'error', 'old waiting event cannot revive a terminal generation');
  host.emit('desktop-remote-controlled-status', { ...fixtures.controlledWaiting, generation: 'generation-old' });
  assert.equal(host.state.controlledDevice.status, 'error');
  host.bridge.stop();
});

test('new explicit A connect replaces the binding but queued old events do not', async () => {
  let result = fixtures.controllerConnected;
  const host = makeBridge({ desktop_remote_controller_connect: () => result });
  await host.bridge.controllerConnect('2345678');
  host.emit('desktop-remote-controller-status', fixtures.controllerError);
  result = { ...fixtures.controllerConnected, claimRequestId: 'claim-0002', lease: { ...fixtures.controllerConnected.lease, connectionId: 'connection-0002' } };
  await host.bridge.controllerConnect('3456789');
  assert.equal(host.state.controllerConnection.connectionId, 'connection-0002');
  host.emit('desktop-remote-controller-status', fixtures.controllerError);
  host.emit('desktop-remote-controller-status', fixtures.controllerConnected);
  assert.equal(host.state.controllerConnection.connectionId, 'connection-0002');
  host.bridge.stop();
});

test('status polling cannot overwrite a newer event with an obsolete binding', async () => {
  let resolvePoll;
  const host = makeBridge({
    desktop_remote_controller_connect: fixtures.controllerConnected,
    desktop_remote_controller_status: () => new Promise((resolve) => { resolvePoll = resolve; }),
  });
  await host.bridge.controllerConnect('2345678');
  const pending = host.bridge.controllerStatus();
  host.emit('desktop-remote-controller-status', fixtures.controllerError);
  resolvePoll(fixtures.controllerConnected);
  await pending;
  assert.equal(host.state.controllerConnection.status, 'error');
  assert.equal(host.state.controllerConnection.connectionId, null);
  host.bridge.stop();
});

test('B old terminal and reserving events cannot replace a refreshed generation', () => {
  let state = logic.applyControlledStatus(logic.createState(), fixtures.controlledWaiting);
  state = logic.beginControlledIntent(state);
  state = logic.applyControlledStatus(state, { ...fixtures.controlledWaiting, generation: 'generation-0002', statusIntent: '2', intent: state.intents.controlled, replaceBinding: true });
  assert.equal(logic.applyControlledStatus(state, fixtures.controlledError), state);
  assert.equal(logic.applyControlledStatus(state, { ...fixtures.controlledError, generation: null }), state);
  state = logic.applyControlledStatus(state, { ...fixtures.controlledWaiting, generation: 'generation-0002', statusIntent: '2', status: 'refreshing' });
  assert.equal(logic.applyControlledStatus(state, { ...fixtures.controlledWaiting, status: 'reserving' }), state);
  state = logic.applyControlledStatus(state, { ...fixtures.controlledWaiting, status: 'reserving', generation: 'generation-0003', statusIntent: '3' });
  assert.equal(state.controlledDevice.generation, 'generation-0003');
});

test('same-epoch full lease tuple and capabilities cannot be substituted', () => {
  const state = logic.applyControllerStatus(logic.createState(), fixtures.controllerConnected);
  for (const key of ['controllerLeaseId', 'relayEpoch', 'remoteControlGeneration']) {
    assert.equal(logic.applyControllerStatus(state, { ...fixtures.controllerConnected, lease: { ...fixtures.controllerConnected.lease, [key]: 'foreign' } }), state);
    assert.equal(logic.applyCapabilities(state, { ...fixtures.controllerConnected.lease, capabilityRevision: '1', [key]: 'foreign' }), state);
  }
});

test('reconnect preserves existing tasks and binding but refuses task events while offline', () => {
  let state = logic.applyControllerStatus(logic.createState(), fixtures.controllerConnected);
  state = logic.applyCapabilities(state, { ...fixtures.controllerConnected.lease, capabilityRevision: '1' });
  state = logic.applyTaskEvent(state, { taskId: 'existing', status: 'running' });
  const capabilities = state.remoteCapabilities;
  state = logic.applyControllerStatus(state, { ...fixtures.controllerConnected, status: 'reconnecting', lease: null });
  assert.equal(state.controllerConnection.connectionId, 'connection-0001');
  assert.equal(state.remoteCapabilities, capabilities);
  assert.equal(logic.matchesLiveContext(state, {}), false);
  assert.equal(logic.applyTaskEvent(state, { taskId: 'late', status: 'running' }), state);
  state = logic.applyControllerStatus(state, fixtures.controllerConnected);
  assert.equal(state.tasks.length, 1);
  assert.equal(state.tasks[0].taskId, 'existing');
});

test('late controlled-device results never revive an older intent', () => {
  let state = logic.createState();
  state = logic.beginControlledIntent(state);
  const firstIntent = state.intents.controlled;
  state = logic.beginControlledIntent(state);
  const secondIntent = state.intents.controlled;

  const stale = logic.applyControlledStatus(state, {
    intent: firstIntent,
    status: 'waiting_connection',
    pairingCode: '2345678',
    generation: 'gen-1',
  });
  assert.equal(stale, state, 'stale intent must be ignored');

  const fresh = logic.applyControlledStatus(state, {
    intent: secondIntent,
    status: 'waiting_connection',
    pairingCode: '3456789',
    generation: 'gen-2',
  });
  assert.equal(fresh.controlledDevice.status, 'waiting_connection');
  assert.equal(fresh.controlledDevice.pairingCode, '3456789');

  const oldGeneration = logic.applyControlledStatus(fresh, {
    status: 'connected',
    generation: 'gen-1',
  });
  assert.equal(oldGeneration, fresh, 'a replaced generation must not overwrite state');
});

test('a stopped controlled device clears the pairing code', () => {
  let state = logic.createState();
  state = logic.applyControlledStatus(state, {
    status: 'waiting_connection',
    pairingCode: '3456789',
    generation: 'gen-1',
  });
  state = logic.applyControlledStatus(state, { status: 'stopped', generation: 'gen-1' });
  assert.equal(state.controlledDevice.status, 'stopped');
  assert.equal(state.controlledDevice.pairingCode, null);
});

test('controller status is gated by connection and lease epoch', () => {
  let state = logic.createState();
  state = logic.applyControllerStatus(state, connected());
  assert.equal(state.controllerConnection.connectionId, 'connection-1');

  const otherConnection = logic.applyControllerStatus(state, connected('connection-2', '1'));
  assert.equal(otherConnection, state, 'another connection must not take over the shell');

  const olderLease = logic.applyControllerStatus(state, connected('connection-1', '1'));
  assert.equal(olderLease, state, 'an older lease epoch must not overwrite the live one');

  const disconnected = logic.applyControllerStatus(state, { status: 'idle' });
  assert.equal(disconnected.controllerConnection.status, 'idle');
  assert.equal(disconnected.remoteCapabilities, null);
});

test('capability snapshots are bound to connection, lease and revision', () => {
  let state = logic.createState();
  state = logic.applyControllerStatus(state, connected());
  const snapshot = {
    ...fixtures.controllerConnected.lease,
    connectionId: 'connection-1',
    leaseEpoch: '2',
    capabilityRevision: 4,
    modes: ['chat', 'code'],
    tools: [{ toolId: 'browser', displayName: 'Browser' }],
    skills: [{ skillId: 'visual-design', displayName: 'Visual' }],
    workspaces: [{ workspaceId: 'workspace-1', displayName: 'Project', codeAllowed: true }],
  };
  const applied = logic.applyCapabilities(state, snapshot);
  assert.equal(applied.remoteCapabilities.capabilityRevision, 4);

  const foreignConnection = logic.applyCapabilities(applied, {
    ...snapshot,
    connectionId: 'connection-9',
    capabilityRevision: 9,
  });
  assert.equal(foreignConnection, applied, 'foreign connection snapshots must be dropped');

  const olderRevision = logic.applyCapabilities(applied, { ...snapshot, capabilityRevision: 3 });
  assert.equal(olderRevision, applied, 'older revisions must not replace the snapshot');

  const newerRevision = logic.applyCapabilities(applied, { ...snapshot, capabilityRevision: 5 });
  assert.equal(newerRevision.remoteCapabilities.capabilityRevision, 5);
});

test('model descriptors stay opaque and never carry credentials', () => {
  const models = logic.applyModels(logic.createState(), [
    {
      modelId: 'model-1',
      displayName: 'DeepSeek',
      exportable: true,
      reason: null,
      visionModelId: null,
      apiKey: 'sk-leak',
      credentialRef: 'vault://x',
      baseUrl: 'https://api.deepseek.com/v1',
    },
  ]);
  const encoded = JSON.stringify(models.remoteModels);
  assert.ok(!encoded.includes('sk-leak'));
  assert.ok(!encoded.includes('vault://'));
  assert.ok(!encoded.includes('api.deepseek.com'));
  const descriptor = models.remoteModels[0];
  assert.equal(descriptor.modelId, 'model-1');
  assert.equal(descriptor.displayName, 'DeepSeek');
  assert.equal(descriptor.exportable, true);
  assert.equal(descriptor.reason, null);
  assert.equal(descriptor.visionModelId, null);
  assert.deepEqual(Object.keys(descriptor).sort(), [
    'displayName',
    'exportable',
    'modelId',
    'reason',
    'visionModelId',
  ]);
});

test('task events deduplicate and never regress a terminal state', () => {
  let state = logic.applyControllerStatus(logic.createState(), connected());
  state = logic.applyTaskEvent(state, { taskId: 'task-1', status: 'admitted' });
  state = logic.applyTaskEvent(state, { taskId: 'task-1', status: 'running' });
  state = logic.applyTaskEvent(state, { taskId: 'task-1', status: 'succeeded' });
  assert.equal(state.tasks[0].status, 'succeeded');

  const regressed = logic.applyTaskEvent(state, { taskId: 'task-1', status: 'running' });
  assert.equal(regressed, state, 'a terminal task must not go back to running');

  const duplicate = logic.applyTaskEvent(state, { taskId: 'task-1', status: 'succeeded' });
  assert.equal(duplicate, state, 'duplicate terminals must not create new entries');

  const resultUnknown = logic.applyTaskEvent(state, {
    taskId: 'task-2',
    status: 'result_unknown',
    outcomeUnknown: true,
  });
  assert.equal(resultUnknown.tasks.length, 2);
  assert.equal(resultUnknown.tasks[1].outcomeUnknown, true);
});

test('live-context matching rejects foreign connections and leases', () => {
  let state = logic.createState();
  state = logic.applyControllerStatus(state, connected());
  assert.equal(logic.matchesLiveContext(state, { connectionId: 'connection-1', leaseEpoch: '2' }), true);
  assert.equal(logic.matchesLiveContext(state, { connectionId: 'connection-2', leaseEpoch: '2' }), false);
  assert.equal(logic.matchesLiveContext(state, { connectionId: 'connection-1', leaseEpoch: '3' }), false);
  const idle = logic.createState();
  assert.equal(logic.matchesLiveContext(idle, {}), false);
});

test('bridge feature gates every desktop call behind the Windows capability', () => {
  const feature = fs.readFileSync(
    path.join(root, 'src', 'platform', 'tauri', 'bridge', 'desktop-remote-control.js'),
    'utf8',
  );
  assert.match(feature, /desktop_remote_control_unsupported/);
  assert.match(feature, /can\("desktopRemoteControl"\)/);
  // The bridge must never forward raw credential fields.
  assert.ok(!/api_key|apiKey|credential_ref|credentialRef/.test(feature));
  // Every desktop command it forwards is a desktop_remote_* command.
  const invokes = [...feature.matchAll(/invoke\("([^"]+)"/g)].map((match) => match[1]);
  const expected = [
    'controlled_start', 'controlled_refresh', 'controlled_stop', 'controlled_disconnect_now',
    'controlled_status', 'controlled_list_workspaces', 'controlled_authorize_workspace',
    'controller_connect', 'controller_disconnect', 'controller_status',
    'controller_list_history', 'controller_create_history', 'capabilities_snapshot',
    'task_cancel', 'task_status', 'journal_replay', 'journal_snapshot', 'journal_acknowledge',
  ].map(name => `desktop_remote_${name}`);
  assert.deepEqual([...new Set(invokes)].sort(), expected.sort());
  for (const command of invokes) {
    assert.ok(
      command.startsWith('desktop_remote_'),
      `unexpected command forwarded from the desktop bridge: ${command}`,
    );
  }
});
