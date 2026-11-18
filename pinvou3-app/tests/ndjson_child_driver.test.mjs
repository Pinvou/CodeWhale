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
      assert.match(error.message, new RegExp(`^${expectedLabel} timed out after 25ms \\(request \\d+\\); observer:`));
      const diagnostic = JSON.parse(error.message.match(/; observer: (\{[^\n]+?\}); child stderr:/)[1]);
      assert.deepEqual(Object.keys(diagnostic).sort(),
        ['child_exit_code', 'observer_elapsed_ms', 'observer_loop_max_delay_ms']);
      assert.ok(Number.isSafeInteger(diagnostic.observer_elapsed_ms) && diagnostic.observer_elapsed_ms >= 0);
      assert.ok(Number.isSafeInteger(diagnostic.observer_loop_max_delay_ms) && diagnostic.observer_loop_max_delay_ms >= 0);
      assert.equal(diagnostic.child_exit_code, null);
      return true;
    });
    assert.deepEqual((await driver.request('echo', { afterTimeout: true })).result, { afterTimeout: true });
  });
}
