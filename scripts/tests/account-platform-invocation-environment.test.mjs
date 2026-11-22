import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

const require = createRequire(new URL('../../pinvou3-app/package.json', import.meta.url));
const { parseDocument } = require('yaml');
const deployment = new URL('../../account-platform/deploy/production/', import.meta.url);
const compose = name => {
  const document = parseDocument(readFileSync(new URL(name, deployment), 'utf8'));
  assert.deepEqual(document.errors, []);
  return document.toJSON();
};
const personal = compose('compose.personal.yaml').services['token-gateway'];
const models = compose('compose.models.yaml').services['token-gateway'];
const invocation = compose('compose.invocation.yaml').services['token-gateway'];
const adminID = 'TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID';
const baseURL = 'TOKEN_RECONCILIATION_NEWAPI_BASE_URL';
const keys = [adminID, baseURL];
const bash = process.platform === 'win32'
  ? ['C:/Program Files/Git/bin/bash.exe', 'C:/Program Files/Git/usr/bin/bash.exe'].find(existsSync)
  : '/bin/bash';
assert.ok(bash && existsSync(bash), 'Bash is required for the controlled environment contract');

// Model only Compose's mapping merge and environment-over-env_file precedence.
// The private-file values here are synthetic. No Docker or real env_file is read.
const effective = (privateValues, interpolation = {}) => {
  const result = { ...privateValues };
  const environment = { ...personal.environment, ...models.environment, ...invocation.environment };
  for (const key of keys) {
    if (!Object.hasOwn(environment, key)) continue;
    const expression = environment[key];
    assert.match(expression, /^\$\{[A-Z_]+:-\}$/);
    const env = { ...process.env };
    for (const name of keys) delete env[name];
    Object.assign(env, interpolation);
    const rendered = spawnSync(bash, ['-c', `printf '%s' "${expression}"`], {
      env, encoding: 'utf8', timeout: 15_000, windowsHide: true,
    });
    assert.equal(rendered.error, undefined);
    assert.equal(rendered.status, 0, rendered.stderr);
    result[key] = rendered.stdout;
  }
  return result;
};

test('invocation retains the required raw private file and does not shadow its evidence inputs', () => {
  assert.equal(invocation.env_file, undefined);
  assert.equal(models.env_file, undefined);
  assert.deepEqual(personal.env_file, [{
    path: '${TOKEN_GATEWAY_PRIVATE_ENV_FILE:?set private Token workload rules file}',
    required: true, format: 'raw',
  }]);
  for (const key of keys) assert.equal(invocation.environment[key], undefined);
});

test('private evidence actor survives without an exported interpolation variable', () => {
  assert.equal(effective({ [adminID]: '17' })[adminID], '17');
  assert.equal(effective({ [adminID]: '17' }, { [adminID]: '' })[adminID], '17');
});

test('private evidence origin survives instead of being replaced by an empty override', () => {
  const url = 'https://evidence.example.invalid';
  assert.equal(effective({ [baseURL]: url })[baseURL], url);
  assert.equal(effective({ [baseURL]: url }, { [baseURL]: '' })[baseURL], url);
});

test('the existing loader still requires an actor and retains the provider-origin fallback', () => {
  const source = readFileSync(new URL('../../account-platform/cmd/token-gateway/personal_execution.go', import.meta.url), 'utf8');
  assert.match(source, /executionPositiveInteger\("TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID", 0\)/);
  assert.match(source, /valueOrDefault\("TOKEN_RECONCILIATION_NEWAPI_BASE_URL", os\.Getenv\("TOKEN_DEFAULT_PROVIDER_BASE_URL"\)\)/);
  const unconfigured = effective({});
  assert.equal(unconfigured[adminID], undefined);
  assert.equal(unconfigured[baseURL], undefined);
});

test('a missing required private file does not become an empty-file success', () => {
  assert.equal(personal.env_file[0].required, true);
  const directory = mkdtempSync(join(tmpdir(), 'pinvou-invocation-env-'));
  try {
    const missing = join(directory, 'missing.env').replaceAll('\\', '/');
    const result = spawnSync(bash, ['-e', '-c', '. "$1"', 'fixture', missing], {
      encoding: 'utf8', timeout: 15_000, windowsHide: true,
    });
    assert.equal(result.error, undefined);
    assert.notEqual(result.status, 0);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test('controlled Bash can read the synthetic private file without requiring exports', () => {
  const directory = mkdtempSync(join(tmpdir(), 'pinvou-invocation-env-'));
  try {
    const file = join(directory, 'fixture.env');
    writeFileSync(file, `${adminID}=17\n${baseURL}=https://evidence.example.invalid\n`);
    const env = { ...process.env };
    for (const key of keys) delete env[key];
    const result = spawnSync(bash, ['-e', '-c', `. "$1"; printf '%s\\n%s' "$${adminID}" "$${baseURL}"`, 'fixture', file.replaceAll('\\', '/')], {
      env, encoding: 'utf8', timeout: 15_000, windowsHide: true,
    });
    assert.equal(result.error, undefined);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, '17\nhttps://evidence.example.invalid');
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
