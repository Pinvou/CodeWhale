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
let currentUserID = '0199a100-0001-7000-8000-000000000101';
let policy = { revision: 2, enabled: false, amount_microcredits: 0, validity_days: 30, updated_at: new Date().toISOString(), eligibility_status: 'READY', recoverable_registration_count: 4, unrecoverable_registration_count: 0 };
let recentAuthenticationRequired = false;
let authenticationBehavior = 'concurrent-update';
const reauthenticationRequests = [];
const writes = [];

function json(response, payload, status = 200) {
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}
const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/readonly') { permissions = ['platform.registration-bonus.read']; return json(response, {}); }
  if (url.pathname === '/__test__/blocked') { policy = { ...policy, eligibility_status: 'BLOCKED', unrecoverable_registration_count: 2 }; return json(response, {}); }
  if (url.pathname === '/__test__/recent-auth') { recentAuthenticationRequired = true; return json(response, {}); }
  if (url.pathname === '/__test__/auth-cancel') { authenticationBehavior = 'cancel'; return json(response, {}); }
  if (url.pathname === '/__test__/auth-switch-user') { authenticationBehavior = 'switch-user'; return json(response, {}); }
  if (url.pathname === '/__test__/writes') return json(response, writes);
  if (url.pathname === '/auth/login') {
    reauthenticationRequests.push(url);
    let returnTo = url.searchParams.get('return_to') || '/admin.html?section=operations';
    if (authenticationBehavior === 'concurrent-update') policy = { ...policy, revision: policy.revision + 1, amount_microcredits: 400_000, validity_days: 45, updated_at: new Date().toISOString() };
    if (authenticationBehavior === 'cancel') returnTo = '/admin.html?section=operations';
    if (authenticationBehavior === 'switch-user') currentUserID = '0199a100-0001-7000-8000-000000000202';
    authenticationBehavior = 'success';
    response.writeHead(302, { Location: returnTo });
    return response.end();
  }
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf-test' });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: currentUserID, platform_permissions: permissions });
  if (url.pathname === '/api/admin/billing/admin/v1/registration-bonus-policy' && request.method === 'GET') return json(response, policy);
  if (url.pathname === '/api/admin/billing/admin/v1/registration-bonus-policy' && request.method === 'PUT') {
    if (recentAuthenticationRequired) {
      recentAuthenticationRequired = false;
      return json(response, { code: 'RECENT_AUTHENTICATION_REQUIRED', message: 'recent authentication required' }, 401);
    }
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
  const desktopLayout = await page.$eval('[data-testid="registration-bonus-policy"]', (node) => {
    const editor = node.querySelector('.adm-registration-bonus-editor').getBoundingClientRect();
    const reason = node.querySelector('.adm-sms-reason').getBoundingClientRect();
    const textarea = node.querySelector('textarea').getBoundingClientRect();
    const actions = node.querySelector('.adm-registration-bonus-actions').getBoundingClientRect();
    const submit = node.querySelector('button[type="submit"]').getBoundingClientRect();
    return {
      reasonWidth: reason.width,
      editorWidth: editor.width,
      textareaHeight: textarea.height,
      submitRightGap: Math.abs(actions.right - submit.right),
    };
  });
  assert.ok(desktopLayout.reasonWidth >= desktopLayout.editorWidth - 40, 'change reason must use the editor width');
  assert.ok(desktopLayout.textareaHeight >= 110, 'change reason must provide a usable writing area');
  assert.ok(desktopLayout.submitRightGap <= 1, 'the primary action must align to the editor edge');
  const inputs = await page.$$('[data-testid="registration-bonus-policy"] input[type="number"]');
  await inputs[0].evaluate((node) => { node.focus(); node.select(); }); await inputs[0].type('25');
  await inputs[1].evaluate((node) => { node.focus(); node.select(); }); await inputs[1].type('90');
  const textareas = await page.$$('[data-testid="registration-bonus-policy"] textarea');
  await textareas[0].type('Enable the controlled launch bonus');
  await page.click('[data-testid="registration-bonus-policy"] input[type="checkbox"]');
  await page.click('[data-testid="registration-bonus-policy"] button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('[data-testid="registration-bonus-policy"] .adm-registration-bonus-summary dd')?.textContent.trim() === '3');
  assert.equal(writes.length, 1);
  assert.equal(writes[0].body.expected_revision, 2);
  assert.equal(writes[0].body.amount_points, 25);
  assert.equal(writes[0].csrf, 'csrf-test');
  assert.match(writes[0].idempotencyKey, /^registration-bonus-/);

  await fetch(`http://127.0.0.1:${port}/__test__/recent-auth`);
  await page.type('[data-testid="registration-bonus-policy"] textarea', 'Confirm recent authentication flow');
  await page.click('[data-testid="registration-bonus-policy"] button[type="submit"]');
  await page.waitForSelector('[data-testid="registration-bonus-policy"] [role="alert"]');
  assert.match(await page.$eval('[data-testid="registration-bonus-policy"] [role="alert"]', (node) => node.textContent), /sign in again/i);
  assert.match(await page.$eval('[data-testid="registration-bonus-policy"] [role="alert"] button', (node) => node.textContent), /sign in again/i);
  await Promise.all([
    page.waitForNavigation({ waitUntil: 'networkidle0' }),
    page.click('[data-testid="registration-bonus-policy"] [role="alert"] button'),
  ]);
  await page.waitForSelector('[data-testid="registration-bonus-policy"] form');
  assert.equal(reauthenticationRequests.length, 1);
  assert.equal(reauthenticationRequests[0].searchParams.get('reauthenticate'), '1');
  const firstReturnTo = new URL(reauthenticationRequests[0].searchParams.get('return_to'), `http://127.0.0.1:${port}`);
  assert.equal(firstReturnTo.pathname, '/admin.html');
  assert.equal(firstReturnTo.searchParams.get('section'), 'operations');
  assert.match(firstReturnTo.searchParams.get('registration_bonus_stepup'), /^[0-9a-f-]{36}$/);
  assert.deepEqual(await page.$$eval('[data-testid="registration-bonus-policy"] input[type="number"]', (nodes) => nodes.map((node) => node.value)), ['25', '90']);
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] input[type="checkbox"]', (node) => node.checked), true);
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] textarea', (node) => node.value), 'Confirm recent authentication flow');
  assert.equal(await page.$eval('html', (node) => node.ownerDocument.defaultView.sessionStorage.getItem('pinvou.admin.registration-bonus.reauthentication-draft')), null);
  assert.equal(new URL(page.url()).searchParams.has('registration_bonus_stepup'), false);
  assert.match(await page.$eval('[data-testid="registration-bonus-policy"] [role="alert"]', (node) => node.textContent), /policy changed during reauthentication/i);
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] button[type="submit"]', (node) => node.disabled), true, 'a stale restored draft must not be submitted against the latest revision');
  assert.equal(writes.length, 1, 'restoring a stale draft must not create a write');
  await page.click('[data-testid="registration-bonus-policy"] [role="alert"] button');
  await page.waitForFunction(() => document.querySelector('[data-testid="registration-bonus-policy"] input[type="number"]')?.value === '40');
  assert.deepEqual(await page.$$eval('[data-testid="registration-bonus-policy"] input[type="number"]', (nodes) => nodes.map((node) => node.value)), ['40', '45']);
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] textarea', (node) => node.value), '');

  await page.type('[data-testid="registration-bonus-policy"] textarea', 'Verify unavailable session storage');
  await fetch(`http://127.0.0.1:${port}/__test__/recent-auth`);
  await page.click('[data-testid="registration-bonus-policy"] button[type="submit"]');
  await page.waitForSelector('[data-testid="registration-bonus-policy"] [role="alert"]');
  await page.$eval('html', (node) => {
    const view = node.ownerDocument.defaultView;
    view.__pinvouOriginalSessionStorageSetItem = view.Storage.prototype.setItem;
    view.Storage.prototype.setItem = () => { throw new Error('session storage unavailable'); };
  });
  const urlBeforeStorageFailure = page.url();
  await page.click('[data-testid="registration-bonus-policy"] [role="alert"] button');
  await page.waitForFunction(() => /draft could not be stored safely/i.test(document.querySelector('[data-testid="registration-bonus-policy"] [role="alert"]')?.textContent || ''));
  assert.equal(page.url(), urlBeforeStorageFailure, 'storage failure must cancel the reauthentication navigation');
  assert.equal(reauthenticationRequests.length, 1, 'storage failure must not reach the authentication endpoint');
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] textarea', (node) => node.value), 'Verify unavailable session storage');
  await page.$eval('html', (node) => {
    const view = node.ownerDocument.defaultView;
    view.Storage.prototype.setItem = view.__pinvouOriginalSessionStorageSetItem;
    delete view.__pinvouOriginalSessionStorageSetItem;
  });

  await fetch(`http://127.0.0.1:${port}/__test__/auth-cancel`);
  await Promise.all([
    page.waitForNavigation({ waitUntil: 'networkidle0' }),
    page.click('[data-testid="registration-bonus-policy"] [role="alert"] button'),
  ]);
  await page.waitForSelector('.adm-registration-bonus-draft-prompt');
  assert.deepEqual(await page.$$eval('[data-testid="registration-bonus-policy"] input[type="number"]', (nodes) => nodes.map((node) => node.value)), ['40', '45'], 'unfinished reauthentication must keep the server policy authoritative until recovery is chosen');
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] textarea', (node) => node.value), '');
  await page.click('.adm-registration-bonus-draft-prompt button:first-child');
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] textarea', (node) => node.value), 'Verify unavailable session storage');
  assert.equal(await page.$eval('html', (node) => node.ownerDocument.defaultView.sessionStorage.getItem('pinvou.admin.registration-bonus.reauthentication-draft')), null);
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForSelector('[data-testid="registration-bonus-policy"] form');

  await page.type('[data-testid="registration-bonus-policy"] textarea', 'Ensure drafts cannot cross administrators');
  await fetch(`http://127.0.0.1:${port}/__test__/recent-auth`);
  await page.click('[data-testid="registration-bonus-policy"] button[type="submit"]');
  await page.waitForSelector('[data-testid="registration-bonus-policy"] [role="alert"]');
  await fetch(`http://127.0.0.1:${port}/__test__/auth-switch-user`);
  await Promise.all([
    page.waitForNavigation({ waitUntil: 'networkidle0' }),
    page.click('[data-testid="registration-bonus-policy"] [role="alert"] button'),
  ]);
  await page.waitForSelector('[data-testid="registration-bonus-policy"] form');
  assert.equal(currentUserID, '0199a100-0001-7000-8000-000000000202');
  assert.equal(await page.$('.adm-registration-bonus-draft-prompt'), null);
  assert.deepEqual(await page.$$eval('[data-testid="registration-bonus-policy"] input[type="number"]', (nodes) => nodes.map((node) => node.value)), ['40', '45']);
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] textarea', (node) => node.value), '', 'a different administrator must never receive the originating administrator draft');
  assert.equal(await page.$eval('html', (node) => node.ownerDocument.defaultView.sessionStorage.getItem('pinvou.admin.registration-bonus.reauthentication-draft')), null);

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
  await page.waitForFunction(() => document.querySelector('[data-testid="registration-bonus-policy"] .adm-registration-bonus-summary dd')?.textContent.trim() === '5');
  assert.equal(writes.at(-1).body.enabled, false);
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] input[type="checkbox"]', (node) => node.disabled), true, 'blocked readiness must prevent re-enabling');

  await fetch(`http://127.0.0.1:${port}/__test__/readonly`);
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForSelector('[data-testid="registration-bonus-policy"] form');
  assert.equal(await page.$('[data-testid="registration-bonus-policy"] button[type="submit"]'), null);
  assert.equal(await page.$eval('[data-testid="registration-bonus-policy"] input[type="number"]', (node) => node.disabled), true);

  permissions = ['platform.registration-bonus.read', 'platform.registration-bonus.manage'];
  policy = { ...policy, eligibility_status: 'READY', unrecoverable_registration_count: 0 };
  await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 1 });
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForSelector('[data-testid="registration-bonus-policy"] form');
  const mobileLayout = await page.$eval('[data-testid="registration-bonus-policy"]', (node) => {
    const fields = [...node.querySelectorAll('input[type="number"]')].map((input) => input.getBoundingClientRect());
    const editor = node.querySelector('.adm-registration-bonus-editor').getBoundingClientRect();
    const submit = node.querySelector('button[type="submit"]').getBoundingClientRect();
    return {
      fieldsStacked: fields[1].top > fields[0].bottom,
      submitWidth: submit.width,
      editorInnerWidth: editor.width - 30,
    };
  });
  assert.equal(mobileLayout.fieldsStacked, true, 'numeric fields must stack on narrow screens');
  assert.ok(Math.abs(mobileLayout.submitWidth - mobileLayout.editorInnerWidth) <= 2, 'primary action must fill the narrow editor');
  console.log('Admin registration bonus UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
