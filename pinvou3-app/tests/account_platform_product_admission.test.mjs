import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { admissionMaterialLimit, validateAdmissionUpload } from '../src/features/account-platform/product-admission-client.js';
import { productAdmissionCopy } from '../src/shared/product-admission-i18n.js';

const catalogID = '11111111-1111-4111-8111-111111111111';
const materialID = '22222222-2222-4222-8222-222222222222';
const verificationID = '33333333-3333-4333-8333-333333333333';
const expires = '2099-10-31T10:00:00Z';
const bytes = Buffer.from('controlled original bytes');
const material = { material_id: materialID, catalog_item_id: catalogID, catalog_version: 1, media_type: 'application/pdf', size: bytes.length, sha256: createHash('sha256').update(bytes).digest('hex'), intended_expires_at: expires };
const review = { verification_id: verificationID, catalog_item_id: catalogID, material_id: materialID, submission_sha: 'b'.repeat(64), status: 'PENDING', snapshot: { publication: { catalog_item_id: catalogID, catalog_version: 1 } }, decisions: [] };
const response = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });

test('original upload sends only raw bytes, scoped metadata and existing CSRF/key; ordinary writes stay JSON', async t => {
  const client = new AdminPlatformClient(); client.csrfToken = 'controlled-csrf'; const calls = [];
  t.mock.method(globalThis, 'fetch', async (url, options) => { calls.push({ url, options }); return response(material, 200); });
  const file = new Blob([bytes], { type: 'application/pdf' });
  assert.deepEqual(await client.uploadProductAdmissionMaterial(catalogID, file, 1, expires, 'controlled-operation-key'), material);
  const { url, options } = calls[0];
  assert.equal(new URL(url, 'https://example.invalid').searchParams.get('intended_expires_at'), expires);
  assert.equal(options.body, file); assert.equal(options.headers['X-CSRF-Token'], client.csrfToken);
  assert.equal(options.headers['Idempotency-Key'], 'controlled-operation-key');
  assert.equal(options.cache, 'no-store'); assert.equal(options.redirect, 'error');
  assert.equal(url.includes('filename'), false);
  await client.updateRechargeProduct(catalogID, { expected_version: 1 }, 'controlled-json-key').catch(() => {});
  assert.equal(calls[1].options.headers['Content-Type'], 'application/json');
  assert.equal(calls[1].options.body, '{"expected_version":1}');
});
test('invalid or oversized uploads never dispatch', async t => {
  let count = 0; t.mock.method(globalThis, 'fetch', async () => { count++; return response(material, 200); });
  const client = new AdminPlatformClient();
  for (const file of [new Blob([]), new Blob(['x'], { type: 'text/html' }), new Blob([new Uint8Array(admissionMaterialLimit + 1)], { type: 'image/png' })]) {
    await assert.rejects(client.uploadProductAdmissionMaterial(catalogID, file, 1, expires, 'controlled-operation-key'), error => error.financeRequestState === 'NOT_DISPATCHED');
  }
  assert.throws(() => validateAdmissionUpload(new Blob(['x'], { type: 'image/png' }), '2000-01-01T00:00:00Z'));
  assert.equal(count, 0);
});
test('download validates actual bytes, length and hash; no silent truncation or stale identity delivery', async t => {
  const client = new AdminPlatformClient(); let returned = bytes;
  t.mock.method(globalThis, 'fetch', async () => new Response(returned, { headers: { 'Content-Type': 'application/pdf' } }));
  assert.deepEqual(Buffer.from(await (await client.downloadProductAdmissionMaterial(catalogID, material)).arrayBuffer()), bytes);
  returned = Buffer.concat([bytes, Buffer.from('extra')]);
  await assert.rejects(client.downloadProductAdmissionMaterial(catalogID, material));
  returned = Buffer.from('x'.repeat(bytes.length));
  await assert.rejects(client.downloadProductAdmissionMaterial(catalogID, material));
  returned = bytes.subarray(1);
  await assert.rejects(client.downloadProductAdmissionMaterial(catalogID, material));
  t.mock.method(globalThis, 'fetch', async () => { client.retireSession(); return new Response(bytes, { headers: { 'Content-Type': 'application/pdf' } }); });
  await assert.rejects(client.downloadProductAdmissionMaterial(catalogID, material), error => error.name === 'AbortError');
});
test('manual create and explicit votes use frozen facts, original key and exact scoped paths', async t => {
  const client = new AdminPlatformClient(); const calls = [];
  t.mock.method(globalThis, 'fetch', async (url, options = {}) => { calls.push({ url, options }); return response(review, url.endsWith('/manual-admission-submissions') ? 201 : 200); });
  const body = { expected_version: 1, material_id: materialID, reason: 'Controlled reason' };
  await client.submitManualProductAdmission(catalogID, body, 'controlled-create-key');
  const decision = { expected_version: 1, submission_sha: review.submission_sha, decision: 'APPROVE', reason: 'Checked original' };
  await client.decideManualProductAdmission(catalogID, verificationID, decision, 'controlled-vote-key');
  await client.decideManualProductAdmission(catalogID, verificationID, decision, 'controlled-vote-key');
  assert.equal(calls.length, 3); assert.equal(calls[0].url.endsWith('/manual-admission-submissions'), true);
  assert.equal(calls[1].url.endsWith(`/${verificationID}/decisions`), true);
  assert.equal(calls[1].options.body, calls[2].options.body);
  assert.equal(calls[1].options.headers['Idempotency-Key'], calls[2].options.headers['Idempotency-Key']);
  assert.equal(calls[0].options.body.includes('snapshot'), false);
  t.mock.method(globalThis, 'fetch', async () => response({ ...review, catalog_item_id: materialID }, 201));
  await assert.rejects(client.submitManualProductAdmission(catalogID, body, 'controlled-create-key'));
});
test('all new product-admission labels are translated without fallback', () => {
  for (const language of ['en', 'ja']) {
    assert.deepEqual(Object.keys(productAdmissionCopy[language]).sort(), Object.keys(productAdmissionCopy.zh).sort());
    assert.ok(Object.values(productAdmissionCopy[language]).every(value => typeof value === 'string' && value.length));
  }
});
