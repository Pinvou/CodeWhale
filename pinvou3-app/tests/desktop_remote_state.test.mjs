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

function liveController(connectionId, leaseEpoch) {
  return {
    status: 'connected', connectionId, controllerLeaseId: `lease-${connectionId}`,
    leaseEpoch, relayEpoch: `relay-${connectionId}`,
    generation: `generation-${connectionId}`,
  };
}

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
  assert.equal(fresh.controlledDevice.pairingRemainingMs, null);

  const oldGeneration = logic.applyControlledStatus(fresh, {
    status: 'connected',
    generation: 'gen-1',
  });
  assert.equal(oldGeneration, fresh, 'a replaced generation must not overwrite state');
});

test('a relay registration result carries only the relative pairing countdown', () => {
  const state = logic.applyControlledStatus(logic.createState(), {
    status: 'waiting_for_pairing',
    pairingCode: '3456789',
    pairingRemainingMs: 30_000,
    generation: 'gen-1',
  });
  assert.equal(state.controlledDevice.pairingRemainingMs, 30_000);
  assert.equal('expiresAtMs' in state.controlledDevice, false);
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
    error: 'desktop_remote_watchdog_expired',
  });
  assert.equal(state.controlledDevice.status, 'error');
  assert.equal(state.controlledDevice.lastError, 'desktop_remote_watchdog_expired');
});

test('controller status is gated by connection and lease epoch', () => {
  let state = logic.createState();
  state = logic.applyControllerStatus(state, {
    ...liveController('connection-1', '2'),
  });
  assert.equal(state.controllerConnection.connectionId, 'connection-1');

  const otherConnection = logic.applyControllerStatus(state, {
    ...liveController('connection-2', '1'),
  });
  assert.equal(otherConnection, state, 'another connection must not take over the shell');

  const olderLease = logic.applyControllerStatus(state, {
    ...liveController('connection-1', '2'),
    leaseEpoch: '1',
  });
  assert.equal(olderLease, state, 'an older lease epoch must not overwrite the live one');

  const disconnected = logic.applyControllerStatus(state, {
    ...liveController('connection-1', '2'), status: 'idle',
  });
  assert.equal(disconnected.controllerConnection.status, 'idle');
  assert.equal(disconnected.remoteCapabilities, null);
});

test('capability snapshots are bound to connection, lease and revision', () => {
  let state = logic.createState();
  state = logic.applyControllerStatus(state, {
    ...liveController('connection-1', '2'),
  });
  const snapshot = {
    connectionId: 'connection-1',
    controllerLeaseId: 'lease-connection-1',
    leaseEpoch: '2',
    relayEpoch: 'relay-connection-1',
    remoteControlGeneration: 'generation-connection-1',
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
    ...liveController('connection-1', '2'),
  });
  state = logic.applyRemoteProxyBinding(state, {
    localSessionId: 'remote-one', remoteSessionRef: 'session-ref-1',
    sessionId: 'session-1', mode: 'chat', connectionId: 'connection-1',
    controllerLeaseId: 'lease-connection-1', leaseEpoch: '2',
    relayEpoch: 'relay-connection-1', remoteControlGeneration: 'generation-connection-1',
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

  state = logic.beginControlledIntent(state, true);
  state = logic.applyControlledStatus(state, {
    intent: state.intents.controlled,
    status: 'registering',
    generation: 'generation-2',
  });
  assert.equal(state.controlledSession.sessionId, null);
  assert.equal(state.controlledTasks.length, 0);
  assert.equal(state.controlledTranscript.length, 0);
});

test('B-side task content remains visible after a terminal failure', () => {
  let state = logic.createState();
  state = logic.applyControlledStatus(state, { status: 'connected', generation: 'generation-1' });
  state = logic.applyControlledSession(state, {
    sessionId: 'session-b',
    sessionRef: 'remote-session-b',
    generation: 'generation-1',
  });
  state = logic.applyControlledTaskEvent(state, {
    taskId: 'task-visible',
    sessionId: 'session-b',
    status: 'admitted',
    text: 'inspect the repository',
  });
  state = logic.applyControlledTaskEvent(state, {
    taskId: 'task-visible',
    sessionId: 'session-b',
    status: 'failed',
    terminalReason: 'task_engine_unavailable',
  });
  state = logic.applyTaskContent(state, {
    eventId: 'journal-partial-1',
    taskId: 'task-visible',
    sessionId: 'session-b',
    role: 'assistant',
    text: 'partial response',
  });
  assert.equal(state.controlledTasks[0].status, 'failed');
  assert.equal(state.controlledTasks[0].text, 'inspect the repository');
  assert.equal(state.controlledTasks[0].terminalReason, 'task_engine_unavailable');
  assert.equal(state.controlledTasks[0].assistantText, 'partial response');

  const duplicate = logic.applyTaskContent(state, {
    eventId: 'journal-partial-1',
    taskId: 'task-visible',
    sessionId: 'session-b',
    role: 'assistant',
    text: 'partial response',
  });
  assert.equal(duplicate, state);
  assert.equal(duplicate.controlledTasks[0].assistantText, 'partial response');
});

test('a new controlled-device generation drops old task projections', () => {
  let state = logic.createState();
  state = logic.applyControlledStatus(state, { status: 'connected', generation: 'generation-1' });
  state = logic.applyControlledSession(state, {
    sessionId: 'old-session',
    sessionRef: 'old-remote-session',
    generation: 'generation-1',
  });
  state = logic.applyControlledTaskEvent(state, {
    taskId: 'old-task',
    sessionId: 'old-session',
    status: 'failed',
    text: 'old prompt',
  });
  state = logic.applyControlledStatus(state, { status: 'stopped', generation: 'generation-1' });
  assert.equal(state.controlledTasks.length, 0);
  state = logic.beginControlledIntent(state, true);
  state = logic.applyControlledStatus(state, {
    intent: state.intents.controlled,
    status: 'waiting_for_pairing',
    generation: 'generation-2',
  });
  assert.equal(state.controlledTasks.length, 0);
});

test('live-context matching rejects foreign connections and leases', () => {
  let state = logic.createState();
  state = logic.applyControllerStatus(state, {
    ...liveController('connection-1', '2'),
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
  assert.doesNotMatch(feature, /"transcript",/, 'A history must not create a second WebView transcript');
  assert.match(feature, /"controlledTranscript",/, 'B local session projection remains available');
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

test('B live status without a generation cannot open the controlled chat', () => {
  const initial = logic.createState();
  assert.equal(logic.applyControlledStatus(initial, { status: 'connected' }), initial);
  assert.equal(logic.applyControlledStatus(initial, { status: 'waiting_for_pairing', pairingCode: '2345678' }), initial);
  const waiting = logic.applyControlledStatus(initial, { status: 'waiting_for_pairing', generation: 'old' });
  assert.equal(waiting.controlledDevice.status, 'waiting_for_pairing');
  assert.equal(waiting.controlledSession.sessionId, null);
});

test('authenticated RemoteProxy notifications require the exact live lease and never carry body text', () => {
  let state = logic.applyControllerStatus(logic.createState(), {
    status: 'connected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  state = logic.applyRemoteProxyBinding(state, {
    localSessionId: 'remote-abc', remoteSessionRef: 'ref-1', sessionId: 'session-b',
    mode: 'chat', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1',
    capabilityRevision: 5, mappingConfirmed: false,
  });
  const event = {
    kind: 'journal', localSessionId: 'remote-abc', remoteSessionRef: 'ref-1',
    controlledSessionId: 'session-b', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
    fields: { journalEpoch: 'journal-1', sequence: 1, body: 'not from SessionStore' },
  };
  assert.equal(logic.applyRemoteProxyNotice(state, { ...event, generation: 'old' }), state);
  assert.equal(logic.applyRemoteProxyNotice(state, { ...event, leaseEpoch: '1' }), state);
  assert.equal(logic.applyRemoteProxyNotice(state, { ...event, controllerLeaseId: 'other' }), state);
  assert.equal(logic.applyRemoteProxyNotice(state, { ...event, localSessionId: 'remote-other' }), state);
  assert.equal(logic.applyRemoteProxyNotice(state, { ...event, remoteSessionRef: 'ref-other' }), state);
  assert.equal(state.controllerConnection.mappingConfirmed, false);
  state = logic.applyRemoteProxyNotice(state, event);
  assert.equal(state.controllerConnection.mappingConfirmed, true);
  assert.equal(state.remoteProxyNotice.localSessionId, 'remote-abc');
  assert.equal(state.remoteProxyNotice.sequence, 1);
  assert.equal(JSON.stringify(state.remoteProxyNotice).includes('not from SessionStore'), false);
  assert.equal(Object.hasOwn(state, 'transcript'), false, 'A WebView has no second journal store');
  assert.equal(logic.applyRemoteProxyNotice(state, event), state, 'duplicate notice is ignored');
  const capability = { ...event, kind: 'capability', fields: { capabilityRevision: 5 } };
  state = logic.applyRemoteProxyNotice(state, capability);
  assert.equal(state.remoteProxyNotice.capabilityRevision, 5);
  assert.equal(logic.applyRemoteProxyNotice(state, {
    ...capability, fields: { capabilityRevision: 4 },
  }), state, 'an older capability revision cannot replace the notice');
  const disconnected = logic.applyControllerStatus(state, {
    status: 'disconnected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  assert.equal(disconnected.remoteProxyNotice, null);
  assert.equal(disconnected.controllerConnection.mappingConfirmed, false);
  assert.equal(logic.applyRemoteProxyNotice(disconnected, event), disconnected);
});

test('capability failure preserves a proven B mapping but never permits task dispatch', () => {
  let state = logic.beginControllerIntent(logic.createState(), true);
  state = logic.applyControllerStatus(state, {
    intent: state.intents.controller, status: 'connected',
    connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  const opened = {
    localSessionId: 'remote-one', remoteSessionRef: 'ref-1', sessionId: 'session-b',
    mode: 'chat', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1',
    capabilityRevision: 2,
  };
  const result = {
    ...opened, capabilityRequestId: 'request-1', capabilityRevision: null,
    capabilityError: 'capability_denied',
  };
  const unconfirmed = logic.applyRemoteProxyBinding(state, { ...opened, mappingConfirmed: false });
  const firstError = logic.applyRemoteProxyCapabilityResult(unconfirmed, {
    ...result, mappingConfirmed: false,
  });
  assert.equal(firstError.controllerConnection.mappingConfirmed, false);
  assert.equal(firstError.controllerConnection.capabilityRevision, null);
  assert.equal(firstError.controllerConnection.capabilityStatus, 'error');

  const mapped = logic.applyRemoteProxyBinding(state, { ...opened, mappingConfirmed: true });
  const renewalError = logic.applyRemoteProxyCapabilityResult(mapped, {
    ...result, mappingConfirmed: true,
  });
  assert.equal(renewalError.controllerConnection.mappingConfirmed, true);
  assert.equal(renewalError.controllerConnection.capabilityRevision, null);
  assert.equal(renewalError.controllerConnection.capabilityStatus, 'error');
  assert.equal(renewalError.controllerConnection.capabilityError, 'capability_denied');
});

test('terminal events from replaced A/B generations cannot clear the current UI', () => {
  let a = logic.applyControllerStatus(logic.createState(), {
    ...liveController('new', '3'),
  });
  assert.equal(logic.applyControllerStatus(a, {
    status: 'disconnected', connectionId: 'old', leaseEpoch: '2', generation: 'old-generation',
  }), a);
  let b = logic.applyControlledStatus(logic.createState(), {
    status: 'waiting_for_pairing', generation: 'new-generation',
  });
  assert.equal(logic.applyControlledStatus(b, {
    status: 'stopped', generation: 'old-generation',
  }), b);
});

test('a new A connection after terminal drops old RemoteProxy revision gates and session identity', () => {
  let state = logic.applyControllerStatus(logic.createState(), {
    status: 'connected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '1', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  state = logic.applyRemoteProxyBinding(state, {
    localSessionId: 'remote-one', remoteSessionRef: 'ref-old', sessionId: 'session-old',
    mode: 'chat', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '1', relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1',
    capabilityRevision: 9, mappingConfirmed: false,
  });
  state = logic.applyRemoteProxyNotice(state, {
    kind: 'capability', localSessionId: 'remote-one', remoteSessionRef: 'ref-old',
    controlledSessionId: 'session-old', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '1', relayEpoch: 'relay-1', generation: 'generation-1',
    fields: { capabilityRevision: 9 },
  });
  assert.equal(logic.applyControllerStatus(state, {
    status: 'connected', connectionId: 'connection-1', controllerLeaseId: 'lease-2',
    leaseEpoch: '2', relayEpoch: 'relay-2', generation: 'generation-2',
  }), state, 'an unsolicited replacement lease must not overwrite the active connection');
  state = logic.applyControllerStatus(state, {
    status: 'disconnected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '1', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  state = logic.beginControllerIntent(state, true);
  state = logic.applyControllerStatus(state, {
    intent: state.intents.controller, status: 'connected', connectionId: 'connection-2',
    controllerLeaseId: 'lease-2', leaseEpoch: '2', relayEpoch: 'relay-2', generation: 'generation-2',
  });
  assert.equal(state.controllerConnection.sessionRef, null);
  assert.equal(state.remoteProxyNotice, null);
  assert.equal(state.remoteProxyCapabilityRevisions['remote-one'], undefined);
});

test('B terminal tombstones an old generation until an explicit start opens a fresh one', () => {
  let state = logic.applyControlledStatus(logic.createState(), {
    status: 'connected', generation: 'generation-old',
  });
  state = logic.applyControlledStatus(state, { status: 'stopped', generation: 'generation-old' });
  assert.equal(state.controlledFenced, true);
  assert.equal(logic.applyControlledStatus(state, {
    status: 'connected', generation: 'generation-old',
  }), state);
  assert.equal(logic.applyControlledStatus(state, {
    status: 'waiting_for_pairing', generation: 'generation-new',
  }), state, 'an event cannot initiate a generation after terminal');
  state = logic.beginControlledIntent(state, true);
  assert.equal(logic.applyControlledStatus(state, {
    status: 'waiting_for_pairing', generation: 'generation-new',
  }), state, 'an event cannot consume an in-flight start intent');
  assert.equal(logic.applyControlledStatus(state, {
    status: 'waiting_for_pairing', generation: 'generation-old',
  }), state, 'even a pending start cannot reuse a retired generation');
  state = logic.applyControlledStatus(state, {
    intent: state.intents.controlled, status: 'waiting_for_pairing', generation: 'generation-new',
  });
  assert.equal(state.controlledDevice.generation, 'generation-new');
  assert.equal(state.controlledFenced, false);
  const unrequestedStart = logic.applyControlledStatus(state, {
    status: 'waiting_for_pairing', generation: 'generation-other',
  });
  assert.equal(unrequestedStart, state, 'a late starting event cannot replace an active generation');
});

test('A terminal tombstones connection plus lease and requires a new connect intent', () => {
  let state = logic.applyControllerStatus(logic.createState(), {
    ...liveController('connection-1', '1'),
  });
  state = logic.applyControllerStatus(state, {
    ...liveController('connection-1', '1'), status: 'disconnected',
  });
  assert.equal(state.controllerFenced, true);
  assert.equal(logic.applyControllerStatus(state, {
    ...liveController('connection-1', '1'),
  }), state);
  assert.equal(logic.applyControllerStatus(state, {
    ...liveController('connection-2', '1'),
  }), state, 'a foreign event cannot start a new connection');
  state = logic.beginControllerIntent(state, true);
  assert.equal(logic.applyControllerStatus(state, {
    ...liveController('connection-2', '1'),
  }), state, 'an event cannot consume an in-flight connect intent');
  assert.equal(logic.applyControllerStatus(state, {
    ...liveController('connection-1', '1'),
  }), state);
  state = logic.applyControllerStatus(state, {
    ...liveController('connection-2', '1'), intent: state.intents.controller,
  });
  assert.equal(state.controllerConnection.connectionId, 'connection-2');
  assert.equal(state.controllerFenced, false);
});

test('A live status requires its full matching lease and accepts a same-lease reconnect', () => {
  let state = logic.applyControllerStatus(logic.createState(), liveController('connection-1', '2'));
  assert.equal(logic.applyControllerStatus(state, { status: 'connected' }), state);
  assert.equal(logic.applyControllerStatus(state, { status: 'disconnected' }), state,
    'an unbound terminal event cannot clear an established A lease');
  assert.equal(logic.applyControllerStatus(state, {
    ...liveController('connection-1', '2'), controllerLeaseId: null,
  }), state);
  assert.equal(logic.applyControllerStatus(state, {
    ...liveController('connection-1', '2'), controllerLeaseId: 'foreign-lease',
  }), state);
  assert.equal(logic.applyControllerStatus(state, {
    ...liveController('connection-1', '2'), relayEpoch: 'foreign-relay',
  }), state);
  assert.equal(logic.applyControllerStatus(state, { status: 'safety_handshake' }), state);
  state = logic.applyControllerStatus(state, {
    ...liveController('connection-1', '2'), status: 'reconnecting',
  });
  assert.equal(state.controllerConnection.status, 'reconnecting');
  state = logic.applyControllerStatus(state, liveController('connection-1', '2'));
  assert.equal(state.controllerConnection.status, 'connected');
});

test('an unbound B terminal event cannot clear an established generation', () => {
  const state = logic.applyControlledStatus(logic.createState(), {
    status: 'connected', generation: 'generation-1',
  });
  assert.equal(logic.applyControlledStatus(state, { status: 'stopped' }), state);
  assert.equal(logic.applyControlledStatus(state, {
    status: 'stopped', generation: 'generation-old',
  }), state);
});

test('identity-less B terminal during start suppresses that reply but not a later start', () => {
  let state = logic.applyControlledStatus(logic.createState(), { status: 'stopped' });
  state = logic.beginControlledIntent(state, true);
  const first = state.intents.controlled;
  state = logic.applyControlledStatus(state, { status: 'stopped' });
  assert.equal(state.controlledTerminatedOpenIntent, first);
  assert.equal(logic.applyControlledStatus(state, {
    intent: first, status: 'waiting_for_pairing', generation: 'dead-generation', pairingCode: '1234567',
  }), state);
  state = logic.beginControlledIntent(state, true);
  const second = state.intents.controlled;
  state = logic.applyControlledStatus(state, {
    intent: second, status: 'waiting_for_pairing', generation: 'live-generation', pairingCode: '7654321',
  });
  assert.equal(state.controlledDevice.generation, 'live-generation');
  assert.equal(state.controlledDevice.pairingCode, '7654321');
});

test('identity-less A terminal during connect suppresses that reply but not a later connect', () => {
  let state = logic.applyControllerStatus(logic.createState(), { status: 'disconnected' });
  state = logic.beginControllerIntent(state, true);
  const first = state.intents.controller;
  state = logic.applyControllerStatus(state, { status: 'error' });
  assert.equal(state.controllerTerminatedOpenIntent, first);
  assert.equal(logic.applyControllerStatus(state, {
    ...liveController('dead-connection', '1'), intent: first,
  }), state);
  state = logic.beginControllerIntent(state, true);
  const second = state.intents.controller;
  state = logic.applyControllerStatus(state, {
    ...liveController('live-connection', '1'), intent: second,
  });
  assert.equal(state.controllerConnection.connectionId, 'live-connection');
  assert.equal(state.controllerFenced, false);
});

test('B pending-terminal fence opens only for a current authoritative generation', () => {
  let state = logic.applyControlledStatus(logic.createState(), { status: 'stopped' });
  state = logic.beginControlledIntent(state, true);
  const first = state.intents.controlled;
  state = logic.applyControlledStatus(state, { status: 'stopped' });
  assert.equal(logic.reconcileControlledOpenStatus(state, first, {
    status: 'stopped', generation: 'new-generation',
  }, 'new-generation'), state);
  assert.equal(logic.reconcileControlledOpenStatus(state, first, {
    status: 'waiting_for_pairing', generation: null,
  }, 'new-generation'), state);
  assert.equal(logic.reconcileControlledOpenStatus(state, first, {
    status: 'waiting_for_pairing', generation: 'other-generation',
  }, 'new-generation'), state);
  state = logic.reconcileControlledOpenStatus(state, first, {
    status: 'waiting_for_pairing', generation: 'new-generation', pairingCode: '1234567',
  }, 'new-generation');
  assert.equal(state.controlledFenced, false);
  assert.equal(state.controlledDevice.pairingCode, '1234567');
});

test('A pending-terminal fence opens only for a complete authoritative live lease', () => {
  let state = logic.applyControllerStatus(logic.createState(), { status: 'disconnected' });
  state = logic.beginControllerIntent(state, true);
  const first = state.intents.controller;
  state = logic.applyControllerStatus(state, { status: 'error' });
  assert.equal(logic.reconcileControllerOpenStatus(state, first, { status: 'disconnected' }), state);
  assert.equal(logic.reconcileControllerOpenStatus(state, first, {
    status: 'connected', connectionId: 'new-connection', leaseEpoch: '2',
  }), state);
  state = logic.reconcileControllerOpenStatus(state, first, liveController('new-connection', '2'));
  assert.equal(state.controllerFenced, false);
  assert.equal(state.controllerConnection.connectionId, 'new-connection');
});
