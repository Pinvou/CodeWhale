import { newAuditExportAttempt } from './admin-audit-export.js';
import { requirePersonalAccount } from './personal-contract.js';

export const AUDIT_EXPORT_STEPUP_QUERY = 'audit_export_stepup';
const keyFor = actor => `pinvou.admin.audit-export.stepup.${actor}`;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const validID = value => typeof value === 'string' && UUID.test(value) && value !== '00000000-0000-0000-0000-000000000000';
const unavailable = () => Object.assign(new Error('audit export recovery is unavailable'), { code: 'AUDIT_EXPORT_RECOVERY_UNAVAILABLE' });
function closed(value, keys) {
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).some(key => !keys.includes(key))) throw unavailable();
}
function attempt(value) {
  closed(value, ['filter', 'reason', 'includePII', 'idempotencyKey']);
  closed(value.filter, ['actor', 'action', 'resource', 'result', 'from', 'to']);
  for (const [key, maximum] of [['actor', 320], ['action', 160], ['resource', 320], ['result', 16]]) {
    if (value.filter[key] !== undefined && (typeof value.filter[key] !== 'string' || value.filter[key].length > maximum)) throw unavailable();
  }
  if (typeof value.filter.from !== 'string' || typeof value.filter.to !== 'string' || typeof value.reason !== 'string' || typeof value.includePII !== 'boolean' || typeof value.idempotencyKey !== 'string' || value.idempotencyKey.length < 8 || value.idempotencyKey.length > 120) throw unavailable();
  const result = newAuditExportAttempt(value.filter, value.reason, value.includePII, value.idempotencyKey);
  if (result.reason !== value.reason) throw unavailable();
  return result;
}
function checked(value) {
  closed(value, ['version', 'actor', 'nonce', 'attempt', 'draft', 'outcomeUnknown', 'exportID', 'ticketIssueUnknown']);
  if (value.version !== 1 || !validID(value.actor) || !validID(value.nonce) || typeof value.outcomeUnknown !== 'boolean' || typeof value.ticketIssueUnknown !== 'boolean' || typeof value.exportID !== 'string' || value.exportID && !validID(value.exportID)) throw unavailable();
  const original = value.attempt === null ? null : attempt(value.attempt);
  let draft = null;
  if (value.draft !== null) {
    closed(value.draft, ['filter', 'reason', 'includePII']);
    const normalized = attempt({ ...value.draft, idempotencyKey: 'audit-draft-validation' });
    draft = { filter: normalized.filter, reason: normalized.reason, includePII: normalized.includePII };
  }
  if (Boolean(original) === Boolean(draft) || (value.outcomeUnknown || value.exportID) && !original) throw unavailable();
  // Never copy a job/response: it may contain a file key or a one-use token.
  return Object.freeze({ version: 1, actor: value.actor, nonce: value.nonce, attempt: original, draft,
    outcomeUnknown: value.outcomeUnknown, exportID: value.exportID, ticketIssueUnknown: value.ticketIssueUnknown });
}
export function auditExportStepupNonce(location = globalThis.location) {
  return new URLSearchParams(location?.search || '').get(AUDIT_EXPORT_STEPUP_QUERY) || '';
}
export function newAuditExportRecovery({ actor, nonce, attempt: original = null, draft = null, outcomeUnknown = false, exportID = '', ticketIssueUnknown = false }) {
  return checked({ version: 1, actor, nonce, attempt: original, draft, outcomeUnknown, exportID, ticketIssueUnknown });
}
export function readAuditExportRecovery(actor, nonce, storage) {
  if (!nonce) return null; // Normal audit use never depends on browser storage.
  storage ??= globalThis.sessionStorage;
  if (!validID(actor) || !validID(nonce)) throw unavailable();
  const raw = storage.getItem(keyFor(actor));
  if (!raw) throw unavailable();
  const result = checked(JSON.parse(raw));
  if (result.actor !== actor || result.nonce !== nonce) throw unavailable();
  return result;
}
export function writeAuditExportRecovery(value, storage = globalThis.sessionStorage) {
  const record = checked(value);
  const raw = JSON.stringify(record);
  storage.setItem(keyFor(record.actor), raw);
  if (storage.getItem(keyFor(record.actor)) !== raw) throw unavailable();
  return record;
}
export function clearAuditExportRecovery(actor, nonce, storage) {
  if (!nonce) return;
  if (!validID(actor) || !validID(nonce)) throw unavailable();
  storage ??= globalThis.sessionStorage;
  const raw = storage.getItem(keyFor(actor));
  if (raw) {
    const record = checked(JSON.parse(raw));
    if (record.actor !== actor || record.nonce !== nonce) throw unavailable();
  }
  storage.removeItem(keyFor(actor));
  if (storage.getItem(keyFor(actor)) !== null) throw unavailable();
  const url = new URL(globalThis.location?.href || 'https://invalid.local/');
  url.searchParams.delete(AUDIT_EXPORT_STEPUP_QUERY);
  globalThis.history?.replaceState(globalThis.history.state, '', `${url.pathname}${url.search}${url.hash}`);
}
export async function requireAuditExportActor(client, actor, includePII = false) {
  const session = await client.session();
  if (!session.authenticated) throw Object.assign(unavailable(), { code: 'AUTH_REQUIRED', status: 401 });
  const me = requirePersonalAccount(await client.authorization());
  if (me.user_id !== actor || !me.platform_permissions.includes('platform.audit.export') || includePII && !me.platform_permissions.includes('platform.audit.pii.export')) throw unavailable();
  return me;
}
export async function reauthenticateAuditExport(client, value, { storage = globalThis.sessionStorage, navigate = url => globalThis.location.assign(url) } = {}) {
  const record = checked(value);
  const session = await client.session();
  if (session.authenticated) await requireAuditExportActor(client, record.actor, Boolean(record.attempt && !record.exportID && record.attempt.includePII));
  writeAuditExportRecovery(record, storage); // Verify before abandoning the in-memory state.
  const returnTo = `/admin.html?section=audit&${AUDIT_EXPORT_STEPUP_QUERY}=${encodeURIComponent(record.nonce)}`;
  if (session.authenticated) client.reauthenticate(returnTo);
  else navigate(`${client.baseURL || ''}/auth/login?return_to=${encodeURIComponent(returnTo)}`);
}
