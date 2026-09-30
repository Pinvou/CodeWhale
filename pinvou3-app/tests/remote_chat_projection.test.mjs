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
