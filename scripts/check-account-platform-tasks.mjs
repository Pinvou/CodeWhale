#!/usr/bin/env node

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const docsRoot = path.join(repositoryRoot, 'account-platform', 'docs');
const targetPattern = /^(REQ-ID|CORE|SCOPE|ID|ACC|TOK|BILL|PAY|UI|ADM|NFR)-(\d{2,})$/;
const incompleteStatuses = new Set(['部分实现', '未实现', '降级实现']);
const canonicalStatusValues = new Set([...incompleteStatuses, '已实现', '移除', '非当前范围']);
const taskStatuses = new Set(['PENDING', 'IN_PROGRESS', 'DONE', 'BLOCKED_EXTERNAL']);

function expandTargets(field, pattern = targetPattern) {
  assert.ok(field, 'reference field must not be empty');
  const targets = [];
  for (const token of field.split(/,\s*/)) {
    const range = token.match(/^([A-Z]+(?:-[A-Z]+)*)-(\d+)–\1-(\d+)$/);
    if (range) {
      const [, prefix, rawStart, rawEnd] = range;
      const start = Number(rawStart);
      const end = Number(rawEnd);
      assert.ok(Number.isSafeInteger(start) && Number.isSafeInteger(end) && start <= end && end - start < 10_000, `invalid target range ${token}`);
      for (let value = start; value <= end; value += 1) {
        const reference = `${prefix}-${String(value).padStart(rawStart.length, '0')}`;
        assert.match(reference, pattern, `invalid reference ${reference}`);
        targets.push(reference);
      }
      continue;
    }
    assert.match(token, pattern, `invalid target ${token}`);
    targets.push(token);
  }
  assert.equal(new Set(targets).size, targets.length, 'duplicate reference');
  return targets;
}

function canonicalStatuses(markdown) {
  const boundary = markdown.search(/^## 1\./m);
  assert.ok(boundary >= 0, 'canonical status section must end at section 1');
  const section = markdown.slice(0, boundary);
  assert.match(section, /\|\s*target_requirement_id\s*\|\s*当前状态\s*\|/, 'canonical status table missing');
  const statuses = new Map();
  for (const line of section.split(/\r?\n/)) {
    const columns = line.split('|').map((value) => value.trim());
    if (!/^[A-Z]+(?:-ID)?-\d+$/.test(columns[1] ?? '')) continue;
    assert.match(columns[1], targetPattern, `invalid canonical target ${columns[1]}`);
    assert.equal(columns.length, 8, `${columns[1]} must have all six evidence fields`);
    assert.ok(columns.slice(1, -1).every(Boolean), `${columns[1]} has an empty evidence field`);
    assert.equal(statuses.has(columns[1]), false, `duplicate canonical status ${columns[1]}`);
    assert.ok(canonicalStatusValues.has(columns[2]), `${columns[1]} has invalid canonical status ${columns[2]}`);
    statuses.set(columns[1], columns[2]);
  }
  assert.ok(statuses.size > 0, 'canonical status table has no targets');
  return statuses;
}

function requirementTargets(markdown) {
  const targets = new Set();
  for (const line of markdown.split(/\r?\n/)) {
    const firstColumn = line.split('|')[1]?.trim();
    if (targetPattern.test(firstColumn ?? '')) targets.add(firstColumn);
    for (const match of line.matchAll(/\[([A-Z]+(?:-ID)?-\d{2,})\]/g)) {
      assert.match(match[1], targetPattern, `invalid requirement target ${match[1]}`);
      targets.add(match[1]);
    }
  }
  assert.ok(targets.size > 0, 'requirements document has no target identifiers');
  return targets;
}

export function validateTaskCoverage({ statusMarkdown, taskMarkdown, requirementsMarkdown }) {
  const canonical = canonicalStatuses(statusMarkdown);
  const required = requirementTargets(requirementsMarkdown);
  assert.deepEqual([...canonical.keys()].sort(), [...required].sort(), 'canonical targets must exactly cover requirements');
  const expectedIncomplete = new Set([...canonical].filter(([, status]) => incompleteStatuses.has(status)).map(([target]) => target));
  const owners = new Map();
  const tasks = new Map();
  const taskNumbers = [];
  assert.match(taskMarkdown, /\|\s*task_id\s*\|/, 'task table missing');
  for (const line of taskMarkdown.split(/\r?\n/)) {
    const columns = line.split('|').map((value) => value.trim());
    if (!(columns[1] ?? '').startsWith('APT-')) continue;
    assert.match(columns[1], /^APT-\d{3}$/, 'invalid task identifier');
    assert.equal(columns.length, 8, `${columns[1]} must have all six task fields`);
    assert.ok(columns.slice(1, -1).every(Boolean), `${columns[1]} has an empty task field`);
    const taskID = columns[1];
    const taskStatus = columns[6];
    assert.equal(tasks.has(taskID), false, `duplicate task ${taskID}`);
    assert.ok(columns[5] === 'CODE' || columns[5] === 'CODE+ENV', `${taskID} has invalid type ${columns[5]}`);
    const dependsOn = columns[4] === '无' ? [] : expandTargets(columns[4], /^APT-\d{3}$/);
    tasks.set(taskID, { status: taskStatus, dependsOn });
    taskNumbers.push(Number(taskID.slice(4)));
    assert.ok(taskStatuses.has(taskStatus), `${taskID} has invalid status ${taskStatus}`);
    for (const target of expandTargets(columns[3])) {
      assert.ok(canonical.has(target), `${taskID} references unknown target ${target}`);
      assert.equal(owners.has(target), false, `${target} owned by ${owners.get(target)} and ${taskID}`);
      owners.set(target, taskID);
      if (taskStatus === 'DONE') {
        assert.equal(canonical.get(target), '已实现', `${taskID} is DONE but ${target} is not implemented`);
      } else {
        assert.ok(expectedIncomplete.has(target), `${taskID} is ${taskStatus} but ${target} is complete`);
      }
    }
  }
  assert.ok(tasks.size > 0, 'task table has no tasks');
  assert.deepEqual(taskNumbers, Array.from({ length: taskNumbers.length }, (_, index) => index + 1), 'task identifiers must be ordered and contiguous');
  const pendingTargets = [...owners].filter(([, owner]) => tasks.get(owner).status !== 'DONE').map(([target]) => target);
  assert.deepEqual(pendingTargets.sort(), [...expectedIncomplete].sort(), 'tasks must own every incomplete target exactly once');
  const visited = new Set();
  const active = new Set();
  function visit(taskID) {
    if (visited.has(taskID)) return;
    assert.equal(active.has(taskID), false, `dependency cycle at ${taskID}`);
    active.add(taskID);
    const task = tasks.get(taskID);
    for (const dependency of task.dependsOn) {
      assert.ok(tasks.has(dependency), `${taskID} references unknown dependency ${dependency}`);
      if (task.status === 'DONE') assert.equal(tasks.get(dependency).status, 'DONE', `${taskID} completed before dependency ${dependency}`);
      visit(dependency);
    }
    active.delete(taskID);
    visited.add(taskID);
  }
  for (const taskID of tasks.keys()) visit(taskID);
  return { taskCount: tasks.size, incompleteTargetCount: expectedIncomplete.size, targetCount: canonical.size };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [statusMarkdown, taskMarkdown, requirementsMarkdown] = await Promise.all([
    readFile(path.join(docsRoot, 'implementation-status.md'), 'utf8'),
    readFile(path.join(docsRoot, 'implementation-tasks.md'), 'utf8'),
    readFile(path.join(docsRoot, 'requirements.md'), 'utf8'),
  ]);
  const result = validateTaskCoverage({ statusMarkdown, taskMarkdown, requirementsMarkdown });
  console.log(`implementation task coverage passed: ${result.taskCount} tasks own ${result.incompleteTargetCount} incomplete targets; ${result.targetCount} requirement/status targets match`);
}
