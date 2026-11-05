import assert from 'node:assert/strict';
import test from 'node:test';
import path from 'node:path';
import { prometheusRuleCommands } from '../../../scripts/account-platform-prometheus-rules.mjs';

test('offline alert entrypoint keeps explicit rendered input and actual promtool execution separate from deployment', () => {
  const rendered = path.resolve('controlled-render.yaml'), output = path.resolve('new-isolated-output'), tool = path.resolve('promtool');
  const commands = prometheusRuleCommands(['--rendered', rendered, '--output', output, '--promtool', tool]);
  assert.equal(commands.length, 2);
  assert.equal(commands[0][0], process.execPath);
  assert.deepEqual(commands[0][1].slice(1), [rendered, output]);
  assert.deepEqual(commands[1], [tool, ['test', 'rules', path.join(output, 'event-relay-missing.test.yaml')]]);
  for (const args of [[], ['--rendered', 'relative', '--output', output, '--promtool', tool], ['--rendered', rendered, '--output', output, '--promtool', tool, '--skip']]) assert.throws(() => prometheusRuleCommands(args));
});
