import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import YAML from 'yaml';
import { checkProductionBusiness } from '../../../scripts/account-platform-business-compose-check.mjs';
import { businessComposeFixture } from '../helpers/business-compose-fixture.mjs';

const read = name => readFileSync(new URL(`../../${name}`, import.meta.url), 'utf8');

test('one-shot approval backfill has an explicit dedicated database authorization policy', () => {
  const entries = JSON.parse(read('deploy/authorization-entrypoints.json')).cli
    .filter(item => item.name === 'approval-authorization-backfill');
  assert.deepEqual(entries, [{
    name: 'approval-authorization-backfill',
    source: 'account-platform/cmd/approval-authorization-backfill/main.go',
    strategy: 'workload.dedicated-database-approval-authorization-backfill',
  }]);
  const main = read('cmd/approval-authorization-backfill/main.go');
  assert.match(main, /getenv\("APPROVAL_BACKFILL_DATABASE_URL"\)/);
  assert.doesNotMatch(main, /getenv\("(?:DATABASE_URL|APPROVAL_WRITER_DATABASE_URL)"\)/);
  assert.match(main, /account\.NewApprovalBackfillPool/);
});

test('approval writer identity is delivered only to Account, never a backfill credential', () => {
  for (const path of ['deploy/compose/compose.yaml', 'deploy/production/compose.business.yaml']) {
    const { services } = YAML.parse(read(path), { merge: true });
    assert.ok(services['account-service'].environment.APPROVAL_WRITER_DATABASE_URL);
    for (const [name, service] of Object.entries(services)) {
      const environment = service.environment || {};
      if (name !== 'account-service') assert.equal(environment.APPROVAL_WRITER_DATABASE_URL, undefined, name);
      assert.equal(environment.APPROVAL_BACKFILL_DATABASE_URL, undefined, name);
    }
  }
  const production = YAML.parse(read('deploy/production/compose.business.yaml'), { merge: true });
  const writer = new URL(production.services['account-service'].environment.APPROVAL_WRITER_DATABASE_URL);
  assert.equal(writer.username, 'account_approval_writer');
  assert.equal(writer.password, '');
  assert.equal(writer.searchParams.get('sslmode'), 'verify-full');
  const development = YAML.parse(read('deploy/compose/compose.yaml'), { merge: true });
  assert.equal(development.services['account-service'].environment.APPROVAL_WRITER_DATABASE_URL,
    'postgresql://account_approval_writer:${ACCOUNT_APPROVAL_WRITER_DB_PASSWORD}@postgres:5432/account_platform_account?sslmode=disable');
  assert.match(read('deploy/compose/.env.example'), /^ACCOUNT_APPROVAL_WRITER_DB_PASSWORD=\S+$/m);
});

test('Helm writer secret is Account-only and missing credentials preserve safe closure routes', () => {
  const source = read('deploy/helm/account-platform/templates/app-deployments.yaml');
  assert.match(source, /if eq \$app\.name "account-service"[\s\S]*?- name: APPROVAL_WRITER_DATABASE_URL\s+valueFrom: \{ secretKeyRef: \{ name: .*?, key: account-approval-writer-database-url, optional: true \} \}/);
  assert.equal(source.includes('APPROVAL_BACKFILL_DATABASE_URL'), false);
  const main = read('cmd/account-service/main.go');
  assert.match(main, /ConfigureApprovalWriter\(ctx, os\.Getenv\("APPROVAL_WRITER_DATABASE_URL"\)\)/);
  assert.equal(main.includes('APPROVAL_BACKFILL_DATABASE_URL'), false);
  assert.match(main, /WithApprovalWriterReadiness\(compatibility\.Wrap\(service\.Handler\(verify\)\)\), repository\.Ping/);
});

test('production gate rejects reused, weakened, or cross-service approval credentials', () => {
  for (const change of [
    services => { services['account-service'].environment.APPROVAL_WRITER_DATABASE_URL = services['account-service'].environment.DATABASE_URL; },
    services => { services['account-service'].environment.APPROVAL_WRITER_DATABASE_URL = services['account-service'].environment.APPROVAL_WRITER_DATABASE_URL.replace('sslmode=verify-full', 'sslmode=disable'); },
    services => { services['billing-service'].environment.APPROVAL_WRITER_DATABASE_URL = services['account-service'].environment.APPROVAL_WRITER_DATABASE_URL; },
    services => { services['account-service'].environment.APPROVAL_BACKFILL_DATABASE_URL = 'unexpected'; },
  ]) {
    const { config, inventory } = businessComposeFixture();
    change(config.services);
    assert.throws(() => checkProductionBusiness(config, inventory));
  }
});
