import assert from 'node:assert/strict';
import test from 'node:test';
import { isRemoteChatActive, remoteChatItems } from '../src/features/remote-control/chat-transcript-projection.mjs';

const connection = { status: 'connected', sessionId: 'session-1' };
const transcript = [
  { sessionId: 'session-1', epoch: 'e1', sequence: 1, role: 'user', text: 'hello' },
  { sessionId: 'session-1', epoch: 'e1', sequence: 2, role: 'assistant', text: 'a' },
  { sessionId: 'session-1', epoch: 'e1', sequence: 3, role: 'assistant', text: 'b' },
  { sessionId: 'session-old', epoch: 'e0', sequence: 4, role: 'assistant', text: 'ignored' },
];

test('only the live remote session projects into ChatView items', () => {
  assert.equal(isRemoteChatActive(connection), true);
  assert.equal(isRemoteChatActive({ status: 'idle' }), false);
  assert.deepEqual(remoteChatItems({ status: 'idle' }, transcript), []);

  const items = remoteChatItems(connection, transcript);
  assert.deepEqual(items.map(item => item.type), ['user', 'assistant']);
  assert.equal(items[0].text, 'hello');
  assert.equal(items[1].text, 'ab');
  assert.equal(items[1].remote, true);
  assert.equal(items[1].streaming, false);
});

test('wire journal entries are matched by the A-generated session id, not the local history ref', () => {
  const items = remoteChatItems(
    { status: 'connected', sessionRef: 'history-ref-1', sessionId: 'wire-session-1' },
    [{ sessionId: 'wire-session-1', epoch: 'e1', sequence: 1, role: 'user', text: 'sent' }],
  );
  assert.equal(items.length, 1);
  assert.equal(items[0].text, 'sent');
});

test('a queued task supplies its prompt until the authoritative user journal arrives', () => {
  const tasks = [{
    taskId: 'task-1',
    sessionId: 'session-1',
    status: 'admitted',
    text: 'visible immediately',
  }];
  const fallback = remoteChatItems(connection, [], tasks);
  assert.equal(fallback.length, 1);
  assert.equal(fallback[0].id, 'remote-task-task-1');
  assert.equal(fallback[0].text, 'visible immediately');

  const authoritative = remoteChatItems(connection, [{
    sessionId: 'session-1',
    epoch: 'e1',
    sequence: 1,
    taskId: 'task-1',
    role: 'user',
    text: 'visible immediately',
  }], tasks);
  assert.equal(authoritative.length, 1, 'the journal must replace, not duplicate, the fallback');
  assert.equal(authoritative[0].id, 'remote-session-1:e1:1');
});

test('controlled-side projection accepts its local session task without an A-side session filter', () => {
  const items = remoteChatItems(
    { status: 'connected' },
    [],
    [{ taskId: 'task-b', sessionId: 'local-b-session', status: 'running', text: 'from controller' }],
  );
  assert.equal(items.length, 1);
  assert.equal(items[0].text, 'from controller');
});
