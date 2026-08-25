import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const internalRoot = path.resolve(import.meta.dirname, '../../internal');

async function goFiles(directory) {
  const files = [];
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const target = path.join(directory, entry.name);
    if (entry.isDirectory()) files.push(...await goFiles(target));
    else if (entry.isFile() && entry.name.endsWith('.go')) files.push(target);
  }
  return files;
}

test('PostgreSQL interval arithmetic explicitly types timestamp parameters', async () => {
  const unsafeParameterArithmetic = /\$\d+\s*-\s*interval\b/gi;
  const violations = [];

  for (const file of await goFiles(internalRoot)) {
    const source = await readFile(file, 'utf8');
    for (const match of source.matchAll(unsafeParameterArithmetic)) {
      const prefix = source.slice(Math.max(0, match.index - 32), match.index);
      if (!/::(?:timestamp with time zone|timestamptz)\s*$/i.test(prefix)) {
        violations.push(`${path.relative(internalRoot, file)}: ${match[0]}`);
      }
    }
  }

  assert.deepEqual(violations, [], 'untyped PostgreSQL timestamp parameters can be inferred as interval');
});
