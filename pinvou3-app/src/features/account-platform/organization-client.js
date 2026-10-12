import { requireFinanceCollection, requireFinanceID, integerMoney, requirePaymentOrder } from './finance-contract.js';

export function organizationID(value) { requireFinanceID(value); if (value !== value.toLowerCase()) throw new Error('INVALID_ORGANIZATION_INPUT'); return value; }
export function organizationVersion(value) { if (!Number.isSafeInteger(value) || value < 1) throw new Error('INVALID_ORGANIZATION_INPUT'); return value; }
const invalid = () => { throw new Error('INVALID_ORGANIZATION_ACK'); };
const orgPath = id => `/v1/organizations/${organizationID(id)}`;
function owned(value, org, field) { try { organizationID(org); organizationID(value?.organization_id); } catch { invalid(); } if (value.organization_id !== org) invalid(); if (field) organizationID(value[field]); return value; }
function collection(value, check) { return requireFinanceCollection(value).map(item => check(item)); }
export function requireOrganization(value, expected = '') {
 organizationID(value?.organization_id); organizationVersion(value.version);
 if (expected && value.organization_id !== expected || !['ACTIVE', 'DISABLED', 'DELETED'].includes(value.status) || typeof value.name !== 'string') invalid();
 return value;
}
export function requireMembership(value, user = '') {
 organizationID(value?.user_id); organizationID(value.membership_id); organizationVersion(value.version);
 if (user && value.user_id !== user || !['ACTIVE', 'DISABLED', 'REVOKED'].includes(value.status) || !Array.isArray(value.roles) || !value.roles.every(role => typeof role === 'string') || !Array.isArray(value.unit_ids)) invalid();
 return value;
}
function membershipAck(value, org, user) { owned(value, org); requireMembership(value.member, user); return value; }
export function organizationUnitIDs(value) { if (!Array.isArray(value)) throw new Error('INVALID_ORGANIZATION_INPUT'); return [...new Set(value.map(organizationID))].sort(); }
function memberUnitsAck(value, org, user, body) {
 membershipAck(value, org, user);
 const expected = organizationUnitIDs(body.unit_ids), current = organizationUnitIDs(value.member.unit_ids);
 if (value.member.version < body.expected_version + 1 || value.replayed !== true && (value.member.status !== 'ACTIVE' || value.member.version !== body.expected_version + 1 || JSON.stringify(current) !== JSON.stringify(expected))) invalid();
 return value;
}
function unitAck(value, org, unit = '') { owned(value?.unit, org, 'unit_id'); organizationVersion(value.structure_version); if (unit && value.unit.unit_id !== unit) invalid(); return value; }
function bindingAck(value, org, binding = '') { owned(value, org, 'binding_id'); organizationID(value.user_id); if (binding && value.binding_id !== binding || !['ACTIVE', 'REVOKED', 'EXPIRED'].includes(value.status)) invalid(); return value; }
function invitationAck(value, org = '', invitation = '') { organizationID(value?.invitation_id); organizationID(value.organization_id); if (org && value.organization_id !== org || invitation && value.invitation_id !== invitation || !['PENDING', 'ACCEPTED', 'REVOKED', 'EXPIRED'].includes(value.status)) invalid(); return value; }
function adminOrganization(value) { organizationID(value?.organization_id); if (typeof value.name !== 'string' || !['ACTIVE', 'DISABLED', 'DELETED'].includes(value.status)) invalid(); return value; }
export function requireOrganizationModelPolicy(value, org) {
 owned(value, org);
 if (!Number.isSafeInteger(value.version) || value.version < 0 || !['ALLOW_ALL', 'ALLOW_LIST', 'DENY_LIST'].includes(value.mode) || !Array.isArray(value.model_ids)) invalid();
 value.model_ids.forEach(organizationID);
 if (new Set(value.model_ids).size !== value.model_ids.length || value.mode === 'ALLOW_ALL' && value.model_ids.length) invalid();
 organizationVersion(value.authorization_version);
 return value;
}
export function requireQuotaGrant(value, org, expected = {}) {
 owned(value, org, 'grant_id');
 for (const name of ['member_user_id', 'package_id', 'component_id']) { organizationID(value[name]); if (expected[name] && value[name] !== expected[name]) invalid(); }
 if (expected.grant_id && value.grant_id !== expected.grant_id || !['ACTIVE', 'SUSPENDED', 'EXPIRED', 'REVOKED', 'RECLAIMED_ACCOUNT_CLOSED'].includes(value.status)) invalid();
 for (const name of ['original_microcredits', 'available_microcredits', 'reserved_microcredits', 'used_microcredits', 'expired_microcredits', 'revoked_microcredits']) integerMoney(value[name]);
 organizationVersion(value.membership_version); organizationVersion(value.allocation_version); return value;
}

// The same transport works over the CSRF BFF and Native bearer bridge. Keys
// and immutable request facts are supplied by the owning operation workspace.
export class OrganizationClient {
 constructor(client) { this.client = client; }
 request(area, path, method = 'GET', body, key = '', statuses = [200], signal) {
  return this.client.request(area, path, { method, body, idempotencyKey: key || undefined, headers: key ? { 'Idempotency-Key': key } : {}, expectedStatus: statuses, signal });
 }
 async organizations(signal) { return collection(await this.request('account', '/v1/me/organizations', 'GET', undefined, '', [200], signal), requireOrganization); }
 async invitations(signal) { return collection(await this.request('account', '/v1/me/invitations', 'GET', undefined, '', [200], signal), item => invitationAck(item)); }
 async switchContext(org, version) {
  const value = await this.request('account', '/v1/sessions/current/active-context', 'POST', { context_type: org ? 'ORGANIZATION' : 'PERSONAL', organization_id: org ? organizationID(org) : null, expected_context_version: organizationVersion(version) });
  if (value?.context_type !== (org ? 'ORGANIZATION' : 'PERSONAL') || org && value.organization_id !== org || !org && value.organization_id != null || ![version, version + 1].includes(value.context_version) || !Array.isArray(value.permissions)) invalid();
  return value;
 }
 async acceptInvitation(id) { const value = await this.request('account', '/v1/invitations/accept', 'POST', { invitation_id: organizationID(id) }); owned(value, value?.organization_id); return membershipAck(value, value.organization_id); }
 async acceptLink(token) { if (typeof token !== 'string' || !token.trim()) throw new Error('INVALID_ORGANIZATION_INPUT'); const value = await this.request('account', '/v1/invitations/accept', 'POST', { token: token.trim() }); return membershipAck(value, value?.organization_id); }
 async units(org, signal) { const value = await this.request('account', `${orgPath(org)}/units`, 'GET', undefined, '', [200], signal); organizationVersion(value.structure_version); collection(value, item => owned(item, org, 'unit_id')); return value; }
 async members(org, signal) { return collection(await this.request('account', `${orgPath(org)}/members`, 'GET', undefined, '', [200], signal), requireMembership); }
 async quotaMembers(org, signal) { return collection(await this.request('account', `${orgPath(org)}/quota-members`, 'GET', undefined, '', [200], signal), item => { organizationID(item.user_id); organizationVersion(item.version); return item; }); }
 async bindings(org, signal) { return collection(await this.request('account', `${orgPath(org)}/role-bindings`, 'GET', undefined, '', [200], signal), item => bindingAck(item, org)); }
 async createUnit(org, body, key) { return unitAck(await this.request('account', `${orgPath(org)}/units`, 'POST', body, key, [200, 201]), org); }
 async moveUnit(org, unit, body, key) { return unitAck(await this.request('account', `${orgPath(org)}/units/${organizationID(unit)}/parent`, 'PATCH', body, key), org, unit); }
 async invite(org, body, key) {
  const value = invitationAck(await this.request('account', `${orgPath(org)}/invitations`, 'POST', body, key, [201]), org);
  if (body.unit_ids !== undefined && JSON.stringify(organizationUnitIDs(value.unit_ids ?? [])) !== JSON.stringify(organizationUnitIDs(body.unit_ids))) invalid();
  return value;
 }
 async revokeInvitation(org, id, key) { return invitationAck(await this.request('account', `${orgPath(org)}/invitations/${organizationID(id)}`, 'DELETE', undefined, key), org, id); }
 async grantRole(org, body, key) { const value = bindingAck(await this.request('account', `${orgPath(org)}/role-bindings`, 'POST', { ...body, idempotency_key: key }, key, [201]), org); if (value.user_id !== body.user_id || value.role_code !== body.role_code || (value.unit_id || null) !== (body.unit_id || null)) invalid(); return value; }
 async revokeRole(org, id, key) { return bindingAck(await this.request('account', `${orgPath(org)}/role-bindings/${organizationID(id)}`, 'DELETE', undefined, key), org, id); }
 async replaceRoles(org, user, roles, key) { return requireMembership(await this.request('account', `${orgPath(org)}/members/${organizationID(user)}/roles`, 'PUT', { roles }, key), user); }
 async replaceUnits(org, user, unitIDs, version, key) {
  const body = { unit_ids: organizationUnitIDs(unitIDs), expected_version: organizationVersion(version) };
  return memberUnitsAck(await this.request('account', `${orgPath(org)}/members/${organizationID(user)}/units`, 'PUT', body, key), org, user, body);
 }
 async removeMember(org, user, key) { return membershipAck(await this.request('account', `${orgPath(org)}/members/${organizationID(user)}`, 'DELETE', undefined, key), org, user); }
 async transfer(org, user, key) { const value = await this.request('account', `${orgPath(org)}/ownership`, 'POST', { new_owner_user_id: organizationID(user), idempotency_key: key }, key); owned(value, org); if (value.new_owner_user_id !== user) invalid(); requireMembership(value.new_owner_membership, user); return value; }
 async balance(org, signal) { const value = await this.request('token', `${orgPath(org)}/balance`, 'GET', undefined, '', [200], signal); if (value?.owner_type !== 'ORGANIZATION' || value.owner_id !== org) invalid(); organizationID(value.wallet_id); for (const name of ['posted_microcredits', 'reserved_microcredits', 'available_microcredits']) integerMoney(value[name]); return value; }
 async modelPolicy(org, signal) { return requireOrganizationModelPolicy(await this.request('token', `${orgPath(org)}/model-policy`, 'GET', undefined, '', [200], signal), org); }
 async saveModelPolicy(org, body, key) {
  const value = requireOrganizationModelPolicy(await this.request('token', `${orgPath(org)}/model-policy`, 'PUT', body, key), org);
  const expected = [...new Set(body.model_ids)].sort();
  if (value.version !== body.expected_version + 1 || value.mode !== body.mode || JSON.stringify([...value.model_ids].sort()) !== JSON.stringify(expected)) invalid();
  return value;
 }
 async products(org, signal) { return collection(await this.request('token', `${orgPath(org)}/recharge/products`, 'GET', undefined, '', [200], signal), item => { if (typeof item.product_id !== 'string' || !item.product_id || !Array.isArray(item.channels)) invalid(); integerMoney(item.amount_fen); return item; }); }
 async capabilities(org, signal) { const value = await this.request('token', `${orgPath(org)}/payment-capabilities`, 'GET', undefined, '', [200], signal); if (!value || !Array.isArray(value.channels) || !value.channels.every(channel => typeof channel === 'string' && channel) || typeof value.test_only !== 'boolean' || typeof value.formal_collection_admitted !== 'boolean') invalid(); return value; }
 async packages(org, signal) { return collection(await this.request('token', `${orgPath(org)}/entitlement-packages`, 'GET', undefined, '', [200], signal), item => { organizationID(item.package_id); integerMoney(item.points_remaining_microcredits); return item; }); }
 async grants(org, signal) { return collection(await this.request('token', `${orgPath(org)}/quota-grants`, 'GET', undefined, '', [200], signal), item => requireQuotaGrant(item, org)); }
 async createOrder(org, body, key) { const value = requirePaymentOrder(await this.request('token', `${orgPath(org)}/recharge/orders`, 'POST', body, key, [201])); if (value.beneficiary_organization_id !== org || value.product_id !== body.product_id || value.channel !== body.channel) invalid(); return value; }
 async order(org, id, signal) { const value = requirePaymentOrder(await this.request('token', `${orgPath(org)}/recharge/orders/${organizationID(id)}`, 'GET', undefined, '', [200], signal), id); if (value.beneficiary_organization_id !== org) invalid(); return value; }
 async allocate(org, body, key) {
  const items = collection(await this.request('token', `${orgPath(org)}/quota-grants:batch`, 'POST', body, key, [207]), item => item);
  if (items.length !== body.items.length) invalid();
  items.forEach((item, index) => { if (item.index !== index || !['SUCCEEDED', 'FAILED'].includes(item.status)) invalid(); if (item.status === 'SUCCEEDED') { const grant = requireQuotaGrant(item.grant, org, body.items[index]); if (integerMoney(grant.original_microcredits) !== integerMoney(body.items[index].amount_microcredits) || grant.membership_version !== body.items[index].membership_version) invalid(); } else if (typeof item.code !== 'string' || !item.code) invalid(); });
  return { items };
 }
 async revokeGrant(org, id, reason, key) { return requireQuotaGrant(await this.request('token', `${orgPath(org)}/quota-grants/${organizationID(id)}/revoke`, 'POST', { reason }, key), org, { grant_id: id }); }
 async reclaimClosedGrant(org, id, reason, key) {
  const result = await this.request('token', `${orgPath(org)}/account-closed-grants/${organizationID(id)}/reclaim`, 'POST', { reason }, key);
  requireQuotaGrant(result?.grant, org, { grant_id: id });
  if (result.grant.status !== 'RECLAIMED_ACCOUNT_CLOSED' || integerMoney(result.amount_microcredits) <= 0n || integerMoney(result.subject_lifecycle_version) < 2n || typeof result.replayed !== 'boolean') invalid();
  return result;
 }
 async reclaimClosedGrants(org, ids, reason, key) {
  if (!Array.isArray(ids) || ids.length < 1 || ids.length > 200 || new Set(ids).size !== ids.length) throw new Error('INVALID_ORGANIZATION_INPUT');
  ids.forEach(organizationID);
  const result = await this.request('token', `${orgPath(org)}/account-closed-grants:reclaim-batch`, 'POST', { grant_ids: ids, reason }, key, [207]);
  if (!Array.isArray(result?.items) || result.items.length !== ids.length) invalid();
  for (const [index, item] of result.items.entries()) {
   if (item?.grant_id !== ids[index] || !['SUCCEEDED', 'FAILED'].includes(item.status)) invalid();
   if (item.status === 'FAILED') { if (item.code !== 'ACCOUNT_CLOSED_GRANT_RECLAIM_REJECTED') invalid(); continue; }
   const receipt = item.result;
   requireQuotaGrant(receipt?.grant, org, { grant_id: ids[index] });
   if (receipt.grant.status !== 'RECLAIMED_ACCOUNT_CLOSED' || integerMoney(receipt.amount_microcredits) <= 0n || integerMoney(receipt.subject_lifecycle_version) < 2n || typeof receipt.replayed !== 'boolean') invalid();
  }
  return result;
 }
}

export class OrganizationAdminClient {
 constructor(client) { this.client = client; }
 async list(filters = {}, signal) { const query = new URLSearchParams({ limit: '25' }); for (const name of ['q', 'status', 'cursor']) if (filters[name]) query.set(name, filters[name]); const value = await this.client.request('account', `/admin/v1/organizations?${query}`, signal); collection(value, adminOrganization); return value; }
 async read(org, resource, signal) { const value = await this.client.request('account', `/admin/v1/organizations/${organizationID(org)}/${resource}`, signal); if (resource === 'units') { organizationVersion(value.structure_version); collection(value, item => owned(item, org, 'unit_id')); } else if (resource === 'members') collection(value, requireMembership); else if (value?.organization_id !== org) invalid(); return value; }
 async members(org, cursor = '', signal) { const query = new URLSearchParams({ limit: '25' }); if (cursor) query.set('cursor', cursor); const value = await this.client.request('account', `/admin/v1/organizations/${organizationID(org)}/members?${query}`, signal); collection(value, requireMembership); return value; }
 async write(org, action, body, key, target = '') {
  const root = `/admin/v1/organizations${org ? `/${organizationID(org)}` : ''}`;
  const id = target ? organizationID(target) : '';
  const spec = {
   create: [root, 'POST', 201], rename: [root, 'PATCH', 204], status: [`${root}/status`, 'PATCH', 204], delete: [root, 'DELETE', 204],
   add: [`${root}/members`, 'POST', [200, 201]], roles: [`${root}/members/${id}/roles`, 'PUT', 200], units: [`${root}/members/${id}/units`, 'PUT', 200], remove: [`${root}/members/${id}`, 'DELETE', 200], transfer: [`${root}/ownership`, 'POST', 200],
   unit: [`${root}/units`, 'POST', [200, 201]], renameUnit: [`${root}/units/${id}`, 'PATCH', 200], moveUnit: [`${root}/units/${id}/parent`, 'PATCH', 200], deleteUnit: [`${root}/units/${id}`, 'DELETE', 200],
  }[action];
  if (!spec) throw new Error('INVALID_ORGANIZATION_INPUT');
  if (action === 'units') body = { unit_ids: organizationUnitIDs(body.unit_ids), expected_version: organizationVersion(body.expected_version) };
  const value = await this.client.mutate('account', spec[0], spec[1], body, key, undefined, spec[2]);
  if (spec[2] === 204) { if (value !== null) invalid(); return null; }
  if (action === 'create') return requireOrganization(value);
  if (['unit', 'renameUnit', 'moveUnit', 'deleteUnit'].includes(action)) return unitAck(value, org, id);
  if (action === 'roles') return requireMembership(value, id);
  if (action === 'units') return memberUnitsAck(value, org, id, body);
  if (action === 'transfer') { owned(value, org); if (value.new_owner_user_id !== body.new_owner_user_id) invalid(); requireMembership(value.new_owner_membership, body.new_owner_user_id); return value; }
  return membershipAck(value, org, action === 'add' ? body.user_id : id);
 }
}

// Products have already passed the server's current environment/admission
// filter. Capabilities and availability can still change between reads; use
// their intersection for selection, without banning controlled test payments.
export function organizationPaymentChannels(product, capabilities) {
 if (product?.availability_status !== 'AVAILABLE' || !Array.isArray(product.channels) || !Array.isArray(capabilities?.channels)) return [];
 return product.channels.filter(channel => capabilities.channels.includes(channel));
}
