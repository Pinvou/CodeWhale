import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const browserPath = [
  process.env.CHROME,
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium or Edge is required');

const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const roles = [
  { code: 'PERSONAL_USER', scope: 'PERSONAL', assignable: false, description: 'Default personal capabilities.', permissions: ['personal.profile.read'] },
  { code: 'PLATFORM_BILLING_OPERATOR', scope: 'PLATFORM', assignable: true, description: 'Platform billing operations.', permissions: ['platform.billing.wallets.read', 'platform.usage.read'] },
  { code: 'ORG_ADMIN', scope: 'ORGANIZATION', assignable: true, description: 'Organization administration.', permissions: ['org.members.read', 'org.roles.assign'] },
  { code: 'UNIT_ADMIN', scope: 'UNIT', assignable: true, description: 'Unit subtree administration.', permissions: ['org.members.read'] },
];
const users = [{ user_id: '11111111-1111-4111-8111-111111111111', user_code: 'USR-TEST-001', username: 'vinpay', display_name: '王敏', status: 'ACTIVE' }];
const organizations = [{ organization_id: '22222222-2222-4222-8222-222222222222', name: 'Pinvou 研发团队', status: 'ACTIVE' }];
let bindings = [{ binding_id: '44444444-4444-4444-8444-444444444444', user_id: users[0].user_id, role_code: 'PLATFORM_BILLING_OPERATOR', scope: 'PLATFORM', starts_at: '2026-09-01T10:00:00Z', status: 'ACTIVE', grant_source: 'ADMIN', created_at: '2026-09-01T10:00:00Z' }];

function json(response, payload, status = 200) {
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'test-csrf' });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: users[0].user_id, platform_permissions: ['platform.roles.read', 'platform.roles.assign', 'platform.users.read', 'platform.organizations.read', 'platform.audit.read'] });
  if (url.pathname === '/api/admin/account/admin/v1/roles') return json(response, { items: roles });
  if (url.pathname === '/api/admin/account/admin/v1/users') return json(response, { items: users });
  if (url.pathname === '/api/admin/account/admin/v1/organizations') return json(response, { items: organizations });
  if (url.pathname.endsWith('/units')) return json(response, { items: [{ unit_id: '33333333-3333-4333-8333-333333333333', name: '应用组', status: 'ACTIVE' }] });
  if (url.pathname === '/api/admin/account/admin/v1/role-bindings' && request.method === 'GET') return json(response, { items: bindings });
  if (url.pathname === '/api/admin/account/admin/v1/role-bindings' && request.method === 'POST') {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    const created = { binding_id: '55555555-5555-4555-8555-555555555555', ...body, scope: body.organization_id ? (body.unit_id ? 'UNIT' : 'ORGANIZATION') : 'PLATFORM', starts_at: body.starts_at || new Date().toISOString(), status: 'ACTIVE', grant_source: 'ADMIN', created_at: new Date().toISOString() };
    bindings = [...bindings, created];
    return json(response, created, 201);
  }
  if (url.pathname.startsWith('/api/admin/account/admin/v1/role-bindings/') && request.method === 'DELETE') return json(response, { ...bindings[0], status: 'REVOKED' });
  if (url.pathname === '/api/admin/account/admin/v1/audit-logs') return json(response, { items: [] });
  const relative = url.pathname === '/admin.html' ? 'admin.html' : url.pathname.replace(/^\//, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot) || !existsSync(target)) { response.writeHead(404); return response.end('not found'); }
  const type = path.extname(target) === '.html' ? 'text/html' : path.extname(target) === '.css' ? 'text/css' : 'text/javascript';
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
  await page.goto(`http://127.0.0.1:${port}/admin.html?section=roles`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-role-workspace');
  assert.equal(await page.$eval('.adm-topbar h1', (node) => node.textContent.trim()), '角色与权限');
  assert.deepEqual(await page.$$eval('.adm-role-summary strong', (nodes) => nodes.map((node) => node.textContent.trim())), ['1', '1', '2']);
  assert.equal(await page.$eval('.adm-role-detail code', (node) => node.textContent.trim()), 'PERSONAL_USER');
  assert.equal(await page.$('#role-bindings-root'), null);
  await page.screenshot({ path: path.join(process.env.TEMP || process.cwd(), 'pinvou-admin-roles-catalog-smoke.png'), fullPage: true });
  await page.click('.adm-role-workspace-head .adm-role-primary');
  await page.waitForSelector('.adm-role-drawer');
  assert.equal(await page.$eval('.adm-role-steps li.active', (node) => node.textContent.trim().includes('选择用户')), true);
  await page.screenshot({ path: path.join(process.env.TEMP || process.cwd(), 'pinvou-admin-roles-grant-smoke.png'), fullPage: true });
  assert.equal(await page.$eval('.adm-role-drawer-head > button', (node) => node.textContent.trim()), '×');
  await page.$eval('.adm-role-drawer-head > button', (node) => node.click());
  await page.waitForSelector('.adm-role-drawer', { hidden: true });
  const tabs = await page.$$('.adm-role-workspace-head [role="tab"]');
  assert.equal(tabs.length, 2);
  await tabs[1].click();
  assert.equal(await page.$eval('.adm-role-binding-table tbody strong', (node) => node.textContent.trim()), '王敏');
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  assert.ok(overflow <= 1, `desktop page must not overflow horizontally: ${overflow}`);
  await page.screenshot({ path: path.join(process.env.TEMP || process.cwd(), 'pinvou-admin-roles-smoke.png'), fullPage: true });

  await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 1 });
  await page.goto(`http://127.0.0.1:${port}/admin.html?section=roles`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-role-workspace');
  const mobileOverflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  assert.ok(mobileOverflow <= 1, `mobile page must not overflow horizontally: ${mobileOverflow}`);
  await page.screenshot({ path: path.join(process.env.TEMP || process.cwd(), 'pinvou-admin-roles-mobile-smoke.png'), fullPage: true });
  console.log('Admin roles UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
