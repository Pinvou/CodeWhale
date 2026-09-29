import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const read = (relative) => readFile(path.join(root, relative), 'utf8');

test('password reset and profile changes are valid SMS send-attempt purposes', async () => {
  const add = await readFile(
    path.join(root, 'migrations/account/000078_expand_sms_send_attempt_purposes.up.sql'),
    'utf8',
  );
  const validate = await readFile(
    path.join(root, 'migrations/account/000079_validate_sms_send_attempt_purposes.up.sql'),
    'utf8',
  );
  const activate = await readFile(
    path.join(root, 'migrations/account/000080_activate_sms_send_attempt_purposes.up.sql'),
    'utf8',
  );
  const down = await readFile(
    path.join(root, 'migrations/account/000080_activate_sms_send_attempt_purposes.down.sql'),
    'utf8',
  );

  assert.match(add, /ADD CONSTRAINT sms_send_attempts_purpose_v2_check/);
  assert.match(add, /SET LOCAL lock_timeout = '2s'/);
  assert.match(add, /NOT VALID/);
  assert.match(
    add,
    /CHECK \(purpose IN \('REGISTER', 'LOGIN', 'PROFILE_MOBILE', 'PASSWORD_RESET'\)\)/,
  );
  assert.match(validate, /VALIDATE CONSTRAINT sms_send_attempts_purpose_v2_check/);
  assert.match(validate, /SET LOCAL lock_timeout = '2s'/);
  assert.match(activate, /AND convalidated/);
  assert.match(activate, /SET LOCAL lock_timeout = '2s'/);
  assert.match(activate, /DROP CONSTRAINT sms_send_attempts_purpose_check/);
  assert.match(activate, /RENAME CONSTRAINT sms_send_attempts_purpose_v2_check/);
  assert.match(down, /ACCOUNT_SMS_SEND_ATTEMPT_PURPOSES_FORWARD_ONLY/);
  assert.match(down, /ERRCODE = '55000'/);
});

test('password reset recovery is fenced, bounded, and migration-safe', async () => {
  const [up, down, service, identity, provisioner, provider, registration, adminRepair, adminContract, gate, image, productionCompose, helmUpgrade, productionValues, runtimeGate, runbook] = await Promise.all([
    read('migrations/account/000085_password_reset_recovery.up.sql'),
    read('migrations/account/000085_password_reset_recovery.down.sql'),
    read('internal/account/profile_changes.go'),
    read('internal/identityadapter/keycloak.go'),
    read('deploy/keycloak/provision-mobile-auth.sh'),
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/PinvouIdentitySecurityResource.java'),
    read('deploy/keycloak/mobile-authenticator/src/main/resources/META-INF/services/org.keycloak.services.resource.RealmResourceProviderFactory'),
    read('internal/account/profile_changes_admin.go'),
    read('contracts/openapi/admin.yaml'),
    read('../.github/workflows/pr-check.yml'),
    read('deploy/keycloak/Dockerfile'),
    read('deploy/production/compose.keycloak.yaml'),
    read('deploy/helm/account-platform/templates/keycloak-realm-upgrade.yaml'),
    read('deploy/helm/account-platform/values-production-new.yaml'),
    read('test/integration/keycloak-identity-security.sh'),
    read('deploy/runbooks/keycloak-realm-upgrade.md'),
  ]);
  assert.match(up, /status='VERIFIED' OR \(status='FAILED' AND last_error_code='IDENTITY_RESET_FAILED'\)/);
  assert.match(up, /'LEGACY_SESSION_REVOCATION_UNCERTAIN'/);
  assert.match(up, /password_write_started_at/);
  assert.match(up, /identity_security_version/);
  assert.match(up, /CREATE UNIQUE INDEX password_reset_operations_active_user_idx/);
  assert.match(down, /RAISE EXCEPTION 'cannot roll back password-reset recovery while nonterminal operations exist'/);
  assert.match(service, /PasswordResetByChallenge\(ctx, challengeID\)/);
  assert.match(service, /MarkPasswordWriteStarted\(ctx, operation\.ID, owner\)/);
  assert.match(service, /WHERE id=\$1 AND status='VERIFIED' AND password_write_started_at IS NULL/);
  assert.match(service, /attempts\+1>=10 OR sla_expires_at<=\$2 THEN 'REPAIR_REQUIRED'/);
  assert.match(identity, /pinvou-identity-security\/users\/.*\/password-reset/);
  assert.match(identity, /pinvou-identity-security\/users\/.*\/profile-change/);
  assert.match(identity, /SetPasswordForOperation[\s\S]*atomicPasswordReset/);
  assert.match(identity, /UpdateUsername[\s\S]*atomicProfileChange/);
  assert.match(identity, /UpdateMobile[\s\S]*atomicProfileChange/);
  assert.doesNotMatch(identity, /updateFencedUser|userLocks/);
  assert.match(provider, /LockModeType\.PESSIMISTIC_WRITE/);
  assert.match(provider, /realmUser\(realm, subject\)[\s\S]*requireResetPassword\(candidate\)[\s\S]*lockRealmUser\(realm, subject, candidate\)/);
  assert.match(provider, /locked JPA row is authoritative only for the fence decision/);
  assert.match(provider, /Mutations must continue through session\.users\(\)/);
  assert.doesNotMatch(provider, /getProvider\(UserProvider\.class, "jpa"\)/);
  assert.match(provider, /requireResetPassword\(user\)/);
  assert.match(provider, /updateCredential[\s\S]*setCommonFence/);
  assert.match(provider, /@Path\("users\/\{subject\}\/profile-change"\)/);
  assert.match(provider, /requireManage\(candidate\)[\s\S]*lockRealmUser\(realm, subject, candidate\)/);
  assert.match(provider, /request\.operationId\.version\(\) != 7/);
  assert.match(registration, /PinvouIdentitySecurityResourceProviderFactory/);
  assert.doesNotMatch(up, /password_write_lease/);
  assert.match(adminRepair, /ATOMIC_PASSWORD_RECOVERY_SCHEDULED/);
  assert.doesNotMatch(adminRepair, /status='FAILED'/);
  assert.doesNotMatch(adminRepair, /password_reset_operations SET status='RETRY',attempts=0/);
  assert.match(adminContract, /action: \{ type: string, const: ATOMIC_PASSWORD_RECOVERY_SCHEDULED \}/);
  for (const attribute of ['pinvou_identity_security_operation_id', 'pinvou_identity_security_fence_version', 'pinvou_password_reset_operation_id', 'pinvou_password_reset_fence_version']) {
    const definition = provisioner.slice(provisioner.indexOf(`"name": "${attribute}"`));
    assert.match(definition, /"view": \[ "admin" \]/);
    assert.match(definition, /"edit": \[ "admin" \]/);
  }
  assert.match(gate, /TestPasswordResetRecoveryPostgresIntegration/);
  assert.match(gate, /! grep -F -- "--- SKIP: \$test_name"/);
  assert.match(gate, /PinvouIdentitySecurityResourceTest/);
  assert.match(gate, /Keycloak identity security two-node PostgreSQL gate/);
  assert.match(gate, /bash account-platform\/test\/integration\/keycloak-identity-security\.sh/);
  assert.match(gate, /KC_HTTP_MANAGEMENT_PORT="\$management_port"/);
  assert.match(gate, /management_port=19082[\s\S]*management_port=19083/);
  assert.match(gate, /KC_CACHE_STACK=jdbc-ping/);
  assert.match(runtimeGate, /KEYCLOAK_NODE_TWO_URL/);
  assert.match(runtimeGate, /pinvou_identity_security_operation_id/);
  assert.match(runtimeGate, /IDENTITY_SECURITY_FENCE_CONFLICT/);
  assert.match(image, /COPY .*provision-mobile-auth\.sh \/opt\/keycloak\/bin\/pinvou-provision-mobile-auth\.sh/);
  assert.match(provisioner, /KCADM_CONFIG=\$\{KCADM_CONFIG:-\/tmp\/pinvou-kcadm\.config\}/);
  assert.match(provisioner, /"\$KCADM_BINARY" "\$@" --config "\$KCADM_CONFIG"/);
  assert.match(productionCompose, /keycloak-realm-upgrade:[\s\S]*pinvou-provision-mobile-auth\.sh/);
  assert.match(productionCompose, /KEYCLOAK_REALM_UPGRADE_SECRET_DIRECTORY/);
  assert.match(productionCompose, /PINVOU_OIDC_CLIENT_ID: pinvou-desktop/);
  assert.match(helmUpgrade, /post-install,post-upgrade/);
  assert.match(helmUpgrade, /keycloak-realm-upgrade-admin-password/);
  assert.match(helmUpgrade, /PINVOU_OIDC_CLIENT_ID[\s\S]*keycloak\.oidcClientId/);
  assert.match(productionValues, /realmUpgradeEnabled: true/);
  assert.match(runbook, /升级失败必须阻断业务发布/);
});
