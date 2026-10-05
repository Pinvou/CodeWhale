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
  'PASSWORD_EDGE_SPACE_NOT_ALLOWED', 'PASSWORD_REFERENCE_INVALID', 'PASSWORD_REFERENCE_CONFLICT',
  'REGISTRATION_IDEMPOTENCY_CONFLICT', 'REGISTRATION_RESTART_REQUIRED', 'REGISTRATION_OPERATION_NOT_FOUND',
  'RECENT_AUTHENTICATION_REQUIRED', 'PROFILE_CHANGE_CONFLICT', 'PROFILE_CHANGE_NOT_FOUND', 'PROFILE_CHANGE_FAILED',
  'MOBILE_VERIFICATION_REQUIRED',
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

function boundedSecret(value) { return typeof value === 'string' && value.length > 0 && value.length <= 4096 && !/[\u0000-\u001f\u007f]/u.test(value); }
function validTime(value) { return typeof value === 'string' && Number.isFinite(Date.parse(value)); }
function validPasswordReference(value) { return boundedSecret(value.password_handle) && boundedSecret(value.password_policy_version) && validTime(value.expires_at) && Date.parse(value.expires_at) > Date.now(); }
const registrationStates = new Set(['SMS_PENDING','REGISTRATION_IN_PROGRESS','IDENTITY_CREATING','IDENTITY_CREATED','ACCOUNT_COMMITTING','ACCOUNT_COMMITTED','PROVISIONING','ACTIVE','REPAIR_REQUIRED','COMPENSATION_PENDING','COMPENSATED','EXPIRED']);
function registrationResult(value, expectedID) {
  if (!validID(value.registration_operation_id) || (expectedID && value.registration_operation_id !== expectedID)
    || !registrationStates.has(value.status) || typeof value.password_preserved !== 'boolean'
    || (value.user_id != null && !validID(value.user_id))
    || (value.username != null && (typeof value.username !== 'string' || !/^[A-Za-z][A-Za-z0-9._-]{2,31}$/.test(value.username)))) throw nativeError('account_platform_request_failed');
  const result = { registration_operation_id:value.registration_operation_id, status:value.status, password_preserved:value.password_preserved };
  for (const key of ['user_id','username','user_code']) if (typeof value[key] === 'string') result[key] = value[key];
  if (value.next_retry_at != null) { if (!validTime(value.next_retry_at)) throw nativeError('account_platform_request_failed'); result.next_retry_at = value.next_retry_at; }
  return result;
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
    if (value.available !== true || value.username !== username.trim() || !validPasswordReference(value)) throw nativeError('account_platform_request_failed');
    return { username: value.username, available: true, password_handle:value.password_handle,
      password_policy_version:value.password_policy_version, expires_at:value.expires_at };
  }
  async sendSms(username, mobile, reference, purpose, captchaId, captchaOffset, signal) {
    const value = mobilePayload(await this.call(() => this.transport.sendSms({
      username, mobile, password_handle:reference?.password_handle || '', password_policy_version:reference?.password_policy_version || '',
      purpose, captcha_id: captchaId, captcha_offset: captchaOffset,
    }), signal));
    if (!validID(value.challenge_id) || !Number.isInteger(value.retry_after_seconds) || value.retry_after_seconds < 1 || value.retry_after_seconds > 86400
      || !validTime(value.expires_at)) throw nativeError('account_platform_request_failed');
    const result = { challenge_id:value.challenge_id, retry_after_seconds:value.retry_after_seconds, expires_at:value.expires_at };
    if (purpose === 'REGISTER') {
      if (!validID(value.registration_operation_id) || !validTime(value.registration_query_token_expires_at)
        || typeof value.recovery_persisted !== 'boolean'
        || Object.prototype.hasOwnProperty.call(value,'registration_query_token')) throw nativeError('account_platform_request_failed');
      Object.assign(result, { registration_operation_id:value.registration_operation_id,
        registration_query_token_expires_at:value.registration_query_token_expires_at, recovery_persisted:value.recovery_persisted });
    }
    return result;
  }
  async registerMobile(username, mobile, challengeId, code, reference, recovery, signal) {
    const value = mobilePayload(await this.call(() => this.transport.registerMobile({
      username, mobile, challenge_id: challengeId, code,
      password_handle:reference?.password_handle, password_policy_version:reference?.password_policy_version,
      registration_operation_id:recovery?.registration_operation_id,
    }), signal));
    return registrationResult(value, recovery?.registration_operation_id);
  }
  async registrationStatus(signal) {
    const value = await this.call(() => this.transport.registrationStatus(), signal);
    return value === null ? null : registrationResult(mobilePayload(value));
  }
  async resetPassword(mobile, challengeId, code, password, signal) {
    const value = mobilePayload(await this.call(() => this.transport.resetPassword({ mobile, challenge_id:challengeId, code, password }), signal));
    return value;
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
  myRoleBindings(signal) { return this.request('account', '/v1/me/role-bindings', { signal }); }
  createProfileChange(field, value) { return this.request('account', '/v1/me/profile-change-requests', { method:'POST', body:{ field, value } }); }
  sendProfileMobileChallenge(requestId, captchaId, captchaOffset) { return this.request('account', `/v1/me/profile-change-requests/${requestId}/mobile-challenges`, { method:'POST', body:{ captcha_id:captchaId, captcha_offset:captchaOffset } }); }
  completeProfileChange(requestId, code = '') { return this.request('account', `/v1/me/profile-change-requests/${requestId}/actions/complete`, { method:'POST', body:{ code } }); }
  profileChangeStatus(requestId, signal) { return this.request('account', `/v1/me/profile-change-requests/${requestId}`, { signal }); }
  usageHistory(cursor = '', signal) {
    const next = cursor ? `&cursor=${encodeURIComponent(cursor)}` : '';
    return this.request('token', `/v1/me/usage-requests?limit=20${next}`, { signal });
  }
}
