import assert from 'node:assert/strict';
import test from 'node:test';

import { createCapabilityRequestLoop } from '../src/features/remote-control/capability-request-loop.mjs';

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

async function flushPromises() {
  await Promise.resolve();
  await Promise.resolve();
}

test('capability requests stay single-flight until the snapshot arrives', async () => {
  const pending = deferred();
  const calls = [];
  const timers = [];
  const loop = createCapabilityRequestLoop({
    send(sessionReference) {
      calls.push(sessionReference);
      return pending.promise;
    },
    schedule(callback) {
      timers.push(callback);
      return callback;
    },
    cancel(callback) {
      const index = timers.indexOf(callback);
      if (index >= 0) timers.splice(index, 1);
    },
  });

  const request = { enabled: true, key: 'connection:1:session', sessionRef: 'session' };
  loop.update(request);
  loop.update(request);
  await flushPromises();
  assert.deepEqual(calls, ['session']);

  pending.resolve();
  await flushPromises();
  assert.equal(timers.length, 1, 'bridge enqueue completion schedules one bounded retry');

  loop.stop();
  assert.equal(timers.length, 0, 'snapshot arrival cancels the pending retry');
});

test('a retry is delayed and a new lease invalidates the old generation', async () => {
  const calls = [];
  const timers = [];
  const loop = createCapabilityRequestLoop({
    send(sessionReference) {
      calls.push(sessionReference);
    },
    schedule(callback) {
      timers.push(callback);
      return callback;
    },
    cancel(callback) {
      const index = timers.indexOf(callback);
      if (index >= 0) timers.splice(index, 1);
    },
  });

  loop.update({ enabled: true, key: 'connection:1:session-a', sessionRef: 'session-a' });
  await flushPromises();
  assert.deepEqual(calls, ['session-a']);
  assert.equal(timers.length, 1);

  loop.update({ enabled: true, key: 'connection:2:session-b', sessionRef: 'session-b' });
  await flushPromises();
  assert.deepEqual(calls, ['session-a', 'session-b']);
  assert.equal(timers.length, 1, 'the prior lease retry is cancelled');

  timers.shift()();
  await flushPromises();
  assert.deepEqual(calls, ['session-a', 'session-b', 'session-b']);
});

test('disposing before the queued bridge call prevents a stale request', async () => {
  const calls = [];
  const loop = createCapabilityRequestLoop({
    send(sessionReference) {
      calls.push(sessionReference);
    },
  });

  loop.update({ enabled: true, key: 'connection:1:session', sessionRef: 'session' });
  loop.dispose();
  await flushPromises();
  assert.deepEqual(calls, []);
});
