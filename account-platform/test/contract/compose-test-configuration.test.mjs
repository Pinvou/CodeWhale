import assert from 'node:assert/strict';
import { generateKeyPairSync } from 'node:crypto';
import { mkdtemp, rm, writeFile, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { composeTestConfiguration } from '../integration/compose-test-configuration.mjs';

function controlledInputs() {
  const { privateKey, publicKey } = generateKeyPairSync('ed25519');
  return {
    HUMAN_DELEGATION_SIGNING_KEY_ID: 'controlled.test',
    HUMAN_DELEGATION_SIGNING_KEY_BASE64URL: privateKey.export({ format: 'der', type: 'pkcs8' }).subarray(-32).toString('base64url'),
    HUMAN_DELEGATION_VERIFICATION_KEYS_JSON: JSON.stringify({ 'controlled.test': publicKey.export({ format: 'der', type: 'spki' }).subarray(-32).toString('base64url') }),
    TOKEN_DEFAULT_PROVIDER_BASE_URL: 'http://newapi:3000',
    TOKEN_DEFAULT_PROVIDER_API_KEY: 'controlled-provider-only',
    TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN: 'controlled-admin-only',
    TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID: '1',
  };
}

test('Compose test configuration binds supplied delegation and provider inputs and independent HMAC keys', async () => {
  const input = controlledInputs();
  const output = composeTestConfiguration(input);
  for (const [name, value] of Object.entries(input)) assert.equal(output[name], value);
  for (const name of ['OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64', 'TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64']) {
    assert.equal(Buffer.from(output[name], 'base64').length, 32);
    assert.notEqual(output[name], composeTestConfiguration(input)[name]);
  }
  assert.notEqual(output.OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64, output.TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64);
  const compose = await readFile(new URL('../../deploy/compose/compose.yaml', import.meta.url), 'utf8');
  assert.equal([...compose.matchAll(/TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64: \$\{TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64:-\}/g)].length, 2);
  const runner = await readFile(new URL('../../../scripts/account-platform.mjs', import.meta.url), 'utf8');
  assert.match(runner, /const operatorConfiguration = composeTestConfiguration\(\)/);
  assert.match(runner, /\.\.\.operatorConfiguration/);
  assert.match(runner, /'--profile', 'real-newapi'/);
});

test('all required materials fail closed without reflecting supplied secrets', () => {
  const input = controlledInputs();
  for (const name of Object.keys(input)) {
    const changed = { ...input }; delete changed[name];
    assert.throws(() => composeTestConfiguration(changed), new RegExp(`COMPOSE_TEST_CONFIGURATION_REQUIRED:${name}$`));
  }
  for (const [name, value] of [
    ['HUMAN_DELEGATION_SIGNING_KEY_ID', 'invalid key'],
    ['HUMAN_DELEGATION_SIGNING_KEY_BASE64URL', 'secret-not-base64url!'],
    ['HUMAN_DELEGATION_VERIFICATION_KEYS_JSON', '[]'],
    ['HUMAN_DELEGATION_VERIFICATION_KEYS_JSON', JSON.stringify({ 'controlled.test': Buffer.alloc(32, 7).toString('base64url') })],
    ['TOKEN_DEFAULT_PROVIDER_BASE_URL', 'http://mock-newapi:8080'],
    ['TOKEN_DEFAULT_PROVIDER_BASE_URL', 'http://newapi:3000/path'],
    ['TOKEN_DEFAULT_PROVIDER_BASE_URL', 'http://user:secret@newapi:3000'],
    ['TOKEN_DEFAULT_PROVIDER_BASE_URL', 'https://example.invalid'],
    ['TOKEN_DEFAULT_PROVIDER_BASE_URL', 'http://newapi:3000?secret=value'],
    ['TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID', '0'],
    ['TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID', '9223372036854775808'],
    ['TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN', 'secret\nvalue'],
  ]) {
    assert.throws(() => composeTestConfiguration({ ...input, [name]: value }), error => /^COMPOSE_TEST_/.test(error.message) && !error.message.includes(value));
  }
});

test('actual integration entrypoints reject missing operator configuration before any Docker invocation', async () => {
  const directory = await mkdtemp(path.join(tmpdir(), 'pinvou-compose-input-'));
  try {
    const file = path.join(directory, 'controlled.jwt');
    await writeFile(file, 'e30.e30.c2ln'); // Syntax fixture; not an issuer/service authorization claim.
    const environment = { ...process.env, PINVOU_TEST_WORKLOAD_CREDENTIAL_FILE: file };
    for (const name of Object.keys(controlledInputs())) delete environment[name];
    for (const action of ['integration', 'e2e', 'fault']) {
      const result = spawnSync(process.execPath, [path.resolve(import.meta.dirname, '../../../scripts/account-platform.mjs'), 'test', action], { env: environment, encoding: 'utf8', shell: false });
      assert.equal(result.status, 1);
      assert.match(result.stderr, /COMPOSE_TEST_CONFIGURATION_REQUIRED:HUMAN_DELEGATION_SIGNING_KEY_ID/);
      assert.equal(result.stdout, '');
      assert.doesNotMatch(result.stderr, /Docker is required|e30\.e30\.c2ln/);
    }
  } finally {
    assert.equal(path.dirname(directory), tmpdir());
    assert.ok(path.basename(directory).startsWith('pinvou-compose-input-'));
    await rm(directory, { recursive: true, force: true });
  }
});
