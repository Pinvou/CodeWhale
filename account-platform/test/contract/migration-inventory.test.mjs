import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { inventory, assertExistingEntriesImmutable } from '../../../scripts/account-platform-migration-manifest.mjs';

const root = path.resolve(import.meta.dirname, '../../migrations');

test('first migration manifest freezes the entire actual up/down inventory without changing SQL', async () => {
  const manifest = JSON.parse(await readFile(path.join(root, 'manifest.json'), 'utf8'));
  assert.equal(manifest.schema_version, 1);
  assert.equal(manifest.algorithm, 'sha256-normalized-lf');
  const actual = await inventory();
  assert.deepEqual(manifest.domains, actual);
  assertExistingEntriesImmutable(manifest, actual, false);
  let count = 0;
  for (const [domain, rows] of Object.entries(actual)) {
    const sql = (await readdir(path.join(root, domain))).filter(name => name.endsWith('.sql')).sort();
    assert.deepEqual(rows.map(row => row.file).sort(), sql);
    for (const row of rows) {
      const source = await readFile(path.join(root, domain, row.file), 'utf8');
      assert.equal(row.sha256, createHash('sha256').update(source.replace(/\r\n?/g, '\n')).digest('hex'));
      count += 1;
    }
  }
  assert.ok(count > 0);
});

test('inventory refuses missing pairs, duplicate numeric versions and unsafe version numbers', async () => {
  const directory = await mkdtemp(path.join(tmpdir(), 'pinvou-migration-inventory-'));
  const domain = path.join(directory, 'token');
  await mkdir(domain);
  try {
    await writeFile(path.join(domain, '000001_first.up.sql'), 'SELECT 1;');
    await assert.rejects(inventory(directory), /must have up and down/);
    await writeFile(path.join(domain, '000001_first.down.sql'), 'SELECT 1;');
    await writeFile(path.join(domain, '000001_second.up.sql'), 'SELECT 1;');
    await assert.rejects(inventory(directory), /duplicate migration version/);
    await rm(path.join(domain, '000001_second.up.sql'));
    await writeFile(path.join(domain, '9007199254740993_overflow.up.sql'), 'SELECT 1;');
    await assert.rejects(inventory(directory), /invalid migration version/);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
