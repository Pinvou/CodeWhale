const isObject = value => value !== null && typeof value === 'object' && !Array.isArray(value);

function requestFailed() {
  const error = new Error('REQUEST_FAILED');
  error.code = 'REQUEST_FAILED';
  return error;
}

async function readPayload(response, required = false) {
  let payload;
  try { payload = await response.json(); }
  catch (reason) {
    if (reason?.name === 'AbortError') throw reason;
    if (required) throw requestFailed();
    return null;
  }
  if (!isObject(payload)) {
    if (required) throw requestFailed();
    return null;
  }
  return payload;
}

function errorFromResponse(response, payload) {
  const code = typeof payload?.code === 'string' && /^[A-Za-z][A-Za-z0-9_]{0,127}$/u.test(payload.code)
    ? payload.code : 'REQUEST_FAILED';
  const error = new Error(code);
  error.code = code;
  error.status = response.status;
  return error;
}

export class AccountPlatformBffClient {
  constructor(baseURL = '') {
    this.baseURL = String(baseURL || '').replace(/\/$/, '');
    this.csrfToken = '';
  }

  async session(signal) {
    this.csrfToken = '';
    const response = await fetch(`${this.baseURL}/api/session`, { credentials: 'same-origin', cache: 'no-store', signal });
    if (response.status === 401) { this.csrfToken = ''; return { authenticated: false }; }
    if (!response.ok) throw errorFromResponse(response, await readPayload(response));
    const payload = await readPayload(response, true);
    if (typeof payload.authenticated !== 'boolean' ||
      (payload.authenticated && (typeof payload.csrf_token !== 'string' || !payload.csrf_token))) throw requestFailed();
    this.csrfToken = payload.authenticated ? payload.csrf_token : '';
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
    const payload = await readPayload(response, response.ok);
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
  units(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/units`, { signal }); }
  members(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/members`, { signal }); }
  createUnit(organizationId, name, parentId = null) { return this.request('account', `/v1/organizations/${organizationId}/units`, { method: 'POST', body: { name, parent_id: parentId } }); }
  invite(organizationId, email, roles) { return this.request('account', `/v1/organizations/${organizationId}/invitations`, { method: 'POST', body: { email, roles, expires_in_hours: 72 } }); }
  products(signal) { return this.request('token', '/v1/recharge/products', { signal }); }
  balance(signal) { return this.request('token', '/v1/me/balance?consistency=strong', { signal }); }
  createRecharge(productId, channel, key) { return this.request('token', '/v1/recharge/orders', { method: 'POST', headers: { 'Idempotency-Key': key }, body: { product_id: productId, channel } }); }
  recentRechargeOrder(signal) { return this.request('token', '/v1/recharge/orders/recent', { signal }); }
  rechargeOrders(signal) { return this.request('token', '/v1/recharge/orders', { signal }); }
  rechargeOrder(orderId, signal) { return this.request('token', `/v1/recharge/orders/${orderId}`, { signal }); }
  entitlementPackages(signal) { return this.request('token', '/v1/me/entitlement-packages', { signal }); }
  usageHistory(cursor = '', signal) {
    const next = cursor ? `&cursor=${encodeURIComponent(cursor)}` : '';
    return this.request('token', `/v1/me/usage-requests?limit=20${next}`, { signal });
  }
  managedTokens(signal) { return this.request('token', '/v1/token-accounts', { signal }); }
  eligibleTokens(model, signal) { return this.request('token', `/v1/token-accounts/eligible?model=${encodeURIComponent(model)}`, { signal }); }
  usageRequest(usageRequestId, signal) { return this.request('token', `/v1/usage-requests/${encodeURIComponent(usageRequestId)}`, { signal }); }
  createTokenAccount(body) { return this.request('token', '/v1/token-accounts', { method: 'POST', body }); }
  credentials(accountId, signal) { return this.request('token', `/v1/token-accounts/${accountId}/credentials`, { signal }); }
  rotateCredential(accountId, body) { return this.request('token', `/v1/token-accounts/${accountId}/credentials`, { method: 'POST', body }); }
  myRoleBindings(signal) { return this.request('account', '/v1/me/role-bindings', { signal }); }
  createSliderCaptcha(mobile, purpose, signal) { return this.request('account', '/v1/auth/captcha/challenges', { method:'POST', body:{ mobile, purpose }, signal }); }
  createProfileChange(field, value) { return this.request('account', '/v1/me/profile-change-requests', { method:'POST', body:{ field,value } }); }
  sendProfileMobileChallenge(requestId, captchaId, captchaOffset) { return this.request('account', `/v1/me/profile-change-requests/${requestId}/mobile-challenges`, { method:'POST', body:{ captcha_id:captchaId,captcha_offset:captchaOffset } }); }
  completeProfileChange(requestId, code = '') { return this.request('account', `/v1/me/profile-change-requests/${requestId}/actions/complete`, { method:'POST', body:{ code } }); }
  profileChangeStatus(requestId, signal) { return this.request('account', `/v1/me/profile-change-requests/${requestId}`, { signal }); }
  async logout() {
    const response = await fetch(`${this.baseURL}/api/logout`, { method: 'POST', headers: { 'X-CSRF-Token': this.csrfToken }, credentials: 'same-origin' });
    if (!response.ok) throw errorFromResponse(response, await readPayload(response));
    this.csrfToken = '';
  }
}

// Commerce and slider QR endpoints return bounded PNG data, never remote images.
export function safePNGImage(value) {
  if (typeof value !== 'string' || value.length > 524288 || !/^data:image\/png;base64,[A-Za-z0-9+/]+={0,2}$/u.test(value)) return '';
  try {
    const bytes = globalThis.atob(value.slice('data:image/png;base64,'.length));
    if (bytes.length < 33 || bytes.slice(0, 8) !== '\x89PNG\r\n\x1a\n' || bytes.slice(12, 16) !== 'IHDR') return '';
    const read32 = offset => bytes.charCodeAt(offset) * 16777216 + bytes.charCodeAt(offset + 1) * 65536 + bytes.charCodeAt(offset + 2) * 256 + bytes.charCodeAt(offset + 3);
    const width = read32(16); const height = read32(20);
    return width > 0 && height > 0 && width <= 4096 && height <= 4096 ? value : '';
  } catch { return ''; }
}
