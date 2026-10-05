import assert from 'node:assert/strict';
import { copyFile, mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import test from 'node:test';
import { validateTaskCoverage } from '../check-account-platform-tasks.mjs';

const statusHeader = '| target_requirement_id | 当前状态 | source_commit | tests | deployment_version | known_gaps |\n|---|---|---|---|---|---|';
const taskHeader = '| task_id | 独立交付目标 | 主目标编号 | 依赖 | 类型 | 状态 |\n|---|---|---|---|---|---|';
test('actual development CLI closes the same profile it starts without deleting volumes', async () => {
  const temporaryRoot = await mkdtemp(path.join(os.tmpdir(), 'pinvou-account-cli-test-'));
  const scripts = path.join(temporaryRoot, 'scripts');
  const compose = path.join(temporaryRoot, 'account-platform', 'deploy', 'compose');
  try {
    await mkdir(scripts, { recursive: true });
    await mkdir(compose, { recursive: true });
    await writeFile(path.join(compose, '.env'), '# Owned test fixture; no credentials.\n');
    const script = path.join(scripts, 'account-platform.mjs');
    await copyFile(new URL('../account-platform.mjs', import.meta.url), script);
    const capture = path.join(temporaryRoot, 'docker-argv.json');
    const preload = path.join(scripts, 'capture-docker.mjs');
    await writeFile(preload, `import childProcess from 'node:child_process';
import { writeFileSync } from 'node:fs';
import { syncBuiltinESMExports } from 'node:module';
childProcess.spawnSync = (command, args, options) => {
  if (command !== 'docker' || options.shell !== false) throw new Error('unexpected CLI boundary');
  writeFileSync(process.env.PINVOU_CLI_CAPTURE, JSON.stringify(args));
  return { status: 0 };
};
syncBuiltinESMExports();
`);
    for (const action of ['up', 'down']) {
      const result = spawnSync(process.execPath, ['--import', pathToFileURL(preload).href, script, 'dev', action], {
        encoding: 'utf8', shell: false, timeout: 10_000,
        env: { ...process.env, PINVOU_CLI_CAPTURE: capture },
      });
      assert.equal(result.error, undefined);
      assert.equal(result.status, 0, result.stderr);
      assert.deepEqual(JSON.parse(await readFile(capture, 'utf8')), [
        'compose', '--env-file', path.join(compose, '.env'), '-f', path.join(compose, 'compose.yaml'),
        '--profile', 'real-newapi', action, ...(action === 'up' ? ['--build', '-d', '--wait'] : []),
      ]);
    }
  } finally {
    // Only the root created above is owned; real Compose and Docker are never invoked.
    assert.equal(path.dirname(temporaryRoot), path.resolve(os.tmpdir()));
    assert.ok(path.basename(temporaryRoot).startsWith('pinvou-account-cli-test-'));
    await rm(temporaryRoot, { recursive: true, force: true });
  }
});

function fixture() {
  return {
    requirementsMarkdown: '1. `[ACC-01]` First target.\n2. `[ACC-02]` Second target.',
    statusMarkdown: `${statusHeader}\n| ACC-01 | 未实现 | FIXED | CHECK | NOT_DEPLOYED | gap |\n| ACC-02 | 部分实现 | FIXED | CHECK | NOT_DEPLOYED | gap |\n## 1. Details`,
    taskMarkdown: `${taskHeader}\n| APT-001 | First task | ACC-01 | 无 | CODE | PENDING |\n| APT-002 | Second task | ACC-02 | APT-001 | CODE+ENV | BLOCKED_EXTERNAL |`,
  };
}

test('current requirements, canonical statuses and task dependency queue are actually checked', async () => {
  const root = new URL('../../account-platform/docs/', import.meta.url);
  const [statusMarkdown, taskMarkdown, requirementsMarkdown] = await Promise.all([
    readFile(new URL('implementation-status.md', root), 'utf8'),
    readFile(new URL('implementation-tasks.md', root), 'utf8'),
    readFile(new URL('requirements.md', root), 'utf8'),
  ]);
  const result = validateTaskCoverage({ statusMarkdown, taskMarkdown, requirementsMarkdown });
  assert.ok(result.targetCount > 0);
  assert.ok(result.taskCount > 0);
});

test('valid queue accepts LF/CRLF and completed tasks without losing remaining coverage', () => {
  assert.deepEqual(validateTaskCoverage(fixture()), { taskCount: 2, incompleteTargetCount: 2, targetCount: 2 });
  const completed = fixture();
  completed.statusMarkdown = completed.statusMarkdown.replace('未实现', '已实现');
  completed.taskMarkdown = completed.taskMarkdown.replace('CODE | PENDING', 'CODE | DONE');
  assert.deepEqual(validateTaskCoverage(completed), { taskCount: 2, incompleteTargetCount: 1, targetCount: 2 });
  const crlf = Object.fromEntries(Object.entries(completed).map(([key, text]) => [key, text.replaceAll('\n', '\r\n')]));
  assert.deepEqual(validateTaskCoverage(crlf), validateTaskCoverage(completed));
});

test('range dependencies preserve three-digit task identifiers', () => {
  const ranged = fixture();
  ranged.requirementsMarkdown += '\n3. `[ACC-03]` Third target.';
  ranged.statusMarkdown = ranged.statusMarkdown.replace('## 1.', '| ACC-03 | 未实现 | FIXED | CHECK | NOT_DEPLOYED | gap |\n## 1.');
  ranged.taskMarkdown += '\n| APT-003 | Third task | ACC-03 | APT-001–APT-002 | CODE | PENDING |';
  assert.equal(validateTaskCoverage(ranged).taskCount, 3);
});

test('hyphenated requirement namespace ranges cover both targets without accepting mixed prefixes', () => {
  const ranged = fixture();
  for (const field of Object.keys(ranged)) ranged[field] = ranged[field].replaceAll('ACC-', 'REQ-ID-');
  ranged.taskMarkdown = `${taskHeader}\n| APT-001 | Requirement governance | REQ-ID-01–REQ-ID-02 | 无 | CODE | PENDING |`;
  assert.deepEqual(validateTaskCoverage(ranged), { taskCount: 1, incompleteTargetCount: 2, targetCount: 2 });
  assert.throws(() => validateTaskCoverage({ ...ranged,
    taskMarkdown: ranged.taskMarkdown.replace('REQ-ID-01–REQ-ID-02', 'REQ-ID-01–ACC-02'),
  }), /invalid target/);
});

test('invalid or incomplete document inputs fail closed', async (t) => {
  const cases = [
    ['empty documents', () => ({ statusMarkdown: '', taskMarkdown: '', requirementsMarkdown: '' }), /canonical status section/],
    ['missing canonical boundary', (x) => ({ ...x, statusMarkdown: x.statusMarkdown.replace('## 1. Details', '') }), /canonical status section/],
    ['empty canonical table', (x) => ({ ...x, statusMarkdown: `${statusHeader}\n## 1. Details` }), /no targets/],
    ['empty task table', (x) => ({ ...x, taskMarkdown: taskHeader }), /no tasks/],
    ['missing requirement owner', (x) => ({ ...x, taskMarkdown: x.taskMarkdown.split('\n').slice(0, -1).join('\n') }), /every incomplete target/],
    ['missing requirement status', (x) => ({ ...x, requirementsMarkdown: `${x.requirementsMarkdown}\n[ACC-03] Third target` }), /exactly cover requirements/],
    ['extra canonical target', (x) => ({ ...x, statusMarkdown: x.statusMarkdown.replace('## 1.', '| ACC-03 | 未实现 | FIXED | CHECK | NOT_DEPLOYED | gap |\n## 1.') }), /exactly cover requirements/],
    ['unknown canonical status', (x) => ({ ...x, statusMarkdown: x.statusMarkdown.replace('未实现', 'UNKNOWN') }), /invalid canonical status/],
    ['empty evidence field', (x) => ({ ...x, statusMarkdown: x.statusMarkdown.replace('| FIXED |', '| |') }), /empty evidence field/],
    ['invalid canonical identifier', (x) => ({ ...x, statusMarkdown: x.statusMarkdown.replace('## 1.', '| BAD-01 | 未实现 | FIXED | CHECK | NOT_DEPLOYED | gap |\n## 1.') }), /invalid canonical target/],
    ['duplicate target owner', (x) => ({ ...x, taskMarkdown: x.taskMarkdown.replace('Second task | ACC-02', 'Second task | ACC-01') }), /owned by/],
    ['duplicate task identifier', (x) => ({ ...x, taskMarkdown: x.taskMarkdown.replace('APT-002 |', 'APT-001 |') }), /duplicate task/],
    ['invalid task identifier', (x) => ({ ...x, taskMarkdown: `${x.taskMarkdown}\n| APT-3 | Third | ACC-01 | 无 | CODE | PENDING |` }), /invalid task identifier/],
    ['unknown task status', (x) => ({ ...x, taskMarkdown: x.taskMarkdown.replace('PENDING', 'UNKNOWN') }), /invalid status/],
    ['unknown dependency', (x) => ({ ...x, taskMarkdown: x.taskMarkdown.replace('ACC-02 | APT-001', 'ACC-02 | APT-099') }), /unknown dependency/],
    ['dependency cycle', (x) => ({ ...x, taskMarkdown: x.taskMarkdown.replace('ACC-01 | 无', 'ACC-01 | APT-002') }), /dependency cycle/],
    ['completed incomplete target', (x) => ({ ...x, taskMarkdown: x.taskMarkdown.replace('CODE | PENDING', 'CODE | DONE') }), /not implemented/],
    ['completed before dependency', (x) => ({ ...x, statusMarkdown: x.statusMarkdown.replace('部分实现', '已实现'), taskMarkdown: x.taskMarkdown.replace('BLOCKED_EXTERNAL', 'DONE') }), /completed before dependency/],
  ];
  for (const [name, mutate, expected] of cases) {
    await t.test(name, () => assert.throws(() => validateTaskCoverage(mutate(fixture())), expected));
  }
});
