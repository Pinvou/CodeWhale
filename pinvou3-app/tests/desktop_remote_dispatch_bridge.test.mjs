import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { fileURLToPath, pathToFileURL } from 'node:url';

const app = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = (relative) => fs.readFileSync(path.join(app, 'src', relative), 'utf8');
const remoteTaskCommandSource = fs.readFileSync(
  path.join(app, 'src-tauri', 'src', 'app', 'commands', 'desktop_remote_task.rs'),
  'utf8',
);

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

function bindProxy(logic, state, remoteRef, sessionId) {
  const live = state.controllerConnection;
  return logic.applyRemoteProxyBinding(state, {
    localSessionId: 'remote-' + remoteRef, remoteSessionRef: remoteRef,
    sessionId, mode: 'chat', connectionId: live.connectionId,
    controllerLeaseId: live.controllerLeaseId, leaseEpoch: live.leaseEpoch,
    relayEpoch: live.relayEpoch, remoteControlGeneration: live.generation,
    mappingConfirmed: false,
  });
}

test('B status consumes only current Rust ControlledStatus fields', async () => {
  const { feature, state } = installFeature((command) => {
    if (command === 'desktop_remote_controlled_start') return Promise.resolve({
      status: 'waiting_for_pairing', generation: 'generation-current',
      pairingCode: '2345678', pairingRemainingMs: 30_000,
      kind: 'connected', remoteControlGeneration: 'generation-old', expiresAtMs: 1,
    });
    return undefined;
  });
  await feature.controlledStart('Device', []);
  assert.equal(state.desktopRemoteControl.controlledDevice.status, 'waiting_for_pairing');
  assert.equal(state.desktopRemoteControl.controlledDevice.generation, 'generation-current');
  assert.equal(state.desktopRemoteControl.controlledDevice.pairingRemainingMs, 30_000);
  assert.equal('pairingExpiresAtMs' in state.desktopRemoteControl.controlledDevice, false);
});

test('B refresh opens and projects the replacement generation after retiring the old code', async () => {
  const refresh = Promise.withResolvers();
  const { feature, state, listeners, logic } = installFeature((command) => {
    if (command === 'desktop_remote_controlled_refresh') {
      return refresh.promise;
    }
    return undefined;
  });
  state.desktopRemoteControl = logic.applyControlledStatus(state.desktopRemoteControl, {
    status: 'waiting_for_pairing', generation: 'generation-old',
    pairingCode: '2345678', pairingRemainingMs: 0,
  });

  const pending = feature.controlledRefresh();
  listeners.get('desktop-remote-controlled-status')({
    payload: { status: 'retiring_generation', generation: 'generation-old' },
  });
  listeners.get('desktop-remote-controlled-status')({
    payload: {
      status: 'waiting_for_pairing', generation: 'generation-new',
      pairingCode: '3456789', pairingRemainingMs: 300_000,
    },
  });
  refresh.resolve({
    status: 'waiting_for_pairing', generation: 'generation-new',
    pairingCode: '3456789', pairingRemainingMs: 300_000,
  });
  await pending;

  assert.equal(state.desktopRemoteControl.controlledDevice.status, 'waiting_for_pairing');
  assert.equal(state.desktopRemoteControl.controlledDevice.generation, 'generation-new');
  assert.equal(state.desktopRemoteControl.controlledDevice.pairingCode, '3456789');
  assert.equal(state.desktopRemoteControl.controlledDevice.pairingRemainingMs, 300_000);
  assert.deepEqual([...state.desktopRemoteControl.retiredControlledGenerations], ['generation-old']);
});

test('B workspace authorization exists only in the start transaction, never a live invoke', () => {
  const rustRoot = path.join(app, 'src-tauri', 'src');
  const rust = (...parts) => fs.readFileSync(path.join(rustRoot, ...parts), 'utf8');
  const command = rust('app', 'commands', 'desktop_remote_controlled.rs');
  const owner = rust('features', 'remote_control', 'controller', 'controlled.rs');
  const lib = rust('lib.rs');
  const feature = read('platform/tauri/bridge/desktop-remote-control.js');
  const domain = read('platform/tauri/bridge.js');
  for (const source of [command, owner, lib, feature, domain]) {
    assert.equal(source.includes('desktop_remote_controlled_authorize_workspace'), false);
    assert.equal(source.includes('controlledAuthorizeWorkspace'), false);
  }
  assert.match(command, /desktop_remote_controlled_start[\s\S]*?workspace_requests\(workspaces\)\?[\s\S]*?manager\.start\(device_name\.as_deref\(\), workspaces\)/);
});

test('permission scope is redacted by the Rust authority before the UI projection', () => {
  const permission = fs.readFileSync(path.join(app, 'src-tauri', 'src',
    'features', 'remote_control', 'controller', 'permission.rs'), 'utf8');
  assert.match(permission, /fn view_locked\(entry: &PendingPermission\) -> PermissionView \{[\s\S]*?target_scope: sanitize_scope\(&entry\.request\.target_scope\)/);
  assert.match(permission, /fn sanitize_scope\(scope: &str\) -> String/);
  assert.doesNotMatch(permission, /target_scope: entry\.request\.target_scope\.clone\(\)/);
});

test('B local send preserves local attachments and skills without leaking parsed fields into task input', async () => {
  const { feature, calls, state, logic } = installFeature((command) => {
    if (command === 'adopt_draft_attachment') return Promise.resolve({
      path: 'C:\\managed\\session-1\\report.txt',
      basename: 'report.txt',
      markdown: 'must-not-be-forwarded',
    });
    if (command === 'desktop_remote_controlled_task_send') return Promise.resolve({
      task_id: 'task-1', session_id: 'session-1', task_status: 'admitted',
      taskId: 'old-task', sessionId: 'old-session', taskStatus: 'failed',
    });
    return undefined;
  });
  state.desktopRemoteControl = logic.applyControlledStatus(state.desktopRemoteControl, {
    status: 'connected', generation: 'generation-1',
  });
  state.desktopRemoteControl = logic.applyControlledSession(state.desktopRemoteControl, {
    sessionId: 'session-1', sessionRef: 'ref-1', generation: 'generation-1', lane: 'chat',
  });
  const managed = { path: 'C:\\draft\\secret.txt', markdown: 'secret body' };
  Object.defineProperty(managed, '__pinvouManagedDraftAttachmentId', {
    value: 'desktop_attach_opaque', enumerable: false,
  });
  const attachment = { id: 7, basename: 'report.txt', status: 'ready', result: managed };
  state.attachments = [attachment];
  await feature.controlledTaskSend('session-1', 'hello', [attachment], ['skill-1']);
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_controlled_task_send',
    args: {
      sessionId: 'session-1', text: 'hello', skillIds: ['skill-1'],
      attachments: [{ path: 'C:\\managed\\session-1\\report.txt' }],
    },
  });
  assert.equal(JSON.stringify(calls.at(-1)).includes('secret body'), false);
  assert.equal(state.attachments.length, 0);
  assert.equal(state.desktopRemoteControl.controlledTasks[0].taskId, 'task-1');
  assert.equal(state.desktopRemoteControl.controlledTasks[0].status, 'admitted');
});

test('B local capability read reuses the negotiated snapshot command', async () => {
  const snapshot = {
    capabilityRevision: '7', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '1', relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1',
    sessionId: 'session-1', sessionLane: 'chat', expiresAtMs: Date.now() + 30_000,
    modes: ['chat'],
    skills: [{ skillId: 'skill-1', displayName: 'Skill 1', scopes: ['chat'], path: 'C:\\secret' }],
  };
  const { feature, calls } = installFeature((command) => {
    if (command === 'desktop_remote_controlled_capabilities_snapshot') return Promise.resolve(snapshot);
    return undefined;
  });
  const projected = await feature.controlledCapabilitiesSnapshot('session-1');
  assert.deepEqual(JSON.parse(JSON.stringify({ ...projected, expiresAtMs: 1 })), {
    connectionId: 'connection-1', controllerLeaseId: 'lease-1', leaseEpoch: '1',
    relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1',
    sessionId: 'session-1', sessionLane: 'chat', capabilityRevision: '7', expiresAtMs: 1,
    modes: ['chat'], tools: [],
    skills: [{ skillId: 'skill-1', displayName: 'Skill 1', scopes: ['chat'] }], workspaces: [],
  });
  assert.equal(calls.at(-1).command, 'desktop_remote_controlled_capabilities_snapshot');
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1).args)), { sessionId: 'session-1' });
});

test('B local text-only chat forwards an explicit empty skill selection', async () => {
  const { feature, calls } = installFeature((command) => {
    if (command === 'desktop_remote_controlled_task_send') return Promise.resolve({});
    return undefined;
  });
  await feature.controlledTaskSend('session-1', 'plain chat', [], []);
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_controlled_task_send',
    args: { sessionId: 'session-1', text: 'plain chat', skillIds: [], attachments: [] },
  });
});

test('B local Rust input derives chat/code lane and keeps attachment paths outside task payload', () => {
  const business = fs.readFileSync(path.join(app, 'src-tauri', 'src', 'app', 'commands',
    'desktop_remote_controller_business.rs'), 'utf8');
  assert.match(remoteTaskCommandSource, /code_mode: lane == RemoteSessionLane::Code/);
  assert.match(remoteTaskCommandSource, /local_controlled_attachments\(&session_root, attachments\)/);
  assert.match(business, /"mode": mode/);
  assert.match(business, /validate_skills_for_mode\(&lease, mode, &input\.skill_ids, now_b\)/);
  assert.match(business, /remote_task_input_digest\(text, &attachment_ids, &input\.skill_ids\)/);
  assert.match(business, /local_attachments: Some\(input\.attachments\)/);
  assert.doesNotMatch(business, /"path":\s*attachment\.path/);
});

test('B local stop is a task-only command during reconnect and rejects a replaced generation', async () => {
  let finishStop;
  const pending = new Promise((resolve) => { finishStop = resolve; });
  const { feature, calls, state, logic } = installFeature((command) => {
    if (command === 'desktop_remote_controlled_task_stop') return pending;
    return undefined;
  });
  state.desktopRemoteControl = logic.applyControlledStatus(state.desktopRemoteControl, {
    status: 'reconnecting', generation: 'generation-1', connectedDurationMs: 1_000,
  });
  state.desktopRemoteControl = logic.applyControlledSession(state.desktopRemoteControl, {
    sessionId: 'session-1', sessionRef: 'ref-1', generation: 'generation-1', lane: 'chat',
  });
  state.desktopRemoteControl = logic.applyControlledTaskEvent(state.desktopRemoteControl, {
    taskId: 'task-1', sessionId: 'session-1', generation: 'generation-1',
    modelSource: 'controller_temporary', status: 'running',
  });
  assert.equal(state.desktopRemoteControl.controlledTasks[0].modelSource, 'controller_temporary');
  const stopping = feature.controlledStopTask('task-1');
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_controlled_task_stop', args: { taskId: 'task-1' },
  });
  state.desktopRemoteControl = logic.beginControlledIntent(state.desktopRemoteControl, true);
  state.desktopRemoteControl = logic.applyControlledStatus(state.desktopRemoteControl, {
    intent: state.desktopRemoteControl.intents.controlled,
    status: 'registering', generation: 'generation-2',
  });
  finishStop({ taskId: 'task-1', sessionId: 'session-1', generation: 'generation-1',
    modelSource: 'controller_temporary', status: 'cancel_requested' });
  await stopping;
  assert.equal(state.desktopRemoteControl.controlledTasks.length, 0);
});

test('new A task forwards only explicit local binding and non-sensitive intent', async () => {
  const { feature, calls } = installFeature();
  await feature.reserveRemoteTask('remote-local-1', 'opaque-ref-1', {
    kind: 'new', text: 'hello', mode: 'chat', workspaceId: null, skillIds: [],
    modelSource: 'controlled_device',
  });
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_task_reserve_a',
    args: { localSessionId: 'remote-local-1', remoteRef: 'opaque-ref-1', intent: {
      kind: 'new', text: 'hello', mode: 'chat', workspaceId: null, skillIds: [],
      modelSource: 'controlled_device', selectedModelId: null,
    } },
  });
  assert.equal(calls.some(({ command }) => command === 'desktop_remote_task_submit'), false);
});

test('unreleased direct-submit and close-session Tauri commands are removed', () => {
  assert.doesNotMatch(remoteTaskCommandSource, /pub fn desktop_remote_task_submit\s*\(/);
  assert.doesNotMatch(remoteTaskCommandSource, /pub fn desktop_remote_task_close_session\s*\(/);
});

test('Rust live-open DTO is lease-matched before a local RemoteProxy ID becomes dispatchable', async () => {
  const dto = {
    localSessionId: 'remote-local-1', remoteSessionRef: 'opaque-ref-1', sessionId: 'wire-session-1',
    mode: 'chat', connectionId: 'connection-1', controllerLeaseId: 'controller-lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1',
    capabilityRevision: 3, temporaryModelAvailable: true,
  };
  const { feature, calls, state, logic } = installFeature((command) => {
    if (command === 'desktop_remote_proxy_open_live') return Promise.resolve(dto);
    return undefined;
  });
  let live = logic.beginControllerIntent(state.desktopRemoteControl, true);
  live = logic.applyControllerStatus(live, {
    intent: live.intents.controller, status: 'connected', connectionId: dto.connectionId,
    controllerLeaseId: dto.controllerLeaseId, leaseEpoch: dto.leaseEpoch,
    relayEpoch: dto.relayEpoch, generation: dto.remoteControlGeneration,
  });
  state.desktopRemoteControl = live;
  assert.deepEqual(JSON.parse(JSON.stringify(await feature.openLiveRemoteProxy(null))), {
    localSessionId: 'remote-local-1', remoteRef: 'opaque-ref-1',
  });
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_proxy_open_live', args: { existingLocalSessionId: null },
  });
  assert.equal(state.desktopRemoteControl.controllerConnection.localSessionId, 'remote-local-1');
  assert.equal(state.desktopRemoteControl.controllerConnection.sessionRef, 'opaque-ref-1');
  assert.equal(state.desktopRemoteControl.controllerConnection.mappingConfirmed, false);

  const bad = { ...dto, remoteControlGeneration: 'generation-old' };
  assert.equal(logic.applyRemoteProxyBinding(live, bad), live);
  assert.equal(logic.applyRemoteProxyBinding(live, { ...dto, remoteSessionRef: 'bad/slash' }), live);
  assert.equal(logic.applyRemoteProxyBinding(live, { ...dto, localSessionId: 'remote-other' }).controllerConnection.localSessionId,
    'remote-other', 'the Rust-issued ID, not a prefix guess, is accepted only after full lease verification');
});

test('narrow authenticated capability result settles only its matching RemoteProxy request', async () => {
  const { feature, state, listeners, logic } = installFeature((command) => {
    if (command === 'desktop_remote_controller_capability_send') {
      return Promise.resolve({ requestId: 'request-1', sessionId: 'session-1' });
    }
    return undefined;
  });
  feature.subscribe();
  await Promise.resolve();
  assert.equal(listeners.has('desktop-remote-controller-session'), false);
  assert.equal(typeof listeners.get('desktop-remote-proxy-capability'), 'function');
  state.desktopRemoteControl = logic.beginControllerIntent(state.desktopRemoteControl, true);
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    intent: state.desktopRemoteControl.intents.controller,
    status: 'connected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  state.desktopRemoteControl = bindProxy(logic, state.desktopRemoteControl, 'ref-1', 'session-1');
  const reply = {
    localSessionId: 'remote-ref-1', remoteSessionRef: 'ref-1', sessionId: 'session-1',
    connectionId: 'connection-1', controllerLeaseId: 'lease-1', leaseEpoch: '2',
    relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1',
    remoteSessionRef: 'ref-1',
    capabilityRequestId: 'request-1', capabilityRevision: 3,
    capabilityValidForMs: 30000, capabilityError: null,
    mappingConfirmed: true, temporaryModelAvailable: true,
  };
  const event = listeners.get('desktop-remote-proxy-capability');
  assert.equal(logic.matchesRemoteProxyCapabilityBinding(state.desktopRemoteControl, reply), true,
    JSON.stringify(state.desktopRemoteControl.controllerConnection));
  const pending = feature.controllerCapabilitySend('ref-1');
  await Promise.resolve();
  event({ payload: { ...reply, remoteSessionRef: 'other-ref' } });
  event({ payload: { ...reply, controllerLeaseId: 'old-lease' } });
  event({ payload: { ...reply, capabilityRequestId: 'request-other' } });
  assert.equal(state.desktopRemoteControl.controllerConnection.mappingConfirmed, false);
  event({ payload: reply });
  assert.equal((await pending).capabilityRequestId, 'request-1');
  assert.equal(state.desktopRemoteControl.controllerConnection.mappingConfirmed, true);
  assert.equal(state.desktopRemoteControl.controllerConnection.capabilityRevision, 3);
  feature.stop();
});

test('a matching capability failure fails closed without another request claiming it', async () => {
  let attempts = 0;
  const { feature, state, listeners, logic } = installFeature((command) => {
    if (command === 'desktop_remote_controller_capability_send') {
      const requestId = `request-${++attempts}`;
      Promise.resolve().then(() => listeners.get('desktop-remote-proxy-capability')({
        payload: { ...reply, capabilityRequestId: requestId },
      }));
      return Promise.resolve({ requestId, sessionId: 'session-1' });
    }
    return undefined;
  });
  feature.subscribe();
  await Promise.resolve();
  state.desktopRemoteControl = logic.beginControllerIntent(state.desktopRemoteControl, true);
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    intent: state.desktopRemoteControl.intents.controller,
    status: 'connected', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  state.desktopRemoteControl = bindProxy(logic, state.desktopRemoteControl, 'ref-1', 'session-1');
  const event = listeners.get('desktop-remote-proxy-capability');
  const reply = {
    localSessionId: 'remote-ref-1', remoteSessionRef: 'ref-1', sessionId: 'session-1',
    connectionId: 'connection-1', controllerLeaseId: 'lease-1', leaseEpoch: '2',
    relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1',
    capabilityRequestId: 'request-1', capabilityRevision: null,
    capabilityError: 'capability_denied', mappingConfirmed: false,
  };
  // A result that arrived before its ACK is buffered, not projected or
  // claimed by another request ID.
  event({ payload: reply });
  assert.equal(state.desktopRemoteControl.controllerConnection.mappingConfirmed, false);
  const pending = feature.controllerCapabilitySend('ref-1');
  await assert.rejects(pending, { code: 'capability_denied' });
  assert.equal(attempts, 3);
  assert.equal(state.desktopRemoteControl.controllerConnection.capabilityError, 'capability_denied');
  feature.stop();
});

test('temporary model ID is an opaque selection, not a credential or wire package', async () => {
  const { feature, calls } = installFeature();
  await feature.reserveRemoteTask('remote-local-1', 'opaque-ref-1', {
    kind: 'new', text: 'hello', mode: 'chat', workspaceId: null, skillIds: [],
    modelSource: 'controller_temporary', selectedModelId: 'model-42',
  });
  assert.equal(calls.at(-1).args.intent.selectedModelId, 'model-42');
  await assert.rejects(feature.reserveRemoteTask('remote-local-1', 'opaque-ref-1', {
    kind: 'new', text: 'hello', mode: 'chat', workspaceId: null, skillIds: [],
    modelSource: 'controller_temporary',
  }), { code: 'remote_task_intent_invalid' });
  await assert.rejects(feature.reserveRemoteTask('remote-local-1', 'opaque-ref-1', {
    kind: 'new', text: 'hello', mode: 'chat', workspaceId: null, skillIds: [],
    modelSource: 'controlled_device', apiKey: 'secret',
  }), { code: 'remote_task_intent_invalid' });
  assert.equal(calls.length, 2, 'malformed intents never reach Rust');
});

test('commit is handle-only; cancel and abandon use explicit local SessionStore identity', async () => {
  const { feature, calls } = installFeature();
  const handle = { taskId: 'task-1', clientRequestId: 'request-1', requestFingerprint: 'a'.repeat(64) };
  await feature.commitRemoteTask('remote-local-1', 'opaque-ref-1', handle, ['attachment-1']);
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_task_commit_a',
    args: { localSessionId: 'remote-local-1', remoteRef: 'opaque-ref-1', handle, attachmentIds: ['attachment-1'] },
  });
  await feature.cancelRemoteTask('remote-local-1', 'opaque-ref-1', 'task-1');
  assert.equal(calls.at(-1).command, 'desktop_remote_task_cancel_a');
  await feature.abandonRemoteTask('remote-local-1', 'request-1', 'task-1');
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_task_abandon_prepared_a',
    args: { localSessionId: 'remote-local-1', clientRequestId: 'request-1', taskId: 'task-1' },
  });
  await assert.rejects(feature.commitRemoteTask('remote-local-1', 'opaque-ref-1', { ...handle, text: 'different prompt' }, []),
    { code: 'remote_task_intent_invalid' });
  assert.equal(calls.length, 4);
});

test('remote attachment, permission and artifact facades use only desktop_remote_action_a', async () => {
  const { feature, calls } = installFeature((command, args) => {
    if (command !== 'desktop_remote_action_a') return undefined;
    switch (args.intent.operation) {
      case 'permission_list': return Promise.resolve({ permissions: [{
        permission_request_id: 'permission-1', task_id: 'task-1', operation_type: 'shell',
        risk_level: 'high', target_scope: 'workspace', preview: 'npm test', state: 'pending',
      }] });
      case 'attachment_begin': return Promise.resolve({ transfer_id: 'transfer-1', chunk_bytes: 4, total_bytes: 4 });
      case 'artifact_index': return Promise.resolve({ artifacts: [{
        artifact_id: 'artifact-1', task_id: 'task-1', display_name: 'report.txt',
        mime_type: 'text/plain', total_bytes: 4, sha256: 'a'.repeat(64), preview_kind: 'text',
      }] });
      default: return Promise.resolve({ ok: true });
    }
  });
  const permissions = await feature.permissionList('remote-local-1', 'opaque-ref-1', 'task-1');
  assert.equal(permissions[0].permissionRequestId, 'permission-1');
  const begin = await feature.attachmentBegin(
    'remote-local-1', 'opaque-ref-1', 'task-1', 'file.txt', 'text/plain', 4, 'a'.repeat(64),
  );
  assert.equal(begin.transferId, 'transfer-1');
  const artifacts = await feature.artifactIndex('remote-local-1', 'opaque-ref-1');
  assert.equal(artifacts[0].artifactId, 'artifact-1');
  assert.deepEqual([...new Set(calls.slice(-3).map(call => call.command))], ['desktop_remote_action_a']);
  assert.equal(JSON.stringify(calls).includes('connectionId'), false);
});

test('remote actions reuse one request id across uncertain retry and later reconciliation', async () => {
  let attempts = 0;
  const { feature, calls } = installFeature((command) => {
    if (command !== 'desktop_remote_action_a') return undefined;
    attempts += 1;
    if (attempts <= 2) return Promise.reject('result_unknown');
    return Promise.resolve({ permissions: [] });
  });
  await assert.rejects(
    feature.permissionList('remote-local-1', 'opaque-ref-1', 'task-1'),
    error => error.code === 'result_unknown' && typeof error.requestId === 'string',
  );
  await feature.permissionList('remote-local-1', 'opaque-ref-1', 'task-1');
  const actionCalls = calls.filter(call => call.command === 'desktop_remote_action_a');
  assert.equal(actionCalls.length, 3);
  assert.equal(new Set(actionCalls.map(call => call.args.requestId)).size, 1);
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
    remoteSessionRef: 'ref-1',
    capabilityRevision: '4', expiresAtMs: '70000', modes: ['chat', 'code', 'secret-mode'],
    tools: [{ toolId: 'tool-1', displayName: 'Tool', credential: 'secret' }],
    skills: [{ skillId: 'skill-1', displayName: 'Skill', scopes: ['chat'], privateKey: 'secret' }],
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
  state.desktopRemoteControl = bindProxy(logic, state.desktopRemoteControl, 'ref-1', 'session-1');
  const result = await feature.capabilitiesSnapshot('ref-1');
  assert.deepEqual(JSON.parse(JSON.stringify(calls.at(-1))), {
    command: 'desktop_remote_controller_capability_snapshot', args: { remoteRef: 'ref-1' },
  });
  assert.equal(calls.some(({ command }) => command === 'desktop_remote_capabilities_snapshot'), false);
  assert.equal(result.capabilityRevision, 4);
  assert.deepEqual(JSON.parse(JSON.stringify(result.tools)), [{ displayName: 'Tool', toolId: 'tool-1' }]);
  assert.deepEqual(JSON.parse(JSON.stringify(result.skills)), [
    { displayName: 'Skill', skillId: 'skill-1', scopes: ['chat'] },
  ]);
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
    remoteSessionRef: 'ref-1',
    modes: ['chat'], tools: [], skills: [], workspaces: [],
  };
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, live);
  state.desktopRemoteControl = bindProxy(logic, state.desktopRemoteControl, 'ref-1', 'session-1');
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
    remoteSessionRef: 'ref-2',
  };
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    ...nextLive, intent: state.desktopRemoteControl.intents.controller,
  });
  state.desktopRemoteControl = bindProxy(logic, state.desktopRemoteControl, 'ref-2', 'session-2');
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
  let authoritative = { status: 'waiting_for_pairing', generation: 'generation-new', pairingCode: '1234567' };
  const { feature, state, listeners, logic } = installFeature((command) => {
    if (command === 'desktop_remote_controlled_start') return new Promise((resolve) => { finishStart = resolve; });
    if (command === 'desktop_remote_controlled_status') return Promise.resolve(authoritative);
    return Promise.resolve({});
  });
  state.desktopRemoteControl = logic.applyControlledStatus(state.desktopRemoteControl, { status: 'stopped' });
  const pending = feature.controlledStart('Device', []);
  listeners.get('desktop-remote-controlled-status')({ payload: { status: 'stopped' } });
  finishStart({ status: 'waiting_for_pairing', generation: 'generation-new', pairingCode: '1234567' });
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
  finishStart({ status: 'waiting_for_pairing', generation: 'generation-dead', pairingCode: '7654321' });
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
  finishB({ status: 'waiting_for_pairing', generation: 'generation-new' });
  await Promise.resolve();
  b.listeners.get('desktop-remote-controlled-status')({ payload: { status: 'stopped' } });
  finishBStatus({ status: 'waiting_for_pairing', generation: 'generation-new', pairingCode: '1234567' });
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
  state.desktopRemoteControl = logic.applyRemoteProxyBinding(state.desktopRemoteControl, {
    localSessionId: 'remote-one', remoteSessionRef: 'ref-one', sessionId: 'b-one',
    mode: 'chat', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1',
    mappingConfirmed: false,
  });
  const event = {
    kind: 'journal', localSessionId: 'remote-one', remoteSessionRef: 'ref-one',
    controlledSessionId: 'b-one', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
    fields: { journalEpoch: 'journal-1', sequence: 1, body: 'raw not displayable' },
  };
  listeners.get('desktop_remote_proxy')({ payload: event });
  assert.equal(state.desktopRemoteControl.remoteProxyNotice.localSessionId, 'remote-one');
  assert.equal(Object.hasOwn(state.desktopRemoteControl, 'transcript'), false);
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
      lastOpenedAt: '2026-09-27T01:00:00Z', pendingUnknownCount: 1, unattemptedPending: [],
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
  state.activeSessionId = 'remote-one';
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

test('restart read exposes only allowlisted unattempted handles and rejects malformed pending', async () => {
  let malformed = false;
  const { feature } = installFeature((command) => {
    if (command === 'desktop_remote_proxy_history_read') return Promise.resolve({
      localSessionId: 'remote-one', entries: [], pendingUnknownCount: 1,
      unattemptedPending: [{
        taskId: 'task-1', clientRequestId: 'client-1', requestFingerprint: 'a'.repeat(64),
        dispatchState: malformed ? 'attempt_may_have_occurred' : 'prepared_never_attempted',
        credential: 'secret',
      }],
    });
    return Promise.resolve({});
  });
  const view = await feature.readRemoteProxyHistory('remote-one');
  assert.equal(view.unattemptedPending[0].taskId, 'task-1');
  assert.equal(view.pendingUnknownCount, 1);
  assert.equal(JSON.stringify(view).includes('credential'), false);
  malformed = true;
  await assert.rejects(feature.readRemoteProxyHistory('remote-one'), {
    code: 'remote_proxy_projection_invalid',
  });
});

test('a history read missing durable pending counters fails closed', async () => {
  const { feature } = installFeature((command) => {
    if (command === 'desktop_remote_proxy_history_read') return Promise.resolve({
      localSessionId: 'remote-one', entries: [], unattemptedPending: [],
    });
    return Promise.resolve({});
  });
  await assert.rejects(feature.readRemoteProxyHistory('remote-one'), {
    code: 'remote_proxy_projection_invalid',
  });
});

test('different RemoteProxy reads resolve independently without overwriting the selected copy', async () => {
  const resolvers = [];
  const { feature, state } = installFeature((command) => {
    if (command === 'desktop_remote_proxy_history_read') {
      return new Promise((resolve) => { resolvers.push(resolve); });
    }
    return Promise.resolve({});
  });
  state.activeSessionId = 'remote-two';
  const first = feature.readRemoteProxyHistory('remote-one');
  const second = feature.readRemoteProxyHistory('remote-two');
  resolvers[1]({ localSessionId: 'remote-two', title: 'Two', entries: [], pendingUnknownCount: 0, unattemptedPending: [] });
  await second;
  resolvers[0]({ localSessionId: 'remote-one', title: 'One', entries: [], pendingUnknownCount: 0, unattemptedPending: [] });
  assert.equal((await first).localSessionId, 'remote-one');
  assert.equal(state.desktopRemoteControl.remoteProxyHistory.localSessionId, 'remote-two');
});

test('a later read of the same RemoteProxy wins without publishing the old response', async () => {
  const resolvers = [];
  const { feature, state } = installFeature((command) => {
    if (command === 'desktop_remote_proxy_history_read') {
      return new Promise(resolve => resolvers.push(resolve));
    }
    return Promise.resolve({});
  });
  state.activeSessionId = 'remote-one';
  const older = feature.readRemoteProxyHistory('remote-one');
  const newer = feature.readRemoteProxyHistory('remote-one');
  resolvers[1]({ localSessionId: 'remote-one', title: 'New', entries: [], pendingUnknownCount: 0, unattemptedPending: [] });
  assert.equal((await newer).title, 'New');
  resolvers[0]({ localSessionId: 'remote-one', title: 'Old', entries: [], pendingUnknownCount: 0, unattemptedPending: [] });
  assert.equal(await older, null);
  assert.equal(state.desktopRemoteControl.remoteProxyHistory.title, 'New');
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
      }], pendingUnknownCount: 0, unattemptedPending: [],
    });
    return Promise.resolve({});
  });
  await Promise.resolve();
  feature.subscribe();
  state.activeSessionId = 'remote-one';
  await feature.readRemoteProxyHistory('remote-one');
  state.desktopRemoteControl = logic.beginControllerIntent(state.desktopRemoteControl, true);
  state.desktopRemoteControl = logic.applyControllerStatus(state.desktopRemoteControl, {
    intent: state.desktopRemoteControl.intents.controller, status: 'connected',
    connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '1', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  state.desktopRemoteControl = logic.applyRemoteProxyBinding(state.desktopRemoteControl, {
    localSessionId: 'remote-one', remoteSessionRef: 'ref-one', sessionId: 'session-b',
    mode: 'chat', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '1', relayEpoch: 'relay-1', remoteControlGeneration: 'generation-1',
    mappingConfirmed: false,
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
