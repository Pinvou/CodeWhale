import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { setTimeout as sleep } from 'node:timers/promises';
import { FixtureScope } from './helpers/fixture-scope.mjs';
import { createBrowserWrapperHost } from './helpers/browser-wrapper-host-fixture.mjs';

function heldBarrier() {
  const shared = new SharedArrayBuffer(Int32Array.BYTES_PER_ELEMENT);
  const view = new Int32Array(shared);
  Atomics.store(view, 0, 1);
  return { shared, release() { Atomics.store(view, 0, 0); Atomics.notify(view, 0); } };
}
async function fixture(options) {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'host-control-'));
  const scope = new FixtureScope();
  const files = { root, workspacePath: path.join(root, 'workspace.json'),
    portPath: path.join(root, 'port.json'), auditPath: path.join(root, 'audit.jsonl') };
  await fs.writeFile(files.auditPath, '');
  try {
    const host = await createBrowserWrapperHost(files, { sessionId: 'control-test', sessionToken: 'test-token' }, { scope, ...options });
    return { host, scope, async close() { await scope.close(); await fs.rm(root, { recursive: true }); } };
  } catch (error) {
    await scope.close(); await fs.rm(root, { recursive: true }); throw error;
  }
}
async function waitFor(predicate) {
  const end = performance.now() + 5000;
  while (!predicate()) {
    assert.ok(performance.now() < end, 'worker did not reach the held directory read');
    await sleep(10);
  }
}

test('pending directory I/O does not block control, and stop waits for the owned I/O', async () => {
  const barrier = heldBarrier();
  const f = await fixture({ requestReadBarrier: barrier.shared });
  try {
    await waitFor(() => f.host.diagnostic().phase === 'directory');
    await f.host.deferOperation('prepare', true);
    assert.ok((await f.host.snapshot()).deferredOperations.has('prepare'));
    let stopped = false;
    const closing = f.host.close().then(() => { stopped = true; });
    await sleep(100);
    assert.equal(stopped, false, 'stop must not acknowledge while directory I/O is still owned');
    barrier.release();
    await closing;
  } finally {
    barrier.release(); await f.close();
  }
});

test('late configure acknowledgement keeps stale mutations rejected but cannot prevent owned retirement', async () => {
  const barrier = heldBarrier();
  const f = await fixture({ configurationReplyBarrier: barrier.shared });
  try {
    await assert.rejects(f.host.deferOperation('prepare', true), /configure acknowledgement timed out/u);
    barrier.release();
    await assert.rejects(f.host.setPreparePublishesWorkspace(false), /stale configuration revision/u);
    await f.host.close();
  } finally {
    barrier.release(); await f.close();
  }
});
