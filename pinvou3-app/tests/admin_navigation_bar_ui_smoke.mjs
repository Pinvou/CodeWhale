import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import path from 'node:path';

import puppeteer from 'puppeteer-core';

import { adminPlatformCopy } from '../src/shared/admin-platform-i18n.js';

const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
const styles = await readFile(new URL('../src/features/account-platform/admin-platform.css', import.meta.url), 'utf8');
assert.match(view, /function PageBreadcrumbs\(/u);
assert.match(view, /onClick=\{\(\) => onNavigate\(groupTarget\)\}/u);
assert.match(view, /onClick=\{\(\) => onNavigate\(section\)\}/u);
assert.match(view, /onBreadcrumbDetailChange\?\.\(breadcrumbRouteKey, breadcrumbRouteLabel\)/u);
assert.match(styles, /@media\(max-width:760px\)[\s\S]*\.adm-breadcrumbs ol\{overflow-x:auto/u);
for (const locale of ['zh', 'en', 'ja']) {
  assert.ok(adminPlatformCopy[locale].pageNavigation, `${locale} pageNavigation is required`);
  assert.ok(adminPlatformCopy[locale].detailPage, `${locale} detailPage is required`);
}

const browserPath = [
  process.env.CHROME,
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium or Edge is required');

const visibleUser = { user_id: '11111111-1111-4111-8111-111111111111', user_code: 'USR-VISIBLE-001', username: 'visible-user', display_name: 'Visible User', status: 'ACTIVE', organization_count: 0 };
const routedUser = { user_id: '22222222-2222-4222-8222-222222222222', user_code: 'USR-ROUTED-002', username: 'route-only-user', display_name: 'Route Only User', status: 'ACTIVE', organization_count: 0 };
const visibleOrganization = { organization_id: '33333333-3333-4333-8333-333333333333', name: 'Visible Organization', status: 'ACTIVE', member_count: 1 };
const routedOrganization = { organization_id: '44444444-4444-4444-8444-444444444444', name: 'Route Only Organization', status: 'ACTIVE', member_count: 1 };
const permissions = ['platform.users.read', 'platform.organizations.read'];
const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');

function json(response, payload, status = 200) {
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'navigation-test' });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: visibleUser.user_id, platform_permissions: permissions });
  if (url.pathname === '/api/admin/account/admin/v1/overview') return json(response, { users: 2, active_users: 2, disabled_users: 0, organizations: 2 });
  if (url.pathname === '/api/admin/account/admin/v1/users') {
    return json(response, { items: url.searchParams.get('q') === routedUser.user_id ? [routedUser] : [visibleUser] });
  }
  if (url.pathname === `/api/admin/account/admin/v1/users/${routedUser.user_id}/memberships`) return json(response, { items: [] });
  if (url.pathname === `/api/admin/account/admin/v1/users/${visibleUser.user_id}/memberships`) return json(response, { items: [] });
  if (url.pathname === '/api/admin/account/admin/v1/organizations') {
    return json(response, { items: url.searchParams.get('q') === routedOrganization.organization_id ? [routedOrganization] : [visibleOrganization] });
  }
  if (url.pathname.endsWith('/units')) return json(response, { items: [], structure_version: 1 });
  if (url.pathname.endsWith('/members')) return json(response, { items: [] });
  if (url.pathname.endsWith('/role-bindings')) return json(response, { items: [] });
  const relative = url.pathname === '/admin.html' ? 'admin.html' : url.pathname.replace(/^\//u, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot) || !existsSync(target)) {
    response.writeHead(404);
    return response.end('not found');
  }
  const extension = path.extname(target);
  const type = extension === '.html' ? 'text/html' : extension === '.css' ? 'text/css' : 'text/javascript';
  response.writeHead(200, { 'Content-Type': `${type}; charset=utf-8` });
  response.end(await readFile(target));
});

await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
const { port } = server.address();
const browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
try {
  const page = await browser.newPage();
  page.on('pageerror', (error) => console.error('PAGE_ERROR', error.message));
  await page.setViewport({ width: 1440, height: 1000, deviceScaleFactor: 1 });
  await page.evaluateOnNewDocument(() => localStorage.setItem('pinvou.admin.language', 'zh'));

  await page.goto(`http://127.0.0.1:${port}/admin.html?section=organizations&organization_id=${routedOrganization.organization_id}&organization_tab=overview&organization_q=route&organization_status=ACTIVE&organization_cursor=next-page`, { waitUntil: 'networkidle0' });
  await page.waitForFunction((label) => [...document.querySelectorAll('.adm-breadcrumbs [aria-current="page"]')].some((node) => node.textContent.trim() === label), {}, routedOrganization.name);
  assert.deepEqual(await page.$$eval('.adm-breadcrumbs li:not(.adm-breadcrumb-separator)', (nodes) => nodes.map((node) => node.textContent.trim())), ['身份与组织', '组织管理', routedOrganization.name]);
  assert.equal(await page.$eval('.adm-topbar h1', (node) => node.textContent.trim()), '平台管理台');
  assert.ok((await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)) <= 1, 'desktop page must not overflow horizontally');

  await page.click('.adm-breadcrumbs button');
  await page.waitForFunction(() => {
    const search = new URL(location.href).searchParams;
    return search.get('section') === 'users' && ['organization_id', 'organization_tab', 'organization_q', 'organization_status', 'organization_cursor'].every((key) => !search.has(key));
  });
  assert.equal(await page.$eval('.adm-breadcrumbs [aria-current="page"]', (node) => node.textContent.trim()), '用户管理');

  await page.goto(`http://127.0.0.1:${port}/admin.html?section=users&user_id=${routedUser.user_id}`, { waitUntil: 'networkidle0' });
  await page.waitForFunction((label) => document.querySelector('.adm-breadcrumbs [aria-current="page"]')?.textContent.trim() === label, {}, routedUser.username);
  assert.deepEqual(await page.$$eval('.adm-breadcrumbs li:not(.adm-breadcrumb-separator)', (nodes) => nodes.map((node) => node.textContent.trim())), ['身份与组织', '用户管理', routedUser.username]);
  await page.click('.adm-breadcrumbs button');
  await page.waitForFunction(() => !new URL(location.href).searchParams.has('user_id'));
  await page.goBack({ waitUntil: 'networkidle0' });
  await page.waitForFunction((id, label) => new URL(location.href).searchParams.get('user_id') === id && document.querySelector('.adm-breadcrumbs [aria-current="page"]')?.textContent.trim() === label, {}, routedUser.user_id, routedUser.username);

  for (const [locale, label] of [['en', 'Current page location'], ['ja', '現在のページ位置']]) {
    const localePage = await browser.newPage();
    await localePage.evaluateOnNewDocument((value) => localStorage.setItem('pinvou.admin.language', value), locale);
    await localePage.goto(`http://127.0.0.1:${port}/admin.html?section=users`, { waitUntil: 'networkidle0' });
    assert.equal(await localePage.$eval('.adm-breadcrumbs', (node) => node.getAttribute('aria-label')), label);
    await localePage.close();
  }

  await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 1 });
  await page.reload({ waitUntil: 'networkidle0' });
  const mobileOverflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  assert.ok(mobileOverflow <= 1, `mobile page must not overflow horizontally: ${mobileOverflow}`);
  console.log('Admin navigation bar UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
