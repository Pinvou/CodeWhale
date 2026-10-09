import { requireSession } from './personal-contract.js';
import { mergeObservedVersions } from './authorization-ui.js';
import { secureRequestID } from './request-id.js';
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

export class AdminPlatformClient {
  constructor(baseURL = '') {
    this.baseURL = String(baseURL || '').replace(/\/$/, '');
    this.csrfToken = '';
    this.authorizationVersions = {};
    this.authorizationVersionListeners = new Set();
    this.inflightReads = new Map();
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
    if (response.status === 401) { this.csrfToken = ''; return { authenticated: false }; }
    if (!response.ok) throw errorFromResponse(response, await response.json().catch(() => ({})));
    const payload = await response.json();
    this.csrfToken = payload.csrf_token || '';
    return requireSession(payload);
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
    const options = {
      method,
      headers,
      credentials: 'same-origin',
      cache: 'no-store',
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    };
    const response = await fetch(`${this.baseURL}/api/admin/${area}${resource}`, options);
    this.observeAuthorizationVersions(response.headers);
    const payload = response.status === 204 ? null : await response.json().catch(() => ({}));
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
