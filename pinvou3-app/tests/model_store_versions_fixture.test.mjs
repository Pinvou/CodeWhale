import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { matchesViteModule } from './helpers/vite-fixture-module.mjs';

test('settings form fixture injection matches POSIX and Windows Vite module IDs', () => {
  const suffix = 'src/features/settings/SettingsView.jsx';
  for (const root of ['/tmp/pinvou3-app', 'C:/workspace/pinvou3-app']) {
    const viteId = `${root}/${suffix}`;
    const filename = root.startsWith('C:')
      ? path.win32.join(root, suffix) : path.posix.join(root, suffix);
    assert.equal(matchesViteModule(viteId, filename), true);
    assert.equal(matchesViteModule(`${viteId}?v=fixture`, filename), true);
    assert.equal(matchesViteModule(filename, viteId), true);
    assert.equal(matchesViteModule(`${root}/src/features/settings/Other.jsx`, filename), false);
    assert.equal(matchesViteModule(`${viteId}.backup`, filename), false);
  }
});

const smokeSource = fs.readFileSync(new URL('./model_store_versions_smoke.mjs', import.meta.url), 'utf8');

// Execute the smoke's actual failure/retirement control flow, replacing only
// the browser assertion body. This keeps the regression tied to its entry point.
function lifecycleSource(source) {
  const declarations = source.indexOf('let primaryFailure;');
  const body = source.indexOf('try {', declarations);
  const tail = source.lastIndexOf('} catch (error) {');
  assert.ok(declarations > 0 && body > declarations && tail > body);
  return `${source.slice(declarations, body)}try { if (shouldThrow) throw primary;\n${source.slice(tail)}`;
}

const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
const primaryCases = [0, false, '', null, undefined, new Error('controlled primary assertion')];

async function settle(promise) {
  try {
    return { rejected: false, value: await promise };
  } catch (error) {
    return { rejected: true, error };
  }
}

async function assertLifecycle(source) {
  const run = new AsyncFunction('primary', 'fixtureScope', 'shouldThrow', lifecycleSource(source));
  for (const primary of primaryCases) {
    const cleanup = new Error('controlled fixture retirement failure');
    let closeCalls = 0;
    const combined = await settle(run(primary, { close: async () => {
      closeCalls += 1;
      throw cleanup;
    } }, true));
    assert.equal(closeCalls, 1);
    assert.equal(combined.rejected, true);
    assert.ok(combined.error instanceof AggregateError);
    assert.equal(combined.error.errors.length, 2);
    assert.ok(Object.is(combined.error.errors[0], primary));
    assert.equal(combined.error.errors[1], cleanup);
    assert.equal(combined.error.cause, cleanup);
    const single = await settle(run(primary, { close: async () => { closeCalls += 1; } }, true));
    assert.equal(closeCalls, 2);
    assert.equal(single.rejected, true);
    assert.ok(Object.is(single.error, primary));
  }
  for (const cleanup of primaryCases) {
    const cleanupOnly = await settle(run(undefined, { close: async () => { throw cleanup; } }, false));
    assert.equal(cleanupOnly.rejected, true);
    assert.ok(Object.is(cleanupOnly.error, cleanup));
  }
  let closeCalls = 0;
  const success = await settle(run(undefined, { close: async () => { closeCalls += 1; } }, false));
  assert.equal(success.rejected, false);
  assert.equal(closeCalls, 1);
}

test('smoke retirement preserves falsy primary rejections and both failures in order', async () => {
  await assertLifecycle(smokeSource);
});

test('smoke lifecycle regression detects lost primary failures, cleanup failures and causes', async () => {
  for (const [before, after] of [
    ['hasPrimaryFailure = true;', 'hasPrimaryFailure = Boolean(error);'],
    ['await fixtureScope.close();', 'await fixtureScope.close().catch(() => {});'],
    ['if (hasPrimaryFailure) throw primaryFailure;', ''],
    ['{ cause: cleanupError }', '{ cause: primaryFailure }'],
  ]) {
    assert.ok(smokeSource.includes(before));
    await assert.rejects(() => assertLifecycle(smokeSource.replace(before, after)));
  }
});

test('every unhandled smoke or cleanup failure exits nonzero after attempting retirement', () => {
  for (const primary of ['0', 'false', "''", 'null', 'undefined', "new Error('controlled primary')"]) {
    for (const cleanupFails of [false, true]) {
      const child = spawnSync(process.execPath, ['--input-type=module', '--eval', `
        const primary = ${primary};
        const shouldThrow = true;
        const fixtureScope = { async close() {
          process.stderr.write('fixture-close-called\\n');
          if (${cleanupFails}) throw new Error('controlled cleanup');
        } };
        ${lifecycleSource(smokeSource)}
      `], { encoding: 'utf8', timeout: 10000 });
      assert.equal(child.error, undefined);
      assert.equal(child.signal, null);
      assert.notEqual(child.status, 0);
      assert.ok(child.stderr.includes('fixture-close-called\n'));
    }
  }
  const child = spawnSync(process.execPath, ['--input-type=module', '--eval', `
    const primary = undefined;
    const shouldThrow = false;
    const fixtureScope = { async close() { throw new Error('controlled cleanup-only'); } };
    ${lifecycleSource(smokeSource)}
  `], { encoding: 'utf8', timeout: 10000 });
  assert.equal(child.error, undefined);
  assert.equal(child.signal, null);
  assert.notEqual(child.status, 0);
  assert.ok(child.stderr.includes('controlled cleanup-only'));
});
