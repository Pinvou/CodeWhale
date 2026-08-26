import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const migration = await readFile(new URL('../../migrations/account/000034_three_scope_authorization.up.sql', import.meta.url), 'utf8');
const productPermissionMigration = await readFile(new URL('../../migrations/account/000037_recharge_product_permissions.up.sql', import.meta.url), 'utf8');
const recoveryMigration = await readFile(new URL('../../migrations/account/000033_payment_provider_resume_policy.up.sql', import.meta.url), 'utf8');
const authorization = await readFile(new URL('../../internal/account/authorization.go', import.meta.url), 'utf8');
const postgres = await readFile(new URL('../../internal/account/postgres.go', import.meta.url), 'utf8');
const directory = await readFile(new URL('../../internal/account/directory.go', import.meta.url), 'utf8');
const membership = await readFile(new URL('../../internal/account/membership_governance.go', import.meta.url), 'utf8');
const service = await readFile(new URL('../../internal/account/service.go', import.meta.url), 'utf8');
const openapi = await readFile(new URL('../../contracts/openapi/account.yaml', import.meta.url), 'utf8');
const design = await readFile(new URL('../../docs/role-system-design.md', import.meta.url), 'utf8');

function desiredRoleSegment(role, nextRole) {
  const start = migration.indexOf(`('${role}',ARRAY[`);
  assert.notEqual(start, -1, `missing ${role} truth table`);
  const end = nextRole ? migration.indexOf(`('${nextRole}',ARRAY[`, start) : migration.indexOf('\n)\nINSERT INTO role_permissions', start);
  assert.notEqual(end, -1, `missing end of ${role} truth table`);
  return migration.slice(start, end);
}

function productRolePermissions(role) {
  const match = productPermissionMigration.match(new RegExp(`\\('${role}',ARRAY\\[([^\\]]*)\\]::text\\[\\]\\)`));
  return match ? [...match[1].matchAll(/'((?:personal|platform|org)\.[a-z0-9.-]+)'/g)].map((item) => item[1]) : [];
}

test('authorization migration creates PERSONAL independently and backfills the lifecycle-owned role', () => {
  assert.match(migration, /scope IN \('PERSONAL','PLATFORM','ORGANIZATION','UNIT'\)/);
  assert.match(migration, /'PERSONAL_USER','PERSONAL',FALSE,TRUE/);
  for (const permission of [
    'personal.session.login', 'personal.profile.read', 'personal.profile.update', 'personal.security.manage',
    'personal.context.switch', 'personal.account.close.request', 'personal.account.close.read',
    'personal.billing.read', 'personal.recharge.create', 'personal.recharge.manage', 'personal.models.read',
    'personal.model.invoke', 'personal.usage.read', 'personal.usage.cancel', 'personal.invitations.read',
    'personal.invitations.accept',
  ]) assert.match(desiredRoleSegment('PERSONAL_USER', 'PLATFORM_SUPER_ADMIN'), new RegExp(`'${permission.replaceAll('.', '\\.')}'`));
  assert.match(migration, /u\.status IN \('BOOTSTRAP_PENDING','PROVISIONING','ACTIVE','REPAIR_REQUIRED'\)/);
  assert.match(migration, /role_assignments_scope_guard/);
  assert.match(migration, /scope='PERSONAL'/);
  assert.match(migration, /scope='PLATFORM'/);
  assert.match(migration, /scope='ORGANIZATION'/);
  assert.match(migration, /scope='UNIT'/);
});

test('built-in role truth tables replace instead of append and preserve separation of duties', () => {
  assert.match(migration, /DELETE FROM role_permissions WHERE role_code IN/);
  const superAdmin = desiredRoleSegment('PLATFORM_SUPER_ADMIN', 'PLATFORM_IDENTITY_ADMIN');
  assert.doesNotMatch(superAdmin, /platform\.tokens\.(read|manage|credentials\.validate)/);
  assert.doesNotMatch(superAdmin, /platform\.audit\.pii\.export/);

  const modelAdmin = desiredRoleSegment('PLATFORM_MODEL_ADMIN', 'PLATFORM_OPERATIONS_ADMIN');
  assert.match(modelAdmin, /platform\.tokens\.read/);
  assert.match(modelAdmin, /platform\.tokens\.manage/);
  assert.match(modelAdmin, /platform\.tokens\.credentials\.validate/);

  const unitAdmin = desiredRoleSegment('UNIT_ADMIN', 'AUDITOR');
  assert.doesNotMatch(unitAdmin, /org\.quota\.(allocate|revoke|read\.all)/);
  assert.doesNotMatch(unitAdmin, /org\.usage\.read\.all/);
  assert.match(unitAdmin, /org\.model\.invoke/);

  const organizationModelAdmin = desiredRoleSegment('ORG_MODEL_ADMIN', 'UNIT_ADMIN');
  assert.match(organizationModelAdmin, /org\.models\.manage/);
  assert.doesNotMatch(organizationModelAdmin, /platform\.tokens|org\.wallet|org\.quota\.allocate/);
});

test('every built-in human role exactly matches the reviewed design truth table', () => {
  const roles = [
    'PERSONAL_USER', 'PLATFORM_SUPER_ADMIN', 'PLATFORM_IDENTITY_ADMIN', 'PLATFORM_ORGANIZATION_ADMIN',
    'PLATFORM_BILLING_OPERATOR', 'PLATFORM_BILLING_APPROVER', 'PLATFORM_PRICING_ADMIN', 'PLATFORM_MODEL_ADMIN',
    'PLATFORM_OPERATIONS_ADMIN', 'PLATFORM_AUDITOR', 'OWNER', 'ORG_ADMIN', 'BILLING_ADMIN',
    'ORG_MODEL_ADMIN', 'UNIT_ADMIN', 'AUDITOR', 'MEMBER',
  ];
  for (const [index, role] of roles.entries()) {
    const designLine = design.split(/\r?\n/).find((line) =>
      line.startsWith(`| \`${role}\` |`) && /`(?:personal|platform|org)\.[a-z0-9.-]+`/.test(line));
    assert.ok(designLine, `missing reviewed design row for ${role}`);
    const expected = [...designLine.matchAll(/`((?:personal|platform|org)\.[a-z0-9.-]+)`/g)].map((match) => match[1]).sort();
    const nextRole = roles[index + 1];
    const actual = [...new Set([
      ...[...desiredRoleSegment(role, nextRole).matchAll(/'((?:personal|platform|org)\.[a-z0-9.-]+)'/g)].map((match) => match[1]),
      ...productRolePermissions(role),
    ])].sort();
    assert.deepEqual(actual, expected, `${role} migration permissions diverge from the reviewed design`);
  }
});

test('legacy permission aliases are accepted only at the Account resolver boundary', () => {
  assert.match(migration, /DELETE FROM permissions WHERE code IN \('model\.invoke','organization\.grant\.reclaim_closed'\)/);
  assert.match(authorization, /case "model\.invoke":\s+return "org\.model\.invoke"/);
  assert.match(authorization, /case "organization\.grant\.reclaim_closed":\s+return "org\.grants\.reclaim_closed"/);
  assert.doesNotMatch(postgres, /'model\.invoke'|'organization\.grant\.reclaim_closed'/);
});

test('authorization context exposes independent versions and personal/platform collections', () => {
  for (const field of [
    'personal_authorization_version', 'platform_authorization_version', 'organization_authorization_version',
    'personal_roles', 'personal_permissions', 'platform_roles', 'platform_permissions',
  ]) assert.match(openapi, new RegExp(field));
  assert.match(postgres, /ro\.scope='PERSONAL'/);
  assert.match(postgres, /ro\.scope='PLATFORM'/);
  assert.match(postgres, /ro\.scope='ORGANIZATION'/);
  assert.match(postgres, /ro\.scope='UNIT'/);
  assert.match(postgres, /unit_authorization_bindings|UnitAuthorizationBindings/);
  assert.doesNotMatch(postgres, /ro\.scope IN \('ORGANIZATION','UNIT'\)/);
  assert.match(openapi, /ORG_MODEL_ADMIN/);
  assert.match(openapi, /unit_authorization_bindings/);
});

test('account bootstrap exposes root personal and platform permissions used by permission-driven navigation', () => {
  assert.match(openapi, /Me:[\s\S]*required: \[[^\]]*personal_permissions[^\]]*platform_permissions[^\]]*\]/);
  assert.match(postgres, /ra\.organization_id IS NOT DISTINCT FROM context\.organization_id/);
  assert.match(postgres, /&user\.ActiveContext\.Permissions,[\s\S]*&user\.PersonalPermissions, &user\.PlatformPermissions/);
  assert.match(postgres, /&me\.ActiveContext\.OrganizationAuthorizationVersion, &me\.ActiveContext\.Permissions,[\s\S]*&me\.PersonalPermissions, &me\.PlatformPermissions/);
});

test('organization management keeps explicit object scope without switching billing context', () => {
  assert.match(service, /root_unit_id/);
  assert.match(service, /optionalQueryUUID/);
  assert.match(openapi, /name: root_unit_id[\s\S]*Required for unit-scoped administrators/);
  assert.match(openapi, /name: unit_id[\s\S]*cross-subtree memberships are excluded/);
  assert.match(authorization, /requireOrganizationMemberPermission/);
  assert.match(authorization, /organization\.status='ACTIVE'/);
  assert.match(authorization, /target\.organization_id=\$2 AND target\.id=\$3 AND target\.status='ACTIVE'/);
  assert.match(directory, /scoped_closure\.ancestor_id=\$2/);
  assert.match(directory, /outside_closure\.ancestor_id=\$2/);
  assert.match(membership, /requireOrganizationMemberPermission\(ctx, tx, actorID, organizationID, targetUserID/);
  assert.match(postgres, /requireOrganizationPermission\(ctx, tx, userID, organizationID, newParentID, PermissionOrganizationUnitsManage\)/);
  assert.doesNotMatch(service, /SetActiveContext[\s\S]{0,200}(?:listUnits|listMembers|createUnit|moveUnit)/);
});

test('the unpublished payment recovery migration uses the real permission schema column', () => {
  assert.match(recoveryMigration, /INSERT INTO permissions\(code,scope,description\)/);
  assert.doesNotMatch(recoveryMigration, /scope_type/);
});
