import { performLogicalOperation } from './logical-operation.js';
import { requireAdminRecord, requireAdminFacts, requirePriceDraftAcknowledgement } from './admin-acknowledgements.js';

export function newRequestUUID() {
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('');
  return [hex.slice(0, 8), hex.slice(8, 12), hex.slice(12, 16), hex.slice(16, 20), hex.slice(20)].join('-');
}

function errorFromResponse(response, payload) {
  const error = new Error(payload?.message || `HTTP ${response.status}`);
  error.code = payload?.code || 'REQUEST_FAILED';
  error.status = response.status;
  return error;
}

async function responsePayload(response) {
  if (response.status === 204) return {};
  try {
    const payload = await response.json();
    if (response.ok && (!payload || typeof payload !== 'object' || Array.isArray(payload))) {
      throw new Error('invalid response');
    }
    return payload;
  } catch {
    if (!response.ok) return {};
    throw errorFromResponse(response, { code: 'REQUEST_FAILED' });
  }
}

export class AdminPlatformClient {
  constructor(baseURL = '') {
    this.baseURL = String(baseURL || '').replace(/\/$/, '');
    this.csrfToken = '';
    this.pendingRefunds = new Map();
    this.pendingOperations = new Map();
  }

  async session(signal) {
    const response = await fetch(`${this.baseURL}/api/session`, { credentials: 'same-origin', cache: 'no-store', signal });
    if (response.status === 401) { this.csrfToken = ''; return { authenticated: false }; }
    if (!response.ok) throw errorFromResponse(response, await response.json().catch(() => ({})));
    const payload = await responsePayload(response);
    if (response.status !== 200 || payload.authenticated !== true || typeof payload.csrf_token !== 'string' || !payload.csrf_token) {
      throw errorFromResponse(response, { code: 'REQUEST_FAILED' });
    }
    this.csrfToken = payload.csrf_token;
    return payload;
  }

  async request(area, resource, signal) {
    const response = await fetch(`${this.baseURL}/api/admin/${area}${resource}`, {
      headers: { Accept: 'application/json' },
      credentials: 'same-origin',
      cache: 'no-store',
      signal,
    });
    const payload = await responsePayload(response);
    if (!response.ok) {
      if (response.status === 401) this.csrfToken = '';
      throw errorFromResponse(response, payload);
    }
    return payload;
  }

  async mutate(area, resource, method, body, idempotencyKey) {
    const response = await fetch(`${this.baseURL}/api/admin/${area}${resource}`, {
      method,
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
        'X-CSRF-Token': this.csrfToken,
        ...(idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : {}),
      },
      credentials: 'same-origin',
      cache: 'no-store',
      // oxlint-disable-next-line unicorn/no-invalid-fetch-options -- Callers supply POST/PUT/PATCH/DELETE; the rule treats a dynamic method as GET.
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const payload = await responsePayload(response);
    if (!response.ok) {
      if (response.status === 401) this.csrfToken = '';
      throw errorFromResponse(response, payload);
    }
    return payload;
  }

  logicalMutation(area, resource, body, validate) {
    const identity = JSON.stringify([this.csrfToken, area, resource, body]);
    return performLogicalOperation(this.pendingOperations, {
      kind:`${area}:${resource}`, identity, createKey:newRequestUUID,
      action:async key => validate(await this.mutate(area, resource, 'POST', body, key)),
    });
  }

  accountOverview(signal) { return this.request('account', '/admin/v1/overview', signal); }
  roles(signal) { return this.request('account', '/admin/v1/roles', signal); }
  users(search, signal) { return this.request('account', `/admin/v1/users?limit=100&q=${encodeURIComponent(search || '')}`, signal); }
  userByCode(userCode, signal) { return this.request('account', `/admin/v1/user-code-lookups/${encodeURIComponent(userCode)}`, signal); }
  userMemberships(userID, signal) { return this.request('account', `/admin/v1/users/${encodeURIComponent(userID)}/memberships`, signal); }
  setUserStatus(userID, status) { return this.mutate('account', `/admin/v1/users/${encodeURIComponent(userID)}/status`, 'PATCH', { status }); }
  deleteUser(userID) { return this.mutate('account', `/admin/v1/users/${encodeURIComponent(userID)}`, 'DELETE'); }
  organizations(search, signal) { return this.request('account', `/admin/v1/organizations?limit=100&q=${encodeURIComponent(search || '')}`, signal); }
  createOrganization(name, ownerUserID) { return this.mutate('account', '/admin/v1/organizations', 'POST', { name, owner_user_id: ownerUserID }); }
  renameOrganization(organizationID, name) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}`, 'PATCH', { name }); }
  setOrganizationStatus(organizationID, status) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/status`, 'PATCH', { status }); }
  deleteOrganization(organizationID) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}`, 'DELETE'); }
  organizationMembers(organizationID, signal) { return this.request('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members`, signal); }
  addOrganizationMember(organizationID, userID, roles, unitAdminUnitIDs) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members`, 'POST', { user_id:userID, roles, unit_admin_unit_ids:unitAdminUnitIDs }); }
  replaceOrganizationMemberRoles(organizationID, userID, roles, unitAdminUnitIDs) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members/${encodeURIComponent(userID)}/roles`, 'PUT', { roles, unit_admin_unit_ids:unitAdminUnitIDs }); }
  removeOrganizationMember(organizationID, userID) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/members/${encodeURIComponent(userID)}`, 'DELETE'); }
  transferOrganizationOwnership(organizationID, userID) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/ownership`, 'POST', { new_owner_user_id:userID }); }
  organizationUnits(organizationID, signal) { return this.request('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units`, signal); }
  createOrganizationUnit(organizationID, name, parentID = null) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units`, 'POST', { name, parent_id: parentID }); }
  renameOrganizationUnit(organizationID, unitID, name) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units/${encodeURIComponent(unitID)}`, 'PATCH', { name }); }
  deleteOrganizationUnit(organizationID, unitID) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units/${encodeURIComponent(unitID)}`, 'DELETE'); }
  audits(filters = {}, signal) {
    const query = new URLSearchParams({ limit:'200' });
    for (const key of ['actor','action','resource','result','from','to']) if (filters[key]) query.set(key, filters[key]);
    return this.request('account', `/admin/v1/audit-logs?${query}`, signal);
  }
  audit(auditID, signal) { return this.request('account', `/admin/v1/audit-logs/${encodeURIComponent(auditID)}`, signal); }
  billingOverview(signal) { return this.request('billing', '/admin/v1/overview', signal); }
  wallets(signal) { return this.request('billing', '/admin/v1/wallets?limit=100', signal); }
  walletLedger(walletID, signal) { return this.request('billing', `/admin/v1/wallets/${encodeURIComponent(walletID)}/ledger?limit=200`, signal); }
  receivables(signal) { return this.request('billing', '/admin/v1/receivables?limit=100', signal); }
  receivableAllocations(receivableID, signal) { return this.request('billing', `/admin/v1/receivables/${encodeURIComponent(receivableID)}/allocations`, signal); }
  priceVersions(signal) { return this.request('billing', '/admin/v1/pricing-versions?limit=100', signal); }
  meteringUnits(signal) { return this.request('billing', '/admin/v1/metering-units', signal); }
  createMeteringUnit(input) { return this.logicalMutation('billing', '/admin/v1/metering-units', input, value => requireAdminFacts(requireAdminRecord(value, 'unit_definition_id'), { ...input, status:'ACTIVE' })); }
  deactivateMeteringUnit(unitID) { return this.mutate('billing', `/admin/v1/metering-units/${encodeURIComponent(unitID)}/deactivate`, 'POST', {}); }
  managedPrices(signal) { return this.request('billing', '/admin/v1/pricing?limit=100', signal); }
  createPriceDraft(input) { return this.logicalMutation('billing', '/admin/v1/pricing/drafts', input, value => requirePriceDraftAcknowledgement(value, input)); }
  async updatePriceDraft(priceID, input) { return requirePriceDraftAcknowledgement(await this.mutate('billing', `/admin/v1/pricing/${encodeURIComponent(priceID)}/draft`, 'PUT', input), input, priceID); }
  previewPrice(priceID, signal) { return this.request('billing', `/admin/v1/pricing/${encodeURIComponent(priceID)}/preview`, signal); }
  publishPrice(priceID, expectedETag) { return this.logicalMutation('billing', `/admin/v1/pricing/${encodeURIComponent(priceID)}/publish`, { expected_etag:expectedETag }, value => requireAdminFacts(requireAdminRecord(value, 'price_version_id', priceID), { lifecycle_status:'PUBLISHED' })); }
  transitionPrice(priceID, input) { return this.logicalMutation('billing', `/admin/v1/pricing/${encodeURIComponent(priceID)}/transition`, input, value => requireAdminFacts(requireAdminRecord(value, 'price_version_id', priceID), { lifecycle_status:input.action === 'CANCEL' ? 'CANCELLED' : 'ENDED', ...(input.mode === 'CONTINUATION' ? { successor_price_version_id:input.replacement_price_version_id } : {}) })); }
  protocols(signal) { return this.request('token', '/admin/v1/protocols', signal); }
  createProtocol(input) { return this.logicalMutation('token', '/admin/v1/protocols', input, value => requireAdminFacts(requireAdminRecord(value, 'protocol_definition_id'), { protocol_code:input.protocol_code, request_schema:input.request_schema, status:'DRAFT' })); }
  publishProtocol(protocolID) { return this.logicalMutation('token', `/admin/v1/protocols/${encodeURIComponent(protocolID)}/publish`, {}, value => requireAdminFacts(requireAdminRecord(value, 'protocol_definition_id', protocolID), { status:'PUBLISHED' })); }
  createFormula(protocolID, input) { return this.logicalMutation('token', `/admin/v1/protocols/${encodeURIComponent(protocolID)}/formulas`, input, value => requireAdminFacts(requireAdminRecord(value, 'formula_version_id'), { protocol_definition_id:protocolID, required_unit_versions:input.required_unit_versions, expression:input.expression, status:'DRAFT' })); }
  publishFormula(formulaID) { return this.logicalMutation('token', `/admin/v1/formulas/${encodeURIComponent(formulaID)}/publish`, {}, value => requireAdminFacts(requireAdminRecord(value, 'formula_version_id', formulaID), { status:'PUBLISHED' })); }
  previewFormula(formulaID, signal) { return this.request('token', `/admin/v1/formulas/${encodeURIComponent(formulaID)}/preview`, signal); }
  createPriceVersion(input) { return this.mutate('billing', '/admin/v1/pricing-versions', 'POST', input); }
  paymentOrders(filters = {}, signal) {
    const query = new URLSearchParams({ limit:'100' });
    if (filters.q) query.set('q', filters.q);
    if (filters.status) query.set('status', filters.status);
    return this.request('billing', `/admin/v1/payment-orders?${query}`, signal);
  }
  paymentOrder(orderID, signal) { return this.request('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}`, signal); }
  refunds(signal) { return this.request('billing', '/admin/v1/refunds?limit=100', signal); }
  syncPaymentOrder(orderID) { return this.mutate('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/sync`, 'POST', {}); }
  repairPaymentOrder(orderID, reason) { return this.mutate('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/repair`, 'POST', { reason }); }
  refundRequestKey(orderID, transactionID, packageID, reason) {
    const facts = JSON.stringify([this.csrfToken, orderID, transactionID, packageID, reason]);
    if (!this.pendingRefunds.has(facts)) this.pendingRefunds.set(facts, newRequestUUID());
    return this.pendingRefunds.get(facts);
  }

  async refundPaymentOrder(orderID, transactionID, packageID, reason, idempotencyKey) {
    if (typeof idempotencyKey !== 'string' || idempotencyKey.length < 16) throw new TypeError('refund idempotency key required');
    const result = requireAdminFacts(requireAdminRecord(await this.mutate('billing', `/admin/v1/payment-orders/${encodeURIComponent(orderID)}/refunds`, 'POST', { payment_transaction_id:transactionID, entitlement_package_id:packageID, reason }, idempotencyKey), 'refund_id'), { payment_order_id:orderID, payment_transaction_id:transactionID, entitlement_package_id:packageID, reason });
    const facts = JSON.stringify([this.csrfToken, orderID, transactionID, packageID, reason]);
    if (this.pendingRefunds.get(facts) === idempotencyKey) this.pendingRefunds.delete(facts);
    return result;
  }
  adjustments(signal) { return this.request('billing', '/admin/v1/adjustments?limit=100', signal); }
  decideAdjustment(adjustmentID, decision, reason) { return this.mutate('billing', `/admin/v1/adjustments/${encodeURIComponent(adjustmentID)}/${decision}`, 'POST', { reason }); }
  holds(signal) { return this.request('billing', '/admin/v1/usage-holds?limit=100', signal); }
  hold(holdID, signal) { return this.request('billing', `/admin/v1/usage-holds/${encodeURIComponent(holdID)}`, signal); }
  releaseHold(holdID, reason) { return this.logicalMutation('billing', `/admin/v1/usage-holds/${encodeURIComponent(holdID)}/release`, { reason }, value => requireAdminFacts(requireAdminRecord(value, 'hold_id', holdID), { status:'RELEASED', actual_microcredits:'0' })); }
  reconciliationRuns(signal) { return this.request('billing', '/admin/v1/reconciliation-runs?limit=100', signal); }
  runReconciliation(channel, businessDate) { return this.mutate('billing', '/admin/v1/reconciliation-runs', 'POST', { channel, business_date:businessDate }); }
  importProviderBill(channel, businessDate, items) { return this.mutate('billing', '/admin/v1/reconciliation-imports', 'POST', { channel, business_date:businessDate, items }); }
  reconciliationCases(runID, signal) { return this.request('billing', `/admin/v1/reconciliation-runs/${encodeURIComponent(runID)}/cases?limit=200`, signal); }
  updateReconciliationCase(caseID, command) { return this.logicalMutation('billing', `/admin/v1/reconciliation-cases/${encodeURIComponent(caseID)}/commands`, command, value => requireAdminRecord(value, 'case_id', caseID)); }
  latePaymentReviews(filters = {}, signal) {
    const query = new URLSearchParams({ limit:'100' });
    for (const key of ['status','provider_id','owner_id']) if (filters[key]) query.set(key, filters[key]);
    for (const key of ['entered_from','entered_to','next_review_before']) if (filters[key]) query.set(key, new Date(filters[key]).toISOString());
    if (filters.overdue) query.set('overdue', 'true');
    return this.request('billing', `/admin/v1/late-payment-reviews?${query}`, signal);
  }
  latePaymentReview(caseID, signal) { return this.request('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}`, signal); }
  claimLatePaymentReview(caseID) { return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/claim`, 'POST', {}); }
  queryLatePaymentReview(caseID) { return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/query`, 'POST', {}); }
  scheduleLatePaymentReview(caseID, nextReviewAt, reason) { return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/schedule`, 'POST', { next_review_at:new Date(nextReviewAt).toISOString(), reason }); }
  archiveLatePaymentReview(caseID) { return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/archive`, 'POST', {}); }
  escalateProviderBlind(caseID, reason) { return this.mutate('billing', `/admin/v1/late-payment-reviews/${encodeURIComponent(caseID)}/provider-blind`, 'POST', { reason }); }
  paymentAnomalies(filters = {}, signal) {
    const query = new URLSearchParams({ limit:'100' });
    for (const key of ['anomaly_type','status','severity','owner_id']) if (filters[key]) query.set(key, filters[key]);
    return this.request('billing', `/admin/v1/payment-anomalies?${query}`, signal);
  }
  paymentAnomaly(caseID, signal) { return this.request('billing', `/admin/v1/payment-anomalies/${encodeURIComponent(caseID)}`, signal); }
  claimPaymentAnomaly(caseID) { return this.logicalMutation('billing', `/admin/v1/payment-anomalies/${encodeURIComponent(caseID)}/claim`, {}, value => requireAdminRecord(value, 'case_id', caseID)); }
  resolvePaymentAnomaly(caseID, resolution, reason) { return this.logicalMutation('billing', `/admin/v1/payment-anomalies/${encodeURIComponent(caseID)}/resolve`, { resolution, reason }, value => requireAdminRecord(value, 'case_id', caseID)); }
  tokenOverview(signal) { return this.request('token', '/admin/v1/overview', signal); }
  tokenAccounts(signal) { return this.request('token', '/admin/v1/token-accounts?limit=100', signal); }
  tokenAccount(tokenAccountID, signal) { return this.request('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}`, signal); }
  setTokenAccountStatus(tokenAccountID, status, reason) { return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/status`, 'PATCH', { status, reason }); }
  upsertTokenRoute(tokenAccountID, route) { return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/routes`, 'PUT', route); }
  deleteTokenRoute(tokenAccountID, model) { return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/routes`, 'DELETE', { model }); }
  validateTokenCredential(tokenAccountID, credentialID) { return this.mutate('token', `/admin/v1/token-accounts/${encodeURIComponent(tokenAccountID)}/credentials/${encodeURIComponent(credentialID)}/validate`, 'POST', {}); }
  usageRequests(signal) { return this.request('token', '/admin/v1/usage-requests?limit=100', signal); }
  usageRequest(usageRequestID, signal) { return this.request('token', `/admin/v1/usage-requests/${encodeURIComponent(usageRequestID)}`, signal); }
  retryUsageReconciliation(usageRequestID, reason) { return this.mutate('token', `/admin/v1/usage-requests/${encodeURIComponent(usageRequestID)}/retry-reconciliation`, 'POST', { reason }); }
  systemStatus(signal) { return this.request('system', '/status', signal); }

  roleBindings(signal) { return this.request('account', '/admin/v1/role-bindings', signal); }
  grantRole(input) { return this.mutate('account', '/admin/v1/role-bindings', 'POST', input); }
  revokeRole(bindingID) { return this.mutate('account', `/admin/v1/role-bindings/${encodeURIComponent(bindingID)}`, 'DELETE'); }
  registrationOperations(filters, signal) {
    const query = new URLSearchParams();
    for (const key of ['status','stage','responsibility','sla','older_than_minutes']) if (filters[key] !== '') query.set(key, filters[key]);
    return this.request('account', `/admin/v1/registration-operations?${query}`, signal);
  }
  repairRegistration(operationID, action) {
    if (!['retry','compensate'].includes(action)) throw new TypeError('invalid registration action');
    return this.mutate('account', `/admin/v1/registration-operations/${encodeURIComponent(operationID)}/actions/${action}`, 'POST');
  }
  approvalRequests(filters, signal) {
    const query = new URLSearchParams({ limit:'100' });
    for (const key of ['command_type','status','risk_class','requester_user_id','target','submitted_from','submitted_to','expires_from','expires_to']) if (filters[key]) query.set(key, filters[key]);
    return this.request('account', `/admin/v1/approval-requests?${query}`, signal);
  }
  createAdjustment(input) { return this.mutate('billing', '/admin/v1/adjustments', 'POST', input); }
  createApproval(input) { return this.mutate('account', '/admin/v1/approval-requests', 'POST', input); }
  approvalAction(id, action, input) {
    if (!['submit','withdraw','decide'].includes(action)) throw new TypeError('invalid approval action');
    return this.mutate('account', `/admin/v1/approval-requests/${encodeURIComponent(id)}/actions/${action}`, 'POST', input);
  }

  async logout() {
    const response = await fetch(`${this.baseURL}/api/logout`, {
      method: 'POST',
      headers: { 'X-CSRF-Token': this.csrfToken },
      credentials: 'same-origin',
      cache: 'no-store',
    });
    if (!response.ok) throw errorFromResponse(response, await response.json().catch(() => ({})));
    this.csrfToken = '';
    this.pendingRefunds.clear();
    this.pendingOperations.clear();
  }
}
