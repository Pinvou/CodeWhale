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
  assert.throws(() => newAuditExportAttempt(filter, '中文五字啊', false, 'key'), { code: 'INVALID_AUDIT_EXPORT_REQUEST' });
  assert.throws(() => newAuditExportAttempt(filter, '😀'.repeat(9), false, 'key'), { code: 'INVALID_AUDIT_EXPORT_REQUEST' });
  assert.equal(newAuditExportAttempt(filter, '😀'.repeat(10), false, 'key').reason, '😀'.repeat(10));
  assert.equal(newAuditExportAttempt(filter, '中'.repeat(167), false, 'key').reason, '中'.repeat(167));
});

test('admin audit export collects reason in the page and forwards the frozen key', () => {
  const view = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AuditWorkspace.jsx', import.meta.url), 'utf8');
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
  const view = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AuditWorkspace.jsx', import.meta.url), 'utf8');
  assert.match(view, /downloadAuditExport\(exportJob\.export_id, exportTicketRef\.current\.download_token\)/);
  assert.match(view, /download_ticket_issued_at && !exportTicketRef\.current/);
});

test('audit filters have accessible names and invalid dates do not crash export', () => {
  const view = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AuditWorkspace.jsx', import.meta.url), 'utf8');
  for (const key of ['actorFilter', 'actionFilter', 'resourceFilter', 'result']) {
    assert.match(view, new RegExp(`aria-label=\\{copy\\.audit\\.${key}\\}`));
  }
  assert.match(view, /try \{ filter = queryFilters\(\); \} catch \{ setError\(copy\.audit\.exportRequestInvalid\); return; \}/);
});

test('a first definite audit rejection releases the attempt; replay after unknown stays frozen', () => {
  const view = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AuditWorkspace.jsx', import.meta.url), 'utf8');
  // Without any unknown history, a first definite rejection permits correction.
  // A later rejected replay never proves the first attempt made no export.
  // The real React committed503 -> replay403 case covers the history boundary.
  assert.match(view, /const status = typeof cause\?\.status === 'number' \? cause\.status : 0;/);
  assert.match(view, /if \(status >= 400 && status < 500 && status !== 408 && status !== 409 && !exportOutcomeUnknownRef\.current\) \{\s+exportAttemptRef\.current = null;\s+setExportAttempted\(false\);\s+\}/);
  // An unknown result - 5xx, idempotency conflict, timeout, network loss or
  // abort - must never release the frozen attempt, so no catch may clear it
  // unconditionally.
  assert.doesNotMatch(view, /catch \{[^}]*exportAttemptRef\.current = null/);
});

import {
  newAuditExportRecovery, readAuditExportRecovery, writeAuditExportRecovery,
  clearAuditExportRecovery, requireAuditExportActor, reauthenticateAuditExport,
} from '../../../pinvou3-app/src/features/account-platform/audit-export-recovery.js';
import { auditWorkspaceCopy } from '../../../pinvou3-app/src/shared/audit-i18n.js';

const actor = '00000000-0000-4000-8000-000000000017';
const otherActor = '00000000-0000-4000-8000-000000000018';
const nonce = '00000000-0000-4000-8000-000000000019';
const exportID = '00000000-0000-4000-8000-000000000020';
const original = () => newAuditExportAttempt({ action: 'admin.role.update', from: '2026-09-01T00:00:00Z', to: '2026-09-02T00:00:00Z' }, 'Investigate original request', true, 'audit-original-key');
const memoryStorage = () => {
  const data = new Map();
  return { data, getItem: key => data.get(key) ?? null, setItem: (key, value) => data.set(key, value), removeItem: key => data.delete(key) };
};
const account = (id = actor, permissions = ['platform.audit.export', 'platform.audit.pii.export']) => ({ user_id: id, status: 'ACTIVE', active_context:{context_type:'PERSONAL',organization_id:null,membership_version:1,context_version:1,authorization_version:1,personal_authorization_version:1,platform_authorization_version:1,organization_authorization_version:1,permissions:[]}, personal_permissions: [], platform_permissions: permissions });
const authClient = (me = account()) => ({ session: async () => ({ authenticated: true }), authorization: async () => me });
const unknownRecord = () => newAuditExportRecovery({ actor, nonce, attempt: original(), outcomeUnknown: true });

test('normal audit operations never access unavailable browser storage', () => {
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'sessionStorage');
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, get() { throw new Error('storage blocked'); } });
  try {
    assert.equal(readAuditExportRecovery(actor, ''), null);
    assert.doesNotThrow(() => clearAuditExportRecovery(actor, ''));
  } finally {
    if (previous) Object.defineProperty(globalThis, 'sessionStorage', previous); else delete globalThis.sessionStorage;
  }
});

test('uncertain audit requests survive navigation with identical facts and key but no response secrets', async () => {
  const storage = memoryStorage();
  const record = unknownRecord();
  writeAuditExportRecovery(record, storage);
  const restored = readAuditExportRecovery(actor, nonce, storage);
  assert.deepEqual(restored.attempt, original());
  assert.equal(restored.outcomeUnknown, true);
  const requests = [];
  await submitAuditExportAttempt({ createAuditExport: async (...args) => requests.push(args) }, restored.attempt);
  assert.deepEqual(requests, [[original().filter, original().reason, true, original().idempotencyKey]]);
  assert.deepEqual(Object.keys(JSON.parse([...storage.data.values()][0])).sort(), ['actor', 'attempt', 'draft', 'exportID', 'nonce', 'outcomeUnknown', 'ticketIssueUnknown', 'version']);
  for (const secret of ['decryption_key', 'download_token', 'wrapped_key', 'csrf_token', 'access_token', 'refresh_token']) {
    assert.throws(() => writeAuditExportRecovery({ ...record, [secret]: 'must-not-persist' }, storage));
    assert.throws(() => newAuditExportRecovery({ actor, nonce, attempt: { ...original(), [secret]: 'must-not-persist' } }));
    assert.throws(() => newAuditExportRecovery({ actor, nonce, attempt: { ...original(), filter: { ...original().filter, [secret]: 'must-not-persist' } } }));
  }
});

test('recovery rejects another actor, nonce, corrupt shape, and secret-bearing stored records', () => {
  const storage = memoryStorage();
  writeAuditExportRecovery(unknownRecord(), storage);
  assert.throws(() => readAuditExportRecovery(otherActor, nonce, storage));
  assert.throws(() => readAuditExportRecovery(actor, otherActor, storage));
  const key = [...storage.data.keys()][0];
  storage.setItem(key, JSON.stringify({ ...unknownRecord(), actor: otherActor }));
  assert.throws(() => readAuditExportRecovery(actor, nonce, storage));
  storage.setItem(key, JSON.stringify({ ...unknownRecord(), decryption_key: 'forbidden' }));
  assert.throws(() => readAuditExportRecovery(actor, nonce, storage));
  storage.setItem(key, '{');
  assert.throws(() => readAuditExportRecovery(actor, nonce, storage));
  assert.throws(() => newAuditExportRecovery({ actor, nonce, draft: { filter: original().filter, reason: original().reason, includePII: false }, outcomeUnknown: true }));
});

test('explicit reauthentication verifies metadata before navigation and does not submit an export', async () => {
  const storage = memoryStorage();
  const calls = [];
  const client = { ...authClient(), reauthenticate: returnTo => {
    assert.deepEqual(readAuditExportRecovery(actor, nonce, storage).attempt, original());
    calls.push(returnTo);
  }, createAuditExport: () => assert.fail('reauthentication must not POST') };
  await reauthenticateAuditExport(client, unknownRecord(), { storage });
  assert.deepEqual(calls, [`/admin.html?section=audit&audit_export_stepup=${nonce}`]);
  const deniedStorage = { ...storage, setItem() { throw new Error('quota exceeded'); } };
  await assert.rejects(reauthenticateAuditExport(client, unknownRecord(), { storage: deniedStorage }));
  assert.equal(calls.length, 1);
  const lyingStorage = { getItem: () => null, setItem() {} };
  await assert.rejects(reauthenticateAuditExport(client, unknownRecord(), { storage: lyingStorage }));
  assert.equal(calls.length, 1);
});

test('expired sessions use existing login while authenticated users use the existing recent-auth entry', async () => {
  const paths = [];
  await reauthenticateAuditExport({ baseURL: '', session: async () => ({ authenticated: false }), reauthenticate: () => assert.fail('expired reauth requires a session') }, unknownRecord(), { storage: memoryStorage(), navigate: path => paths.push(path) });
  assert.equal(paths[0], '/auth/login?return_to='+encodeURIComponent(`/admin.html?section=audit&audit_export_stepup=${nonce}`));
  await assert.rejects(requireAuditExportActor({ session: async () => ({ authenticated: false }) }, actor), { status: 401, code: 'AUTH_REQUIRED' });
});

test('fresh actor and existing export permissions are rechecked; PII is required only for create or replay', async () => {
  await assert.rejects(requireAuditExportActor(authClient(account(otherActor)), actor), { code: 'AUDIT_EXPORT_RECOVERY_UNAVAILABLE' });
  await assert.rejects(requireAuditExportActor(authClient(account(actor, [])), actor), { code: 'AUDIT_EXPORT_RECOVERY_UNAVAILABLE' });
  const baseOnly = authClient(account(actor, ['platform.audit.export']));
  assert.equal((await requireAuditExportActor(baseOnly, actor)).user_id, actor);
  await assert.rejects(requireAuditExportActor(baseOnly, actor, true));
  let moved = 0;
  await assert.rejects(reauthenticateAuditExport({ ...baseOnly, reauthenticate: () => moved++ }, unknownRecord(), { storage: memoryStorage() }));
  const knownJob = newAuditExportRecovery({ actor, nonce, attempt: original(), exportID, ticketIssueUnknown: true });
  await reauthenticateAuditExport({ ...baseOnly, reauthenticate: () => moved++ }, knownJob, { storage: memoryStorage() });
  assert.equal(moved, 1); // Existing job GET/ticket/download never add a new PII policy.
});

test('recovery clearing fails closed without losing a different actor or an undeletable request', () => {
  const storage = memoryStorage();
  writeAuditExportRecovery(unknownRecord(), storage);
  assert.throws(() => clearAuditExportRecovery(actor, otherActor, storage));
  assert.equal(storage.data.size, 1);
  assert.throws(() => clearAuditExportRecovery(actor, nonce, { ...storage, removeItem() {} }));
  assert.equal(storage.data.size, 1);
  const key = [...storage.data.keys()][0];
  storage.setItem(key, JSON.stringify({ ...unknownRecord(), actor: otherActor }));
  assert.throws(() => clearAuditExportRecovery(actor, nonce, storage));
  assert.equal(storage.data.size, 1);
  writeAuditExportRecovery(unknownRecord(), storage);
  clearAuditExportRecovery(actor, nonce, storage);
  assert.equal(storage.data.size, 0);
});

test('audit recovery copy exists in all three languages with the same closed keys', () => {
  const keys = ['recoveryCleanupFailed', 'recoveryUnavailable', 'recoveryReady', 'reauthenticateHelp', 'reauthenticate', 'recoveryStorageFailed', 'recoverOriginal'];
  for (const language of ['zh', 'en', 'ja']) for (const key of keys) assert.ok(auditWorkspaceCopy[language].audit[key]?.length, `${language}:${key}`);
});
