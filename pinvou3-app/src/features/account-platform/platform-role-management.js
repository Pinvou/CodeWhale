import { secureRequestID } from './request-id.js';
import { requireFinanceID } from './finance-contract.js';
const id = value => typeof value === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value) && value !== '00000000-0000-0000-0000-000000000000';
const protectedCodes = new Set(['PLATFORM_SUPER_ADMIN', 'OWNER', 'PERSONAL_USER']);
// An empty string is an intentional platform-wide query; an invalid ID must never become one.
export function platformRoleBindingUserID(value = '') {
  const normalized = typeof value === 'string' ? value.trim().toLowerCase() : value;
  return normalized ? requireFinanceID(normalized) : '';
}

export function grantablePlatformRole(role) { return role?.scope === 'PLATFORM' && role.assignable === true && !protectedCodes.has(role.code); }
export function requirePlatformRoles(payload) {
  if (!Array.isArray(payload?.items) || payload.items.some(role => typeof role.code !== 'string' || !role.code || role.scope !== 'PLATFORM' || typeof role.assignable !== 'boolean' || !Array.isArray(role.permissions) || role.permissions.some(value => typeof value !== 'string' || !value))) throw new Error('INVALID_ROLE_CATALOG');
  return payload.items;
}
export function requirePlatformRoleBinding(value, expected = null) {
  if (!id(value?.binding_id) || !id(value.user_id) || typeof value.role_code !== 'string' || value.scope !== 'PLATFORM' || value.organization_id != null || value.unit_id != null || !['ACTIVE', 'SCHEDULED', 'EXPIRED', 'REVOKED'].includes(value.status) || typeof value.grant_source !== 'string' || !value.grant_source || !Number.isFinite(Date.parse(value.starts_at)) || value.expires_at != null && !Number.isFinite(Date.parse(value.expires_at))) throw new Error('INVALID_ROLE_BINDING_ACK');
  if (expected && (value.user_id !== expected.user_id || value.role_code !== expected.role_code || value.grant_source !== 'ADMIN' || expected.created_by && value.created_by !== expected.created_by || Date.parse(value.starts_at) !== Date.parse(expected.starts_at) || (value.expires_at == null) !== (expected.expires_at == null) || value.expires_at != null && Date.parse(value.expires_at) !== Date.parse(expected.expires_at))) throw new Error('INVALID_ROLE_BINDING_ACK');
  return value;
}
export function platformRoleGrantIntent(owner, fields, role, now = new Date()) {
  if (!id(owner) || !id(fields.user_id) || !grantablePlatformRole(role) || role.code !== fields.role_code) throw new Error('INVALID_ROLE_GRANT');
  const userID = requireFinanceID(fields.user_id.toLowerCase());
  const starts = fields.starts_at ? new Date(fields.starts_at).toISOString() : now.toISOString();
  const expires = fields.expires_at ? new Date(fields.expires_at).toISOString() : null;
  const reason = String(fields.reason || '').trim();
  if (expires && (Date.parse(expires) <= Date.parse(starts) || Date.parse(expires) <= now.getTime())) throw new Error('INVALID_ROLE_GRANT');
  const reasonBytes = new TextEncoder().encode(reason).length;
  if (role.code === 'PLATFORM_AUDIT_PII_EXPORT' && (!expires || Date.parse(expires) - Date.parse(starts) > 86400000 || reasonBytes < 10 || reasonBytes > 500)) throw new Error('INVALID_ROLE_GRANT');
  return Object.freeze({ owner, body: Object.freeze({ user_id: userID, role_code: role.code, starts_at: starts, expires_at: expires, reason, idempotency_key: secureRequestID() }) });
}
// Retain one uncertain command per client+human, including navigation away and
// back. It is not transferred across a different authenticated user or client.
const commands = new WeakMap();
export function platformRolePending(client, owner, next = undefined) {
  let owners = commands.get(client);
  if (!owners) { owners = new Map(); commands.set(client, owners); }
  if (next === null) owners.delete(owner);
  else if (next !== undefined) owners.set(owner, next);
  return owners.get(owner) || null;
}
export async function platformRoleFreshAuthority(client, owner, epoch) {
  const session = await client.session();
  if (!session?.authenticated || (client.financeSessionEpoch || 0) !== epoch) throw new Error('ROLE_AUTHORITY_CHANGED');
  const current = await client.authorization();
  if (current?.user_id !== owner || !current.platform_permissions?.includes('platform.roles.assign') || (client.financeSessionEpoch || 0) !== epoch) throw new Error('ROLE_AUTHORITY_CHANGED');
  return current;
}

export function platformRoleFailureState(error, wasUnknown, dispatched) {
  if (wasUnknown) return 'UNKNOWN';
  if (error?.status === 401 && error?.code === 'RECENT_AUTHENTICATION_REQUIRED') return 'REAUTH_NOT_DISPATCHED';
  if (!dispatched || [400, 403, 404, 409, 422].includes(error?.status)) return 'NOT_DISPATCHED';
  return 'UNKNOWN';
}
