import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { FixtureScope, ownedDirectory } from './helpers/fixture-scope.mjs';

async function signalProbe(t, { failure = false, primary = 0, signal = 'SIGTERM', blockedVerification = false, earlierFailure = false } = {}) {
  const directory = ownedDirectory('signal-probe');
  t.after(() => directory.remove());
  const ledger = path.join(directory.path, 'receipts');
  fs.mkdirSync(ledger);
  const source = `
    import fs from 'node:fs';
    import path from 'node:path';
    ${blockedVerification ? `const realTimer = globalThis.setTimeout; globalThis.setTimeout = (fn, ms, ...args) => realTimer(fn, ms === 30_000 ? 25 : ms, ...args);` : ''}
    const { fixtureScope } = await import(${JSON.stringify(new URL('./helpers/fixture-scope.mjs', import.meta.url).href)});
    fixtureScope.register({ kind: 'probe', id: 'signal',
      close() { ${failure ? `throw new Error('controlled signal close failure');` : ''} },
      verify: ${blockedVerification ? '() => new Promise(() => {})' : '() => true'},
      remove() { fs.writeFileSync(path.join(process.env.PINVOU_CI_RESOURCE_LEDGER, 'removed'), 'confirmed'); },
    });
    ${earlierFailure ? `fixtureScope.register({kind:'probe', id:'earlier-failure', close(){throw new Error('earlier close diagnostic');}});` : ''}
    process.exitCode = ${primary};
    setInterval(() => {}, 1000);
    if (process.platform === 'win32') process.emit(${JSON.stringify(signal)});
    else process.kill(process.pid, ${JSON.stringify(signal)});
  `;
  const result = await new Promise((resolve, reject) => {
    const child = spawn(process.execPath, ['--input-type=module', '--eval', source], {
      env: { ...process.env, PINVOU_CI_RESOURCE_LEDGER: ledger }, stdio: ['ignore', 'pipe', 'pipe'],
    });
    let output = '';
    let timedOut = false;
    const timer = setTimeout(() => { timedOut = true; child.kill('SIGKILL'); }, 10_000);
    child.stdout.on('data', chunk => { output += chunk; });
    child.stderr.on('data', chunk => { output += chunk; });
    child.once('error', error => { clearTimeout(timer); reject(error); });
    child.once('close', (code, exitSignal) => {
      clearTimeout(timer);
      if (timedOut) reject(new Error('actual fixture signal handler failed to exit within its test budget'));
      else resolve({ code, exitSignal, output });
    });
  });
  return { ...result, receipt: JSON.parse(fs.readFileSync(path.join(ledger, 'signal.json'))),
    earlierReceipt: earlierFailure ? JSON.parse(fs.readFileSync(path.join(ledger, 'earlier-failure.json'))) : undefined,
    removed: fs.existsSync(path.join(ledger, 'removed')) };
}

test('successful SIGTERM and SIGINT cleanup exits even with another active handle', async t => {
  for (const [signal, code] of [['SIGTERM', 143], ['SIGINT', 130]]) {
    const result = await signalProbe(t, { signal });
    assert.equal(result.code, code);
    assert.equal(result.receipt.state, 'released');
    assert.equal(result.removed, true);
  }
});

test('failed signal cleanup exits and keeps the original failure and unfinished receipt', async t => {
  for (const primary of [0, 7]) {
    const result = await signalProbe(t, { failure: true, primary });
    assert.equal(result.code, primary || 1);
    assert.match(result.output, /controlled signal close failure/);
    assert.equal(result.receipt.state, 'unconfirmed');
    assert.equal(result.removed, false);
  }
});

test('signal watchdog bounds a blocked verification without authorizing removal', async t => {
  // Only the watchdog clock is accelerated; the actual signal hook and exit run.
  const result = await signalProbe(t, { blockedVerification: true });
  assert.equal(result.code, 1);
  assert.match(result.output, /fixture signal cleanup deadline exceeded/);
  assert.equal(result.receipt.state, 'registered');
  assert.equal(result.removed, false);
});

test('watchdog preserves an earlier close diagnostic when a later resource stalls', async t => {
  const result = await signalProbe(t, { blockedVerification: true, earlierFailure: true, primary: 7 });
  assert.equal(result.code, 7);
  assert.match(result.output, /earlier close diagnostic/);
  assert.match(result.output, /fixture signal cleanup deadline exceeded/);
  assert.equal(result.receipt.state, 'registered');
  assert.equal(result.earlierReceipt.state, 'unconfirmed');
  assert.equal(result.removed, false);
});

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
