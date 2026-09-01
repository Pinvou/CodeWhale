import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = (path) => readFile(new URL(path, import.meta.url), 'utf8');

test('organization governance mutations use independent atomic permissions', async () => {
  const [authorization, service, registry, ui] = await Promise.all([
    read('../../internal/account/authorization.go'),
    read('../../internal/account/service.go'),
    read('../../internal/platform/approvalpolicy/registry.go'),
    read('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
  ]);
  for (const permission of [
    'platform.organizations.profile.manage',
    'platform.organizations.members.manage',
    'platform.organizations.units.manage',
  ]) {
    assert.match(authorization, new RegExp(permission.replaceAll('.', '\\.'), 'u'));
    assert.match(registry, new RegExp(permission.replaceAll('.', '\\.'), 'u'));
    assert.match(ui, new RegExp(permission.replaceAll('.', '\\.'), 'u'));
  }
  assert.match(service, /admin\.PATCH\("\/organizations\/:organization_id", s\.requirePermission\(PermissionPlatformOrganizationsProfileManage\)/u);
  assert.match(service, /admin\.POST\("\/organizations\/:organization_id\/members", s\.requirePermission\(PermissionPlatformOrganizationsMembersManage\)/u);
  assert.match(service, /admin\.POST\("\/organizations\/:organization_id\/units", s\.requirePermission\(PermissionPlatformOrganizationsUnitsManage\)/u);
  assert.match(service, /admin\.PATCH\("\/organizations\/:organization_id\/status", s\.requirePermission\(PermissionPlatformOrganizationsStatusManage\)/u);
});

test('permission migration preserves previous role capabilities and approval history', async () => {
  const [up, down] = await Promise.all([
    read('../../migrations/account/000039_organization_governance_permissions.up.sql'),
    read('../../migrations/account/000039_organization_governance_permissions.down.sql'),
  ]);
  assert.match(up, /WHERE permission_code='platform\.organizations\.status\.manage'/u);
  assert.match(up, /ON CONFLICT DO NOTHING/u);
  assert.match(up, /bump_role_assignment_scope_version/u);
  assert.match(up, /ACCOUNT_ORGANIZATION_RENAME',2,'AUDIT_ONLY','platform\.organizations\.profile\.manage'/u);
  assert.match(up, /ACCOUNT_MEMBER_ADD',2,'AUDIT_ONLY','platform\.organizations\.members\.manage'/u);
  assert.match(up, /ACCOUNT_UNIT_CREATE',2,'AUDIT_ONLY','platform\.organizations\.units\.manage'/u);
  assert.doesNotMatch(up, /UPDATE approval_command_policies/u);
  assert.match(down, /DROP TRIGGER approval_command_policies_immutable/u);
  assert.match(down, /policy_version=2/u);
});
