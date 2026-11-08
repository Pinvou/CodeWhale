import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { parse } from 'yaml';

const workflow = parse(readFileSync(new URL('../../.github/workflows/pr-check.yml', import.meta.url), 'utf8'));
const job = workflow.jobs['release-contract-test'];
const step = job.steps.find((value) => value.name === 'Windows 远控发布与安全契约');

test('Windows release step retains every mandatory command and required-gate dependency', () => {
  assert.ok(step);
  assert.equal(step.if, "${{ runner.os == 'Windows' }}");
  assert.equal(step.env.PINVOU_DESKTOP_CONTROLLER_ENABLED, '0');
  assert.equal(step['continue-on-error'], undefined);
  assert.ok(workflow.jobs['required-gate'].needs.includes('release-contract-test'));
  assert.equal(step.run.split('\n').filter((line) => /^(?:npm|node)\b/u.test(line)).length, 3);
  assert.match(step.run, /npm --prefix remote-control-relay run release:gate/u);
  assert.match(step.run, /windows-e2e-orchestrator-helpers\.test\.js/u);
  assert.match(step.run, /remote_control_feature_gate_contract\.test\.mjs/u);
  const regression = job.steps.find((value) => value.name === 'Windows 原生命令退出码回归');
  assert.ok(job.steps.indexOf(regression) < job.steps.indexOf(step));
  assert.match(regression.run, /python scripts\/ci-fixture-process\.py --receipt .+ -- node --test remote-control-relay\/test\/windows-release-exit-code\.test\.js/u);
  const pathFilter = workflow.jobs.changes.steps.find((value) => value.id === 'filter');
  assert.ok(parse(pathFilter.with.filters).release_contract.includes('remote-control-relay/test/windows-release-exit-code.test.js'));
});

for (const shell of ['pwsh', 'powershell.exe']) {
  test(`real workflow stops on each native failure in ${shell}`, { skip: process.platform !== 'win32', timeout: 60_000 }, () => {
    for (const failure of [0, 1, 2, null]) {
      let command = 0;
      // Substitute only the three external contracts. Execute their actual
      // workflow control flow so deleting a guard fails this regression.
      const script = step.run.split('\n').map((line) => {
        if (!/^(?:npm|node)\b/u.test(line)) return line;
        const index = command++;
        const exitCode = failure === index ? 17 : 0;
        return `& $env:PINVOU_NATIVE_EXIT_TEST_NODE -e 'console.log(${index});process.exit(${exitCode})'`;
      }).join('\n');
      assert.equal(command, 3);
      const result = spawnSync(shell, ['-NoProfile', '-NonInteractive', '-Command', script], {
        env: { ...process.env, PINVOU_NATIVE_EXIT_TEST_NODE: process.execPath },
        encoding: 'utf8', windowsHide: true, timeout: 15_000,
      });
      assert.ifError(result.error);
      assert.equal(result.status, failure === null ? 0 : 17, `failure=${failure}: ${result.stderr}`);
      const expected = Array.from({ length: failure === null ? 3 : failure + 1 }, (_, index) => String(index));
      assert.deepEqual(result.stdout.trim().split(/\r?\n/u), expected, `later commands must not mask failure=${failure}`);
    }
  });
}
