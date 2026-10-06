import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const load = (path) => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('invitations support target-bound recovery and one-time links without enumeration input', async () => {
  const [migration, repository, service, openapi] = await Promise.all([
    load('migrations/account/000021_membership_governance.up.sql'),
    load('internal/account/invitations.go'),
    load('internal/account/service.go'),
    load('contracts/openapi/account.yaml'),
  ]);

  assert.match(migration, /target_type IN \('EMAIL','PHONE','USERNAME','LINK'\)/);
  assert.match(migration, /target_digest BYTEA/);
  assert.match(migration, /invitations are retained permanently/);
  assert.match(repository, /case "PHONE"/);
  assert.match(repository, /case "USERNAME"/);
  assert.match(repository, /case "LINK"/);
  assert.match(repository, /target_type<>'LINK'/);
  assert.match(service, /GET\("\/v1\/me\/invitations"/);
  assert.match(openapi, /Recover invitations from the authenticated identity/);
});

test('membership commands are idempotent, immediately restrictive, and preserve one owner', async () => {
  const [migration, membership, billing, billingMigration, billingService, policy] = await Promise.all([
    load('migrations/account/000021_membership_governance.up.sql'),
    load('internal/account/membership_governance.go'),
    load('internal/billing/organization_member_lifecycle.go'),
    load('migrations/billing/000020_organization_member_lifecycle.up.sql'),
    load('internal/billing/service.go'),
    load('internal/platform/approvalpolicy/registry.go'),
  ]);

  assert.match(migration, /CREATE TABLE membership_commands/);
  assert.match(migration, /effective organization must have exactly one owner/);
  assert.match(migration, /role_bindings_no_physical_delete/);
  assert.match(membership, /storedHash != requestHash/);
  assert.match(membership, /UPDATE user_active_contexts SET context_type='PERSONAL'/);
  assert.match(membership, /active_context_type=CASE WHEN active_organization_id=/);
  assert.doesNotMatch(membership, /DELETE FROM role_bindings/);
  assert.match(billingMigration, /organization_member_lifecycle_states/);
  assert.match(billing, /membershipVersion < currentVersion/);
  assert.match(billingService, /BILLING_ORGANIZATION_MEMBER_LIFECYCLE_APPLY/);
  assert.match(policy, /BILLING_ORGANIZATION_MEMBER_LIFECYCLE_APPLY/);
});
