import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = (path) => readFile(new URL(path, import.meta.url), 'utf8');

test('admin organization unit move is permission-gated, CAS-bound, idempotent, and audited', async () => {
  const [service, admin, repository, policy, migration, openapi] = await Promise.all([
    read('../../internal/account/service.go'),
    read('../../internal/account/admin.go'),
    read('../../internal/account/organization_unit_admin.go'),
    read('../../internal/platform/approvalpolicy/registry.go'),
    read('../../migrations/account/000040_admin_organization_unit_move_policy.up.sql'),
    read('../../contracts/openapi/admin.yaml'),
  ]);
  assert.match(service, /admin\.PATCH\("\/organizations\/:organization_id\/units\/:unit_id\/parent", s\.requirePermission\(PermissionPlatformOrganizationsUnitsManage\), approvalpolicy\.Require\("ACCOUNT_UNIT_MOVE"/u);
  assert.match(admin, /ExpectedStructureVersion\s+int64\s+`json:"expected_structure_version"`/u);
  assert.match(admin, /repository\.AdminMoveUnit/u);
  assert.match(repository, /pg_advisory_xact_lock/u);
  assert.match(repository, /structureVersion != expectedStructureVersion/u);
  assert.match(repository, /organization_unit_closure/u);
  assert.match(repository, /organizationUnitRequestHash\("MOVE"/u);
  assert.match(repository, /appendAdminAudit[\s\S]*admin\.organization_unit\.move/u);
  assert.match(policy, /"ACCOUNT_UNIT_MOVE":\s+"platform\.organizations\.units\.manage"/u);
  assert.match(migration, /ACCOUNT_UNIT_MOVE',1,'AUDIT_ONLY','platform\.organizations\.units\.manage'/u);
  assert.match(openapi, /moveAdminOrganizationUnit[\s\S]*expected_structure_version[\s\S]*"409"[\s\S]*"422"/u);
});

test('admin organization tree supports valid destination selection and stale-tree recovery', async () => {
  const [client, view, translations] = await Promise.all([
    read('../../../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../../../pinvou3-app/src/shared/admin-platform-i18n.js'),
  ]);
  assert.match(client, /moveOrganizationUnit\(organizationID, unitID, parentID, expectedStructureVersion\)/u);
  assert.match(client, /expected_structure_version: expectedStructureVersion/u);
  assert.match(view, /excludedMoveTargets/u);
  assert.match(view, /candidate\.status === 'ACTIVE' && !excludedMoveTargets\.has\(candidate\.unit_id\)/u);
  assert.match(view, /reason\?\.code === 'ACCOUNT_CONFLICT'[\s\S]*refreshUnits/u);
  assert.match(view, /reason\?\.code === 'ORG_UNIT_HIERARCHY_INVALID'/u);
  assert.equal([...translations.matchAll(/unitStructureConflict:/gu)].length, 3);
  assert.equal([...translations.matchAll(/unitMoveInvalid:/gu)].length, 3);
});
