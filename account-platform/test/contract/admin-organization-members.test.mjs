import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = (path) => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('admin organization member directory is server-paged and searchable without email UI', async () => {
  const [service, model, openapi, client, view] = await Promise.all([
    read('internal/account/admin.go'),
    read('internal/account/model.go'),
    read('contracts/openapi/admin.yaml'),
    read('../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
  ]);

  assert.match(service, /type AdminMemberFilter struct[\s\S]*CursorJoinedAt \*time\.Time/);
  assert.match(service, /ORDER BY m\.joined_at DESC,m\.id DESC LIMIT \$7/);
  assert.match(service, /INVALID_ORGANIZATION_MEMBER_FILTER/);
  assert.match(service, /NOT EXISTS\(SELECT 1 FROM memberships m WHERE m\.organization_id=\$3 AND m\.user_id=u\.id AND m\.status<>'REVOKED'\)/);
  assert.match(service, /filter\.ExcludeOrganizationID != nil && !platformauthorization\.GrantedPermission\(r\.Context\(\), PermissionPlatformOrganizationsRead\)/);
  assert.match(model, /UserCode\s+string\s+`json:"user_code"`/);
  assert.match(openapi, /AdminOrganizationMemberPage/);
  for (const parameter of ['Search', 'Limit', 'Cursor']) assert.match(openapi, new RegExp(`parameters/${parameter}`));
  assert.match(client, /organizationMembers\(organizationID, filtersOrSignal/);
  assert.match(client, /exclude_organization_id/);
  assert.match(view, /memberCursorBack\.current/);
  assert.match(view, /setSelectedMember\(member\)/);
  assert.doesNotMatch(view, /member\.email/);
});
