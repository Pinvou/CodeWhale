import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { fileURLToPath, pathToFileURL } from 'node:url';

const app = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = (relative) => fs.readFileSync(path.join(app, 'src', relative), 'utf8');

function installFeature(onInvoke) {
  const root = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  const sandbox = { window: root, TextEncoder, setTimeout, clearTimeout };
  vm.createContext(sandbox);
  vm.runInContext(read('features/remote-control/remote-control-state.js'), sandbox);
  vm.runInContext(read('platform/tauri/bridge/desktop-remote-control.js'), sandbox);
  const calls = [];
  const listeners = new Map();
  const state = { desktopRemoteControl: root.__PINVOU_REMOTE_CONTROL_STATE__.createState() };
  const feature = root.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control']({
    state,
    notify() {},
    can: () => true,
    listen: (name, callback) => {
      listeners.set(name, callback);
      return Promise.resolve(() => listeners.delete(name));
    },
    invoke(command, args) {
      calls.push({ command, args });
      if (onInvoke) {
        const response = onInvoke(command, args);
        if (response !== undefined) return response;
      }
      if (command === 'desktop_remote_controller_status') return Promise.resolve({ status: 'idle' });
      return Promise.resolve({ state: 'queued' });
    },
  });
  return { feature, calls, state, listeners, logic: root.__PINVOU_REMOTE_CONTROL_STATE__ };
}

test('new A task forwards only explicit local binding and non-sensitive intent', async () => {
  const { feature, calls } = installFeature();
  await feature.submitRemoteTask('remote-local-1', 'opaque-ref-1', {
    kind: 'new', text: 'hello', modelSource: 'controlled_device',
  });
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_task_dispatch_a',
    args: { localSessionId: 'remote-local-1', remoteRef: 'opaque-ref-1', intent: {
      kind: 'new', text: 'hello', modelSource: 'controlled_device', selectedModelId: null,
    } },
  });
  assert.equal(calls.some(({ command }) => command === 'desktop_remote_task_submit'), false);
});

test('temporary model ID is an opaque selection, not a credential or wire package', async () => {
  const { feature, calls } = installFeature();
  await feature.submitRemoteTask('remote-local-1', 'opaque-ref-1', {
    kind: 'new', text: 'hello', modelSource: 'controller_temporary', selectedModelId: 'model-42',
  });
  assert.equal(calls.at(-1).args.intent.selectedModelId, 'model-42');
  await assert.rejects(feature.submitRemoteTask('remote-local-1', 'opaque-ref-1', {
    kind: 'new', text: 'hello', modelSource: 'controller_temporary',
  }), { code: 'remote_task_intent_invalid' });
  await assert.rejects(feature.submitRemoteTask('remote-local-1', 'opaque-ref-1', {
    kind: 'new', text: 'hello', modelSource: 'controlled_device', apiKey: 'secret',
  }), { code: 'remote_task_intent_invalid' });
  assert.equal(calls.length, 2, 'malformed intents never reach Rust');
});

test('retry is handle-only; abandon uses explicit local SessionStore identity', async () => {
  const { feature, calls } = installFeature();
  const handle = { taskId: 'task-1', clientRequestId: 'request-1', requestFingerprint: 'a'.repeat(64) };
  await feature.submitRemoteTask('remote-local-1', 'opaque-ref-1', { kind: 'retry', handle });
  assert.equal(calls.at(-1).args.intent.kind, 'retry');
  await feature.abandonLostRemoteTask('remote-local-1', 'request-1', 'task-1');
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_task_abandon_lost_a',
    args: { localSessionId: 'remote-local-1', clientRequestId: 'request-1', taskId: 'task-1' },
  });
  await assert.rejects(feature.submitRemoteTask('remote-local-1', 'opaque-ref-1', {
    kind: 'retry', handle, text: 'different prompt',
  }), { code: 'remote_task_intent_invalid' });
  assert.equal(calls.length, 3);
});

test('cancel send exposes only ACK identifiers, never a wire payload', async () => {
  const { feature, calls } = installFeature((command) => {
    if (command === 'desktop_remote_controller_task_cancel_send') return Promise.resolve({
      requestId: 'request-1', sessionId: 'session-1', taskId: 'task-1',
      clientRequestId: 'client-1', payload: { token: 'secret' },
    });
    return Promise.resolve({ status: 'idle' });
  });
  const ack = await feature.controllerTaskCancelSend('ref-1', 'task-1');
  assert.deepEqual(JSON.parse(JSON.stringify(ack)), {
    requestId: 'request-1', sessionId: 'session-1', taskId: 'task-1', clientRequestId: 'client-1',
  });
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_controller_task_cancel_send', args: { remoteRef: 'ref-1', taskId: 'task-1' },
  });
});

test('desktop remote control capability stays false until explicit Windows Rust flag', async () => {
  const desktopBridge = read('platform/tauri/bridge.js');
  assert.match(desktopBridge, /detected\.desktopRemoteControlSupported === true/);
  assert.match(desktopBridge, /detected\.os === "windows"/);
  const previousWindow = globalThis.window;
  const runtime = { loaded: false, os: 'unknown', desktopRemoteControl: false };
  globalThis.window = {
    PinvouPlatform: { kind: 'desktop', isWeb: false },
    TauriBridge: { available: true, state: { getMany: () => ({ platformCapabilities: runtime }) } },
  };
  try {
    const moduleUrl = pathToFileURL(path.join(app, 'src/shared/platform.js')).href;
    const { can } = await import(`${moduleUrl}?desktop-remote-contract`);
    assert.equal(can('desktopRemoteControl'), false);
    Object.assign(runtime, { loaded: true, os: 'linux', desktopRemoteControl: true });
    assert.equal(can('desktopRemoteControl'), false);
    Object.assign(runtime, { os: 'windows', desktopRemoteControl: true });
    assert.equal(can('desktopRemoteControl'), true);
    runtime.desktopRemoteControl = false;
    assert.equal(can('desktopRemoteControl'), false);
  } finally {
    globalThis.window = previousWindow;
  }
});

test('bridge installation is safe before runtime capability detection', () => {
  const root = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  const sandbox = { window: root, TextEncoder, setTimeout, clearTimeout };
  vm.createContext(sandbox);
  vm.runInContext(read('features/remote-control/remote-control-state.js'), sandbox);
  vm.runInContext(read('platform/tauri/bridge/desktop-remote-control.js'), sandbox);
  const calls = [];
  const api = root.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control']({
    state: {}, notify() {}, can: () => false,
    invoke: (command) => { calls.push(command); return Promise.resolve({}); },
    listen: () => Promise.resolve(() => {}),
  });
  assert.equal(api.isAvailable(), false);
  assert.deepEqual(calls, [], 'startup must not issue a remote command before Rust capability arrives');
});

test('model reads strip unexpected fields and discard a prior connection result', async () => {
  let resolveFirst;
  const { feature, state, logic } = installFeature((command) => {
    if (command === 'desktop_remote_model_list_exportable') {
      return new Promise((resolve) => { resolveFirst = resolve; });
    }
    return Promise.resolve({});
  });
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    status: 'connected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '1', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  const pending = feature.listExportableModels();
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    status: 'disconnected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '1', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  resolveFirst([{ modelId: 'model-1', displayName: 'safe', exportable: true, credential: 'secret' }]);
  assert.deepEqual(JSON.parse(JSON.stringify(await pending)), []);
  assert.equal(state.desktopRemoteControl.remoteModels.length, 0);

  const next = feature.listExportableModels();
  resolveFirst([{ modelId: 'model-1', displayName: 'safe', exportable: true, credential: 'secret' }]);
  const result = await next;
  assert.deepEqual(JSON.parse(JSON.stringify(result)), [
    { modelId: 'model-1', displayName: 'safe', exportable: true, reason: null, visionModelId: null },
  ]);
  assert.equal(JSON.stringify(result).includes('secret'), false);
});

test('A capability read uses its bound projection command and strips non-allowlisted data', async () => {
  const projection = {
    connectionId: 'connection-1', controllerLeaseId: 'lease-1', leaseEpoch: '2',
    relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1',
    capabilityRevision: '4', expiresAtMs: '70000', modes: ['chat', 'code', 'secret-mode'],
    tools: [{ toolId: 'tool-1', displayName: 'Tool', credential: 'secret' }],
    skills: [{ skillId: 'skill-1', displayName: 'Skill', privateKey: 'secret' }],
    workspaces: [{ workspaceId: 'workspace-1', displayName: 'Workspace', codeAllowed: true, hostPath: 'C:\\secret' }],
    token: 'secret',
  };
  const { feature, state, calls, logic } = installFeature((command) => {
    if (command === 'desktop_remote_controller_capability_snapshot') return Promise.resolve(projection);
    return Promise.reject(new Error(`unexpected command ${command}`));
  });
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    status: 'connected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  state.desktopRemoteControl = logic.applyControllerSession(state.desktopRemoteControl, {
    sessionRef: 'ref-1', sessionId: 'session-1', connectionId: 'connection-1', leaseEpoch: '2',
  });
  const result = await feature.capabilitiesSnapshot('ref-1');
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_controller_capability_snapshot', args: { remoteRef: 'ref-1' },
  });
  assert.equal(calls.some(({ command }) => command === 'desktop_remote_capabilities_snapshot'), false);
  assert.equal(result.capabilityRevision, 4);
  assert.deepEqual(JSON.parse(JSON.stringify(result.tools)), [{ displayName: 'Tool', toolId: 'tool-1' }]);
  assert.deepEqual(JSON.parse(JSON.stringify(result.modes)), ['chat', 'code']);
  assert.equal(JSON.stringify(result).includes('secret'), false);
  await assert.rejects(feature.capabilitiesSnapshot('other-ref'), { code: 'capability_snapshot_unavailable' });
  assert.equal(calls.filter(({ command }) => command === 'desktop_remote_controller_capability_snapshot').length,
    1, 'wrong ref must never invoke the capability command');
});

test('A capability read drops changed bindings and non-increasing revisions', async () => {
  let resolveRead;
  const { feature, state, calls, logic } = installFeature((command) => {
    if (command === 'desktop_remote_controller_capability_snapshot') {
      return new Promise((resolve) => { resolveRead = resolve; });
    }
    return Promise.resolve({});
  });
  const live = {
    status: 'connected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
  };
  const projection = {
    connectionId: 'connection-1', controllerLeaseId: 'lease-1', leaseEpoch: '2',
    relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1', capabilityRevision: '3',
    modes: ['chat'], tools: [], skills: [], workspaces: [],
  };
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, live);
  state.desktopRemoteControl = logic.applyControllerSession(state.desktopRemoteControl, {
    sessionRef: 'ref-1', sessionId: 'session-1', connectionId: 'connection-1', leaseEpoch: '2',
  });
  const pending = feature.capabilitiesSnapshot('ref-1');
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    status: 'disconnected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  resolveRead(projection);
  assert.equal(await pending, null);
  assert.equal(state.desktopRemoteControl.remoteCapabilities, null);

  state.desktopRemoteControl = logic.beginControllerIntent(state.desktopRemoteControl, true);
  const nextLive = {
    ...live, connectionId: 'connection-2', controllerLeaseId: 'lease-2',
    leaseEpoch: '3', relayEpoch: 'relay-2', generation: 'generation-2',
  };
  const nextProjection = {
    ...projection, connectionId: 'connection-2', controllerLeaseId: 'lease-2',
    leaseEpoch: '3', relayEpoch: 'relay-2', remoteControlGeneration: 'generation-2',
  };
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    ...nextLive, intent: state.desktopRemoteControl.intents.controller,
  });
  state.desktopRemoteControl = logic.applyControllerSession(state.desktopRemoteControl, {
    sessionRef: 'ref-2', sessionId: 'session-2', connectionId: 'connection-2', leaseEpoch: '3',
  });
  const next = feature.capabilitiesSnapshot('ref-2');
  resolveRead(nextProjection);
  assert.equal((await next).capabilityRevision, 3);
  const old = feature.capabilitiesSnapshot('ref-2');
  resolveRead({ ...nextProjection, capabilityRevision: '2' });
  assert.equal(await old, null);
  assert.equal(state.desktopRemoteControl.remoteCapabilities.capabilityRevision, 3);
  assert.equal(calls.filter(({ command }) => command === 'desktop_remote_controller_capability_snapshot').length, 3);
});

test('B start reconciles an identity-less terminal only from current authoritative status', async () => {
  let finishStart;
  let authoritative = { status: 'waiting_connection', generation: 'generation-new', pairingCode: '1234567' };
  const { feature, state, listeners, logic } = installFeature((command) => {
    if (command === 'desktop_remote_controlled_start') return new Promise((resolve) => { finishStart = resolve; });
    if (command === 'desktop_remote_controlled_status') return Promise.resolve(authoritative);
    return Promise.resolve({});
  });
  state.desktopRemoteControl = logic.applyControlledStatus(state.desktopRemoteControl, { status: 'stopped' });
  const pending = feature.controlledStart('Device', []);
  listeners.get('desktop-remote-controlled-status')({ payload: { status: 'stopped' } });
  finishStart({ status: 'waiting_connection', generation: 'generation-new', pairingCode: '1234567' });
  await pending;
  assert.equal(state.desktopRemoteControl.controlledFenced, false);
  assert.equal(state.desktopRemoteControl.controlledDevice.pairingCode, '1234567');

  state.desktopRemoteControl = logic.applyControlledStatus(state.desktopRemoteControl, {
    status: 'stopped', generation: 'generation-new',
  });
  assert.equal(state.desktopRemoteControl.controlledFenced, true);
  authoritative = { status: 'stopped' };
  const dead = feature.controlledStart('Device', []);
  assert.equal(state.desktopRemoteControl.controlledOpenIntent, state.desktopRemoteControl.intents.controlled);
  listeners.get('desktop-remote-controlled-status')({ payload: { status: 'stopped' } });
  assert.equal(state.desktopRemoteControl.controlledTerminatedOpenIntent, state.desktopRemoteControl.intents.controlled);
  finishStart({ status: 'waiting_connection', generation: 'generation-dead', pairingCode: '7654321' });
  await dead;
  assert.equal(state.desktopRemoteControl.controlledFenced, true);
  assert.notEqual(state.desktopRemoteControl.controlledDevice.pairingCode, '7654321');
});

test('A connect reconciles only a complete authoritative lease after an identity-less terminal', async () => {
  let finishConnect;
  let statusReads = 0;
  let authoritative = { status: 'connected', lease: {
    connectionId: 'connection-new', controllerLeaseId: 'lease-new', leaseEpoch: '2',
    relayEpoch: 'relay-new', remoteControlGeneration: 'generation-new',
  } };
  const { feature, state, listeners, logic } = installFeature((command) => {
    if (command === 'desktop_remote_controller_connect') return new Promise((resolve) => { finishConnect = resolve; });
    if (command === 'desktop_remote_controller_status') {
      statusReads += 1;
      return Promise.resolve(statusReads === 1 ? { status: 'idle' } : authoritative);
    }
    return Promise.resolve({});
  });
  await Promise.resolve();
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, { status: 'disconnected' });
  const pending = feature.controllerConnect('code');
  listeners.get('desktop-remote-controller-status')({ payload: { status: 'error' } });
  assert.equal(state.desktopRemoteControl.controllerTerminatedOpenIntent, state.desktopRemoteControl.intents.controller);
  finishConnect(authoritative);
  await pending;
  assert.equal(state.desktopRemoteControl.controllerFenced, false);
  assert.equal(state.desktopRemoteControl.controllerConnection.connectionId, 'connection-new');

  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    status: 'disconnected', connectionId: 'connection-new', controllerLeaseId: 'lease-new',
    leaseEpoch: '2', relayEpoch: 'relay-new', generation: 'generation-new',
  });
  authoritative = { status: 'disconnected' };
  const dead = feature.controllerConnect('code');
  listeners.get('desktop-remote-controller-status')({ payload: { status: 'error' } });
  finishConnect({ status: 'connected', lease: {
    connectionId: 'connection-dead', controllerLeaseId: 'lease-dead', leaseEpoch: '3',
    relayEpoch: 'relay-dead', remoteControlGeneration: 'generation-dead',
  } });
  await dead;
  assert.equal(state.desktopRemoteControl.controllerFenced, true);
  assert.equal(state.desktopRemoteControl.controllerConnection.connectionId, null);
});

test('a terminal event during authoritative status query blocks stale live recovery on both sides', async () => {
  let finishB;
  let finishBStatus;
  const b = installFeature((command) => {
    if (command === 'desktop_remote_controlled_start') return new Promise((resolve) => { finishB = resolve; });
    if (command === 'desktop_remote_controlled_status') return new Promise((resolve) => { finishBStatus = resolve; });
    return Promise.resolve({});
  });
  b.state.desktopRemoteControl = b.logic.applyControlledStatus(b.state.desktopRemoteControl, { status: 'stopped' });
  const bPending = b.feature.controlledStart('Device', []);
  b.listeners.get('desktop-remote-controlled-status')({ payload: { status: 'stopped' } });
  finishB({ status: 'waiting_connection', generation: 'generation-new' });
  await Promise.resolve();
  b.listeners.get('desktop-remote-controlled-status')({ payload: { status: 'stopped' } });
  finishBStatus({ status: 'waiting_connection', generation: 'generation-new', pairingCode: '1234567' });
  await bPending;
  assert.equal(b.state.desktopRemoteControl.controlledFenced, true);

  let finishA;
  let finishAStatus;
  let statusReads = 0;
  const a = installFeature((command) => {
    if (command === 'desktop_remote_controller_connect') return new Promise((resolve) => { finishA = resolve; });
    if (command === 'desktop_remote_controller_status') {
      statusReads += 1;
      return statusReads === 1 ? Promise.resolve({ status: 'idle' })
        : new Promise((resolve) => { finishAStatus = resolve; });
    }
    return Promise.resolve({});
  });
  await Promise.resolve();
  a.state.desktopRemoteControl = a.logic.applyControllerStatus(a.state.desktopRemoteControl, { status: 'disconnected' });
  const aPending = a.feature.controllerConnect('code');
  a.listeners.get('desktop-remote-controller-status')({ payload: { status: 'error' } });
  const live = { status: 'connected', lease: {
    connectionId: 'connection-new', controllerLeaseId: 'lease-new', leaseEpoch: '2',
    relayEpoch: 'relay-new', remoteControlGeneration: 'generation-new',
  } };
  finishA(live);
  await Promise.resolve();
  a.listeners.get('desktop-remote-controller-status')({ payload: { status: 'error' } });
  finishAStatus(live);
  await aPending;
  assert.equal(a.state.desktopRemoteControl.controllerFenced, true);
});

test('desktop_remote_proxy event is only a live-bound invalidation, never raw transcript content', async () => {
  const { feature, state, listeners, logic } = installFeature();
  await Promise.resolve();
  feature.subscribe();
  assert.equal(typeof listeners.get('desktop_remote_proxy'), 'function');
  state.desktopRemoteControl = logic.beginControllerIntent(state.desktopRemoteControl, true);
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    intent: state.desktopRemoteControl.intents.controller,
    status: 'connected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  const event = {
    kind: 'journal', localSessionId: 'remote-one', remoteSessionRef: 'ref-one',
    controlledSessionId: 'b-one', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
    fields: { journalEpoch: 'journal-1', sequence: 1, body: 'raw not displayable' },
  };
  listeners.get('desktop_remote_proxy')({ payload: event });
  assert.equal(state.desktopRemoteControl.remoteProxyNotice.localSessionId, 'remote-one');
  assert.equal(state.desktopRemoteControl.transcript.length, 0);
  listeners.get('desktop_remote_proxy')({ payload: { ...event, generation: 'stale' } });
  assert.equal(state.desktopRemoteControl.remoteProxyNotice.sequence, 1);
});

test('RemoteProxy history list/read are SessionStore-derived allowlists and delete is soft-hide', async () => {
  let deleted = false;
  const { feature, calls, state } = installFeature((command) => {
    if (command === 'desktop_remote_proxy_history_list') return Promise.resolve(deleted ? [] : [
      { localSessionId: 'remote-one', title: 'One', pinned: true,
        lastOpenedAt: '2026-09-27T01:00:00Z', projectedEntryCount: 1, credential: 'secret' },
      { localSessionId: 'local-chat', title: 'invalid' },
    ]);
    if (command === 'desktop_remote_proxy_history_read') return Promise.resolve({
      localSessionId: 'remote-one', title: 'One', pinned: true,
      lastOpenedAt: '2026-09-27T01:00:00Z', pendingUnknownCount: 1,
      authenticatedCursor: { journalEpoch: 'epoch-1', sequence: 1 },
      token: 'secret', entries: [{
        journalEpoch: 'epoch-1', sequence: 1, eventId: 'event-1', kind: 'user_message',
        taskId: 'task-1', body: 'Discuss an API key', privateKey: 'secret',
        attachments: [{ id: 'attachment-1', displayName: 'file.txt', contentType: 'text/plain',
          byteLength: 4, checksumSha256: 'a'.repeat(64), path: 'private-path' }],
      }],
    });
    if (command === 'desktop_remote_proxy_history_delete') {
      deleted = true;
      return Promise.resolve();
    }
    return Promise.resolve({});
  });
  const list = await feature.listRemoteProxyHistory();
  assert.equal(list.length, 1);
  assert.equal(list[0].localSessionId, 'remote-one');
  assert.equal(JSON.stringify(list).includes('secret'), false);
  const view = await feature.readRemoteProxyHistory('remote-one');
  assert.equal(view.entries[0].body, 'Discuss an API key', 'user-authored text is not keyword-filtered');
  assert.equal(JSON.stringify(view).includes('privateKey'), false);
  assert.equal(JSON.stringify(view).includes('private-path'), false);
  assert.equal(view.entries[0].attachments[0].id, 'attachment-1');
  assert.equal(await feature.deleteRemoteProxyHistory('remote-one'), undefined);
  assert.equal(state.desktopRemoteControl.remoteProxyHistory, null);
  assert.equal(state.desktopRemoteControl.remoteProxyHistories.length, 0);
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-2))), {
    command: 'desktop_remote_proxy_history_delete', args: { localSessionId: 'remote-one' },
  });
});

test('malformed RemoteProxy read/list responses fail closed instead of returning an older copy', async () => {
  const { feature, state } = installFeature((command) => {
    if (command === 'desktop_remote_proxy_history_list') return Promise.resolve({ token: 'wrong shape' });
    if (command === 'desktop_remote_proxy_history_read') return Promise.resolve({
      localSessionId: 'remote-one', entries: 'wrong shape',
    });
    return Promise.resolve({});
  });
  await assert.rejects(feature.listRemoteProxyHistory(), { code: 'remote_proxy_projection_invalid' });
  await assert.rejects(feature.readRemoteProxyHistory('remote-one'), { code: 'remote_proxy_projection_invalid' });
  assert.equal(state.desktopRemoteControl.remoteProxyHistory, null);
});

test('an older RemoteProxy read cannot replace a newer selected offline copy', async () => {
  const resolvers = [];
  const { feature, state } = installFeature((command) => {
    if (command === 'desktop_remote_proxy_history_read') {
      return new Promise((resolve) => { resolvers.push(resolve); });
    }
    return Promise.resolve({});
  });
  const first = feature.readRemoteProxyHistory('remote-one');
  const second = feature.readRemoteProxyHistory('remote-two');
  resolvers[1]({ localSessionId: 'remote-two', title: 'Two', entries: [], pendingUnknownCount: 0 });
  await second;
  resolvers[0]({ localSessionId: 'remote-one', title: 'One', entries: [], pendingUnknownCount: 0 });
  assert.equal(await first, null);
  assert.equal(state.desktopRemoteControl.remoteProxyHistory.localSessionId, 'remote-two');
});

test('an authenticated RemoteProxy notice refreshes the selected copy via SessionStore commands', async () => {
  const { feature, state, listeners, logic, calls } = installFeature((command) => {
    if (command === 'desktop_remote_proxy_history_list') return Promise.resolve([
      { localSessionId: 'remote-one', title: 'One', projectedEntryCount: 1 },
    ]);
    if (command === 'desktop_remote_proxy_history_read') return Promise.resolve({
      localSessionId: 'remote-one', title: 'One', entries: [{
        journalEpoch: 'epoch-1', sequence: 1, eventId: 'event-1', kind: 'assistant_message',
        body: 'stored response', attachments: [],
      }], pendingUnknownCount: 0,
    });
    return Promise.resolve({});
  });
  await Promise.resolve();
  feature.subscribe();
  await feature.readRemoteProxyHistory('remote-one');
  state.desktopRemoteControl = logic.beginControllerIntent(state.desktopRemoteControl, true);
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    intent: state.desktopRemoteControl.intents.controller, status: 'connected',
    connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '1', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  const before = calls.length;
  listeners.get('desktop_remote_proxy')({ payload: {
    kind: 'journal', localSessionId: 'remote-one', remoteSessionRef: 'ref-one',
    controlledSessionId: 'session-b', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '1', relayEpoch: 'relay-1', generation: 'generation-1',
    fields: { journalEpoch: 'epoch-1', sequence: 1, body: 'transport body must be ignored' },
  } });
  await new Promise((resolve) => setTimeout(resolve, 75));
  const refreshed = calls.slice(before).map(({ command }) => command);
  assert.ok(refreshed.includes('desktop_remote_proxy_history_list'));
  assert.ok(refreshed.includes('desktop_remote_proxy_history_read'));
  assert.equal(state.desktopRemoteControl.remoteProxyHistory.entries[0].body, 'stored response');
  assert.equal(JSON.stringify(state.desktopRemoteControl.remoteProxyHistory).includes('transport body'), false);
});
