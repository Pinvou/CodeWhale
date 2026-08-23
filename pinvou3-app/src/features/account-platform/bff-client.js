function errorFromResponse(response, payload) {
  const error = new Error(payload && payload.message ? payload.message : `HTTP ${response.status}`);
  error.code = payload && payload.code ? payload.code : 'REQUEST_FAILED';
  error.status = response.status;
  return error;
}

export class AccountPlatformBffClient {
  constructor(baseURL = '') {
    this.baseURL = String(baseURL || '').replace(/\/$/, '');
    this.csrfToken = '';
    this.contextVersion = 0;
    this.contextVersionListeners = new Set();
  }

  onContextVersion(listener) {
    this.contextVersionListeners.add(listener);
    return () => this.contextVersionListeners.delete(listener);
  }

  observeContextVersion(value) {
    const version = Number(value || 0);
    if (!Number.isSafeInteger(version) || version <= this.contextVersion) return;
    this.contextVersion = version;
    for (const listener of this.contextVersionListeners) listener(version);
  }

  async session(signal) {
    const response = await fetch(`${this.baseURL}/api/session`, { credentials: 'same-origin', cache: 'no-store', signal });
    if (!response.ok) return { authenticated: false };
    const payload = await response.json();
    this.csrfToken = payload.csrf_token || '';
    return payload;
  }

  async request(area, resource, options = {}) {
    const method = options.method || 'GET';
    const headers = { Accept: 'application/json', ...(options.headers || {}) };
    if (method !== 'GET' && method !== 'HEAD') headers['X-CSRF-Token'] = this.csrfToken;
    if (options.body !== undefined) headers['Content-Type'] = 'application/json';
    const response = await fetch(`${this.baseURL}/api/${area}${resource}`, {
      method,
      headers,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      credentials: 'same-origin',
      cache: 'no-store',
      signal: options.signal,
    });
    this.observeContextVersion(response.headers.get('X-Context-Version'));
    if (response.status === 204) return null;
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw errorFromResponse(response, payload);
    return payload;
  }

  bootstrap(signal) { return this.request('account', '/v1/account/bootstrap', { method: 'POST', body: {}, signal }); }
  accountClosure(signal) { return this.request('account', '/v1/me/account-closure-request', { signal }); }
  createAccountClosure(confirmation, key) { return this.request('account', '/v1/me/account-closure-requests', { method: 'POST', headers: { 'Idempotency-Key': key }, body: { confirmation } }); }
  organizations(signal) { return this.request('account', '/v1/me/organizations', { signal }); }
  switchContext(contextType, organizationId, expectedContextVersion, signal) {
    return this.request('account', '/v1/sessions/current/active-context', { method: 'POST', body: { context_type: contextType, organization_id: organizationId || null, expected_context_version: expectedContextVersion }, signal });
  }
  units(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/units`, { signal }); }
  members(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/members`, { signal }); }
  quotaMembers(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/quota-members`, { signal }); }
  createUnit(organizationId, name, parentId = null) { return this.request('account', `/v1/organizations/${organizationId}/units`, { method: 'POST', body: { name, parent_id: parentId } }); }
  invite(organizationId, email, roles) { return this.request('account', `/v1/organizations/${organizationId}/invitations`, { method: 'POST', body: { email, roles, expires_in_hours: 72 } }); }
  products(signal) { return this.request('token', '/v1/recharge/products', { signal }); }
  models(signal) { return this.request('token', '/v1/models', { signal }); }
  organizationModelPolicy(organizationId, signal) { return this.request('token', `/v1/organizations/${organizationId}/model-policy`, { signal }); }
  updateOrganizationModelPolicy(organizationId, policy, key) { return this.request('token', `/v1/organizations/${organizationId}/model-policy`, { method: 'PUT', headers: { 'Idempotency-Key': key }, body: policy }); }
  balance(signal) { return this.request('token', '/v1/me/balance?consistency=strong', { signal }); }
  createRecharge(productId, channel, key) { return this.request('token', '/v1/recharge/orders', { method: 'POST', headers: { 'Idempotency-Key': key }, body: { product_id: productId, channel } }); }
  purchaseOrganizationEntitlements(organizationId, productId, channel, key) { return this.request('token', `/v1/organizations/${organizationId}/recharge/orders`, { method: 'POST', headers: { 'Idempotency-Key': key }, body: { product_id: productId, channel } }); }
  organizationQuotaGrants(organizationId, signal) { return this.request('token', `/v1/organizations/${organizationId}/quota-grants`, { signal }); }
  allocateOrganizationQuota(organizationId, items, key) { return this.request('token', `/v1/organizations/${organizationId}/quota-grants:batch`, { method: 'POST', headers: { 'Idempotency-Key': key }, body: { items } }); }
  revokeOrganizationQuota(organizationId, grantId, reason, key) { return this.request('token', `/v1/organizations/${organizationId}/quota-grants/${grantId}/revoke`, { method: 'POST', headers: { 'Idempotency-Key': key }, body: { reason } }); }
  reclaimAccountClosedGrant(organizationId, grantId, reason, key) { return this.request('token', `/v1/organizations/${organizationId}/account-closed-grants/${grantId}/reclaim`, { method: 'POST', headers: { 'Idempotency-Key': key }, body: { reason } }); }
  recentRechargeOrder(signal) { return this.request('token', '/v1/recharge/orders/recent', { signal }); }
  rechargeOrders(signal) { return this.request('token', '/v1/recharge/orders', { signal }); }
  rechargeOrder(orderId, signal) { return this.request('token', `/v1/recharge/orders/${orderId}`, { signal }); }
  entitlementPackages(signal) { return this.request('token', '/v1/me/entitlement-packages', { signal }); }
  usageHistory(cursor = '', signal) {
    const next = cursor ? `&cursor=${encodeURIComponent(cursor)}` : '';
    return this.request('token', `/v1/me/usage-requests?limit=20${next}`, { signal });
  }
  cancelUsage(usageRequestId, key) { return this.request('token', `/v1/usage-requests/${encodeURIComponent(usageRequestId)}/cancel`, { method: 'POST', headers: { 'Idempotency-Key': key }, body: {} }); }
  logout() { return fetch(`${this.baseURL}/api/logout`, { method: 'POST', headers: { 'X-CSRF-Token': this.csrfToken }, credentials: 'same-origin' }); }
}
