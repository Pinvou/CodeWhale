import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const workflow = await readFile(new URL('../../.github/workflows/pr-check.yml', import.meta.url), 'utf8');
const nativeRunner = `runs-on: \${{ fromJSON(vars.PINVOU_WINDOWS_LIGHT_RUNNER || vars.PINVOU_WINDOWS_RUNNER || '["windows-latest"]') }}`;
function requireNativeGate(text) {
  const job = text.match(/^  windows-relay-dacl:\r?\n([\s\S]*?)(?=^  \S|(?![\s\S]))/m)?.[1];
  assert.ok(job, 'native DACL job must exist');
  assert.ok(job.includes(nativeRunner), 'DACL checks must use the Windows light pool with a native hosted fallback');
  assert.match(job, /vars\.PINVOU_WINDOWS_ENABLED != 'false'/);
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
    workflow.replace(nativeRunner, 'runs-on: ubuntu-22.04'),
    workflow.replace(nativeRunner, nativeRunner.replace('WINDOWS_LIGHT_RUNNER', 'LINUX_LIGHT_RUNNER')),
    workflow.replace(nativeRunner, nativeRunner.replace('windows-latest', 'ubuntu-22.04')),
    workflow.replace('      - windows-relay-dacl\n', ''),
    workflow.replace('"windows-relay-dacl:$WINDOWS_RELAY_DACL_RESULT"', ''),
    workflow.replace('run: node scripts/run-windows-relay-dacl.mjs', 'run: echo skipped'),
  ]) {
    assert.notEqual(damaged, workflow, 'mutation must alter the live workflow');
    assert.throws(() => requireNativeGate(damaged));
  }
});
