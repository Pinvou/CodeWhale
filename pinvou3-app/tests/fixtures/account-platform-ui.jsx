// Controlled public test data. This exercises the real React view and BFF client,
// not a running BFF, PostgreSQL, identity provider, or payment service.
import React, { useState } from 'react';
import { AccountPlatformPopover, AccountPlatformSidebarEntry } from '../../src/features/account-platform/AccountPlatformPopover.jsx';
import { createRoot } from 'react-dom/client';
import { AccountPlatformView } from '../../src/features/account-platform/AccountPlatformView.jsx';
import { AdminPlatformView } from '../../src/features/account-platform/AdminPlatformView.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
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
const createdTokens = [];
let rotated = false;
let balanceRequests = 0;
let nativeAuthenticated = scenario?.startsWith('popover-') && !scenario.startsWith('popover-login') && !scenario.startsWith('popover-mobile');
const publicPNG = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg==';
const challenge = () => ({ captcha_id: ids.usage, background_image: publicPNG, piece_image: publicPNG,
  width: 280, height: 120, piece_width: 44, piece_height: 44, piece_y: 30,
  expires_at: new Date(Date.now() + 300000).toISOString() });
let entitlementReads = 0;
let profileStatus = 'PENDING';
let profileReads = 0;
let profileLogouts = 0;
let roleGranted = false;
let roleAttempts = 0;
let registrationPolls = 0;
let resetAttempts = 0;
let approvalSubmitted = false;
const approvalReplayStatus = scenario?.startsWith('admin-a08-approval-replay-') ? scenario.slice('admin-a08-approval-replay-'.length) : '';
let adjustmentCreates = 0;
let adjustmentRejected = false;
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
  payment_url: location.origin + '/fixture-payment',
  amount_fen: 100,
  ...(scenario === 'qr-png' ? { qr_data: publicPNG } : {}),
  ...(scenario === 'qr-invalid' ? { qr_data: 'https://untrusted.invalid/image.png' } : {}),
  ...(scenario === 'qr-terminal' ? { status: 'MANUAL_REVIEW', qr_data: publicPNG } : {}),
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
  titleCopy(scope, locale) { return dict[locale]?.[scope === "admin" ? "uiAdminPlatform" : "uiAccountPlatform"]?.title; },
  get pending() { return [...pending.keys()]; },
  get copy() { return dict[language].uiAccountPlatform; },
  get adminCopy() { return dict[language].uiAdminPlatform; },
  settle(key, outcome) {
    const item = pending.get(key);
    if (!item) throw new Error(`Unknown fixture request: ${key}`);
    pending.delete(key);
    if (outcome === 'abort') item.reject(new DOMException('Fixture body aborted', 'AbortError'));
    else if (key === 'nativeLogin') { nativeAuthenticated = true; item.resolve({ enabled: true, configured: true, authenticated: true, errorCode: null }); }
    else if (key === 'nativeLogout') { nativeAuthenticated = false; item.resolve({ enabled: true, configured: true, authenticated: false, errorCode: null }); }
    else if (key === 'mobilePrecheck') item.resolve({ username: 'FixtureUser', available: true, password_handle:'fixture-handle',password_policy_version:'v1',expires_at:'2099-01-01T00:00:00Z' });
    else if (key === 'captcha') item.resolve(challenge());
    else if (key === 'nativeRegister') item.resolve({ registration_operation_id:ids.user,user_id: ids.user, username: 'FixtureUser', status: 'PROVISIONING', password_preserved: true });
    else if (key === 'sms') item.resolve({ challenge_id: ids.usage, expires_at: new Date(Date.now() + 300000).toISOString(), retry_after_seconds: 60 });
    else if (key === 'registration') item.resolve({ registration_operation_id:ids.user,user_id: ids.user, username: 'FixtureUser', status: 'PROVISIONING' });
    else if (key === 'bootstrap') item.resolve(json({ user_id: ids.user, display_name: 'Late account', active_context: { context_type: 'PERSONAL' } }));
    else if (key === 'balance') item.resolve(json(wallet(3330000, '')));
    else if (key === 'rotation') { rotated = true; item.resolve(json({ credential_id: 'public-fixture-credential-v2', status: 'ACTIVE', version: 2 })); }
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
  const body = options.body === undefined ? null : JSON.parse(options.body);
  calls.push({ path: url.pathname, method, idempotencyKey: headers.get('Idempotency-Key'), body, csrf: headers.get('X-CSRF-Token') });
  const path = url.pathname;
  if (path === '/api/session') return scenario === 'session-unavailable'
    ? failed('UPSTREAM_UNAVAILABLE', 503)
    : json({ authenticated: true, csrf_token: 'public-fixture-csrf-token-00000000' });
  if (path.startsWith('/api/admin/') && scenario === 'portal-title-admin') return json({});
  if (scenario?.startsWith('admin-a08-') && path.startsWith('/api/admin/')) {
    if (scenario === 'admin-a08-limited' && (path.endsWith('/overview') || path.endsWith('/pricing-versions') || path.endsWith('/reconciliation-runs'))) return failed('PERMISSION_DENIED',403);
    if (path.endsWith('/roles')) return json({items:[{code:'OWNER',assignable:false},{code:'MEMBER',assignable:false},{code:'PLATFORM_SUPER_ADMIN',assignable:false},{code:'PLATFORM_BILLING_OPERATOR',assignable:true},{code:'ORG_ADMIN',assignable:true}]});
    if (path.endsWith('/users')) return json({items:[{user_id:ids.user,username:'AuthorizedIdentity',status:'ACTIVE'}]});
    if (path.endsWith('/wallets')) return json({items:[{wallet_id:ids.order,owner_type:'USER',owner_id:ids.user,posted_microcredits:'10000',reserved_microcredits:'0',status:'ACTIVE'}]});
    if (scenario?.startsWith('admin-a08-adjustment-workflow') && path.endsWith('/adjustments') && method==='GET') return json({items:[{adjustment_id:ids.order,wallet_id:ids.personalToken,amount_microcredits:'10000',reason:'Controlled pending adjustment',requested_by:ids.user,version:1,status:adjustmentRejected?'REJECTED':'PENDING_APPROVAL'}]});
    if (scenario?.startsWith('admin-a08-adjustment-workflow') && path.endsWith('/approve')) return failed('APPROVAL_WORKFLOW_REQUIRED',409);
    if (scenario?.startsWith('admin-a08-adjustment-workflow') && path.endsWith('/reject')) {adjustmentRejected=true;return json({adjustment_id:ids.order,status:'REJECTED',amount_microcredits:'10000'});}
    if (method === 'GET' && ['/orders','/adjustments','/holds','/reconciliation-runs','/pricing-versions'].some(suffix=>path.endsWith(suffix))) return json({items:[]});
    const binding = { binding_id:ids.usage,user_id:ids.user,role_code:'ORG_ADMIN',scope:'ORG',organization_id:ids.organization,unit_id:null,starts_at:'2026-10-01T00:00:00Z',expires_at:null,grant_source:'ADMIN',status:'ACTIVE' };
    if (path.endsWith('/role-bindings')) {
      if (method === 'POST') { roleAttempts += 1; if (scenario === 'admin-a08-role-retry' && roleAttempts === 1) return failed('UPSTREAM_UNAVAILABLE',503); roleGranted=true; return json({...binding, ...body},201); }
      return json({items:roleGranted ? [binding] : [{...binding,role_code:'MEMBER'}]});
    }
    if (path.endsWith('/registration-operations')) return json({items:[{registration_operation_id:ids.usage,status:'REPAIR_REQUIRED',stage:'IDENTITY_CREATING',responsibility:'IDENTITY',attempts:2,reason:'CONTROLLED',next_retry_at:'2026-10-01T00:00:00Z',sla_expires_at:'2026-10-02T00:00:00Z',updated_at:'2026-10-01T00:00:00Z',compensation_allowed:scenario !== 'admin-a08-repair-denied'}]});
    if (path.includes('/registration-operations/') && method === 'POST') return scenario === 'admin-a08-repair-denied' ? failed('PERMISSION_DENIED',403) : json({status:'REGISTRATION_IN_PROGRESS'});
    if (path.endsWith('/adjustments') && method === 'POST') { adjustmentCreates += 1; if (adjustmentCreates > 1) approvalSubmitted = false; return json({adjustment_id:ids.order,wallet_id:body.wallet_id,amount_microcredits:scenario === 'admin-a08-approval-malformed' ? '10000' : body.amount_microcredits,requested_by:ids.user,version:approvalReplayStatus && approvalSubmitted ? 2 : 1},201); }
    if (path.endsWith('/approval-requests')) {
      if (method === 'POST') { if (approvalReplayStatus && approvalSubmitted && body.critical_resource_versions.adjustment_version !== 1) return failed('APPROVAL_CONFLICT',409); return json({approval_request_id:ids.usage,status:approvalSubmitted ? approvalReplayStatus || 'SUBMITTED' : 'DRAFT',version:approvalSubmitted ? 3 : 1},201); }
      return json({items:approvalSubmitted ? [{approval_request_id:ids.usage,command_type:'WALLET_ADJUSTMENT',status:approvalReplayStatus || 'SUBMITTED',risk_class:'HIGH',requester_user_id:ids.user,target_resource_id:ids.order,version:2,allowed_actions:['APPROVE','REJECT','UNKNOWN'],critical_resource_versions:{adjustment_version:1}}] : []});
    }
    if (path.endsWith('/actions/submit')) { approvalSubmitted=true; if (approvalReplayStatus) return failed('UPSTREAM_UNAVAILABLE',503); return json({approval_request_id:ids.usage,status:'SUBMITTED',version:2}); }
    if (path.endsWith('/actions/decide')) return failed('APPROVAL_CONFLICT',409);
    return json({});
  }
  if (path === '/api/logout') {
    profileLogouts += 1;
    if (scenario?.startsWith('a08-profile-') && (scenario !== 'a08-profile-logout-retry' || profileLogouts > 1)) return new Response(null,{status:204});
    return failed('LOGOUT_UNAVAILABLE',503);
  }
  if (path === '/api/account/v1/account/bootstrap' && scenario === 'popover-stale-read') return defer('bootstrap');
  if (path === '/api/account/v1/account/bootstrap') return scenario === 'bootstrap-error'
    ? failed('UPSTREAM_UNAVAILABLE', 503)
    : json({ user_id: ids.user, user_code:'USR-CONTROLLED',username:'FixtureUser',display_name: 'Fixture User', active_context: { context_type: 'PERSONAL' } });
  if (path === '/api/account/v1/me/role-bindings') return scenario === 'a08-roles-403' ? failed('PERMISSION_DENIED',403) : json({items:[{binding_id:ids.usage,role_code:'MEMBER',scope:'ORG',organization_id:ids.organization,starts_at:'2026-10-01T00:00:00Z',expires_at:null,grant_source:'ACCOUNT',status:'ACTIVE'}]});
  if (path === '/api/account/v1/me/profile-change-requests' && method === 'POST') return scenario === 'a08-profile-recent' ? failed('RECENT_AUTHENTICATION_REQUIRED',403) : json({profile_change_request_id:ids.usage,field:body.field,new_value:body.value,status:'PENDING'});
  if (path === '/api/account/v1/auth/captcha/challenges') return json(challenge());
  if (path.endsWith('/mobile-challenges')) return json({challenge_id:ids.order,retry_after_seconds:60,expires_at:'2099-01-01T00:00:00Z'});
  if (path === `/api/account/v1/me/profile-change-requests/${ids.usage}/actions/complete`) { profileStatus=['a08-profile-retry','a08-profile-worker'].includes(scenario) ? 'RETRY' : 'COMPLETED'; return json({profile_change_request_id:ids.usage,field:scenario === 'a08-profile-mobile' ? 'MOBILE' : 'USERNAME',new_value:scenario === 'a08-profile-mobile' ? '13900000000' : 'ChangedUser',status:profileStatus}); }
  if (path === `/api/account/v1/me/profile-change-requests/${ids.usage}`) {
    profileReads += 1;
    profileStatus = scenario === 'a08-profile-worker' ? ['VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','COMPLETED'][Math.min(profileReads-1,3)] : 'COMPLETED';
    return json({profile_change_request_id:ids.usage,field:'USERNAME',new_value:'ChangedUser',status:profileStatus});
  }
  if (path === '/api/account/v1/me/organizations') return json({
    items: scenario === 'no-organization' ? [] : [{ organization_id: ids.organization, name: 'Fixture Organization' }],
  });
  if (path === '/api/account/v1/sessions/current/active-context') {
    organizationId = body.organization_id || '';
    return json({ context_type: body.context_type, organization_id: organizationId || null });
  }
  if (path === '/api/account/v1/organizations' && method === 'POST') {
    return failed(scenario === 'action-401' ? 'LOGIN_REQUIRED' : 'PERMISSION_DENIED',
      scenario === 'action-401' ? 401 : 403);
  }
  if (path.startsWith('/api/account/v1/organizations/') && method === 'POST') return failed(scenario === 'action-401' ? 'LOGIN_REQUIRED' : 'PERMISSION_DENIED', scenario === 'action-401' ? 401 : 403);
  if (path.startsWith('/api/account/v1/organizations/')) return json({ items: [] });
  if (path === '/api/token/v1/recharge/products') return scenario === 'context-401'
    ? failed('LOGIN_REQUIRED', 401)
    : json({ items: [{ product_id: 'CNY_1', amount_fen: 100, display_points: 100,
      channels: ['UNIONPAY', 'ALIPAY', 'WECHAT'] }] });
  if (path === '/api/token/v1/me/balance') {
    balanceRequests += 1;
    if (scenario === 'popover-balance-401') return failed('LOGIN_REQUIRED', 401);
    if (['balance-context', 'balance-paid'].includes(scenario) && balanceRequests === 2) return defer('balance');
    return json(wallet(organizationId ? 2220000 : 1110000));
  }
  if (path === '/api/token/v1/me/entitlement-packages') {
    entitlementReads += 1;
    if (scenario === 'entitlement-401') return failed('LOGIN_REQUIRED', 401);
    return json({ summary: { available_microcredits: '90071992547409930000' }, items: scenario === 'entitlement-history' ? [{
      package_id: ids.usage, pinvou_order_id: ids.order, mall_order_id: 'public-mall-order',
      plan_name: 'Public Plan', sku_code: 'PUBLIC_PLAN', status: 'ACTIVE',
      amount_fen: '9007199254740993', points_total_microcredits: '90071992547409930000',
      points_used_microcredits: '10000', points_remaining_microcredits: '90071992547409920000',
      valid_from: '2026-10-01T00:00:00Z', valid_until: '2026-11-01T00:00:00Z', credited_at: '2026-10-01T00:00:00Z',
      components:[{component_id:ids.order,component_type:'BASE',original_microcredits:'90071992547409930000',available_microcredits:'90071992547409920000',reserved_microcredits:'0',used_microcredits:'10000',expired_microcredits:'0',revoked_microcredits:'0',refunded_microcredits:'0',refundable_microcredits:'0',expires_at:null}],
    }] : [] });
  }
  if (path === '/api/token/v1/recharge/orders' && method === 'GET') {
    if (scenario === 'orders-401') return failed('LOGIN_REQUIRED', 401);
    return json({ items: scenario === 'entitlement-history' ? [{
      order_id: ids.order, pinvou_order_id: ids.order, mall_order_id: 'public-mall-order', plan_name: 'Public Plan',
      sku_code: 'PUBLIC_PLAN', channel: 'ALIPAY', status: 'PAID', amount_fen: '9007199254740993',
      created_at: '2026-10-01T00:00:00Z', paid_at: null, credited_at: null, valid_until: null,
      fulfillment_status: 'CREDITED', invoice_status: 'UNSUPPORTED',
    }] : [] });
  }
  if (path === '/api/token/v1/me/usage-requests') return json({ items: [{ usage_request_id: ids.usage, display_status: 'SETTLED', model: 'model-a', actual_microcredits: '1000000', started_at: '2026-10-01T00:00:00Z', input_tokens: 2, output_tokens: 3, cached_input_tokens: 0 }], next_cursor: null });
  if (path === '/api/token/v1/token-accounts' && method === 'POST') {
    calls[calls.length - 1].body = body;
    const created = { ...body, token_account_id: ids.usage, status: 'ACTIVE' };
    createdTokens.push(created); return json(created, 201);
  }
  if (path === '/api/token/v1/token-accounts') return json({ items: [token(), ...createdTokens.filter(item => item.owner_id === (organizationId || ids.user))] });
  if (path.endsWith('/credentials')) {
    if (method === 'POST') { calls[calls.length - 1].body = body; return defer('rotation'); }
    if (scenario === 'credential-late' && path.includes(ids.personalToken)) return defer('credential');
    return json({ items: [{ credential_id: 'public-fixture-credential',
      provider: organizationId ? 'organization-provider' : 'personal-provider',
      status: 'ACTIVE', version: rotated ? 2 : 1, fingerprint: '0123456789abcdef' }] });
  }
  if (path === '/api/token/v1/token-accounts/eligible') return defer('eligible');
  if (path.startsWith('/api/token/v1/usage-requests/')) return defer('usage');
  if (path === '/api/token/v1/recharge/orders/recent') return json({ order:
    ['balance-context', 'balance-paid', 'poll-401', 'qr-png', 'qr-invalid', 'qr-terminal'].includes(scenario) && !organizationId ? order() : null,
  });
  if (path === `/api/token/v1/recharge/orders/${ids.order}`) return scenario === 'poll-401'
    ? failed('LOGIN_REQUIRED', 401) : json(order('PAID'));
  if (path === '/api/token/v1/recharge/orders' && method === 'POST') return json(order(), 201);
  throw new Error(`Unimplemented fixture route: ${method} ${path}`);
};

async function mount() {
  if (!await ensureLanguage(language)) throw new Error('Fixture language failed to load');
  if (scenario?.startsWith('portal-title-')) {
    const scope = scenario.endsWith('admin') ? 'admin' : 'account';
    localStorage.setItem('pinvou.' + scope + '.language', language);
    if (scope === 'admin') { document.getElementById('account-root').id = 'admin-root'; await import('../../src/app/admin-main.jsx'); }
    else await import('../../src/app/account-main.jsx');
    return;
  }
  if (scenario?.startsWith('admin-a08-')) {
    createRoot(document.getElementById('account-root')).render(<AdminPlatformView client={new AdminPlatformClient()} copy={dict[language].uiAdminPlatform} language={language} onLanguageChange={()=>{}} />);
    return;
  }
  const popover = scenario?.startsWith('popover-');
  const desktop = scenario?.startsWith('desktop-') || scenario?.startsWith('sidebar-') || popover;
  const client = desktop ? new AccountPlatformDesktopClient({
    session: async () => { calls.push({ path: 'nativeSession', method: 'LOCAL' }); return { enabled: true, configured: true, authenticated: nativeAuthenticated, errorCode: null }; },
    createSliderCaptcha: async input => {
      calls.push({ path: 'nativeCaptcha', method: 'LOCAL', input });
      return scenario === 'popover-mobile-captcha-late' ? defer('captcha') : challenge();
    },
    registrationStatus:async () => {
      calls.push({path:'nativeRegistrationStatus',method:'LOCAL'}); registrationPolls += 1;
      if (scenario === 'popover-mobile-restored-pending') return {registration_operation_id:ids.user,username:'FixtureUser',status:'IDENTITY_CREATING',password_preserved:true};
      if (scenario === 'popover-mobile-restored-active' && registrationPolls === 1) return {registration_operation_id:ids.user,username:'FixtureUser',status:'ACTIVE',password_preserved:true};
      return null;
    },
    precheckMobileRegistration: async input => {
      calls.push({ path: 'nativePrecheck', method: 'LOCAL', input });
      return scenario === 'popover-mobile-precheck-late' ? defer('mobilePrecheck') : { username: 'FixtureUser', available: true, password_handle:'fixture-handle',password_policy_version:'v1',expires_at:'2099-01-01T00:00:00Z' };
    },
    sendSms: async input => {
      calls.push({ path: 'nativeSendSMS', method: 'LOCAL', input });
      if (scenario === 'popover-mobile-rate') throw 'mobile_auth_failed:SMS_RATE_LIMITED:86400';
      if (scenario === 'popover-mobile-unknown-error') throw 'PRIVATE_PROVIDER_DETAIL SMS_UNAVAILABLE';
      return scenario === 'popover-mobile-sms-late' ? defer('sms')
        : { challenge_id: ids.usage, expires_at: new Date(Date.now() + 300000).toISOString(), retry_after_seconds: 60, ...(input.purpose === 'REGISTER' ? { registration_operation_id:ids.user,registration_query_token_expires_at:'2099-01-01T00:00:00Z',recovery_persisted:scenario !== 'popover-mobile-memory-only' } : {}) };
    },
    registerMobile: async input => {
      calls.push({ path: 'nativeRegister', method: 'LOCAL', input });
      return scenario?.startsWith('popover-mobile-recovered-late') ? defer('nativeRegister')
        : { registration_operation_id:ids.user,user_id: ids.user, username: 'FixtureUser', status: 'PROVISIONING', password_preserved: scenario === 'popover-mobile-recovered' };
    },
    loginSms: async input => {
      calls.push({ path: 'nativeSMSLogin', method: 'LOCAL', input });
      nativeAuthenticated = true;
      return { enabled: true, configured: true, authenticated: true, errorCode: null };
    },
    resetPassword:async input => { calls.push({path:'nativeResetPassword',method:'LOCAL',input}); resetAttempts += 1; if (scenario === 'popover-mobile-forgot-retry' && resetAttempts === 1) throw 'mobile_auth_failed:PROFILE_CHANGE_FAILED'; return {}; },
    login: async credentials => {
      calls.push({ path: 'nativeLogin', method: 'LOCAL', credentials });
      if (scenario === 'popover-mobile-register-login-failure' || scenario === 'desktop-login-failure' || scenario === 'popover-login-failure') throw 'oidc_token_endpoint_unavailable: PRIVATE_PROVIDER_DETAIL';
      return defer('nativeLogin');
    },
    logout: async () => { calls.push({ path: 'nativeLogout', method: 'LOCAL' }); if (scenario === 'popover-logout-delay') return defer('nativeLogout'); nativeAuthenticated = false; return { enabled: true, configured: true, authenticated: false, errorCode: null }; },
    request: async request => {
      const response = await fetch('/api/' + request.area + request.resource, {
        method: request.method, body: request.body === undefined ? undefined : JSON.stringify(request.body),
        headers: request.idempotencyKey ? { 'Idempotency-Key': request.idempotencyKey } : {},
      });
      return { status: response.status, body: await response.json() };
    },
  }) : new AccountPlatformBffClient();
  function PopoverFixture() {
    const [open, setOpen] = useState(true);
    const [locale, setLocale] = useState(language);
    const [activeClient, setActiveClient] = useState(client);
    window.__accountReview.replaceClient = () => setActiveClient(new AccountPlatformDesktopClient({
      session: async () => ({ enabled: true, configured: true, authenticated: true, errorCode: null }),
      request: async request => ({ status: 200, body: request.resource === '/v1/billing/balance'
        ? wallet(1700000) : { user_id: ids.user, display_name: 'Other User', status: 'ACTIVE' } }),
    }));
    window.__accountReview.setOpen = setOpen;
    window.__accountReview.changeLanguage = async next => { await ensureLanguage(next); setLocale(next); };
    return <AccountPlatformPopover open={open} client={activeClient} copy={dict[locale].uiAccountPlatform}
      language={locale} theme="light" collapsed={false}
      onClose={() => { calls.push({ path: 'popoverClose', method: 'LOCAL' }); setOpen(false); }} />;
  }
  if (scenario?.startsWith('sidebar-')) {
    createRoot(document.getElementById('account-root')).render(<AccountPlatformSidebarEntry transport={client.transport}
      copy={dict[language].uiAccountPlatform} language={language} theme="light" collapsed={false}
      onOpen={() => calls.push({ path: 'sidebarOpen', method: 'LOCAL' })}
      onOpenCenter={() => calls.push({ path: 'openAccountCenter', method: 'LOCAL' })} />);
    return;
  }
  if (popover) { createRoot(document.getElementById('account-root')).render(<PopoverFixture />); return; }
  createRoot(document.getElementById('account-root')).render(<AccountPlatformView
    client={client} copy={dict[language].uiAccountPlatform}
    language={language} onLanguageChange={() => {}} portal={!desktop}
    onClose={desktop ? () => calls.push({ path: 'nativeClose', method: 'LOCAL' }) : undefined}
  />);
}

mount();
