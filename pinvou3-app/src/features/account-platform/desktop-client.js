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

  session(signal) { return this.call(() => this.transport.session(), signal); }
  openPortal(signal) { return this.call(() => this.transport.openPortal(), signal); }
  selectCatalogModel(modelId, signal) { return this.call(() => this.transport.selectCatalogModel({ modelId }), signal); }
  selectSessionCatalogModel(sessionId, modelId, signal) {
    return this.call(() => this.transport.selectSessionCatalogModel({ sessionId, modelId }), signal);
  }
  modelCatalog(signal) { return this.call(() => this.transport.modelCatalog(), signal); }
  branchReplacementSession(input, signal) { return this.call(() => this.transport.branchReplacementSession(input), signal); }
  login(account, password, signal) {
    return this.call(() => this.transport.login({ account, password }), signal);
  }
  createSliderCaptcha(mobile, purpose, signal) { return this.call(() => this.transport.createSliderCaptcha({ mobile, purpose }), signal); }
  precheckMobileRegistration(username, mobile, password, signal) { return this.call(() => this.transport.precheckMobileRegistration({ username, mobile, password }), signal); }
  sendSms(username, mobile, passwordReference, purpose, captchaId, captchaOffset, signal) { return this.call(() => this.transport.sendSms({ username, mobile, password_handle: passwordReference?.password_handle || '', password_policy_version: passwordReference?.password_policy_version || '', purpose, captcha_id: captchaId, captcha_offset: captchaOffset }), signal); }
  registerMobile(username, mobile, challengeId, code, passwordReference, recovery, signal) { return this.call(() => this.transport.registerMobile({ username, mobile, challenge_id: challengeId, code, password_handle: passwordReference?.password_handle || '', password_policy_version: passwordReference?.password_policy_version || '', registration_operation_id: recovery?.registration_operation_id || '', registration_query_token: recovery?.registration_query_token || '' }), signal); }
  registrationStatus(signal) { return this.call(() => this.transport.registrationStatus(), signal); }
  loginSms(mobile, challengeId, code, signal) { return this.call(() => this.transport.loginSms({ mobile, challenge_id: challengeId, code }), signal); }
  resetPassword(mobile, challengeId, code, password, signal) { return this.call(() => this.transport.resetPassword({ mobile, challenge_id: challengeId, code, password }), signal); }
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
    this.observeContextVersion(response.contextVersion);
    if (response.status < 200 || response.status >= 300) throw responseError(response);
    return response.body;
  }

  bootstrap(signal) { return this.request('account', '/v1/account/bootstrap', { method: 'POST', body: {}, signal }); }
  accountClosure(signal) { return this.request('account', '/v1/me/account-closure-request', { signal }); }
  createAccountClosure(confirmation, key) { return this.request('account', '/v1/me/account-closure-requests', { method: 'POST', idempotencyKey: key, body: { confirmation } }); }
  organizations(signal) { return this.request('account', '/v1/me/organizations', { signal }); }
  switchContext(contextType, organizationId, expectedContextVersion, signal) { return this.request('account', '/v1/sessions/current/active-context', { method: 'POST', body: { context_type: contextType, organization_id: organizationId || null, expected_context_version: expectedContextVersion }, signal }); }
  units(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/units`, { signal }); }
  members(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/members`, { signal }); }
  quotaMembers(organizationId, signal) { return this.request('account', `/v1/organizations/${organizationId}/quota-members`, { signal }); }
  createUnit(organizationId, name, parentId = null) { return this.request('account', `/v1/organizations/${organizationId}/units`, { method: 'POST', body: { name, parent_id: parentId } }); }
  invite(organizationId, email, roles) { return this.request('account', `/v1/organizations/${organizationId}/invitations`, { method: 'POST', body: { email, roles, expires_in_hours: 72 } }); }
  products(signal) { return this.request('token', '/v1/recharge/products', { signal }); }
  models(signal) { return this.request('token', '/v1/models', { signal }); }
  organizationModelPolicy(organizationId, signal) { return this.request('token', `/v1/organizations/${organizationId}/model-policy`, { signal }); }
  updateOrganizationModelPolicy(organizationId, policy, key) { return this.request('token', `/v1/organizations/${organizationId}/model-policy`, { method: 'PUT', idempotencyKey: key, body: policy }); }
  balance(signal) { return this.request('token', '/v1/me/balance?consistency=strong', { signal }); }
  createRecharge(productId, channel, key) { return this.request('token', '/v1/recharge/orders', { method: 'POST', idempotencyKey: key, body: { product_id: productId, channel } }); }
  purchaseOrganizationEntitlements(organizationId, productId, channel, key) { return this.request('token', `/v1/organizations/${organizationId}/recharge/orders`, { method: 'POST', idempotencyKey: key, body: { product_id: productId, channel } }); }
  organizationQuotaGrants(organizationId, signal) { return this.request('token', `/v1/organizations/${organizationId}/quota-grants`, { signal }); }
  allocateOrganizationQuota(organizationId, items, key) { return this.request('token', `/v1/organizations/${organizationId}/quota-grants:batch`, { method: 'POST', idempotencyKey: key, body: { items } }); }
  revokeOrganizationQuota(organizationId, grantId, reason, key) { return this.request('token', `/v1/organizations/${organizationId}/quota-grants/${grantId}/revoke`, { method: 'POST', idempotencyKey: key, body: { reason } }); }
  reclaimAccountClosedGrant(organizationId, grantId, reason, key) { return this.request('token', `/v1/organizations/${organizationId}/account-closed-grants/${grantId}/reclaim`, { method: 'POST', idempotencyKey: key, body: { reason } }); }
  recentRechargeOrder(signal) { return this.request('token', '/v1/recharge/orders/recent', { signal }); }
  rechargeOrders(signal) { return this.request('token', '/v1/recharge/orders', { signal }); }
  rechargeOrder(orderId, signal) { return this.request('token', `/v1/recharge/orders/${orderId}`, { signal }); }
  entitlementPackages(signal) { return this.request('token', '/v1/me/entitlement-packages', { signal }); }
  usageHistory(cursor = '', signal) {
    const next = cursor ? `&cursor=${encodeURIComponent(cursor)}` : '';
    return this.request('token', `/v1/me/usage-requests?limit=20${next}`, { signal });
  }
  cancelUsage(usageRequestId, key) { return this.request('token', `/v1/usage-requests/${encodeURIComponent(usageRequestId)}/cancel`, { method: 'POST', idempotencyKey: key, body: {} }); }
}
