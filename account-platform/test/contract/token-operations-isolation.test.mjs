import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const root = new URL('../../', import.meta.url);
const read = path => readFileSync(new URL(path, root), 'utf8');

test('platform Token operations are role isolated and resource scoped', () => {
  const rbac = read('migrations/account/000010_authorization_rbac.up.sql');
  const superAdminExpansion = read('migrations/account/000042_super_admin_platform_permissions.up.sql');
  const command = read('migrations/account/000026_token_account_operations.up.sql');
  const service = read('internal/token/service.go');
  const admin = read('internal/token/admin.go');
  const repository = read('internal/token/postgres.go');
  const quarantine = read('migrations/token/000022_legacy_token_account_quarantine.up.sql');
  const policy = read('internal/platform/approvalpolicy/registry.go');
  const adminAPI = read('contracts/openapi/admin.yaml');
  const organizationAPI = read('contracts/openapi/token-gateway.yaml');
  const portal = read('internal/portal/service.go');
  const client = read('../pinvou3-app/src/features/account-platform/admin-client.js');
  const ui = read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx');
  const tokenResourcesUI = read('../pinvou3-app/src/features/account-platform/AdminTokenResourcesWorkspace.jsx');

  for (const permission of ['platform.tokens.read', 'platform.tokens.manage', 'platform.tokens.credentials.validate']) {
    assert.match(rbac, new RegExp(`'PLATFORM_MODEL_ADMIN'.*${permission.replaceAll('.', '\\.')}`));
  }
  // Keep the original migration immutable and prove the later forward
  // migration intentionally changes the effective super-admin boundary.
  assert.match(rbac, /PLATFORM_SUPER_ADMIN[\s\S]*NOT IN \('platform\.tokens\.read', 'platform\.tokens\.manage', 'platform\.tokens\.credentials\.validate'\)/);
  assert.match(superAdminExpansion, /INSERT INTO role_permissions\(role_code,permission_code\)[\s\S]*'PLATFORM_SUPER_ADMIN',code[\s\S]*scope='PLATFORM'/);
  assert.match(superAdminExpansion, /code<>'platform\.audit\.pii\.export'/);
  assert.match(superAdminExpansion, /bump_role_assignment_scope_version[\s\S]*role_code='PLATFORM_SUPER_ADMIN'/);
  assert.doesNotMatch(rbac.match(/UNION ALL SELECT 'OWNER'[\s\S]*?UNION ALL SELECT 'AUDITOR'/)?.[0] || '', /platform\.tokens/);

  assert.match(service, /POST\("\/token-accounts"[\s\S]*platform\.tokens\.manage[\s\S]*TOKEN_ACCOUNT_CREATE/);
  assert.doesNotMatch(service, /credentials\/:credential_id\/validate"/, 'legacy direct credential validation must stay removed');
  assert.match(read('internal/token/command_governance.go'), /TOKEN_CREDENTIAL_VALIDATE[\s\S]*platform\.tokens\.credentials\.validate/);
  assert.match(command, /TOKEN_ACCOUNT_CREATE[\s\S]*AUDIT_ONLY[\s\S]*platform\.tokens\.manage/);
  assert.match(policy, /TOKEN_ACCOUNT_CREATE.*platform\.tokens\.manage/);

  assert.match(admin, /WHERE a\.management_mode='PLATFORM'/);
  assert.match(admin, /AdminTokenAccount[\s\S]*management_mode/);
  assert.match(admin, /ManagementMode: "PLATFORM"/);
  assert.match(repository, /management_mode\) VALUES[\s\S]*account\.ManagementMode/);
  assert.match(repository, /request\.ManagementMode != "PLATFORM"/);
  assert.match(repository, /ta\.management_mode='PLATFORM' AND provision\.id IS NOT NULL/);
  assert.match(admin, /CREDENTIAL_CHECK_FAILED/);
  assert.match(admin, /AdminCredentialValidationResult/);
  assert.doesNotMatch(admin.match(/type AdminCredentialMetadata struct \{([\s\S]*?)\n\}/)?.[1] || '', /BaseURL|Secret|Ciphertext|KeyID/);

  assert.match(adminAPI, /createAdminTokenAccount/);
  assert.match(adminAPI, /TokenCredentialValidationGovernedResult/);
  const policySchema = organizationAPI.match(/OrganizationModelPolicy:[\s\S]*?(?=\n    [A-Z][A-Za-z]+:)/)?.[0] || '';
  assert.doesNotMatch(policySchema, /token_account_id|credential_id|route_id|provisioning_id/);
  assert.match(portal, /resource == "\/admin\/v1\/token-accounts" && method == http\.MethodPost/);
  assert.match(client, /createTokenAccount/);
  assert.match(ui, /createTokenAccount/);
  assert.match(ui, /const canValidateCredentials = false/);
  assert.doesNotMatch(ui, /validation_status === 'VALID'/);
  assert.doesNotMatch(tokenResourcesUI, /validateTokenCredential|validation_status/);

  assert.doesNotMatch(organizationAPI, /^  \/token-accounts(?::|\/)/m);
  assert.doesNotMatch(service, /protected\.(?:GET|POST|PUT|PATCH|DELETE)\("\/v1\/token-accounts/);
  assert.doesNotMatch(read('internal/token/proxy.go'), /X-Pinvou-Token-Account-ID|ExplicitTokenAccountID/);
  assert.match(quarantine, /WHERE account\.management_mode='USER'/);
  assert.match(quarantine, /SET status='FROZEN'/);
  assert.match(quarantine, /SET status='RETIRED'/);
  assert.match(quarantine, /SET health_status='UNHEALTHY'/);
  assert.doesNotMatch(quarantine, /ciphertext|base_url|fingerprint|secret/i);
});
