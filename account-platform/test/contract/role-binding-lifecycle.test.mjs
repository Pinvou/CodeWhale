import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('role bindings have validity, idempotent revocation, cache invalidation, and three-language views', async () => {
  const [migration, authorization, lifecycle, adminLifecycle, service, main, accountOpenAPI, adminOpenAPI, myUI, adminUI] = await Promise.all([
    read('migrations/account/000016_role_binding_lifecycle.up.sql'),
    read('internal/account/authorization.go'),
    read('internal/account/role_bindings.go'),
    read('internal/account/role_bindings_admin.go'),
    read('internal/account/service.go'),
    read('cmd/account-service/main.go'),
    read('contracts/openapi/account.yaml'),
    read('contracts/openapi/admin.yaml'),
    read('../pinvou3-app/src/features/account-platform/AccountProfile.jsx'),
    read('../pinvou3-app/src/features/account-platform/AdminOperations.jsx')
  ]);

  for (const column of ['starts_at', 'expires_at', 'revoked_at', 'revoked_by', 'grant_source', 'idempotency_key']) {
    assert.match(migration, new RegExp(column));
  }
  assert.match(migration, /CREATE UNIQUE INDEX role_bindings_active_unique_idx/);
  assert.match(migration, /CREATE OR REPLACE FUNCTION mirror_legacy_role_binding/);
  assert.match(authorization, /ra\.revoked_at IS NULL AND ra\.starts_at<=statement_timestamp\(\) AND \(ra\.expires_at IS NULL OR ra\.expires_at>statement_timestamp\(\)\)/);
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
  const { dict } = await import('../../../pinvou3-app/tests/helpers/i18n-all.js');
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(dict[language].uiAccountPlatform.myRoleBindings.title);
    assert.ok(dict[language].uiAdminPlatform.roleBindings.title);
  }
  assert.match(myUI, /allCopy\.myRoleBindings/);
  assert.match(adminUI, /allCopy\.roleBindings/);
});
