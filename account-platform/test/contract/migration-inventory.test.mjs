import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
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

const frozenEntry = (version, name, direction) => ({
  file: `${String(version).padStart(6, '0')}_${name}.${direction}.sql`,
  version,
  sha256: 'a'.repeat(64),
});
const frozenPair = (version, name) => ['down', 'up'].map(direction => frozenEntry(version, name, direction));

test('manifest advances ordinary and supplemental migrations independently without allowing backfills', () => {
  const domains = {
    account: [...frozenPair(93, 'previous'), ...frozenPair(900006, 'supplemental')],
    billing: [...frozenPair(79, 'previous'), ...frozenPair(900022, 'supplemental')],
  };
  const manifest = { domains };
  const actual = structuredClone(domains);
  actual.account.push(...frozenPair(94, 'next'), ...frozenPair(900007, 'next_supplemental'));
  actual.billing.push(...frozenPair(80, 'next'), ...frozenPair(900023, 'next_supplemental'));
  assert.doesNotThrow(() => assertExistingEntriesImmutable(manifest, actual, true));
  assert.throws(() => assertExistingEntriesImmutable(manifest, actual, false), /manifest is missing/);
  for (const [domain, version] of [['account', 92], ['account', 93], ['account', 900005], ['account', 900006], ['billing', 78], ['billing', 900021]]) {
    const backfill = structuredClone(domains);
    backfill[domain].push(...frozenPair(version, 'backfill'));
    assert.throws(() => assertExistingEntriesImmutable(manifest, backfill, true), /must advance/);
  }
});

test('supplemental versions never relax the immutability of any recorded migration', () => {
  const manifest = { domains: { account: [...frozenPair(93, 'ordinary'), ...frozenPair(900006, 'supplemental')] } };
  for (const index of [0, 1, 2, 3]) {
    for (const field of ['file', 'version', 'sha256']) {
      const actual = structuredClone(manifest.domains);
      actual.account[index][field] = field === 'version' ? 95 : 'changed';
      actual.account.push(...frozenPair(94, 'next'));
      assert.throws(() => assertExistingEntriesImmutable(manifest, actual, true), /removed|modified/);
    }
    const deleted = structuredClone(manifest.domains);
    deleted.account.splice(index, 1);
    assert.throws(() => assertExistingEntriesImmutable(manifest, deleted, true), /removed/);
  }
  assert.throws(() => assertExistingEntriesImmutable(manifest, {}, true), /domain was removed/);
});

test('manifest CLI appends complete lower ordinary pairs while preserving recorded SQL and checksums', async () => {
  const directory = await mkdtemp(path.join(tmpdir(), 'pinvou-migration-append-'));
  const scripts = path.join(directory, 'scripts');
  const migrations = path.join(directory, 'account-platform', 'migrations');
  const executable = path.join(scripts, 'account-platform-migration-manifest.mjs');
  const manifestPath = path.join(migrations, 'manifest.json');
  const sql = new Map();
  async function pair(domain, version, name) {
    for (const direction of ['up', 'down']) {
      const file = `${String(version).padStart(6, '0')}_${name}.${direction}.sql`;
      const target = path.join(migrations, domain, file);
      const body = `-- ${domain}/${file}\r\nSELECT ${version};\r\n`;
      sql.set(target, body);
      await writeFile(target, body);
    }
  }
  function cli(command, succeeds = true) {
    const result = spawnSync(process.execPath, [executable, command], { encoding: 'utf8', timeout: 30_000 });
    assert.equal(result.error, undefined);
    if (succeeds) assert.equal(result.status, 0, result.stderr);
    else assert.notEqual(result.status, 0);
    return result;
  }
  try {
    await mkdir(scripts);
    await mkdir(path.join(migrations, 'account'), { recursive: true });
    await mkdir(path.join(migrations, 'billing'));
    await writeFile(executable, await readFile(new URL('../../../scripts/account-platform-migration-manifest.mjs', import.meta.url)));
    await pair('account', 93, 'previous');
    await pair('account', 900006, 'supplemental');
    await pair('billing', 79, 'previous');
    await pair('billing', 900022, 'supplemental');
    cli('init');
    const before = JSON.parse(await readFile(manifestPath, 'utf8'));
    await pair('account', 94, 'next');
    await pair('billing', 80, 'next');
    assert.match(cli('check', false).stderr, /manifest is missing/);
    cli('append');
    cli('check');
    const afterBytes = await readFile(manifestPath, 'utf8');
    const after = JSON.parse(afterBytes);
    assert.deepEqual(after.domains, await inventory(migrations));
    assertExistingEntriesImmutable(before, after.domains, true);
    assert.equal(Object.values(after.domains).flat().length, 12);
    for (const [target, original] of sql) assert.equal(await readFile(target, 'utf8'), original);
    const oldSQL = [...sql.keys()][0];
    await writeFile(oldSQL, 'SELECT 0;');
    assert.match(cli('append', false).stderr, /modified/);
    assert.equal(await readFile(manifestPath, 'utf8'), afterBytes);
    await writeFile(oldSQL, sql.get(oldSQL));
    await pair('account', 92, 'backfill');
    assert.match(cli('append', false).stderr, /must advance/);
    assert.equal(await readFile(manifestPath, 'utf8'), afterBytes);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
