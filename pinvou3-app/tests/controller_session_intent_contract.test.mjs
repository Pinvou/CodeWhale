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
    leaseEpoch: '2',
  });
}

test('the session projection carries the real remote session id into UI state', () => {
  const state = logic.applyControllerSession(connectedState(), {
    sessionRef: 'session-ref-1',
    sessionId: 'session-abc',
    mode: 'chat',
    connectionId: 'connection-1',
    leaseEpoch: '2',
    capabilityRevision: 4,
    temporaryModelAvailable: true,
  });
  assert.equal(state.controllerConnection.sessionId, 'session-abc');
  assert.equal(state.controllerConnection.sessionRef, 'session-ref-1');
  assert.equal(state.controllerConnection.sessionMode, 'chat');
  assert.equal(state.controllerConnection.capabilityRevision, 4);
  assert.equal(state.controllerConnection.temporaryModelAvailable, true);
});

test('foreign connections, older leases and terminal states never adopt a session', () => {
  const base = connectedState();
  const projection = {
    sessionRef: 'session-ref-1',
    sessionId: 'session-abc',
    connectionId: 'connection-1',
    leaseEpoch: '2',
  };
  const foreign = logic.applyControllerSession(base, {
    ...projection,
    connectionId: 'connection-9',
  });
  assert.equal(foreign, base, 'another connection must not take over the session');

  const olderLease = logic.applyControllerSession(base, { ...projection, leaseEpoch: '1' });
  assert.equal(olderLease, base, 'an older lease must not take over the session');

  const withSession = logic.applyControllerSession(base, projection);
  assert.equal(withSession.controllerConnection.sessionId, 'session-abc');
  const disconnected = logic.applyControllerStatus(withSession, { status: 'disconnected' });
  assert.equal(disconnected.controllerConnection.sessionId, null);
  assert.equal(disconnected.controllerConnection.sessionRef, null);
  assert.equal(disconnected.controllerConnection.capabilityRevision, null);
  assert.equal(disconnected.controllerConnection.temporaryModelAvailable, false);
});

test('the bridge maps the lease-bound temporary-model decision without exposing credentials', () => {
  const mapping = bridge.slice(
    bridge.indexOf('function applyControllerSession'),
    bridge.indexOf('function controllerListHistory'),
  );
  assert.ok(mapping.includes('temporaryModelAvailable: payload.temporaryModelAvailable === true'));
  assert.ok(!/credential|apiKey|privateKey/.test(mapping));
});

test('an absent temporary-model projection fails closed for mixed client versions', () => {
  const state = logic.applyControllerSession(connectedState(), {
    sessionRef: 'session-ref-old-b',
    sessionId: 'session-old-b',
    mode: 'chat',
    connectionId: 'connection-1',
    leaseEpoch: '2',
    capabilityRevision: 1,
  });
  assert.equal(state.controllerConnection.temporaryModelAvailable, false);
});

test('capability snapshots keep the revision and the negotiated modes', () => {
  let state = logic.applyControllerSession(connectedState(), {
    sessionRef: 'session-ref-1',
    sessionId: 'session-abc',
    connectionId: 'connection-1',
    leaseEpoch: '2',
  });
  state = logic.applyCapabilities(state, {
    connectionId: 'connection-1',
    leaseEpoch: '2',
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
    'controllerSessionOpen',
    'controllerSessionState',
    'controllerSessionClose',
    'controllerCapabilityRequest',
    'controllerApplyCapabilitySnapshot',
    'controllerTaskIntent',
    'controllerTaskCancelIntent',
    'controllerTaskSend',
    'controllerTaskCancelSend',
  ]) {
    assert.ok(bridge.includes(`${method}: ${method}`), `${method} must be exported`);
  }
  // 任务意图只把正文交给 Rust；协议字段与摘要一律不在 JS 出现。
  const intent = bridge.slice(
    bridge.indexOf('function controllerTaskIntent'),
    bridge.indexOf('function controllerTaskCancelIntent'),
  );
  assert.ok(intent.includes('text: String(text || "")'));
  assert.ok(!/input_digest|inputDigest|connection_id|controller_lease_id|relay_epoch/.test(intent));
  const uiPaths = [
    bridge.slice(
      bridge.indexOf('function controllerSessionOpen'),
      bridge.indexOf('function controllerSessionState'),
    ),
    intent,
    bridge.slice(
      bridge.indexOf('function controllerTaskCancelIntent'),
      bridge.indexOf('function applyControllerSession'),
    ),
  ].join('\n');
  assert.ok(
    !/input_digest|client_request_id|capability_revision|controller_lease_id|relay_epoch/.test(uiPaths),
    'UI paths must not carry protocol field names',
  );
  // 会话事件驱动 UI 状态（sessionId 由 Rust 投影）。
  assert.ok(bridge.includes('"desktop-remote-controller-session"'));
  assert.ok(bridge.includes('applyControllerSession'));
});

test('controller command results map the nested Rust lease into UI state', () => {
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
    bridge.indexOf('function controllerSessionOpen'),
  );
  assert.ok(mapping.includes('wire.lease'));
  assert.ok(mapping.includes('lease.connectionId'));
  assert.ok(mapping.includes('lease.leaseEpoch'));
});

test('the desktop facade exposes the A-side session and task bridge methods', () => {
  for (const method of [
    'controllerSessionOpen',
    'controllerSessionState',
    'controllerSessionClose',
    'controllerCapabilityRequest',
    'controllerCapabilitySend',
    'controllerApplyCapabilitySnapshot',
    'controllerTaskIntent',
    'controllerTaskCancelIntent',
    'controllerTaskSend',
    'controllerTaskCancelSend',
  ]) {
    assert.ok(
      desktopFacade.includes(`${method}: desktopRemoteControlFeature.${method}`),
      `${method} must be exposed by the public desktop bridge facade`,
    );
  }
});

test('capability snapshots cross the bridge without leaking wire field names', () => {
  // 快照从 wire 进来后先映射成 UI 字段，再进入能力域状态。
  assert.ok(bridge.includes('function capabilitySnapshotForUi'));
  assert.ok(bridge.includes('applyCapabilities(current(), capabilitySnapshotForUi(body))'));
  const mapping = bridge.slice(
    bridge.indexOf('function capabilitySnapshotForUi'),
    bridge.indexOf('function controllerTaskIntent'),
  );
  assert.ok(mapping.includes('wire.capability_revision'));
  assert.ok(mapping.includes('wire.workspaces'));
  assert.ok(mapping.includes('capabilityRevision'));
  assert.ok(!/workspaces: Array.isArray\(wire/.test(bridge.slice(0, bridge.indexOf('function capabilitySnapshotForUi'))));
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

test('reopening a session reuses the connection session reference', () => {
  const open = bridge.slice(
    bridge.indexOf('function controllerSessionOpen'),
    bridge.indexOf('function controllerSessionState'),
  );
  assert.ok(open.includes('controllerConnection.sessionRef'));
  assert.ok(open.includes('remoteRef: existing || null'));
  assert.ok(open.includes('var snapshot = current();'));
  assert.ok(!open.includes('var current = current();'));
});

test('the encrypted send entry points only carry UI fields', () => {
  const send = bridge.slice(
    bridge.indexOf('function controllerTaskSend'),
    bridge.indexOf('function applyControllerSession'),
  );
  assert.ok(send.includes('var visibleText = String(text || "").trim()'));
  assert.ok(send.includes('text: visibleText'));
  assert.ok(send.includes('taskId: taskId'));
  const invokePayload = send.slice(send.indexOf('return invoke('), send.indexOf('}).catch'));
  assert.ok(
    !/input_digest|client_request_id|session_id|controller_lease_id/.test(invokePayload),
    'the send path must not assemble protocol fields',
  );
});
