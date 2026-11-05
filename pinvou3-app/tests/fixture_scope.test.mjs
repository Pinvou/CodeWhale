import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { FixtureScope } from './helpers/fixture-scope.mjs';

test('all resource closes precede directory retirement and repeated cleanup is idempotent', async () => {
  const scope = new FixtureScope();
  const calls = [];
  for (const id of ['one', 'two']) scope.register({ kind: 'probe', id, close: () => calls.push('close-' + id), remove: () => calls.push('remove-' + id) });
  await scope.close();
  await scope.close();
  assert.deepEqual(calls, ['close-two', 'close-one', 'remove-two', 'remove-one']);
  assert.throws(() => scope.register({ kind: 'probe', close() {} }), /closing/);
});

test('one failed close does not prevent other closes, preserves all files and the original error', async () => {
  const scope = new FixtureScope();
  const primary = new Error('controlled close failure');
  const calls = [];
  scope.register({ kind: 'probe', close: () => calls.push('other-close'), remove: () => calls.push('remove') });
  scope.register({ kind: 'probe', close: () => { throw primary; }, remove: () => calls.push('remove') });
  await assert.rejects(scope.close(), error => error instanceof AggregateError && error.errors.includes(primary));
  assert.deepEqual(calls, ['other-close']);
});

test('a close return without its acknowledgement does not authorize deletion', async () => {
  const scope = new FixtureScope();
  let removed = false;
  scope.register({ kind: 'probe', close() {}, verify: () => false, remove: () => { removed = true; } });
  await assert.rejects(scope.close(), /did not become clean/);
  assert.equal(removed, false);
});

test('durable receipts contain the lifecycle state without callbacks or environment data', async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-ledger-test-'));
  try {
    const scope = new FixtureScope({ ledger: directory });
    const release = scope.register({ kind: 'probe', id: 'owned', close() {} });
    assert.equal(JSON.parse(fs.readFileSync(path.join(directory, 'owned.json'))).state, 'registered');
    await release();
    await release();
    const receipt = JSON.parse(fs.readFileSync(path.join(directory, 'owned.json')));
    assert.equal(receipt.state, 'released');
    assert.deepEqual(Object.keys(receipt).sort(), ['id', 'kind', 'pid', 'schema', 'state']);
  } finally { fs.rmSync(directory, { recursive: true }); }
});
