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
  }

  async session(signal) {
    const response = await fetch(`${this.baseURL}/api/session`, { credentials: 'same-origin', cache: 'no-store', signal });
    if (response.status === 401) { this.csrfToken = ''; return { authenticated: false }; }
    if (!response.ok) throw errorFromResponse(response, await response.json().catch(() => ({})));
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
    if (response.status === 204) return null;
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) {
      if (response.status === 401) this.csrfToken = '';
      throw errorFromResponse(response, payload);
    }
    return payload;
  }

  bootstrap(signal) { return this.request('account', '/v1/account/bootstrap', { method: 'POST', body: {}, signal }); }
  organizations(signal) { return this.request('account', '/v1/me/organizations', { signal }); }
  switchContext(contextType, organizationId, signal) {
    return this.request('account', '/v1/sessions/current/active-context', { method: 'POST', body: { context_type: contextType, organization_id: organizationId || null }, signal });
  }
  createOrganization(name) { return this.request('account', '/v1/organizations', { method: 'POST', body: { name } }); }
  units(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/units`, { signal }); }
  members(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/members`, { signal }); }
  createUnit(organizationId, name, parentId = null) { return this.request('account', `/v1/organizations/${organizationId}/units`, { method: 'POST', body: { name, parent_id: parentId } }); }
  invite(organizationId, email, roles) { return this.request('account', `/v1/organizations/${organizationId}/invitations`, { method: 'POST', body: { email, roles, expires_in_hours: 72 } }); }
  products(signal) { return this.request('token', '/v1/recharge/products', { signal }); }
  balance(signal) { return this.request('token', '/v1/me/balance?consistency=strong', { signal }); }
  createRecharge(productId, channel, key) { return this.request('token', '/v1/recharge/orders', { method: 'POST', headers: { 'Idempotency-Key': key }, body: { product_id: productId, channel } }); }
  recentRechargeOrder(signal) { return this.request('token', '/v1/recharge/orders/recent', { signal }); }
  rechargeOrder(orderId, signal) { return this.request('token', `/v1/recharge/orders/${orderId}`, { signal }); }
  managedTokens(signal) { return this.request('token', '/v1/token-accounts', { signal }); }
  eligibleTokens(model, signal) { return this.request('token', `/v1/token-accounts/eligible?model=${encodeURIComponent(model)}`, { signal }); }
  usageRequest(usageRequestId, signal) { return this.request('token', `/v1/usage-requests/${encodeURIComponent(usageRequestId)}`, { signal }); }
  createTokenAccount(body) { return this.request('token', '/v1/token-accounts', { method: 'POST', body }); }
  credentials(accountId, signal) { return this.request('token', `/v1/token-accounts/${accountId}/credentials`, { signal }); }
  rotateCredential(accountId, body) { return this.request('token', `/v1/token-accounts/${accountId}/credentials`, { method: 'POST', body }); }
  async logout() {
    const response = await fetch(`${this.baseURL}/api/logout`, { method: 'POST', headers: { 'X-CSRF-Token': this.csrfToken }, credentials: 'same-origin' });
    if (!response.ok) throw errorFromResponse(response, await response.json().catch(() => ({})));
    this.csrfToken = '';
  }
}
