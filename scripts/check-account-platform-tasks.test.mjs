import assert from 'node:assert/strict';
import test from 'node:test';

import { validateTaskDocuments } from './check-account-platform-tasks.mjs';

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

| target_requirement_id | 当前状态 |
|---|---|
${[...statuses].map(([target, status]) => `| ${target} | ${status} |`).join('\n')}

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

function validateFixture({ statuses = baseStatuses, tasks = baseTasks, remediations = baseRemediations } = {}) {
  return validateTaskDocuments(buildStatusMarkdown(statuses), buildTaskMarkdown(tasks, remediations));
}

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

test('rejects APT-138 progress while any remediation remains unfinished', () => {
  const statuses = new Map([
    ['ADM-60', '部分实现'],
    ['UI-49', '部分实现'],
    ['ADM-64', '未实现'],
  ]);
  const tasks = Array.from({ length: 138 }, (_, index) => {
    const number = index + 1;
    const id = `APT-${String(number).padStart(3, '0')}`;
    if (number === 1) return { id, target: 'ADM-60', dependencies: '无', status: 'DONE' };
    if (number === 2) return { id, target: 'UI-49', dependencies: 'APT-001', status: 'DONE' };
    if (number === 3) return { id, target: 'TOK-1003', dependencies: 'APT-002', status: 'DONE' };
    if (number === 138) return { id, target: 'ADM-64', dependencies: 'APT-137', status: 'IN_PROGRESS' };
    return {
      id,
      target: `TOK-${1000 + number}`,
      dependencies: `APT-${String(number - 1).padStart(3, '0')}`,
      status: 'DONE',
    };
  });
  for (const { target } of tasks.slice(2, -1)) statuses.set(target, '已实现');
  const remediations = [
    { id: 'APT-001-R1', target: 'ADM-60', dependencies: 'APT-001', status: 'PENDING' },
    { id: 'APT-002-R1', target: 'UI-49', dependencies: 'APT-001-R1, APT-002', status: 'PENDING' },
  ];
  assert.throws(
    () => validateFixture({ statuses, tasks, remediations }),
    /APT-138 must remain PENDING until remediations are DONE/,
  );
});
