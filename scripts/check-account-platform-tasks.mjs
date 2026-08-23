#!/usr/bin/env node

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const docsRoot = path.join(repositoryRoot, 'account-platform', 'docs');
const targetPattern = /^(CORE|SCOPE|ID|ACC|TOK|BILL|PAY|UI|ADM|NFR)-(\d+)$/;
const incompleteStatuses = new Set(['部分实现', '未实现', '降级实现']);
const taskStatuses = new Set(['PENDING', 'IN_PROGRESS', 'DONE', 'BLOCKED_EXTERNAL']);

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

const [statusMarkdown, taskMarkdown] = await Promise.all([
  readFile(path.join(docsRoot, 'implementation-status.md'), 'utf8'),
  readFile(path.join(docsRoot, 'implementation-tasks.md'), 'utf8'),
]);
const canonical = canonicalStatuses(statusMarkdown);
const expectedIncomplete = new Set(
  [...canonical].filter(([, status]) => incompleteStatuses.has(status)).map(([target]) => target),
);
const owners = new Map();
const taskNumbers = [];

for (const line of taskMarkdown.split(/\r?\n/)) {
  const columns = line.split('|').map((value) => value.trim());
  if (!/^APT-\d{3}$/.test(columns[1] ?? '')) continue;
  const taskID = columns[1];
  const taskStatus = columns[6];
  taskNumbers.push(Number(taskID.slice(4)));
  assert.ok(taskStatuses.has(taskStatus), `${taskID} has invalid status ${taskStatus}`);
  for (const target of expandTargets(columns[3])) {
    assert.ok(canonical.has(target), `${taskID} references unknown target ${target}`);
    assert.equal(owners.has(target), false, `${target} owned by ${owners.get(target)} and ${taskID}`);
    owners.set(target, taskID);
    if (taskStatus === 'DONE') {
      assert.equal(expectedIncomplete.has(target), false, `${taskID} is DONE but ${target} remains incomplete`);
    } else {
      assert.ok(expectedIncomplete.has(target), `${taskID} is ${taskStatus} but ${target} is complete`);
    }
  }
}

assert.deepEqual(taskNumbers, Array.from({ length: taskNumbers.length }, (_, index) => index + 1));
assert.equal(owners.size, expectedIncomplete.size);
assert.deepEqual([...owners.keys()].sort(), [...expectedIncomplete].sort());
console.log(`implementation task coverage passed: ${taskNumbers.length} tasks own ${owners.size} incomplete targets`);
