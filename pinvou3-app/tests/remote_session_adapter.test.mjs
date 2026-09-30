import assert from 'node:assert/strict';
import test from 'node:test';
import {
  activeRemoteTaskId,
  liveRemoteProxyBinding,
  mergeSessionStoreHistory,
  newRemoteTaskIntent,
  remoteProxyHandshakeBinding,
  remoteProxyLeaseIdentity,
  remoteProxyPresentationState,
  remoteProxyChatItems,
  remoteProxyForSession,
  sameRemoteComposerOwner,
  selectedExportableRemoteModelId,
} from '../src/features/remote-control/remote-session-adapter.mjs';

test('result_unknown is terminal and never exposes a stale cancel target', () => {
  const terminal = {
    localSessionId: 'remote-local-1',
    entries: [{
      journalEpoch: 'e1', sequence: 1, eventId: 'terminal-1', kind: 'status',
      taskId: 'task-1', body: 'result_unknown', attachments: [],
    }],
  };
  assert.equal(activeRemoteTaskId('remote-local-1', terminal), null);
});

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
  assert.deepEqual(items.map(item => item.type), ['user', 'assistant', 'system']);
  assert.deepEqual(items.map(item => item.text), ['hello', 'answer', 'waiting']);
  assert.equal(items.every(item => item.remote && item.remoteProxy), true);
  assert.equal(items[1].streaming, true, 'without a terminal event, the replayed task is still in progress');
  assert.equal(items[0].streaming || items[2].streaming, false);
  assert.deepEqual(remoteProxyChatItems('local-1', history), []);
});

test('streamed assistant deltas coalesce per task without crossing reasoning, tool or error boundaries', () => {
  const entries = [
    { journalEpoch: 'e1', sequence: 1, eventId: 'a', kind: 'assistant_message', taskId: 'task-1', body: 'Hel', attachments: [] },
    { journalEpoch: 'e1', sequence: 2, eventId: 'b', kind: 'assistant_message', taskId: 'task-1', body: 'lo', attachments: [] },
    { journalEpoch: 'e1', sequence: 3, eventId: 'c', kind: 'reasoning', taskId: 'task-1', body: 'think ', attachments: [] },
    { journalEpoch: 'e1', sequence: 4, eventId: 'd', kind: 'reasoning', taskId: 'task-1', body: 'more', attachments: [] },
    { journalEpoch: 'e1', sequence: 5, eventId: 'e', kind: 'assistant_message', taskId: 'task-1', body: ' after reasoning', attachments: [] },
    { journalEpoch: 'e1', sequence: 6, eventId: 'f', kind: 'tool', taskId: 'task-1', body: 'tool finished', attachments: [] },
    { journalEpoch: 'e1', sequence: 7, eventId: 'g', kind: 'assistant_message', taskId: 'task-1', body: ' after tool', attachments: [] },
    { journalEpoch: 'e1', sequence: 8, eventId: 'h', kind: 'error', taskId: 'task-1', body: 'recoverable error', attachments: [] },
    { journalEpoch: 'e1', sequence: 9, eventId: 'i', kind: 'assistant_message', taskId: 'task-1', body: ' after error', attachments: [] },
  ];
  const running = remoteProxyChatItems('remote-local-1', { localSessionId: 'remote-local-1', entries });
  assert.deepEqual(running.map(item => [item.type, item.text]), [
    ['assistant', 'Hello'],
    ['reasoning', 'think more'],
    ['assistant', ' after reasoning'],
    ['system', 'tool finished'],
    ['assistant', ' after tool'],
    ['system', 'recoverable error'],
    ['assistant', ' after error'],
  ]);
  assert.equal(running.filter(item => ['assistant', 'reasoning'].includes(item.type))
    .every(item => item.streaming), true, 'the latest non-terminal state keeps the current turn streaming');

  const completed = remoteProxyChatItems('remote-local-1', {
    localSessionId: 'remote-local-1',
    entries: [...entries, {
      journalEpoch: 'e1', sequence: 10, eventId: 'j', kind: 'status',
      taskId: 'task-1', body: 'succeeded', attachments: [],
    }],
  });
  assert.equal(completed.filter(item => ['assistant', 'reasoning'].includes(item.type))
    .every(item => !item.streaming), true, 'a terminal replay renders every response segment as complete');
});

test('only frozen projection kinds render; permission/tool/error never spoof an assistant reply', () => {
  const entries = ['tool', 'approval', 'status', 'error', 'attachment', 'artifact', 'snapshot_marker', 'unrecognized']
    .map((kind, index) => ({ journalEpoch: 'e1', sequence: index + 1, eventId: `event-${index}`, kind, body: kind, attachments: [] }));
  const items = remoteProxyChatItems('remote-local-1', { localSessionId: 'remote-local-1', entries });
  assert.equal(items.length, 7);
  assert.equal(items.every(item => item.type === 'system' && item.remoteProxy), true);
  assert.ok(items.every(item => item.remoteKind !== 'unrecognized'));
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
  assert.equal(remoteProxyPresentationState('remote-local-1', history, live), 'ready');
  assert.equal(remoteProxyPresentationState('remote-local-1', history, { ...live, capabilityRevision: null }), 'preparing');
  assert.equal(remoteProxyPresentationState('remote-local-1', history, { ...live, capabilityStatus: 'error' }), 'capability_error');
  assert.equal(remoteProxyPresentationState('remote-local-1', history, { ...live, status: 'reconnecting' }), 'reconnecting');
  assert.equal(remoteProxyPresentationState('remote-local-1', history, { ...live, localSessionId: 'remote-other' }), 'offline');
  assert.equal(remoteProxyPresentationState('remote-local-1', history, { ...live, status: 'disconnected' }), 'offline');
  assert.equal(remoteProxyPresentationState('remote-other', history, live), 'absent');
});

test('B model intent is ID-free while A temporary intent requires selected allowlisted opaque ID', () => {
  const listing = { key: 'remote-local-1:lease-1', models: [
    { modelId: 'exportable-1', displayName: 'Model', exportable: true },
    { modelId: 'private-2', exportable: false },
  ] };
  const selected = { key: listing.key, modelId: 'exportable-1' };
  assert.equal(selectedExportableRemoteModelId(listing.key, listing, selected), 'exportable-1');
  assert.deepEqual(newRemoteTaskIntent('hello', 'controlled_device', null), {
    kind: 'new', text: 'hello', mode: 'chat', workspaceId: null, skillIds: [],
    modelSource: 'controlled_device', selectedModelId: null,
  });
  assert.deepEqual(newRemoteTaskIntent('hello', 'controller_temporary', 'exportable-1'), {
    kind: 'new', text: 'hello', mode: 'chat', workspaceId: null, skillIds: [],
    modelSource: 'controller_temporary', selectedModelId: 'exportable-1',
  });
  assert.deepEqual(newRemoteTaskIntent(
    'fix it', 'controlled_device', null, 'code', 'workspace-1', ['skill-1', 'skill-1'],
  ), {
    kind: 'new', text: 'fix it', mode: 'code', workspaceId: 'workspace-1',
    skillIds: ['skill-1'], modelSource: 'controlled_device', selectedModelId: null,
  });
  assert.equal(newRemoteTaskIntent('fix it', 'controlled_device', null, 'code', null), null);
  assert.equal(newRemoteTaskIntent('fix it', 'controller_temporary', 'exportable-1', 'code', 'workspace-1'), null);
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
