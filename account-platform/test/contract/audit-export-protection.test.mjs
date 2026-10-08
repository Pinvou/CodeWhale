import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('audit export is server-generated, recently authenticated, redacted by default, and separately PII-authorized', async () => {
  const [migration, service, roles, contract, ui] = await Promise.all([
    read('migrations/account/000032_audit_export_protection.up.sql'),
    read('internal/account/audit_exports.go'),
    read('internal/account/role_bindings_admin.go'),
    read('contracts/openapi/admin.yaml'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
  ]);
  assert.match(migration, /PLATFORM_AUDIT_PII_EXPORT/);
  assert.match(migration, /DELETE FROM role_permissions[\s\S]*PLATFORM_SUPER_ADMIN[\s\S]*platform\.audit\.pii\.export/);
  assert.match(migration, /ACCOUNT_AUDIT_EXPORT_CREATE/);
  assert.match(migration, /ACCOUNT_AUDIT_EXPORT_DOWNLOAD_TICKET/);
  assert.match(service, /auditExportRecentAuth\s+= 15 \* time\.Minute/);
  assert.match(service, /PermissionPlatformAuditPIIExport/);
  assert.match(service, /subject_[\s\S]*hmacHex/);
  assert.match(roles, /PLATFORM_AUDIT_PII_EXPORT[\s\S]*ExpiresAt[\s\S]*24\*time\.Hour/);
  assert.match(contract, /\/account\/admin\/v1\/audit-exports:/);
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
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
    read('../scripts/account-platform.mjs'),
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
