import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { EventEmitter } from 'node:events';
import { parse } from 'acorn';
import { FixtureScope } from './helpers/fixture-scope.mjs';
import { confirmedChildStopper } from './helpers/browser-wrapper-fixture-cleanup.mjs';

// Run the actual factory body with in-memory ownership boundaries. The native
// launcher and directory are fakes; no browser, process or directory is created.
const source = readFileSync(new URL('./helpers/browser-fixture.mjs', import.meta.url), 'utf8');
const declaration = parse(source, { ecmaVersion: 'latest', sourceType: 'module' }).body.find(node => node.type === 'ExportNamedDeclaration' && node.declaration?.id?.name === 'launchBrowser')?.declaration;
assert.ok(declaration, 'the real browser factory is available');
const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
const launchOwned = new AsyncFunction('fixtureScope', 'ownedDirectory', 'confirmedChildStopper', 'launchNative', 'options', source.slice(declaration.body.start + 1, declaration.body.end - 1));

async function fixture({ exitCode = null, signalCode = null, historicalClose = false, automaticClose = false } = {}) {
  const child = Object.assign(new EventEmitter(), { exitCode, signalCode });
  if (historicalClose) child.emit('close', exitCode, signalCode);
  const scope = new FixtureScope(), calls = { native: 0, remove: 0 };
  let nativeStarted;
  const started = new Promise(resolve => { nativeStarted = resolve; });
  const browser = {
    connected: true,
    process: () => child,
    close: async () => {
      calls.native += 1;
      browser.connected = false;
      nativeStarted();
      if (automaticClose) {
        child.exitCode = 0;
        child.emit('exit', 0, null);
        queueMicrotask(() => child.emit('close', child.exitCode, child.signalCode));
      }
    },
  };
  await launchOwned(scope, () => ({ path: 'synthetic-private-profile', remove: () => { calls.remove += 1; } }),
    (owned, stop) => confirmedChildStopper(owned, stop, { closeMs: 5 }),
    async options => { assert.equal(options.userDataDir, 'synthetic-private-profile'); return browser; }, {});
  return { browser, child, calls, started };
}

for (const [name, state] of [
  ['exit code', { exitCode: 0 }],
  ['signal code', { signalCode: 'SIGTERM' }],
  ['historical unobserved close', { exitCode: 0, historicalClose: true }],
]) {
  test(`${name} alone cannot authorize retiring an unconfirmed browser profile`, async () => {
    const value = await fixture(state);
    await assert.rejects(value.browser.close(), /close was not confirmed/);
    assert.equal(value.calls.native, 1);
    assert.equal(value.calls.remove, 0, 'an unconfirmed owned profile is retained');
  });
}

test('an exited child still waits for its owned close observation and repeated cleanup is idempotent', async () => {
  const value = await fixture({ exitCode: 0 });
  const closing = value.browser.close();
  await value.started;
  assert.equal(value.calls.remove, 0);
  value.child.emit('close', 0, null);
  await closing;
  await value.browser.close();
  assert.deepEqual(value.calls, { native: 1, remove: 1 });
});

test('the ordinary running-child close path retains its confirmation and profile cleanup', async () => {
  const value = await fixture({ automaticClose: true });
  await value.browser.close();
  assert.deepEqual(value.calls, { native: 1, remove: 1 });
});
