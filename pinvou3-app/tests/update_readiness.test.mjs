import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

import { commitPendingUpdateReady } from '../src/platform/tauri/update-readiness.js';

const mainSource = readFileSync(new URL('../src/app/main.jsx', import.meta.url), 'utf8');

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

test('main window commits update readiness from the first React layout commit', () => {
  const firstCommit = mainSource.indexOf("mark('react:first_commit')");
  const revealWindow = mainSource.indexOf('void revealStartupWindow()', firstCommit);
  const commitReady = mainSource.indexOf('void commitPendingUpdateReady()', firstCommit);
  const firstEffectEnd = mainSource.indexOf('}, []);', firstCommit);

  assert.notEqual(firstCommit, -1, 'first React commit marker must exist');
  assert.ok(revealWindow > firstCommit, 'window reveal must be queued after the first commit');
  assert.ok(commitReady > revealWindow, 'readiness commit must not delay the window reveal');
  assert.ok(commitReady < firstEffectEnd, 'readiness commit must stay in the first layout effect');
});
