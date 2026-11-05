import assert from 'node:assert/strict';
import test from 'node:test';
import { prepareRemoteTaskCapabilities } from '../src/features/remote-control/remote-task-preflight.mjs';
import { remoteProxyComposerIdentity } from '../src/features/remote-control/remote-session-adapter.mjs';

function fixture(remainingMs = -1) {
  let now = 60_000;
  let current = true;
  const binding = { localSessionId: 'local-1', remoteRef: 'ref-1' };
  const state = {
    controllerConnection: {
      localSessionId: 'local-1', sessionRef: 'ref-1', status: 'connected',
      connectionId: 'conn-1', controllerLeaseId: 'lease-1', leaseEpoch: '1',
      relayEpoch: 'relay-1', generation: 'gen-1', capabilityRevision: 1,
      capabilityStatus: 'ready', mappingConfirmed: true,
    },
  };
  const project = (revision, remaining) => {
    const connection = state.controllerConnection;
    connection.capabilityRevision = revision;
    state.remoteCapabilities = {
      ...connection, remoteSessionRef: connection.sessionRef,
      remoteControlGeneration: connection.generation,
      expiresAtMs: now + remaining,
    };
  };
  project(1, remainingMs);
  const calls = [];
  const ports = {
    binding, ownerIdentity: remoteProxyComposerIdentity(binding.localSessionId, state.controllerConnection),
    now: () => now, readState: () => state, isCurrent: () => current,
    snapshot: async () => { calls.push('read'); },
    renew: async () => { calls.push('renew'); project(2, 30_000); },
  };
  return { ports, calls, state, project, retire: () => { current = false; },
    advance: ms => { now += ms; } };
}

test('idle expiration renews before exposing a new dispatch context', async () => {
  const f = fixture();
  const renewed = Promise.withResolvers();
  f.ports.renew = async () => {
    f.calls.push('renew');
    await renewed.promise;
    f.project(2, 30_000);
  };
  let settled = false;
  const pending = prepareRemoteTaskCapabilities(f.ports).then(context => {
    settled = true;
    return context;
  });
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(f.calls, ['read', 'renew']);
  assert.equal(settled, false, 'a queued renewal must not admit a task');
  renewed.resolve();
  assert.equal((await pending).capabilityRevision, 2);
  assert.deepEqual(f.calls, ['read', 'renew', 'read']);
});

test('an authoritative expired-cache error triggers one renewal', async () => {
  const f = fixture(30_000);
  let reads = 0;
  f.ports.snapshot = async () => {
    if (++reads === 1) throw Object.assign(new Error(), { code: 'capability_snapshot_expired' });
  };
  assert.equal((await prepareRemoteTaskCapabilities(f.ports)).capabilityRevision, 2);
  assert.equal(reads, 2);
  assert.deepEqual(f.calls, ['renew']);
});

test('fresh contexts avoid renewal while the five-second boundary renews', async () => {
  for (const remaining of [5_001, 5_000, 0]) {
    const f = fixture(remaining);
    await prepareRemoteTaskCapabilities(f.ports);
    assert.equal(f.calls.includes('renew'), remaining <= 5_000);
  }
});

test('a new revision requires its matching projection before admission', async () => {
  const f = fixture(30_000);
  f.state.controllerConnection.capabilityRevision = 2;
  f.ports.renew = async () => { f.calls.push('renew'); };
  await assert.rejects(prepareRemoteTaskCapabilities(f.ports), { code: 'capability_snapshot_stale' });
  assert.deepEqual(f.calls, ['read', 'renew', 'read']);
});

test('a failed renewal or still-expired reply does not expose dispatch context', async () => {
  const f = fixture();
  f.ports.renew = async () => { throw Object.assign(new Error(), { code: 'capability_snapshot_timeout' }); };
  await assert.rejects(prepareRemoteTaskCapabilities(f.ports), { code: 'capability_snapshot_timeout' });
  f.ports.renew = async () => { f.advance(30_000); };
  await assert.rejects(prepareRemoteTaskCapabilities(f.ports), { code: 'capability_snapshot_stale' });
});

test('unrelated native failures are not retried as expired snapshots', async () => {
  const f = fixture();
  f.ports.snapshot = async () => { throw Object.assign(new Error(), { code: 'controller_not_connected' }); };
  await assert.rejects(prepareRemoteTaskCapabilities(f.ports), { code: 'controller_not_connected' });
  assert.deepEqual(f.calls, []);
});

test('session, lease, generation and retired preparation cannot cross renewal', async () => {
  const changes = [
    f => { f.state.controllerConnection.localSessionId = 'local-2'; },
    f => { f.state.controllerConnection.sessionRef = 'ref-2'; },
    f => { f.state.controllerConnection.controllerLeaseId = 'lease-2'; },
    f => { f.state.controllerConnection.leaseEpoch = '2'; },
    f => { f.state.controllerConnection.generation = 'gen-2'; },
    f => { f.state.controllerConnection.relayEpoch = 'relay-2'; },
    f => { f.state.controllerConnection.status = 'disconnected'; },
    f => { f.retire(); },
  ];
  for (const change of changes) {
    const f = fixture();
    f.ports.renew = async () => { change(f); f.project(2, 30_000); };
    await assert.rejects(prepareRemoteTaskCapabilities(f.ports), { code: 'remote_binding_mismatch' });
    assert.deepEqual(f.calls, ['read'], 'no second read after ownership retirement');
  }
});

test('a host lease change not yet rendered by React cannot start a preparation', async () => {
  const f = fixture(30_000);
  f.state.controllerConnection.leaseEpoch = '2';
  await assert.rejects(prepareRemoteTaskCapabilities(f.ports), { code: 'remote_binding_mismatch' });
  assert.deepEqual(f.calls, []);
});
