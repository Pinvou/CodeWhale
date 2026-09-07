import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('role bindings have validity, idempotent revocation, cache invalidation, and an admin-only management view', async () => {
  const [migration, authorization, lifecycle, adminLifecycle, service, main, accountOpenAPI, adminOpenAPI, adminUI, adminClient, accountHTML, adminHTML, viteConfig] = await Promise.all([
    read('migrations/account/000016_role_binding_lifecycle.up.sql'),
    read('internal/account/authorization.go'),
    read('internal/account/role_bindings.go'),
    read('internal/account/role_bindings_admin.go'),
    read('internal/account/service.go'),
    read('cmd/account-service/main.go'),
    read('contracts/openapi/account.yaml'),
    read('contracts/openapi/admin.yaml'),
    read('../pinvou3-app/src/features/account-platform/AdminRolesWorkspace.jsx'),
    read('../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('internal/portal/static/account.html'),
    read('internal/portal/static/admin.html'),
    read('../pinvou3-app/vite.config.mjs')
  ]);

  for (const column of ['starts_at', 'expires_at', 'revoked_at', 'revoked_by', 'grant_source', 'idempotency_key']) {
    assert.match(migration, new RegExp(column));
  }
  assert.match(migration, /CREATE UNIQUE INDEX role_bindings_active_unique_idx/);
  assert.match(migration, /CREATE OR REPLACE FUNCTION mirror_legacy_role_binding/);
  assert.match(authorization, /ra\.revoked_at IS NULL AND ra\.starts_at<=now\(\) AND \(ra\.expires_at IS NULL OR ra\.expires_at>now\(\)\)/);
  assert.match(lifecycle, /GrantOrganizationRoleBinding/);
  assert.match(lifecycle, /RevokeOrganizationRoleBinding/);
  assert.match(lifecycle, /authorization_cache_invalidated/);
  assert.match(lifecycle, /ExpireRoleBindings/);
  assert.match(lifecycle, /role\.binding\.expire/);
  assert.match(adminLifecycle, /PLATFORM_SUPER_ADMIN/);
  assert.match(adminLifecycle, /AdminRevokeRoleBinding/);
  assert.match(service, /\/v1\/me\/role-bindings/);
  assert.match(service, /\/v1\/organizations\/:organization_id\/role-bindings/);
  assert.match(service, /admin := protected\.Group\("\/admin\/v1"\)/);
  assert.match(service, /admin\.POST\("\/role-bindings"/);
  assert.match(main, /NewRoleBindingExpiryWorker/);
  assert.match(accountOpenAPI, /operationId: listMyRoleBindings/);
  assert.match(accountOpenAPI, /operationId: revokeOrganizationRoleBinding/);
  assert.match(adminOpenAPI, /operationId: grantAdminRoleBinding/);
  assert.match(adminOpenAPI, /operationId: revokeAdminRoleBinding/);
  assert.match(adminUI, /function GrantDrawer/);
  assert.match(adminUI, /client\.grantRoleBinding/);
  assert.match(adminUI, /client\.revokeRoleBinding/);
  assert.doesNotMatch(accountHTML, /my-role-bindings/, 'the personal center must not mount the raw role binding surface');
  assert.doesNotMatch(viteConfig, /my-role-bindings/, 'the personal center build must not re-inject the raw role binding surface');
  assert.match(adminClient, /mutate\('account', '\/admin\/v1\/role-bindings'/, 'role management remains behind the admin Account proxy');
  assert.match(adminClient, /request\('account', `\/admin\/v1\/role-bindings\$\{suffix\}`/, 'role management remains available in the admin console');
  assert.doesNotMatch(adminHTML, /role-bindings-root|role-bindings\.js/, 'the retired standalone role surface is not mounted');
});
