// Controlled public test data. This exercises the real React view and BFF client,
// not a running BFF, PostgreSQL, identity provider, or payment service.
import React from 'react';
import { createRoot } from 'react-dom/client';
import { AccountPlatformView } from '../../src/features/account-platform/AccountPlatformView.jsx';
import { AccountPlatformBffClient } from '../../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../../src/features/account-platform/desktop-client.js';
import { dict, ensureLanguage } from '../../src/shared/i18n.js';

const query = new URLSearchParams(location.search);
const scenario = query.get('scenario');
const language = query.get('language') || 'en';
const ids = {
  user: '00000000-0000-4000-8000-000000000001',
  organization: '00000000-0000-4000-8000-000000000002',
  personalToken: '00000000-0000-4000-8000-000000000003',
  organizationToken: '00000000-0000-4000-8000-000000000004',
  order: '00000000-0000-4000-8000-000000000005',
  usage: '00000000-0000-4000-8000-000000000006',
};
let organizationId = '';
let balanceRequests = 0;
const calls = [];
const pending = new Map();
const json = (body, status = 200) => new Response(JSON.stringify(body), {
  status, headers: { 'Content-Type': 'application/json' },
});
const failed = (code, status) => json({ code, message: 'UNTRANSLATED_FIXTURE_ERROR' }, status);
const wallet = (amount, organization = organizationId) => ({
  wallet_id: organization ? 'organization-wallet' : 'personal-wallet',
  available_microcredits: amount, reserved_microcredits: 0,
});
const token = (organization = organizationId) => ({
  token_account_id: organization ? ids.organizationToken : ids.personalToken,
  name: organization ? 'Organization Token' : 'Personal Token',
  status: 'ACTIVE', share_scope: organization ? 'ORG_ALL' : 'OWNER_UNIT_ONLY',
});
const order = (status = 'PENDING') => ({
  order_id: ids.order, status, expires_at: new Date(Date.now() + 60_000).toISOString(),
  payment_url: `${location.origin}/fixture-payment`,
});

function defer(key) {
  if (pending.has(key)) return pending.get(key).promise;
  let resolve;
  let reject;
  const promise = new Promise((accept, decline) => { resolve = accept; reject = decline; });
  pending.set(key, { promise, resolve, reject });
  return promise;
}

window.__accountReview = {
  ids, calls,
  get pending() { return [...pending.keys()]; },
  get copy() { return dict[language].uiAccountPlatform; },
  settle(key, outcome) {
    const item = pending.get(key);
    if (!item) throw new Error(`Unknown fixture request: ${key}`);
    pending.delete(key);
    if (outcome === 'abort') item.reject(new DOMException('Fixture body aborted', 'AbortError'));
    else if (key === 'nativeLogin') item.resolve({ authenticated: true });
    else if (key === 'balance') item.resolve(json(wallet(333, '')));
    else if (key === 'eligible') item.resolve(json({ items: [{
      token_account_id: ids.personalToken, name: 'Eligible Personal Token',
      owner_type: 'USER', allowed_models: ['model-a'], health_status: 'HEALTHY',
    }] }));
    else if (key === 'usage') item.resolve(json({
      usage_request_id: ids.usage, display_status: 'SETTLEMENT_PENDING',
      model: 'model-a', maximum_microcredits: 20,
    }));
    else throw new Error(`Unsupported fixture completion: ${key}`);
  },
};

// Only API calls use this controlled transport. Browser module loading and QR
// generation use the installed Vite and qrcode packages without substitutions.
globalThis.fetch = async (input, options = {}) => {
  const url = new URL(input, location.href);
  if (url.origin !== location.origin) throw new Error('Fixture refuses external requests');
  const headers = new Headers(options.headers);
  const method = options.method || 'GET';
  calls.push({ path: url.pathname, method, idempotencyKey: headers.get('Idempotency-Key') });
  const body = options.body === undefined ? null : JSON.parse(options.body);
  const path = url.pathname;
  if (path === '/api/session') return scenario === 'session-unavailable'
    ? failed('UPSTREAM_UNAVAILABLE', 503)
    : json({ authenticated: true, csrf_token: 'public-fixture-csrf-token-00000000' });
  if (path === '/api/logout') return failed('LOGOUT_UNAVAILABLE', 503);
  if (path === '/api/account/v1/account/bootstrap') return scenario === 'bootstrap-error'
    ? failed('UPSTREAM_UNAVAILABLE', 503)
    : json({ user_id: ids.user, display_name: 'Fixture User', active_context: { context_type: 'PERSONAL' } });
  if (path === '/api/account/v1/me/organizations') return json({
    items: [{ organization_id: ids.organization, name: 'Fixture Organization' }],
  });
  if (path === '/api/account/v1/sessions/current/active-context') {
    organizationId = body.organization_id || '';
    return json({ context_type: body.context_type, organization_id: organizationId || null });
  }
  if (path === '/api/account/v1/organizations' && method === 'POST') {
    return failed(scenario === 'action-401' ? 'LOGIN_REQUIRED' : 'PERMISSION_DENIED',
      scenario === 'action-401' ? 401 : 403);
  }
  if (path.startsWith('/api/account/v1/organizations/')) return json({ items: [] });
  if (path === '/api/token/v1/recharge/products') return scenario === 'context-401'
    ? failed('LOGIN_REQUIRED', 401)
    : json({ items: [{ product_id: 'CNY_1', amount_fen: 100, display_points: 100,
      channels: ['UNIONPAY', 'ALIPAY', 'WECHAT'] }] });
  if (path === '/api/token/v1/me/balance') {
    balanceRequests += 1;
    if (['balance-context', 'balance-paid'].includes(scenario) && balanceRequests === 2) return defer('balance');
    return json(wallet(organizationId ? 222 : 111));
  }
  if (path === '/api/token/v1/token-accounts') return json({ items: [token()] });
  if (path.endsWith('/credentials')) {
    if (scenario === 'credential-late' && path.includes(ids.personalToken)) return defer('credential');
    return json({ items: [{ credential_id: 'public-fixture-credential',
      provider: organizationId ? 'organization-provider' : 'personal-provider',
      status: 'ACTIVE', version: 1, fingerprint: '0123456789abcdef' }] });
  }
  if (path === '/api/token/v1/token-accounts/eligible') return defer('eligible');
  if (path.startsWith('/api/token/v1/usage-requests/')) return defer('usage');
  if (path === '/api/token/v1/recharge/orders/recent') return json({ order:
    ['balance-context', 'balance-paid', 'poll-401'].includes(scenario) && !organizationId ? order() : null,
  });
  if (path === `/api/token/v1/recharge/orders/${ids.order}`) return scenario === 'poll-401'
    ? failed('LOGIN_REQUIRED', 401) : json(order('PAID'));
  if (path === '/api/token/v1/recharge/orders' && method === 'POST') return json(order(), 201);
  throw new Error(`Unimplemented fixture route: ${method} ${path}`);
};

async function mount() {
  if (!await ensureLanguage(language)) throw new Error('Fixture language failed to load');
  const desktop = scenario?.startsWith('desktop-');
  const client = desktop ? new AccountPlatformDesktopClient({
    session: async () => ({ authenticated: false }),
    login: async callbackCopy => {
      calls.push({ path: 'nativeLogin', method: 'LOCAL', callbackCopy });
      if (scenario === 'desktop-login-failure') throw 'oidc_token_endpoint_unavailable: PRIVATE_PROVIDER_DETAIL';
      return defer('nativeLogin');
    },
    logout: async () => { calls.push({ path: 'nativeLogout', method: 'LOCAL' }); return { authenticated: false }; },
    request: async request => {
      const response = await fetch('/api/' + request.area + request.resource, {
        method: request.method, body: request.body === undefined ? undefined : JSON.stringify(request.body),
        headers: request.idempotencyKey ? { 'Idempotency-Key': request.idempotencyKey } : {},
      });
      return { status: response.status, body: await response.json() };
    },
  }) : new AccountPlatformBffClient();
  createRoot(document.getElementById('account-root')).render(<AccountPlatformView
    client={client} copy={dict[language].uiAccountPlatform}
    language={language} onLanguageChange={() => {}} portal={!desktop}
    onClose={desktop ? () => calls.push({ path: 'nativeClose', method: 'LOCAL' }) : undefined}
  />);
}

mount();
