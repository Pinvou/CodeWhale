import assert from 'node:assert/strict';
import test from 'node:test';
import { createTaskEventBuffer } from '../src/features/megacube/task-event-buffer.js';

test('events retain original session ordering across delayed controller load and remount', () => {
  const queue = createTaskEventBuffer();
  queue.publish('toolStarted', { id: 'call' }, { activeSessionId: 'original' });
  queue.publish('manualSelection', { sessionId: 'original' }, { activeSessionId: 'new' });
  const seen = [];
  const stop = queue.subscribe((...args) => seen.push(args));
  assert.deepEqual(seen.map(args => [args[0], args[2].activeSessionId]), [['toolStarted', 'original'], ['manualSelection', 'new']]);
  queue.publish('toolEnded', { id: 'call' }, { activeSessionId: 'new' });
  assert.equal(seen.length, 3);
  stop();
  queue.publish('runtimeTask', { id: 'accepted' }, { activeSessionId: 'new' });
  const recovered = [];
  queue.subscribe((...args) => recovered.push(args));
  assert.equal(recovered.length, 1);
  assert.equal(recovered[0][1].id, 'accepted');
});

test('pending events stay bounded if the controller chunk cannot load', () => {
  const queue = createTaskEventBuffer(2);
  for (let index = 0; index < 10; index++) queue.publish('event', index, {});
  const seen = [];
  queue.subscribe((method, value) => seen.push(value));
  assert.deepEqual(seen, [8, 9]);
});
