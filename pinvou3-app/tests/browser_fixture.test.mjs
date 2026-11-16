import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { EventEmitter } from 'node:events';
import { launchBrowser } from './helpers/browser-fixture.mjs';
import { FixtureScope, ownedDirectory } from './helpers/fixture-scope.mjs';

function fixture(t) {
  const ledger = ownedDirectory('browser-lease-test');
  t.after(() => ledger.remove());
  const scope = new FixtureScope({ ledger: ledger.path });
  let removals = 0;
  // No real browser or profile is created: exercise the actual lease decisions.
  const createDirectory = () => ({ path: 'controlled-profile', remove() { removals++; } });
  const receipts = () => fs.readdirSync(ledger.path)
    .map(name => JSON.parse(fs.readFileSync(path.join(ledger.path, name), 'utf8')));
  return { scope, createDirectory, receipts, removals: () => removals };
}

for (const failure of ['launch-rejected', 'missing-handle', 'handle-read-rejected']) {
  test(`browser ${failure} preserves failure and never authorizes profile removal`, async t => {
    const lease = fixture(t);
    const primary = new Error('controlled launch or handle failure');
    const diagnostics = [];
    t.mock.method(console, 'error', (...args) => diagnostics.push(args));
    let launchCalls = 0;
    const launch = async options => {
      launchCalls++;
      assert.equal(options.userDataDir, 'controlled-profile');
      if (failure === 'launch-rejected') throw primary;
      return { connected: false, close() {}, process() {
        if (failure === 'handle-read-rejected') throw primary;
        return null;
      } };
    };
    await assert.rejects(launchBrowser(launch, {}, lease), error => failure === 'missing-handle'
      ? /process handle is unavailable/.test(error.message) : error === primary);
    assert.equal(launchCalls, 1);
    assert.equal(lease.removals(), 0);
    assert.deepEqual(lease.receipts().map(({ state }) => state), ['unconfirmed']);
    assert.match(diagnostics[0][1].message, /no child lifecycle handle/);
    await assert.rejects(lease.scope.close(), /did not become clean/);
    assert.equal(lease.removals(), 0);
    assert.deepEqual(lease.receipts().map(({ state }) => state), ['unconfirmed']);
  });
}

test('browser with an exited child retires after acknowledged close and only once', async t => {
  const lease = fixture(t);
  const child = Object.assign(new EventEmitter(), { exitCode: 0, signalCode: null });
  const browser = { connected: true, process: () => child,
    async close() { assert.equal(lease.removals(), 0); this.connected = false; child.emit('close'); } };
  assert.equal(await launchBrowser(async () => browser, {}, lease), browser);
  await browser.close();
  await browser.close();
  await lease.scope.close();
  assert.equal(lease.removals(), 1);
  assert.deepEqual(lease.receipts().map(({ state }) => state), ['released']);
});

for (const exitCode of [null, 0]) {
  test(`native close without child close acknowledgement retains the profile (exitCode=${exitCode})`, async t => {
    const lease = fixture(t);
    const child = Object.assign(new EventEmitter(), { exitCode, signalCode: null });
    let acknowledgeRequest;
    const requested = new Promise(resolve => { acknowledgeRequest = resolve; });
    const browser = { connected: true, process: () => child,
      async close() { this.connected = false; acknowledgeRequest(); } };
    await launchBrowser(async () => browser, {}, lease);
    let closed = false;
    const closing = browser.close().then(() => { closed = true; });
    await requested;
    // Let all already-ready promise continuations settle, without delivering
    // the child acknowledgement. exit alone must not complete the lease.
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(closed, false);
    assert.equal(lease.removals(), 0);
    assert.deepEqual(lease.receipts().map(({ state }) => state), ['registered']);
    child.emit('close');
    await closing;
    assert.equal(lease.removals(), 1);
    assert.deepEqual(lease.receipts().map(({ state }) => state), ['released']);
  });
}

test('an exited child without an observed close remains unconfirmed', async t => {
  const lease = fixture(t);
  const child = Object.assign(new EventEmitter(), { exitCode: 0, signalCode: null });
  const browser = { connected: true, process: () => child,
    async close() { this.connected = false; } };
  await launchBrowser(async () => browser, {}, lease);
  await assert.rejects(browser.close(), /owned browser wrapper close was not confirmed/);
  assert.equal(lease.removals(), 0);
  assert.deepEqual(lease.receipts().map(({ state }) => state), ['unconfirmed']);
  child.emit('close');
  await assert.rejects(lease.scope.close(), /did not become clean/);
  assert.equal(lease.removals(), 0);
  assert.deepEqual(lease.receipts().map(({ state }) => state), ['unconfirmed']);
});

test('registration failure removes the unused private profile without launching', async t => {
  const lease = fixture(t);
  await lease.scope.close();
  let launched = false;
  await assert.rejects(launchBrowser(async () => { launched = true; }, {}, lease), /already closing/);
  assert.equal(launched, false);
  assert.equal(lease.removals(), 1);
  assert.deepEqual(lease.receipts(), []);
});
