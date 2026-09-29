import { secureRequestID } from './request-id.js';
import { mergeObservedVersions } from './authorization-ui.js';

const ADMIN_BILLING_LIMITS = new Set(['25', '50', '100', '200']);
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

function billingCollectionQuery(filters = {}, { keys = [], dateTimeKeys = [], dateKeys = [] } = {}) {
  const query = new URLSearchParams({ limit: ADMIN_BILLING_LIMITS.has(String(filters.limit)) ? String(filters.limit) : '50' });
  if (filters.cursor) query.set('cursor', filters.cursor);
  for (const key of keys) if (filters[key]) query.set(key, filters[key]);
  for (const key of dateTimeKeys) if (filters[key]) query.set(key, new Date(filters[key]).toISOString());
  for (const key of dateKeys) if (filters[key]) query.set(key, filters[key]);
  if (filters.overdue) query.set('overdue', 'true');
  return query;
}

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

function approvalControlChanged() {
  const error = new Error('APPROVAL_ACTION_CONTROL_CHANGED');
  error.code = 'APPROVAL_ACTION_CONTROL_CHANGED';
  error.status = 409;
  return error;
}

function approvalControlKey(payload) {
  if (!payload?.control) return '';
  const { mode, approval_wire_version, proof_required, epoch,
    authorization_relay_state, authorization_relay_epoch, version } = payload.control;
  return JSON.stringify([payload.session_approval_wire_version, mode, approval_wire_version,
    proof_required, epoch, authorization_relay_state, authorization_relay_epoch, version]);
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

export class AdminPlatformClient {
  constructor(baseURL = '') {
    this.baseURL = String(baseURL || '').replace(/\/$/, '');
    this.csrfToken = '';
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
    if (!response.ok) return { authenticated: false };
    const payload = await response.json();
    this.csrfToken = payload.csrf_token || '';
    return payload;
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
        const payload = await response.json().catch(() => ({}));
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

  async mutate(area, resource, method, body, idempotencyKey = '', signal) {
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
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
    this.observeAuthorizationVersions(response.headers);
    const payload = response.status === 204 ? {} : await response.json().catch(() => ({}));
    if (!response.ok) throw errorFromResponse(response, payload);
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

  accountOverview(signal) {
    return this.request('account', '/admin/v1/overview', signal);
  }
  smsConfiguration(signal) {
    return this.request('account', '/admin/v1/sms-configuration', signal);
  }
  updateSMSConfiguration(input, idempotencyKey = `sms-configuration-${secureRequestID()}`, signal) {
    return this.mutate('account', '/admin/v1/sms-configuration', 'PUT', input, idempotencyKey, signal);
  }
  roles(signal) {
    return this.request('account', '/admin/v1/roles', signal);
  }
  roleBindings(filters = {}, signal) {
    const query = new URLSearchParams();
    if (filters.user_id) query.set('user_id', filters.user_id);
    if (filters.organization_id) query.set('organization_id', filters.organization_id);
    const suffix = query.size ? `?${query}` : '';
    return this.request('account', `/admin/v1/role-bindings${suffix}`, signal);
  }
  grantRoleBinding(input) {
    return this.mutate('account', '/admin/v1/role-bindings', 'POST', input);
  }
  revokeRoleBinding(bindingID) {
    return this.mutate('account', `/admin/v1/role-bindings/${encodeURIComponent(bindingID)}`, 'DELETE');
  }
  users(searchOrFilters = '', signal, cursor = '') {
    const filters = typeof searchOrFilters === 'string' ? { q:searchOrFilters, cursor } : (searchOrFilters || {});
    const query = new URLSearchParams({ limit: '25' });
    if (filters.q) query.set('q', filters.q);
    if (filters.cursor) query.set('cursor', filters.cursor);
    if (filters.exclude_organization_id) query.set('exclude_organization_id', filters.exclude_organization_id);
    return this.request('account', `/admin/v1/users?${query}`, signal);
  }
  userByCode(userCode, signal) {
    return this.request('account', `/admin/v1/user-code-lookups/${encodeURIComponent(userCode)}`, signal);
  }
  userMemberships(userID, signal) {
    return this.request('account', `/admin/v1/users/${encodeURIComponent(userID)}/memberships`, signal);
  }
  setUserStatus(userID, status) {
    return this.mutate('account', `/admin/v1/users/${encodeURIComponent(userID)}/status`, 'PATCH', { status });
  }
  accountClosures(signal) {
    return this.request('account', '/admin/v1/account-closure-requests', signal);
  }
  retryAccountClosure(closureRequestID, reason) {
    return this.mutate('account', `/admin/v1/account-closure-requests/${encodeURIComponent(closureRequestID)}/actions/retry`, 'POST', { reason }, `account-closure-retry-${secureRequestID()}`);
  }
  registrationRepairs(filters = {}, signal) {
    const query = new URLSearchParams({ limit: '100' });
    for (const key of ['status', 'stage', 'responsibility', 'sla', 'older_than_minutes']) if (filters[key]) query.set(key, filters[key]);
    return this.request('account', `/admin/v1/registration-operations?${query}`, signal);
  }
  retryRegistration(operationID) {
    return this.mutate('account', `/admin/v1/registration-operations/${encodeURIComponent(operationID)}/actions/retry`, 'POST');
  }
  compensateRegistration(operationID) {
    return this.mutate('account', `/admin/v1/registration-operations/${encodeURIComponent(operationID)}/actions/compensate`, 'POST');
  }
  profileChangeRepairs(signal) {
    return this.request('account', '/admin/v1/profile-change-requests', signal);
  }
  retryProfileChange(requestID) {
    return this.mutate('account', `/admin/v1/profile-change-requests/${encodeURIComponent(requestID)}/actions/retry`, 'POST');
  }
  organizations(searchOrFilters = {}, signal) {
    const filters = typeof searchOrFilters === 'string' ? { q: searchOrFilters } : searchOrFilters || {};
    const query = new URLSearchParams({ limit: '25' });
    if (filters.q) query.set('q', filters.q);
    if (filters.status) query.set('status', filters.status);
    if (filters.cursor) query.set('cursor', filters.cursor);
    return this.request('account', `/admin/v1/organizations?${query}`, signal);
  }
  createOrganization(name, ownerUserID) {
    return this.mutate('account', '/admin/v1/organizations', 'POST', {
      name,
      owner_user_id: ownerUserID,
    });
  }
  renameOrganization(organizationID, name) {
    return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}`, 'PATCH', { name });
  }
  setOrganizationStatus(organizationID, status) {
    return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/status`, 'PATCH', { status });
  }
  organizationDeletionReadiness(organizationID, signal) {
    return this.request('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/deletion-readiness`, signal);
  }
  deleteOrganization(organizationID, reason) {
    return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}`, 'DELETE', { reason });
  }
  organizationMembers(organizationID, filtersOrSignal = {}, signal) {
    const filters = filtersOrSignal instanceof globalThis.AbortSignal ? {} : (filtersOrSignal || {});
    const requestSignal = filtersOrSignal instanceof globalThis.AbortSignal ? filtersOrSignal : signal;
    const params = new URLSearchParams({ limit:'25' });
    if (filters.q) params.set('q', filters.q);
    if (filters.role) params.set('role', filters.role);
    if (filters.cursor) params.set('cursor', filters.cursor);
    return this.request('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members?${params}`, requestSignal);
  }
  addOrganizationMember(organizationID, userID, roles, unitAdminUnitIDs) {
    return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members`, 'POST', { user_id: userID, roles, unit_admin_unit_ids: unitAdminUnitIDs });
  }
  replaceOrganizationMemberRoles(organizationID, userID, roles, unitAdminUnitIDs) {
    return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members/${encodeURIComponent(userID)}/roles`, 'PUT', { roles, unit_admin_unit_ids: unitAdminUnitIDs });
  }
  removeOrganizationMember(organizationID, userID) {
    return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members/${encodeURIComponent(userID)}`, 'DELETE');
  }
  transferOrganizationOwnership(organizationID, userID) {
    return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/ownership`, 'POST', { new_owner_user_id: userID });
  }
  organizationUnits(organizationID, signal) {
    return this.request('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units`, signal);
  }
  createOrganizationUnit(organizationID, name, parentID = null) {
    return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units`, 'POST', { name, parent_id: parentID });
  }
  renameOrganizationUnit(organizationID, unitID, name) {
    return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units/${encodeURIComponent(unitID)}`, 'PATCH', { name });
  }
  moveOrganizationUnit(organizationID, unitID, parentID, expectedStructureVersion) {
    return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units/${encodeURIComponent(unitID)}/parent`, 'PATCH', {
      parent_id: parentID || null,
      expected_structure_version: expectedStructureVersion,
    });
  }
  deleteOrganizationUnit(organizationID, unitID) {
    return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units/${encodeURIComponent(unitID)}`, 'DELETE');
  }
  audits(filters = {}, signal) {
    const query = new URLSearchParams({ limit: '200' });
    for (const key of ['actor', 'action', 'resource', 'result', 'from', 'to']) if (filters[key]) query.set(key, filters[key]);
    return this.request('account', `/admin/v1/audit-logs?${query}`, signal);
  }
  audit(auditID, signal) {
    return this.request('account', `/admin/v1/audit-logs/${encodeURIComponent(auditID)}`, signal);
  }
  createAuditExport(filter, reason, includePII = false, idempotencyKey) {
    if (!idempotencyKey) throw new Error('audit export idempotency key is required');
    return this.mutate('account', '/admin/v1/audit-exports', 'POST', {
      filter,
      reason,
      include_pii: includePII,
      idempotency_key: idempotencyKey,
    });
  }
  auditExport(exportID, signal) {
    return this.request('account', `/admin/v1/audit-exports/${encodeURIComponent(exportID)}`, signal);
  }
  issueAuditExportDownloadTicket(exportID) {
    return this.mutate('account', `/admin/v1/audit-exports/${encodeURIComponent(exportID)}/download-tickets`, 'POST', {});
  }
  async downloadAuditExport(exportID, token) {
    const response = await fetch(`${this.baseURL}/api/admin/account/admin/v1/audit-exports/${encodeURIComponent(exportID)}/download`, {
      headers: {
        Accept: 'application/vnd.pinvou.audit-envelope+json',
        'X-Pinvou-Download-Token': token,
      },
      credentials: 'same-origin',
      cache: 'no-store',
    });
    if (!response.ok) throw errorFromResponse(response, await response.json().catch(() => ({})));
    return response.blob();
  }
  billingOverview(signal) {
    return this.request('billing', '/admin/v1/overview', signal);
  }
  financeAccountsOverview(signal) {
    return this.request('billing', '/admin/v1/finance-overviews/accounts', signal);
  }
  financePaymentsOverview(window = {}, signal) {
    const query = new URLSearchParams();
    if (window.from) query.set('from', window.from);
    if (window.to) query.set('to', window.to);
    const suffix = query.size ? `?${query}` : '';
    return this.request('billing', `/admin/v1/finance-overviews/payments${suffix}`, signal);
  }
  financePricingOverview(signal) {
    return this.request('billing', '/admin/v1/finance-overviews/pricing', signal);
  }
  financeProductsOverview(signal) {
    return this.request('billing', '/admin/v1/finance-overviews/products', signal);
  }
  wallets(filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['owner_type', 'status', 'q'] });
    return this.request('billing', `/admin/v1/wallets?${query}`, signal);
  }
  walletLedger(walletID, filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['entry_type', 'business_reference'], dateTimeKeys: ['created_from', 'created_to'] });
    return this.request('billing', `/admin/v1/wallets/${encodeURIComponent(walletID)}/ledger?${query}`, signal);
  }
  receivables(filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['wallet_id', 'status', 'source_type'], dateTimeKeys: ['created_from', 'created_to'] });
    return this.request('billing', `/admin/v1/receivables?${query}`, signal);
  }
  receivableAllocations(receivableID, signal) {
    return this.request('billing', `/admin/v1/receivables/${encodeURIComponent(receivableID)}/allocations`, signal);
  }
  priceVersions(signal) {
    return this.request('billing', '/admin/v1/pricing-versions?limit=100', signal);
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
  createMeteringUnit(input) {
    return this.mutate('billing', '/admin/v1/metering-units', 'POST', input);
  }
  deactivateMeteringUnit(unitDefinitionID) {
    return this.mutate('billing', `/admin/v1/metering-units/${encodeURIComponent(unitDefinitionID)}/deactivate`, 'POST', {});
  }
  createPriceDraft(input) {
    return this.mutate('billing', '/admin/v1/pricing/drafts', 'POST', input);
  }
  updatePriceDraft(priceVersionID, input) {
    return this.mutate('billing', `/admin/v1/pricing/${encodeURIComponent(priceVersionID)}/draft`, 'PUT', input);
  }
  previewPrice(priceVersionID, signal) {
    return this.request('billing', `/admin/v1/pricing/${encodeURIComponent(priceVersionID)}/preview`, signal);
  }
  publishPrice(priceVersionID, expectedETag, idempotencyKey) {
    return this.mutate('billing', `/admin/v1/pricing/${encodeURIComponent(priceVersionID)}/publish`, 'POST', { expected_etag: expectedETag }, idempotencyKey);
  }
  transitionPrice(priceVersionID, input, idempotencyKey) {
    return this.mutate('billing', `/admin/v1/pricing/${encodeURIComponent(priceVersionID)}/transition`, 'POST', input, idempotencyKey);
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
    return this.mutate('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/sync`, 'POST', {});
  }
  repairPaymentOrder(orderID, reason) {
    return this.mutate('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/repair`, 'POST', { reason });
  }
  refundPaymentOrder(orderID, transactionID, packageID, reason, idempotencyKey) {
    return this.mutate(
      'billing',
      `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/refunds`,
      'POST',
      {
        payment_transaction_id: transactionID,
        entitlement_package_id: packageID,
        reason,
      },
      idempotencyKey,
    );
  }
  adjustments(filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['wallet_id', 'status', 'requested_by'], dateTimeKeys: ['created_from', 'created_to'] });
    return this.request('billing', `/admin/v1/adjustments?${query}`, signal);
  }
  createAdjustment(input) {
    return this.mutate('billing', '/admin/v1/adjustments', 'POST', input);
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
      const payload = await response.json().catch(() => ({}));
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
  async approvalCommandPrecheck(payload, signal) {
    const response = await fetch(`${this.baseURL}/api/admin/approvals/prechecks`, {
      method: 'POST',
      headers: { Accept: 'application/json', 'Content-Type': 'application/json', 'X-CSRF-Token': this.csrfToken },
      credentials: 'same-origin',
      cache: 'no-store',
      body: JSON.stringify(payload),
      signal,
    });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw errorFromResponse(response, body);
    return body;
  }
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
      const payload = await response.json().catch(() => ({}));
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
        credentials: 'same-origin', cache: 'no-store', body: JSON.stringify(body), signal: controller.signal,
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
        credentials: 'same-origin', cache: 'no-store', body: JSON.stringify(body), signal: controller.signal,
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
        if (response.status !== 201 || typeof payload.approval_request_id !== 'string' || !payload.approval_request_id) throw approvalControlChanged();
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
  holds(filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['wallet_id', 'status', 'model_id'], dateTimeKeys: ['created_from', 'created_to'] });
    return this.request('billing', `/admin/v1/usage-holds?${query}`, signal);
  }
  hold(holdID, signal) {
    return this.request('billing', `/admin/v1/usage-holds/${encodeURIComponent(holdID)}`, signal);
  }
  releaseHold(holdID, reason, idempotencyKey) {
    return this.mutate('billing', `/admin/v1/usage-holds/${encodeURIComponent(holdID)}/release`, 'POST', { reason }, idempotencyKey);
  }
  reconciliationRuns(filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['provider_id', 'channel', 'status'], dateKeys: ['business_date_from', 'business_date_to'] });
    return this.request('billing', `/admin/v1/reconciliation-runs?${query}`, signal);
  }
  runReconciliation(channel, businessDate) {
    return this.mutate('billing', '/admin/v1/reconciliation-runs', 'POST', {
      channel,
      business_date: businessDate,
    });
  }
  importProviderBill(channel, businessDate, items) {
    return this.mutate('billing', '/admin/v1/reconciliation-imports', 'POST', {
      channel,
      business_date: businessDate,
      items,
    });
  }
  reconciliationCases(runID, filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['status', 'severity', 'owner_id', 'case_type'], dateTimeKeys: ['created_from', 'created_to'] });
    return this.request('billing', `/admin/v1/reconciliation-runs/${encodeURIComponent(runID)}/cases?${query}`, signal);
  }
  updateReconciliationCase(caseID, command, idempotencyKey = `reconciliation-case-${secureRequestID()}`) {
    return this.mutate('billing', `/admin/v1/reconciliation-cases/${encodeURIComponent(caseID)}/commands`, 'POST', command, idempotencyKey);
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
    return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/claim`, 'POST', {});
  }
  queryLatePaymentReview(caseID) {
    return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/query`, 'POST', {});
  }
  scheduleLatePaymentReview(caseID, nextReviewAt, reason) {
    return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/schedule`, 'POST', { next_review_at: new Date(nextReviewAt).toISOString(), reason });
  }
  archiveLatePaymentReview(caseID) {
    return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/archive`, 'POST', {});
  }
  escalateProviderBlind(caseID, reason) {
    return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/provider-blind`, 'POST', { reason });
  }
  paymentAnomalies(filters = {}, signal) {
    const query = billingCollectionQuery(filters, { keys: ['q', 'anomaly_type', 'status', 'severity', 'owner_id', 'provider_id'], dateTimeKeys: ['created_from', 'created_to'] });
    return this.request('billing', `/admin/v1/payment-anomalies?${query}`, signal);
  }
  paymentAnomaly(caseID, signal) {
    return this.request('billing', `/admin/v1/payment-anomalies/${encodeURIComponent(caseID)}`, signal);
  }
  claimPaymentAnomaly(caseID) {
    return this.mutate('billing', `/admin/v1/payment-anomalies/${encodeURIComponent(caseID)}/claim`, 'POST', {}, `payment-anomaly-claim-${secureRequestID()}`);
  }
  resolvePaymentAnomaly(caseID, resolution, reason, idempotencyKey) {
    return this.mutate('billing', `/admin/v1/payment-anomalies/${encodeURIComponent(caseID)}/resolve`, 'POST', { resolution, reason }, idempotencyKey);
  }
  tokenOverview(signal) {
    return this.request('token', '/admin/v1/overview', signal);
  }
  tokenOperationsOverview(window = '24h', signal) {
    const query = new URLSearchParams({ window });
    return this.request('token', `/admin/v1/operations-overview?${query}`, signal);
  }
  tokenModelCatalogOverview(signal) {
    return this.request('token', '/admin/v1/overviews/model-catalog', signal);
  }
  tokenResourcesPageOverview(signal) {
    return this.request('token', '/admin/v1/overviews/token-resources', signal);
  }
  tokenResourceReadiness(signal) {
    return this.request('token', '/admin/v1/token-resources/readiness', signal);
  }
  tokenModelOperationsPageOverview(window = '', signal) {
    const suffix = window ? `?${new URLSearchParams({ window })}` : '';
    return this.request('token', `/admin/v1/overviews/model-operations${suffix}`, signal);
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
  upsertTokenRoute(tokenAccountID, route) {
    return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/routes`, 'PUT', route);
  }
  tokenAccountStatusImpact(tokenAccountID, targetStatus, signal) {
    const query = new URLSearchParams({ target_status: targetStatus });
    return this.request('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/impact/status?${query}`, signal);
  }
  stableTokenRouteImpact(tokenAccountID, input, signal) {
    const query = new URLSearchParams({ action: input.action });
    if (input.route_id) query.set('route_id', input.route_id);
    if (input.model_id) query.set('model_id', input.model_id);
    if (input.protocol_version_id) query.set('protocol_version_id', input.protocol_version_id);
    return this.request('token', `/admin/v2/token-accounts/${encodeURIComponent(tokenAccountID)}/impact/route?${query}`, signal);
  }
  tokenCredentialImpact(tokenAccountID, action, credentialID, signal) {
    const query = new URLSearchParams({ action });
    if (credentialID) query.set('credential_id', credentialID);
    return this.request('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/impact/credential?${query}`, signal);
  }
  deleteTokenRoute(tokenAccountID, model) {
    return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/routes`, 'DELETE', { model });
  }
  validateTokenCredential(tokenAccountID, credentialID) {
    return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/credentials/${encodeURIComponent(credentialID)}/validate`, 'POST', {});
  }
  usageRequests(filtersOrSignal = {}, signal) {
    if (isAbortSignal(filtersOrSignal)) {
      return this.request('token', '/admin/v1/usage-requests?limit=100', filtersOrSignal);
    }
    const filters = filtersOrSignal;
    const requestSignal = signal;
    return this.request('token', `/admin/v1/usage-requests?${tokenAdminCollectionQuery('usage', filters)}`, requestSignal);
  }
  usageRequest(usageRequestID, signal) {
    return this.request('token', `/admin/v1/usage-requests/${encodeURIComponent(usageRequestID)}`, signal);
  }
  usageReconciliationCommandReadiness(signal) {
    return this.request('token', '/admin/v1/commands/TOKEN_USAGE_RECONCILIATION_RETRY/readiness', signal);
  }
  usageEvidence(usageRequestID, filters = {}, signal) {
    return this.request('token', `/admin/v1/usage-requests/${encodeURIComponent(usageRequestID)}/evidence?${tokenAdminCollectionQuery('usageEvidence', filters)}`, signal);
  }
  retryUsageReconciliation(usageRequestID, reason, idempotencyKey, signal) {
    return this.mutate('token', `/admin/v1/usage-requests/${encodeURIComponent(usageRequestID)}/retry-reconciliation`, 'POST', { reason }, idempotencyKey, signal);
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
  createProtocolDraft(input, idempotencyKey, signal) {
    return this.mutate('token', '/admin/v1/protocols', 'POST', input, idempotencyKey, signal);
  }
  publishProtocol(protocolDefinitionID, input, idempotencyKey, signal) {
    return this.mutate('token', `/admin/v1/protocols/${encodeURIComponent(protocolDefinitionID)}/publish`, 'POST', input, idempotencyKey, signal);
  }
  createFormulaDraft(protocolDefinitionID, input, idempotencyKey, signal) {
    return this.mutate('token', `/admin/v1/protocols/${encodeURIComponent(protocolDefinitionID)}/formulas`, 'POST', input, idempotencyKey, signal);
  }
  previewFormula(formulaVersionID, signal) {
    return this.request('token', `/admin/v1/formulas/${encodeURIComponent(formulaVersionID)}/preview`, signal);
  }
  publishFormula(formulaVersionID, input, idempotencyKey, signal) {
    return this.mutate('token', `/admin/v1/formulas/${encodeURIComponent(formulaVersionID)}/publish`, 'POST', input, idempotencyKey, signal);
  }
  tokenReconciliationCases(filters = {}, signal) {
    return this.request('token', `/admin/v1/reconciliation-cases?${tokenAdminCollectionQuery('reconciliation', filters)}`, signal);
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
  saveModelDraft(modelID, input) {
    return this.mutate('token', `/admin/v1/models/${encodeURIComponent(modelID)}/draft`, 'PUT', input);
  }
  publishModelDraft(modelID, input) {
    return this.mutate('token', `/admin/v1/models/${encodeURIComponent(modelID)}/publish`, 'POST', input);
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
  transitionModel(modelID, input) {
    return this.mutate('token', `/admin/v1/models/${encodeURIComponent(modelID)}/lifecycle`, 'POST', input);
  }
  syncModelCatalog() {
    return this.mutate('token', '/admin/v1/models:sync', 'POST');
  }
  rollbackModelCatalog(input) {
    return this.mutate('token', '/admin/v1/model-catalog:rollback', 'POST', input);
  }
  provisionings(filtersOrSignal = {}, signal) {
    if (isAbortSignal(filtersOrSignal)) {
      return this.request('token', '/admin/v1/provisionings?limit=100', filtersOrSignal);
    }
    const filters = filtersOrSignal;
    const requestSignal = signal;
    return this.request('token', `/admin/v1/provisionings?${tokenAdminCollectionQuery('provisionings', filters)}`, requestSignal);
  }
  provisioning(provisioningID, signal) {
    return this.request('token', `/admin/v2/provisionings/${encodeURIComponent(provisioningID)}`, signal);
  }
  provisioningAttempts(provisioningID, filters = {}, signal) {
    return this.request('token', `/admin/v1/provisionings/${encodeURIComponent(provisioningID)}/attempts?${tokenAdminCollectionQuery('provisioningAttempts', filters)}`, signal);
  }
  repairProvisioning(provisioningID, reason, idempotencyKey, signal) {
    return this.mutate('token', `/admin/v1/provisionings/${encodeURIComponent(provisioningID)}/repair`, 'POST', { reason }, idempotencyKey, signal);
  }
  systemStatus(signal) {
    return this.request('system', '/status', signal);
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
        return null;
      }
    }
    if (!response.ok) throw errorFromResponse(response, payload);
    this.csrfToken = '';
    return payload;
  }
}
