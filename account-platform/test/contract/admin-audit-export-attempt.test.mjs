import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { newAuditExportAttempt, submitAuditExportAttempt } from '../../../pinvou3-app/src/features/account-platform/admin-audit-export.js';

test('audit export retry preserves one frozen filter, reason, PII choice, and idempotency key', async () => {
  const filter = { action: 'admin.role.update', from: '2026-09-01T00:00:00.000Z', to: '2026-09-02T00:00:00.000Z' };
  const attempt = newAuditExportAttempt(filter, '  investigate access  ', false, 'audit-export-fixed');
  filter.action = 'another';
  const requests = [];
  const client = { createAuditExport: async (...args) => { requests.push(args); if (requests.length === 1) throw new Error('lost response'); return { export_id: 'same-job' }; } };
  await assert.rejects(submitAuditExportAttempt(client, attempt));
  assert.equal((await submitAuditExportAttempt(client, attempt)).export_id, 'same-job');
  assert.deepEqual(requests, [
    [{ action: 'admin.role.update', from: filter.from, to: filter.to }, 'investigate access', false, 'audit-export-fixed'],
    [{ action: 'admin.role.update', from: filter.from, to: filter.to }, 'investigate access', false, 'audit-export-fixed'],
  ]);
  assert.throws(() => newAuditExportAttempt(filter, 'short', false, 'key'), { code: 'INVALID_AUDIT_EXPORT_REQUEST' });
});

test('admin audit export collects reason in the page and forwards the frozen key', () => {
  const view = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const client = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/admin-client.js', import.meta.url), 'utf8');
  assert.doesNotMatch(view, /window\.prompt\(copy\.audit\.exportReason\)/);
  assert.match(view, /newAuditExportAttempt\(filter, exportReason, includePII/);
  assert.match(view, /submitAuditExportAttempt\(client, exportAttemptRef\.current\)/);
  assert.match(client, /createAuditExport\(filter, reason, includePII = false, idempotencyKey/);
  assert.match(client, /idempotency_key: idempotencyKey/);
});
