import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';

const read = (path) => readFile(new URL(path, import.meta.url), 'utf8');

test('approved admin UI requirements and task ownership remain traceable without adding business behavior', async () => {
  const [requirements, design, tasks, status] = await Promise.all([
    read('../../docs/requirements.md'),
    read('../../docs/admin-portal-ui-redesign.md'),
    read('../../docs/implementation-tasks.md'),
    read('../../docs/implementation-status.md'),
  ]);

  for (const id of ['UI-28', 'UI-29', 'UI-30', 'UI-31', 'UI-32']) {
    assert.equal((requirements.match(new RegExp(`\\| ${id} \\|`, 'g')) || []).length, 1, `${id} has one formal requirement row`);
  }
  for (const task of ['APT-094', 'APT-095', 'APT-096', 'APT-097', 'APT-098']) {
    assert.equal((tasks.match(new RegExp(`^\\| ${task} \\|`, 'gm')) || []).length, 1, `${task} has one owner row`);
  }
  assert.match(design, /不批准任何新的业务能力、权限、状态迁移或服务端接口/);
  assert.match(design, /`UI-32`[\s\S]*不修改产品代码/);
  assert.match(design, /admin-portal-ui-prototype\.html.*第一优先级/);
  assert.match(design, /不得退回旧版管理台配色或仅移动原有 Tab/);
  assert.match(status, /\| UI-28 \| 已实现/);
  assert.match(status, /\| UI-31 \| 已实现/);
});

test('source and built admin portal preserve the HTML prototype five-group navigation and capability boundaries', async () => {
  const [view, authorization, navigation, i18n, prototype, sourceCSS, builtHTML, builtJS, builtCSS] = await Promise.all([
    read('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../../../pinvou3-app/src/features/account-platform/authorization-ui.js'),
    read('../../../pinvou3-app/src/features/account-platform/admin-navigation.js'),
    read('../../../pinvou3-app/src/shared/admin-platform-i18n.js'),
    read('../../docs/admin-portal-ui-prototype.html'),
    read('../../../pinvou3-app/src/features/account-platform/admin-platform.css'),
    read('../../internal/portal/static/admin.html'),
    read('../../internal/portal/static/assets/admin.js'),
    read('../../internal/portal/static/assets/admin.css'),
  ]);

  for (const group of ['workbench', 'identity', 'finance', 'models', 'governance']) {
    assert.match(navigation, new RegExp(`key: '${group}'`));
    assert.ok(builtJS.includes(group));
  }
  assert.doesNotMatch(navigation, /key: 'operations'/);
  const exactSections = ['overview', 'users', 'roles', 'organizations', 'myorg', 'billing', 'payments', 'products', 'tokens', 'approvals', 'audit', 'system', 'settings'];
  for (const section of exactSections) assert.ok(navigation.includes(`'${section}'`), `${section} is part of the canonical information architecture`);
  for (const label of ['工作台', '平台总览', '身份与组织', '用户管理', '角色与授权', '组织管理', '我的组织', '财务与支付', '计费运营', '支付与资金', '定价与商品', '模型与 Token', 'Token Gateway', '治理与安全', '审批中心', '审计与导出', '系统与安全', '偏好设置']) {
    assert.ok(prototype.includes(label), `${label} is defined by the HTML prototype`);
    assert.ok(i18n.includes(label), `${label} is preserved by source translations`);
    assert.ok(builtJS.includes(label), `${label} is preserved by the built portal`);
  }
  assert.match(navigation, /URLSearchParams/);
  assert.match(navigation, /historyLike\.pushState/);
  assert.match(navigation, /historyLike\.replaceState/);
  assert.match(authorization, /platform\.tokens\.read/);
  assert.match(view, /'pricing-admin-root': false/);
  assert.match(view, /'approval-requests-root': section === 'approvals'/);
  assert.match(view, /'product-admin-root': section === 'products'/);
  assert.match(view, /surface="payments"/);
  assert.match(view, /function SettingsWorkspace/);
  assert.match(view, /copy\.runbooks\.noExecution/);
  assert.doesNotMatch(view, /createUser\(|createRole\(|eventReplay\(/);
  for (const root of ['approval-requests-root', 'pricing-admin-root', 'product-admin-root', 'registration-repair-root', 'role-bindings-root']) {
    assert.ok(builtHTML.includes(`id="${root}"`), `${root} is retained for permission-gated mounting`);
    assert.match(sourceCSS, new RegExp(`\\.admin-portal-body\\s*>\\s*#${root}`));
    assert.ok(builtCSS.includes(`#${root}`));
  }
});

test('admin portal follows the approved HTML visual system and business-page composition', async () => {
  const [view, i18n, sourceCSS, builtJS, builtCSS] = await Promise.all([
    read('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../../../pinvou3-app/src/shared/admin-platform-i18n.js'),
    read('../../../pinvou3-app/src/features/account-platform/admin-platform.css'),
    read('../../internal/portal/static/assets/admin.js'),
    read('../../internal/portal/static/assets/admin.css'),
  ]);

  assert.equal((i18n.match(/runbooks: \{/g) || []).length, 3);
  assert.equal((i18n.match(/copyIdentifier:/g) || []).length, 3);
  for (const translation of ['只读运行手册', 'Read-only runbooks', '読み取り専用ランブック']) {
    assert.ok(i18n.includes(translation));
    assert.ok(builtJS.includes(translation));
  }
  for (const evidence of [
    /className="adm-skip-link"/,
    /aria-label=\{copy\.primaryNavigation\}/,
    /role="tablist"/,
    /aria-selected=/,
    /aria-current=/,
    /role="progressbar"/,
    /role="alert"/,
    /role="status"/,
  ]) assert.match(view, evidence);
  for (const visualToken of [
    '--adm-primary:#3159e8',
    '--adm-primary-soft:#edf1ff',
    '--adm-sidebar:272px',
    '--adm-topbar:72px',
    '--adm-radius:16px',
  ]) assert.ok(sourceCSS.includes(visualToken), `${visualToken} matches the approved HTML prototype`);
  for (const legacyToken of ['#148f84', '#18a999', '#14242b', '#1d3038']) {
    assert.ok(!sourceCSS.toLowerCase().includes(legacyToken), `${legacyToken} legacy visual token is removed`);
  }
  assert.match(view, /function PageHeader\(/);
  assert.match(view, /function DetailDrawer\(/);
  assert.match(view, /className="adm-page"/);
  assert.match(view, /className="adm-page-head"/);
  assert.match(view, /className="adm-drawer-backdrop"/);
  assert.match(view, /role="dialog" aria-modal="true"/);
  assert.match(view, /event\.key === 'Escape'/);
  assert.match(view, /previousFocus\?\.focus\?\.\(\)/);
  assert.match(view, /<DetailDrawer label=\{`\$\{copy\.tokens\.accountDetail\}/);
  assert.match(view, /<DetailDrawer label=\{copy\.audit\.detail\}/);
  assert.match(view, /currentGroup = visibleGroups\.find/);
  assert.doesNotMatch(view, /className="adm-hero"/);
  assert.match(sourceCSS, /\.adm-sidebar\{[^}]*position:fixed[^}]*background:var\(--adm-surface\)[^}]*border-right:1px solid var\(--adm-line\)/);
  assert.match(sourceCSS, /\.adm-topbar\{[^}]*position:fixed[^}]*height:var\(--adm-topbar\)/);
  assert.match(sourceCSS, /\.adm-page\{[^}]*max-width:1480px/);
  assert.match(sourceCSS, /\.adm-metrics\{[^}]*grid-template-columns:repeat\(4/);
  assert.match(sourceCSS, /@media\s*\(max-width:\s*1100px\)/);
  assert.match(sourceCSS, /@media\s*\(max-width:\s*760px\)/);
  assert.match(sourceCSS, /@media\s*\(pointer:\s*coarse\)[\s\S]*min-height:44px/);
  assert.match(sourceCSS, /@media\s*\(prefers-reduced-motion:\s*reduce\)/);
  assert.match(sourceCSS, /\.adm-table-wrap\{[^}]*max-width:100%[^}]*overflow:auto/);
  assert.match(view, /className="adm-system-sections"[\s\S]*copy\.system\.services[\s\S]*copy\.system\.relays[\s\S]*copy\.system\.recentErrors/);
  assert.match(sourceCSS, /\.adm-system-sections\{[^}]*display:grid[^}]*gap:18px[^}]*margin-bottom:18px/);
  assert.match(sourceCSS, /\.adm-drawer-backdrop\{[^}]*position:fixed[^}]*backdrop-filter:blur\(3px\)/);
  assert.match(sourceCSS, /\.adm-detail-drawer\{[^}]*width:min\(620px,100vw\)[^}]*height:100vh/);
  assert.match(sourceCSS, /\.adm-extension-stack \.product-admin\{[^}]*background:var\(--adm-surface\)/);
  assert.match(sourceCSS, /:focus-visible/);
  assert.ok(builtCSS.includes('#3159e8'));
  assert.ok(builtCSS.includes('--adm-sidebar:272px'));
  assert.ok(builtJS.includes('adm-page-head'));
  assert.ok(builtCSS.includes('@media (width<=760px)'));
  assert.ok(builtCSS.includes('@media (pointer:coarse)'));
  assert.ok(builtCSS.includes('prefers-reduced-motion:reduce'));
  for (const state of ['loading', 'error', 'denied', 'guest']) assert.ok(view.includes(`'${state}'`) || view.includes(`"${state}"`));
  assert.match(i18n, /数据状态已经变化/);
  assert.match(i18n, /temporarily unavailable|temporarily unavailable|temporarily unavailable/i);
  assert.match(view, /window\.confirm/);
  assert.match(view, /window\.prompt/);
  assert.doesNotMatch(sourceCSS, /https?:\/\//);
});

test('platform overview uses permission-scoped sources, correct business metrics, and actionable queues', async () => {
  const [view, css, i18n, builtJS, accountService, billingService, tokenService, accountAdmin, billingAdmin, tokenAdmin, systemStatus, openapi, requirements, design] = await Promise.all([
    read('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../../../pinvou3-app/src/features/account-platform/admin-platform.css'),
    read('../../../pinvou3-app/src/shared/admin-platform-i18n.js'),
    read('../../internal/portal/static/assets/admin.js'),
    read('../../internal/account/service.go'),
    read('../../internal/billing/service.go'),
    read('../../internal/token/service.go'),
    read('../../internal/account/admin.go'),
    read('../../internal/billing/admin.go'),
    read('../../internal/token/admin.go'),
    read('../../internal/portal/system_status.go'),
    read('../../contracts/openapi/admin.yaml'),
    read('../../docs/requirements.md'),
    read('../../docs/admin-portal-ui-redesign.md'),
  ]);

  assert.match(view, /Promise\.allSettled/);
  for (const source of ["'identity'", "'finance'", "'modelToken'", "'system'"]) assert.ok(view.includes(source));
  assert.match(view, /posted_microcredits \|\| 0\) - Number\(b\.reserved_microcredits \|\| 0\) - Number\(b\.receivable_microcredits \|\| 0\)/);
  assert.match(view, /healthyRoutes \/ routeTotal/);
  assert.match(view, /className="adm-overview-domains"/);
  assert.match(view, /onNavigate\(item\.section\)/);
  assert.match(view, /section === 'settings' \|\| section === 'overview'/);
  assert.match(css, /\.adm-overview-domains\{[^}]*repeat\(3/);
  assert.doesNotMatch(css, /\.adm-overview-domain\.accent\{[^}]*gradient/);

  assert.match(accountService, /requireAnyPermission\(PermissionPlatformUsersRead, PermissionPlatformOrganizationsRead\)/);
  assert.match(billingService, /Require\("platform\.billing\.wallets\.read"\)/);
  assert.match(tokenService, /RequireAny\("platform\.tokens\.read", "platform\.usage\.read", "platform\.system\.status\.read"\)/);
  assert.match(accountAdmin, /GrantedPermission\(r\.Context\(\), PermissionPlatformUsersRead\)/);
  assert.match(accountAdmin, /GrantedPermission\(r\.Context\(\), PermissionPlatformOrganizationsRead\)/);
  assert.match(billingAdmin, /receivable_microcredits/);
  assert.match(tokenAdmin, /SELECT count\(\*\) FROM routing_policies\)/);
  assert.match(tokenAdmin, /response\["routes"\] = result\.Routes/);
  assert.match(systemStatus, /\/v1\/account\/bootstrap/);
  assert.match(systemStatus, /permission == "platform\.system\.status\.read"/);
  assert.doesNotMatch(systemStatus, /targets := \[\]\*url\.URL/);

  for (const contract of ['AccountOverview:', 'BillingOverview:', 'TokenOverview:', 'receivable_microcredits:', 'routes:']) assert.ok(openapi.includes(contract));
  for (const translation of ['身份与组织', 'Funds & billing', 'システム正常性', '数据暂不可用']) {
    assert.ok(i18n.includes(translation));
    assert.ok(builtJS.includes(translation));
  }
  assert.match(requirements, /平台总览采用身份与组织、资金与计费、模型与 Token 三张领域摘要卡/);
  assert.match(design, /healthy_routes \/ routes/);
});
