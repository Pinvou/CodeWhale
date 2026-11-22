import { requireFinanceID, parseFinanceJSON } from './finance-contract.js';
import { financeNotDispatched } from './finance-operation-state.js';

export const admissionMaterialLimit = 10 * 1024 * 1024;
const mediaTypes = new Set(['image/png', 'image/jpeg', 'application/pdf']);
const digestPattern = /^[a-f0-9]{64}$/;
const invalid = () => new Error('INVALID_PRODUCT_ADMISSION_ACK');
export function requireAdmissionMaterial(value, catalogID) {
  requireFinanceID(value?.material_id);
  if (value.catalog_item_id !== catalogID || !mediaTypes.has(value.media_type) ||
      !Number.isSafeInteger(value.size) || value.size < 1 || value.size > admissionMaterialLimit ||
      !digestPattern.test(value.sha256) || !Number.isFinite(Date.parse(value.intended_expires_at))) throw invalid();
  return value;
}
export function requireManualAdmission(value, catalogID, verificationID = '') {
  requireFinanceID(value?.verification_id); requireFinanceID(value?.material_id);
  if (value.catalog_item_id !== catalogID || (verificationID && value.verification_id !== verificationID) ||
      !digestPattern.test(value.submission_sha) || !['PENDING', 'VERIFIED', 'FAILED'].includes(value.status) ||
      value.snapshot?.publication?.catalog_item_id !== catalogID || !Array.isArray(value.decisions)) throw invalid();
  return value;
}
export function validateAdmissionUpload(file, expiresAt, requireFuture = true) {
  if (!(file instanceof Blob) || !mediaTypes.has(file.type) || file.size < 1 || file.size > admissionMaterialLimit ||
      !Number.isFinite(Date.parse(expiresAt)) || (requireFuture && Date.parse(expiresAt) <= Date.now())) throw financeNotDispatched(invalid(), true);
}
async function responseError(response) {
  const payload = await response.json().catch(() => ({}));
  return Object.assign(new Error('PRODUCT_ADMISSION_REQUEST_FAILED'), { status: response.status, code: payload.code || 'REQUEST_FAILED' });
}
export async function uploadAdmissionMaterial(client, catalogID, file, version, expiresAt, key, signal) {
  try {
    requireFinanceID(catalogID); validateAdmissionUpload(file, expiresAt, false);
    if (!Number.isSafeInteger(version) || version < 1 || typeof key !== 'string' || key.length < 16 || key.length > 128) throw invalid();
  } catch (error) { throw financeNotDispatched(error, true); }
  const epoch = client.financeSessionEpoch;
  const query = new URLSearchParams({ expected_version: String(version), intended_expires_at: expiresAt });
  const response = await fetch(`${client.baseURL}/api/admin/billing/admin/v1/recharge-products/${catalogID}/admission-materials?${query}`, {
    method: 'POST', headers: { Accept: 'application/json', 'Content-Type': file.type, 'X-CSRF-Token': client.csrfToken, 'Idempotency-Key': key },
    credentials: 'same-origin', cache: 'no-store', redirect: 'error', body: file, signal,
  });
  client.requireCurrentSession(epoch, signal); client.observeAuthorizationVersions(response.headers, epoch);
  if (!response.ok) throw await responseError(response);
  const value = requireAdmissionMaterial(parseFinanceJSON(await response.text()), catalogID);
  client.requireCurrentSession(epoch, signal);
  if (response.status !== 200 || value.catalog_version !== version || value.media_type !== file.type || value.size !== file.size ||
      Date.parse(value.intended_expires_at) !== Date.parse(expiresAt)) throw invalid();
  return value;
}
export async function downloadAdmissionMaterial(client, catalogID, material, signal) {
  requireFinanceID(catalogID); requireAdmissionMaterial(material, catalogID);
  const epoch = client.financeSessionEpoch;
  const response = await fetch(`${client.baseURL}/api/admin/billing/admin/v1/recharge-products/${catalogID}/admission-materials/${material.material_id}/content`, {
    credentials: 'same-origin', cache: 'no-store', redirect: 'error', signal,
  });
  client.requireCurrentSession(epoch, signal); client.observeAuthorizationVersions(response.headers, epoch);
  if (!response.ok) throw await responseError(response);
  if (response.status !== 200 || response.headers.get('Content-Type')?.split(';')[0] !== material.media_type) throw invalid();
  // Read incrementally so an oversized or misleading upstream response never
  // becomes an unbounded Blob in the administrator's browser.
  const reader = response.body.getReader(); const chunks = []; let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read(); if (done) break;
      total += value.byteLength;
      if (total > material.size || total > admissionMaterialLimit) throw invalid();
      chunks.push(value);
    }
  } finally { await reader.cancel(); reader.releaseLock(); }
  client.requireCurrentSession(epoch, signal);
  const blob = new Blob(chunks, { type: material.media_type });
  const digest = [...new Uint8Array(await globalThis.crypto.subtle.digest('SHA-256', await blob.arrayBuffer()))].map(byte => byte.toString(16).padStart(2, '0')).join('');
  client.requireCurrentSession(epoch, signal);
  if (total !== material.size || digest !== material.sha256) throw invalid();
  return blob;
}
