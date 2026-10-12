import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

const require = createRequire(new URL('../../pinvou3-app/package.json', import.meta.url));
const { parseDocument } = require('yaml');
const root = new URL('../../account-platform/deploy/production/', import.meta.url);
const readCompose = name => {
  const document = parseDocument(readFileSync(new URL(name, root), 'utf8'));
  assert.deepEqual(document.errors, []);
  return document.toJSON();
};
const personal = readCompose('compose.personal.yaml');
const finance = readCompose('compose.finance.yaml');
const models = readCompose('compose.models.yaml');
const mode = 'ACCOUNT_PLATFORM_PROFILE';
const requiredMode = '${ACCOUNT_PLATFORM_PROFILE:?set personal or models matching selected composition}';
const services = ['account-service', 'token-gateway', 'approval-resources'];

// Compose merges environment mappings by key. Only that rule is modeled here;
// this does not render secrets, start Docker or prove deployment acceptance.
const mergedModes = (overlays, operatorMode) => Object.fromEntries(services.map(service => {
  const environment = Object.assign({}, ...overlays.map(overlay => overlay.services[service]?.environment));
  return [service, environment[mode] === requiredMode ? operatorMode : environment[mode]];
}));
const expectModes = (overlays, operatorMode) => {
  assert.deepEqual(mergedModes(overlays, operatorMode), Object.fromEntries(services.map(service => [service, operatorMode])));
};

test('personal base explicitly selects balance-only Account and Token service assembly', () => {
  for (const service of services.slice(0, 2)) assert.equal(personal.services[service].environment[mode], 'personal');
  assert.equal(personal.services['account-service'].environment.BILLING_HTTP_URL, 'http://billing-service:8080');
  assert.equal(personal.services['token-gateway'].environment.BILLING_HTTP_URL, 'http://billing-service:8080');
  assert.equal(personal.services['token-gateway'].environment.TOKEN_SECRET_PROVIDER, undefined);
  assert.equal(personal.services['token-migrate'], undefined);
});

test('models overlay explicitly selects all three runtime modes without replacing model safety inputs', () => {
  for (const service of services) assert.equal(models.services[service].environment[mode], 'models');
  assert.equal(models.services['account-service'].environment.TOKEN_HTTP_URL, 'http://token-gateway:8080');
  for (const key of ['TOKEN_GOVERNANCE_POLICY_SET_DIGEST', 'TOKEN_GOVERNANCE_HANDLER_SET_DIGEST']) {
    assert.match(models.services['account-service'].environment[key], /^\$\{[A-Z_]+:\?/);
  }
  const token = models.services['token-gateway'];
  assert.deepEqual(token.networks, ['backend', 'kms']);
  assert.equal(token.environment.TOKEN_SECRET_PROVIDER, 'vault-transit');
  for (const key of ['TOKEN_SECRET_PROVIDER_URL', 'TOKEN_SECRET_KEY_ID', 'TOKEN_PROVIDER_ALLOWED_HOSTS']) {
    assert.match(token.environment[key], /^\$\{[A-Z_]+:\?/);
  }
  assert.equal(token.environment.PGPASSFILE, '/run/pinvou/database/pgpass');
  assert.equal(token.depends_on['token-migrate'].condition, 'service_completed_successfully');
});

test('finance requires explicit operator mode and never writes a personal fallback', () => {
  assert.equal(finance.services['approval-resources'].environment[mode], requiredMode);
  for (const service of services.slice(0, 2)) assert.equal(finance.services[service]?.environment?.[mode], undefined);
  assert.deepEqual(finance.services['approval-resources'].profiles, ['approval-operations']);
});

test('personal finance composition keeps all three services personal', () => {
  expectModes([personal, finance], 'personal');
});

test('canonical model composition keeps all three services models', () => {
  expectModes([personal, finance, models], 'models');
});

test('a later finance overlay cannot reset the explicit models selection', () => {
  expectModes([personal, models, finance], 'models');
});

test('required operator interpolation refuses both missing and empty mode before configuration', () => {
  assert.equal(finance.services['approval-resources'].environment[mode], requiredMode);
  const bash = process.platform === 'win32'
    ? ['C:/Program Files/Git/bin/bash.exe', 'C:/Program Files/Git/usr/bin/bash.exe'].find(existsSync)
    : '/bin/bash';
  assert.ok(bash && existsSync(bash), 'Bash is required for the controlled interpolation contract');
  for (const value of [undefined, '', 'personal', 'models']) {
    const env = { ...process.env };
    delete env[mode];
    if (value !== undefined) env[mode] = value;
    const result = spawnSync(bash, ['-c', 'printf \'%s\' "${ACCOUNT_PLATFORM_PROFILE:?set personal or models matching selected composition}"'], {
      env, encoding: 'utf8', timeout: 15_000,
    });
    assert.equal(result.error, undefined);
    if (!value) {
      assert.notEqual(result.status, 0);
      assert.equal(result.stdout, '');
      assert.match(result.stderr, /set personal or models matching selected composition/);
    } else {
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, value);
    }
  }
});

const organizations = readCompose('compose.organizations.yaml');

test('organization overlay explicitly retains complete models assembly for all three services', () => {
  for (const service of services) assert.equal(organizations.services[service].environment[mode], 'models');
  assert.equal(organizations.services['account-service'].environment.TOKEN_HTTP_URL, 'http://token-gateway:8080');
  assert.equal(organizations.services['account-service'].environment.BILLING_HTTP_URL, 'http://billing-service:8080');
  expectModes([personal, finance, models, organizations], 'models');
  expectModes([personal, models, organizations, finance], 'models');
  const token = Object.assign({}, models.services['token-gateway'].environment, organizations.services['token-gateway'].environment);
  assert.equal(token.TOKEN_SECRET_PROVIDER, 'vault-transit');
  assert.equal(token.PGPASSFILE, '/run/pinvou/database/pgpass');
});
