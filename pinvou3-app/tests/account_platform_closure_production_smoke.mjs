import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import puppeteer from 'puppeteer-core';
import { accountClosureCopy } from '../src/shared/account-closure-i18n.js';
import { financeCopy } from '../src/shared/finance-i18n.js';
import { accountClosedGrantCopy } from '../src/shared/account-closed-grants-i18n.js';
import { organizationCopy } from '../src/shared/organization-i18n.js';

const browserPath = [process.env.CHROME, 'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe', 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'].find(value => value && existsSync(value));
assert.ok(browserPath, 'Chromium is required');
const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const actorA = '0199a100-0001-7000-8000-000000000101';
const actorB = '0199a100-0001-7000-8000-000000000102';
const closureID = '0199a100-0001-7000-8000-000000000201';
const organizationID = '0199a100-0001-7000-8000-000000000301';
const grantID = '0199a100-0001-7000-8000-000000000401';
let actor = actorA, csrf = 'csrf-initial', mode = 'personal', writes = [], effects = 0, auths = 0;
let readOnly = false;
let immediateReceipt = false;
let unknownBatchItem = false;
const closure = status => ({ closure_request_id: closureID, status, stage: status === 'COMPLETED' ? 'COMPLETED' : 'CLOSURE_REQUESTED', blockers: status === 'BLOCKED' ? [{ code: 'FORCED_REFUND_UNRESOLVED', domain: 'BILLING', resource_type: 'FORCED_REFUND', user_action: 'FINISH_REFUND' }] : [], sla_expires_at: '2026-10-12T00:00:00Z' });
const grant = reclaimed => ({ grant_id: grantID, organization_id: organizationID, member_user_id: actorA, package_id: closureID, component_id: closureID, status: reclaimed ? 'RECLAIMED_ACCOUNT_CLOSED' : 'SUSPENDED', suspension_reason: reclaimed ? null : 'ACCOUNT_CLOSED', membership_version: 2, allocation_version: 2, original_microcredits: 10, available_microcredits: reclaimed ? 0 : 10, reserved_microcredits: 0, used_microcredits: 0, expired_microcredits: 0, revoked_microcredits: reclaimed ? 10 : 0 });
const grantReceipt = () => ({ grant: grant(true), amount_microcredits: 10, subject_lifecycle_version: 2, replayed: true });
const json = (response, payload, status = 200) => { response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); response.end(JSON.stringify(payload)); };
const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/auth/login') {
    assert.equal(url.searchParams.get('reauthenticate'), '1');
    auths++; csrf = `csrf-reauth-${auths}`;
    response.writeHead(302, { Location: url.searchParams.get('return_to') }); return response.end();
  }
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: csrf });
  if (url.pathname === '/api/account/v1/account/bootstrap') {
    assert.equal(request.headers['x-csrf-token'], csrf);
    return json(response, { user_id: actor, active_context: { context_type: mode.startsWith('org') ? 'ORGANIZATION' : 'PERSONAL', organization_id: organizationID, context_version: 1, organization_authorization_version: 1, permissions: mode.startsWith('org') ? ['org.quota.read.self', 'org.grants.reclaim_closed'] : [] }, status: 'ACTIVE', personal_permissions: mode === 'personal' ? ['personal.account.close.read', ...readOnly ? [] : ['personal.account.close.request']] : [], platform_permissions: mode === 'admin' ? ['platform.account-closures.read', ...readOnly ? [] : ['platform.account-closures.repair']] : [] });
  }
  const orgBase = `/api/token/v1/organizations/${organizationID}`;
  if (request.method === 'GET' && url.pathname === `${orgBase}/quota-grants`) return json(response, { items: [grant(Boolean(effects))] });
  if (request.method === 'POST' && [ `${orgBase}/account-closed-grants/${grantID}/reclaim`, `${orgBase}/account-closed-grants:reclaim-batch` ].includes(url.pathname)) {
    assert.equal(request.headers['x-csrf-token'], csrf);
    const chunks = []; for await (const chunk of request) chunks.push(chunk);
    const facts = JSON.parse(Buffer.concat(chunks).toString('utf8')), key = request.headers['idempotency-key'];
    writes.push({ actor, key, facts });
    if (writes.length === 1) {
      effects++;
      if (unknownBatchItem && mode === 'org-batch') return json(response, { items: [{ grant_id: grantID, status: 'FAILED', code: 'COMMAND_RESULT_UNKNOWN' }] }, 207);
      return json(response, { code: 'COMMAND_RESULT_UNKNOWN' }, 503);
    }
    assert.equal(key, writes[0].key); assert.deepEqual(facts, writes[0].facts);
    if (writes.length === 2) return json(response, { code: 'PERMISSION_DENIED' }, mode === 'org-batch' ? 409 : 403);
    return mode === 'org-batch' ? json(response, { items: [{ grant_id: grantID, status: 'SUCCEEDED', result: grantReceipt() }] }, 207) : json(response, grantReceipt());
  }
  if (request.method === 'GET' && url.pathname === '/api/account/v1/me/account-closure-request') return effects ? json(response, closure('BLOCKED')) : json(response, { code: 'NOT_FOUND' }, 404);
  if (request.method === 'GET' && url.pathname === '/api/admin/account/admin/v1/account-closure-requests') return json(response, { items: effects ? [] : [closure('REPAIR_REQUIRED')] });
  const personal = url.pathname === '/api/account/v1/me/account-closure-requests';
  const admin = url.pathname === `/api/admin/account/admin/v1/account-closure-requests/${closureID}/actions/retry`;
  if (request.method === 'POST' && (personal || admin)) {
    assert.equal(request.headers['x-csrf-token'], csrf);
    const body = []; for await (const chunk of request) body.push(chunk);
    const facts = JSON.parse(Buffer.concat(body).toString('utf8'));
    const key = request.headers['idempotency-key'];
    assert.match(key, /^closure-/); writes.push({ actor, key, facts });
    if (writes.length === 1) { effects++; return immediateReceipt ? json(response, closure('COMPLETED'), 202) : json(response, { code: 'COMMAND_RESULT_UNKNOWN' }, 503); }
    assert.equal(key, writes[0].key); assert.deepEqual(facts, writes[0].facts); assert.equal(actor, writes[0].actor);
    if (writes.length === 2) return json(response, { code: 'RECENT_AUTHENTICATION_REQUIRED' }, 401);
    return json(response, closure('COMPLETED'), personal ? 202 : 200);
  }
  if (url.pathname.startsWith('/api/')) return json(response, { code: 'UNEXPECTED_TEST_ROUTE' }, 404);
  const file = path.resolve(staticRoot, url.pathname === '/' ? 'account.html' : `.${url.pathname}`);
  if (!file.startsWith(`${staticRoot}${path.sep}`)) return json(response, {}, 404);
  try { const contents = await readFile(file); response.writeHead(200, { 'Content-Type': file.endsWith('.html') ? 'text/html; charset=utf-8' : file.endsWith('.css') ? 'text/css' : 'text/javascript' }); response.end(contents); }
  catch { json(response, {}, 404); }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
const browser = await puppeteer.launch({ executablePath: browserPath, headless: true, args: ['--no-sandbox'] });
let passed = 0;
const click = async (page, label) => {
  try { await page.waitForFunction(text => [...document.querySelectorAll('button')].some(button => button.textContent === text && !button.disabled), { timeout: 5000 }, label); }
  catch (error) { console.log('CONTROLLED_FIXTURE_PAGE', await page.evaluate(() => document.body.innerText)); throw error; }
  await page.evaluate(text => [...document.querySelectorAll('button')].find(button => button.textContent === text).click(), label);
};
try {
  for (const locale of ['zh', 'en', 'ja']) {
    mode = 'personal'; actor = actorA; readOnly = false; effects = 0; auths = 0; writes = []; csrf = 'csrf-initial';
    const page = await browser.newPage();
    page.on('pageerror', error => console.log('CONTROLLED_FIXTURE_RUNTIME', error.message));
    page.on('requestfailed', request => console.log('CONTROLLED_FIXTURE_LOAD', new URL(request.url()).pathname, request.failure()?.errorText));
    await page.evaluateOnNewDocument(language => localStorage.setItem('pinvou.account.language', language), locale);
    page.on('dialog', dialog => dialog.accept('CLOSE_MY_ACCOUNT'));
    await page.goto(`${origin}/account.html`, { waitUntil: 'networkidle0' });
    await click(page, accountClosureCopy[locale].start);
    await click(page, financeCopy[locale].retry);
    await page.waitForFunction(() => location.pathname === '/' && document.querySelector('.ap-danger-section'), { timeout: 5000 });
    await page.waitForFunction(text => document.body.textContent.includes(text), {}, accountClosureCopy[locale].statuses.BLOCKED);
    await page.waitForFunction(text => document.body.textContent.includes(text), {}, accountClosureCopy[locale].blockerCodes.FORCED_REFUND_UNRESOLVED);
    assert.equal(await page.evaluate(() => document.body.textContent.includes('FORCED_REFUND_UNRESOLVED')), false);
    assert.equal(auths, 1); assert.equal(writes.length, 2); assert.equal(effects, 1);
    // A fresh client and CSRF after the actual redirect must not automatically replay.
    await page.reload({ waitUntil: 'networkidle0' }); assert.equal(writes.length, 2);
    await click(page, financeCopy[locale].retry);
    await page.waitForFunction(text => document.body.textContent.includes(text), {}, accountClosureCopy[locale].requested);
    assert.equal(writes.length, 3); assert.equal(effects, 1);
    assert.equal(await page.evaluate(() => Object.keys(document.defaultView.sessionStorage).filter(key => key.startsWith('pinvou.closure.intent.v1:')).length), 0);
    passed++; console.log(`PASS ${locale} compiled personal unknown survives real reauthentication and reload; same key/facts, one effect`);
    await page.close();
  }
  mode = 'admin'; readOnly = false; effects = 0; writes = []; auths = 0; csrf = 'csrf-initial';
  const page = await browser.newPage(); await page.evaluateOnNewDocument(() => localStorage.setItem('pinvou.admin.language', 'en'));
  await page.goto(`${origin}/admin.html?section=closures`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('textarea'); await page.type('textarea', 'Retry only the original closure command');
  await click(page, financeCopy.en.retry); await click(page, financeCopy.en.retry);
  await click(page, accountClosureCopy.en.reauthenticate);
  await page.waitForFunction(() => location.search.includes('section=closures') && document.querySelector('textarea'), { timeout: 5000 });
  await page.reload({ waitUntil: 'networkidle0' });
  assert.equal(writes.length, 2); assert.equal(effects, 1);
  await click(page, financeCopy.en.retry);
  await page.waitForFunction(() => !document.querySelector('textarea'), { timeout: 5000 });
  assert.equal(writes.length, 3); assert.equal(effects, 1); passed++;
  console.log('PASS compiled admin completed case absent from list retains original reason/key across authentication/reload');
  await page.close();
  for (const orgMode of ['org-single', 'org-batch', 'org-batch-unknown-item']) {
    unknownBatchItem = orgMode === 'org-batch-unknown-item';
    mode = orgMode === 'org-single' ? orgMode : 'org-batch'; actor = actorA; readOnly = false; effects = 0; writes = []; csrf = 'csrf-initial';
    const orgPage = await browser.newPage();
    await orgPage.evaluateOnNewDocument(() => localStorage.setItem('pinvou.account.language', 'en'));
    orgPage.on('dialog', dialog => dialog.accept());
    await orgPage.goto(`${origin}/account.html`, { waitUntil: 'networkidle0' });
    if (orgMode !== 'org-single') {
      await orgPage.waitForSelector('select[multiple]');
      await orgPage.select('select[multiple]', grantID);
    }
    await click(orgPage, orgMode !== 'org-single' ? accountClosedGrantCopy.en.batch : accountClosedGrantCopy.en.single);
    await orgPage.waitForFunction(text => document.body.textContent.includes(text), {}, organizationCopy.en.unknown);
    await orgPage.reload({ waitUntil: 'networkidle0' });
    assert.equal(writes.length, 1); assert.equal(effects, 1);
    // The list now has no eligible grants; the original pending operation remains usable.
    await click(orgPage, organizationCopy.en.retry);
    await orgPage.waitForFunction(text => document.body.textContent.includes(text), {}, organizationCopy.en.unknown);
    await click(orgPage, organizationCopy.en.retry);
    await orgPage.waitForFunction(() => !Object.keys(document.defaultView.sessionStorage).some(key => key.startsWith('pinvou.closure.intent.v1:')), { timeout: 5000 });
    assert.equal(writes.length, 3); assert.equal(effects, 1);
    assert.equal(await orgPage.evaluate(() => Object.keys(document.defaultView.sessionStorage).filter(key => key.startsWith('pinvou.closure.intent.v1:')).length), 0);
    passed++; console.log(`PASS compiled ${orgMode} reload with no eligible grants retains facts/key after explicit 4xx and recovers original receipt`);
    await orgPage.close();
  }
  mode = 'personal'; actor = actorB; readOnly = true; effects = 0; writes = [];
  const readonlyPage = await browser.newPage();
  await readonlyPage.goto(`${origin}/account.html`, { waitUntil: 'networkidle0' });
  assert.equal(await readonlyPage.evaluate(() => document.querySelector('.ap-danger-section')?.querySelectorAll('button').length), 1);
  assert.equal(writes.length, 0); passed++; console.log('PASS compiled personal read-only permission has no mutation action');
  await readonlyPage.close();
  mode = 'personal'; actor = actorA; readOnly = false; effects = 0; writes = [];
  const corruptPage = await browser.newPage();
  await corruptPage.evaluateOnNewDocument(user => {
    document.defaultView.sessionStorage.setItem(`pinvou.closure.intent.v1:${user}:account-closure`, '{broken');
    localStorage.setItem('pinvou.account.language', 'en');
  }, actorA);
  await corruptPage.goto(`${origin}/account.html`, { waitUntil: 'networkidle0' });
  await corruptPage.waitForFunction(text => document.body.textContent.includes(text), {}, accountClosureCopy.en.recoveryUnavailable);
  assert.equal(await corruptPage.evaluate(text => [...document.querySelectorAll('button')].find(button => button.textContent === text)?.disabled, accountClosureCopy.en.start), true);
  assert.equal(writes.length, 0); passed++; console.log('PASS compiled corrupted durable proof fails closed before any mutation');
  await corruptPage.close();
  for (const failure of ['unwritable', 'readback-mismatch']) {
    const unavailablePage = await browser.newPage();
    await unavailablePage.evaluateOnNewDocument(kind => {
      localStorage.setItem('pinvou.account.language', 'en');
      const original = document.defaultView.Storage.prototype.setItem;
      document.defaultView.Storage.prototype.setItem = function (key, value) {
        if (key.startsWith('pinvou.closure.intent.v1:')) {
          if (kind === 'unwritable') throw new DOMException('Storage unavailable', 'QuotaExceededError');
          return original.call(this, key, '{damaged');
        }
        return original.call(this, key, value);
      };
    }, failure);
    unavailablePage.on('dialog', dialog => dialog.accept('CLOSE_MY_ACCOUNT'));
    await unavailablePage.goto(`${origin}/account.html`, { waitUntil: 'networkidle0' });
    await click(unavailablePage, accountClosureCopy.en.start);
    await unavailablePage.waitForFunction(text => document.body.textContent.includes(text), {}, accountClosureCopy.en.recoveryUnavailable);
    assert.equal(writes.length, 0); passed++; console.log(`PASS compiled ${failure} proof persistence prevents sending a command`);
    await unavailablePage.close();
  }
  immediateReceipt = true;
  const clearFailurePage = await browser.newPage();
  await clearFailurePage.evaluateOnNewDocument(() => {
    localStorage.setItem('pinvou.account.language', 'en');
    const original = document.defaultView.Storage.prototype.removeItem;
    document.defaultView.Storage.prototype.removeItem = function (key) {
      if (key.startsWith('pinvou.closure.intent.v1:')) throw new DOMException('Storage unavailable', 'SecurityError');
      return original.call(this, key);
    };
  });
  clearFailurePage.on('dialog', dialog => dialog.accept('CLOSE_MY_ACCOUNT'));
  await clearFailurePage.goto(`${origin}/account.html`, { waitUntil: 'networkidle0' });
  await click(clearFailurePage, accountClosureCopy.en.start);
  await clearFailurePage.waitForFunction(text => document.body.textContent.includes(text), {}, accountClosureCopy.en.recoveryUnavailable);
  assert.equal(writes.length, 1); assert.equal(effects, 1);
  assert.equal(await clearFailurePage.evaluate(() => Object.keys(document.defaultView.sessionStorage).some(key => key.startsWith('pinvou.closure.intent.v1:'))), true);
  assert.equal(await clearFailurePage.evaluate(() => [...document.querySelectorAll('.ap-danger-section button')].filter(button => !button.disabled).length), 1);
  passed++; console.log('PASS compiled receipt with proof-clear failure retains evidence and blocks another command');
  await clearFailurePage.close(); immediateReceipt = false; effects = 0; writes = [];
  const changedActorPage = await browser.newPage();
  await changedActorPage.evaluateOnNewDocument(() => localStorage.setItem('pinvou.account.language', 'en'));
  changedActorPage.on('dialog', dialog => dialog.accept('CLOSE_MY_ACCOUNT'));
  await changedActorPage.goto(`${origin}/account.html`, { waitUntil: 'networkidle0' });
  await click(changedActorPage, accountClosureCopy.en.start);
  await changedActorPage.waitForFunction(text => document.body.textContent.includes(text), {}, financeCopy.en.unknown);
  const proofBefore = await changedActorPage.evaluate(() => Object.entries(document.defaultView.sessionStorage));
  actor = actorB; csrf = 'csrf-new-actor';
  await changedActorPage.reload({ waitUntil: 'networkidle0' });
  assert.equal(writes.length, 1);
  assert.deepEqual(await changedActorPage.evaluate(() => Object.entries(document.defaultView.sessionStorage)), proofBefore);
  assert.equal(await changedActorPage.evaluate(text => [...document.querySelectorAll('button')].some(button => button.textContent === text), financeCopy.en.retry), false);
  passed++; console.log('PASS compiled new actor cannot replay old actor proof and retains old evidence');
  await changedActorPage.close();
} finally { await browser.close(); await new Promise(resolve => server.close(resolve)); }
console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
