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
let csrfToken = 'csrf-user-a';
let policy = { revision: 2, enabled: false, amount_microcredits: 0, validity_days: 30, updated_at: new Date().toISOString(), eligibility_status: 'READY', recoverable_registration_count: 4, unrecoverable_registration_count: 0 };
let recentAuthenticationRequired = false;
let authenticationBehavior = 'concurrent-update';
let nextPolicyReadDelayMs = 0;
let delayedPolicyReadStarted = false;
let nextAuthorizationReadDelayMs = 0;
let delayNextSaveAuthorizationRead = false;
let delayedSaveAuthorizationStarted = false;
let delayNextPolicyWrite = false;
let delayedPolicyWriteStarted = false;
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
  if (url.pathname === '/__test__/auth-cancel') { authenticationBehavior = 'cancel'; nextPolicyReadDelayMs = 300; return json(response, {}); }
  if (url.pathname === '/__test__/auth-switch-user') { authenticationBehavior = 'switch-user'; return json(response, {}); }
  if (url.pathname === '/__test__/auth-expire-return') { authenticationBehavior = 'expire-return'; return json(response, {}); }
  if (url.pathname === '/__test__/delay-next-save-authorization') {
    delayNextSaveAuthorizationRead = true;
    delayedSaveAuthorizationStarted = false;
    return json(response, {});
  }
  if (url.pathname === '/__test__/delay-next-policy-write') {
    delayNextPolicyWrite = true;
    delayedPolicyWriteStarted = false;
    return json(response, {});
  }
  if (url.pathname === '/__test__/switch-current-user') {
    currentUserID = currentUserID.endsWith('0101') ? '0199a100-0001-7000-8000-000000000202' : '0199a100-0001-7000-8000-000000000101';
    csrfToken = currentUserID.endsWith('0101') ? 'csrf-user-a' : 'csrf-user-b';
    nextAuthorizationReadDelayMs = 500;
    return json(response, {});
  }
  if (url.pathname === '/__test__/switch-current-user-with-stale-policy-read') {
    currentUserID = currentUserID.endsWith('0101') ? '0199a100-0001-7000-8000-000000000202' : '0199a100-0001-7000-8000-000000000101';
    csrfToken = currentUserID.endsWith('0101') ? 'csrf-user-a' : 'csrf-user-b';
    nextAuthorizationReadDelayMs = 100;
    nextPolicyReadDelayMs = 800;
    delayedPolicyReadStarted = false;
    return json(response, {});
  }
  if (url.pathname === '/__test__/writes') return json(response, writes);
  if (url.pathname === '/auth/login') {
    reauthenticationRequests.push(url);
    let returnTo = url.searchParams.get('return_to') || '/admin.html?section=operations';
    if (authenticationBehavior === 'concurrent-update') policy = { ...policy, revision: policy.revision + 1, amount_microcredits: 400_000, validity_days: 45, updated_at: new Date().toISOString() };
    if (authenticationBehavior === 'cancel') returnTo = '/admin.html?section=operations';
    if (authenticationBehavior === 'switch-user') {
      currentUserID = currentUserID.endsWith('0101') ? '0199a100-0001-7000-8000-000000000202' : '0199a100-0001-7000-8000-000000000101';
      csrfToken = currentUserID.endsWith('0101') ? 'csrf-user-a' : 'csrf-user-b';
    }
    if (authenticationBehavior === 'expire-return') {
      authenticationBehavior = 'success';
      nextPolicyReadDelayMs = 1500;
      response.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' });
      return response.end(`<script>const key='pinvou.admin.registration-bonus.reauthentication-draft';const draft=JSON.parse(sessionStorage.getItem(key));draft.expires_at=Date.now()+1000;sessionStorage.setItem(key,JSON.stringify(draft));location.replace(${JSON.stringify(returnTo)});</script>`);
    }
    authenticationBehavior = 'success';
    response.writeHead(302, { Location: returnTo });
    return response.end();
  }
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: csrfToken });
  if (url.pathname === '/api/account/v1/account/bootstrap') {
    if (request.headers['x-csrf-token'] !== csrfToken) return json(response, { code: 'CSRF_REJECTED' }, 403);
    const requestedUserID = currentUserID;
    const requestedPermissions = [...permissions];
    if (delayNextSaveAuthorizationRead) {
      delayNextSaveAuthorizationRead = false;
      delayedSaveAuthorizationStarted = true;
      await new Promise((resolve) => { setTimeout(resolve, 250); });
    } else if (nextAuthorizationReadDelayMs > 0) {
      const delay = nextAuthorizationReadDelayMs;
      nextAuthorizationReadDelayMs = 0;
      await new Promise((resolve) => { setTimeout(resolve, delay); });
    }
    return json(response, { user_id: requestedUserID, status: "ACTIVE", personal_permissions: [], platform_permissions: requestedPermissions });
  }
  if (url.pathname === '/api/admin/billing/admin/v1/registration-bonus-policy' && request.method === 'GET') {
    const responsePolicy = { ...policy };
    if (nextPolicyReadDelayMs > 0) {
      const delay = nextPolicyReadDelayMs;
      nextPolicyReadDelayMs = 0;
      delayedPolicyReadStarted = true;
      await new Promise((resolve) => { setTimeout(resolve, delay); });
    }
    return json(response, responsePolicy);
  }
  if (url.pathname === '/api/admin/billing/admin/v1/registration-bonus-policy' && request.method === 'PUT') {
    if (request.headers['x-csrf-token'] !== csrfToken) return json(response, { code: 'CSRF_REJECTED' }, 403);
    if (delayNextPolicyWrite) {
      delayNextPolicyWrite = false;
      delayedPolicyWriteStarted = true;
      await new Promise((resolve) => { setTimeout(resolve, 500); });
    }
    if (recentAuthenticationRequired) {
      recentAuthenticationRequired = false;
      return json(response, { code: 'RECENT_AUTHENTICATION_REQUIRED', message: 'recent authentication required' }, 401);
    }
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    writes.push({ body, idempotencyKey: request.headers['idempotency-key'], csrf: request.headers['x-csrf-token'] });
    policy = { ...policy, revision: policy.revision + 1, enabled: body.enabled, amount_microcredits: (BigInt(body.amount_points) * 10_000n).toString(), validity_days: body.validity_days, updated_at: new Date().toISOString() };
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
const browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--no-first-run', `--explicitly-allowed-ports=${port}`] });
try {
  const cases = [
    ['minimum-zero', '0', '0'],
    ['minimum-positive-point', '10000', '1'],
    ['large-legal-integral-policy', '5000000000000070000', '500000000000007'],
    ['max-int64-allowed-integral-points', '9223372036854770000', '922337203685477'],
    ['legacy-nonintegral-truncates-towards-zero', '10001', '1'],
  ];
  let failures = 0;
  for (const [name, microcredits, expectedPoints] of cases) {
    const page = await browser.newPage();
    try {
      policy = { ...policy, revision: 2, enabled: false, amount_microcredits: microcredits, eligibility_status: 'READY' };
      writes.length = 0;
      await page.setViewport({ width: 1280, height: 900 });
      await page.evaluateOnNewDocument(() => localStorage.setItem('pinvou.admin.language', 'en'));
      await page.goto(`http://127.0.0.1:${port}/admin.html?section=operations`, { waitUntil: 'networkidle0' });
      await page.waitForSelector('[data-testid="registration-bonus-policy"] form');
      const shown = await page.$eval('[data-testid="registration-bonus-policy"] input[type="number"]', n => n.value);
      // Exercise a real enabled/reason-only save even when the before rendering is wrong.
      await page.type('[data-testid="registration-bonus-policy"] textarea', 'Keep amount while changing only policy enablement');
      await page.click('[data-testid="registration-bonus-policy"] input[type="checkbox"]');
      await page.click('[data-testid="registration-bonus-policy"] button[type="submit"]');
      await page.waitForFunction(() => document.querySelector('[data-testid="registration-bonus-policy"] .adm-registration-bonus-summary dd')?.textContent.trim() === '3');
      assert.equal(writes.length, 1);
      assert.equal(shown, expectedPoints, `${name}: exact current points shown`);
      assert.equal(String(writes[0].body.amount_points), expectedPoints, `${name}: reason/enable save keeps point count`);
      assert.equal(policy.amount_microcredits, (BigInt(expectedPoints) * 10000n).toString());
      assert.equal(writes[0].body.enabled, true);
      assert.equal(writes[0].body.expected_revision, 2);
      assert.equal(writes[0].csrf, 'csrf-user-a');
      assert.match(writes[0].idempotencyKey, /^registration-bonus-/);
      console.log(JSON.stringify({ action: 'pass', test: name }));
    } catch (error) {
      failures += 1;
      console.log(JSON.stringify({ action: 'fail', test: name, message: error.message, writes: writes.map(w => ({amount_points:w.body.amount_points})) }));
    } finally { await page.close(); }
  }
  if (failures) process.exitCode = 1;
} finally { await browser.close(); await new Promise(resolve => server.close(resolve)); }
