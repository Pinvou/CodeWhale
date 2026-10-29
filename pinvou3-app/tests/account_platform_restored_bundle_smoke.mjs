import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import http from 'node:http';
import * as puppeteer from 'puppeteer-core';
import { accountPlatformCopy, organizationCopy } from '../src/shared/i18n.js';

const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const chrome = [process.env.CHROME, 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(value => value && fs.existsSync(value));
assert.ok(chrome, 'Chromium required; built-entry verification cannot be skipped');
const id = '11111111-1111-4111-8111-111111111111', orgID = '33333333-3333-4333-8333-333333333333';
const calls = [];
const me = { user_id: id, username: 'sample-account', mobile: '+8613800000000', status: 'ACTIVE', personal_permissions: ['personal.billing.read', 'personal.profile.update'], platform_permissions: ['platform.users.read', 'platform.organizations.read', 'platform.organizations.create'], active_context: { context_type: 'PERSONAL', context_version: 1, permissions: [] } };
const server = http.createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  const send = (value, status = 200) => { response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); response.end(JSON.stringify(value)); };
  if (url.pathname.startsWith('/api/')) {
    calls.push({ method: request.method, path: url.pathname });
    if (url.pathname === '/api/session') return send({ authenticated: true, csrf_token: 'CONTROL_SYNTHETIC_CSRF', feature_flags: {} });
    if (url.pathname === '/api/account/v1/account/bootstrap' && request.method === 'POST') {
      if (request.headers['x-csrf-token'] !== 'CONTROL_SYNTHETIC_CSRF') return send({ code: 'CSRF_FAILED' }, 403);
      return send(me);
    }
    if (request.method !== 'GET') return send({ code: 'CONTROLLED_WRITE_NOT_ALLOWED' }, 501);
    if (url.pathname === '/api/token/v1/me/balance') { response.writeHead(200, { 'Content-Type': 'application/json' }); return response.end('{"available_microcredits":9223372036854775807,"reserved_microcredits":10000}'); }
    if (url.pathname === '/api/token/v1/recharge/products') return send({ items: [{ product_id: 'sample-points', current_plan_name_zh_cn: '标准积分包', current_plan_name_en: 'Standard points', current_plan_name_ja: '標準ポイント', amount_fen: 990, channels: ['ALIPAY'] }] });
    if (url.pathname === '/api/token/v1/payment-capabilities') return send({});
    if (['/api/token/v1/recharge/orders', '/api/token/v1/me/entitlement-packages', '/api/account/v1/me/organizations'].includes(url.pathname)) return send({ items: [] });
    if (url.pathname === '/api/admin/account/admin/v1/users') return send({ items: [{ user_id: id, username: 'sample-account', user_code: 'SAMPLE-001', status: 'ACTIVE' }], next_cursor: null });
    if (url.pathname === `/api/admin/account/admin/v1/users/${id}`) return send({ user_id: id, username: 'sample-account', user_code: 'SAMPLE-001', status: 'ACTIVE', created_at: '2026-10-01T00:00:00Z' });
    if (url.pathname === `/api/admin/account/admin/v1/users/${id}/memberships`) return send({ items: [] });
    if (url.pathname === '/api/admin/account/admin/v1/organizations') return send({ items: [{ organization_id: orgID, name: 'Sample organization', status: 'ACTIVE', member_count: 1 }], next_cursor: null });
    return send({ code: 'CONTROLLED_READ_NOT_IMPLEMENTED' }, 404);
  }
  const relative = url.pathname === '/' ? 'account.html' : url.pathname.replace(/^\//, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot + path.sep) || !fs.existsSync(target) || !fs.statSync(target).isFile()) { response.writeHead(404); return response.end(); }
  let content = fs.readFileSync(target);
  if (process.env.UI_STYLE_NEGATIVE_CONTROL === '1' && target.endsWith('.css')) content = Buffer.from(content.toString().replaceAll('.ap-org-confirm-backdrop', '.controlled-missing-confirmation-style'));
  response.writeHead(200, { 'Content-Type': target.endsWith('.html') ? 'text/html' : target.endsWith('.css') ? 'text/css' : 'text/javascript', 'Cache-Control': 'no-store' }); response.end(content);
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
let browser, passed = 0;
try {
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage(), errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const base = `http://127.0.0.1:${server.address().port}`;
  await page.setViewport({ width: 1440, height: 1000 });
  await page.goto(base + '/?section=creditsUsage', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('.ap-product-picker label');
  await page.select('.ap-sidebar-foot select', 'zh');
  assert.equal(await page.$eval('.ap-balance-hero>strong', node => node.textContent), '922,337,203,685,477');
  assert.equal(await page.$$eval('.ap-sidebar [role=tab]', nodes => nodes.length), 3);
  await page.click('.ap-sidebar [data-section=accountSettings]');
  await page.waitForSelector('.ap-profile-rows .ap-profile-row button');
  await page.click('.ap-profile-rows .ap-profile-row button');
  await page.waitForSelector('.ap-restored-dialog');
  assert.equal(await page.$eval('.ap-restored-dialog-backdrop', node => getComputedStyle(node).position), 'fixed');
  await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
  await page.waitForFunction(() => document.activeElement.closest('.ap-profile-row'));
  assert.ok(await page.evaluate(() => Boolean(document.activeElement.closest('.ap-profile-row'))));
  for (const width of [1440, 390]) { await page.setViewport({ width, height: 1000 }); assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1)); }
  assert.deepEqual(errors, []); passed++; console.log('PASS actual account.html bundle: canonical tabs, exact balance, dialog styles, focus and mobile layout');
  await page.setViewport({ width: 1440, height: 1000 });
  await page.goto(base + '/admin.html?section=users', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('.adm-table tbody tr'); await page.select('.adm-sidebar-foot select', 'zh');
  assert.equal(await page.$eval('body', node => getComputedStyle(node).margin), '0px');
  assert.equal(await page.$eval('.adm-sidebar', node => Math.round(node.getBoundingClientRect().width)), 272);
  await page.click('.adm-sidebar [data-section=organizations]');
  await page.waitForFunction(value => [...document.querySelectorAll('.adm-section-toolbar button')].some(node => node.textContent.trim() === value && !node.disabled), {}, organizationCopy.zh.create);
  const trigger = await page.$('.adm-section-toolbar button'); await trigger.click(); await page.waitForSelector('.ap-restored-dialog');
  assert.equal(await page.$eval('.ap-restored-dialog-backdrop', node => getComputedStyle(node).position), 'fixed');
  await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
  await page.waitForFunction(() => document.activeElement.closest('.adm-section-toolbar'));
  // This style probe deliberately checks the separate built admin entry. A
  // fixture importing both roots would mask a missing shared confirmation CSS.
  assert.equal(await page.evaluate(() => { const node = document.createElement('div'); node.className = 'ap-org-confirm-backdrop'; document.body.append(node); const position = getComputedStyle(node).position; node.remove(); return position; }), 'fixed');
  for (const width of [1440, 390]) { await page.setViewport({ width, height: 1000 }); assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1)); }
  assert.deepEqual(errors, []); assert.ok(!calls.some(call => call.method !== 'GET' && call.path !== '/api/account/v1/account/bootstrap'));
  passed++; console.log('PASS actual admin.html bundle: independent CSS, original frame, organization modal and zero business writes');
  assert.ok(accountPlatformCopy.zh.sections.accountSettings);
  console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
} catch (error) { console.error(error); process.exitCode = 1; }
finally { if (browser) await browser.close(); await new Promise(resolve => server.close(resolve)); }
