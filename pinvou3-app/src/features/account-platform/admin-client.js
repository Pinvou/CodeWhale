import { parseUsageJSON, encodeUsageJSON } from './admin-usage-governance.js';
import { parseFinanceJSON, encodeFinanceJSON, requireFinanceID, requirePaymentOrder, integerMoney } from './finance-contract.js';
import { ENTITLEMENT_CORRECTION_COMMAND, parseEntitlementCorrectionJSON, encodeEntitlementCorrectionJSON, parseApprovalEntitlementJSON } from './admin-entitlement-correction.js';
import { requireSession } from './personal-contract.js';
import { mergeObservedVersions } from './authorization-ui.js';
import { secureRequestID } from './request-id.js';
import { requireReconciliationDate, requireReconciliationRun } from './reconciliation-contract.js';
import { responseRetryAfter } from './auth-error.js';
function errorFromResponse(response, payload) {
  const error = new Error(payload?.message || `HTTP ${response.status}`);
  error.code = payload?.code || 'REQUEST_FAILED';
  error.status = response.status;
  error.details = payload?.details || null;
  error.requestID = payload?.request_id || '';
  error.retryAfterSeconds = responseRetryAfter(payload, response.headers.get('Retry-After'));
  return error;
}
// Only the installed write-off legacy rows use these int64 versions. Preserve
// the original decoding of every other legacy command, including ENT's codec.
function parseWriteOffApprovalJSON(text, fallback = JSON.parse) {
  const original = fallback(text);
  if (original?.command_type === 'RECEIVABLE_WRITE_OFF') return parseFinanceJSON(text);
  if (!original?.items?.some(item => item?.command_type === 'RECEIVABLE_WRITE_OFF')) return original;
  const exact = parseFinanceJSON(text);
  return { ...original, items: original.items.map((item, i) => item?.command_type === 'RECEIVABLE_WRITE_OFF' ? exact.items[i] : item) };
}
function abortRequestError() {
  if (typeof globalThis.DOMException === 'function') return new globalThis.DOMException('The operation was aborted.', 'AbortError');
  const error = new Error('The operation was aborted.');
  error.name = 'AbortError';
  return error;
}
function waitForSharedRead(entry, signal) {
  if (signal?.aborted) return Promise.reject(abortRequestError());
  entry.consumers += 1;
  return new Promise((resolve, reject) => {
    let finished = false;
    const finish = (callback, value) => {
      if (finished) return;
      finished = true;
      if (signal) signal.removeEventListener('abort', aborted);
      entry.consumers -= 1;
      if (entry.consumers === 0 && !entry.settled) {
        entry.abandon();
        entry.controller.abort();
      }
      callback(value);
    };
    const aborted = () => finish(reject, abortRequestError());
    if (signal) signal.addEventListener('abort', aborted, { once: true });
    entry.promise.then(
      (value) => {
        finish(resolve, value);
      },
      (error) => {
        finish(reject, error);
      },
    );
  });
}

function approvalControlChanged() { return Object.assign(new Error('approval control changed'), { code: 'APPROVAL_ACTION_CONTROL_CHANGED' }); }
function approvalControlKey(value) { return JSON.stringify([value?.control?.version, value?.control?.epoch, value?.control?.authorization_relay_epoch, value?.session_approval_wire_version]); }
function billingCollectionQuery(filters = {}, { keys = [], dateTimeKeys = [] } = {}) {
 const query = new URLSearchParams({ limit: String(filters.limit || 25) });
 for (const key of [...keys, ...dateTimeKeys, 'cursor']) if (filters[key]) query.set(key, filters[key]);
 return query;
}
function requireManagedPriceAck(value, expectedID = '') {
  requireFinanceID(value?.price_version_id);
  if (expectedID && value.price_version_id !== expectedID) throw new Error('INVALID_MUTATION_ACK');
  return value;
}
export class AdminPlatformClient {
  constructor(baseURL = '') {
    this.baseURL = String(baseURL || '').replace(/\/$/, '');
    this.csrfToken = '';
    this.financeSessionEpoch = 0;
    this.authorizationVersions = {};
    this.authorizationVersionListeners = new Set();
    this.inflightReads = new Map();
    this.approvalControl = null;
    this.approvalControllers = new Set();
  }

  onAuthorizationVersion(listener) {
    this.authorizationVersionListeners.add(listener);
    return () => this.authorizationVersionListeners.delete(listener);
  }

  observeAuthorizationVersions(headers) {
    const next = mergeObservedVersions(this.authorizationVersions, {
      context_version: headers.get('X-Context-Version'),
      personal_authorization_version: headers.get('X-Personal-Authorization-Version'),
      platform_authorization_version: headers.get('X-Platform-Authorization-Version'),
      organization_authorization_version: headers.get('X-Organization-Authorization-Version'),
    });
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
    if (response.status === 401) { if (this.csrfToken) this.financeSessionEpoch++; this.csrfToken = ''; return { authenticated: false }; }
    if (!response.ok) throw errorFromResponse(response, await response.json().catch(() => ({})));
    const payload = await response.json();
    const current = requireSession(payload);
    if (this.csrfToken !== current.csrf_token) this.financeSessionEpoch++;
    this.csrfToken = current.csrf_token;
    return current;
  }

  reauthenticate(returnTo = '/admin.html') {
    globalThis.location.assign(`${this.baseURL}/auth/login?reauthenticate=1&return_to=${encodeURIComponent(returnTo)}`);
  }

  async request(area, resource, signal, decode = null) {
    if (signal?.aborted) throw abortRequestError();
    const url = `${this.baseURL}/api/admin/${area}${resource}`;
    let entry = this.inflightReads.get(url);
    if (!entry) {
      entry = { controller: new globalThis.AbortController(), consumers: 0, settled: false, promise: null, abandon: null };
      entry.abandon = () => {
        if (this.inflightReads.get(url) === entry) this.inflightReads.delete(url);
      };
      entry.promise = (async () => {
        const response = await fetch(url, {
          headers: { Accept: 'application/json' },
          credentials: 'same-origin',
          cache: 'no-store',
          signal: entry.controller.signal,
        });
        this.observeAuthorizationVersions(response.headers);
        const payload = response.ok && decode ? decode(await response.text()) : area === 'billing' && response.ok ? parseFinanceJSON(await response.text()) : await response.json().catch(() => ({}));
        if (!response.ok) throw errorFromResponse(response, payload);
        return payload;
      })();
      this.inflightReads.set(url, entry);
      const cleanup = () => {
        entry.settled = true;
        entry.abandon();
      };
      entry.promise.then(cleanup, cleanup);
    }
    return waitForSharedRead(entry, signal);
  }

  invalidateRead(area, resource) {
    const url = `${this.baseURL}/api/admin/${area}${resource}`;
    const entry = this.inflightReads.get(url);
    if (!entry) return;
    entry.abandon();
    entry.controller.abort();
  }

  async mutate(area, resource, method, body, idempotencyKey = '', signal, expectedStatus = 0, codec = null) {
    const headers = {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      'X-CSRF-Token': this.csrfToken,
    };
    if (idempotencyKey) headers['Idempotency-Key'] = idempotencyKey;
    const options = {
      method,
      headers,
      credentials: 'same-origin',
      cache: 'no-store',
      body: body === undefined ? undefined : codec ? codec.encode(body) : area === 'billing' ? encodeFinanceJSON(body) : JSON.stringify(body),
      signal,
    };
    const response = await fetch(`${this.baseURL}/api/admin/${area}${resource}`, options);
    this.observeAuthorizationVersions(response.headers);
    const payload = response.status === 204 ? null : response.ok && codec ? codec.decode(await response.text()) : area === 'billing' && response.ok ? parseFinanceJSON(await response.text()) : await response.json().catch(() => ({}));
    if (!response.ok) throw errorFromResponse(response, payload);
    if (expectedStatus && !(Array.isArray(expectedStatus) ? expectedStatus.includes(response.status) : response.status === expectedStatus)) throw new Error('INVALID_MUTATION_ACK');
    return payload;
  }

  approvalCommandPrecheck(input, signal) {
    return this.mutate('account', '/admin/v2/approval-command-prechecks', 'POST', input, '', signal, 200,
      input?.command_type === ENTITLEMENT_CORRECTION_COMMAND
        ? { encode: encodeEntitlementCorrectionJSON, decode: parseEntitlementCorrectionJSON }
        : { encode: encodeFinanceJSON, decode: parseFinanceJSON });
  }
  entitlementCorrectionPrecheck(packageID, orderID, reasonCode, signal) {
    return this.approvalCommandPrecheck({
      command_type: ENTITLEMENT_CORRECTION_COMMAND,
      target_resource_id: requireFinanceID(packageID),
      payment_order_id: requireFinanceID(orderID),
      reason_code: reasonCode,
    }, signal);
  }

  async authorization(signal) {
    const response = await fetch(`${this.baseURL}/api/account/v1/account/bootstrap`, {
      method: 'POST',
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
        'X-CSRF-Token': this.csrfToken,
      },
      body: '{}',
      credentials: 'same-origin',
      cache: 'no-store',
      signal,
    });
    this.observeAuthorizationVersions(response.headers);
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw errorFromResponse(response, payload);
    return payload;
  }

  smsConfiguration(signal) {
    return this.request('account', '/admin/v1/sms-configuration', signal);
  }

  updateSMSConfiguration(input, idempotencyKey = `sms-configuration-${secureRequestID()}`, signal) {
    return this.mutate('account', '/admin/v1/sms-configuration', 'PUT', input, idempotencyKey, signal);
  }

  accountClosures(signal) { return this.request('account', '/admin/v1/account-closure-requests', signal); }
  retryAccountClosure(id, reason, key, signal) { return this.mutate('account', `/admin/v1/account-closure-requests/${encodeURIComponent(id)}/actions/retry`, 'POST', { reason }, key, signal, 200); }

  registrationBonusPolicy(signal, { fresh = false } = {}) {
    if (fresh) this.invalidateRead('billing', '/admin/v1/registration-bonus-policy');
    return this.request('billing', '/admin/v1/registration-bonus-policy', signal);
  }
  updateRegistrationBonusPolicy(input, idempotencyKey = `registration-bonus-${secureRequestID()}`, signal) {
    return this.mutate('billing', '/admin/v1/registration-bonus-policy', 'PUT', input, idempotencyKey, signal);
  }
  users(searchOrFilters = '', signal, cursor = '') {
    const filters = typeof searchOrFilters === 'string' ? { q:searchOrFilters, cursor } : (searchOrFilters || {});
    const query = new URLSearchParams({ limit: '25' });
    if (filters.q) query.set('q', filters.q);
    if (filters.cursor) query.set('cursor', filters.cursor);
    if (filters.exclude_organization_id) query.set('exclude_organization_id', filters.exclude_organization_id);
    return this.request('account', `/admin/v1/users?${query}`, signal);
  }

  setUserStatus(userID, status) {
    if (!['ACTIVE', 'DISABLED'].includes(status)) return Promise.reject(new Error('INVALID_USER_STATUS'));
    return this.mutate('account', `/admin/v1/users/${encodeURIComponent(userID)}/status`, 'PATCH', { status }, '', undefined, 204);
  }

  registrationRepairs(filters = {}, signal) {
    const query = new URLSearchParams({ limit: '100' });
    for (const key of ['status', 'stage', 'responsibility', 'sla', 'older_than_minutes']) if (filters[key]) query.set(key, filters[key]);
    return this.request('account', `/admin/v1/registration-operations?${query}`, signal);
  }

  async retryRegistration(operationID) {
    const result = await this.mutate('account', `/admin/v1/registration-operations/${encodeURIComponent(operationID)}/actions/retry`, 'POST', undefined, '', undefined, 202);
    if (result?.registration_operation_id !== operationID || typeof result.status !== 'string' || !result.status) throw new Error('INVALID_MUTATION_ACK');
    return result;
  }

  compensateRegistration(operationID) {
    return this.mutate('account', `/admin/v1/registration-operations/${encodeURIComponent(operationID)}/actions/compensate`, 'POST', undefined, '', undefined, 204);
  }

  profileChangeRepairs(signal) {
    return this.request('account', '/admin/v1/profile-change-requests', signal);
  }

  async retryProfileChange(requestID) {
    const result = await this.mutate('account', `/admin/v1/profile-change-requests/${encodeURIComponent(requestID)}/actions/retry`, 'POST', undefined, '', undefined, 202);
    if (result?.request_id !== requestID || result.status !== 'RETRY' || typeof result.action !== 'string' || !result.action || !['profile_change', 'password_reset'].includes(result.resource_type)) throw new Error('INVALID_MUTATION_ACK');
    return result;
  }

  paymentOrders(filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['q', 'status', 'provider_id', 'channel'], dateTimeKeys: ['created_from', 'created_to'] });
    return this.request('billing', `/admin/v1/payment-orders?${query}`, signal);
  }
  paymentOrder(orderID, signal) {
    return this.request('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}`, signal);
  }
  paymentProviderControls(signal) {
    return this.request('billing', '/admin/v1/payment-provider-controls', signal);
  }
  refunds(filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['q', 'status', 'provider_id'], dateTimeKeys: ['created_from', 'created_to'] });
    return this.request('billing', `/admin/v1/refunds?${query}`, signal);
  }
  syncPaymentOrder(orderID) {
    return this.mutate('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/sync`, 'POST', {}, '', undefined, 200);
  }
  repairPaymentOrder(orderID, reason) {
    return this.mutate('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/repair`, 'POST', { reason }, '', undefined, 200);
  }
  async refundPaymentOrder(orderID, transactionID, packageID, reason, idempotencyKey) {
    [orderID, transactionID, packageID].forEach(requireFinanceID);
    const value = await this.mutate(
      'billing',
      `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/refunds`,
      'POST',
      {
        payment_transaction_id: transactionID,
        entitlement_package_id: packageID,
        reason,
      },
      idempotencyKey,
      undefined, 201,
    );
    requireFinanceID(value?.refund_id);
    if (value.payment_order_id !== orderID || value.payment_transaction_id !== transactionID || value.entitlement_package_id !== packageID || value.reason !== reason) throw new Error('INVALID_FINANCE_ACK');
    return value;
  }
  adjustments(filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['wallet_id', 'status', 'requested_by'], dateTimeKeys: ['created_from', 'created_to'] });
    return this.request('billing', `/admin/v1/adjustments?${query}`, signal);
  }
  createAdjustment(input) {
    return this.mutate('billing', '/admin/v1/adjustments', 'POST', input, input.idempotency_key, undefined, 201);
  }
  approvalRequests(filters = {}, signal) {
    const query = new URLSearchParams({ limit: '200' });
    for (const [key, value] of Object.entries(filters)) if (value) query.set(key, value);
    return this.request('account', `/admin/v1/approval-requests?${query}`, signal, text => parseApprovalEntitlementJSON(text, value => parseWriteOffApprovalJSON(value)));
  }
  approvalRequest(requestID, signal) {
    return this.request('account', `/admin/v1/approval-requests/${encodeURIComponent(requestID)}`, signal, text => parseApprovalEntitlementJSON(text, value => parseWriteOffApprovalJSON(value)));
  }
  async governedApprovalCapability(signal) {
    const response = await fetch(`${this.baseURL}/api/admin/approvals/capability`, {
      headers: { Accept: 'application/json' }, credentials: 'same-origin', cache: 'no-store', signal,
    });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) {
      if (payload?.code === 'APPROVAL_PROTOCOL_UNAVAILABLE' || payload?.code === 'APPROVAL_CAPABILITY_UNAVAILABLE') {
        this.approvalControl = null;
        for (const controller of this.approvalControllers) controller.abort();
        this.approvalControllers.clear();
      }
      throw errorFromResponse(response, payload);
    }
    const control = payload?.control;
    if (!Number.isSafeInteger(control?.version) || control.version < 1 ||
      !Number.isSafeInteger(control?.epoch) || control.epoch < 1 ||
      !Number.isSafeInteger(control?.authorization_relay_epoch) || control.authorization_relay_epoch < 1 ||
      ![1, 2].includes(payload?.session_approval_wire_version)) throw approvalControlChanged();
    const current = this.approvalControl;
    if (current && control.version < current.control.version) {
      throw approvalControlChanged();
    }
    if (current && control.version === current.control.version && approvalControlKey(payload) !== approvalControlKey(current)) {
      this.approvalControl = null;
      for (const controller of this.approvalControllers) controller.abort();
      this.approvalControllers.clear();
      throw approvalControlChanged();
    }
    if (!current || approvalControlKey(payload) !== approvalControlKey(current)) {
      this.approvalControl = payload;
      for (const controller of this.approvalControllers) controller.abort();
      this.approvalControllers.clear();
    }
    return payload;
  }
  async governedApprovalRead(path, { detail = false, signal } = {}) {
    const capability = await this.governedApprovalCapability(signal);
    const key = approvalControlKey(capability);
    const controller = new AbortController();
    const abort = () => controller.abort();
    signal?.addEventListener('abort', abort, { once: true });
    if (signal?.aborted) controller.abort();
    this.approvalControllers.add(controller);
    try {
      const response = await fetch(`${this.baseURL}/api/admin/approvals/requests${path}`, {
        headers: { Accept: 'application/json' }, credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
      });
      const payload = response.ok ? parseApprovalEntitlementJSON(await response.text()) : await response.json().catch(() => ({}));
      if (!response.ok) {
        if (payload?.code === 'APPROVAL_ACTION_CONTROL_CHANGED' || payload?.code === 'APPROVAL_PROTOCOL_UNAVAILABLE') {
          this.approvalControl = null;
          for (const active of this.approvalControllers) active.abort();
        }
        throw errorFromResponse(response, payload);
      }
      if (approvalControlKey(this.approvalControl) !== key) throw approvalControlChanged();
      if (!detail) {
        return { ...payload, items: (payload.items || []).map(item => ({ ...item, allowed_actions: [] })) };
      }
      if (capability.session_approval_wire_version === 2 &&
        payload.detail_availability === 'DETAIL_PRESENTATION_UNAVAILABLE' && !('allowed_actions' in payload)) {
        return { ...payload, allowed_actions: [] };
      }
      const rawVersion = capability.session_approval_wire_version === 2
        ? payload.action_control_version : response.headers.get('X-Pinvou-Approval-Action-Control-Version');
      const version = capability.session_approval_wire_version === 1 && !/^[1-9][0-9]*$/.test(rawVersion || '')
        ? NaN : Number(rawVersion);
      if (!Number.isSafeInteger(version) || version !== capability.control.version) throw approvalControlChanged();
      return { ...payload, action_control_version: version };
    } finally {
      this.approvalControllers.delete(controller);
      signal?.removeEventListener('abort', abort);
    }
  }
  governedApprovalRequests(filters = {}, signal) {
    const query = new URLSearchParams({ limit: String(filters.limit || 50) });
    for (const [key, value] of Object.entries(filters)) if (key !== 'limit' && value) query.set(key, value);
    return this.governedApprovalRead(`?${query}`, { signal });
  }
  governedApprovalRequest(requestID, signal) {
    return this.governedApprovalRead(`/${encodeURIComponent(requestID)}`, { detail: true, signal });
  }
  // The business page reads the closed owner projection of one repair target
  // through this single read-only endpoint; the browser never derives facts.
  async governedApprovalDirectory(signal) {
    const capability = await this.governedApprovalCapability(signal);
    const key = approvalControlKey(capability);
    const controller = new AbortController();
    const abort = () => controller.abort();
    signal?.addEventListener('abort', abort, { once: true });
    if (signal?.aborted) controller.abort();
    this.approvalControllers.add(controller);
    try {
      const response = await fetch(`${this.baseURL}/api/admin/approvals/directory`, {
        headers: { Accept: 'application/json' }, credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
      });
      const payload = response.ok ? parseFinanceJSON(await response.text()) : await response.json().catch(() => ({}));
      if (!response.ok) throw errorFromResponse(response, payload);
      const header = response.headers.get('X-Platform-Authorization-Version');
      if (approvalControlKey(this.approvalControl) !== key || payload?.schema_version !== 1 ||
        !Number.isSafeInteger(payload?.platform_authorization_version) || payload.platform_authorization_version < 1 ||
        !Array.isArray(payload?.items) || !/^[1-9][0-9]*$/.test(header || '') ||
        Number(header) !== payload.platform_authorization_version) throw approvalControlChanged();
      return payload;
    } finally {
      this.approvalControllers.delete(controller);
      signal?.removeEventListener('abort', abort);
    }
  }
  async decideGovernedApproval(requestID, detail, decision, reason, signal) {
    const capability = await this.governedApprovalCapability(signal);
    const version = detail?.action_control_version;
    if (detail?.approval_request_id !== requestID || !Number.isSafeInteger(version) || version !== capability.control.version ||
      !Number.isSafeInteger(detail?.version) || detail.version < 1) throw approvalControlChanged();
    const body = { expected_version: detail.version, decision, reason };
    if (capability.session_approval_wire_version === 2) body.action_control_version = version;
    else body.observed_versions = detail.critical_resource_versions || {};
    const controller = new AbortController();
    const abort = () => controller.abort();
    signal?.addEventListener('abort', abort, { once: true });
    if (signal?.aborted) controller.abort();
    this.approvalControllers.add(controller);
    try {
      const response = await fetch(`${this.baseURL}/api/admin/approvals/requests/${encodeURIComponent(requestID)}/actions/decide`, {
        method: 'POST', headers: { Accept: 'application/json', 'Content-Type': 'application/json',
          'X-CSRF-Token': this.csrfToken, 'X-Pinvou-Approval-Action-Control-Version': String(version) },
        credentials: 'same-origin', cache: 'no-store', body: encodeFinanceJSON(body), signal: controller.signal,
      });
      const payload = response.status === 204 ? {} : await response.json().catch(() => ({}));
      if (!response.ok) {
        if (payload?.code === 'APPROVAL_ACTION_CONTROL_CHANGED' || payload?.code === 'APPROVAL_PROTOCOL_UNAVAILABLE') {
          this.approvalControl = null;
          for (const active of this.approvalControllers) active.abort();
        }
        throw errorFromResponse(response, payload);
      }
      if (approvalControlKey(this.approvalControl) !== approvalControlKey(capability)) throw approvalControlChanged();
      if (response.status !== 204) throw approvalControlChanged();
      return null; // A successful decision must be followed by a fresh detail read.
    } finally {
      this.approvalControllers.delete(controller);
      signal?.removeEventListener('abort', abort);
    }
  }
  async writeGovernedApproval(kind, requestID, detail, draft, signal) {
    const capability = await this.governedApprovalCapability(signal);
    const version = capability.control.version;
    if (kind !== 'create' && (detail?.approval_request_id !== requestID ||
      !Number.isSafeInteger(detail?.action_control_version) || detail.action_control_version !== version ||
      !Number.isSafeInteger(detail?.version) || detail.version < 1)) throw approvalControlChanged();
    const body = kind === 'create' ? { ...draft } : kind === 'update'
      ? { ...draft, expected_version: detail.version } : { expected_version: detail.version };
    const suffix = kind === 'create' ? '' : kind === 'update' ? `/${encodeURIComponent(requestID)}`
      : `/${encodeURIComponent(requestID)}/actions/${kind}`;
    const controller = new AbortController();
    const abort = () => controller.abort();
    signal?.addEventListener('abort', abort, { once: true });
    if (signal?.aborted) controller.abort();
    this.approvalControllers.add(controller);
    try {
      const response = await fetch(`${this.baseURL}/api/admin/approvals/requests${suffix}`, {
        method: kind === 'update' ? 'PUT' : 'POST',
        headers: { Accept: 'application/json', 'Content-Type': 'application/json',
          'X-CSRF-Token': this.csrfToken, 'X-Pinvou-Approval-Action-Control-Version': String(version) },
        credentials: 'same-origin', cache: 'no-store',
        body: body.command_type === ENTITLEMENT_CORRECTION_COMMAND ? encodeEntitlementCorrectionJSON(body) : encodeFinanceJSON(body),
        signal: controller.signal,
      });
      const payload = response.status === 204 ? {} : await response.json().catch(() => ({}));
      if (!response.ok) {
        if (payload?.code === 'APPROVAL_ACTION_CONTROL_CHANGED' || payload?.code === 'APPROVAL_PROTOCOL_UNAVAILABLE') {
          this.approvalControl = null;
          for (const active of this.approvalControllers) active.abort();
        }
        throw errorFromResponse(response, payload);
      }
      if (approvalControlKey(this.approvalControl) !== approvalControlKey(capability)) throw approvalControlChanged();
      if (kind === 'create') {
        if (response.status !== 201) throw approvalControlChanged();
        requireFinanceID(payload.approval_request_id);
        return payload.approval_request_id;
      }
      if (response.status !== 204) throw approvalControlChanged();
      return null; // Any successful write requires a fresh detail read before another action.
    } finally {
      this.approvalControllers.delete(controller);
      signal?.removeEventListener('abort', abort);
    }
  }
  createGovernedApprovalDraft(draft, signal) {
    return this.writeGovernedApproval('create', '', null, draft, signal);
  }
  updateGovernedApprovalDraft(requestID, detail, draft, signal) {
    return this.writeGovernedApproval('update', requestID, detail, draft, signal);
  }
  submitGovernedApprovalRequest(requestID, detail, signal) {
    return this.writeGovernedApproval('submit', requestID, detail, null, signal);
  }
  withdrawGovernedApprovalRequest(requestID, detail, signal) {
    return this.writeGovernedApproval('withdraw', requestID, detail, null, signal);
  }
  createApprovalDraft(input, signal) {
    return this.mutate('account', '/admin/v1/approval-requests', 'POST', input, '', signal);
  }
  submitApprovalRequest(requestID, expectedVersion, signal) {
    return this.mutate('account', `/admin/v1/approval-requests/${encodeURIComponent(requestID)}/actions/submit`, 'POST', { expected_version: expectedVersion }, '', signal);
  }
  latePaymentReviews(filters = {}, signal) {
    const query = billingCollectionQuery(filters, {
      keys: ['q', 'status', 'severity', 'provider_id', 'owner_id'],
      dateTimeKeys: ['created_from', 'created_to', 'entered_from', 'entered_to', 'next_review_before'],
    });
    return this.request('billing', `/admin/v1/late-payment-reviews?${query}`, signal);
  }
  latePaymentReview(caseID, signal) {
    return this.request('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}`, signal);
  }
  claimLatePaymentReview(caseID) {
    return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/claim`, 'POST', {}, '', undefined, 200);
  }
  queryLatePaymentReview(caseID) {
    return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/query`, 'POST', {}, '', undefined, 200);
  }
  scheduleLatePaymentReview(caseID, nextReviewAt, reason) {
    return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/schedule`, 'POST', { next_review_at: new Date(nextReviewAt).toISOString(), reason }, '', undefined, 200);
  }
  archiveLatePaymentReview(caseID) {
    return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/archive`, 'POST', {}, '', undefined, 200);
  }
  escalateProviderBlind(caseID, reason) {
    return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/provider-blind`, 'POST', { reason }, '', undefined, 200);
  }
  paymentAnomalies(filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['q', 'anomaly_type', 'status', 'severity', 'owner_id', 'provider_id'], dateTimeKeys: ['created_from', 'created_to'] });
    return this.request('billing', `/admin/v1/payment-anomalies?${query}`, signal);
  }
  paymentAnomaly(caseID, signal) {
    return this.request('billing', `/admin/v1/payment-anomalies/${encodeURIComponent(caseID)}`, signal);
  }
  claimPaymentAnomaly(caseID, key) {
    return this.mutate('billing', `/admin/v1/payment-anomalies/${encodeURIComponent(caseID)}/claim`, 'POST', {}, key, undefined, 200);
  }
  resolvePaymentAnomaly(caseID, resolution, reason, idempotencyKey) {
    return this.mutate('billing', `/admin/v1/payment-anomalies/${encodeURIComponent(caseID)}/resolve`, 'POST', { resolution, reason }, idempotencyKey, undefined, 200);
  }

  wallets(filters = {}, signal) { return this.request('billing', `/admin/v1/wallets?${billingCollectionQuery(filters, { keys: ['q', 'status', 'owner_type'] })}`, signal); }
  walletLedger(walletID, filters = {}, signal) { return this.request('billing', `/admin/v1/wallets/${encodeURIComponent(walletID)}/ledger?${billingCollectionQuery(filters)}`, signal); }
  receivables(filters = {}, signal) { return this.request('billing', `/admin/v1/receivables?${billingCollectionQuery(filters, { keys: ['wallet_id', 'status'] })}`, signal); }
  receivableAllocations(id, signal) { return this.request('billing', `/admin/v1/receivables/${encodeURIComponent(id)}/allocations`, signal); }
  financeOverview(surface, signal) { return this.request('billing', `/admin/v1/finance-overviews/${surface}`, signal); }
  async syncOrder(orderID) { return requirePaymentOrder(await this.syncPaymentOrder(orderID), orderID); }
  rechargeProducts(filters = {}, signal) { return this.request('billing', `/admin/v1/recharge-products?${billingCollectionQuery(filters, { keys: ['q', 'lifecycle_status', 'include_deleted'] })}`, signal); }
  rechargeProductCreateOptions(signal) { return this.request('billing', '/admin/v1/recharge-product-create-options', signal); }
  async createRechargeProduct(body, key, signal) { const item = await this.mutate('billing', '/admin/v1/recharge-products', 'POST', body, key, signal, 201); requireFinanceID(item?.catalog_item_id); if (item.product_id !== body.product_id) throw new Error('INVALID_FINANCE_ACK'); return item; }
  async updateRechargeProduct(id, body, key, signal) { const item = await this.mutate('billing', `/admin/v1/recharge-products/${encodeURIComponent(id)}/draft`, 'PUT', body, key, signal, 200); if (item?.catalog_item_id !== id) throw new Error('INVALID_FINANCE_ACK'); return item; }
  productAdmissionVerifications(id, signal) { return this.request('billing', `/admin/v1/recharge-products/${encodeURIComponent(id)}/admission-verifications`, signal); }
  async productAdmissionAction(id, action, body, key, verificationID = '', signal) {
    const resource = `/admin/v1/recharge-products/${encodeURIComponent(id)}/admission-verifications${verificationID ? `/${encodeURIComponent(verificationID)}/${action}` : ''}`;
    const result = await this.mutate('billing', resource, 'POST', body, key, signal, action === 'revoke' ? 200 : 202);
    if (action === 'revoke') { if (result?.status !== 'REVOKED') throw new Error('INVALID_FINANCE_ACK'); }
    else { requireFinanceID(result?.verification_id); if (result.catalog_item_id !== id) throw new Error('INVALID_FINANCE_ACK'); }
    return result;
  }
  async transitionRechargeProduct(id, action, body, key, signal) {
    if (!['publish', 'unpublish', 'delete', 'display-names'].includes(action)) throw new Error('INVALID_FINANCE_ACK');
    const result = await this.mutate('billing', `/admin/v1/recharge-products/${encodeURIComponent(id)}/${action}`, 'POST', body, key, signal, 200);
    if (result?.catalog_item_id !== id) throw new Error('INVALID_FINANCE_ACK'); return result;
  }

  reconciliationRuns(filters = {}, signal) {
    return this.request('billing', `/admin/v1/reconciliation-runs?${billingCollectionQuery(filters, { keys: ['channel', 'status', 'provider_id', 'business_date_from', 'business_date_to'] })}`, signal);
  }
  reconciliationCases(filters = {}, signal) {
    return this.request('billing', `/admin/v1/reconciliation-cases?${billingCollectionQuery(filters, { keys: ['run_id', 'case_type', 'status', 'severity', 'owner_id'], dateTimeKeys: ['created_from', 'created_to'] })}`, signal);
  }
  async runReconciliation(facts, signal) {
    requireReconciliationDate(facts.business_date);
    return requireReconciliationRun(await this.mutate('billing', '/admin/v1/reconciliation-runs', 'POST', facts, '', signal, 200), facts);
  }
  async importReconciliation(facts, signal) {
    requireReconciliationDate(facts.business_date);
    return requireReconciliationRun(await this.mutate('billing', '/admin/v1/reconciliation-imports', 'POST', facts, '', signal, 201), facts);
  }
  async reconciliationCaseCommand(id, facts, key, signal) {
    requireFinanceID(id);
    const result = await this.mutate('billing', `/admin/v1/reconciliation-cases/${encodeURIComponent(id)}/commands`, 'POST', facts, key, signal, 200);
    if (result?.case_id !== id) throw new Error('INVALID_FINANCE_ACK');
    return result;
  }

  async logout() {
    const response = await fetch(`${this.baseURL}/api/logout`, {
      method: 'POST',
      headers: { 'X-CSRF-Token': this.csrfToken },
      credentials: 'same-origin',
    });
    const payload = response.status === 204 ? null : await response.json().catch(() => ({}));
    if (response.status === 403 && payload?.code === 'CSRF_REJECTED') {
      const confirmation = await fetch(`${this.baseURL}/api/session`, { credentials: 'same-origin', cache: 'no-store' });
      if (confirmation.status === 401) {
        this.csrfToken = '';
        this.financeSessionEpoch++;
        return null;
      }
    }
    if (!response.ok) throw errorFromResponse(response, payload);
    this.csrfToken = '';
    this.financeSessionEpoch++;
    return payload;
  }

managedPrices(signal) {
    return this.request('billing', '/admin/v1/pricing?limit=100', signal);
  }

managedPrice(priceVersionID, signal) {
    return this.request('billing', `/admin/v1/pricing?price_version_id=${encodeURIComponent(priceVersionID)}`, signal);
  }

managedPriceSpace(input, signal) {
    const query = new URLSearchParams({
      limit: '200',
      scope_type: input.scope_type,
      protocol_code: input.protocol_code,
      formula_version_id: input.formula_version_id,
      credit_unit: input.credit_unit || 'MICROCREDIT',
    });
    if (input.scope_type === 'MODEL') query.set('scope_id', input.scope_id);
    return this.request('billing', `/admin/v1/pricing?${query}`, signal);
  }

meteringUnits(signal) {
    return this.request('billing', '/admin/v1/metering-units', signal);
  }

pricingProtocols(signal) {
    return this.request('token', '/admin/v1/protocols', signal);
  }

async createMeteringUnit(input, idempotencyKey) {
    const item = await this.mutate('billing', '/admin/v1/metering-units', 'POST', input, idempotencyKey, undefined, 201);
    requireFinanceID(item?.unit_definition_id);
    if (item.unit_code !== input.unit_code || integerMoney(item.minimum_precision) !== integerMoney(input.minimum_precision) || integerMoney(item.price_denominator) !== integerMoney(input.price_denominator) || item.rounding_rule !== input.rounding_rule) throw new Error('INVALID_MUTATION_ACK');
    return item;
  }

async deactivateMeteringUnit(unitDefinitionID, idempotencyKey) {
    const item = await this.mutate('billing', `/admin/v1/metering-units/${encodeURIComponent(unitDefinitionID)}/deactivate`, 'POST', {}, idempotencyKey, undefined, 200);
    if (item?.unit_definition_id !== unitDefinitionID || item.status !== 'INACTIVE') throw new Error('INVALID_MUTATION_ACK');
    return item;
  }

async createPriceDraft(input, idempotencyKey) {
    const item = requireManagedPriceAck(await this.mutate('billing', '/admin/v1/pricing/drafts', 'POST', input, idempotencyKey, undefined, 201));
    if (item.lifecycle_status !== 'DRAFT' || item.scope_type !== input.scope_type || String(item.scope_id || '') !== String(input.scope_id || '') || item.formula_version_id !== input.formula_version_id || item.protocol_code !== input.protocol_code) throw new Error('INVALID_MUTATION_ACK');
    return item;
  }

async updatePriceDraft(priceVersionID, input, idempotencyKey) {
    const item = requireManagedPriceAck(await this.mutate('billing', `/admin/v1/pricing/${encodeURIComponent(priceVersionID)}/draft`, 'PUT', input, idempotencyKey, undefined, 200), priceVersionID);
    if (item.lifecycle_status !== 'DRAFT') throw new Error('INVALID_MUTATION_ACK');
    return item;
  }

previewPrice(priceVersionID, signal) {
    return this.request('billing', `/admin/v1/pricing/${encodeURIComponent(priceVersionID)}/preview`, signal);
  }

async publishPrice(priceVersionID, expectedETag, idempotencyKey) {
    return requireManagedPriceAck(await this.mutate('billing', `/admin/v1/pricing/${encodeURIComponent(priceVersionID)}/publish`, 'POST', { expected_etag: expectedETag }, idempotencyKey, undefined, 200), priceVersionID);
  }

async transitionPrice(priceVersionID, input, idempotencyKey) {
    return requireManagedPriceAck(await this.mutate('billing', `/admin/v1/pricing/${encodeURIComponent(priceVersionID)}/transition`, 'POST', input, idempotencyKey, undefined, 200), priceVersionID);
  }

tokenResourceReadiness(signal) {
    return this.request('token', '/admin/v1/token-resources/readiness', signal);
  }

tokenAccounts(filtersOrSignal = {}, signal) {
    if (isAbortSignal(filtersOrSignal)) {
      return this.request('token', '/admin/v1/token-accounts?limit=100', filtersOrSignal);
    }
    const filters = filtersOrSignal;
    const requestSignal = signal;
    return this.request('token', `/admin/v1/token-accounts?${tokenAdminCollectionQuery('tokenAccounts', filters)}`, requestSignal);
  }

tokenAccount(tokenAccountID, signal) {
    return this.request('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}`, signal);
  }

tokenAccountSafeDetail(tokenAccountID, signal) {
		return this.request('token', `/admin/v2/token-accounts/${encodeURIComponent(tokenAccountID)}`, signal);
	}

tokenAccountCredentials(tokenAccountID, filters = {}, signal) {
		return this.request('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/credentials?${tokenAdminCollectionQuery('tokenCredentials', filters)}`, signal);
	}

tokenAccountRoutes(tokenAccountID, filters = {}, signal) {
		return this.request('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/routes?${tokenAdminCollectionQuery('tokenRoutes', filters)}`, signal);
	}

createCredentialLaunch(tokenAccountID, action, input, idempotencyKey, signal) {
		return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/credential-launches/${String(action).toLowerCase()}`, 'POST', input, idempotencyKey, signal);
	}

credentialIsolationResult(resultRef, signal) {
		return this.request('token', `/admin/v1/credential-results/${encodeURIComponent(resultRef)}`, signal);
	}

revokeCredentialWindow(resultRef, reasonCode, signal) {
		return this.mutate('token', `/admin/v1/credential-results/${encodeURIComponent(resultRef)}/revoke-window`, 'POST', { reason_code: reasonCode }, '', signal);
	}

tokenCredentialImpact(tokenAccountID, action, credentialID, signal) {
    const query = new URLSearchParams({ action });
    if (credentialID) query.set('credential_id', credentialID);
    return this.request('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/impact/credential?${query}`, signal);
  }

catalogModels(filtersOrSignal = {}, signal) {
    if (isAbortSignal(filtersOrSignal)) {
      return this.request('token', '/admin/v1/models', filtersOrSignal);
    }
    const filters = filtersOrSignal;
    const requestSignal = signal;
    return this.request('token', `/admin/v1/models?${tokenAdminCollectionQuery('models', filters)}`, requestSignal);
  }

catalogModel(modelID, signal) {
    return this.request('token', `/admin/v1/models/${encodeURIComponent(modelID)}`, signal);
  }

modelVersions(modelID, filters = {}, signal) {
    return this.request('token', `/admin/v1/models/${encodeURIComponent(modelID)}/versions?${tokenAdminCollectionQuery('modelVersions', filters)}`, signal);
  }

modelLifecyclePlans(modelID, filters = {}, signal) {
		return this.request('token', `/admin/v1/models/${encodeURIComponent(modelID)}/lifecycle-plans?${tokenAdminCollectionQuery('modelLifecyclePlans', filters)}`, signal);
	}

modelLifecyclePlan(modelID, planID, signal) {
		return this.request('token', `/admin/v1/models/${encodeURIComponent(modelID)}/lifecycle-plans/${encodeURIComponent(planID)}`, signal);
	}

createModelLifecyclePlan(modelID, input, idempotencyKey, signal) {
		return this.mutate('token', `/admin/v1/models/${encodeURIComponent(modelID)}/lifecycle-plans`, 'POST', input, idempotencyKey, signal);
	}

cancelModelLifecyclePlan(modelID, planID, input, idempotencyKey, signal) {
		return this.mutate('token', `/admin/v1/models/${encodeURIComponent(modelID)}/lifecycle-plans/${encodeURIComponent(planID)}/cancel`, 'POST', input, idempotencyKey, signal);
	}

protocols(filters = {}, signal) {
    return this.request('token', `/admin/v1/protocols?${tokenAdminCollectionQuery('protocols', filters)}`, signal);
  }

formulas(filters = {}, signal) {
    return this.request('token', `/admin/v1/formulas?${tokenAdminCollectionQuery('formulas', filters)}`, signal);
  }

previewFormula(formulaVersionID, signal) {
    return this.request('token', `/admin/v1/formulas/${encodeURIComponent(formulaVersionID)}/preview`, signal);
  }

tokenOwnerCandidates(filters = {}, signal) {
    return this.request('token', `/admin/v1/owner-candidates?${tokenAdminCollectionQuery('ownerCandidates', filters)}`, signal);
  }

tokenResourceAudit(filters = {}, signal) {
    return this.request('token', `/admin/v1/resource-audit?${tokenAdminCollectionQuery('resourceAudit', filters)}`, signal);
  }

modelPriceProjection(modelID, protocolCode, formulaVersionID, creditUnit = 'MICROCREDIT', signal) {
    const query = new URLSearchParams({
      protocol_code: String(protocolCode || ''),
      formula_version_id: String(formulaVersionID || ''),
      credit_unit: String(creditUnit || ''),
    });
    return this.request('billing', `/admin/v1/models/${encodeURIComponent(modelID)}/price-projection?${query}`, signal);
  }

modelLifecycleImpact(modelID, targetStatus, replacementModelID = '', signal, lifecyclePlanID = '') {
    const query = new URLSearchParams({ target_status: targetStatus });
    if (replacementModelID) query.set('replacement_model_id', replacementModelID);
		if (lifecyclePlanID) query.set('lifecycle_plan_id', lifecyclePlanID);
    return this.request('token', `/admin/v1/models/${encodeURIComponent(modelID)}/impact/lifecycle?${query}`, signal);
  }

catalogRollbackImpact(sourceCatalogVersion, signal) {
    const query = new URLSearchParams({ source_catalog_version: String(sourceCatalogVersion) });
    return this.request('token', `/admin/v1/model-catalog/impact/rollback?${query}`, signal);
  }

usageRequests(filters = {}, signal) {
    // F05 advertises personal execution, not an organization producer/filter.
    const personalFilters = { ...filters };
    delete personalFilters.organization_id;
    return this.request('token', `/admin/v1/usage-requests?${tokenAdminCollectionQuery('usage', personalFilters)}`, signal, parseUsageJSON);
  }

  usageRequest(id, signal) {
    return this.request('token', `/admin/v1/usage-requests/${requireFinanceID(id)}`, signal, parseUsageJSON);
  }

  usageEvidence(id, filters = {}, signal) {
    return this.request('token', `/admin/v1/usage-requests/${requireFinanceID(id)}/evidence?${tokenAdminCollectionQuery('usageEvidence', filters)}`, signal, parseUsageJSON);
  }

  usageReconciliationCases(filters = {}, signal) {
    return this.request('token', `/admin/v1/reconciliation-cases?${tokenAdminCollectionQuery('reconciliation', filters)}`, signal, parseUsageJSON);
  }

  usageReconciliationCommandReadiness(signal) {
    return this.request('token', '/admin/v1/commands/TOKEN_USAGE_RECONCILIATION_RETRY/readiness', signal);
  }

  usageReconciliationPrecheck(input, signal) {
    if (input.command_type !== 'TOKEN_USAGE_RECONCILIATION_RETRY') throw new Error('INVALID_USAGE_RESPONSE');
    return this.mutate('token', '/admin/v1/commands:precheck', 'POST', input, '', signal, 200, { encode: encodeUsageJSON, decode: parseUsageJSON });
  }

  usageReconciliationResult(reference, signal) {
    return this.request('token', `/admin/v1/commands/${requireFinanceID(reference)}`, signal, parseUsageJSON);
  }

  usageReconciliationByIdempotencyKey(key, signal) {
    return this.mutate('token', '/admin/v1/commands/by-idempotency-key?command_type=TOKEN_USAGE_RECONCILIATION_RETRY', 'GET', undefined, key, signal, 0, { encode: encodeUsageJSON, decode: parseUsageJSON });
  }

  createUsageReconciliationCommand(input, key, signal) {
    if (input.command_type !== 'TOKEN_USAGE_RECONCILIATION_RETRY') throw new Error('INVALID_USAGE_RESPONSE');
    return this.mutate('token', '/admin/v1/commands', 'POST', input, key, signal, 0, { encode: encodeUsageJSON, decode: parseUsageJSON });
  }

  governedTokenCommandPrecheck(input, signal) {
    return this.mutate('token', '/admin/v1/commands:precheck', 'POST', input, '', signal);
  }

createGovernedTokenCommand(input, idempotencyKey, signal) {
    return this.mutate('token', '/admin/v1/commands', 'POST', input, idempotencyKey, signal);
  }

governedTokenCommandResult(commandReference, signal) {
    return this.request('token', `/admin/v1/commands/${encodeURIComponent(commandReference)}`, signal);
  }

governedTokenCommandByIdempotencyKey(commandType, idempotencyKey, signal) {
    const query = new URLSearchParams({ command_type: commandType });
    return this.mutate('token', `/admin/v1/commands/by-idempotency-key?${query}`, 'GET', undefined, idempotencyKey, signal);
  }

createTokenAccountRecoveryAction(commandReference, input, idempotencyKey, signal) {
    return this.mutate('token', `/admin/v1/commands/${encodeURIComponent(commandReference)}/recovery-actions`, 'POST', input, idempotencyKey, signal);
  }

tokenAccountRecoveryQueue(filters = {}, signal) {
    return this.request('token', `/admin/v1/commands/recovery-queue?${tokenAdminCollectionQuery('accountRecovery', filters)}`, signal);
  }

tokenAccountRecoveryAction(commandReference, actionID, signal) {
    return this.request('token', `/admin/v1/commands/${encodeURIComponent(commandReference)}/recovery-actions/${encodeURIComponent(actionID)}`, signal);
  }

tokenAccountRecoveryActionByIdempotencyKey(commandReference, idempotencyKey, signal) {
    return this.mutate('token', `/admin/v1/commands/${encodeURIComponent(commandReference)}/recovery-actions/by-idempotency-key`, 'GET', undefined, idempotencyKey, signal);
  }

tokenModelCatalogOverview(signal) { return this.request('token', '/admin/v1/overviews/model-catalog', signal); }
  tokenResourcesPageOverview(signal) { return this.request('token', '/admin/v1/overviews/token-resources', signal); }
  tokenProvisioningPageOverview(signal, window = '') { return this.request('token', `/admin/v1/overviews/model-operations${window === '24h' ? '?window=24h' : ''}`, signal); }
  provisionings(filtersOrSignal = {}, signal) {
    if (isAbortSignal(filtersOrSignal)) {
      return this.request('token', '/admin/v1/provisionings?limit=100', filtersOrSignal);
    }
    const filters = filtersOrSignal;
    const requestSignal = signal;
    return this.request('token', `/admin/v1/provisionings?${tokenAdminCollectionQuery('provisionings', filters)}`, requestSignal);
  }
}

const TOKEN_ADMIN_FILTERS = Object.freeze({
  models: ['q', 'lifecycle_status', 'protocol_code', 'source_adapter', 'source_missing'],
  protocols: ['q', 'status', 'protocol_code', 'published', 'created_from', 'created_to'],
  formulas: ['q', 'status', 'protocol_definition_id', 'metering_unit_code', 'published', 'created_from', 'created_to'],
  tokenAccounts: ['q', 'owner_type', 'owner_id', 'status', 'health_status', 'credential_status'],
  accountRecovery: ['q'],
  provisionings: ['q', 'status', 'model_id', 'token_account_id', 'scope_type', 'scope_id', 'sla_overdue'],
  usage: ['q', 'execution_status', 'settlement_status', 'model', 'token_account_id', 'organization_id', 'started_from', 'started_to'],
  reconciliation: ['q', 'status', 'severity', 'case_type', 'owner_id', 'sla_overdue'],
  ownerCandidates: ['q', 'owner_type', 'status', 'organization_id'],
  resourceAudit: ['resource_type', 'resource_id', 'action', 'actor_id', 'occurred_from', 'occurred_to'],
  modelVersions: [],
	modelLifecyclePlans: ['status', 'target_lifecycle'],
  provisioningAttempts: ['status'],
  usageEvidence: ['evidence_type'],
	tokenCredentials: ['status'],
	tokenRoutes: ['health_status'],
});

function tokenAdminCollectionQuery(source, filters = {}) {
  const query = new URLSearchParams({ limit: '50' });
  for (const key of [...(TOKEN_ADMIN_FILTERS[source] || []), 'cursor']) {
    const value = String(filters[key] || '').trim();
    if (value) query.set(key, value);
  }
  return query;
}

function isAbortSignal(value) {
  return typeof globalThis.AbortSignal !== 'undefined' && value instanceof globalThis.AbortSignal;
}
