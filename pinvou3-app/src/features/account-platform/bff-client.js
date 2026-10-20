import { parseFinanceJSON, requirePaymentOrder, encodeFinanceJSON } from './finance-contract.js';
import { parseClosedGrantsJSON } from './account-closed-grants-contract.js';
import { requireSession, requireProfileChange } from './personal-contract.js';
import { mergeObservedVersions } from './authorization-ui.js';
import { secureRequestID } from './request-id.js';
import { parseBalanceJSON } from './balance-view.js';
import { responseRetryAfter } from './auth-error.js';
function errorFromResponse(response, payload) {
  const error = new Error(payload && payload.message ? payload.message : `HTTP ${response.status}`);
  error.code = payload && payload.code ? payload.code : 'REQUEST_FAILED';
  error.status = response.status;
  error.retryAfterSeconds = responseRetryAfter(payload, response.headers.get('Retry-After'));
  return error;
}

export class AccountPlatformBffClient {
  constructor(baseURL = '') {
    this.baseURL = String(baseURL || '').replace(/\/$/, '');
    this.csrfToken = '';
    this.financeSessionEpoch = 0;
    this.deviceID = secureRequestID();
    this.contextVersion = 0;
    this.contextVersionListeners = new Set();
    this.authorizationVersions = {};
    this.authorizationVersionListeners = new Set();
  }

  onContextVersion(listener) {
    this.contextVersionListeners.add(listener);
    return () => this.contextVersionListeners.delete(listener);
  }

  onAuthorizationVersion(listener) {
    this.authorizationVersionListeners.add(listener);
    return () => this.authorizationVersionListeners.delete(listener);
  }

  observeContextVersion(value) {
    const version = Number(value || 0);
    if (!Number.isSafeInteger(version) || version <= this.contextVersion) return;
    this.contextVersion = version;
    for (const listener of this.contextVersionListeners) listener(version);
  }

  observeAuthorizationVersions(headers) {
    const observed = {
      context_version: headers.get('X-Context-Version'),
      personal_authorization_version: headers.get('X-Personal-Authorization-Version'),
      platform_authorization_version: headers.get('X-Platform-Authorization-Version'),
      organization_authorization_version: headers.get('X-Organization-Authorization-Version'),
    };
    const next = mergeObservedVersions(this.authorizationVersions, observed);
    if (JSON.stringify(next) === JSON.stringify(this.authorizationVersions)) return;
    this.authorizationVersions = next;
    for (const listener of this.authorizationVersionListeners) listener({ ...next });
  }

  async session(signal) {
    const response = await fetch(`${this.baseURL}/api/session`, {
      credentials: 'same-origin',
      cache: 'no-store',
      signal,
    });
    const payload = await response.json().catch(() => ({}));
    if (response.status === 401) { if (this.csrfToken) this.financeSessionEpoch++; this.csrfToken = ''; return { authenticated: false }; }
    if (!response.ok) throw errorFromResponse(response, payload);
    requireSession(payload);
    if (this.csrfToken !== payload.csrf_token) this.financeSessionEpoch++;
    this.csrfToken = payload.csrf_token || '';
    return requireSession(payload);
  }

  async request(area, resource, options = {}) {
    const method = options.method || 'GET';
    const mutation = method !== 'GET' && method !== 'HEAD';
    // The BFF preserves CSRF across ordinary refreshes. A different token is a
    // different browser login; an old operation must not follow that login.
    const binding = this.csrfToken;
    const send = async (csrfToken) => {
      const headers = { Accept: 'application/json', ...(options.headers || {}) };
      if (mutation) headers['X-CSRF-Token'] = csrfToken;
      if (options.body !== undefined) headers['Content-Type'] = 'application/json';
      const response = await fetch(`${this.baseURL}/api/${area}${resource}`, {
        method,
        headers,
        body: options.body === undefined ? undefined : area === 'token' && resource.endsWith('/quota-grants:batch') ? encodeFinanceJSON(options.body) : JSON.stringify(options.body),
        credentials: 'same-origin',
        cache: 'no-store',
        signal: options.signal,
      });
      this.observeContextVersion(response.headers.get('X-Context-Version'));
      this.observeAuthorizationVersions(response.headers);
      if (response.status === 204) return { response, payload: null };
      return { response, payload: area === 'token' && resource.startsWith('/v1/me/balance') && response.ok ? parseBalanceJSON(await response.text()) : area === 'token' && resource.includes('/account-closed-grants') && response.ok ? parseClosedGrantsJSON(await response.text()) : area === 'token' && response.ok ? parseFinanceJSON(await response.text()) : await response.json().catch(() => ({})) };
    };

    let result = await send(this.csrfToken);
    if (mutation && result.response.status === 403 && result.payload?.code === 'CSRF_REJECTED') {
      const session = await this.session(options.signal);
      if (session.authenticated && binding && session.csrf_token === binding) result = await send(binding);
    }
    if (!result.response.ok) throw errorFromResponse(result.response, result.payload);
    if (options.expectedStatus && !options.expectedStatus.includes(result.response.status)) { const error = new Error('UNKNOWN_ORGANIZATION_ACK'); error.code = 'COMMAND_RESULT_UNKNOWN'; throw error; }
    return result.payload;
  }

  bootstrap(signal) {
    return this.request('account', '/v1/account/bootstrap', {
      method: 'POST',
      body: {},
      signal,
    });
  }

  reauthenticate(returnTo = '/?section=accountSettings') {
    globalThis.location.assign(`${this.baseURL}/auth/login?reauthenticate=1&return_to=${encodeURIComponent(returnTo)}`);
  }

  async createProfileChange(field, value, signal) {
    const result = await this.request('account', '/v1/me/profile-change-requests', {
      method: 'POST',
      body: { field, value },
      signal,
    });
    return requireProfileChange(result, '', field);
  }

  createProfileSliderCaptcha(mobile, signal) {
    return this.request('account', '/v1/auth/captcha/challenges', {
      method: 'POST',
      body: { mobile, purpose: 'PROFILE_MOBILE' },
      signal,
    });
  }

  sendProfileMobileChallenge(requestId, captchaId, captchaOffset, signal) {
    return this.request('account', `/v1/me/profile-change-requests/${encodeURIComponent(requestId)}/mobile-challenges`, {
      method: 'POST',
      body: { captcha_id: captchaId, captcha_offset: captchaOffset },
      headers: { 'X-Device-ID': this.deviceID },
      signal,
    });
  }

  async completeProfileChange(requestId, code = '', signal) {
    const result = await this.request('account', `/v1/me/profile-change-requests/${encodeURIComponent(requestId)}/actions/complete`, { method: 'POST', body: { code }, headers: { 'X-Device-ID': this.deviceID }, signal });
    return requireProfileChange(result, requestId);
  }

  products(signal) { return this.request('token', '/v1/recharge/products', { signal }); }
  accountClosure(signal) { return this.request('account', '/v1/me/account-closure-request', { signal }); }
  createAccountClosure(confirmation, key, signal) {
    return this.request('account', '/v1/me/account-closure-requests', { method: 'POST', body: { confirmation }, headers: { 'Idempotency-Key': key }, signal });
  }
  paymentCapabilities(signal) { return this.request('token', '/v1/payment-capabilities', { signal }); }
  rechargeOrders(signal) { return this.request('token', '/v1/recharge/orders', { signal }); }
  recentRechargeOrder(signal) { return this.request('token', '/v1/recharge/orders/recent', { signal }); }
  async rechargeOrder(orderID, signal) {
    return requirePaymentOrder(await this.request('token', `/v1/recharge/orders/${encodeURIComponent(orderID)}`, { signal }), orderID);
  }
  async createRecharge(productID, channel, key, signal) {
    return requirePaymentOrder(await this.request('token', '/v1/recharge/orders', {
      method: 'POST', headers: { 'Idempotency-Key': key },
      body: { product_id: productID, channel }, signal,
    }));
  }
  async closeRechargeOrder(orderID, signal) {
    return requirePaymentOrder(await this.request('token', `/v1/recharge/orders/${encodeURIComponent(orderID)}/close`, { method: 'POST', body: {}, signal }), orderID);
  }
  addPaymentClaim(orderID, reason, signal) {
    return this.request('token', `/v1/recharge/orders/${encodeURIComponent(orderID)}/payment-claims`, { method: 'POST', body: { statement: reason }, signal });
  }
  entitlementPackages(signal) { return this.request('token', '/v1/me/entitlement-packages', { signal }); }

  balance(signal) {
    return this.request('token', '/v1/me/balance?consistency=strong', {
      signal,
    });
  }

  async logout() {
    const binding = this.csrfToken;
    const send = async (csrfToken) => {
      const response = await fetch(`${this.baseURL}/api/logout`, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrfToken },
        credentials: 'same-origin',
      });
      return { response, payload: response.status === 204 ? null : await response.json().catch(() => ({})) };
    };
    let { response, payload } = await send(this.csrfToken);
    if (response.status === 403 && payload?.code === 'CSRF_REJECTED') {
      const session = await this.session();
      if (!session.authenticated) {
        this.csrfToken = '';
        return null;
      }
      if (session.authenticated && binding && session.csrf_token === binding) ({ response, payload } = await send(binding));
    }
    if (!response.ok) throw errorFromResponse(response, payload);
    this.financeSessionEpoch++;
    this.csrfToken = '';
    return payload;
  }

  models(signal) {
    return this.request('token', '/v1/models', { signal });
  }
  async usageHistory(cursor = '', signal) {
    const next = cursor ? `&cursor=${encodeURIComponent(cursor)}` : '';
    if (cursor && !cursor.startsWith('turn-v1.')) {
      return this.request('token', `/v1/me/usage-requests?limit=20${next}`, { signal });
    }
    try {
      return await this.request('token', `/v1/me/usage-turns?limit=20${next}`, { signal });
    } catch (error) {
      const fallback = !cursor && (error?.status === 404 || ['USAGE_HISTORY_PROJECTION_BUILDING', 'USAGE_HISTORY_PROJECTION_UNAVAILABLE'].includes(error?.code));
      if (!fallback) throw error;
      return this.request('token', '/v1/me/usage-requests?limit=20', { signal });
    }
  }
  cancelUsage(usageRequestId, key) {
    return this.request('token', `/v1/usage-requests/${encodeURIComponent(usageRequestId)}/cancel`, { method: 'POST', headers: { 'Idempotency-Key': key }, body: {} });
  }
}
