function abortError() {
  const error = new Error('The operation was aborted');
  error.name = 'AbortError';
  return error;
}

function responseError(response) {
  const code = response?.body?.code;
  const safeCode = typeof code === 'string' && /^[A-Za-z][A-Za-z0-9_]{1,80}$/.test(code) ? code : 'REQUEST_FAILED';
  const error = new Error(safeCode);
  error.code = safeCode;
  error.status = Number.isInteger(response?.status) ? response.status : 500;
  return error;
}

function sessionStatus(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)
    || typeof value.enabled !== 'boolean' || typeof value.configured !== 'boolean'
    || typeof value.authenticated !== 'boolean'
    || (value.authenticated && (!value.enabled || !value.configured))
    || (value.errorCode != null && typeof value.errorCode !== 'string')) {
    throw nativeError('account_platform_request_failed');
  }
  return value;
}

function nativeError(value) {
  // Native errors carry stable codes only; provider bodies and credentials must
  // never become WebView error messages.
  const code = typeof value === 'string' ? value.split(':', 1)[0] : value?.code;
  const safeCode = typeof code === 'string' && /^[a-z][a-z0-9_]{1,80}$/.test(code)
    ? code : 'account_platform_request_failed';
  const error = new Error(safeCode);
  error.code = safeCode;
  if (safeCode === 'authentication_required') error.status = 401;
  return error;
}

/**
 * Same business contract as AccountPlatformBffClient, transported through the
 * Tauri account-platform domain. OAuth tokens are intentionally absent here.
 */
export class AccountPlatformDesktopClient {
  constructor(transport) {
    if (!transport) throw new Error('Account Platform desktop bridge is unavailable');
    this.transport = transport;
  }

  async session(signal) { return sessionStatus(await this.call(() => this.transport.session(), signal)); }
  async login(account, password, signal) {
    const status = sessionStatus(await this.call(() => this.transport.login({ account, password }), signal));
    if (!status.authenticated) throw nativeError(status.errorCode || 'authentication_required');
    return status;
  }
  async logout(signal) {
    const status = sessionStatus(await this.call(() => this.transport.logout(), signal));
    if (status.authenticated) throw nativeError('account_platform_request_failed');
    return status;
  }

  async call(action, signal) {
    if (signal?.aborted) throw abortError();
    let value;
    try {
      value = await action();
    } catch (error) {
      if (signal?.aborted) throw abortError();
      throw nativeError(error);
    }
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
    if (!response || !Number.isInteger(response.status) || response.status < 200 || response.status >= 300) throw responseError(response);
    if (response.status !== 204 && (!response.body || typeof response.body !== 'object' || Array.isArray(response.body))) throw responseError({ status: 500 });
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
  managedTokens(signal) { return this.request('token', '/v1/token-accounts', { signal }); }
  eligibleTokens(model, signal) { return this.request('token', `/v1/token-accounts/eligible?model=${encodeURIComponent(model)}`, { signal }); }
  usageRequest(usageRequestId, signal) { return this.request('token', `/v1/usage-requests/${encodeURIComponent(usageRequestId)}`, { signal }); }
  createTokenAccount(body) { return this.request('token', '/v1/token-accounts', { method: 'POST', body }); }
  credentials(accountId, signal) { return this.request('token', `/v1/token-accounts/${accountId}/credentials`, { signal }); }
  rotateCredential(accountId, body) { return this.request('token', `/v1/token-accounts/${accountId}/credentials`, { method: 'POST', body }); }
  usageHistory(cursor = '', signal) {
    const next = cursor ? `&cursor=${encodeURIComponent(cursor)}` : '';
    return this.request('token', `/v1/me/usage-requests?limit=20${next}`, { signal });
  }
}
