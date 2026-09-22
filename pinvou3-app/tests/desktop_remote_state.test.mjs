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

test('late controlled-device results never revive an older intent', () => {
  let state = logic.createState();
  state = logic.beginControlledIntent(state);
  const firstIntent = state.intents.controlled;
  state = logic.beginControlledIntent(state);
  const secondIntent = state.intents.controlled;

  const stale = logic.applyControlledStatus(state, {
    intent: firstIntent,
    status: 'waiting_for_pairing',
    pairingCode: '2345678',
    generation: 'gen-1',
  });
  assert.equal(stale, state, 'stale intent must be ignored');

  const fresh = logic.applyControlledStatus(state, {
    intent: secondIntent,
    status: 'waiting_for_pairing',
    pairingCode: '3456789',
    generation: 'gen-2',
  });
  assert.equal(fresh.controlledDevice.status, 'waiting_for_pairing');
  assert.equal(fresh.controlledDevice.pairingCode, '3456789');
  assert.equal(fresh.controlledDevice.expiresAtMs, null);

  const oldGeneration = logic.applyControlledStatus(fresh, {
    status: 'connected',
    generation: 'gen-1',
  });
  assert.equal(oldGeneration, fresh, 'a replaced generation must not overwrite state');
});

test('a relay registration result carries the pairing expiry into the UI state', () => {
  const state = logic.applyControlledStatus(logic.createState(), {
    status: 'waiting_for_pairing',
    pairingCode: '3456789',
    pairingExpiresAtMs: 1_800_000_000_000,
    generation: 'gen-1',
  });
  assert.equal(state.controlledDevice.expiresAtMs, 1_800_000_000_000);
});

test('a stopped controlled device clears the pairing code', () => {
  let state = logic.createState();
  state = logic.applyControlledStatus(state, {
    status: 'waiting_for_pairing',
    pairingCode: '3456789',
    generation: 'gen-1',
  });
  state = logic.applyControlledStatus(state, { status: 'stopped', generation: 'gen-1' });
  assert.equal(state.controlledDevice.status, 'stopped');
  assert.equal(state.controlledDevice.pairingCode, null);
});

test('controlled status preserves the bounded Rust error code for diagnostics', () => {
  const state = logic.applyControlledStatus(logic.createState(), {
    status: 'error',
    errorCode: 'desktop_remote_watchdog_expired',
  });
  assert.equal(state.controlledDevice.status, 'error');
  assert.equal(state.controlledDevice.lastError, 'desktop_remote_watchdog_expired');
});

test('controller status is gated by connection and lease epoch', () => {
  let state = logic.createState();
  state = logic.applyControllerStatus(state, {
    status: 'connected',
    connectionId: 'connection-1',
    leaseEpoch: '2',
  });
  assert.equal(state.controllerConnection.connectionId, 'connection-1');

  const otherConnection = logic.applyControllerStatus(state, {
    status: 'connected',
    connectionId: 'connection-2',
    leaseEpoch: '1',
  });
  assert.equal(otherConnection, state, 'another connection must not take over the shell');

  const olderLease = logic.applyControllerStatus(state, {
    status: 'connected',
    connectionId: 'connection-1',
    leaseEpoch: '1',
  });
  assert.equal(olderLease, state, 'an older lease epoch must not overwrite the live one');

  const disconnected = logic.applyControllerStatus(state, { status: 'idle' });
  assert.equal(disconnected.controllerConnection.status, 'idle');
  assert.equal(disconnected.remoteCapabilities, null);
});

test('capability snapshots are bound to connection, lease and revision', () => {
  let state = logic.createState();
  state = logic.applyControllerStatus(state, {
    status: 'connected',
    connectionId: 'connection-1',
    leaseEpoch: '2',
  });
  const snapshot = {
    connectionId: 'connection-1',
    leaseEpoch: '2',
    relayEpoch: 'relay-1',
    remoteControlGeneration: 'gen-1',
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

test('capability preparation exposes pending and bounded failure states', () => {
  let state = logic.createState();
  state = logic.applyControllerStatus(state, {
    status: 'connected',
    connectionId: 'connection-1',
    leaseEpoch: '2',
  });
  state = logic.applyControllerSession(state, {
    sessionRef: 'session-ref-1',
    sessionId: 'session-1',
    connectionId: 'connection-1',
    leaseEpoch: '2',
    mode: 'chat',
  });
  state = logic.applyCapabilityPending(state, { sessionRef: 'session-ref-1' });
  assert.equal(state.controllerConnection.capabilityStatus, 'preparing');
  assert.equal(state.controllerConnection.capabilityError, null);

  state = logic.applyCapabilityFailure(state, {
    sessionRef: 'session-ref-1',
    requestId: 'request-1',
    code: 'capability_snapshot_timeout',
  });
  assert.equal(state.controllerConnection.capabilityStatus, 'error');
  assert.equal(state.controllerConnection.capabilityError, 'capability_snapshot_timeout');
  assert.equal(state.controllerConnection.capabilityRevision, null);
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
  let state = logic.createState();
  state = logic.applyTaskEvent(state, { taskId: 'task-1', status: 'admitted' });
  state = logic.applyTaskEvent(state, { taskId: 'task-1', status: 'running' });
  state = logic.applyTaskEvent(state, {
    taskId: 'task-1',
    status: 'admitted',
    text: 'late prompt projection',
  });
  assert.equal(state.tasks[0].status, 'running', 'late admitted must not regress running');
  assert.equal(state.tasks[0].text, 'late prompt projection', 'late prompt text is still retained');
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

test('B-side task projection keeps locally visible prompt text across status changes', () => {
  let state = logic.createState();
  state = logic.applyControlledStatus(state, {
    status: 'connected',
    generation: 'generation-b',
  });
  state = logic.applyControlledSession(state, {
    sessionId: 'session-b',
    sessionRef: 'session-ref-a',
    generation: 'generation-b',
  });
  state = logic.applyControlledTaskEvent(state, {
    taskId: 'task-visible',
    sessionId: 'session-b',
    status: 'admitted',
  });
  state = logic.applyControlledTaskEvent(state, {
    taskId: 'task-visible',
    sessionId: 'session-b',
    status: 'admitted',
    text: 'inspect the repository',
  });
  assert.equal(state.controlledTasks[0].text, 'inspect the repository');

  state = logic.applyControlledTaskEvent(state, {
    task_id: 'task-visible',
    session_id: 'session-b',
    task_status: 'running',
  });
  assert.equal(state.controlledTasks[0].status, 'running');
  assert.equal(state.controlledTasks[0].text, 'inspect the repository');
});

test('B-side projections reject stale sessions and reset with the controlled generation', () => {
  let state = logic.createState();
  state = logic.applyControlledStatus(state, {
    status: 'connected',
    generation: 'generation-1',
  });
  state = logic.applyControlledSession(state, {
    sessionId: 'session-current',
    sessionRef: 'session-ref-current',
    generation: 'generation-1',
  });

  const staleTask = logic.applyControlledTaskEvent(state, {
    taskId: 'task-stale',
    sessionId: 'session-old',
    status: 'running',
  });
  assert.equal(staleTask, state);

  const staleJournal = logic.applyControlledJournalEvent(state, {
    event: {
      sessionId: 'session-old',
      epoch: 'epoch-1',
      sequence: 1,
      kind: 'user_message',
      payload: { text: 'stale' },
    },
  });
  assert.equal(staleJournal, state);

  state = logic.applyControlledTaskEvent(state, {
    taskId: 'task-current',
    sessionId: 'session-current',
    status: 'running',
  });
  state = logic.applyControlledJournalEvent(state, {
    event: {
      sessionId: 'session-current',
      epoch: 'epoch-1',
      sequence: 1,
      kind: 'user_message',
      taskId: 'task-current',
      payload: { text: 'current' },
    },
  });
  assert.equal(state.controlledTasks.length, 1);
  assert.equal(state.controlledTranscript.length, 1);

  state = logic.applyControlledStatus(state, {
    status: 'reserving',
    generation: 'generation-2',
  });
  assert.equal(state.controlledSession.sessionId, null);
  assert.equal(state.controlledTasks.length, 0);
  assert.equal(state.controlledTranscript.length, 0);
});

test('live-context matching rejects foreign connections and leases', () => {
  let state = logic.createState();
  state = logic.applyControllerStatus(state, {
    status: 'connected',
    connectionId: 'connection-1',
    leaseEpoch: '2',
  });
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
  assert.match(feature, /"transcript",/, 'authoritative remote journal must be mirrored into ChatView state');
  // The bridge must never forward raw credential fields.
  assert.ok(!/api_key|apiKey|credential_ref|credentialRef/.test(feature));
  // Every desktop command it forwards is a desktop_remote_* command.
  const invokes = [...feature.matchAll(/invoke\("([^"]+)"/g)].map((match) => match[1]);
  assert.ok(invokes.length > 20);
  for (const command of invokes) {
    assert.ok(
      command.startsWith('desktop_remote_'),
      `unexpected command forwarded from the desktop bridge: ${command}`,
    );
  }
});
