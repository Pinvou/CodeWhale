import assert from 'node:assert/strict';
import test from 'node:test';
import { copyFile, mkdtemp, mkdir, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

import { createHash } from 'node:crypto';
import { developmentNATSFixture } from '../account-platform/test/fixtures/development-nats.mjs';

import { validateTaskDocuments } from './check-account-platform-tasks.mjs';

test('actual current requirements and remediation ownership are checked together', async () => {
  const root = new URL('../account-platform/docs/', import.meta.url);
  const inputs = await Promise.all(['implementation-status.md', 'implementation-tasks.md', 'requirements.md'].map(name => readFile(new URL(name, root), 'utf8')));
  const result = validateTaskDocuments(...inputs);
  assert.ok(result.taskCount > 0);
  assert.ok(result.activeOwnerCount > 0);
});

const baseStatuses = new Map([
  ['ADM-60', '部分实现'],
  ['UI-49', '部分实现'],
  ['ADM-64', '未实现'],
  ['ADM-65', '已实现'],
]);

const baseTasks = [
  { id: 'APT-001', target: 'ADM-60', dependencies: '无', status: 'DONE' },
  { id: 'APT-002', target: 'UI-49', dependencies: 'APT-001', status: 'DONE' },
  { id: 'APT-003', target: 'ADM-65', dependencies: 'APT-001–APT-002', status: 'DONE' },
  { id: 'APT-004', target: 'ADM-64', dependencies: 'APT-003', status: 'PENDING' },
];

const baseRemediations = [
  { id: 'APT-001-R1', target: 'ADM-60', dependencies: 'APT-001', status: 'PENDING' },
  { id: 'APT-002-R1', target: 'UI-49', dependencies: 'APT-001-R1, APT-002', status: 'PENDING' },
  {
    id: 'APT-003-R1',
    target: '无（ADM-64 前置，不主拥有规范目标）',
    dependencies: 'APT-002-R1, APT-003',
    status: 'PENDING',
  },
];

function cloneRows(rows) {
  return rows.map((row) => ({ ...row }));
}

function buildStatusMarkdown(statuses = baseStatuses) {
  return `# implementation status

| target_requirement_id | 当前状态 | source_commit | tests | deployment_version | known_gaps |
|---|---|---|---|---|---|
${[...statuses].map(([target, status]) => `| ${target} | ${status} | FIXED | TESTED | NOT_DEPLOYED | tracked |`).join('\n')}

## 1. details
`;
}

function buildTaskMarkdown(tasks = baseTasks, remediations = baseRemediations) {
  return `# implementation tasks

| 任务 ID | 任务 | 目标 | 依赖 | 类型 | 状态 |
|---|---|---|---|---|---|
${tasks
  .map(({ id, target, dependencies, status }) => `| ${id} | ${id} task | ${target} | ${dependencies} | CODE | ${status} |`)
  .join('\n')}

### APT-138 前置领域适配整改批次

| 整改批次 | 目标映射 | 状态 | 依赖 | 冻结命令 | 唯一代码所有权 | 原子性与兼容验收 | 独立验证 |
|---|---|---|---|---|---|---|---|
${remediations
  .map(
    ({ id, target, dependencies, status }) =>
      `| ${id} | ${target} | ${status} | ${dependencies} | command | owner | atomic | tests |`,
  )
  .join('\n')}

### APT-138 十九项命令一致性总验收与原子激活
`;
}

function validateFixture({ statuses = baseStatuses, tasks = baseTasks, remediations = baseRemediations, requirements = [...statuses.keys()].map(target => `[${target}] required`).join('\n') } = {}) {
  return validateTaskDocuments(buildStatusMarkdown(statuses), buildTaskMarkdown(tasks, remediations), requirements);
}

test('rejects unknown canonical status and requirements omitted from both ownership tables', () => {
  const statuses = new Map(baseStatuses);
  statuses.set('ADM-65', 'UNKNOWN');
  assert.throws(() => validateFixture({ statuses }), /invalid canonical status/);
  const requirements = [...baseStatuses.keys(), 'ACC-02'].map(target => `[${target}] required`).join('\n');
  assert.throws(() => validateFixture({ requirements }), /canonical targets must exactly cover requirements/);
  assert.throws(() => validateTaskDocuments(buildStatusMarkdown(), buildTaskMarkdown()), /requirements document is required/);
});

test('validates canonical evidence and retains requirement governance identifiers', () => {
  const status = buildStatusMarkdown().replace('FIXED', '');
  const tasks = buildTaskMarkdown();
  const requirements = [...baseStatuses.keys()].map(target => `[${target}] required`).join('\n');
  assert.throws(() => validateTaskDocuments(status, tasks, requirements), /empty evidence field/);
  assert.throws(() => validateTaskDocuments(buildStatusMarkdown().replace('## 1.', '## End.'), tasks, requirements), /must end at section 1/);
  const statuses = new Map(baseStatuses).set('REQ-ID-01', '已实现');
  assert.equal(validateFixture({ statuses }).activeOwnerCount, 3);
});

test('rejects empty task queues and malformed identifiers while retaining CRLF input', () => {
  const requirements = [...baseStatuses.keys()].map(target => `[${target}] required`).join('\n');
  assert.throws(() => validateTaskDocuments(buildStatusMarkdown(), buildTaskMarkdown([], []), requirements), /task table has no tasks/);
  const tasks = cloneRows(baseTasks);
  tasks[3].id = 'APT-4';
  assert.throws(() => validateFixture({ tasks }), /invalid task identifier/);
  assert.deepEqual(validateTaskDocuments(buildStatusMarkdown().replaceAll('\n', '\r\n'), buildTaskMarkdown().replaceAll('\n', '\r\n'), requirements.replaceAll('\n', '\r\n')), validateFixture());
});

test('actual development CLI preserves material and storage preflights and never removes volumes', async (t) => {
  const parent = await realpath(os.tmpdir());
  const temporaryRoot = await mkdtemp(path.join(parent, 'pinvou-account-cli-test-'));
  const scripts = path.join(temporaryRoot, 'scripts');
  const compose = path.join(temporaryRoot, 'account-platform', 'deploy', 'compose');
  const integration = path.join(temporaryRoot, 'account-platform', 'test', 'integration');
  try {
    await Promise.all([mkdir(scripts), mkdir(compose, { recursive: true }), mkdir(integration, { recursive: true })]);
    await writeFile(path.join(compose, '.env'), '# Owned fixture; no credentials.\n');
    // Execute the unchanged CLI and its complete production helper graph.
    for (const name of ['account-platform.mjs', 'account-platform-dev-nats.mjs', 'account-platform-dev-nats-storage.mjs', 'account-platform-dev-nats-runner.mjs']) {
      await copyFile(new URL(`./${name}`, import.meta.url), path.join(scripts, name));
    }
    for (const name of ['workload-credential.mjs', 'compose-test-configuration.mjs']) {
      await copyFile(new URL(`../account-platform/test/integration/${name}`, import.meta.url), path.join(integration, name));
    }
    const capture = path.join(temporaryRoot, 'docker-argv.jsonl');
    const preload = path.join(scripts, 'capture-docker.mjs');
    await writeFile(preload, `import childProcess from 'node:child_process';
import { appendFileSync } from 'node:fs';
import path from 'node:path';
import { syncBuiltinESMExports } from 'node:module';
childProcess.spawnSync = (command, args, options) => {
  if (command !== 'docker' || options.shell !== false) throw new Error('unexpected CLI boundary');
  appendFileSync(process.env.PINVOU_CLI_CAPTURE, JSON.stringify(args) + '\\n');
  const scenario = process.env.PINVOU_CLI_SCENARIO;
  const data = options.env.DEV_NATS_DATA_DIRECTORY;
  const project = 'controlled-task-cli';
  let stdout;
  if (args[0] === 'context') stdout = JSON.stringify(scenario === 'remote' ? 'ssh://fixture.invalid' : 'unix:///var/run/docker.sock');
  else if (args[0] === 'compose' && args.includes('config')) stdout = JSON.stringify({ name: project, services: { nats: { volumes: [{ type: 'bind', target: '/data', source: data }] } } });
  else if (args[0] === 'volume' && args[1] === 'ls') stdout = scenario === 'old-volume' ? project + '_nats-data' : '';
  else if (args[0] === 'compose' && args.includes('ps')) stdout = ['same-bind', 'different-bind'].includes(scenario) ? 'a'.repeat(64) : '';
  else if (args[0] === 'inspect') stdout = JSON.stringify([{ Type: 'bind', Destination: '/data', Source: scenario === 'different-bind' ? path.join(data, 'other') : data }]);
  else if (args[0] === 'compose' && (args.includes('up') || args.includes('down'))) stdout = '';
  else throw new Error('unexpected Docker command');
  return { status: 0, stdout };
};
syncBuiltinESMExports();
`);
    const prefix = ['compose', '--env-file', path.join(compose, '.env'), '-f', path.join(compose, 'compose.yaml'), '-f', path.join(compose, 'compose.dev-nats.yaml'), '--profile', 'real-newapi'];
    const context = ['context', 'inspect', '--format', '{{json .Endpoints.docker.Host}}'];
    const volumes = [
      ['volume', 'ls', '-q', '--filter', 'label=com.docker.compose.project=controlled-task-cli', '--filter', 'label=com.docker.compose.volume=nats-data'],
      ['volume', 'ls', '-q', '--filter', 'name=controlled-task-cli_nats-data'],
    ];
    const preflight = [context, [...prefix, 'config', '--format', 'json'], ...volumes, [...prefix, 'ps', '-aq', 'nats']];
    const inspect = ['inspect', '--format', '{{json .Mounts}}', 'a'.repeat(64)];
    for (const scenario of ['new', 'same-bind', 'down', 'down-missing-material', 'bad-material', 'missing-receipt', 'old-volume', 'different-bind', 'remote', 'host-override']) {
      await t.test(scenario, async () => {
        const directory = path.join(temporaryRoot, scenario);
        await mkdir(directory, { mode: 0o700 });
        // Reuse the offline assembly fixture: ephemeral TLS material and syntax
        // identities only, not issued credentials or real broker cutover proof.
        const fixture = developmentNATSFixture(directory);
        const receipt = Buffer.from(JSON.stringify({
          schema: 'approval-consumer-cutover-receipt-v1', schema_version: 1,
          identity_sha256: fixture.config.currentPublicKeysSHA256,
          manifest_sha256: fixture.config.manifestSHA256,
          stream: 'ACCOUNT_PLATFORM', control: 'ADMIN_APPROVAL',
          captured_mode: 'QUIESCING', captured_relay_state: 'FENCED',
          control_version: '3', approval_epoch: '2', authorization_relay_epoch: '2',
          captured_at: new Date().toISOString(), high_watermark: '0',
          billing_starts: [{ durable: 'fixture-only-durable', start_sequence: '1' }],
        }));
        await writeFile(fixture.config.receiptFile, receipt, { mode: 0o600 });
        fixture.config.receiptSHA256 = createHash('sha256').update(receipt).digest('hex');
        if (scenario === 'bad-material') fixture.config.authorizationSHA256 = '0'.repeat(64);
        fixture.save();
        if (scenario === 'missing-receipt' || scenario === 'down-missing-material') await rm(fixture.config.receiptFile);
        if (scenario === 'down-missing-material') await rm(fixture.config.caFile);
        await writeFile(capture, '');
        const env = { ...process.env, ...fixture.environment, PINVOU_CLI_CAPTURE: capture, PINVOU_CLI_SCENARIO: scenario };
        // A developer's Docker host override cannot redirect this inert fixture.
        for (const key of Object.keys(env)) if (key.toUpperCase() === 'DOCKER_HOST') delete env[key];
        if (scenario === 'host-override') env.DOCKER_HOST = 'tcp://fixture.invalid:2376';
        const action = scenario.startsWith('down') ? 'down' : 'up';
        const result = spawnSync(process.execPath, ['--import', pathToFileURL(preload).href, path.join(scripts, 'account-platform.mjs'), 'dev', action], {
          encoding: 'utf8', shell: false, timeout: 10_000, windowsHide: true, env,
        });
        assert.equal(result.error, undefined);
        const calls = (await readFile(capture, 'utf8')).trim().split('\n').filter(Boolean).map(line => JSON.parse(line));
        const success = ['new', 'same-bind', 'down', 'down-missing-material'].includes(scenario);
        assert.equal(result.status, success ? 0 : 1, result.stderr);
        assert.ok(calls.every(args => !args.some(value => ['--volumes', '-v', 'rm', 'prune'].includes(value))), 'developer CLI must never remove volumes or storage');
        let expected;
        if (action === 'down') expected = [[...prefix, 'down']];
        else if (scenario === 'new') expected = [...preflight, [...prefix, 'up', '--build', '-d', '--wait']];
        else if (scenario === 'same-bind') expected = [...preflight, inspect, [...prefix, 'up', '--build', '-d', '--wait']];
        else if (scenario === 'old-volume') expected = preflight.slice(0, 3);
        else if (scenario === 'different-bind') expected = [...preflight, inspect];
        else if (scenario === 'remote') expected = [context];
        else expected = [];
        assert.deepEqual(calls, expected, `${scenario} must retain its exact read-only preflight and compose overlay`);
        if (!success) assert.match(result.stderr, ['bad-material', 'missing-receipt'].includes(scenario) ? /DEV_NATS_MATERIALS_INVALID/ : /DEV_NATS_EXISTING_STORAGE_UNPROVEN/);
      });
    }
  } finally {
    assert.equal(path.dirname(temporaryRoot), parent);
    assert.ok(path.basename(temporaryRoot).startsWith('pinvou-account-cli-test-'));
    await rm(temporaryRoot, { recursive: true, force: true });
  }
});

test('accepts DONE history with one active remediation owner per reopened target', () => {
  assert.deepEqual(validateFixture(), {
    taskCount: 4,
    remediationCount: 3,
    activeOwnerCount: 3,
    completedOwnerCount: 3,
  });
});

test('rejects duplicate active remediation owners', () => {
  const remediations = cloneRows(baseRemediations);
  remediations[0].target = 'ADM-60, UI-49';
  assert.throws(() => validateFixture({ remediations }), /duplicate remediation owners/);
});

test('rejects duplicate DONE remediation owners', () => {
  const statuses = new Map(baseStatuses);
  statuses.set('ADM-60', '已实现');
  statuses.set('UI-49', '已实现');
  const remediations = cloneRows(baseRemediations);
  remediations[0] = { ...remediations[0], target: 'UI-49', status: 'DONE' };
  remediations[1] = { ...remediations[1], target: 'UI-49', status: 'DONE' };
  assert.throws(() => validateFixture({ statuses, remediations }), /duplicate remediation owners/);
});

test('rejects a reopened target without an active remediation owner', () => {
  const remediations = cloneRows(baseRemediations);
  remediations[1].target = '无（ADM-64 前置，不主拥有规范目标）';
  assert.throws(() => validateFixture({ remediations }), /lacks an active remediation owner/);
});

test('rejects an active remediation for a complete target', () => {
  const statuses = new Map(baseStatuses);
  statuses.set('UI-49', '已实现');
  assert.throws(() => validateFixture({ statuses }), /active target UI-49 must be 部分实现/);
});

test('rejects a DONE remediation whose target is not synchronized to complete', () => {
  const remediations = cloneRows(baseRemediations);
  remediations[0].status = 'DONE';
  assert.throws(() => validateFixture({ remediations }), /is DONE but ADM-60 remains incomplete/);
});

test('rejects a remediation dependency cycle', () => {
  const remediations = cloneRows(baseRemediations);
  remediations[0].dependencies = 'APT-002-R1, APT-001';
  assert.throws(() => validateFixture({ remediations }), /task dependency cycle/);
});

function activationFixture() {
  const statuses = new Map();
  const tasks = Array.from({ length: 138 }, (_, index) => {
    const number = index + 1;
    const id = `APT-${String(number).padStart(3, '0')}`;
    const target = number === 138 ? 'ADM-64' : `TOK-${1000 + number}`;
    statuses.set(target, '已实现');
    return {
      id,
      target,
      dependencies: number === 1 ? '无' : `APT-${String(number - 1).padStart(3, '0')}`,
      status: 'DONE',
    };
  });
  const remediations = [
    { id: 'APT-122-R2', target: 'TOK-1122', dependencies: 'APT-122', status: 'DONE' },
    { id: 'APT-123-R2', target: 'TOK-1123', dependencies: 'APT-123, APT-122-R2', status: 'DONE' },
    { id: 'APT-125-R1', target: 'TOK-1125', dependencies: 'APT-125, APT-123-R2', status: 'DONE' },
  ];
  return { statuses, tasks, remediations };
}

test('rejects APT-138 progress while each frozen activation prerequisite remains unfinished', () => {
  for (let index = 0; index < 3; index += 1) {
    const fixture = activationFixture();
    fixture.tasks[137].status = 'IN_PROGRESS';
    fixture.statuses.set('ADM-64', '未实现');
    // Later batches cannot complete before the selected prerequisite either.
    for (const remediation of fixture.remediations.slice(index)) {
      remediation.status = 'PENDING';
      fixture.statuses.set(remediation.target, '部分实现');
    }
    assert.throws(() => validateFixture(fixture), /APT-138 must remain PENDING until remediations are DONE/);
  }
});

test('retains completed activation history with an owned post-completion correction', () => {
  const fixture = activationFixture();
  fixture.statuses.set('ADM-64', '部分实现');
  fixture.remediations.push({ id: 'APT-138-R1', target: 'ADM-64', dependencies: 'APT-138', status: 'PENDING' });
  assert.equal(validateFixture(fixture).activeOwnerCount, 1);
  assert.equal(validateFixture(fixture).remediationCount, 4);
});

test('post-completion correction still cannot close an incomplete target', () => {
  const fixture = activationFixture();
  fixture.statuses.set('ADM-64', '部分实现');
  fixture.remediations.push({ id: 'APT-138-R1', target: 'ADM-64', dependencies: 'APT-138', status: 'DONE' });
  assert.throws(
    () => validateFixture(fixture),
    /APT-138-R1 is DONE but ADM-64 remains incomplete/,
  );
});

test('post-completion correction still cannot close before its implementation dependency', () => {
  const fixture = activationFixture();
  fixture.statuses.set('ADM-67', '部分实现');
  fixture.tasks.push({ id: 'APT-139', target: 'ADM-67', dependencies: 'APT-138', status: 'IN_PROGRESS' });
  fixture.remediations.push({
    id: 'APT-138-R1', target: 'ADM-64', dependencies: 'APT-138, APT-139', status: 'DONE',
  });
  assert.throws(
    () => validateFixture(fixture),
    /APT-138-R1 is DONE but depends on unfinished APT-139/,
  );
});
