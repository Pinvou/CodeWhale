import assert from 'node:assert/strict';
import test from 'node:test';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { existsSync, lstatSync, mkdtempSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from './helpers/net-fixture.mjs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const appRoot = fileURLToPath(new URL('../', import.meta.url));
const fixtureUrl = new URL('./account_platform_recharge_polling_server.mjs', import.meta.url).href;
const fixturePath = fileURLToPath(fixtureUrl);
const faultUrl = new URL('./helpers/recharge-polling-faults.mjs', import.meta.url).href;
const netFixtureUrl = new URL('./helpers/net-fixture.mjs', import.meta.url).href;


async function withOwner(action) {
  const owner = mkdtempSync(join(tmpdir(), 'pinvou-polling-lifecycle-'));
  const identity = lstatSync(owner);
  writeFileSync(join(owner, 'canary'), 'preserve');
  try { return await action(owner); }
  finally {
    assert.equal(dirname(resolve(owner)), resolve(tmpdir()));
    assert.ok(resolve(owner).startsWith(resolve(tmpdir()) + sep));
    const current = lstatSync(owner);
    assert.equal(current.isSymbolicLink(), false);
    assert.equal(current.dev, identity.dev); assert.equal(current.ino, identity.ino);
    // Every child has exited before its private directory can be removed.
    rmSync(owner, { recursive: true, force: true });
  }
}

function runChild(owner, { mode = '', code, cli = false, port, ready, endStdinOnSpawn = false } = {}) {
  return new Promise((resolveChild, rejectChild) => {
    const args = ['--import', faultUrl,
      ...(cli ? [fixturePath] : ['--input-type=module', '--eval', code])];
    const env = { ...process.env, TEMP: owner, TMP: owner, TMPDIR: owner,
      FIXTURE_LIFECYCLE_OWNER: owner, FIXTURE_LIFECYCLE_MODE: mode };
    if (port !== undefined) env.PINVOU_TEST_TOK03_PORT = String(port);
    else delete env.PINVOU_TEST_TOK03_PORT;
    const child = spawn(process.execPath, args, { cwd: appRoot, env, stdio: ['pipe', 'pipe', 'pipe', 'ipc'] });
    let stdout = '', stderr = '', lineBuffer = '', timedOut = false, failure;
    let readiness = Promise.resolve();
    const messages = [];
    const timer = setTimeout(() => { timedOut = true; child.kill('SIGKILL'); }, 30000);
    if (endStdinOnSpawn) child.once('spawn', () => child.stdin.end());
    child.stdin.on('error', reason => { failure = reason; if (child.pid) child.kill('SIGKILL'); });
    child.on('message', message => messages.push(message));
    child.stdout.on('data', data => {
      const text = data.toString(); stdout += text; lineBuffer += text;
      while (lineBuffer.includes('\n')) {
        const end = lineBuffer.indexOf('\n'), line = lineBuffer.slice(0, end); lineBuffer = lineBuffer.slice(end + 1);
        let value; try { value = JSON.parse(line); } catch { continue; }
        if (value.url) {
          readiness = readiness.then(() => ready?.(child, value))
            .catch(reason => { failure = reason; child.kill('SIGKILL'); });
        }
      }
    });
    child.stderr.on('data', data => { stderr += data; });
    child.once('error', reason => { failure = reason; if (child.pid) child.kill('SIGKILL'); });
    child.once('close', async (exitCode, signal) => {
      clearTimeout(timer);
      await readiness;
      if (failure) rejectChild(failure);
      else if (timedOut) rejectChild(new Error('Owned fixture child timed out: ' + stderr));
      else resolveChild({ exitCode, signal, stdout, stderr, messages });
    });
  });
}

const importFixture = 'const { startRechargePollingFixture } = await import(' + JSON.stringify(fixtureUrl) + ');';
const expectSuccess = result => assert.equal(result.exitCode, 0, result.stdout + result.stderr);
const projections = owner => readdirSync(owner).filter(name => name.startsWith('pinvou-public-tok03-app-'));
const assertRetired = owner => assert.deepEqual(projections(owner), [], JSON.stringify({
  remaining: readdirSync(owner, { recursive: true }).filter(name => name.startsWith('pinvou-public-tok03-app-')).slice(0, 25),
}));

test('real fixture HTTP, bounded ownership and concurrent close remain usable', async () => withOwner(async owner => {
  const code = importFixture + String.raw`
    const assert = (await import('node:assert/strict')).default;
    const fs = await import('node:fs');
    const net = await import(${JSON.stringify(netFixtureUrl)});
    const events = await import('node:events');
    const before = process.listenerCount('SIGTERM');
    const fixture = await startRechargePollingFixture();
    try {
      assert.equal(process.listenerCount('SIGTERM'), before);
      assert.equal((await fetch(fixture.url)).status, 200);
      const origin = new URL(fixture.url).origin;
      const outside = process.env.FIXTURE_LIFECYCLE_OWNER.replaceAll('\\', '/') + '/canary';
      assert.equal((await fetch(origin + '/@fs/' + outside)).status, 403);
      await fetch(origin + '/__fixture__/control', { method: 'POST', body: JSON.stringify({ hold: true }) });
      const pending = fetch(origin + '/api/token/v1/recharge/orders/33333333-3333-4333-8333-333333333333').then(() => false, () => true);
      for (let attempt = 0;; attempt++) {
        const state = await (await fetch(origin + '/__fixture__/state')).json();
        if (state.held === 1) break;
        assert.ok(attempt < 100, 'the real held request must be observed');
        await new Promise(resolve => setTimeout(resolve, 5));
      }
      const first = fixture.close(); assert.equal(first, fixture.close()); await first;
      assert.equal(await pending, true); assert.equal(fs.existsSync(fixture.rootPath), false);
      assert.equal(fixture.close(), first);
      const server = net.createServer(); server.listen(Number(new URL(fixture.url).port), '127.0.0.1');
      await events.once(server, 'listening'); await new Promise((resolve, reject) => server.close(e => e ? reject(e) : resolve()));
    } finally { await fixture.close(); }
  `;
  expectSuccess(await runChild(owner, { code })); assertRetired(owner);
}));

test('invalid port and every pre-server copy boundary retire only their own directory', async () => {
  for (const mode of ['', 'copy-cp', 'copy-mkdir', 'copy-file']) await withOwner(async owner => {
    const result = await runChild(owner, { mode, port: mode ? 0 : 99999, code: importFixture + String.raw`
      const assert = (await import('node:assert/strict')).default;
      await assert.rejects(startRechargePollingFixture(), reason =>
        reason.message === (process.env.FIXTURE_LIFECYCLE_MODE ? 'INJECTED_COPY_FAILURE' : 'FIXTURE_PORT_INVALID'));
    ` });
    expectSuccess(result); assert.deepEqual(projections(owner), []); assert.equal(existsSync(join(owner, 'canary')), true);
  });
});

test('real bind failure preserves its cause and removes the allocated projection', async () => withOwner(async owner => {
  const result = await runChild(owner, { code: importFixture + String.raw`
    const assert = (await import('node:assert/strict')).default;
    const server = (await import(${JSON.stringify(netFixtureUrl)})).createServer();
    server.listen(0, '127.0.0.1'); await (await import('node:events')).once(server, 'listening');
    try {
      process.env.PINVOU_TEST_TOK03_PORT = String(server.address().port);
      await assert.rejects(startRechargePollingFixture(), reason => reason.code === 'EADDRINUSE');
      assert.equal(server.listening, true, 'the unrelated listener belongs to this test and must remain live');
    } finally { await new Promise(resolve => server.close(resolve)); }
  ` });
  expectSuccess(result); assertRetired(owner);
}));

test('partially sent control and order POST bodies retire before close resolves', async () => withOwner(async owner => {
  const result = await runChild(owner, { code: importFixture + String.raw`
    const assert = (await import('node:assert/strict')).default;
    const http = await import('node:http');
    const { once } = await import('node:events');
    const fs = await import('node:fs');
    for (const path of ['/__fixture__/control', '/api/token/v1/recharge/orders']) {
      const fixture = await startRechargePollingFixture();
      const origin = new URL(fixture.url).origin;
      const request = http.request(origin + path, { method: 'POST', headers: { 'Content-Length': 100, Expect: '100-continue' } });
      const aborted = new Promise(resolve => request.once('error', reason => resolve(reason.code)));
      try {
        // Node sends 100 Continue before invoking the real request handler in
        // the same turn. Receiving it fixes the body-read/close interleaving.
        request.flushHeaders(); await once(request, 'continue');
        await new Promise((resolve, reject) => request.write('{', reason => reason ? reject(reason) : resolve()));
        if (path.startsWith('/api/')) {
          const state = await (await fetch(origin + '/__fixture__/state')).json();
          assert.ok(state.requests.some(item => item.path === path && item.method === 'POST'));
        }
        const close = fixture.close(); assert.equal(close, fixture.close()); await close;
        assert.equal(await aborted, 'ECONNRESET'); assert.equal(fs.existsSync(fixture.rootPath), false);
      } finally { request.destroy(); await fixture.close(); }
    }
  ` });
  expectSuccess(result); assertRetired(owner);
}));

test('copy and cleanup failures preserve both errors and CLI cleanup errors remain visible', async () => {
  await withOwner(async owner => {
    const result = await runChild(owner, { mode: 'copy-and-remove', code: importFixture + String.raw`
      const assert = (await import('node:assert/strict')).default;
      await assert.rejects(startRechargePollingFixture(), reason => reason instanceof AggregateError
        && reason.cause.code === 'INJECTED_COPY_FAILURE'
        && reason.errors[0].code === 'INJECTED_COPY_FAILURE'
        && reason.errors[1].code === 'INJECTED_REMOVE_FAILURE');
    ` });
    expectSuccess(result); assert.equal(projections(owner).length, 1);
  });
  await withOwner(async owner => {
    const result = await runChild(owner, { mode: 'remove', cli: true, ready: child => child.stdin.end() });
    assert.equal(result.exitCode, 1, result.stderr);
    assert.match(result.stderr, /INJECTED_REMOVE_FAILURE/);
  });
});

test('real stdin EOF before startup and after readiness retires the CLI projection', async () => {
  for (const beforeStartup of [true, false]) await withOwner(async owner => {
    const result = await runChild(owner, { cli: true, endStdinOnSpawn: beforeStartup,
      ready: beforeStartup ? undefined : child => child.stdin.end() });
    expectSuccess(result); assertRetired(owner);
    if (beforeStartup) assert.doesNotMatch(result.stdout, /"url":/, 'an early EOF must not announce a usable fixture');
  });
});

test('startup signal and repeated signal events await the same CLI cleanup', async () => {
  await withOwner(async owner => {
    const result = await runChild(owner, { mode: 'startup-signal', cli: true });
    expectSuccess(result); assert.deepEqual(projections(owner), []);
    assert.doesNotMatch(result.stdout, /"url":/, 'shutdown during startup must not announce a usable fixture');
  });
  await withOwner(async owner => {
    const result = await runChild(owner, { mode: 'repeat-signal', cli: true, ready: child => child.send('signal') });
    expectSuccess(result); assert.deepEqual(projections(owner), []);
    assert.ok(result.messages.some(message => message.signalListenersAfterFirst > 0), 'signal ownership must survive the first event');
  });
});

for (const signal of ['SIGINT', 'SIGTERM']) test(`POSIX ${signal} retires held requests, port and projection`, { skip: process.platform === 'win32' }, async () => withOwner(async owner => {
  let pending, port;
  const result = await runChild(owner, { mode: 'os-repeat-signal', cli: true, ready: async (child, { url }) => {
    const origin = new URL(url).origin; port = Number(new URL(url).port);
    assert.equal((await fetch(origin + '/__fixture__/control', { method: 'POST', body: JSON.stringify({ hold: true }) })).status, 200);
    pending = fetch(origin + '/api/token/v1/recharge/orders/33333333-3333-4333-8333-333333333333').then(() => false, () => true);
    for (let attempt = 0;; attempt++) {
      const state = await (await fetch(origin + '/__fixture__/state')).json();
      if (state.held === 1) break;
      assert.ok(attempt < 100, 'observe the real held response before signaling');
      await new Promise(resolve => setTimeout(resolve, 5));
    }
    child.kill(signal); setTimeout(() => { if (child.exitCode === null) child.kill(signal); }, 5);
  } });
  expectSuccess(result); assertRetired(owner); assert.equal(await pending, true);
  const server = createServer();
  try { server.listen(port, '127.0.0.1'); await once(server, 'listening'); }
  finally { await new Promise(resolveClose => server.close(resolveClose)); }
}));
