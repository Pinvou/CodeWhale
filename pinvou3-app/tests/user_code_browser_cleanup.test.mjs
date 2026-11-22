import assert from 'node:assert/strict';
import path from 'node:path';
import os from 'node:os';
import test from 'node:test';
import { cleanupUserCodeLookupFixture, finishUserCodeLookupFixture } from './fixtures/user-code-browser-cleanup.mjs';

const tmpRoot = path.resolve(os.tmpdir());
const profile = path.join(tmpRoot, 'pinvou-code-lookup-controlled');

test('owned fixture closes both resources then removes only its temporary profile', async () => {
  const calls = [];
  const close = name => ({ async close() { calls.push(name); } });
  const errors = await cleanupUserCodeLookupFixture({ browser: close('browser'), vite: close('vite'), profile, tmpRoot,
    remove(target, options) { calls.push(target); assert.deepEqual(options, { recursive: true, force: true }); } });
  assert.deepEqual(calls, ['browser', 'vite', profile]);
  assert.deepEqual(errors, []);
  assert.doesNotThrow(() => finishUserCodeLookupFixture({ failed: false, cleanupErrors: errors }));
});

test('close failures do not prevent later cleanup and all failures remain visible', async () => {
  const failures = [new Error('browser close'), new Error('vite close'), new Error('remove')];
  const errors = await cleanupUserCodeLookupFixture({ profile, tmpRoot,
    browser: { async close() { throw failures[0]; } }, vite: { async close() { throw failures[1]; } },
    remove() { throw failures[2]; } });
  assert.deepEqual(errors, failures);
  assert.throws(() => finishUserCodeLookupFixture({ failed: false, cleanupErrors: errors }), error => error instanceof AggregateError && error.errors.every((value, index) => value === failures[index]));
});

test('unowned profile is refused without deletion while resources still close', async () => {
  let closed = 0;
  const errors = await cleanupUserCodeLookupFixture({ profile: path.join(tmpRoot, 'unowned'), tmpRoot,
    browser: { async close() { closed++; } }, vite: { async close() { closed++; } },
    remove() { assert.fail('unowned path must not be deleted'); } });
  assert.equal(closed, 2);
  assert.equal(errors.length, 1);
  assert.match(errors[0].message, /unowned browser profile/);
});

test('primary UI failure identity survives cleanup failures, which are separately reported', () => {
  const primaryFailure = new Error('original UI assertion');
  const cleanupFailure = new Error('secondary cleanup failure');
  const reports = [];
  assert.throws(() => finishUserCodeLookupFixture({ failed: true, primaryFailure, cleanupErrors: [cleanupFailure], report: error => reports.push(error) }), error => error === primaryFailure);
  assert.equal(reports.length, 1);
  assert.deepEqual(reports[0].errors, [cleanupFailure]);
});

test('primary failure is preserved when cleanup succeeds', () => {
  const primaryFailure = new Error('original');
  assert.throws(() => finishUserCodeLookupFixture({ failed: true, primaryFailure, cleanupErrors: [], report() { assert.fail('no secondary failure'); } }), error => error === primaryFailure);
});
