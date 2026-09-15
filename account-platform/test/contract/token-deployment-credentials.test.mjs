import assert from 'node:assert/strict';
import { readFileSync, existsSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = name => readFileSync(path.join(root, name), 'utf8');
const compose = YAML.parse(read('deploy/compose/compose.yaml'), { merge: true });
const deploymentKeys = ['ACCOUNT_MIGRATION_DB_PASSWORD', 'ACCOUNT_APPROVAL_WRITER_DB_PASSWORD', 'ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD', 'BILLING_MIGRATION_DB_PASSWORD', 'TOKEN_MIGRATION_DB_PASSWORD', 'TOKEN_POLICY_DB_PASSWORD'];
const runtimeKeys = ['ACCOUNT_DB_PASSWORD', 'BILLING_DB_PASSWORD', 'TOKEN_DB_PASSWORD', 'PAYMENT_SIM_DB_PASSWORD', 'NEWAPI_DB_PASSWORD', 'KEYCLOAK_DB_PASSWORD'];

test('migration and bootstrap credentials stay outside runtime and run in Billing then Token order', () => {
  const services = compose.services;
  assert.equal(services['billing-migrate'].depends_on['deployment-credentials-check'].condition, 'service_completed_successfully');
  assert.equal(services['token-migrate'].depends_on['billing-migrate'].condition, 'service_completed_successfully');
  assert.equal(services['token-policy-bootstrap'].depends_on['token-migrate'].condition, 'service_completed_successfully');
  assert.equal(services['payment-simulator-migrate'].depends_on['billing-migrate'], undefined);
  assert.match(services['account-migrate'].environment.DATABASE_URL, /account_migrator:\$\{ACCOUNT_MIGRATION_DB_PASSWORD\}/);
  assert.equal(services['account-migrate'].environment.MIGRATION_ROLE, 'account_service');
  assert.match(services['billing-migrate'].environment.DATABASE_URL, /\$\{BILLING_MIGRATION_DB_PASSWORD\}/);
  assert.match(services['token-migrate'].environment.DATABASE_URL, /\$\{TOKEN_MIGRATION_DB_PASSWORD\}/);
  assert.match(services['token-policy-bootstrap'].environment.TOKEN_POLICY_DATABASE_URL, /\$\{TOKEN_POLICY_DB_PASSWORD\}/);
  for (const [name, service] of Object.entries(services)) {
    if (['postgres', 'deployment-credentials-check', 'account-migrate', 'billing-migrate', 'token-migrate', 'token-policy-bootstrap'].includes(name)) continue;
    for (const key of deploymentKeys) {
      if (name === 'account-service' && key === 'ACCOUNT_APPROVAL_WRITER_DB_PASSWORD') continue;
      assert.ok(!JSON.stringify(service.environment || {}).includes(key), `${name} must not receive ${key}`);
    }
  }
  assert.match(read('deploy/compose/init-databases.sh'), /\. \/opt\/pinvou\/validate-database-secrets\.sh/);
  assert.match(read('deploy/compose/init-databases.sh'), /CREATE ROLE account_approval_control_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS/);
  assert.match(read('deploy/compose/init-databases.sh'), /CREATE ROLE account_migrator LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS/);
  assert.match(read('deploy/production/pg_hba.conf'), /account_platform_account account_service,account_migrator,account_approval_writer,account_approval_backfill all scram-sha-256/);
  assert.match(read('deploy/compose/.env.example'), /^ACCOUNT_MIGRATION_DB_PASSWORD=replace-with-independent-account-migration-password$/m);
  const helmMigrations = read('deploy/helm/account-platform/templates/migrations.yaml');
  assert.match(helmMigrations, /key: \{\{ \$domain \}\}-migration-database-url/);
  assert.match(helmMigrations, /MIGRATION_ROLE[\s\S]*account_service/);
  const helmReadme = read('deploy/helm/account-platform/README.md');
  assert.match(helmReadme, /`account-migration-database-url` must identify\s+`account_migrator`/);
  assert.match(helmReadme, /Account Job keeps `MIGRATION_ROLE=account_service`/);
});

test('database credential preflight rejects missing or reused secrets without disclosing them', () => {
  const shell = process.platform === 'win32' ? path.join(process.env.ProgramFiles || 'C:/Program Files', 'Git/bin/sh.exe') : '/bin/sh';
  assert.ok(existsSync(shell), 'POSIX shell is required for the real credential preflight test');
  const input = read('deploy/compose/validate-database-secrets.sh').replace(/\r\n/g, '\n');
  const fixture = Object.fromEntries([...runtimeKeys, ...deploymentKeys].map((key, index) => [key, `fixture-${index}-independent-value`]));
  const run = env => spawnSync(shell, ['-s'], { input, encoding: 'utf8', env: { ...process.env, ...fixture, ...env } });
  assert.equal(run({}).status, 0);
  assert.equal(run({ ENABLE_PAYMENT_SIMULATOR_DB: 'false', PAYMENT_SIM_DB_PASSWORD: '' }).status, 0);
  assert.notEqual(run({ ENABLE_PAYMENT_SIMULATOR_DB: 'true', PAYMENT_SIM_DB_PASSWORD: '' }).status, 0);
  for (const key of deploymentKeys) {
    assert.notEqual(run({ [key]: '' }).status, 0);
    for (const other of [...runtimeKeys, ...deploymentKeys.filter(candidate => candidate !== key)]) {
      const result = run({ [key]: fixture[other] });
      assert.notEqual(result.status, 0, `${key} must not reuse ${other}`);
      assert.ok(!(result.stdout + result.stderr).includes(fixture[other]), 'preflight must not disclose credentials');
    }
  }
});
