import assert from 'node:assert/strict';
import test from 'node:test';
import { spawnNdjsonChild, stopNdjsonChild } from './helpers/ndjson-child-driver.mjs';
import { confirmedChildStopper } from './helpers/browser-wrapper-fixture-cleanup.mjs';

const childSource = `
  import { createInterface } from 'node:readline';
  createInterface({ input: process.stdin }).on('line', line => {
    const message = JSON.parse(line);
    if (message.method === 'hold') return;
    process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: message.id, result: message.params }) + '\\n');
  });
`;

for (const [name, options, expectedLabel] of [
  ['numeric', 25, 'hold'],
  ['labelled options', { timeoutMs: 25, label: 'bounded read' }, 'bounded read'],
]) {
  test(`NDJSON driver retains ${name} deadlines and remains reusable`, { timeout: 10_000 }, async t => {
    const driver = spawnNdjsonChild({
      args: ['--input-type=module', '-e', childSource],
      timeoutMs: 60_000,
      unrefTimeout: true,
    });
    t.after(confirmedChildStopper(driver.child, stopNdjsonChild));
    assert.deepEqual((await driver.request('ready', { ready: true })).result, { ready: true });
    await assert.rejects(driver.request('hold', {}, options), error => {
      assert.match(error.message, new RegExp(`^${expectedLabel} timed out after 25ms \\(request \\d+\\); child stderr:`));
      return true;
    });
    assert.deepEqual((await driver.request('echo', { afterTimeout: true })).result, { afterTimeout: true });
  });
}
