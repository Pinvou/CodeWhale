import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { parse } from 'yaml';
import { minimatch } from 'minimatch';

const workflow = parse(readFileSync(new URL('../../.github/workflows/pr-check.yml', import.meta.url), 'utf8'));
const job = workflow.jobs['release-contract-test'];
const step = job.steps.find((value) => value.name === 'Windows 远控发布与安全契约');
const buildWorkflow = parse(readFileSync(new URL('../../.github/workflows/windows-remote-control-build-evidence.yml', import.meta.url), 'utf8'));
const buildStep = Object.values(buildWorkflow.jobs).flatMap((value) => value.steps)
  .find((value) => value.name === 'Build signed installer from checked-out commit');
const nativeContractBudgetMs = 120_000;

// Official 的第三条外部契约（windows-e2e-orchestrator 系列）被 python
// ci-fixture-process receipt 包装器包裹（行首为 python，且行内含未展开的
// `${{ … }}` GitHub 表达式），与 Community 的 3 条直排 npm/node 不同。三条
// 契约统一按“外部契约命令”识别：变异测试把它们替换为确定性 node stub
// （表达式行若原样交给 pwsh 会 ParserError，包装的真实套件也超出共享预算）。
const externalContractCommand = /^(?:npm|node)\b|python scripts\/ci-fixture-process\.py --receipt /u;

test('Windows release step retains every mandatory command and required-gate dependency', () => {
  assert.ok(step);
  assert.equal(step.if, "${{ runner.os == 'Windows' }}");
  assert.equal(step.env.PINVOU_DESKTOP_CONTROLLER_ENABLED, '0');
  assert.equal(step['continue-on-error'], undefined);
  assert.ok(workflow.jobs['required-gate'].needs.includes('release-contract-test'));
  // 顶格 npm/node 直排命令为 2 条；第三条外部契约是 python receipt 包装行。
  assert.equal(step.run.split('\n').filter((line) => /^(?:npm|node)\b/u.test(line)).length, 2);
  assert.equal(step.run.split('\n').filter((line) => externalContractCommand.test(line)).length, 3);
  assert.match(step.run, /npm --prefix remote-control-relay run release:gate/u);
  assert.match(step.run, /windows-e2e-orchestrator-helpers\.test\.js/u);
  assert.match(step.run, /remote_control_feature_gate_contract\.test\.mjs/u);
  const regression = job.steps.find((value) => value.name === 'Windows 原生命令退出码回归');
  assert.ok(job.steps.indexOf(regression) < job.steps.indexOf(step));
  assert.match(regression.run, /python scripts\/ci-fixture-process\.py --receipt .+ -- node --test remote-control-relay\/test\/windows-release-exit-code\.test\.js/u);
  const pathFilter = workflow.jobs.changes.steps.find((value) => value.id === 'filter');
  assert.ok(parse(pathFilter.with.filters).release_contract.some((pattern) =>
    minimatch('remote-control-relay/test/windows-release-exit-code.test.js', pattern,
      { dot: true, nocase: false, nonegate: true })));
  assert.ok(parse(pathFilter.with.filters).release_contract.includes('.github/workflows/windows-remote-control-build-evidence.yml'));
  assert.equal(buildStep.shell, 'pwsh');
  assert.equal(buildStep['working-directory'], 'pinvou3-app');
  assert.equal(buildStep['continue-on-error'], undefined);
  assert.deepEqual(buildStep.run.split('\n').filter((line) => /^(?:npm|node)\b/u.test(line)), [
    'npm ci --prefer-offline --no-audit', 'npm run runtime:windows:init',
    'npm run runtime:windows:onnx-smoke', 'node ../scripts/sync-version.mjs', 'npm run build:nsis',
  ]);
});

for (const [name, target, count] of [['contracts', step, 3], ['protected-build', buildStep, 5]]) {
 for (const shell of ['pwsh', 'powershell.exe']) {
  test(`real ${name} workflow stops on each native failure in ${shell}`, { skip: process.platform !== 'win32', timeout: nativeContractBudgetMs }, () => {
    const deadline = performance.now() + nativeContractBudgetMs;
    for (const failure of [...Array.from({ length: count }, (_, index) => index), null]) {
      let command = 0;
      // Substitute only the external contracts (direct npm/node commands and,
      // on Official, the python receipt-wrapped suite). Execute their actual
      // workflow control flow so deleting a guard fails this regression.
      const script = target.run.split('\n').map((line) => {
        if (!externalContractCommand.test(line)) return line;
        const index = command++;
        const exitCode = failure === index ? 17 : 0;
        return `& $env:PINVOU_NATIVE_EXIT_TEST_NODE -e 'console.log(${index});process.exit(${exitCode})'`;
      }).join('\n')
        // 被保留的行可能携带未展开的 `${{ … }}` GitHub 表达式；它们在真实
        // 运行时由 Actions 渲染，本地契约模拟必须替换为无害字面量，否则
        // pwsh 会把 `${` 当作变量解析直接 ParserError。
        .replace(/\$\{\{[^}]*\}\}/gu, 'GH_RENDERED_EXPRESSION');
      assert.equal(command, count);
      // These are exit-code/control-flow contracts, not shell startup benchmarks.
      // All cases share the existing test budget, including cold PowerShell and
      // native-process startup; a stalled case must not reset that budget.
      const remainingMs = Math.floor(deadline - performance.now());
      assert.ok(remainingMs > 0, 'native exit-code contract exhausted its shared time budget');
      const result = spawnSync(shell, ['-NoProfile', '-NonInteractive', '-Command', script], {
        env: { ...process.env, PINVOU_NATIVE_EXIT_TEST_NODE: process.execPath },
        encoding: 'utf8', windowsHide: true, timeout: remainingMs,
      });
      assert.ifError(result.error);
      assert.equal(result.status, failure === null ? 0 : 17, `failure=${failure}: ${result.stderr}`);
      const expected = Array.from({ length: failure === null ? count : failure + 1 }, (_, index) => String(index));
      assert.deepEqual(result.stdout.trim().split(/\r?\n/u), expected, `later commands must not mask failure=${failure}`);
    }
  });
 }
}
