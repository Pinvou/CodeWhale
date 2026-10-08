import assert from 'node:assert/strict';
import test from 'node:test';

import { commitPendingUpdateReady } from '../src/platform/tauri/update-readiness.js';

test('browser preview does not invoke the native update readiness command', async () => {
  let calls = 0;
  const result = await commitPendingUpdateReady({
    available: () => false,
    invoke: async () => { calls += 1; },
  });
  assert.equal(result, null);
  assert.equal(calls, 0);
});

test('desktop first commit invokes the exact readiness command once', async () => {
  const commands = [];
  const result = await commitPendingUpdateReady({
    available: () => true,
    invoke: async (command) => {
      commands.push(command);
      return { committed: true, result: 'ready' };
    },
  });
  assert.deepEqual(commands, ['commit_pending_update_ready']);
  assert.equal(result.committed, true);
});

test('readiness persistence failure is diagnostic and never breaks first render', async () => {
  const warnings = [];
  const result = await commitPendingUpdateReady({
    available: () => true,
    invoke: async () => { throw new Error('disk unavailable'); },
    warn: (...args) => warnings.push(args),
  });
  assert.equal(result, null);
  assert.equal(warnings.length, 1);
});
