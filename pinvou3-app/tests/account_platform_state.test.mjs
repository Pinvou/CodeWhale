import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import { createContextRequestGuard, eligibleForContext, rechargePollDelay } from '../src/features/account-platform/context-guard.js';
import { formatPoints, MICROCREDITS_PER_POINT, microcreditsToPoints } from '../src/features/account-platform/credit-units.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { buildUnitTree } from '../src/features/account-platform/admin-organization-tree.js';

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
    assert.equal(calls[0].url, '/portal/api/token/v1/me/usage-requests?limit=50');
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
  assert.match(popover, /autoComplete="current-password"/);
  assert.match(popover, /await client\.login\(account\.trim\(\), password\)/);
  assert.match(bridge, /invoke\("account_platform_login", \{ credentials: credentials \}\)/);
  assert.doesNotMatch(backend, /open_target|authorization_request|TcpListener/);
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

test('personal center exposes only user-facing usage history instead of Token administration', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const bffClient = await readFile(new URL('../src/features/account-platform/bff-client.js', import.meta.url), 'utf8');
  const desktopClient = await readFile(new URL('../src/features/account-platform/desktop-client.js', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/account-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(view, /section === 'usage'/);
  assert.match(view, /client\.usageHistory\(request\.signal\)/);
  assert.match(i18n, /usage:'用量记录'/);
  assert.doesNotMatch(view, /section === 'tokens'|client\.(managedTokens|eligibleTokens|credentials|rotateCredential|createTokenAccount)/);
  assert.doesNotMatch(i18n, /Token 管理|供应商凭据|模型消费准备状态|消费结算状态|可管理 Token/);
  assert.match(bffClient, /usageHistory\(signal\)/);
  assert.match(desktopClient, /usageHistory\(signal\)/);
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
