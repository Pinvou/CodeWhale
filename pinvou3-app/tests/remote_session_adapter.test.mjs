import assert from 'node:assert/strict';
import test from 'node:test';
import {
  liveRemoteProxyBinding,
  mergeSessionStoreHistory,
  newRemoteTaskIntent,
  remoteProxyHandshakeBinding,
  remoteProxyLeaseIdentity,
  remoteProxyChatItems,
  remoteProxyForSession,
  sameRemoteComposerOwner,
  selectedExportableRemoteModelId,
} from '../src/features/remote-control/remote-session-adapter.mjs';

const history = {
  localSessionId: 'remote-local-1',
  entries: [
    { journalEpoch: 'e1', sequence: 1, eventId: 'a', kind: 'user_message', taskId: 'task-1', body: 'hello', attachments: [] },
    { journalEpoch: 'e1', sequence: 2, eventId: 'b', kind: 'assistant_message', taskId: 'task-1', body: 'answer', attachments: [] },
    { journalEpoch: 'e1', sequence: 3, eventId: 'c', kind: 'approval', taskId: 'task-1', body: 'waiting', attachments: [] },
  ],
};

test('RemoteProxy history requires an exact verified read, not an ID prefix or stale selection', () => {
  assert.equal(remoteProxyForSession('remote-local-1', history), history);
  assert.equal(remoteProxyForSession('remote-other', history), null);
  assert.equal(remoteProxyForSession('remote-local-1', { localSessionId: 'remote-local-1' }), null);
  assert.deepEqual(remoteProxyChatItems('remote-other', history), []);
});

test('one SessionStore sidebar view preserves kind and never duplicates a colliding ID', () => {
  const result = mergeSessionStoreHistory(
    [{ id: 'local-1', title: 'Local' }, { id: 'remote-local-1', title: 'collision' }],
    [{ localSessionId: 'remote-local-1', title: 'Remote' }, { localSessionId: 'remote-local-2', title: 'Remote 2', pinned: true }],
  );
  assert.deepEqual(result.map(item => [item.id, item.kind]), [
    ['local-1', 'Local'], ['remote-local-1', 'Local'], ['remote-local-2', 'RemoteProxy'],
  ]);
  assert.equal(result[2].pinned, true);
});

test('only current RemoteProxy projection becomes ChatView items; no legacy transcript fallback', () => {
  const items = remoteProxyChatItems('remote-local-1', history);
  assert.deepEqual(items.map(item => item.type), ['user', 'assistant', 'assistant']);
  assert.deepEqual(items.map(item => item.text), ['hello', 'answer', 'waiting']);
  assert.equal(items.every(item => item.remote && !item.streaming), true);
  assert.deepEqual(remoteProxyChatItems('local-1', history), []);
});

test('live dispatch needs the same local ID and remote ref; offline projection remains readable', () => {
  const live = {
    status: 'connected', localSessionId: 'remote-local-1', sessionRef: 'ref-1', sessionMode: 'chat',
    connectionId: 'connection-1', controllerLeaseId: 'lease-1', leaseEpoch: '2',
    relayEpoch: 'relay-1', generation: 'generation-1', capabilityRevision: 2,
    mappingConfirmed: true, capabilityStatus: 'ready',
  };
  assert.deepEqual(liveRemoteProxyBinding('remote-local-1', history, live), {
    localSessionId: 'remote-local-1', remoteRef: 'ref-1',
  });
  assert.equal(liveRemoteProxyBinding('remote-local-1', history, { ...live, localSessionId: 'remote-old' }), null);
  assert.equal(liveRemoteProxyBinding('remote-local-1', history, { ...live, status: 'disconnected' }), null);
  assert.equal(liveRemoteProxyBinding('remote-local-1', history, { ...live, mappingConfirmed: false }), null);
  assert.equal(liveRemoteProxyBinding('remote-local-1', history, { ...live, capabilityStatus: 'error' }), null);
  assert.deepEqual(remoteProxyHandshakeBinding('remote-local-1', history, { ...live, mappingConfirmed: false, capabilityRevision: null }), {
    localSessionId: 'remote-local-1', remoteRef: 'ref-1',
  });
  assert.equal(remoteProxyChatItems('remote-local-1', history).length, 3);
});

test('B model intent is ID-free while A temporary intent requires selected allowlisted opaque ID', () => {
  const listing = { key: 'remote-local-1:lease-1', models: [
    { modelId: 'exportable-1', displayName: 'Model', exportable: true },
    { modelId: 'private-2', exportable: false },
  ] };
  const selected = { key: listing.key, modelId: 'exportable-1' };
  assert.equal(selectedExportableRemoteModelId(listing.key, listing, selected), 'exportable-1');
  assert.deepEqual(newRemoteTaskIntent('hello', 'controlled_device', null), {
    kind: 'new', text: 'hello', modelSource: 'controlled_device', selectedModelId: null,
  });
  assert.deepEqual(newRemoteTaskIntent('hello', 'controller_temporary', 'exportable-1'), {
    kind: 'new', text: 'hello', modelSource: 'controller_temporary', selectedModelId: 'exportable-1',
  });
  assert.equal(newRemoteTaskIntent('hello', 'controller_temporary', null), null);
  assert.equal(selectedExportableRemoteModelId(listing.key, listing, { key: listing.key, modelId: 'private-2' }), null);
  assert.equal(selectedExportableRemoteModelId('remote-local-2:lease-1', listing, selected), null,
    'switching local RemoteProxy cannot reuse the previous model selection');
  assert.equal(selectedExportableRemoteModelId(listing.key, listing, { key: 'remote-local-2:lease-1', modelId: 'exportable-1' }), null);
});

test('late send failure cannot restore text or error across session, lease or capability revision', () => {
  const connection = {
    sessionRef: 'ref-1', connectionId: 'connection-1', controllerLeaseId: 'lease-1',
    leaseEpoch: '2', relayEpoch: 'relay-1', generation: 'generation-1', capabilityRevision: 3,
  };
  const sent = remoteProxyLeaseIdentity('remote-local-1', connection);
  assert.equal(sameRemoteComposerOwner('remote-local-1', sent, 'remote-local-1', sent), true);
  assert.equal(sameRemoteComposerOwner('remote-local-1', sent, 'remote-local-2', sent), false);
  for (const changed of [
    { controllerLeaseId: 'lease-2' }, { relayEpoch: 'relay-2' },
    { generation: 'generation-2' }, { capabilityRevision: 4 },
  ]) {
    const current = remoteProxyLeaseIdentity('remote-local-1', { ...connection, ...changed });
    assert.equal(sameRemoteComposerOwner('remote-local-1', sent, 'remote-local-1', current), false);
  }
});
