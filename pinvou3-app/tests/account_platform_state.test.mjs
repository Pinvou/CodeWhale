import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import { createContextRequestGuard, eligibleForContext, rechargePollDelay } from '../src/features/account-platform/context-guard.js';
import { formatPoints, MICROCREDITS_PER_POINT, microcreditsToPoints } from '../src/features/account-platform/credit-units.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { buildUnitTree } from '../src/features/account-platform/admin-organization-tree.js';
import { accountPlatformCopy } from '../src/shared/account-platform-i18n.js';

test('account balances convert internal microcredits to display points', () => {
  assert.equal(MICROCREDITS_PER_POINT, 10_000);
  assert.equal(microcreditsToPoints(1_000_000), 100);
  assert.equal(formatPoints(1_000_000, 'en'), '100');
  assert.equal(formatPoints(7_969_994, 'en'), '796.9994');
  assert.equal(formatPoints(1, 'en'), '0.0001');
  assert.equal(formatPoints(0, 'en'), '0');
  assert.equal(formatPoints(null, 'en'), '—');
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
    await client.switchContext('PERSONAL', null);
    assert.equal(calls[1].options.headers['X-CSRF-Token'], 'c'.repeat(32));
    assert.equal(calls[1].options.credentials, 'same-origin');
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
  assert.doesNotMatch(backend, /open_target|authorization_request|TcpListener/);
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
  assert.match(backend, /\("token", &Method::GET, \["v1", "recharge", "orders", id\], None\) => safe_id\(id\)/);
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
  const runtimeProvider = await readFile(new URL('../src-tauri/src/features/llmapi_hub/runtime_model.rs', import.meta.url), 'utf8');
  assert.match(accountBackend, /chat_runtime_authorization/);
  assert.match(accountBackend, /token_gateway_openai_base/);
  assert.match(accountBackend, /\("token", &Method::POST, \["v1", "chat", "completions"\], None\)/);
  assert.match(runtimeProvider, /chat_runtime_authorization\(\)/);
  assert.match(runtimeProvider, /model\.base_url = authorization\.base_url/);
  assert.match(runtimeProvider, /RuntimeModelCredential::api_key\([\s\S]*authorization\.access_token/);
});

test('personal center and desktop chat share one account identity lifecycle', async () => {
  const accountBackend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const commands = await readFile(new URL('../src-tauri/src/app/commands/account_platform.rs', import.meta.url), 'utf8');
  assert.match(accountBackend, /bootstrap_account_identity/);
  assert.match(accountBackend, /account_user_id: Option<String>/);
  assert.match(accountBackend, /oidc-user-\{account_user_id\}-session-\{generation\}/);
  assert.equal(
    (commands.match(/pool\.mark_model_updated\(BUILTIN_LLMAPI_MODEL_ID\)/g) || []).length,
    3,
  );
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

test('personal center hides the organization tab when the user has no organizations', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  assert.match(view, /organizations\.length > 0 \? \['organization'\] : \[\]/);
  assert.match(view, /personalContext \? \['wallet', 'orders', 'invoice'\] : \[\]/);
  assert.match(view, /!navigationSections\.includes\(section\)[\s\S]*setSection\('overview'\)/);
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

test('personal center follows the restrained WorkBuddy-inspired visual system', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const styles = await readFile(new URL('../src/features/account-platform/account-platform.css', import.meta.url), 'utf8');
  assert.match(view, /className="ap-sidebar-brand"/);
  assert.match(view, /className="ap-user-summary"/);
  assert.match(view, /<NavIcon name=\{key\}/);
  assert.match(styles, /--ap-page: #f5f7fa/);
  assert.match(styles, /--ap-primary: #1b1a24/);
  assert.match(styles, /--ap-accent: #4ecba0/);
  assert.match(styles, /grid-template-columns: 250px minmax\(0, 1fr\)/);
  assert.match(styles, /max-width: 1000px/);
  assert.match(styles, /border-radius: 24px/);
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
  const provider = await readFile(new URL('../../account-platform/internal/billing/h3c_mall.go', import.meta.url), 'utf8');
  assert.match(provider, /Provider: "h3c_mall", OrderSyncSupported: true, ManualRepairSupported: true/);
  assert.match(view, /refundSupported = paymentCapabilities\.refund_supported === true/);
  assert.match(view, /reconciliationSupported = paymentCapabilities\.reconciliation_supported === true/);
  assert.match(view, /repairSupported = paymentCapabilities\.manual_repair_supported === true/);
  assert.match(view, /client\.repairPaymentOrder\(item\.order_id, reason\)/);
  assert.doesNotMatch(view, /paymentCapabilities\.provider === 'h3c_mall'/);
  assert.match(view, /disabled=\{busy \|\| !refundSupported/);
  assert.match(view, /disabled=\{busy \|\| !repairSupported/);
  assert.match(view, /disabled=\{busy \|\| !reconciliationSupported\}/);
  assert.match(i18n, /不支持的退款、主动关单、支付回调或日终账单对账操作已自动禁用/);
});

test('personal plan history shows traceable package and payment identifiers', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  for (const field of ['package_id', 'pinvou_order_id', 'mall_order_id', 'sku_code', 'credited_at', 'valid_until', 'invoice_status']) {
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
  assert.match(view, /channel_transaction_id/);
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
  assert.match(reconciliation, /COALESCE\(payment_url,''\),channel_transaction_id/);
});
