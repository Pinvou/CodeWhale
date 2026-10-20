import assert from 'node:assert/strict';
import { mkdtemp, readFile, readdir, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { renderNginxFixtures } from '../../../scripts/account-platform-render-nginx-fixtures.mjs';

test('public NGINX fixture renderer closes six parser inputs without network or deployment', async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'pinvou-nginx-contract-'));
  try {
    const result = await renderNginxFixtures(directory, '/fixture/runtime');
    assert.equal(result.files.length, 6);
    assert.equal(result.environment_status, 'ENV_PENDING');
    assert.equal(result.deployment_status, 'NOT_DEPLOYED');
    assert.deepEqual((await readdir(directory)).sort(), ['.pinvou-nginx-fixture', ...result.files].sort());
    for (const filename of result.files) {
      const source = await readFile(path.join(directory, filename), 'utf8');
      assert.doesNotMatch(source, /__[A-Z_]+__|ssl_verify_client off|proxy_ssl_verify off/);
      assert.match(source, /proxy_next_upstream off;/);
      assert.match(source, /location = \/credential-handoff/);
      assert.match(source, /location = \/launch/);
    }
    await assert.rejects(renderNginxFixtures(directory, '/fixture/runtime'), /DIRECTORY_NOT_EMPTY/);
    await assert.rejects(renderNginxFixtures(directory, '/fixture/../runtime'), /PATH_INVALID/);
  } finally {
    assert.equal(path.dirname(directory), path.resolve(os.tmpdir()));
    assert.ok(path.basename(directory).startsWith('pinvou-nginx-contract-'));
    await rm(directory, { recursive: true, force: true });
  }
});

test('production npm carriers require evidence while policy carriers stay separate', async () => {
  const pkg = JSON.parse(await readFile(new URL('../../package.json', import.meta.url), 'utf8'));
  const cwd = fileURLToPath(new URL('../../../', import.meta.url));
  for (const name of ['ha', 'recovery', 'performance']) {
    const production = pkg.scripts[`check:production-${name}`];
    const policy = pkg.scripts[`check:${name}-policy`];
    assert.equal(production, `${policy} --require-evidence`);
    const missing = spawnSync(process.execPath, [production.split(' ')[1].replace('../', ''), '--require-evidence'], {
      cwd, encoding: 'utf8', timeout: 15000,
    });
    assert.equal(missing.error, undefined);
    assert.notEqual(missing.status, 0, `${name} must not pass without externally bound evidence`);
    assert.doesNotMatch(missing.stdout, /ARTIFACT_BINDINGS_VERIFIED|evidence passed/i);
  }
});
