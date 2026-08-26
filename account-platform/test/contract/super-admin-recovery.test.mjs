import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('super administrator bootstrap and break-glass remain offline, one-time, and last-admin safe', async () => {
  const [migration, trustedOperatorMigration, manager, identity, cli, password, keycloak, closure, postgres, service, deployment] = await Promise.all([
    read('migrations/account/000017_super_admin_recovery.up.sql'),
    read('migrations/account/000035_break_glass_trusted_operator.up.sql'),
    read('internal/securityadmin/security.go'),
    read('internal/securityadmin/break_glass_identity.go'),
    read('cmd/account-security/main.go'),
    read('internal/identityadapter/password.go'),
    read('internal/identityadapter/keycloak.go'),
    read('internal/account/account_closure_roles.go'),
    read('internal/account/postgres.go'),
    read('internal/account/service.go'),
    read('docs/deployment.md')
  ]);

  assert.match(migration, /BOOTSTRAP_PENDING/);
  assert.match(migration, /super_admin_bootstrap_credentials/);
  assert.match(migration, /failed_attempts INTEGER NOT NULL DEFAULT 0 CHECK \(failed_attempts BETWEEN 0 AND 5\)/);
  assert.match(migration, /break_glass_operations_immutable/);
  assert.match(migration, /audit_logs_security_no_truncate/);
  assert.match(migration, /LAST_ACTIVE_SUPER_ADMIN/);
  assert.match(manager, /base64\.RawURLEncoding\.EncodeToString/);
  assert.match(manager, /hmac\.Equal/);
  assert.match(manager, /SUPER_ADMIN_BOOTSTRAP_LOCKED/);
  assert.match(manager, /ALL_SUPER_ADMINS_UNREACHABLE/);
  assert.match(manager, /BREAK_GLASS_ACTIVATED/);
  assert.match(identity, /ed25519\.Verify/);
  assert.match(identity, /pinvou-account-security-break-glass/);
  assert.match(identity, /15\s*\*\s*time\.Minute/);
  assert.match(trustedOperatorMigration, /operator_credential_id/);
  assert.match(trustedOperatorMigration, /UNIQUE \(operator_credential_id\)/);
  const triggerDrop = trustedOperatorMigration.indexOf('DROP TRIGGER break_glass_operations_immutable');
  const compatibilityBackfill = trustedOperatorMigration.indexOf('UPDATE break_glass_operations');
  const triggerRestore = trustedOperatorMigration.indexOf('CREATE TRIGGER break_glass_operations_immutable');
  assert.ok(triggerDrop >= 0 && triggerDrop < compatibilityBackfill);
  assert.ok(triggerRestore > compatibilityBackfill);
  assert.match(manager, /operator_credential_id,incident_ticket/);
  assert.match(manager, /appendSecurityAudit\(ctx, tx, nil, "security\.break-glass\.activated"/);
  assert.match(cli, /term\.ReadPassword/);
  assert.match(cli, /operator-credential-fd/);
  assert.match(cli, /BREAK_GLASS_OPERATOR_PUBLIC_KEY_BASE64URL/);
  assert.doesNotMatch(cli, /workload-identity|target-username/);
  assert.match(cli, /bootstrap credentials are never written to stdout/);
  assert.doesNotMatch(cli, /String\("password"/);
  assert.match(password, /ValidatePrivilegedPassword/);
  assert.match(keycloak, /"enabled":\s+false/);
  assert.match(keycloak, /ActivateBootstrapAdmin/);
  assert.match(closure, /RevokeAccountClosureRoleBindings/);
  assert.match(postgres, /user\.Status == "BOOTSTRAP_PENDING"/);
  assert.match(service, /"LAST_ACTIVE_SUPER_ADMIN"/);
  assert.match(deployment, /account-security bootstrap/);
  assert.match(deployment, /operator-credential-fd/);
  assert.doesNotMatch(service, /break-glass/);
});
