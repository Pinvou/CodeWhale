import test from 'node:test';
import assert from 'node:assert/strict';
import { checkBusinessMigrations } from '../../../scripts/account-platform-business-migrations-check.mjs';
import { businessMigrationsFixture } from '../helpers/business-migrations-fixture.mjs';
function reject(change) {
  const f = businessMigrationsFixture(); change(f.config, f.inventory);
  assert.throws(() => checkBusinessMigrations(f.config, f.inventory, f.now));
}
test('production migrations: exact three-domain template and immutable evidence', () => {
  const f = businessMigrationsFixture();
  assert.equal(f.source.includes(':-'), false);
  for (const service of Object.values(f.input.services)) {
    assert.equal(service.mem_limit, '256m');
    assert.deepEqual(service.ulimits, { core: { soft: 0, hard: 0 } });
    assert.deepEqual(service.networks, ['data']);
  }
  assert.deepEqual(checkBusinessMigrations(f.config, f.inventory, f.now), { decision: 'MIGRATION_CONFIG_VALID', environment_status: 'ENV_PENDING', migrations_executed: false });
});
test('production migrations reject missing, future, stale or inconsistent backup', () => {
  reject((c, i) => { delete i.backup; });
  reject((c, i) => { i.backup.completed_at = '2026-09-07T02:00:00Z'; });
  reject((c, i) => { i.backup.completed_at = '2026-09-09T04:00:00Z'; });
  reject((c, i) => { i.backup.manifest_sha256 = 'not-a-digest'; });
  reject(c => { delete c.services['account-migrate'].environment.MIGRATION_BACKUP_EVIDENCE_ID; });
  reject(c => { c.services['billing-migrate'].environment.PINVOU_DEPLOYMENT_ENVIRONMENT = 'development'; });
  reject(c => { c.services['token-migrate'].environment.MIGRATION_BACKUP_SHA256 = '3'.repeat(64); });
});
test('production migrations reject implicit execution, rollback, wrong role or domain', () => {
  reject(c => { delete c.services['account-migrate'].profiles; });
  reject(c => { c.services['account-migrate'].command = ['--dir', '/migrations/account', '--direction', 'down']; });
  reject(c => { c.services['account-migrate'].depends_on = { 'billing-migrate': { condition: 'service_started' } }; });
  reject(c => { c.services['billing-migrate'].environment.MIGRATION_ROLE = 'account_platform_admin'; });
  reject(c => { c.services['account-migrate'].environment.DATABASE_URL = c.services['token-migrate'].environment.DATABASE_URL; });
  reject(c => { c.services['account-migrate'].entrypoint = ['/bin/sh']; });
});
test('production migrations reject plaintext, password URLs, extra environments and unsafe mount sources', () => {
  reject(c => { c.services['account-migrate'].environment.DATABASE_URL = c.services['account-migrate'].environment.DATABASE_URL.replace('verify-full', 'disable'); });
  reject(c => { c.services['account-migrate'].environment.DATABASE_URL = c.services['account-migrate'].environment.DATABASE_URL.replace('@', ':PUBLIC_FIXTURE_ONLY@'); });
  reject(c => { c.services['token-migrate'].environment.PGPASSWORD = 'PUBLIC_FIXTURE_ONLY'; });
  reject(c => { c.services['billing-migrate'].volumes[0].source = c.services['account-migrate'].volumes[0].source; });
  reject(c => { c.services['account-migrate'].volumes[0].source = '/fixture/migrations'; });
  reject(c => { c.services['account-migrate'].volumes[0].read_only = false; });
  reject(c => { c.services['account-migrate'].volumes[0].bind.create_host_path = true; });
});
test('production migrations reject new networks, public access, privileges and image drift', () => {
  reject(c => { c.services['account-migrate'].image = 'postgres:latest'; });
  reject(c => { c.services['account-migrate'].build = '.'; });
  reject(c => { c.services['account-migrate'].networks.edge = {}; });
  reject(c => { c.networks.data.external = false; });
  reject(c => { c.services['account-migrate'].ports = ['5432:5432']; });
  reject(c => { c.services['account-migrate'].privileged = true; });
  reject(c => { c.services['account-migrate'].user = '0:0'; });
  reject(c => { c.services['account-migrate'].ulimits = {}; });
  reject(c => { c.services['account-migrate'].ulimits = { core: { hard: -1 } }; });
});
