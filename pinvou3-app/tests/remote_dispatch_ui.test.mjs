import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import {
  forgetRemoteDispatch, markRemoteDispatchPayloadLost, recordRemoteDispatch, remoteDispatchCases,
} from '../src/features/remote-control/remote-dispatch-ui.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = relative => fs.readFileSync(path.join(root, 'src', relative), 'utf8');
const handle = {
  taskId: 'task-1', clientRequestId: 'client-1', requestFingerprint: 'a'.repeat(64),
};

test('only queued is accepted; prepared keeps original text and exact retry handle per local session', () => {
  const prepared = recordRemoteDispatch('remote-a', 'ref-a', 'lease-a', 'original', {
    state: 'prepared_not_sent', retryHandle: handle,
  });
  assert.equal(prepared.state, 'prepared_not_sent');
  assert.deepEqual(remoteDispatchCases('remote-b'), []);
  assert.deepEqual(remoteDispatchCases('remote-a')[0].handle, handle);
  assert.equal(remoteDispatchCases('remote-a')[0].text, 'original');
  const queued = recordRemoteDispatch('remote-a', 'ref-a', 'lease-a', 'original', {
    state: 'queued', retryHandle: handle,
  });
  assert.equal(queued.state, 'queued');
  assert.deepEqual(remoteDispatchCases('remote-a'), []);
});

test('outcome unknown and malformed reports never become a new-send draft', () => {
  const unknown = recordRemoteDispatch('remote-unknown', 'ref-u', 'lease-u', 'do not replay', {
    state: 'outcome_unknown', retryHandle: handle,
  });
  assert.equal(unknown.state, 'outcome_unknown');
  assert.equal(remoteDispatchCases('remote-unknown')[0].text, null);
  const invalid = recordRemoteDispatch('remote-malformed', 'ref-m', 'lease-m', 'possibly sent', {
    state: 'queued', retryHandle: null,
  });
  assert.equal(invalid.state, 'outcome_unknown');
  assert.equal(remoteDispatchCases('remote-malformed')[0].handle, null);
  assert.equal(remoteDispatchCases('remote-malformed')[0].text, null);
});

test('failed abandon drops the retry payload but retains the durable handle for another abandon', () => {
  const id = 'remote-abandon-failure';
  recordRemoteDispatch(id, 'ref', 'lease', 'sensitive body', {
    state: 'prepared_not_sent', retryHandle: handle,
  });
  assert.equal(markRemoteDispatchPayloadLost(id, handle.taskId), true);
  const history = { localSessionId: id, entries: [], pendingUnknownCount: 0,
    unattemptedPending: [{ ...handle, dispatchState: 'prepared_never_attempted' }] };
  const [item] = remoteDispatchCases(id, history);
  assert.equal(item.origin, 'history');
  assert.equal(item.text, null);
  assert.deepEqual(item.handle, handle);
  assert.equal(forgetRemoteDispatch(id, handle.taskId), true);
  assert.deepEqual(remoteDispatchCases(id, history), []);
});

test('restart history exposes only unattempted abandon hints and aggregate unknown count', () => {
  const history = {
    localSessionId: 'remote-restart', pendingUnknownCount: 2,
    unattemptedPending: [{ ...handle, dispatchState: 'prepared_never_attempted' }],
  };
  const cases = remoteDispatchCases('remote-restart', history);
  assert.deepEqual(cases.map(item => [item.state, item.origin]), [
    ['prepared_not_sent', 'history'], ['outcome_unknown', 'history'],
  ]);
  assert.equal(cases[0].text, null, 'no plaintext is fabricated after restart');
  assert.equal(cases[1].count, 2);
  forgetRemoteDispatch('remote-restart', handle.taskId);
  assert.equal(remoteDispatchCases('remote-restart', history).some(item => item.taskId === handle.taskId), false,
    'an old read cannot resurrect an abandoned task');
  assert.equal(remoteDispatchCases('remote-restart', { ...history, unattemptedPending: [] }).length, 1);
});

test('only an authenticated exact-task terminal clears process cases; count changes do not guess', () => {
  const other = { taskId: 'task-2', clientRequestId: 'client-2', requestFingerprint: 'b'.repeat(64) };
  const stale = { localSessionId: 'remote-terminal', entries: [], pendingUnknownCount: 0, unattemptedPending: [] };
  recordRemoteDispatch('remote-terminal', 'ref-t', 'lease-t', 'first', {
    state: 'outcome_unknown', retryHandle: handle,
  }, stale);
  recordRemoteDispatch('remote-terminal', 'ref-t', 'lease-t', 'second', {
    state: 'outcome_unknown', retryHandle: other,
  }, stale);
  assert.equal(remoteDispatchCases('remote-terminal', stale).length, 2,
    'a pre-dispatch count=0 cannot clear a just-created report');
  const history = { ...stale, pendingUnknownCount: 1 };
  assert.deepEqual(remoteDispatchCases('remote-terminal', history).map(item => item.taskId), ['history-unknown'],
    'a lower aggregate count cannot identify which task became terminal');
  assert.equal(remoteDispatchCases('remote-terminal', { ...history, entries: [
    { kind: 'status', taskId: 'other-task', body: 'failed' },
  ] }).length, 1);
  const third = { taskId: 'task-3', clientRequestId: 'client-3', requestFingerprint: 'c'.repeat(64) };
  recordRemoteDispatch('remote-exact', 'ref-t', 'lease-t', 'third', {
    state: 'outcome_unknown', retryHandle: third,
  }, stale);
  assert.equal(remoteDispatchCases('remote-exact', {
    localSessionId: 'remote-exact', entries: [{ kind: 'status', taskId: 'task-3', body: 'running' }],
    pendingUnknownCount: 1, unattemptedPending: [],
  }).length, 1);
  assert.deepEqual(remoteDispatchCases('remote-exact', {
    localSessionId: 'remote-exact', entries: [{ kind: 'status', taskId: 'task-3', body: 'succeeded' }],
    pendingUnknownCount: 0, unattemptedPending: [],
  }), []);
});

test('a terminal outside the bounded history window is safely retired by authoritative zero count', () => {
  const stale = { localSessionId: 'remote-window', entries: [], pendingUnknownCount: 0, unattemptedPending: [] };
  recordRemoteDispatch('remote-window', 'ref-w', 'lease-w', 'text', {
    state: 'outcome_unknown', retryHandle: handle,
  }, stale);
  assert.equal(remoteDispatchCases('remote-window', stale).length, 1);
  assert.deepEqual(remoteDispatchCases('remote-window', { ...stale, entries: Array.from({ length: 512 }, (_, sequence) => ({
    kind: 'assistant_message', taskId: 'other', body: 'later', sequence,
  })) }), []);
});

test('prepared reports reconcile against exact durable handle, not a pending count', () => {
  const stale = { localSessionId: 'remote-prepared', entries: [], pendingUnknownCount: 0, unattemptedPending: [] };
  recordRemoteDispatch('remote-prepared', 'ref-p', 'lease-p', 'text', {
    state: 'prepared_not_sent', retryHandle: handle,
  }, stale);
  assert.equal(remoteDispatchCases('remote-prepared', stale).length, 1);
  const fresh = { ...stale, unattemptedPending: [{ ...handle, dispatchState: 'prepared_never_attempted' }] };
  assert.equal(remoteDispatchCases('remote-prepared', fresh).length, 1);
  assert.deepEqual(remoteDispatchCases('remote-prepared', {
    ...stale, unattemptedPending: [{ ...handle, clientRequestId: 'other', dispatchState: 'prepared_never_attempted' }],
  }).map(item => item.origin), ['history']);
});

test('ChatView uses handle-only retry, explicit abandon and never restores unknown as fresh text', () => {
  const chat = read('features/chat/ChatView.jsx');
  assert.match(chat, /outcome\.state !== 'queued'\) return 'held'/);
  assert.match(chat, /\{ kind: 'retry', handle: item\.handle \}/);
  assert.match(chat, /sameHandle \? report : \{ state: 'outcome_unknown'/);
  assert.match(chat, /abandonPreparedRemoteTask\([\s\S]*?item\.handle\.clientRequestId, item\.handle\.taskId/);
  assert.match(chat, /await bridge\.desktopRemoteControl\.abandonPreparedRemoteTask\([\s\S]*?forgetRemoteDispatch/);
  assert.match(chat, /markRemoteDispatchPayloadLost\(item\.localSessionId, item\.taskId\);[\s\S]*?readRemoteProxyHistory/);
  assert.match(chat, /if \(item\.text\) \{[\s\S]*?restoreTaskDraft\(item\.text, \{ sessionId: item\.localSessionId/);
  assert.match(chat, /else if \(remoteTranscriptActive && sentSessionId\) \{[\s\S]*?restoreTaskDraft\(text, \{ sessionId: sentSessionId/);
  assert.match(chat, /item\.origin === 'process' && item\.identity === remoteSendIdentity && remoteDispatchReady/);
  assert.ok(chat.includes("!pendingRemoteDispatches.some(item => item.state === 'prepared_not_sent')"));
  assert.ok(!chat.includes("{ kind: 'new', text: item.text"));
  for (const locale of ['zh', 'en', 'ja']) {
    const copy = read(`shared/i18n/${locale}.js`);
    for (const key of ['preparedNotSent', 'preparedPayloadLost', 'outcomeUnknownNoResend',
      'retrySameTask', 'abandonPreparedTask', 'pendingActionFailed', 'deletePreparedBlocked']) {
      assert.ok(copy.includes(`${key}:`), `${locale} ${key}`);
    }
  }
});
