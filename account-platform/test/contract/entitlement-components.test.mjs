import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const repositoryRoot = path.resolve(root, '..');
const read = (relative) => readFile(path.join(root, relative), 'utf8');
const readRepository = (relative) => readFile(path.join(repositoryRoot, relative), 'utf8');

test('entitlements conserve BASE and BONUS components and consume deterministically', async () => {
  const [migration, repository, adjustments, restoreAudit] = await Promise.all([
    read('migrations/billing/000016_entitlement_components.up.sql'),
    read('internal/billing/postgres.go'),
    read('internal/billing/adjustments.go'),
    read('deploy/runbooks/sql/billing-ledger-audit.sql'),
  ]);

  assert.match(migration, /component_type TEXT NOT NULL CHECK \(component_type IN \('BASE', 'BONUS'\)\)/);
  for (const bucket of ['available_microcredits', 'reserved_microcredits', 'used_microcredits', 'active_grant_microcredits', 'suspended_grant_microcredits', 'expired_microcredits', 'revoked_microcredits']) {
    assert.match(migration, new RegExp(bucket));
  }
  for (const source of [repository, adjustments]) {
    assert.match(source, /ORDER BY component\.expires_at,CASE component\.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,component\.credited_at,component\.id/);
  }
  assert.match(migration, /usage_loss_reviews/);
  assert.match(repository, /ENTITLEMENT_EXPIRED_UNCERTAIN/);
  for (const check of ['entitlement_component_conservation_mismatch', 'entitlement_component_lot_mismatch', 'entitlement_package_projection_mismatch', 'entitlement_hold_allocation_mismatch']) {
    assert.match(restoreAudit, new RegExp(check));
  }
});

test('entitlement API and all UI languages expose composition and refund semantics', async () => {
  const [contractSource, view, copy] = await Promise.all([
    read('contracts/openapi/token-gateway.yaml'),
    readRepository('pinvou3-app/src/features/account-platform/AccountPlatformView.jsx'),
    readRepository('pinvou3-app/src/shared/account-platform-i18n.js'),
  ]);
  const contract = YAML.parse(contractSource);
  assert.deepEqual(contract.components.schemas.EntitlementComponent.properties.component_type.enum, ['BASE', 'BONUS']);
  assert.ok(contract.components.schemas.EntitlementComponent.required.includes('effective_original_microcredits'));
  assert.ok(contract.components.schemas.EntitlementComponent.required.includes('reclassified_out_microcredits'));
  assert.equal(contract.components.schemas.EntitlementPackage.properties.components.type, 'array');
  assert.match(view, /item\.components\?\.map/);
  assert.match(view, /component\.effective_original_microcredits \?\? component\.original_microcredits/);
  assert.match(view, /refundCompositionNotice/);
  assert.match(copy, /BONUS 不产生现金退款金额/);
  assert.match(copy, /BONUS has no cash refund value/);
  assert.match(copy, /BONUS は現金返金額を生みません/);
});
