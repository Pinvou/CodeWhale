import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const browserPath = [process.env.CHROME, 'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe', 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium or Edge is required');
const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
let permissions = ['platform.registration-bonus.read', 'platform.registration-bonus.manage'];
let policy = { revision: 2, enabled: false, amount_microcredits: 0, validity_days: 30, updated_at: new Date().toISOString(), eligibility_status: 'READY', recoverable_registration_count: 4, unrecoverable_registration_count: 0 };
const writes = [];

function json(response, payload, status = 200) {
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}
const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/readonly') { permissions = ['platform.registration-bonus.read']; return json(response, {}); }
  if (url.pathname === '/__test__/blocked') { policy = { ...policy, eligibility_status: 'BLOCKED', unrecoverable_registration_count: 2 }; return json(response, {}); }
  if (url.pathname === '/__test__/writes') return json(response, writes);
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf-test' });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: '0199a100-0001-7000-8000-000000000101', platform_permissions: permissions });
  if (url.pathname === '/api/admin/billing/admin/v1/registration-bonus-policy' && request.method === 'GET') return json(response, policy);
  if (url.pathname === '/api/admin/billing/admin/v1/registration-bonus-policy' && request.method === 'PUT') {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    writes.push({ body, idempotencyKey: request.headers['idempotency-key'], csrf: request.headers['x-csrf-token'] });
    policy = { ...policy, revision: policy.revision + 1, enabled: body.enabled, amount_microcredits: body.amount_points * 10_000, validity_days: body.validity_days, updated_at: new Date().toISOString() };
    return json(response, policy);
  }
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
  await page.setViewport({ width: 1280, height: 900, deviceScaleFactor: 1 });
  await page.evaluateOnNewDocument(() => localStorage.setItem('pinvou.admin.language', 'en'));
  await page.goto(`http://127.0.0.1:${port}/admin.html?section=operations`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('[data-testid="registration-bonus-policy"] form');
  const text = await page.$eval('[data-testid="registration-bonus-policy"]', (node) => node.textContent);
  assert.match(text, /permanently consumes eligibility/i);
  assert.match(await page.$eval('[data-testid="registration-bonus-readiness"]', (node) => node.textContent), /backfill is complete/i);
  const inputs = await page.$$('[data-testid="registration-bonus-policy"] input[type="number"]');
  await inputs[0].evaluate((node) => { node.focus(); node.select(); }); await inputs[0].type('25');
  await inputs[1].evaluate((node) => { node.focus(); node.select(); }); await inputs[1].type('90');
  const textareas = await page.$$('[data-testid="registration-bonus-policy"] textarea');
  await textareas[0].type('Enable the controlled launch bonus');
  await page.click('[data-testid="registration-bonus-policy"] input[type="checkbox"]');
  await page.click('[data-testid="registration-bonus-policy"] button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('[data-testid="registration-bonus-policy"] .adm-sms-summary strong')?.textContent.trim() === '3');
  assert.equal(writes.length, 1);
  assert.equal(writes[0].body.expected_revision, 2);
  assert.equal(writes[0].body.amount_points, 25);
  assert.equal(writes[0].csrf, 'csrf-test');
  assert.match(writes[0].idempotencyKey, /^registration-bonus-/);

  await fetch(`http://127.0.0.1:${port}/__test__/blocked`);
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForSelector('[data-testid="registration-bonus-policy"] form');
  assert.match(await page.$eval('[data-testid="registration-bonus-readiness"]', (node) => node.textContent), /2 anonymized registrations/i);
  const enabledCheckbox = await page.$('[data-testid="registration-bonus-policy"] input[type="checkbox"]');
  assert.equal(await enabledCheckbox.evaluate((node) => node.disabled), false, 'an enabled policy must retain its kill switch while readiness is blocked');
  await enabledCheckbox.click();
  const blockedTextarea = await page.$('[data-testid="registration-bonus-policy"] textarea');
  await blockedTextarea.type('Disable while eligibility is blocked');
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] button[type="submit"]', (node) => node.disabled), false, 'disabling must remain available while blocked');
  await page.click('[data-testid="registration-bonus-policy"] button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('[data-testid="registration-bonus-policy"] .adm-sms-summary strong')?.textContent.trim() === '4');
  assert.equal(writes.at(-1).body.enabled, false);
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] input[type="checkbox"]', (node) => node.disabled), true, 'blocked readiness must prevent re-enabling');

  await fetch(`http://127.0.0.1:${port}/__test__/readonly`);
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForSelector('[data-testid="registration-bonus-policy"] form');
  assert.equal(await page.$('[data-testid="registration-bonus-policy"] button[type="submit"]'), null);
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] input[type="number"]', (node) => node.disabled), true);
  console.log('Admin registration bonus UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
