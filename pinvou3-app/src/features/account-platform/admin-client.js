function errorFromResponse(response, payload) {
  const error = new Error(payload?.message || `HTTP ${response.status}`);
  error.code = payload?.code || 'REQUEST_FAILED';
  error.status = response.status;
  return error;
}

export class AdminPlatformClient {
  constructor(baseURL = '') {
    this.baseURL = String(baseURL || '').replace(/\/$/, '');
    this.csrfToken = '';
  }

  async session(signal) {
    const response = await fetch(`${this.baseURL}/api/session`, { credentials: 'same-origin', cache: 'no-store', signal });
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
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw errorFromResponse(response, payload);
    return payload;
  }

  async mutate(area, resource, method, body) {
    const response = await fetch(`${this.baseURL}/api/admin/${area}${resource}`, {
      method,
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
        'X-CSRF-Token': this.csrfToken,
      },
      credentials: 'same-origin',
      cache: 'no-store',
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const payload = response.status === 204 ? {} : await response.json().catch(() => ({}));
    if (!response.ok) throw errorFromResponse(response, payload);
    return payload;
  }

  accountOverview(signal) { return this.request('account', '/admin/v1/overview', signal); }
  users(search, signal) { return this.request('account', `/admin/v1/users?limit=100&q=${encodeURIComponent(search || '')}`, signal); }
  setUserStatus(userID, status) { return this.mutate('account', `/admin/v1/users/${encodeURIComponent(userID)}/status`, 'PATCH', { status }); }
  deleteUser(userID) { return this.mutate('account', `/admin/v1/users/${encodeURIComponent(userID)}`, 'DELETE'); }
  organizations(search, signal) { return this.request('account', `/admin/v1/organizations?limit=100&q=${encodeURIComponent(search || '')}`, signal); }
  createOrganization(name, ownerUserID) { return this.mutate('account', '/admin/v1/organizations', 'POST', { name, owner_user_id: ownerUserID }); }
  setOrganizationStatus(organizationID, status) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/status`, 'PATCH', { status }); }
  deleteOrganization(organizationID) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}`, 'DELETE'); }
  organizationUnits(organizationID, signal) { return this.request('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units`, signal); }
  createOrganizationUnit(organizationID, name, parentID = null) { return this.mutate('account', `/admin/v1/organizations/${encodeURIComponent(organizationID)}/units`, 'POST', { name, parent_id: parentID }); }
  audits(signal) { return this.request('account', '/admin/v1/audit-logs?limit=100', signal); }
  billingOverview(signal) { return this.request('billing', '/admin/v1/overview', signal); }
  wallets(signal) { return this.request('billing', '/admin/v1/wallets?limit=100', signal); }
  paymentOrders(signal) { return this.request('billing', '/admin/v1/payment-orders?limit=100', signal); }
  adjustments(signal) { return this.request('billing', '/admin/v1/adjustments?limit=100', signal); }
  holds(signal) { return this.request('billing', '/admin/v1/usage-holds?limit=100', signal); }
  tokenOverview(signal) { return this.request('token', '/admin/v1/overview', signal); }
  tokenAccounts(signal) { return this.request('token', '/admin/v1/token-accounts?limit=100', signal); }
  usageRequests(signal) { return this.request('token', '/admin/v1/usage-requests?limit=100', signal); }

  logout() {
    return fetch(`${this.baseURL}/api/logout`, {
      method: 'POST',
      headers: { 'X-CSRF-Token': this.csrfToken },
      credentials: 'same-origin',
    });
  }
}
