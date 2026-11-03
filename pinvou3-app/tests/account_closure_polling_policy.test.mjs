import assert from 'node:assert/strict';
import test from 'node:test';
import { createAccountClosureReader } from '../src/features/account-platform/account-closure-reader.js';

const deferred = () => {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
};

test('foreground and background consumers share one read and the next read remains available', async () => {
  const response = deferred(); let calls = 0;
  const client = { accountClosure: () => { calls++; return response.promise; } };
  const reader = createAccountClosureReader(client, () => true);
  const first = reader.read(), joined = reader.read();
  assert.equal(first, joined);
  assert.equal(reader.pending(), true);
  response.resolve(null);
  assert.deepEqual(await joined, { status: 'ready', closure: null });
  assert.equal(calls, 1);
  assert.equal(reader.pending(), false);
  await reader.read();
  assert.equal(calls, 2);
  reader.dispose();
});

test('confirmation or mutation retires an earlier response even if its client ignores abort', async () => {
  const old = deferred(), fresh = deferred(), signals = [];
  const reader = createAccountClosureReader({
    accountClosure(signal) { signals.push(signal); return signals.length === 1 ? old.promise : fresh.promise; },
  }, () => true);
  const earlier = reader.read();
  await Promise.resolve();
  reader.invalidate();
  assert.equal(signals[0].aborted, true);
  const next = reader.read();
  await Promise.resolve();
  const rejected = assert.rejects(earlier, { name: 'AbortError' });
  old.resolve(null);
  await rejected;
  assert.equal(reader.pending(), true, 'the old finally must not release the newer read');
  const closure = { request_id: 'new-request' };
  fresh.resolve(closure);
  assert.deepEqual(await next, { status: 'ready', closure });
  reader.dispose();
});

test('live session changes and disposal reject stale reads without another API call', async () => {
  const response = deferred(); let calls = 0;
  const client = { financeSessionEpoch: 1, accountClosure: () => { calls++; return response.promise; } };
  const reader = createAccountClosureReader(client, () => true), earlier = reader.read();
  await Promise.resolve();
  client.financeSessionEpoch++;
  const rejected = assert.rejects(earlier, { name: 'AbortError' });
  response.resolve(null);
  await rejected;
  await assert.rejects(reader.read(), { name: 'AbortError' });
  assert.equal(calls, 1);
  reader.dispose();
  await assert.rejects(reader.read(), { name: 'AbortError' });
});

test('reads preserve definitive authorization errors for the polling retirement policy', async () => {
  for (const status of [401, 403]) {
    const reason = Object.assign(new Error('not readable'), { status });
    const reader = createAccountClosureReader({ accountClosure: () => Promise.reject(reason) }, () => true);
    assert.deepEqual(await reader.read(), { status: 'error', closure: null, reason });
    reader.dispose();
  }
});
