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
  const repository = read('internal/token/postgres.go') + read('internal/token/model_account_governance.go');
  const quarantine = read('migrations/token/000022_legacy_token_account_quarantine.up.sql');
  const policy = read('internal/platform/approvalpolicy/registry.go');
  const adminAPI = read('contracts/openapi/admin.yaml');
  const organizationAPI = read('contracts/openapi/token-gateway.yaml');
  const portal = read('internal/portal/service.go');
  const client = read('../pinvou3-app/src/features/account-platform/admin-client.js');
  const ui = read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx');
  const tokenResourcesUI = read('../pinvou3-app/src/features/account-platform/AdminTokenResourcesWorkspace.jsx');
  const ownerCandidates = read('internal/token/owner_candidates.go');
  const ownerCapUp = read('migrations/token/000029_token_account_owner_cap_policy.up.sql');
  const ownerCapDown = read('migrations/token/000029_token_account_owner_cap_policy.down.sql');
  const walletReceiptUp = read('migrations/billing/000053_token_account_wallet_provisioning_receipts.up.sql');
  const walletReceiptDown = read('migrations/billing/000053_token_account_wallet_provisioning_receipts.down.sql');
  const bootstrap = read('cmd/token-policy-bootstrap/main.go');
  const compose = read('deploy/compose/compose.yaml');
  const composeRoles = read('deploy/compose/init-databases.sh');
  const helmMigrations = read('deploy/helm/account-platform/templates/migrations.yaml');
  const helmSchema = read('deploy/helm/account-platform/values.schema.json');

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
	assert.match(service, /POST\("\/token-accounts"[\s\S]*RequireAnyAnd\(nil, \[\]string\{"platform\.tokens\.manage", "platform\.tokens\.owner-candidates\.read"\}\)[\s\S]*selectLegacyTokenDirect\(legacyTokenAuditOnly\)/);
	assert.match(service, /PATCH\("\/token-accounts\/:token_account_id\/status"[\s\S]*selectLegacyTokenDirect\(legacyTokenAuditOnly\)/);
	assert.match(service, /PUT\("\/models\/:model_id\/draft"[\s\S]*selectLegacyTokenDirect\(legacyTokenAuditOnly\)/);
	assert.match(service, /POST\("\/models\/:model_id\/publish"[\s\S]*selectLegacyTokenDirect\(legacyTokenAuditOnly\)/);
	assert.match(service, /POST\("\/model-catalog:rollback"[\s\S]*selectLegacyTokenDirect\(legacyTokenApprovalRequired\)/);
  assert.doesNotMatch(service, /credentials\/:credential_id\/validate"/, 'legacy direct credential validation must stay removed');
  assert.match(read('internal/token/command_governance.go'), /TOKEN_CREDENTIAL_VALIDATE[\s\S]*platform\.tokens\.credentials\.validate/);
  assert.match(command, /TOKEN_ACCOUNT_CREATE[\s\S]*AUDIT_ONLY[\s\S]*platform\.tokens\.manage/);
  assert.match(policy, /TOKEN_ACCOUNT_CREATE.*platform\.tokens\.manage/);

  assert.match(admin, /WHERE a\.management_mode='PLATFORM'/);
  assert.match(admin, /AdminTokenAccount[\s\S]*management_mode/);
  assert.match(repository, /ManagementMode: "PLATFORM"/);
  assert.match(repository, /management_mode\) VALUES[\s\S]*'PLATFORM'/);
  assert.match(repository, /request\.ManagementMode != "PLATFORM"/);
  assert.match(repository, /ta\.management_mode='PLATFORM' AND provision\.id IS NOT NULL/);
  assert.match(admin, /CREDENTIAL_CHECK_FAILED/);
  assert.match(admin, /AdminCredentialValidationResult/);
  assert.doesNotMatch(admin.match(/type AdminCredentialMetadata struct \{([\s\S]*?)\n\}/)?.[1] || '', /BaseURL|Secret|Ciphertext|KeyID/);

  assert.match(adminAPI, /createAdminTokenAccount/);
  assert.match(adminAPI, /TokenCredentialValidationGovernedResult/);
  const policySchema = organizationAPI.match(/OrganizationModelPolicy:[\s\S]*?(?=\n    [A-Z][A-Za-z]+:)/)?.[0] || '';
  assert.doesNotMatch(policySchema, /token_account_id|credential_id|route_id|provisioning_id/);
  assert.doesNotMatch(portal, /resource == "\/admin\/v1\/token-accounts" && method == http\.MethodPost/);
  assert.doesNotMatch(client, /\n\s+(?:createTokenAccount|setTokenAccountStatus)\s*\(/);
  assert.doesNotMatch(ui, /client\.(?:createTokenAccount|setTokenAccountStatus)/);
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

  assert.match(ownerCandidates, /AuthorizationVersion int64\s+`json:"-"`/);
  assert.doesNotMatch(ownerCandidates.match(/type AdminOwnerCandidate struct \{[\s\S]*?\n\}/)?.[0] || '', /json:"authorization_version"/);
  assert.match(ownerCapUp, /CREATE TABLE public\.token_account_owner_cap_policy_revisions/);
  assert.match(ownerCapUp, /CREATE TABLE public\.token_account_owner_cap_policy_active/);
  assert.match(ownerCapUp, /BEFORE UPDATE OR DELETE ON public\.token_account_owner_cap_policy_revisions/);
  assert.match(ownerCapUp, /SECURITY DEFINER\s+SET search_path\s*=\s*pg_catalog/);
  assert.match(ownerCapUp, /REVOKE ALL ON FUNCTION public\.apply_token_account_owner_cap_policy[\s\S]*FROM PUBLIC/);
  assert.match(ownerCapUp, /SET ROLE token_cap_policy_deployer;[\s\S]*CREATE TABLE public\.token_account_owner_cap_policy_revisions/);
  assert.match(ownerCapUp, /SET ROLE token_cap_policy_deployer;[\s\S]*CREATE FUNCTION public\.apply_token_account_owner_cap_policy/);
  assert.match(ownerCapUp, /REVOKE INSERT,UPDATE,DELETE,TRUNCATE ON public\.token_account_owner_cap_policy_revisions[\s\S]*FROM token_gateway,PUBLIC/);
  assert.match(ownerCapUp, /REVOKE CREATE ON SCHEMA public FROM token_cap_policy_deployer/);
  assert.match(ownerCapUp, /principal_digest[\s\S]*session_user/);
  assert.match(ownerCapUp, /INSERT INTO public\.outbox_events[\s\S]*token\.account-owner-cap-policy\.changed\.v1/);
  assert.match(ownerCapDown, /token_command_results WHERE command_type='TOKEN_ACCOUNT_CREATE'/);
  assert.match(ownerCapDown, /RAISE EXCEPTION 'owner-cap\/provisioning history exists; keep migration 000029'/);
  assert.match(walletReceiptUp, /session_user<>'billing_migrator'/);
  assert.match(walletReceiptUp, /SET ROLE billing_wallet_receipt_deployer;[\s\S]*CREATE TABLE public\.token_account_wallet_provisioning_receipts/);
  assert.match(walletReceiptUp, /GRANT SELECT,INSERT ON public\.token_account_wallet_provisioning_receipts TO billing_service/);
  assert.match(walletReceiptDown, /wallet provisioning receipt history exists; keep migration 000053/);
  assert.match(composeRoles, /CREATE ROLE token_migrator LOGIN NOINHERIT/);
  assert.match(composeRoles, /CREATE ROLE token_policy_operator LOGIN NOINHERIT/);
  assert.match(composeRoles, /CREATE ROLE billing_migrator LOGIN NOINHERIT/);
  assert.match(compose, /token-policy-bootstrap:[\s\S]*TOKEN_POLICY_DATABASE_URL:[\s\S]*TOKEN_POLICY_MAX_PER_OWNER/);
  assert.match(bootstrap, /case "quiescence":[\s\S]*token_account_create_writers=quiesced/);
  assert.match(bootstrap, /case "readiness":[\s\S]*TOKEN_ACCOUNT_POLICY_UNAVAILABLE/);
  assert.match(helmMigrations, /tokenAccountPolicyBootstrap\.enabled[\s\S]*token-policy-database-url[\s\S]*token-policy-idempotency-key/);
  assert.match(helmSchema, /"tokenAccountPolicyBootstrap"[\s\S]*"enabled": \{ "type": "boolean" \}/);
});
