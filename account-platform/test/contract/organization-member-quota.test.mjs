import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const repositoryRoot = path.resolve(root, '..');
const read = relative => readFile(path.join(root, relative), 'utf8');
const readRepository = relative => readFile(path.join(repositoryRoot, relative), 'utf8');

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

test('organization authorization context requires the per-organization membership version', async () => {
  const accountContract = YAML.parse(await read('contracts/openapi/account.yaml'));
  const schema = accountContract.paths['/me/authorization-context'].get.responses['200'].content['application/json'].schema;
  const organizationRule = schema.allOf.find(rule => rule.if?.properties?.context_type?.const === 'ORGANIZATION');
  assert.ok(organizationRule);
  assert.deepEqual(organizationRule.then.required, ['organization_id', 'organization_membership_version']);
  assert.equal(organizationRule.then.properties.organization_id.type, 'string');
  assert.equal(organizationRule.then.properties.organization_membership_version.minimum, 1);
});

test('member quota reconciliation migration locks grants before audit DDL', async () => {
  const migration = await read('migrations/billing/000078_member_quota_membership_version_audit.up.sql');
  const grantLock = migration.indexOf('LOCK TABLE public.member_quota_grants IN ACCESS EXCLUSIVE MODE');
  const auditLock = migration.indexOf('LOCK TABLE public.member_quota_grant_audits IN ACCESS EXCLUSIVE MODE');
  const auditDDL = migration.indexOf('ALTER TABLE public.member_quota_grant_audits DROP CONSTRAINT');
  const lifecycleRead = migration.indexOf('FROM public.organization_member_lifecycle_states lifecycle');
  const outboxWrite = migration.indexOf('INSERT INTO public.outbox_events');
  assert.ok(grantLock >= 0);
  assert.ok(grantLock < auditLock);
  assert.ok(auditLock < auditDDL);
  assert.ok(auditDDL < lifecycleRead);
  assert.ok(lifecycleRead < outboxWrite);
  assert.match(migration, /UPDATE public\.member_quota_grants quota_grant/);
  assert.doesNotMatch(migration, /\b(?:UPDATE|FROM)\s+public\.member_quota_grants\s+grant\b/);
  assert.doesNotMatch(migration, /LOCK TABLE public\.(?:organization_member_lifecycle_states|outbox_events)/);
});
