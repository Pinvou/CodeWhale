import { safePNGImage } from './bff-client.js';
import { builtinModelCatalog } from '../../shared/model-options.js';

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

const mobileErrorCodes = new Set([
  'USERNAME_INVALID', 'USERNAME_ALREADY_REGISTERED', 'PASSWORD_INVALID',
  'MOBILE_INVALID', 'MOBILE_ALREADY_REGISTERED', 'CAPTCHA_INVALID',
  'SMS_RATE_LIMITED', 'SMS_UNAVAILABLE', 'SMS_CODE_INVALID', 'AUTH_INTERNAL_ERROR', 'INVALID_REQUEST',
]);

function mobilePayload(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw nativeError('account_platform_request_failed');
  return value;
}

function validID(value) {
  return typeof value === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value)
    && value !== '00000000-0000-0000-0000-000000000000';
}

function nativeError(value) {
  // Native errors carry stable codes only; provider bodies and credentials must
  // never become WebView error messages.
  const mobile = typeof value === 'string'
    ? /^mobile_auth_failed:([A-Z_]+)(?::([1-9][0-9]{0,4}))?$/.exec(value) : null;
  if (mobile && mobileErrorCodes.has(mobile[1])
    && (mobile[2] === undefined || (mobile[1] === 'SMS_RATE_LIMITED' && Number(mobile[2]) <= 86400))) {
    const error = new Error(mobile[1]);
    error.code = mobile[1];
    if (mobile[2] !== undefined) error.retryAfterSeconds = Number(mobile[2]);
    return error;
  }
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

  async getModelCatalog(signal) {
    return builtinModelCatalog(await this.call(() => this.transport.getModelCatalog(), signal));
  }
  async setDefaultModel(model, signal) {
    const result = builtinModelCatalog(await this.call(() => this.transport.setDefaultModel(model), signal));
    if (result.default_model !== model || !result.available_models.includes(model)) {
      throw nativeError('account_platform_request_failed');
    }
    return result;
  }
  async session(signal) { return sessionStatus(await this.call(() => this.transport.session(), signal)); }
  async login(account, password, signal) {
    const status = sessionStatus(await this.call(() => this.transport.login({ account, password }), signal));
    if (!status.authenticated) throw nativeError(status.errorCode || 'authentication_required');
    return status;
  }
  async createSliderCaptcha(mobile, purpose, signal) {
    const value = mobilePayload(await this.call(() => this.transport.createSliderCaptcha({ mobile, purpose }), signal));
    const sizes = ['width', 'height', 'piece_width', 'piece_height'];
    if (!validID(value.captcha_id) || !sizes.every(key => Number.isInteger(value[key]) && value[key] > 0 && value[key] <= 4096)
      || value.piece_width >= value.width || value.piece_height > value.height
      || !Number.isInteger(value.piece_y) || value.piece_y < 0 || value.piece_y + value.piece_height > value.height
      || !safePNGImage(value.background_image) || !safePNGImage(value.piece_image)
      || typeof value.expires_at !== 'string' || !Number.isFinite(Date.parse(value.expires_at))) throw nativeError('account_platform_request_failed');
    return Object.fromEntries(['captcha_id', 'background_image', 'piece_image', ...sizes, 'piece_y', 'expires_at'].map(key => [key, value[key]]));
  }
  async precheckMobileRegistration(username, mobile, password, signal) {
    const value = mobilePayload(await this.call(() => this.transport.precheckMobileRegistration({ username, mobile, password }), signal));
    if (value.available !== true || typeof value.username !== 'string' || !/^[A-Za-z][A-Za-z0-9._-]{2,31}$/.test(value.username)) throw nativeError('account_platform_request_failed');
    return { username: value.username, available: true };
  }
  async sendSms(username, mobile, password, purpose, captchaId, captchaOffset, signal) {
    const value = mobilePayload(await this.call(() => this.transport.sendSms({
      username, mobile, password, purpose, captcha_id: captchaId, captcha_offset: captchaOffset,
    }), signal));
    if (!validID(value.challenge_id) || !Number.isInteger(value.retry_after_seconds) || value.retry_after_seconds < 1 || value.retry_after_seconds > 86400
      || typeof value.expires_at !== 'string' || !Number.isFinite(Date.parse(value.expires_at))) throw nativeError('account_platform_request_failed');
    return { challenge_id: value.challenge_id, retry_after_seconds: value.retry_after_seconds, expires_at: value.expires_at };
  }
  async registerMobile(username, mobile, challengeId, code, password, signal) {
    const value = mobilePayload(await this.call(() => this.transport.registerMobile({
      username, mobile, challenge_id: challengeId, code, password,
    }), signal));
    if (!validID(value.user_id) || typeof value.username !== 'string' || !/^[A-Za-z][A-Za-z0-9._-]{2,31}$/.test(value.username)
      || value.status !== 'PROVISIONING' || typeof value.password_preserved !== 'boolean') throw nativeError('account_platform_request_failed');
    return { user_id: value.user_id, username: value.username, status: value.status, password_preserved: value.password_preserved };
  }
  async loginSms(mobile, challengeId, code, signal) {
    const status = sessionStatus(await this.call(() => this.transport.loginSms({
      mobile, challenge_id: challengeId, code,
    }), signal));
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
  rechargeOrders(signal) { return this.request('token', '/v1/recharge/orders', { signal }); }
  entitlementPackages(signal) { return this.request('token', '/v1/me/entitlement-packages', { signal }); }
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
