function abortError() {
  const error = new Error('The operation was aborted');
  error.name = 'AbortError';
  return error;
}

function responseError(response) {
  const payload = response?.body || {};
  const error = new Error(payload.message || `HTTP ${response?.status || 500}`);
  error.code = payload.code || 'REQUEST_FAILED';
  error.status = response?.status || 500;
  return error;
}

/**
 * Same business contract as AccountPlatformBffClient, transported through the
 * Tauri account-platform domain. OAuth tokens are intentionally absent here.
 */
export class AccountPlatformDesktopClient {
  constructor(transport = globalThis.TauriBridge?.accountPlatform) {
    if (!transport) throw new Error('Account Platform desktop bridge is unavailable');
    this.transport = transport;
  }

  session(signal) { return this.call(() => this.transport.session(), signal); }
  login(account, password, signal) {
    return this.call(() => this.transport.login({ account, password }), signal);
  }
  logout(signal) { return this.call(() => this.transport.logout(), signal); }

  async call(action, signal) {
    if (signal?.aborted) throw abortError();
    const value = await action();
    if (signal?.aborted) throw abortError();
    return value;
  }

  async request(area, resource, options = {}) {
    const response = await this.call(() => this.transport.request({
      area,
      resource,
      method: options.method || 'GET',
      body: options.body,
      idempotencyKey: options.idempotencyKey,
    }), options.signal);
    if (response.status < 200 || response.status >= 300) throw responseError(response);
    return response.body;
  }

  bootstrap(signal) { return this.request('account', '/v1/account/bootstrap', { method: 'POST', body: {}, signal }); }
  organizations(signal) { return this.request('account', '/v1/me/organizations', { signal }); }
  switchContext(contextType, organizationId, signal) { return this.request('account', '/v1/sessions/current/active-context', { method: 'POST', body: { context_type: contextType, organization_id: organizationId || null }, signal }); }
  units(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/units`, { signal }); }
  members(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/members`, { signal }); }
  createUnit(organizationId, name, parentId = null) { return this.request('account', `/v1/organizations/${organizationId}/units`, { method: 'POST', body: { name, parent_id: parentId } }); }
  invite(organizationId, email, roles) { return this.request('account', `/v1/organizations/${organizationId}/invitations`, { method: 'POST', body: { email, roles, expires_in_hours: 72 } }); }
  products(signal) { return this.request('token', '/v1/recharge/products', { signal }); }
  balance(signal) { return this.request('token', '/v1/me/balance?consistency=strong', { signal }); }
  createRecharge(productId, channel, key) { return this.request('token', '/v1/recharge/orders', { method: 'POST', idempotencyKey: key, body: { product_id: productId, channel } }); }
  recentRechargeOrder(signal) { return this.request('token', '/v1/recharge/orders/recent', { signal }); }
  rechargeOrder(orderId, signal) { return this.request('token', `/v1/recharge/orders/${orderId}`, { signal }); }
  usageHistory(cursor = '', signal) {
    const next = cursor ? `&cursor=${encodeURIComponent(cursor)}` : '';
    return this.request('token', `/v1/me/usage-requests?limit=20${next}`, { signal });
  }
}
