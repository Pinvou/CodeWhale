import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { acquireAuditExportTicket, newAuditExportAttempt, submitAuditExportAttempt } from '../../../pinvou3-app/src/features/account-platform/admin-audit-export.js';

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

test('one-time audit download ticket is reused after an uncertain download', async () => {
  let issued = 0;
  const client = { issueAuditExportDownloadTicket: async () => { issued += 1; return { download_token: 'one-time-token' }; } };
  const ticket = await acquireAuditExportTicket(client, 'export-1', null);
  assert.equal((await acquireAuditExportTicket(client, 'export-1', ticket)).download_token, 'one-time-token');
  assert.equal(issued, 1);
  const view = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  assert.match(view, /downloadAuditExport\(exportJob\.export_id, exportTicketRef\.current\.download_token\)/);
  assert.match(view, /download_ticket_issued_at && !exportTicketRef\.current/);
});

test('audit filters have accessible names and invalid dates do not crash export', () => {
  const view = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  for (const key of ['actorFilter', 'actionFilter', 'resourceFilter', 'result']) {
    assert.match(view, new RegExp(`aria-label=\\{copy\\.audit\\.${key}\\}`));
  }
  assert.match(view, /try \{ filter = queryFilters\(\); \} catch \{ setError\(copy\.audit\.exportRequestInvalid\); return; \}/);
});

test('a definite audit export rejection releases the frozen attempt while unknown results keep it', () => {
  const view = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  // The server answering 4xx (except the two ambiguous ones) proves no export
  // job was created: the operator must be able to correct the reason or PII
  // choice instead of being trapped on a disabled form with no reset entry.
  assert.match(view, /const status = typeof cause\?\.status === 'number' \? cause\.status : 0;/);
  assert.match(view, /if \(status >= 400 && status < 500 && status !== 408 && status !== 409\) \{\s+exportAttemptRef\.current = null;\s+setExportAttempted\(false\);\s+\}/);
  // An unknown result - 5xx, idempotency conflict, timeout, network loss or
  // abort - must never release the frozen attempt, so no catch may clear it
  // unconditionally.
  assert.doesNotMatch(view, /catch \{[^}]*exportAttemptRef\.current = null/);
});
