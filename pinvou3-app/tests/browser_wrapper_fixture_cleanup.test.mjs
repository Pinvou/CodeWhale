import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { readPendingHostRequest, cleanupBrowserFixture, confirmedChildStopper, confirmedProcessExited } from './helpers/browser-wrapper-fixture-cleanup.mjs';

test('only process absence proves exit; denied or unknown probes preserve files', async () => {
  assert.equal(confirmedProcessExited(123, () => {}), false);
  assert.equal(confirmedProcessExited(123, () => { throw Object.assign(new Error('absent'), { code: 'ESRCH' }); }), true);
  for (const code of ['EPERM', 'EIO']) {
    let removed = false;
    await assert.rejects(cleanupBrowserFixture({
      closeHost: () => {},
      stopChildren: [() => confirmedProcessExited(123, () => { throw Object.assign(new Error('unconfirmed'), { code }); })],
      closeServer: () => {},
      removeFiles: () => { removed = true; },
    }), AggregateError);
    assert.equal(removed, false);
  }
});

test('owned child close is observed before stop and remembered for repeated cleanup', async () => {
  const child = new EventEmitter();
  let stops = 0;
  const stop = confirmedChildStopper(child, async (actual, grace) => {
    assert.equal(actual, child);
    assert.equal(grace, 5_000);
    stops += 1;
    child.emit('close');
  });
  await stop();
  await stop();
  assert.equal(stops, 1);
});

test('a stop callback returning without close cannot authorize deleting fixture files', async () => {
  const child = new EventEmitter();
  let removed = false;
  await assert.rejects(cleanupBrowserFixture({
    closeHost: () => {},
    stopChildren: [confirmedChildStopper(child, async () => {}, { closeMs: 10 })],
    closeServer: () => {},
    removeFiles: () => { removed = true; },
  }), error => error instanceof AggregateError && /close was not confirmed/.test(error.errors[0].message));
  assert.equal(removed, false);
  child.emit('close');
});

test('a child already closed since spawn needs no additional termination', async () => {
  const child = new EventEmitter();
  const stop = confirmedChildStopper(child, () => { throw new Error('must not stop an exited child'); });
  child.emit('close');
  await stop();
});

test('cancel rename sharing failures never acknowledge a request; later complete read remains available', () => {
  for (const code of ['ENOENT', 'EPERM', 'EBUSY']) {
    const error = Object.assign(new Error('transient request rename'), { code });
    assert.equal(readPendingHostRequest('owned-request', () => { throw error; }), null);
  }
  assert.equal(readPendingHostRequest('owned-request', () => '{"operation":"activate_tab"}'), '{"operation":"activate_tab"}');
});

test('unexpected request read errors remain failures', () => {
  const error = Object.assign(new Error('read failed'), { code: 'EIO' });
  assert.throws(() => readPendingHostRequest('owned-request', () => { throw error; }), caught => caught === error);
});

test('recorded host error still closes all children and server and remains a failing result', async () => {
  const calls = [];
  const failure = new Error('recorded host failure after poller stopped');
  await assert.rejects(cleanupBrowserFixture({
    closeHost: () => { calls.push('host'); throw failure; },
    stopChildren: [() => calls.push('child-a'), () => calls.push('child-b')],
    closeServer: () => calls.push('server'),
    removeFiles: () => calls.push('files'),
  }), error => error instanceof AggregateError && error.errors[0] === failure);
  assert.deepEqual(calls, ['host', 'child-a', 'child-b', 'server', 'files']);
});

test('unconfirmed child shutdown preserves files but closes remaining resources', async () => {
  const calls = [];
  await assert.rejects(cleanupBrowserFixture({
    closeHost: () => calls.push('host'),
    stopChildren: [() => { calls.push('child-a'); throw new Error('stop failed'); }, () => calls.push('child-b')],
    closeServer: () => calls.push('server'),
    removeFiles: () => calls.push('files'),
  }), AggregateError);
  assert.deepEqual(calls, ['host', 'child-a', 'child-b', 'server']);
});

test('unconfirmed server shutdown preserves files and propagates failure', async () => {
  let removed = false;
  await assert.rejects(cleanupBrowserFixture({
    closeHost: () => {}, stopChildren: [() => {}],
    closeServer: () => { throw new Error('close failed'); },
    removeFiles: () => { removed = true; },
  }), AggregateError);
  assert.equal(removed, false);
});
