import { mergeObservedVersions } from './authorization-ui.js';
import { invitationCreateBody } from './invitation-client.js';

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
    if (response.status === 401) return { authenticated: false };
    if (!response.ok) throw errorFromResponse(response, payload);
    this.csrfToken = payload.csrf_token || '';
    return payload;
  }

  async request(area, resource, options = {}) {
    const method = options.method || 'GET';
    const mutation = method !== 'GET' && method !== 'HEAD';
    const send = async () => {
      const headers = { Accept: 'application/json', ...(options.headers || {}) };
      if (mutation) headers['X-CSRF-Token'] = this.csrfToken;
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
      this.observeAuthorizationVersions(response.headers);
      if (response.status === 204) return { response, payload: null };
      return { response, payload: await response.json().catch(() => ({})) };
    };

    let result = await send();
    if (mutation && result.response.status === 403 && result.payload?.code === 'CSRF_REJECTED') {
      const session = await this.session(options.signal);
      if (session.authenticated && this.csrfToken) result = await send();
    }
    if (!result.response.ok) throw errorFromResponse(result.response, result.payload);
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
  createProfileChange(field, value, signal) {
    return this.request('account', '/v1/me/profile-change-requests', {
      method: 'POST',
      body: { field, value },
      signal,
    });
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
      signal,
    });
  }
  completeProfileChange(requestId, code = '', signal) {
    return this.request('account', `/v1/me/profile-change-requests/${encodeURIComponent(requestId)}/actions/complete`, { method: 'POST', body: { code }, signal });
  }
  accountClosure(signal) {
    return this.request('account', '/v1/me/account-closure-request', {
      signal,
    });
  }
  createAccountClosure(confirmation, key) {
    return this.request('account', '/v1/me/account-closure-requests', {
      method: 'POST',
      headers: { 'Idempotency-Key': key },
      body: { confirmation },
    });
  }
  organizations(signal) {
    return this.request('account', '/v1/me/organizations', { signal });
  }
  switchContext(contextType, organizationId, expectedContextVersion, signal) {
    return this.request('account', '/v1/sessions/current/active-context', {
      method: 'POST',
      body: {
        context_type: contextType,
        organization_id: organizationId || null,
        expected_context_version: expectedContextVersion,
      },
      signal,
    });
  }
  units(organizationId, signal) {
    return this.request('account', `/v1/organizations/${organizationId}/units`, { signal });
  }
  members(organizationId, signal) {
    return this.request('account', `/v1/organizations/${organizationId}/members`, { signal });
  }
  quotaMembers(organizationId, signal) {
    return this.request('account', `/v1/organizations/${organizationId}/quota-members`, { signal });
  }
  createUnit(organizationId, name, parentId = null) {
    return this.request('account', `/v1/organizations/${organizationId}/units`, { method: 'POST', body: { name, parent_id: parentId } });
  }
  invite(organizationId, target, roles, key = '') {
    return this.request('account', `/v1/organizations/${organizationId}/invitations`, {
      method: 'POST',
      headers: key ? { 'Idempotency-Key': key } : undefined,
      body: invitationCreateBody(target, roles),
    });
  }
  myInvitations(signal) {
    return this.request('account', '/v1/me/invitations', { signal });
  }
  acceptInvitation(invitationId, signal) {
    return this.request('account', '/v1/invitations/accept', { method: 'POST', body: { invitation_id: invitationId }, signal });
  }
  products(signal) {
    return this.request('token', '/v1/recharge/products', { signal });
  }
  models(signal) {
    return this.request('token', '/v1/models', { signal });
  }
  organizationModelPolicy(organizationId, signal) {
    return this.request('token', `/v1/organizations/${organizationId}/model-policy`, { signal });
  }
  updateOrganizationModelPolicy(organizationId, policy, key) {
    return this.request('token', `/v1/organizations/${organizationId}/model-policy`, { method: 'PUT', headers: { 'Idempotency-Key': key }, body: policy });
  }
  balance(signal) {
    return this.request('token', '/v1/me/balance?consistency=strong', {
      signal,
    });
  }
  organizationBalance(organizationId, signal) {
    return this.request('token', `/v1/organizations/${organizationId}/balance`, { signal });
  }
  createRecharge(productId, channel, key) {
    return this.request('token', '/v1/recharge/orders', {
      method: 'POST',
      headers: { 'Idempotency-Key': key },
      body: { product_id: productId, channel },
    });
  }
  purchaseOrganizationEntitlements(organizationId, productId, channel, key) {
    return this.request('token', `/v1/organizations/${organizationId}/recharge/orders`, {
      method: 'POST',
      headers: { 'Idempotency-Key': key },
      body: { product_id: productId, channel },
    });
  }
  organizationQuotaGrants(organizationId, signal) {
    return this.request('token', `/v1/organizations/${organizationId}/quota-grants`, { signal });
  }
  allocateOrganizationQuota(organizationId, items, key) {
    return this.request('token', `/v1/organizations/${organizationId}/quota-grants:batch`, { method: 'POST', headers: { 'Idempotency-Key': key }, body: { items } });
  }
  revokeOrganizationQuota(organizationId, grantId, reason, key) {
    return this.request('token', `/v1/organizations/${organizationId}/quota-grants/${grantId}/revoke`, { method: 'POST', headers: { 'Idempotency-Key': key }, body: { reason } });
  }
  reclaimAccountClosedGrant(organizationId, grantId, reason, key) {
    return this.request('token', `/v1/organizations/${organizationId}/account-closed-grants/${grantId}/reclaim`, { method: 'POST', headers: { 'Idempotency-Key': key }, body: { reason } });
  }
  recentRechargeOrder(signal) {
    return this.request('token', '/v1/recharge/orders/recent', { signal });
  }
  rechargeOrders(signal) {
    return this.request('token', '/v1/recharge/orders', { signal });
  }
  rechargeOrder(orderId, signal) {
    return this.request('token', `/v1/recharge/orders/${orderId}`, { signal });
  }
  organizationRechargeOrder(organizationId, orderId, signal) {
    return this.request('token', `/v1/organizations/${organizationId}/recharge/orders/${orderId}`, { signal });
  }
  entitlementPackages(signal) {
    return this.request('token', '/v1/me/entitlement-packages', { signal });
  }
  organizationEntitlementPackages(organizationId, signal) {
    return this.request('token', `/v1/organizations/${organizationId}/entitlement-packages`, { signal });
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
  async logout() {
    const send = async () => {
      const response = await fetch(`${this.baseURL}/api/logout`, {
        method: 'POST',
        headers: { 'X-CSRF-Token': this.csrfToken },
        credentials: 'same-origin',
      });
      return { response, payload: response.status === 204 ? null : await response.json().catch(() => ({})) };
    };
    let { response, payload } = await send();
    if (response.status === 403 && payload?.code === 'CSRF_REJECTED') {
      const session = await this.session();
      if (!session.authenticated) {
        this.csrfToken = '';
        return null;
      }
      if (session.authenticated && this.csrfToken) ({ response, payload } = await send());
    }
    if (!response.ok) throw errorFromResponse(response, payload);
    this.csrfToken = '';
    return payload;
  }
}
