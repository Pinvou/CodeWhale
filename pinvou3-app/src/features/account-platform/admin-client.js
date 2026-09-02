import { mergeObservedVersions } from './authorization-ui.js';

const ADMIN_BILLING_LIMITS = new Set(['25', '50', '100', '200']);

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
  return error;
}

export class AdminPlatformClient {
  constructor(baseURL = '') {
    this.baseURL = String(baseURL || '').replace(/\/$/, '');
    this.csrfToken = '';
    this.authorizationVersions = {};
    this.authorizationVersionListeners = new Set();
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
    const response = await fetch(`${this.baseURL}/api/admin/${area}${resource}`, {
      headers: { Accept: 'application/json' },
      credentials: 'same-origin',
      cache: 'no-store',
      signal,
    });
    this.observeAuthorizationVersions(response.headers);
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw errorFromResponse(response, payload);
    return payload;
  }

  async mutate(area, resource, method, body, idempotencyKey = '') {
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
    return this.mutate('account', `/admin/v1/account-closure-requests/${encodeURIComponent(closureRequestID)}/actions/retry`, 'POST', { reason }, `account-closure-retry-${crypto.randomUUID()}`);
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
  createAuditExport(filter, reason, includePII = false) {
    return this.mutate('account', '/admin/v1/audit-exports', 'POST', {
      filter,
      reason,
      include_pii: includePII,
      idempotency_key: `audit-export-${crypto.randomUUID()}`,
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
  createApprovalDraft(input) {
    return this.mutate('account', '/admin/v1/approval-requests', 'POST', input);
  }
  submitApprovalRequest(requestID, expectedVersion) {
    return this.mutate('account', `/admin/v1/approval-requests/${encodeURIComponent(requestID)}/actions/submit`, 'POST', { expected_version: expectedVersion });
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
  updateReconciliationCase(caseID, command, idempotencyKey = `reconciliation-case-${crypto.randomUUID()}`) {
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
    return this.mutate('billing', `/admin/v1/payment-anomalies/${encodeURIComponent(caseID)}/claim`, 'POST', {}, `payment-anomaly-claim-${crypto.randomUUID()}`);
  }
  resolvePaymentAnomaly(caseID, resolution, reason, idempotencyKey) {
    return this.mutate('billing', `/admin/v1/payment-anomalies/${encodeURIComponent(caseID)}/resolve`, 'POST', { resolution, reason }, idempotencyKey);
  }
  tokenOverview(signal) {
    return this.request('token', '/admin/v1/overview', signal);
  }
  tokenAccounts(signal) {
    return this.request('token', '/admin/v1/token-accounts?limit=100', signal);
  }
  createTokenAccount(input) {
    return this.mutate('token', '/admin/v1/token-accounts', 'POST', input);
  }
  tokenAccount(tokenAccountID, signal) {
    return this.request('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}`, signal);
  }
  setTokenAccountStatus(tokenAccountID, status, reason) {
    return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/status`, 'PATCH', { status, reason });
  }
  upsertTokenRoute(tokenAccountID, route) {
    return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/routes`, 'PUT', route);
  }
  deleteTokenRoute(tokenAccountID, model) {
    return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/routes`, 'DELETE', { model });
  }
  validateTokenCredential(tokenAccountID, credentialID) {
    return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/credentials/${encodeURIComponent(credentialID)}/validate`, 'POST', {});
  }
  usageRequests(signal) {
    return this.request('token', '/admin/v1/usage-requests?limit=100', signal);
  }
  usageRequest(usageRequestID, signal) {
    return this.request('token', `/admin/v1/usage-requests/${encodeURIComponent(usageRequestID)}`, signal);
  }
  retryUsageReconciliation(usageRequestID, reason) {
    return this.mutate('token', `/admin/v1/usage-requests/${encodeURIComponent(usageRequestID)}/retry-reconciliation`, 'POST', { reason }, `usage-reconcile-${crypto.randomUUID()}`);
  }
  catalogModels(signal) {
    return this.request('token', '/admin/v1/models', signal);
  }
  saveModelDraft(modelID, input) {
    return this.mutate('token', `/admin/v1/models/${encodeURIComponent(modelID)}/draft`, 'PUT', input);
  }
  publishModelDraft(modelID, input) {
    return this.mutate('token', `/admin/v1/models/${encodeURIComponent(modelID)}/publish`, 'POST', input);
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
  provisionings(signal) {
    return this.request('token', '/admin/v1/provisionings?limit=100', signal);
  }
  provisioning(provisioningID, signal) {
    return this.request('token', `/admin/v1/provisionings/${encodeURIComponent(provisioningID)}`, signal);
  }
  repairProvisioning(provisioningID, reason) {
    return this.mutate('token', `/admin/v1/provisionings/${encodeURIComponent(provisioningID)}/repair`, 'POST', { reason }, `provisioning-repair-${crypto.randomUUID()}`);
  }
  systemStatus(signal) {
    return this.request('system', '/status', signal);
  }

  logout() {
    return fetch(`${this.baseURL}/api/logout`, {
      method: 'POST',
      headers: { 'X-CSRF-Token': this.csrfToken },
      credentials: 'same-origin',
    });
  }
}
