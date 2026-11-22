import assert from 'node:assert/strict';
import test from 'node:test';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
const id = 'ec4df7c4-2bd1-4cbb-a267-2018abf90f63';
const other = 'e5447cdb-c62f-49f7-a72e-613ae1d56d35';
const mediaType = 'application/vnd.pinvou.audit-envelope+json';
async function withFetch(fn) {
  const before = globalThis.fetch;
  const client = new AdminPlatformClient(); client.csrfToken = 'controlled-csrf';
  try { await fn(client); } finally { globalThis.fetch = before; }
}
test('actual audit read methods encode only supported filters and bind detail ID', () => withFetch(async client => {
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return Response.json({ items: [] }); };
  await client.audits({ actor: 'a&b', from: '2026-10-06T00:00:00Z', forged: 'ignored' });
  await client.audit(id);
  const query = new URL(calls[0].url, 'https://controlled.invalid').searchParams;
  assert.equal(query.get('actor'), 'a&b'); assert.equal(query.has('forged'), false);
  assert.ok(calls[1].url.endsWith('/audit-logs/' + id));
  assert.equal(calls[0].options.cache, 'no-store');
  await assert.rejects(async () => client.audit('../other'));
}));
test('status read binds exact job and never exposes an upstream key', () => withFetch(async client => {
  globalThis.fetch = async () => Response.json({ export_id: id, status: 'READY', decryption_key: 'controlled-key', decryption_key_format: 'format' });
  const result = await client.auditExport(id);
  assert.equal(result.export_id, id); assert.equal('decryption_key' in result, false); assert.equal('decryption_key_format' in result, false);
  globalThis.fetch = async () => Response.json({ export_id: other, status: 'READY' });
  await assert.rejects(client.auditExport(id), /INVALID_AUDIT_EXPORT_ACK/);
}));
test('create request carries frozen body/header key, PII and CSRF; destroyed-key terminal replay is authoritative', () => withFetch(async client => {
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return Response.json({ export_id: id, status: 'EXPIRED', replayed: true }, { status: 202 }); };
  const filter = { from: '2026-10-06T00:00:00Z', to: '2026-10-07T00:00:00Z' };
  assert.equal((await client.createAuditExport(filter, 'controlled justification', true, 'controlled-export-key')).status, 'EXPIRED');
  assert.deepEqual(JSON.parse(calls[0].options.body), { filter, reason: 'controlled justification', include_pii: true, idempotency_key: 'controlled-export-key' });
  assert.equal(calls[0].options.headers['Idempotency-Key'], 'controlled-export-key');
  assert.equal(calls[0].options.headers['X-CSRF-Token'], 'controlled-csrf'); assert.equal(calls[0].options.cache, 'no-store');
}));
for (const status of [400, 429, 409]) test('actual create error preserves status ' + status, () => withFetch(async client => {
  globalThis.fetch = async () => Response.json({ code: 'CONTROLLED_REJECTION' }, { status, headers: { 'Retry-After': '3' } });
  await assert.rejects(client.createAuditExport({}, 'controlled justification', false, 'controlled-key'), error => error.status === status && error.code === 'CONTROLLED_REJECTION' && error.retryAfterSeconds === 3);
}));
test('ticket acquisition is exact POST with CSRF and no persisted key/token cache', () => withFetch(async client => {
  let call;
  globalThis.fetch = async (url, options) => { call = { url, options }; return Response.json({ export_id: id, download_token: 'a'.repeat(43), expires_at: '2099-01-01T00:00:00Z' }, { status: 201 }); };
  await client.issueAuditExportDownloadTicket(id);
  assert.equal(call.options.method, 'POST'); assert.equal(call.options.headers['X-CSRF-Token'], 'controlled-csrf');
  assert.equal(call.options.cache, 'no-store'); assert.equal(client.inflightReads.size, 0);
}));
test('download reads full response once, supplies opaque token only in header and refuses redirects', () => withFetch(async client => {
  const payload = 'controlled encrypted envelope'.repeat(100); let calls = 0;
  globalThis.fetch = async (url, options) => {
    calls++; assert.equal(new URL(url, 'https://controlled.invalid').search, '');
    assert.equal(options.headers['X-Pinvou-Download-Token'], 'a'.repeat(43));
    assert.equal(options.headers.Accept, mediaType); assert.equal(options.credentials, 'same-origin');
    assert.equal(options.cache, 'no-store'); assert.equal(options.redirect, 'error');
    return new Response(payload, { headers: { 'Content-Type': mediaType, 'Content-Length': String(payload.length) } });
  };
  assert.equal(await (await client.downloadAuditExport(id, 'a'.repeat(43))).text(), payload);
  assert.equal(calls, 1); assert.equal(client.inflightReads.size, 0);
}));
for (const [name, body, headers] of [
  ['truncated', 'short', { 'Content-Type': mediaType, 'Content-Length': '10' }],
  ['empty', '', { 'Content-Type': mediaType }],
  ['wrong type', 'body', { 'Content-Type': 'text/html' }],
  ['invalid length', 'body', { 'Content-Type': mediaType, 'Content-Length': '-1' }],
  ['over limit', 'body', { 'Content-Type': mediaType, 'Content-Length': String(500 * 1024 * 1024 + 1) }],
]) test('download rejects ' + name + ' without retrying one-time ticket', () => withFetch(async client => {
  let calls = 0; globalThis.fetch = async () => { calls++; return new Response(body, { headers }); };
  await assert.rejects(client.downloadAuditExport(id, 'a'.repeat(43)), /INVALID_AUDIT_EXPORT_DOWNLOAD/);
  assert.equal(calls, 1);
}));
test('complete download without Content-Length is supported; unauthorized download remains an error', () => withFetch(async client => {
  globalThis.fetch = async () => new Response('complete controlled body', { headers: { 'Content-Type': mediaType } });
  assert.equal((await client.downloadAuditExport(id, 'a'.repeat(43))).size, 24);
  globalThis.fetch = async () => Response.json({ code: 'PERMISSION_DENIED' }, { status: 403 });
  await assert.rejects(client.downloadAuditExport(id, 'a'.repeat(43)), error => error.status === 403);
}));
