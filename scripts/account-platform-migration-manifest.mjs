#!/usr/bin/env node

import { createHash } from 'node:crypto';
import { existsSync } from 'node:fs';
import { readFile, readdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const migrationsRoot = path.join(repositoryRoot, 'account-platform', 'migrations');
const manifestPath = path.join(migrationsRoot, 'manifest.json');
const migrationName = /^(\d+)_([a-z0-9_]+)\.(up|down)\.sql$/;

function digest(value) {
  return createHash('sha256').update(value.replace(/\r\n?/g, '\n')).digest('hex');
}

export async function inventory(root = migrationsRoot) {
  const domains = {};
  for (const domain of (await readdir(root, { withFileTypes: true }))
    .filter((entry) => entry.isDirectory()).map((entry) => entry.name).sort()) {
    const directory = path.join(root, domain);
    const files = (await readdir(directory)).filter((name) => migrationName.test(name)).sort();
    const pairs = new Map();
    const namesByVersion = new Map();
    domains[domain] = [];
    for (const file of files) {
      const [, rawVersion, name, direction] = file.match(migrationName);
      const version = Number(rawVersion);
      if (!Number.isSafeInteger(version) || version < 1) throw new Error(`${domain}/${file} has an invalid migration version`);
      const versionName = namesByVersion.get(version);
      if (versionName && versionName !== name) throw new Error(`${domain} has duplicate migration version ${version}`);
      namesByVersion.set(version, name);
      const key = `${rawVersion}_${name}`;
      const pair = pairs.get(key) || new Set();
      pair.add(direction);
      pairs.set(key, pair);
      domains[domain].push({
        file,
        version,
        sha256: digest(await readFile(path.join(directory, file), 'utf8')),
      });
    }
    for (const [name, directions] of pairs) {
      if (!directions.has('up') || !directions.has('down')) throw new Error(`${domain}/${name} must have up and down scripts`);
    }
    const versions = [...new Set(domains[domain].map((entry) => entry.version))];
    if (versions.some((version, index) => index > 0 && version <= versions[index - 1])) {
      throw new Error(`${domain} migration versions are not strictly increasing`);
    }
  }
  return domains;
}

function validateManifestShape(manifest) {
  if (manifest.schema_version !== 1 || manifest.algorithm !== 'sha256-normalized-lf') {
    throw new Error('unsupported migration manifest format');
  }
  if (!manifest.domains || Array.isArray(manifest.domains)) throw new Error('migration manifest domains are required');
}

export function assertExistingEntriesImmutable(manifest, current, allowNew) {
  const recordedDomains = Object.keys(manifest.domains).sort();
  const currentDomains = Object.keys(current).sort();
  for (const domain of recordedDomains) {
    if (!current[domain]) throw new Error(`recorded migration domain was removed: ${domain}`);
    const recorded = manifest.domains[domain];
    const actualByFile = new Map(current[domain].map((entry) => [entry.file, entry]));
    let maxRecordedVersion = 0;
    for (const entry of recorded) {
      maxRecordedVersion = Math.max(maxRecordedVersion, entry.version);
      const actual = actualByFile.get(entry.file);
      if (!actual) throw new Error(`recorded migration was removed: ${domain}/${entry.file}`);
      if (actual.version !== entry.version || actual.sha256 !== entry.sha256) {
        throw new Error(`recorded migration was modified: ${domain}/${entry.file}`);
      }
    }
    const recordedFiles = new Set(recorded.map((entry) => entry.file));
    for (const entry of current[domain]) {
      if (recordedFiles.has(entry.file)) continue;
      if (!allowNew) throw new Error(`migration manifest is missing: ${domain}/${entry.file}`);
      if (entry.version <= maxRecordedVersion) throw new Error(`new migration must advance ${domain} beyond version ${maxRecordedVersion}`);
    }
  }
  for (const domain of currentDomains) {
    if (!recordedDomains.includes(domain) && !allowNew) throw new Error(`migration manifest is missing domain: ${domain}`);
  }
}

async function main() {
  const command = process.argv[2] || 'check';
  const current = await inventory();
  if (command === 'init') {
    if (existsSync(manifestPath)) throw new Error('migration manifest already exists; use append for new versions');
    await writeFile(manifestPath, `${JSON.stringify({ schema_version: 1, algorithm: 'sha256-normalized-lf', domains: current }, null, 2)}\n`);
    console.log(`migration manifest initialized with ${Object.values(current).flat().length} files`);
    return;
  }
  if (!existsSync(manifestPath)) throw new Error('migration manifest is missing; initialize it once and commit it');
  const manifest = JSON.parse(await readFile(manifestPath, 'utf8'));
  validateManifestShape(manifest);
  if (command === 'append') {
    assertExistingEntriesImmutable(manifest, current, true);
    await writeFile(manifestPath, `${JSON.stringify({ ...manifest, domains: current }, null, 2)}\n`);
    console.log(`migration manifest appended to ${Object.values(current).flat().length} files`);
    return;
  }
  if (command !== 'check') throw new Error('usage: account-platform-migration-manifest.mjs [check|init|append]');
  assertExistingEntriesImmutable(manifest, current, false);
  const expected = JSON.stringify({ ...manifest, domains: current });
  if (JSON.stringify(manifest) !== expected) throw new Error('migration manifest order or metadata is not canonical');
  console.log(`migration immutability passed: ${Object.values(current).flat().length} files`);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(`[migration-manifest] ${error.message}`);
    process.exitCode = 1;
  });
}
