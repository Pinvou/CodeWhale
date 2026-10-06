import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

import YAML from 'yaml';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';

const root = path.resolve(import.meta.dirname, '../..');
const repositoryRoot = path.resolve(root, '..');
const read = relative => readFile(path.join(root, relative), 'utf8');
const readRepository = relative => readFile(path.join(repositoryRoot, relative), 'utf8');

test('organization checkout schema keeps the existing single-order wire and requires its beneficiary', async () => {
  const [billing, gateway] = await Promise.all([
    read('contracts/openapi/billing.yaml').then(source => YAML.parse(source)),
    read('contracts/openapi/token-gateway.yaml').then(source => YAML.parse(source)),
  ]);
  const ajv = new Ajv2020({ strict: true, strictRequired: false, allErrors: true });
  addFormats(ajv);
  ajv.addFormat('int64', { type: 'number', validate: Number.isInteger });
  ajv.addKeyword({ keyword: 'example', valid: true });
  const schemas = JSON.parse(JSON.stringify(billing.components.schemas).replaceAll('#/components/schemas/', '#/$defs/'));
  ajv.addSchema({ $id: 'urn:pinvou:organization-orders', $defs: schemas });
  const response = gateway.paths['/organizations/{organization_id}/recharge/orders'].post.responses['201'].content['application/json'].schema;
  const validate = ajv.compile(JSON.parse(JSON.stringify(response).replaceAll('./billing.yaml#/components/schemas/', 'urn:pinvou:organization-orders#/$defs/')));
  const id = '11111111-1111-4111-8111-111111111111';
  const order = { order_id: id, merchant_order_no: 'owned-order', requester_id: id, wallet_id: id, beneficiary_organization_id: id,
    status: 'PENDING', fulfillment_status: 'PENDING', product_id: 'PLAN', amount_fen: 100, currency: 'CNY', credit_microcredits: 1000,
    channel: 'ALIPAY', expires_at: '2026-10-06T00:00:00Z',
    provider_payable_until: '2026-10-06T00:00:00Z', late_review_boundary_at: '2026-10-09T00:00:00Z',
    final_archive_eligible_at: '2026-11-06T00:00:00Z', archive_status: 'ACTIVE' };
  assert.ok(validate(order), JSON.stringify(validate.errors));
  for (const field of ['beneficiary_organization_id', 'wallet_id', 'provider_payable_until', 'late_review_boundary_at', 'final_archive_eligible_at', 'archive_status']) {
    const missing = { ...order }; delete missing[field]; assert.equal(validate(missing), false);
  }
  assert.equal(validate({ ...order, beneficiary_organization_id: 'foreign-shape' }), false);
  assert.equal(validate({ ...order, amount_fen: '100' }), false, 'the existing single-order amount remains a number');
  assert.equal(validate({ ...order, unexpected: true }), false);
  const grant = gateway.components.schemas.MemberQuotaGrant;
  for (const field of ['original_microcredits', 'available_microcredits', 'reserved_microcredits', 'used_microcredits', 'expired_microcredits', 'revoked_microcredits']) {
    assert.equal(grant.properties[field].type, 'string');
    assert.ok(grant.required.includes(field));
  }
  assert.deepEqual(grant.properties.expires_at.type, ['string', 'null']);
});

test('organization grants preserve exact component conservation and lifecycle ownership', async () => {
  const [migration, grants, holds, lifecycle, quotaHTTP] = await Promise.all([
    read('migrations/billing/000021_organization_entitlement_grants.up.sql'),
    read('internal/billing/member_quota_grants.go'),
    read('internal/billing/member_quota_holds.go'),
    read('internal/billing/organization_member_lifecycle.go'),
    read('internal/billing/member_quota_http.go'),
  ]);
  assert.match(migration, /beneficiary_organization_id UUID/);
  assert.match(migration, /available_microcredits \+ reserved_microcredits \+ used_microcredits \+ expired_microcredits \+ revoked_microcredits = original_microcredits/);
  assert.match(migration, /UNIQUE \(organization_id,idempotency_key\)/);
  assert.match(quotaHTTP, /INSUFFICIENT_COMPONENT_CAPACITY/);
  assert.match(grants, /request_fingerprint/);
  assert.match(holds, /member_quota_hold_allocations/);
  assert.match(holds, /active_grant_microcredits=active_grant_microcredits-\$2,reserved_microcredits=reserved_microcredits\+\$2/);
  assert.match(lifecycle, /suspension_reason='MEMBER_REMOVED'/);
  assert.match(lifecycle, /billing\.member-quota\.restored\.v1/);
});

test('organization quota contract exposes permission-gated purchase, batch allocation, and revoke', async () => {
  const [contractSource, accountContractSource, tokenService, accountDirectory, view, copy] = await Promise.all([
    read('contracts/openapi/token-gateway.yaml'),
    read('contracts/openapi/account.yaml'),
    read('internal/token/organization_billing.go'),
    read('internal/account/directory.go'),
    readRepository('pinvou3-app/src/features/account-platform/AccountPlatformView.jsx'),
    readRepository('pinvou3-app/src/shared/account-platform-i18n.js'),
  ]);
  const contract = YAML.parse(contractSource);
  const accountContract = YAML.parse(accountContractSource);
  assert.ok(contract.paths['/organizations/{organization_id}/recharge/orders'].post);
  assert.equal(contract.paths['/organizations/{organization_id}/quota-grants:batch'].post.responses['207'].description.includes('Per-item'), true);
  assert.deepEqual(contract.components.schemas.MemberQuotaAllocation.properties.component_type.enum, ['BASE', 'BONUS']);
  assert.ok(accountContract.paths['/organizations/{organization_id}/quota-members'].get);
  assert.match(accountDirectory, /PermissionOrganizationQuotaAllocate/);
  for (const permission of ['org.entitlements.purchase', 'org.quota.allocate', 'org.quota.revoke']) assert.match(tokenService, new RegExp(permission.replaceAll('.', '\\.')));
  assert.match(view, /purchaseOrganizationEntitlements/);
  assert.match(view, /allocateOrganizationQuota/);
  assert.match(copy, /为组织购买权益/);
  assert.match(copy, /Purchase organization benefits/);
  assert.match(copy, /組織特典を購入/);
});
