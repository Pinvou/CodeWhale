import { parseFinanceJSON, requirePaymentOrder } from './finance-contract.js';
import { requireSession, requireProfileChange } from './personal-contract.js';
import { parseBalanceJSON } from './balance-view.js';
import { mergeObservedVersions } from './authorization-ui.js';
import { LANG_TO_TAG } from '../../shared/i18n.js';
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
  constructor(transport) {
    if (!transport) throw new Error('Account Platform desktop bridge is unavailable');
    this.transport = transport;
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

  observeAuthorizationVersions(response) {
    const next = mergeObservedVersions(this.authorizationVersions, {
      context_version: response.contextVersion,
      personal_authorization_version: response.personalAuthorizationVersion,
      platform_authorization_version: response.platformAuthorizationVersion,
      organization_authorization_version: response.organizationAuthorizationVersion,
    });
    if (JSON.stringify(next) === JSON.stringify(this.authorizationVersions)) return;
    this.authorizationVersions = next;
    for (const listener of this.authorizationVersionListeners) listener({ ...next });
  }

  async session(signal) {
    return requireSession(await this.call(() => this.transport.session(), signal));
  }

  openPortal(language, signal) {
    return this.call(() => this.transport.openPortal({ language: LANG_TO_TAG[language] || 'zh-Hans' }), signal);
  }

  login(account, password, signal) {
    return this.call(() => this.transport.login({ account, password }), signal);
  }

  createSliderCaptcha(mobile, purpose, signal) {
    return this.call(() => this.transport.createSliderCaptcha({ mobile, purpose }), signal);
  }

  precheckMobileRegistration(username, mobile, password, signal) {
    return this.call(
      () =>
        this.transport.precheckMobileRegistration({
          username,
          mobile,
          password,
        }),
      signal,
    );
  }

  sendSms(username, mobile, passwordReference, purpose, captchaId, captchaOffset, signal) {
    return this.call(
      () =>
        this.transport.sendSms({
          username,
          mobile,
          password_handle: passwordReference?.password_handle || '',
          password_policy_version: passwordReference?.password_policy_version || '',
          purpose,
          captcha_id: captchaId,
          captcha_offset: captchaOffset,
        }),
      signal,
    );
  }

  registerMobile(username, mobile, challengeId, code, passwordReference, recovery, signal) {
    return this.call(
      () =>
        this.transport.registerMobile({
          username,
          mobile,
          challenge_id: challengeId,
          code,
          password_handle: passwordReference?.password_handle || '',
          password_policy_version: passwordReference?.password_policy_version || '',
          registration_operation_id: recovery?.registration_operation_id || '',
          registration_query_token: recovery?.registration_query_token || '',
        }),
      signal,
    );
  }

  registrationStatus(signal) {
    return this.call(() => this.transport.registrationStatus(), signal);
  }

  loginSms(mobile, challengeId, code, signal) {
    return this.call(() => this.transport.loginSms({ mobile, challenge_id: challengeId, code }), signal);
  }

  resetPassword(mobile, challengeId, code, password, signal) {
    return this.call(
      () =>
        this.transport.resetPassword({
          mobile,
          challenge_id: challengeId,
          code,
          password,
        }),
      signal,
    );
  }

  logout(signal) {
    return this.call(() => this.transport.logout(), signal);
  }

  async call(action, signal) {
    if (signal?.aborted) throw abortError();
    const value = await action();
    if (signal?.aborted) throw abortError();
    return value;
  }

  async request(area, resource, options = {}) {
    const response = await this.call(
      () =>
        this.transport.request({
          area,
          resource,
          method: options.method || 'GET',
          body: options.body,
          idempotencyKey: options.idempotencyKey,
        }),
      options.signal,
    );
    this.observeContextVersion(response.contextVersion);
    this.observeAuthorizationVersions(response);
    if (response.status < 200 || response.status >= 300) throw responseError(response);
    if (area === 'token' && resource.startsWith('/v1/me/balance')) {
      if (typeof response.bodyText === 'string') return parseBalanceJSON(response.bodyText);
      for (const field of ['available_microcredits', 'reserved_microcredits']) {
        if (!Number.isSafeInteger(response.body?.[field])) throw new Error('INVALID_BALANCE');
      }
      return parseBalanceJSON(JSON.stringify(response.body));
    }
    if (area === 'token' && typeof response.bodyText === 'string') return parseFinanceJSON(response.bodyText);
    return response.body;
  }

  bootstrap(signal) {
    return this.request('account', '/v1/account/bootstrap', {
      method: 'POST',
      body: {},
      signal,
    });
  }

  products(signal) { return this.request('token', '/v1/recharge/products', { signal }); }
  paymentCapabilities(signal) { return this.request('token', '/v1/payment-capabilities', { signal }); }
  rechargeOrders(signal) { return this.request('token', '/v1/recharge/orders', { signal }); }
  recentRechargeOrder(signal) { return this.request('token', '/v1/recharge/orders/recent', { signal }); }
  async rechargeOrder(orderID, signal) {
    return requirePaymentOrder(await this.request('token', `/v1/recharge/orders/${encodeURIComponent(orderID)}`, { signal }), orderID);
  }
  async createRecharge(productID, channel, key, signal) {
    return requirePaymentOrder(await this.request('token', '/v1/recharge/orders', {
      method: 'POST', idempotencyKey: key,
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
      signal,
    });
  }

  async completeProfileChange(requestId, code = '', signal) {
    const result = await this.request('account', `/v1/me/profile-change-requests/${encodeURIComponent(requestId)}/actions/complete`, { method: 'POST', body: { code }, signal });
    return requireProfileChange(result, requestId);
  }
}
