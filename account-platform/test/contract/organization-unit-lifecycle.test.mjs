import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const load = (path) => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('organization unit commands are idempotent, structure-versioned, and logically retained', async () => {
  const [migration, commands, repository, accountOpenAPI, adminOpenAPI] = await Promise.all([
    load('migrations/account/000020_organization_unit_lifecycle.up.sql'),
    load('internal/account/organization_unit_commands.go'),
    load('internal/account/organization_unit_admin.go'),
    load('contracts/openapi/account.yaml'),
    load('contracts/openapi/admin.yaml'),
  ]);

  assert.match(migration, /structure_version BIGINT NOT NULL DEFAULT 1/);
  assert.match(migration, /CREATE TABLE organization_unit_commands/);
  assert.match(migration, /organization units are retained permanently/);
  assert.match(migration, /WHERE status <> 'DELETED'/);
  assert.match(commands, /request_hash,result FROM organization_unit_commands/);
  assert.match(commands, /storedHash != requestHash/);
  assert.match(repository, /status='DELETED',deleted_at=/);
  assert.doesNotMatch(repository, /DELETE FROM organization_units/);
  assert.match(repository, /PROTECTED_CHILD_UNIT/);
  assert.match(repository, /MEMBER_ATTACHED/);
  assert.match(repository, /ROLE_BINDING_ATTACHED/);
  assert.match(repository, /PENDING_APPROVAL_REQUEST/);
  assert.match(accountOpenAPI, /expected_structure_version/);
  assert.match(accountOpenAPI, /OrganizationUnitMutation/);
  assert.match(adminOpenAPI, /Logically delete an unreferenced organization unit after cross-domain preflight/);
});

test('organization unit deletion becomes restrictive before cross-domain finalization', async () => {
  const [account, billing, token, billingMigration, tokenMigration, tokenRepository, billingService, tokenService, policy] = await Promise.all([
    load('internal/account/organization_unit_lifecycle.go'),
    load('internal/billing/organization_unit_blockers.go'),
    load('internal/token/organization_unit_blockers.go'),
    load('migrations/billing/000019_organization_unit_blockers.up.sql'),
    load('migrations/token/000009_organization_unit_blockers.up.sql'),
    load('internal/token/postgres.go'),
    load('internal/billing/service.go'),
    load('internal/token/service.go'),
    load('internal/platform/approvalpolicy/registry.go'),
  ]);

  assert.match(account, /DELETION_PENDING|ApplyUnitLifecycle/);
  assert.match(billingMigration, /organization_unit_lifecycle_states/);
  assert.match(tokenMigration, /organization_unit_lifecycle_states/);
  assert.match(billing, /organization_unit_governance_blockers/);
  assert.match(billing, /currentStatus == "DELETION_PENDING" && status == "ACTIVE"/);
  assert.match(token, /TOKEN_ACCOUNT_ATTACHED/);
  assert.match(token, /UNSETTLED_UNIT_USAGE/);
  assert.match(token, /currentStatus == "DELETION_PENDING" && status == "ACTIVE"/);
  assert.match(tokenRepository, /lifecycleStatus != "ACTIVE"/);
  assert.match(tokenRepository, /organization-unit:/);
  assert.match(billingService, /BILLING_ORGANIZATION_UNIT_LIFECYCLE_APPLY/);
  assert.match(tokenService, /TOKEN_ORGANIZATION_UNIT_LIFECYCLE_APPLY/);
  assert.match(policy, /BILLING_ORGANIZATION_UNIT_LIFECYCLE_APPLY/);
  assert.match(policy, /TOKEN_ORGANIZATION_UNIT_LIFECYCLE_APPLY/);
});
