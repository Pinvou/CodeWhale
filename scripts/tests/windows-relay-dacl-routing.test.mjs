import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const workflow = await readFile(new URL('../../.github/workflows/pr-check.yml', import.meta.url), 'utf8');
function requireNativeGate(text) {
  const job = text.match(/^  windows-relay-dacl:\r?\n([\s\S]*?)(?=^  \S|(?![\s\S]))/m)?.[1];
  assert.ok(job, 'native DACL job must exist');
  assert.match(job, /runs-on: windows-latest/);
  assert.match(job, /needs\.changes\.outputs\.relay == 'true'/);
  assert.match(job, /github\.event_name == 'merge_group'/);
  assert.match(job, /github\.event_name == 'pull_request'/);
  assert.doesNotMatch(job, /draft|continue-on-error/);
  assert.match(job, /run: node scripts\/run-windows-relay-dacl\.mjs/);
  const gate = text.slice(text.indexOf('\n  required-gate:'));
  assert.match(gate, /- windows-relay-dacl/);
  assert.match(gate, /WINDOWS_RELAY_DACL_RESULT: \$\{\{ needs\.windows-relay-dacl\.result \}\}/);
  assert.ok(gate.includes('"windows-relay-dacl:$WINDOWS_RELAY_DACL_RESULT"'));
  assert.match(text, /- 'scripts\/run-windows-relay-dacl\.mjs'/);
  assert.ok(text.includes('          scripts/tests/windows-relay-dacl-routing.test.mjs'));
  assert.ok(text.includes('          scripts/tests/windows-relay-dacl-runner.test.mjs'));
}

test('native DACL gate covers Relay changes and cannot disappear from required aggregation', () => {
  requireNativeGate(workflow);
  for (const damaged of [
    workflow.replace('runs-on: windows-latest\n    timeout-minutes: 10\n    steps:\n      - uses: actions/checkout@v7',
      'runs-on: ubuntu-22.04\n    timeout-minutes: 10\n    steps:\n      - uses: actions/checkout@v7'),
    workflow.replace('      - windows-relay-dacl\n', ''),
    workflow.replace('"windows-relay-dacl:$WINDOWS_RELAY_DACL_RESULT"', ''),
    workflow.replace('run: node scripts/run-windows-relay-dacl.mjs', 'run: echo skipped'),
  ]) assert.throws(() => requireNativeGate(damaged));
});
