import { parseFinanceJSON, encodeFinanceJSON, requireFinanceID, requirePaymentOrder } from './finance-contract.js';
import { requireSession } from './personal-contract.js';
import { mergeObservedVersions } from './authorization-ui.js';
import { secureRequestID } from './request-id.js';
import { requireReconciliationDate, requireReconciliationRun } from './reconciliation-contract.js';
function errorFromResponse(response, payload) {
  const error = new Error(payload?.message || `HTTP ${response.status}`);
  error.code = payload?.code || 'REQUEST_FAILED';
  error.status = response.status;
  error.details = payload?.details || null;
  error.requestID = payload?.request_id || '';
  const retryAfter = String(response.headers.get('Retry-After') || '').trim();
  const retryAfterNumber = Number.parseInt(retryAfter, 10);
  const retryAfterDate = Number.isNaN(retryAfterNumber) ? Date.parse(retryAfter) : Number.NaN;
  const retryAfterSeconds = Number.isNaN(retryAfterNumber)
    ? Math.ceil((retryAfterDate - Date.now()) / 1000)
    : retryAfterNumber;
  error.retryAfterSeconds = Number.isFinite(retryAfterSeconds) && retryAfterSeconds > 0
    ? Math.min(retryAfterSeconds, 86_400)
    : 0;
  return error;
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

  async request(area, resource, signal) {
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
        const payload = area === 'billing' && response.ok ? parseFinanceJSON(await response.text()) : await response.json().catch(() => ({}));
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

  async mutate(area, resource, method, body, idempotencyKey = '', signal, expectedStatus = 0) {
    const headers = {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      'X-CSRF-Token': this.csrfToken,
    };
    if (idempotencyKey) headers['Idempotency-Key'] = idempotencyKey;
    const response = await fetch(`${this.baseURL}/api/admin/${area}${resource}`, {
      method,
      headers,
      credentials: 'same-origin',
      cache: 'no-store',
      body: body === undefined ? undefined : area === 'billing' ? encodeFinanceJSON(body) : JSON.stringify(body),
      signal,
    });
    this.observeAuthorizationVersions(response.headers);
    const payload = response.status === 204 ? null : area === 'billing' && response.ok ? parseFinanceJSON(await response.text()) : await response.json().catch(() => ({}));
    if (!response.ok) throw errorFromResponse(response, payload);
    if (expectedStatus && response.status !== expectedStatus) throw new Error('INVALID_MUTATION_ACK');
    return payload;
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
    return this.request('account', `/admin/v1/approval-requests?${query}`, signal);
  }
  approvalRequest(requestID, signal) {
    return this.request('account', `/admin/v1/approval-requests/${encodeURIComponent(requestID)}`, signal);
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
      const payload = response.ok ? parseFinanceJSON(await response.text()) : await response.json().catch(() => ({}));
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
}
