import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { realpathSync } from 'node:fs';
import { mkdtemp, rm, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath, pathToFileURL } from 'node:url';

const root = fileURLToPath(new URL('../../../', import.meta.url));
const entries = [
  ['credential-browser-scope', /^CREDENTIAL_EDGE_/],
  ['credential-edge', /^CREDENTIAL_EDGE_/],
  ['credential-origin-probe', /^CREDENTIAL_EDGE_/],
  ['ha-gate', /^\[ha-gate\]/],
  ['migration-manifest', /^\[migration-manifest\]/],
  ['model-token-deployment-check', /^model_token_deployment_/],
  ['model-token-evidence', /^model_token_/],
  ['model-token-operations', /^MODEL_TOKEN_OPERATION_/],
  ['model-token-release-plan', /^model_token_release_/],
  ['performance-gate', /^\[performance-gate\]/],
  ['prometheus-rules', /^offline Prometheus validation failed/],
  ['recovery-gate', /^\[recovery-gate\]/],
  ['render-nginx-fixtures', /^NGINX_FIXTURE_USAGE/],
];
const filename = name => `account-platform-${name}.mjs`;
const invoke = script => spawnSync(process.execPath, [script, '--entry-contract-invalid'], { cwd: root, encoding: 'utf8', timeout: 10000 });

test('linked release directory runs every CLI and preserves its usage failure', async t => {
  const base = await mkdtemp(path.join(realpathSync(tmpdir()), 'pinvou-cli-entry-'));
  t.after(() => rm(base, { recursive: true, force: true }));
  const linked = path.join(base, 'current');
  await symlink(path.join(root, 'scripts'), linked, process.platform === 'win32' ? 'junction' : 'dir');
  for (const [name, diagnostic] of entries) {
    await t.test(name, () => {
      const direct = invoke(path.join(root, 'scripts', filename(name)));
      assert.equal(direct.error, undefined);
      assert.equal(direct.signal, null);
      assert.equal(direct.status, 1);
      assert.equal(direct.stdout, '');
      assert.match(direct.stderr, diagnostic);
      const throughLink = invoke(path.join(linked, filename(name)));
      assert.equal(throughLink.error, undefined);
      assert.equal(throughLink.signal, null);
      assert.equal(throughLink.status, direct.status, 'linked CLI must not silently exit successfully');
      assert.equal(throughLink.stdout, direct.stdout);
      assert.equal(throughLink.stderr, direct.stderr);
    });
  }
});

test('importing CLI modules with an unrelated existing entry does not invoke them', () => {
  const modules = entries.map(([name]) => pathToFileURL(path.join(root, 'scripts', filename(name))).href);
  const code = `process.argv[1] = ${JSON.stringify(fileURLToPath(import.meta.url))}; await Promise.all(${JSON.stringify(modules)}.map(url => import(url))); console.log('IMPORTED_WITHOUT_CLI');`;
  const result = spawnSync(process.execPath, ['--input-type=module', '--eval', code], { cwd: root, encoding: 'utf8', timeout: 10000 });
  assert.equal(result.error, undefined);
  assert.equal(result.signal, null);
  assert.equal(result.status, 0);
  assert.equal(result.stdout, 'IMPORTED_WITHOUT_CLI\n');
  assert.equal(result.stderr, '');
});
