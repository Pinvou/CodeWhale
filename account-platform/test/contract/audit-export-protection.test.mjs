import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('audit export is server-generated, recently authenticated, redacted by default, and separately PII-authorized', async () => {
  const [migration, service, roles, roleValidation, contract, ui] = await Promise.all([
    read('migrations/account/000032_audit_export_protection.up.sql'),
    read('internal/account/audit_exports.go'),
    read('internal/account/role_bindings_admin.go'),
    read('internal/account/platform_roles.go'),
    read('contracts/openapi/audit-export.json'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
  ]);
  assert.match(migration, /PLATFORM_AUDIT_PII_EXPORT/);
  assert.match(migration, /DELETE FROM role_permissions[\s\S]*PLATFORM_SUPER_ADMIN[\s\S]*platform\.audit\.pii\.export/);
  assert.match(migration, /ACCOUNT_AUDIT_EXPORT_CREATE/);
  assert.match(migration, /ACCOUNT_AUDIT_EXPORT_DOWNLOAD_TICKET/);
  assert.match(service, /auditExportRecentAuth\s+= 15 \* time\.Minute/);
  assert.match(service, /PermissionPlatformAuditPIIExport/);
  assert.match(service, /subject_[\s\S]*hmacHex/);
  assert.match(roles, /if err = validatePlatformRoleGrant\(request, principal, validationNow\); err != nil/);
  assert.match(roleValidation, /PLATFORM_AUDIT_PII_EXPORT[\s\S]*ExpiresAt[\s\S]*24\*time\.Hour/);
  assert.match(roleValidation, /recentlyAuthenticated\(principal\.AuthenticatedAt, now\)/);
  assert.match(contract, /"\/admin\/v1\/audit-exports"\s*:/);
  assert.doesNotMatch(ui, /function exportAuditCSV/);
});

test('audit export enforces range row size concurrency daily and secret-safe envelope limits', async () => {
  const [service, migration] = await Promise.all([
    read('internal/account/audit_exports.go'),
    read('migrations/account/000032_audit_export_protection.up.sql'),
  ]);
  for (const limit of ['31 * 24 * time.Hour', '100_000', '500 * 1024 * 1024', 'active >= 2', 'daily >= 10']) assert.match(service, new RegExp(limit.replaceAll('*', '\\*').replaceAll('.', '\\.')));
  assert.match(service, /SELECT id FROM users WHERE id=\$1 FOR UPDATE/);
  assert.match(service, /change_summary_sha256/);
  assert.match(service, /AES-256-GCM/);
  assert.match(service, /HMAC-SHA-256/);
  assert.match(service, /WATERMARK/);
  assert.match(service, /safeAuditCSVCell/);
  assert.match(migration, /audit_export_events_immutable/);
});

test('audit export download tickets expire in fifteen minutes and are consumed with payload deletion once', async () => {
  const [service, portal, deploy, runner] = await Promise.all([
    read('internal/account/audit_exports.go'),
    read('internal/portal/service.go'),
    read('deploy/production/compose.audit-export.yaml'),
    read('cmd/account-service/main.go'),
  ]);
  assert.match(service, /auditExportTicketTTL\s+= 15 \* time\.Minute/);
  assert.match(service, /status='DOWNLOADED',file_payload=NULL,download_token_digest=NULL/);
  assert.match(service, /download_token_digest=NULL,wrapped_file_key_nonce=NULL,wrapped_file_key=NULL/);
  assert.doesNotMatch(service, /status='READY',wrapped_file_key_nonce=NULL,wrapped_file_key=NULL/);
  assert.match(service, /DOWNLOAD_FAILED/);
  assert.match(service, /status='EXPIRED'[\s\S]*file_payload=NULL/);
  assert.match(portal, /X-Pinvou-Download-Token/);
  assert.match(portal, /audit-exports/);
  assert.match(deploy, /AUDIT_EXPORT_MASTER_KEY_BASE64/);
  assert.match(runner, /AUDIT_EXPORT_MASTER_KEY_BASE64/);
});

test('audit export overlay preserves the existing migrator service and private configuration inheritance', async () => {
 const [base, overlay] = await Promise.all([read('deploy/production/compose.personal.yaml'),read('deploy/production/compose.audit-export.yaml')]);
 assert.match(overlay,/^name: pinvou-personal$/m);
 assert.match(base,/^  account-migrate:\n[\s\S]*?^    env_file:\n[\s\S]*?ACCOUNT_MIGRATION_PRIVATE_ENV_FILE/m);
 assert.match(overlay,/^  account-migrate:\n    image: \$\{MIGRATE_IMAGE:/m);
 assert.doesNotMatch(overlay,/^  migrate-account:/m);
 assert.match(overlay,/^  account-service:\n    image: \$\{ACCOUNT_SERVICE_IMAGE:/m);
 assert.match(overlay,/^  token-portal:\n    image: \$\{TOKEN_PORTAL_IMAGE:/m);
 // The overlay changes images only: it cannot replace the base's private
 // DSN/pgpass, network or authority configuration with an empty new service.
 assert.doesNotMatch(overlay,/^    (?:env_file|networks|volumes|command):/m);
});
