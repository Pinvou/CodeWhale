import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile, readdir } from 'node:fs/promises';
import { createContextRequestGuard, eligibleForContext, rechargePollDelay } from '../src/features/account-platform/context-guard.js';
import { formatPoints, MICROCREDITS_PER_POINT, microcreditsToPoints } from '../src/features/account-platform/credit-units.js';
import { initialPortalSection, portalNavigationSections, successfulPaymentOrders } from '../src/features/account-platform/portal-sections.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { buildUnitTree } from '../src/features/account-platform/admin-organization-tree.js';
import { accountPlatformCopy } from '../src/shared/account-platform-i18n.js';

async function sourceFiles(root) {
  const files = [];
  for (const entry of await readdir(root, { withFileTypes: true })) {
    const child = new URL(`${entry.name}${entry.isDirectory() ? '/' : ''}`, root);
    if (entry.isDirectory()) files.push(...await sourceFiles(child));
    else if (/\.(?:js|jsx|mjs|rs)$/.test(entry.name)) files.push(child);
  }
  return files;
}

test('account balances convert internal microcredits to display points', () => {
  assert.equal(MICROCREDITS_PER_POINT, 10_000);
  assert.equal(microcreditsToPoints(1_000_000), 100);
  assert.equal(formatPoints(1_000_000, 'en'), '100');
  assert.equal(formatPoints(7_969_994, 'en'), '796.9994');
  assert.equal(formatPoints(1, 'en'), '0.0001');
  assert.equal(formatPoints(0, 'en'), '0');
  assert.equal(formatPoints(null, 'en'), '—');
});

test('Web personal center uses three consolidated tabs and hides unsuccessful orders', () => {
  assert.deepEqual(portalNavigationSections, ['creditsUsage', 'ordersInvoices', 'accountSettings']);
  assert.equal(initialPortalSection(''), 'accountSettings');
  assert.equal(initialPortalSection('?section=profile'), 'accountSettings');
  assert.equal(initialPortalSection('?section=usage'), 'creditsUsage');
  assert.equal(initialPortalSection('?section=invoice'), 'ordersInvoices');
  assert.deepEqual(successfulPaymentOrders([
    { order_id:'created', status:'CREATED' },
    { order_id:'pending', status:'PENDING' },
    { order_id:'paid', status:'PAID' },
    { order_id:'late', status:'PAID_LATE' },
    { order_id:'failed', status:'FAILED' },
    { order_id:'review', status:'MANUAL_REVIEW' },
  ]).map(order => order.order_id), ['paid', 'late']);
  for (const locale of ['zh', 'en', 'ja']) {
    assert.ok(accountPlatformCopy[locale].sections.creditsUsage);
    assert.ok(accountPlatformCopy[locale].sections.ordersInvoices);
    assert.ok(accountPlatformCopy[locale].sections.accountSettings);
    assert.ok(accountPlatformCopy[locale].noPaidOrders);
  }
});

test('admin organization client uses protected organization, member, and unit endpoints', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    return new Response(JSON.stringify(options.method === 'POST' ? { organization_id:'org-id' } : { items:[] }), { status:options.method === 'POST' ? 201 : 200, headers:{ 'Content-Type':'application/json' } });
  };
  try {
    const client = new AdminPlatformClient('/portal');
    client.csrfToken = 'c'.repeat(32);
    await client.createOrganization('Example', 'owner-id');
    await client.organizations('研发 & AI');
    await client.organizationMembers('org-id');
    await client.transferOrganizationOwnership('org-id', 'new-owner-id');
    await client.organizationUnits('org-id');
    await client.createOrganizationUnit('org-id', 'Engineering', 'parent-id');
    await client.renameOrganization('org-id', 'Renamed');
    await client.renameOrganizationUnit('org-id', 'unit-id', 'Platform');
    await client.deleteOrganizationUnit('org-id', 'unit-id');
    assert.deepEqual(calls.map(call => [call.url, call.options.method || 'GET']), [
      ['/portal/api/admin/account/admin/v1/organizations', 'POST'],
      ['/portal/api/admin/account/admin/v1/organizations?limit=100&q=%E7%A0%94%E5%8F%91%20%26%20AI', 'GET'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/members', 'GET'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/ownership', 'POST'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/units', 'GET'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/units', 'POST'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id', 'PATCH'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/units/unit-id', 'PATCH'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/units/unit-id', 'DELETE'],
    ]);
    assert.deepEqual(JSON.parse(calls[0].options.body), { name:'Example', owner_user_id:'owner-id' });
    assert.deepEqual(JSON.parse(calls[3].options.body), { new_owner_user_id:'new-owner-id' });
    assert.deepEqual(JSON.parse(calls[5].options.body), { name:'Engineering', parent_id:'parent-id' });
    assert.deepEqual(JSON.parse(calls[6].options.body), { name:'Renamed' });
    assert.deepEqual(JSON.parse(calls[7].options.body), { name:'Platform' });
  } finally { globalThis.fetch = originalFetch; }
});

test('user code is displayed to its owner and resolved through a dedicated exact admin endpoint', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    calls.push(url);
    return new Response(JSON.stringify({ user_id:'user-id', user_code:'USR-0123-4567-89AB-CDEF-MV', status:'ACTIVE' }), { status:200, headers:{ 'Content-Type':'application/json' } });
  };
  try {
    const client = new AdminPlatformClient('/portal');
    await client.userByCode('usr-0123-4567-89ab-cdef-mv');
    assert.deepEqual(calls, ['/portal/api/admin/account/admin/v1/user-code-lookups/usr-0123-4567-89ab-cdef-mv']);
  } finally { globalThis.fetch = originalFetch; }

  const personal = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const admin = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  assert.match(personal, /me\?\.user_code/);
  assert.match(popover, /me\?\.user_code/);
  assert.match(admin, /client\.userByCode/);
  assert.match(admin, /key:'user_code'/);
  assert.doesNotMatch(personal, /me\?\.user_id/);
  for (const locale of ['zh', 'en', 'ja']) assert.ok(accountPlatformCopy[locale].userCode);
});

test('admin organization units form a nested management tree', () => {
  const roots = buildUnitTree([
    { unit_id:'child', parent_id:'root', name:'Child' },
    { unit_id:'root', parent_id:null, name:'Root' },
  ]);
  assert.equal(roots.length, 1);
  assert.equal(roots[0].unit_id, 'root');
  assert.equal(roots[0].children[0].unit_id, 'child');
});

test('context guard invalidates and aborts stale organization loads', () => {
  const guard = createContextRequestGuard();
  const first = guard.begin();
  const second = guard.begin();
  assert.equal(first.signal.aborted, true);
  assert.equal(guard.isCurrent(first.generation), false);
  assert.equal(guard.isCurrent(second.generation), true);
});

test('recharge polling backs off from two to five seconds with bounded jitter', () => {
  assert.equal(rechargePollDelay(0, () => 0.5), 2000);
  assert.equal(rechargePollDelay(3, () => 0.5), 3500);
  assert.equal(rechargePollDelay(99, () => 0.5), 5000);
  assert.ok(rechargePollDelay(0, () => 0) >= 1800);
  assert.ok(rechargePollDelay(99, () => 1) <= 5500);
});

test('organization context never accepts a personal Token candidate', () => {
  const items = [{ token_account_id: 'personal', owner_type: 'USER' }, { token_account_id: 'organization', owner_type: 'ORG' }];
  assert.deepEqual(eligibleForContext(items, 'organization-id').map(item => item.token_account_id), ['organization']);
  assert.deepEqual(eligibleForContext(items, '').map(item => item.token_account_id), ['personal']);
});

test('BFF client keeps CSRF on mutations and never persists OIDC tokens', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    if (String(url).endsWith('/api/session')) return new Response(JSON.stringify({ authenticated: true, csrf_token: 'c'.repeat(32) }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    return new Response(JSON.stringify({ items: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('https://portal.example');
    await client.session();
    await client.switchContext('PERSONAL', null, 7);
    assert.equal(calls[1].options.headers['X-CSRF-Token'], 'c'.repeat(32));
    assert.equal(calls[1].options.credentials, 'same-origin');
    assert.equal(JSON.parse(calls[1].options.body).expected_context_version, 7);
    assert.equal('accessToken' in client, false);
    assert.equal('refreshToken' in client, false);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('BFF client restores the recent order without browser persistence', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    return new Response(JSON.stringify({ order: null }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('/portal');
    assert.deepEqual(await client.recentRechargeOrder(), { order: null });
    assert.equal(calls[0].url, '/portal/api/token/v1/recharge/orders/recent');
    assert.equal(calls[0].options.cache, 'no-store');
    assert.equal('recentOrder' in client, false);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('personal usage history is read-only and never resends a model request', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    return new Response(JSON.stringify({ items: [{ model: 'example-model', display_status: 'SETTLED', actual_microcredits: 10_000 }] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('/portal');
    const history = await client.usageHistory();
    assert.equal(history.items[0].display_status, 'SETTLED');
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, '/portal/api/token/v1/me/usage-requests?limit=20');
    assert.equal(calls[0].options.method, 'GET');
    assert.equal(calls[0].url.includes('/chat/completions'), false);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('desktop client keeps OAuth tokens behind the account-platform Bridge', async () => {
  const calls = [];
  const transport = {
    session: async () => ({ enabled: true, configured: true, authenticated: true }),
    login: async credentials => {
      calls.push({ credentials });
      return { authenticated: true };
    },
    logout: async () => ({ authenticated: false }),
    request: async request => {
      calls.push(request);
      return { status: 200, body: { items: [] } };
    },
  };
  const client = new AccountPlatformDesktopClient(transport);
  assert.equal((await client.session()).authenticated, true);
  await client.login('person@example.com', 'secret-password');
  await client.createRecharge('product-10', 'ALIPAY', 'recharge-id');
  assert.deepEqual(calls[0], { credentials: { account: 'person@example.com', password: 'secret-password' } });
  assert.deepEqual(calls[1], {
    area: 'token',
    resource: '/v1/recharge/orders',
    method: 'POST',
    body: { product_id: 'product-10', channel: 'ALIPAY' },
    idempotencyKey: 'recharge-id',
  });
  assert.equal('accessToken' in client, false);
  assert.equal('refreshToken' in client, false);
});

test('desktop client surfaces backend error codes without exposing auth state', async () => {
  const client = new AccountPlatformDesktopClient({
    request: async () => ({ status: 403, body: { code: 'permission_denied', message: 'denied' } }),
  });
  await assert.rejects(client.organizations(), error => {
    assert.equal(error.code, 'permission_denied');
    assert.equal(error.status, 403);
    return true;
  });
});

test('account clients publish only increasing global context versions', async () => {
  const originalFetch = globalThis.fetch;
  const observed = [];
  let version = 4;
  globalThis.fetch = async () => new Response(JSON.stringify({ items: [] }), {
    status: 200,
    headers: { 'Content-Type': 'application/json', 'X-Context-Version': String(version) },
  });
  try {
    const client = new AccountPlatformBffClient('/portal');
    const unsubscribe = client.onContextVersion(value => observed.push(value));
    await client.organizations();
    version = 3;
    await client.organizations();
    version = 6;
    await client.organizations();
    unsubscribe();
    assert.deepEqual(observed, [4, 6]);
    assert.equal(client.contextVersion, 6);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('personal usage history supports opaque pagination without exposing routing secrets', async () => {
  const bffClient = await readFile(new URL('../src/features/account-platform/bff-client.js', import.meta.url), 'utf8');
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  assert.match(bffClient, /cursor=\$\{encodeURIComponent\(cursor\)\}/);
  assert.match(view, /record\.input_tokens/);
  assert.match(view, /record\.output_tokens/);
  assert.match(view, /page\.next_cursor/);
  assert.doesNotMatch(view, /record\.(?:token_account_id|credential_id|wallet_id|provider)/);
});

test('desktop account entry is a popover and no longer mounts a full workspace', async () => {
  const source = await readFile(new URL('../src/app/main.jsx', import.meta.url), 'utf8');
  assert.match(source, /data-testid="account-platform-button"/);
  assert.match(source, /<AccountPlatformPopover/);
  assert.doesNotMatch(source, /accountPlatformEnabled && accountPlatformClient/);
  assert.doesNotMatch(source, /data-testid="llmapi-account-button"/);
  assert.doesNotMatch(source, /<AccountPopover/);
  assert.doesNotMatch(source, /navigateFromScheduledRun\('accountPlatform'\)/);
  assert.doesNotMatch(source, /currentView === 'accountPlatform' && \(/);
  assert.doesNotMatch(source, /<AccountPlatformView/);
});

test('desktop account entry uses the same monochrome round icon treatment as sidebar controls', async () => {
  const source = await readFile(new URL('../src/app/main.jsx', import.meta.url), 'utf8');
  const entry = source.match(/data-testid="account-platform-button"[\s\S]*?<AccountPlatformPopover/)?.[0] || '';
  assert.match(entry, /rounded-full/);
  assert.match(entry, /<User size=\{18\}/);
  assert.doesNotMatch(entry, /bg-gradient-to-br/);
});

test('desktop sign-in renders inside the popover and never opens a browser', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const bridge = await readFile(new URL('../src/platform/tauri/bridge/account-platform.js', import.meta.url), 'utf8');
  const backend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  assert.match(popover, /type="password"/);
  assert.match(popover, /'password', 'sms', 'register'/);
  assert.match(popover, /copy\.registrationUsername/);
  assert.match(popover, /client\.registerMobile\(account\.trim\(\), mobile\.trim\(\)/);
  assert.match(popover, /client\.precheckMobileRegistration\(account\.trim\(\), mobile\.trim\(\), password\)/);
  assert.match(popover, /client\.createSliderCaptcha\(mobile\.trim\(\)/);
  assert.match(popover, /<SliderCaptcha/);
  assert.match(popover, /setSmsCooldown\(Math\.max\(1, Number\(result\.retry_after_seconds \|\| 60\)\)\)/);
  assert.match(popover, /smsCooldown > 0 \? copy\.resendCountdown\(smsCooldown\)/);
  assert.match(popover, /autoComplete=\{loginMode === 'register' \? 'new-password' : 'current-password'\}/);
  assert.match(popover, /await client\.loginSms\(mobile\.trim\(\), smsChallenge, smsCode\.trim\(\)\)/);
  assert.match(popover, /await client\.registerMobile/);
  assert.match(popover, /try \{\s*await client\.login\(registeredUsername, password\);\s*\} catch \{/);
  assert.match(popover, /setError\(copy\.registrationCompletedLoginFailed\)/);
  assert.match(popover, /setLoginMode\('password'\)/);
  assert.match(bridge, /invoke\("account_platform_login", \{ credentials: credentials \}\)/);
  const loginImplementation = backend.match(/pub async fn login\([sS]*?pub async fn send_sms/)?.[0] || '';
  assert.doesNotMatch(loginImplementation, /open_target|authorization_request|TcpListener/);
});

test('desktop mobile registration confirms matching passwords before network requests', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  assert.match(popover, /const \[passwordConfirmation, setPasswordConfirmation\] = useState\(''\)/);
  assert.match(popover, /if \(password !== passwordConfirmation\) return copy\.passwordMismatch/);
  assert.match(popover, /registrationInputError\(copy, account, mobile, password, passwordConfirmation\)/);
  assert.match(popover, /placeholder=\{copy\.confirmPassword\}[\s\S]*?value=\{passwordConfirmation\}|value=\{passwordConfirmation\}[\s\S]*?placeholder=\{copy\.confirmPassword\}/);
  assert.match(popover, /!passwordConfirmation \|\| password !== passwordConfirmation/);
  assert.equal(accountPlatformCopy.zh.mobileOrAccount, '手机号码或用户名');
  assert.equal(accountPlatformCopy.zh.password, '密码（至少 8 位，含字母和数字）');
  assert.equal(accountPlatformCopy.zh.confirmPassword, '再次输入密码');
  assert.equal(accountPlatformCopy.zh.passwordMismatch, '两次输入的密码不一致。');
});

test('desktop retains forgotten password while profile changes are Web-only', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const client = await readFile(new URL('../src/features/account-platform/desktop-client.js', import.meta.url), 'utf8');
  const backend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const webView = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const webClient = await readFile(new URL('../src/features/account-platform/bff-client.js', import.meta.url), 'utf8');
  const portal = await readFile(new URL('../../account-platform/internal/portal/service.go', import.meta.url), 'utf8');
  const accountRoutes = await readFile(new URL('../../account-platform/internal/account/service.go', import.meta.url), 'utf8');
  const accountProfile = await readFile(new URL('../../account-platform/internal/account/profile_changes.go', import.meta.url), 'utf8');
  assert.match(popover, /'password', 'sms', 'register', 'forgot'/);
  assert.match(popover, /await client\.resetPassword\(mobile\.trim\(\), smsChallenge, smsCode\.trim\(\), password\)/);
  assert.doesNotMatch(popover, /profileSettings|profileChange|PROFILE_MOBILE/);
  assert.match(client, /resetPassword\(mobile, challengeId, code, password/);
  assert.doesNotMatch(client, /ProfileChange|profile-change-requests/);
  assert.match(backend, /PasswordReset|reset_password|password-resets|PASSWORD_RESET/);
  assert.doesNotMatch(backend, /profile-change-requests|PROFILE_MOBILE/);
  assert.doesNotMatch(client, /password-resets/);
  assert.match(webView, /section === 'profile'/);
  assert.match(webView, /client\.createProfileChange\('USERNAME'/);
  assert.match(webView, /client\.sendProfileMobileChallenge/);
  assert.match(webClient, /reauthenticate=1/);
  assert.match(webClient, /section=accountSettings/);
  assert.match(webClient, /profile-change-requests/);
  assert.match(portal, /query\.Set\("prompt", "login"\)/);
  assert.match(portal, /query\.Set\("max_age", "0"\)/);
  assert.match(accountRoutes, /POST\("\/v1\/auth\/password-resets"/);
  assert.match(accountRoutes, /POST\("\/v1\/me\/profile-change-requests"/);
  assert.match(accountProfile, /case "USERNAME":/);
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(accountPlatformCopy[language].authModes.forgot);
    assert.ok(accountPlatformCopy[language].profileSettings);
    assert.ok(accountPlatformCopy[language].changeUsername);
    assert.ok(accountPlatformCopy[language].changeMobile);
  }
});

test('slider verification overlays the login form without changing its layout height', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const loginForm = popover.match(/<form className=\{`space-y-3[\s\S]*?<\/form>/)?.[0] || '';
  assert.doesNotMatch(loginForm, /<SliderCaptcha/);
  assert.match(popover, /role="dialog" aria-modal="true"[\s\S]*?absolute inset-0 z-20/);
  assert.match(popover, /onClose=\{\(\) => \{ if \(busy\) return; setCaptcha\(null\); setCaptchaOffset\(0\); \}\}/);
  assert.match(popover, /if \(captcha && !busy\)[\s\S]*?setCaptcha\(null\)/);
  assert.equal(accountPlatformCopy.zh.captchaClose, '关闭验证');
});

test('desktop account popover does not expose the internal user id', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  assert.doesNotMatch(popover, /me\?\.user_id/);
});

test('desktop account popover only loads the user and balance summary', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  assert.doesNotMatch(popover, /client\.organizations/);
  assert.doesNotMatch(popover, /client\.managedTokens/);
  assert.doesNotMatch(popover, /client\.switchContext/);
  assert.doesNotMatch(popover, /copy\.activeContext/);
  assert.doesNotMatch(popover, /copy\.accountStatus/);
});

test('desktop recharge completes inside the account popover without opening a web page', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const backend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  assert.match(popover, /copy\.balance[\s\S]*copy\.recharge/);
  assert.match(popover, /client\.products\(controller\.signal\)/);
  assert.doesNotMatch(popover, /client\.recentRechargeOrder/);
  assert.match(popover, /const openRecharge = \(\) => \{\s*setOrder\(null\)/);
  assert.match(popover, /client\.createRecharge\(productId, channel, newIdempotencyKey\(\)\)/);
  assert.match(popover, /order\?\.qr_data/);
  assert.doesNotMatch(popover, /import\('qrcode'\)/);
  assert.match(popover, /client\.rechargeOrder\(order\.order_id\)/);
  assert.match(popover, /refreshedBalance = await client\.balance\(\)/);
  assert.match(popover, /copy\.paymentReceived/);
  assert.doesNotMatch(popover, /target=["']_blank["']|window\.open|open_target|href=\{order\.(?:payment_url|qr_content)\}/);
  assert.match(backend, /\("token", &Method::GET, \["v1", "recharge", "products"\], None\)/);
  assert.match(backend, /\("token", &Method::POST, \["v1", "recharge", "orders"\], None\)/);
  assert.match(backend, /\("token", &Method::GET, \["v1", "recharge", "orders", id\], None\)[\s\S]{0,500}?=> safe_id\(id\)/);
});

test('desktop account persists only refresh credentials and clears them on logout or auth failure', async () => {
  const backend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const credentialStore = await readFile(new URL('../src-tauri/src/platform/credential_store.rs', import.meta.url), 'utf8');
  assert.match(backend, /SystemCredentialStore/);
  assert.match(backend, /for_account_platform_refresh_token/);
  assert.match(backend, /self\.credentials\s*\.set/);
  assert.match(backend, /self\.credentials\s*\.delete/);
  assert.match(backend, /ensure_restored/);
  assert.match(backend, /self\.clear_authentication\(\)\.await\?/);
  assert.doesNotMatch(backend, /\.set\([^\n]*password/);
  assert.match(backend, /refresh_token: String/);
  assert.match(credentialStore, /pinvou3-account-platform-refresh-token/);
});

test('built-in desktop chat uses the in-memory OIDC session and Token Gateway route', async () => {
  const accountBackend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const runtimeProvider = await readFile(new URL('../src-tauri/src/features/account_platform/runtime_model.rs', import.meta.url), 'utf8');
  assert.match(accountBackend, /chat_runtime_authorization/);
  assert.match(accountBackend, /token_gateway_openai_base/);
  assert.match(accountBackend, /\("token", &Method::POST, \["v1", "chat", "completions"\], None\)/);
  assert.match(runtimeProvider, /chat_runtime_authorization\(\)/);
  assert.match(runtimeProvider, /model\.base_url = authorization\.base_url/);
  assert.match(runtimeProvider, /RuntimeModelCredential::api_key\([\s\S]*authorization\.access_token/);
});

test('personal center and desktop chat refresh the built-in runtime after account or catalog changes', async () => {
  const accountBackend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const commands = await readFile(new URL('../src-tauri/src/app/commands/account_platform.rs', import.meta.url), 'utf8');
  assert.match(accountBackend, /bootstrap_account_identity/);
  assert.match(accountBackend, /account_user_id: Option<String>/);
  assert.match(accountBackend, /oidc-user-\{account_user_id\}-session-\{generation\}/);
  assert.equal(
    (commands.match(/pool\.mark_model_updated\(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID\)/g) || []).length,
    4,
  );
});

test('legacy built-in model data has an idempotent client-only migration', async () => {
  const migration = await readFile(new URL('../src-tauri/src/features/account_platform/client_migration.rs', import.meta.url), 'utf8');
  const prefs = await readFile(new URL('../src-tauri/src/platform/prefs/mod.rs', import.meta.url), 'utf8');
  assert.match(migration, /sessions.*_session_models\.json/s);
  assert.match(migration, /scheduled-runs.*session-profiles\.json/s);
  assert.match(migration, /legacy-llmapi-bindings-v1\.json/);
  assert.match(migration, /MigrationState::CleanupPending/);
  assert.match(prefs, /migrate_account_platform_model_path/);
  assert.match(prefs, /LEGACY_BUILTIN_LLMAPI_MODEL_ID/);
  assert.match(prefs, /BUILTIN_ACCOUNT_PLATFORM_MODEL_ID/);
});

test('new desktop build has no legacy model runtime or compatibility switch', async () => {
  const lib = await readFile(new URL('../src-tauri/src/lib.rs', import.meta.url), 'utf8');
  const features = await readFile(new URL('../src-tauri/src/features/mod.rs', import.meta.url), 'utf8');
  const account = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  assert.doesNotMatch(lib, /commands::llmapi|features::llmapi_hub/);
  assert.doesNotMatch(features, /llmapi_hub/);
  assert.doesNotMatch(account, /PINVOU_ACCOUNT_PLATFORM_ENABLED|account_platform_disabled/);

  const allowedMigrationFiles = new Set([
    '/src-tauri/src/features/account_platform/client_migration.rs',
    '/src-tauri/src/platform/prefs/mod.rs',
    '/src-tauri/src/platform/prefs/model.rs',
  ]);
  for (const file of [
    ...await sourceFiles(new URL('../src/', import.meta.url)),
    ...await sourceFiles(new URL('../src-tauri/src/', import.meta.url)),
  ]) {
    const pathname = file.pathname.replace(/\\/g, '/');
    if ([...allowedMigrationFiles].some(suffix => pathname.endsWith(suffix))) continue;
    const source = await readFile(file, 'utf8');
    assert.doesNotMatch(source, /builtin_llmapi|llmapi_hub|get_llmapi|commands::llmapi|PINVOU_ACCOUNT_PLATFORM_ENABLED/, file.pathname);
  }
});

test('desktop account restoration starts with the app without blocking startup', async () => {
  const source = await readFile(new URL('../src/app/main.jsx', import.meta.url), 'utf8');
  assert.match(source, /accountPlatformClient\.session\(controller\.signal\)\.catch/);
});

test('admin console exposes organizations and users as independent primary tabs', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/admin-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(view, /section === 'organizations'/);
  assert.match(view, /section === 'users'/);
  assert.doesNotMatch(view, /section === 'accounts'/);
  assert.doesNotMatch(i18n, /账号与组织/);
  assert.match(i18n, /organizations:'组织', users:'用户'/);
  assert.match(view, /key:'username', label:copy\.username/);
  assert.match(i18n, /username:'用户名'/);
});

test('personal center keeps context on overview and cannot create organizations', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const styles = await readFile(new URL('../src/features/account-platform/account-platform.css', import.meta.url), 'utf8');
  const bffClient = await readFile(new URL('../src/features/account-platform/bff-client.js', import.meta.url), 'utf8');
  const desktopClient = await readFile(new URL('../src/features/account-platform/desktop-client.js', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/account-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(i18n, /title:'个人中心'/);
  assert.match(i18n, /activeContext:'当前身份', switchContext:'切换身份', personal:'个人账号'/);
  assert.doesNotMatch(i18n, /当前上下文|身份上下文/);
  assert.match(view, /function Overview[\s\S]*copy\.activeContext/);
  assert.equal((view.match(/copy\.activeContext/g) || []).length, 1);
  assert.match(view, /copy\.organizationIdentity\(activeOrganization\.name\)/);
  assert.doesNotMatch(view, /client\.createOrganization/);
  assert.doesNotMatch(bffClient, /createOrganization\(/);
  assert.doesNotMatch(desktopClient, /createOrganization\(/);
  assert.match(styles, /\.ap-sidebar-foot select \{[^}]*color: var\(--ap-primary\);[^}]*background: var\(--ap-component\);/);
});

test('Web personal center consolidates navigation while preserving context-sensitive capabilities', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/account-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(view, /organizations\.length > 0 \? \['organization'\] : \[\]/);
  assert.match(view, /'overview',[\s\S]*'credits',[\s\S]*'entitlements',[\s\S]*'usage'/);
  assert.match(view, /personalContext \? \['orders', 'invoice'\] : \[\]/);
  assert.match(view, /portal \? portalNavigationSections/);
  assert.match(view, /section === 'creditsUsage'[\s\S]*<PortalCreditsUsage/);
  assert.match(view, /section === 'ordersInvoices'[\s\S]*<PortalOrdersInvoices/);
  assert.match(view, /section === 'accountSettings'[\s\S]*<PortalAccountSettings/);
  assert.doesNotMatch(view, /section === 'wallet'/);
  assert.match(view, /personalContext \? <Wallet[\s\S]*: <ContextCredits/);
  assert.match(view, /activeOrganization && <Organization/);
  assert.match(view, /successfulOnly \? successfulPaymentOrders\(orders\) : orders/);
  assert.equal((i18n.match(/creditsTitle:/g) || []).length, 3);
  assert.equal((i18n.match(/entitlementsTitle:/g) || []).length, 3);
  assert.match(view, /!navigationSections\.includes\(section\)[\s\S]*setSection\(portal \? 'accountSettings' : 'overview'\)/);
  assert.match(view, /<nav>\{navigationSections\.map/);
});

test('personal center exposes only user-facing usage history instead of Token administration', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const bffClient = await readFile(new URL('../src/features/account-platform/bff-client.js', import.meta.url), 'utf8');
  const desktopClient = await readFile(new URL('../src/features/account-platform/desktop-client.js', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/account-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(view, /section === 'usage'/);
  assert.match(view, /client\.usageHistory\('', request\.signal\)/);
  assert.match(i18n, /usage:'用量记录'/);
  assert.doesNotMatch(view, /section === 'tokens'|client\.(managedTokens|eligibleTokens|credentials|rotateCredential|createTokenAccount)/);
  assert.doesNotMatch(i18n, /Token 管理|供应商凭据|模型消费准备状态|消费结算状态|可管理 Token/);
  assert.match(bffClient, /usageHistory\(cursor = '', signal\)/);
  assert.match(desktopClient, /usageHistory\(cursor = '', signal\)/);
  assert.doesNotMatch(`${bffClient}\n${desktopClient}`, /managedTokens|eligibleTokens|rotateCredential|createTokenAccount/);
});

test('account portal uses stable asset names with revalidated caching', async () => {
  const viteConfig = await readFile(new URL('../vite.config.js', import.meta.url), 'utf8');
  const portalService = await readFile(new URL('../../account-platform/internal/portal/service.go', import.meta.url), 'utf8');
  assert.match(viteConfig, /entryFileNames: 'assets\/\[name\]\.js'/);
  assert.match(viteConfig, /chunkFileNames: 'assets\/\[name\]\.js'/);
  assert.match(viteConfig, /assetFileNames: 'assets\/\[name\]\[extname\]'/);
  assert.match(portalService, /Header\(\)\.Set\("Cache-Control", "no-cache"\)/);
});

test('personal center uses the canonical pinvou.com account route end to end', async () => {
  const desktop = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const portal = await readFile(new URL('../../account-platform/internal/portal/static.go', import.meta.url), 'utf8');
  const nginx = await readFile(new URL('../../deploy/nginx-pinvou-domain.conf', import.meta.url), 'utf8');
  assert.match(desktop, /DEFAULT_PORTAL_URL: &str = "https:\/\/pinvou\.com\/account"/);
  assert.doesNotMatch(desktop, /join\("account\.html"\)/);
  assert.match(portal, /r\.URL\.Path == "\/account" \|\| r\.URL\.Path == "\/account\/"/);
  assert.match(nginx, /location = \/account \{/);
  assert.match(nginx, /location = \/account\/ \{\s*return 308 \/account;\s*\}/);
  assert.match(nginx, /account\\\.js\|account\\\.css\|account-platform-i18n\\\.js/);
  assert.doesNotMatch(nginx, /location \^~ \/assets\//);
});

test('personal center follows the restrained neutral visual system', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const styles = await readFile(new URL('../src/features/account-platform/account-platform.css', import.meta.url), 'utf8');
  assert.match(view, /className="ap-sidebar-brand"/);
  assert.match(view, /className="ap-user-summary"/);
  assert.match(view, /<NavIcon name=\{key\}/);
  assert.match(view, /<AccountProfileCard/);
  assert.match(view, /<IdentityDialog/);
  assert.match(view, /<ProfileChangeDialog/);
  assert.match(styles, /--ap-page: #f7f8fa/);
  assert.match(styles, /--ap-primary: #202124/);
  assert.match(styles, /--ap-accent: #1e8e3e/);
  assert.match(styles, /grid-template-columns: 250px minmax\(0, 1fr\)/);
  assert.match(styles, /max-width: 1000px/);
  assert.match(styles, /\.ap-profile-card \{[\s\S]*border-radius: 12px/);
  assert.match(styles, /\.ap-dialog-backdrop[\s\S]*rgba\(0, 0, 0, \.45\)/);
  assert.match(styles, /\.ap-product-grid button\.selected \{ color: var\(--ap-primary\);[\s\S]*background: #ececec/);
  assert.match(styles, /\.ap-sidebar nav button \{ color: rgba\(27, 26, 36, \.42\); \}/);
  assert.match(styles, /\.ap-sidebar nav button\.active \{ color: var\(--ap-primary\);/);
});

test('admin organization detail supports ownership transfer, rename, leaf deletion, and list search', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/admin-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(view, /client\.organizationMembers\(organization\.organization_id/);
  assert.match(view, /client\.transferOrganizationOwnership\(organization\.organization_id, target\.user_id\)/);
  assert.match(view, /copy\.accounts\.confirmOwnership/);
  assert.match(view, /copy\.accounts\.transferOwner/);
  assert.match(i18n, /transferOwner:'转让所有权'/);
  assert.match(view, /client\.renameOrganization\(organization\.organization_id, nextName\)/);
  assert.match(view, /client\.renameOrganizationUnit\(organization\.organization_id, unit\.unit_id, trimmed\)/);
  assert.match(view, /client\.deleteOrganizationUnit\(organization\.organization_id, unit\.unit_id\)/);
  assert.match(view, /unit\.children\.length > 0/);
  assert.match(view, /copy\.accounts\.organizationSearch/);
  assert.match(view, /editingOrganizationName \? <form/);
  assert.match(view, /setEditingOrganizationName\(true\)/);
});

test('payment capabilities isolate provider limitations and enable reviewed-order repair', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/admin-platform-i18n.js', import.meta.url), 'utf8');
  const provider = await readFile(new URL('../../account-platform/internal/platform/h3cpayment/provider.go', import.meta.url), 'utf8');
  assert.match(provider, /ProviderID: "h3c_mall"/);
  assert.match(provider, /OrderQuerySupported: true/);
  assert.match(provider, /ManualRepairSupported: true/);
  assert.match(view, /refundSupported = paymentCapabilities\.refund_supported === true/);
  assert.match(view, /reconciliationSupported = paymentCapabilities\.bill_supported === true/);
  assert.match(view, /reconciliationImportSupported = paymentCapabilities\.bill_file_import_supported === true/);
  assert.match(view, /repairSupported = paymentCapabilities\.manual_repair_supported === true/);
  assert.match(view, /client\.repairPaymentOrder\(item\.order_id, reason\)/);
  assert.doesNotMatch(view, /paymentCapabilities\.provider === 'h3c_mall'/);
  assert.match(view, /refundSupported && item\.status === 'ACTIVE'/);
  assert.match(view, /disabled=\{busy \|\| !repairSupported/);
  assert.match(view, /paymentCapabilities\.reconciliation_mode/);
  assert.match(i18n, /不支持的退款、主动关单、支付回调或日终账单对账操作已自动禁用/);
});

test('late-payment review operations expose filters, evidence, and audited commands', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const client = await readFile(new URL('../src/features/account-platform/admin-client.js', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/admin-platform-i18n.js', import.meta.url), 'utf8');
  for (const field of ['provider_id','owner_id','entered_from','entered_to','next_review_before','overdue']) assert.match(view, new RegExp(`lateFilter\\.${field}`));
  for (const method of ['claimLatePaymentReview','queryLatePaymentReview','scheduleLatePaymentReview','archiveLatePaymentReview','escalateProviderBlind']) assert.match(view, new RegExp(`client\\.${method}`));
  assert.match(view, /lateReviewDetail\.evidence/);
  assert.match(view, /lateReviewDetail\.events/);
  assert.match(client, /\/admin\/v1\/late-payment-reviews/);
  assert.match(i18n, /晚到支付复核/);
  assert.doesNotMatch(view, /paid_status|markPaid|手工.*已支付/);
});

test('personal plan history shows traceable package and payment identifiers', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  for (const field of ['package_id', 'pinvou_order_id', 'provider_order_reference', 'sku_code', 'credited_at', 'valid_until', 'invoice_status']) {
    assert.match(view, new RegExp(`item\\.${field}`));
  }
  assert.match(view, /points_total_microcredits/);
  assert.match(view, /points_used_microcredits/);
  assert.match(view, /points_remaining_microcredits/);
});

test('billing operators can search repair candidates without provider-specific UI branches', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const client = await readFile(new URL('../src/features/account-platform/admin-client.js', import.meta.url), 'utf8');
  assert.match(view, /orderFilter/);
  assert.match(view, /mobile_masked/);
  assert.match(view, /primary_provider_transaction_id/);
  assert.match(client, /query\.set\('q', filters\.q\)/);
  assert.match(client, /query\.set\('status', filters\.status\)/);
  assert.doesNotMatch(view, /h3c|新华三/i);
});

test('billing order readers tolerate provider fields that are null before the first successful sync', async () => {
  const payments = await readFile(new URL('../../account-platform/internal/billing/payments.go', import.meta.url), 'utf8');
  const refunds = await readFile(new URL('../../account-platform/internal/billing/refunds.go', import.meta.url), 'utf8');
  const reconciliation = await readFile(new URL('../../account-platform/internal/billing/reconciliation.go', import.meta.url), 'utf8');
  assert.match(payments, /COALESCE\(payment_url,''\),COALESCE\(qr_data,''\)/);
  assert.equal((refunds.match(/COALESCE\(payment_url,''\),COALESCE\(qr_data,''\)/g) || []).length, 2);
  assert.match(reconciliation, /COALESCE\(payment_url,''\),primary_provider_transaction_id/);
});
