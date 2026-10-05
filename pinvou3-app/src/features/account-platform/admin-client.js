function newRequestUUID() {
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('');
  return [hex.slice(0, 8), hex.slice(8, 12), hex.slice(12, 16), hex.slice(16, 20), hex.slice(20)].join('-');
}

function errorFromResponse(response, payload) {
  const error = new Error(payload?.message || `HTTP ${response.status}`);
  error.code = payload?.code || 'REQUEST_FAILED';
  error.status = response.status;
  return error;
}

async function responsePayload(response) {
  if (response.status === 204) return {};
  try {
    const payload = await response.json();
    if (response.ok && (!payload || typeof payload !== 'object' || Array.isArray(payload))) {
      throw new Error('invalid response');
    }
    return payload;
  } catch {
    if (!response.ok) return {};
    throw errorFromResponse(response, { code: 'REQUEST_FAILED' });
  }
}

export class AdminPlatformClient {
  constructor(baseURL = '') {
    this.baseURL = String(baseURL || '').replace(/\/$/, '');
    this.csrfToken = '';
    this.pendingRefunds = new Map();
  }

  async session(signal) {
    const response = await fetch(`${this.baseURL}/api/session`, { credentials: 'same-origin', cache: 'no-store', signal });
    if (response.status === 401) { this.csrfToken = ''; return { authenticated: false }; }
    if (!response.ok) throw errorFromResponse(response, await response.json().catch(() => ({})));
    const payload = await responsePayload(response);
    if (response.status !== 200 || payload.authenticated !== true || typeof payload.csrf_token !== 'string' || !payload.csrf_token) {
      throw errorFromResponse(response, { code: 'REQUEST_FAILED' });
    }
    this.csrfToken = payload.csrf_token;
    return payload;
  }

  async request(area, resource, signal) {
    const response = await fetch(`${this.baseURL}/api/admin/${area}${resource}`, {
      headers: { Accept: 'application/json' },
      credentials: 'same-origin',
      cache: 'no-store',
      signal,
    });
    const payload = await responsePayload(response);
    if (!response.ok) {
      if (response.status === 401) this.csrfToken = '';
      throw errorFromResponse(response, payload);
    }
    return payload;
  }

  async mutate(area, resource, method, body, idempotencyKey) {
    const response = await fetch(`${this.baseURL}/api/admin/${area}${resource}`, {
      method,
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
        'X-CSRF-Token': this.csrfToken,
        ...(idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : {}),
      },
      credentials: 'same-origin',
      cache: 'no-store',
      // oxlint-disable-next-line unicorn/no-invalid-fetch-options -- Callers supply POST/PUT/PATCH/DELETE; the rule treats a dynamic method as GET.
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const payload = await responsePayload(response);
    if (!response.ok) {
      if (response.status === 401) this.csrfToken = '';
      throw errorFromResponse(response, payload);
    }
    return payload;
  }

  accountOverview(signal) { return this.request('account', '/admin/v1/overview', signal); }
  roles(signal) { return this.request('account', '/admin/v1/roles', signal); }
  users(search, signal) { return this.request('account', `/admin/v1/users?limit=100&q=${encodeURIComponent(search || '')}`, signal); }
  userMemberships(userID, signal) { return this.request('account', `/admin/v1/users/${encodeURIComponent(userID)}/memberships`, signal); }
  setUserStatus(userID, status) { return this.mutate('account', `/admin/v1/users/${encodeURIComponent(userID)}/status`, 'PATCH', { status }); }
  deleteUser(userID) { return this.mutate('account', `/admin/v1/users/${encodeURIComponent(userID)}`, 'DELETE'); }
  organizations(search, signal) { return this.request('account', `/admin/v1/organizations?limit=100&q=${encodeURIComponent(search || '')}`, signal); }
  createOrganization(name, ownerUserID) { return this.mutate('account', '/admin/v1/organizations', 'POST', { name, owner_user_id: ownerUserID }); }
  renameOrganization(organizationID, name) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}`, 'PATCH', { name }); }
  setOrganizationStatus(organizationID, status) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/status`, 'PATCH', { status }); }
  deleteOrganization(organizationID) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}`, 'DELETE'); }
  organizationMembers(organizationID, signal) { return this.request('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members`, signal); }
  addOrganizationMember(organizationID, userID, roles, unitAdminUnitIDs) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members`, 'POST', { user_id:userID, roles, unit_admin_unit_ids:unitAdminUnitIDs }); }
  replaceOrganizationMemberRoles(organizationID, userID, roles, unitAdminUnitIDs) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members/${encodeURIComponent(userID)}/roles`, 'PUT', { roles, unit_admin_unit_ids:unitAdminUnitIDs }); }
  removeOrganizationMember(organizationID, userID) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members/${encodeURIComponent(userID)}`, 'DELETE'); }
  transferOrganizationOwnership(organizationID, userID) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/ownership`, 'POST', { new_owner_user_id:userID }); }
  organizationUnits(organizationID, signal) { return this.request('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units`, signal); }
  createOrganizationUnit(organizationID, name, parentID = null) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units`, 'POST', { name, parent_id: parentID }); }
  renameOrganizationUnit(organizationID, unitID, name) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units/${encodeURIComponent(unitID)}`, 'PATCH', { name }); }
  deleteOrganizationUnit(organizationID, unitID) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units/${encodeURIComponent(unitID)}`, 'DELETE'); }
  audits(filters = {}, signal) {
    const query = new URLSearchParams({ limit:'200' });
    for (const key of ['actor','action','resource','result','from','to']) if (filters[key]) query.set(key, filters[key]);
    return this.request('account', `/admin/v1/audit-logs?${query}`, signal);
  }
  audit(auditID, signal) { return this.request('account', `/admin/v1/audit-logs/${encodeURIComponent(auditID)}`, signal); }
  billingOverview(signal) { return this.request('billing', '/admin/v1/overview', signal); }
  wallets(signal) { return this.request('billing', '/admin/v1/wallets?limit=100', signal); }
  walletLedger(walletID, signal) { return this.request('billing', `/admin/v1/wallets/${encodeURIComponent(walletID)}/ledger?limit=200`, signal); }
  priceVersions(signal) { return this.request('billing', '/admin/v1/pricing-versions?limit=100', signal); }
  createPriceVersion(input) { return this.mutate('billing', '/admin/v1/pricing-versions', 'POST', input); }
  paymentOrders(filters = {}, signal) {
    const query = new URLSearchParams({ limit:'100' });
    if (filters.q) query.set('q', filters.q);
    if (filters.status) query.set('status', filters.status);
    return this.request('billing', `/admin/v1/payment-orders?${query}`, signal);
  }
  syncPaymentOrder(orderID) { return this.mutate('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/sync`, 'POST', {}); }
  repairPaymentOrder(orderID, reason) { return this.mutate('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/repair`, 'POST', { reason }); }
  refundRequestKey(orderID, amountFen, reason) {
    const facts = JSON.stringify([orderID, amountFen, reason]);
    if (!this.pendingRefunds.has(facts)) this.pendingRefunds.set(facts, newRequestUUID());
    return this.pendingRefunds.get(facts);
  }

  async refundPaymentOrder(orderID, amountFen, reason, idempotencyKey) {
    if (typeof idempotencyKey !== 'string' || idempotencyKey.length < 16) throw new TypeError('refund idempotency key required');
    const result = await this.mutate('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/refunds`, 'POST', { amount_fen:amountFen, reason }, idempotencyKey);
    const facts = JSON.stringify([orderID, amountFen, reason]);
    if (this.pendingRefunds.get(facts) === idempotencyKey) this.pendingRefunds.delete(facts);
    return result;
  }
  adjustments(signal) { return this.request('billing', '/admin/v1/adjustments?limit=100', signal); }
  decideAdjustment(adjustmentID, decision, reason) { return this.mutate('billing', `/admin/v1/adjustments/${encodeURIComponent(adjustmentID)}/${decision}`, 'POST', { reason }); }
  holds(signal) { return this.request('billing', '/admin/v1/usage-holds?limit=100', signal); }
  releaseHold(holdID, reason) { return this.mutate('billing', `/admin/v1/usage-holds/${encodeURIComponent(holdID)}/release`, 'POST', { reason }); }
  reconciliationRuns(signal) { return this.request('billing', '/admin/v1/reconciliation-runs?limit=100', signal); }
  runReconciliation(channel, businessDate) { return this.mutate('billing', '/admin/v1/reconciliation-runs', 'POST', { channel, business_date:businessDate }); }
  reconciliationCases(runID, signal) { return this.request('billing', `/admin/v1/reconciliation-runs/${encodeURIComponent(runID)}/cases?limit=200`, signal); }
  tokenOverview(signal) { return this.request('token', '/admin/v1/overview', signal); }
  tokenAccounts(signal) { return this.request('token', '/admin/v1/token-accounts?limit=100', signal); }
  tokenAccount(tokenAccountID, signal) { return this.request('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}`, signal); }
  setTokenAccountStatus(tokenAccountID, status, reason) { return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/status`, 'PATCH', { status, reason }); }
  upsertTokenRoute(tokenAccountID, route) { return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/routes`, 'PUT', route); }
  deleteTokenRoute(tokenAccountID, model) { return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/routes`, 'DELETE', { model }); }
  validateTokenCredential(tokenAccountID, credentialID) { return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/credentials/${encodeURIComponent(credentialID)}/validate`, 'POST', {}); }
  usageRequests(signal) { return this.request('token', '/admin/v1/usage-requests?limit=100', signal); }
  usageRequest(usageRequestID, signal) { return this.request('token', `/admin/v1/usage-requests/${encodeURIComponent(usageRequestID)}`, signal); }
  retryUsageReconciliation(usageRequestID, reason) { return this.mutate('token', `/admin/v1/usage-requests/${encodeURIComponent(usageRequestID)}/retry-reconciliation`, 'POST', { reason }); }
  systemStatus(signal) { return this.request('system', '/status', signal); }

  async logout() {
    const response = await fetch(`${this.baseURL}/api/logout`, {
      method: 'POST',
      headers: { 'X-CSRF-Token': this.csrfToken },
      credentials: 'same-origin',
      cache: 'no-store',
    });
    if (!response.ok) throw errorFromResponse(response, await response.json().catch(() => ({})));
    this.csrfToken = '';
    this.pendingRefunds.clear();
  }
}
