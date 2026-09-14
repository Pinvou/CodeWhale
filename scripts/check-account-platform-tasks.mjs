#!/usr/bin/env node

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const docsRoot = path.join(repositoryRoot, 'account-platform', 'docs');
const targetPattern = /^(CORE|SCOPE|ID|ACC|TOK|BILL|PAY|UI|ADM|NFR)-(\d+)$/;
const incompleteStatuses = new Set(['部分实现', '未实现', '降级实现']);
const taskStatuses = new Set(['PENDING', 'IN_PROGRESS', 'IN_REVIEW', 'DONE', 'BLOCKED_EXTERNAL']);
const remediationStatuses = new Set(['PENDING', 'IN_PROGRESS', 'IN_REVIEW', 'DONE']);
const remediationPattern = /^APT-\d{3}-R\d+$/;

function unquoteCell(value) {
  const trimmed = value.trim();
  return trimmed.startsWith('`') && trimmed.endsWith('`') ? trimmed.slice(1, -1) : trimmed;
}

function expandTargets(field) {
  const targets = [];
  for (const token of field.split(/,\s*/)) {
    const range = token.match(/^([A-Z]+)-(\d+)–\1-(\d+)$/);
    if (range) {
      const [, prefix, rawStart, rawEnd] = range;
      const start = Number(rawStart);
      const end = Number(rawEnd);
      assert.ok(start <= end, `invalid target range ${token}`);
      for (let value = start; value <= end; value += 1) {
        targets.push(`${prefix}-${String(value).padStart(2, '0')}`);
      }
      continue;
    }
    assert.match(token, targetPattern, `invalid target ${token}`);
    targets.push(token);
  }
  return targets;
}

function expandTaskDependencies(field) {
  if (field === '无') return [];
  const dependencies = [];
  for (const token of field.split(/,\s*/)) {
    const range = token.match(/^APT-(\d{3})–APT-(\d{3})$/);
    if (range) {
      const start = Number(range[1]);
      const end = Number(range[2]);
      assert.ok(start <= end, `invalid task dependency range ${token}`);
      for (let value = start; value <= end; value += 1) {
        dependencies.push(`APT-${String(value).padStart(3, '0')}`);
      }
      continue;
    }
    assert.match(token, /^APT-\d{3}$/, `invalid task dependency ${token}`);
    dependencies.push(token);
  }
  return dependencies;
}

function expandRemediationDependencies(field) {
  if (field === '无') return [];
  return field.split(/,\s*/).map((token) => {
    assert.match(token, /^(APT-\d{3}|APT-\d{3}-R\d+)$/, `invalid remediation dependency ${token}`);
    return token;
  });
}

function canonicalStatuses(markdown) {
  const statuses = new Map();
  for (const line of markdown.slice(0, markdown.indexOf('\n## 1.')).split(/\r?\n/)) {
    const columns = line.split('|').map((value) => value.trim());
    if (columns.length < 4 || !targetPattern.test(columns[1])) continue;
    assert.equal(statuses.has(columns[1]), false, `duplicate canonical status ${columns[1]}`);
    statuses.set(columns[1], columns[2]);
  }
  return statuses;
}

export function validateTaskDocuments(statusMarkdown, taskMarkdown) {
  const canonical = canonicalStatuses(statusMarkdown);
  const expectedIncomplete = new Set(
    [...canonical].filter(([, status]) => incompleteStatuses.has(status)).map(([target]) => target),
  );
  const owners = new Map();
  const activeOwners = new Map();
  const remediationTargetOwners = new Map();
  const remediationOwners = new Map();
  const taskNumbers = [];
  const tasks = new Map();
  const remediations = new Map();

  const remediationSectionHeading = '### APT-138 前置领域适配整改批次';
  const remediationSectionEndHeading = '### APT-138 十九项命令一致性总验收与原子激活';
  const remediationSectionStart = taskMarkdown.indexOf(remediationSectionHeading);
  const remediationSectionEnd = taskMarkdown.indexOf(remediationSectionEndHeading, remediationSectionStart);
  assert.notEqual(remediationSectionStart, -1, `missing remediation section ${remediationSectionHeading}`);
  assert.ok(remediationSectionEnd > remediationSectionStart, `missing remediation section end ${remediationSectionEndHeading}`);
  const remediationMarkdown = taskMarkdown.slice(remediationSectionStart, remediationSectionEnd);

  for (const line of taskMarkdown.split(/\r?\n/)) {
    const columns = line.split('|').map((value) => value.trim());
    if (!/^APT-\d{3}$/.test(columns[1] ?? '')) continue;
    const taskID = columns[1];
    const taskStatus = columns[6];
    taskNumbers.push(Number(taskID.slice(4)));
    assert.ok(taskStatuses.has(taskStatus), `${taskID} has invalid status ${taskStatus}`);
    assert.equal(tasks.has(taskID), false, `duplicate task ${taskID}`);
    const targets = expandTargets(columns[3]);
    tasks.set(taskID, {
      status: taskStatus,
      dependencies: expandTaskDependencies(columns[4]),
      targets,
    });
    for (const target of targets) {
      assert.ok(canonical.has(target), `${taskID} references unknown target ${target}`);
      assert.equal(owners.has(target), false, `${target} owned by ${owners.get(target)} and ${taskID}`);
      owners.set(target, taskID);
    }
  }

  assert.deepEqual(taskNumbers, Array.from({ length: taskNumbers.length }, (_, index) => index + 1));

  for (const line of remediationMarkdown.split(/\r?\n/)) {
    const columns = line.split('|').map((value) => value.trim());
    const remediationID = unquoteCell(columns[1] ?? '');
    if (!remediationPattern.test(remediationID)) continue;
    const targetField = unquoteCell(columns[2] ?? '');
    const status = unquoteCell(columns[3] ?? '');
    const dependencyField = unquoteCell(columns[4] ?? '');
    assert.ok(remediationStatuses.has(status), `${remediationID} has invalid remediation status ${status}`);
    assert.equal(remediations.has(remediationID), false, `duplicate remediation ${remediationID}`);
    const targets = targetField.startsWith('无（') ? [] : expandTargets(targetField);
    if (targets.length === 0) {
      assert.match(targetField, /^无（ADM-64 前置，不主拥有规范目标）$/, `${remediationID} has invalid empty target mapping`);
    }
    const dependencies = expandRemediationDependencies(dependencyField);
    assert.equal(new Set(targets).size, targets.length, `${remediationID} has duplicate target mappings`);
    assert.equal(new Set(dependencies).size, dependencies.length, `${remediationID} has duplicate dependencies`);
    remediations.set(remediationID, {
      status,
      dependencies,
      targets,
    });
  }

  for (const [remediationID, remediation] of remediations) {
    const parentID = remediationID.replace(/-R\d+$/, '');
    assert.ok(tasks.has(parentID), `${remediationID} references missing historical parent ${parentID}`);
    assert.equal(tasks.get(parentID).status, 'DONE', `${remediationID} parent ${parentID} must retain DONE history`);
    for (const target of remediation.targets) {
      assert.ok(canonical.has(target), `${remediationID} references unknown target ${target}`);
      assert.ok(owners.has(target), `${remediationID} target ${target} has no historical task owner`);
      assert.equal(tasks.get(owners.get(target)).status, 'DONE', `${remediationID} target ${target} historical owner must be DONE`);
      assert.equal(
        remediationTargetOwners.has(target),
        false,
        `${target} has duplicate remediation owners ${remediationTargetOwners.get(target)} and ${remediationID}`,
      );
      remediationTargetOwners.set(target, remediationID);
      if (remediation.status === 'DONE') {
        assert.equal(expectedIncomplete.has(target), false, `${remediationID} is DONE but ${target} remains incomplete`);
      } else {
        assert.equal(canonical.get(target), '部分实现', `${remediationID} active target ${target} must be 部分实现`);
        assert.equal(
          remediationOwners.has(target),
          false,
          `${target} has duplicate active remediation owners ${remediationOwners.get(target)} and ${remediationID}`,
        );
        remediationOwners.set(target, remediationID);
      }
    }
  }

  for (const [taskID, task] of tasks) {
    for (const target of task.targets) {
      if (task.status === 'DONE') {
        if (expectedIncomplete.has(target)) {
          assert.ok(remediationOwners.has(target), `${taskID} is DONE but ${target} lacks an active remediation owner`);
        }
      } else {
        assert.ok(expectedIncomplete.has(target), `${taskID} is ${task.status} but ${target} is complete`);
        assert.equal(remediationOwners.has(target), false, `${target} cannot have both an active task and remediation owner`);
        activeOwners.set(target, taskID);
      }
    }
    for (const dependency of task.dependencies) {
      assert.ok(tasks.has(dependency), `${taskID} references unknown dependency ${dependency}`);
      assert.notEqual(dependency, taskID, `${taskID} depends on itself`);
      const dependencyStatus = tasks.get(dependency).status;
      if (task.status === 'DONE' || task.status === 'BLOCKED_EXTERNAL') {
        assert.ok(
          dependencyStatus === 'DONE' || dependencyStatus === 'BLOCKED_EXTERNAL',
          `${taskID} is ${task.status} but depends on unfinished ${dependency} (${dependencyStatus})`,
        );
      }
    }
  }

  for (const [remediationID, remediation] of remediations) {
    for (const dependency of remediation.dependencies) {
      assert.notEqual(dependency, remediationID, `${remediationID} depends on itself`);
      assert.ok(tasks.has(dependency) || remediations.has(dependency), `${remediationID} references unknown dependency ${dependency}`);
      if (remediation.status !== 'PENDING') {
        const dependencyStatus = tasks.get(dependency)?.status ?? remediations.get(dependency)?.status;
        assert.ok(
          dependencyStatus === 'DONE' || dependencyStatus === 'BLOCKED_EXTERNAL',
          `${remediationID} is ${remediation.status} but depends on unfinished ${dependency} (${dependencyStatus})`,
        );
      }
    }
  }

  if (tasks.has('APT-138')) {
    // Only these frozen pre-activation batches gate APT-138. A later correction
    // must retain its parent's DONE history while reopening the affected target.
    const activationPrerequisites = new Set(['APT-122-R2', 'APT-123-R2', 'APT-125-R1']);
    const unfinishedRemediations = [...remediations].filter(
      ([id, remediation]) => activationPrerequisites.has(id) && remediation.status !== 'DONE',
    );
    if (unfinishedRemediations.length > 0) {
      assert.equal(
        tasks.get('APT-138').status,
        'PENDING',
        `APT-138 must remain PENDING until remediations are DONE: ${unfinishedRemediations
          .map(([remediationID]) => remediationID)
          .join(', ')}`,
      );
    }
  }

  const formalActiveOwnerCount = activeOwners.size;

  const allTasks = new Map([...tasks, ...remediations]);
  const visiting = new Set();
  const visited = new Set();
  function visitTask(taskID, taskPath = []) {
    if (visiting.has(taskID)) {
      const cycleStart = taskPath.indexOf(taskID);
      assert.fail(`task dependency cycle: ${[...taskPath.slice(cycleStart), taskID].join(' -> ')}`);
    }
    if (visited.has(taskID)) return;
    visiting.add(taskID);
    for (const dependency of allTasks.get(taskID).dependencies) {
      visitTask(dependency, [...taskPath, taskID]);
    }
    visiting.delete(taskID);
    visited.add(taskID);
  }
  for (const taskID of allTasks.keys()) visitTask(taskID);

  for (const [target, remediationID] of remediationOwners) {
    assert.equal(canonical.get(target), '部分实现', `${target} owned by ${remediationID} must remain 部分实现`);
    activeOwners.set(target, remediationID);
  }
  assert.equal(activeOwners.size, expectedIncomplete.size);
  assert.deepEqual([...activeOwners.keys()].sort(), [...expectedIncomplete].sort());
  return {
    taskCount: taskNumbers.length,
    remediationCount: remediations.size,
    activeOwnerCount: activeOwners.size,
    completedOwnerCount: owners.size - formalActiveOwnerCount,
  };
}

if (process.argv[1] && pathToFileURL(path.resolve(process.argv[1])).href === import.meta.url) {
  const [statusMarkdown, taskMarkdown] = await Promise.all([
    readFile(path.join(docsRoot, 'implementation-status.md'), 'utf8'),
    readFile(path.join(docsRoot, 'implementation-tasks.md'), 'utf8'),
  ]);
  const result = validateTaskDocuments(statusMarkdown, taskMarkdown);
  console.log(
    `implementation task coverage passed: ${result.taskCount} tasks and ${result.remediationCount} remediations own ${result.activeOwnerCount} incomplete targets; ${result.completedOwnerCount} completed targets retained for traceability`,
  );
}
