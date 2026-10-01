// E2E-03 contract: the A-side remote session identity and capability revision
// live in the UI state, and the frontend never assembles protocol fields — the
// bridge forwards UI-level fields only and Rust builds every wire payload.
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
const statusFixtures = JSON.parse(fs.readFileSync(path.join(root, 'tests/fixtures/desktop_remote_status.json'), 'utf8'));

const bridge = fs.readFileSync(
  path.join(root, 'src', 'platform', 'tauri', 'bridge', 'desktop-remote-control.js'),
  'utf8',
);
const desktopFacade = fs.readFileSync(
  path.join(root, 'src', 'platform', 'tauri', 'bridge.js'),
  'utf8',
);

function connectedState() {
  return logic.applyControllerStatus(logic.createState(), {
    status: 'connected',
    connectionId: 'connection-1',
    controllerLeaseId: 'controller-lease-1',
    leaseEpoch: '2',
    relayEpoch: 'relay-1',
    generation: 'generation-1',
  });
}

function bind(state = connectedState(), overrides = {}) {
  return logic.applyRemoteProxyBinding(state, {
    localSessionId: 'remote-local-1', remoteSessionRef: 'session-ref-1',
    sessionId: 'session-abc', mode: 'chat', connectionId: 'connection-1',
    controllerLeaseId: 'controller-lease-1', leaseEpoch: '2', relayEpoch: 'relay-1',
    remoteControlGeneration: 'generation-1', capabilityRevision: 4,
    temporaryModelAvailable: true, mappingConfirmed: false,
    ...overrides,
  });
}

test('the authenticated RemoteProxy open carries the real remote session id into UI state', () => {
  const state = bind();
  assert.equal(state.controllerConnection.sessionId, 'session-abc');
  assert.equal(state.controllerConnection.sessionRef, 'session-ref-1');
  assert.equal(state.controllerConnection.sessionMode, 'chat');
  assert.equal(state.controllerConnection.capabilityRevision, 4);
  assert.equal(state.controllerConnection.temporaryModelAvailable, true);
});

test('foreign connections, older leases and terminal states never adopt a RemoteProxy binding', () => {
  const base = connectedState();
  const foreign = bind(base, { connectionId: 'connection-9' });
  assert.equal(foreign, base, 'another connection must not take over the session');
  const olderLease = bind(base, { leaseEpoch: '1' });
  assert.equal(olderLease, base, 'an older lease must not take over the session');
  const withSession = bind(base);
  assert.equal(withSession.controllerConnection.sessionId, 'session-abc');
  const disconnected = logic.applyControllerStatus(withSession, {
    status: 'disconnected', connectionId: 'connection-1', controllerLeaseId: 'controller-lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1',
  });
  assert.equal(disconnected.controllerConnection.sessionId, null);
  assert.equal(disconnected.controllerConnection.sessionRef, null);
  assert.equal(disconnected.controllerConnection.capabilityRevision, null);
  assert.equal(disconnected.controllerConnection.temporaryModelAvailable, false);
});

test('the bridge maps the lease-bound temporary-model decision without exposing credentials', () => {
  const mapping = bridge.slice(
    bridge.indexOf('function openLiveRemoteProxy'),
    bridge.indexOf('function readRemoteProxyHistory'),
  );
  assert.ok(mapping.includes('logic.applyRemoteProxyBinding'));
  assert.ok(!/credential|apiKey|privateKey/.test(mapping));
});

test('an absent temporary-model projection fails closed for mixed client versions', () => {
  const state = bind(connectedState(), { temporaryModelAvailable: undefined });
  assert.equal(state.controllerConnection.temporaryModelAvailable, false);
});

test('session projection requires the exact full tuple including large epochs and an online connection', () => {
  const live = { ...statusFixtures.controllerConnected.lease, leaseEpoch: '9007199254740993' };
  const connected = { ...statusFixtures.controllerConnected, lease: live };
  const base = logic.applyControllerStatus(logic.createState(), connected);
  const projection = { ...live, localSessionId: 'remote-large-epoch', remoteSessionRef: 'session-ref-1', sessionId: 'session-abc', mode: 'chat', capabilityRevision: 7 };
  const withSession = logic.applyRemoteProxyBinding(base, projection);
  assert.equal(withSession.controllerConnection.sessionId, 'session-abc');
  for (const key of ['connectionId', 'controllerLeaseId', 'leaseEpoch', 'relayEpoch', 'remoteControlGeneration']) {
    const stale = { ...projection, sessionId: 'stale-session', [key]: key === 'leaseEpoch' ? '9007199254740992' : 'foreign-binding' };
    assert.equal(logic.applyRemoteProxyBinding(withSession, stale), withSession, `${key} must match exactly`);
    assert.equal(logic.applyRemoteProxyBinding(withSession, { ...projection, [key]: null }), withSession, `${key} must not be omitted`);
  }
  const reconnecting = logic.applyControllerStatus(withSession, { ...connected, status: 'reconnecting', lease: null });
  assert.equal(reconnecting.controllerConnection.sessionId, 'session-abc');
  assert.equal(logic.applyRemoteProxyBinding(reconnecting, { ...projection, sessionId: 'late-session' }), reconnecting);
  const renewed = logic.applyControllerStatus(reconnecting, { ...connected, lease: { ...live, leaseEpoch: '9007199254740994' } });
  assert.equal(renewed.controllerConnection.sessionId, null);
  assert.equal(renewed.controllerConnection.capabilityRevision, null);
});

test('capability snapshots keep the revision and the negotiated modes', () => {
  let state = bind();
  state = logic.applyCapabilities(state, {
    ...statusFixtures.controllerConnected.lease,
    connectionId: 'connection-1',
    controllerLeaseId: 'controller-lease-1',
    leaseEpoch: '2',
    relayEpoch: 'relay-1',
    remoteControlGeneration: 'generation-1',
    capabilityRevision: 7,
    modes: ['chat'],
    tools: [],
    skills: [],
    workspaces: [],
  });
  assert.equal(state.remoteCapabilities.capabilityRevision, 7);
  assert.deepEqual(state.remoteCapabilities.modes, ['chat']);
  // 视图据此拿到「远程 sessionId + capability revision」两件事。
  assert.equal(state.controllerConnection.sessionId, 'session-abc');
  assert.equal(state.remoteCapabilities.capabilityRevision, 7);
});

test('the bridge forwards UI fields only and never builds protocol payloads', () => {
  for (const method of [
    'openLiveRemoteProxy',
    'controllerCapabilitySend',
    'submitRemoteTask',
  ]) {
    assert.ok(bridge.includes(`${method}: ${method}`), `${method} must be exported`);
  }
  // The dispatch bridge forwards bounded UI intent; Rust owns wire construction.
  const dispatch = bridge.slice(
    bridge.indexOf('function submitRemoteTask'),
    bridge.indexOf('function abandonPreparedRemoteTask'),
  );
  assert.ok(dispatch.includes('invoke("desktop_remote_task_dispatch_a"'));
  assert.ok(!/input_digest|inputDigest|connection_id|controller_lease_id|relay_epoch/.test(dispatch));
  const uiPaths = [
    bridge.slice(
      bridge.indexOf('function openLiveRemoteProxy'),
      bridge.indexOf('function readRemoteProxyHistory'),
    ),
  ].join('\n');
  assert.ok(
    !/input_digest|client_request_id|capability_revision|controller_lease_id|relay_epoch/.test(uiPaths),
    'UI paths must not carry protocol field names',
  );
  assert.ok(bridge.includes('"desktop-remote-proxy-capability"'));
  assert.ok(!bridge.includes('"desktop-remote-controller-session"'));
  assert.ok(bridge.includes('settleCapabilityResponse(payload)'));
});

test('controller command results preserve the complete Rust DTO for shared state projection', () => {
  const apply = bridge.slice(
    bridge.indexOf('function applyController'),
    bridge.indexOf('function controlledStart'),
  );
  assert.ok(apply.includes('controllerStatusPayload(result)'));
  assert.ok(apply.includes('connectionId: payload.connectionId'));
  assert.ok(apply.includes('leaseEpoch: payload.leaseEpoch'));
  assert.ok(apply.includes('deviceName: payload.deviceName'));
  const mapping = bridge.slice(
    bridge.indexOf('function controllerStatusPayload'),
    bridge.indexOf('function rememberCapabilityOutcome'),
  );
  assert.ok(mapping.includes('wire.lease'));
  assert.ok(mapping.includes('lease.connectionId'));
  assert.ok(mapping.includes('lease.leaseEpoch'));
});

test('the desktop facade exposes the A-side session and task bridge methods', () => {
  for (const method of [
    'openLiveRemoteProxy',
    'controllerCapabilitySend',
    'submitRemoteTask',
  ]) {
    assert.ok(
      desktopFacade.includes(`${method}: desktopRemoteControlFeature.${method}`),
      `${method} must be exposed by the public desktop bridge facade`,
    );
  }
});

test('legacy wire-building entry points are absent', () => {
  assert.ok(!bridge.includes('desktop_remote_controller_apply_capability_snapshot'));
  assert.ok(!bridge.includes('desktop_remote_controller_task_send'));
  assert.ok(!desktopFacade.includes('controllerApplyCapabilitySnapshot:'));
  assert.ok(!desktopFacade.includes('controllerTaskSend:'));
  for (const method of ['controllerCapabilityRequest', 'controllerTaskIntent', 'controllerTaskCancelIntent', 'controllerTaskCancelSend']) {
    assert.ok(!bridge.includes(`function ${method}`));
    assert.ok(!bridge.includes(`${method}: ${method}`));
    assert.ok(!desktopFacade.includes(`${method}:`));
  }
  assert.ok(!bridge.includes('desktop_remote_controller_task_intent'));
  assert.ok(!bridge.includes('desktop_remote_controller_task_cancel_intent'));
  assert.ok(!bridge.includes('desktop_remote_controller_capability_request'));
  assert.ok(!bridge.includes('desktop_remote_controller_task_cancel_send'));
});

test('capability send consumes a narrow acknowledgement, not a wire payload', () => {
  const capabilitySend = bridge.slice(
    bridge.indexOf('function capabilityAttempt'),
    bridge.indexOf('function scheduleCapabilityRenewal'),
  );
  assert.ok(capabilitySend.includes('waitForCapabilityResponse(intent, remoteRef)'));
  assert.ok(!capabilitySend.includes('intent.payload'));
});

test('capability preparation waits for a response, retries finitely and renews before expiry', () => {
  assert.ok(bridge.includes('CAPABILITY_RESPONSE_TIMEOUT_MS = 8_000'));
  assert.ok(bridge.includes('CAPABILITY_MAX_ATTEMPTS = 3'));
  assert.ok(bridge.includes('settleCapabilityResponse(payload)'));
  assert.ok(bridge.includes('waitForCapabilityResponse(intent, remoteRef)'));
  assert.ok(bridge.includes('attempt >= CAPABILITY_MAX_ATTEMPTS'));
  assert.ok(bridge.includes('validForMs - CAPABILITY_RENEWAL_LEAD_MS'));
  assert.ok(bridge.includes('beginCapabilityHandshake(sessionRef, true)'));
});

test('opening a live RemoteProxy reuses only an explicitly selected local ID', () => {
  const open = bridge.slice(
    bridge.indexOf('function openLiveRemoteProxy'),
    bridge.indexOf('function readRemoteProxyHistory'),
  );
  assert.ok(open.includes('existingLocalSessionId: existingLocalSessionId || null'));
  assert.ok(open.includes('logic.applyRemoteProxyBinding'));
  for (const old of ['controllerSessionOpen', 'controllerSessionState', 'controllerSessionClose']) {
    assert.ok(!bridge.includes(`function ${old}`));
    assert.ok(!desktopFacade.includes(`${old}:`));
  }
});

test('durable A dispatch carries only a local SessionStore identity and intent', () => {
  const send = bridge.slice(
    bridge.indexOf('function submitRemoteTask'),
    bridge.indexOf('function abandonPreparedRemoteTask'),
  );
  assert.ok(send.includes('desktop_remote_task_dispatch_a'));
  assert.ok(send.includes('localSessionId: localSessionId'));
  assert.ok(send.includes('remoteRef: remoteRef'));
  assert.ok(!/input_digest|connection_id|controller_lease_id|credential_ref/.test(send));
});
