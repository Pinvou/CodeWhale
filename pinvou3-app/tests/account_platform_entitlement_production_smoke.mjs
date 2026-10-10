import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import puppeteer from 'puppeteer-core';
import { entitlementCorrectionCopy } from '../src/shared/entitlement-correction-i18n.js';
import { financeCopy } from '../src/shared/finance-i18n.js';
import { encodeEntitlementCorrectionJSON } from '../src/features/account-platform/admin-entitlement-correction.js';

const browserPath = [process.env.CHROME, 'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe', 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'].find(value => value && existsSync(value));
assert.ok(browserPath, 'Chromium is required');
const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const id = suffix => `00000000-0000-4000-8000-${String(suffix).padStart(12, '0')}`;
const actorA = id(101), actorB = id(102), packageID = id(1), orderID = id(2), requestID = id(20);
const proof = { command_type: 'ENTITLEMENT_COMPONENT_ADJUSTMENT', policy_version: 2, snapshot_schema_version: 2,
  target_resource_type: 'ENTITLEMENT_PACKAGE', target_resource_id: packageID,
  normalized_command: { package_id: packageID, payment_order_id: orderID, reason_code: 'CORRECTION' },
  critical_resource_versions: { package_id: packageID, wallet_id: id(3), wallet_version: '9223372036854775807', payment_order_id: orderID,
    payment_transaction_id: id(4), source_component_id: id(5), source_lot_id: id(6), target_component_id: null, target_lot_id: null,
    source_type: 'BASE', target_type: 'BONUS', amount_microcredits: '9007199254740993', source_evidence_digest: 'a'.repeat(64), facts_digest: 'b'.repeat(64), policy_version: 2 },
  financial_snapshot: { package_total_microcredits: '9223372036854775807', source_original_microcredits: '9223372036854775807', target_original_microcredits: '0', effective_total_microcredits: '9223372036854775807' } };
let actor = actorA, csrf = 'csrf-initial', permission = true, writes = [], effects = 0, status = '', mode = 'normal', auths = 0, version = 1, updates = [];
let releaseCreate, createWaiting, submitCalls;
const json = (response, payload, code = 200) => { response.writeHead(code, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); response.end(encodeEntitlementCorrectionJSON(payload)); };
const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/auth/login') {
    assert.equal(url.searchParams.get('reauthenticate'), '1'); csrf = `csrf-new-${++auths}`;
    response.writeHead(302, { Location: url.searchParams.get('return_to') }); return response.end();
  }
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: csrf });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: actor, status: 'ACTIVE', personal_permissions: [], platform_permissions: permission ? ['platform.billing.repairs.request'] : [] });
  if (url.pathname === '/api/admin/account/admin/v2/approval-command-prechecks') {
    assert.equal(request.method, 'POST'); assert.equal(request.headers['x-csrf-token'], csrf);
    const chunks = []; for await (const chunk of request) chunks.push(chunk);
    assert.deepEqual(JSON.parse(Buffer.concat(chunks).toString()), { command_type: proof.command_type, target_resource_id: packageID, payment_order_id: orderID, reason_code: 'CORRECTION' });
    assert.equal(effects, 0, 'precheck is not a write');
    return json(response, mode === 'foreign-proof' ? { ...proof, target_resource_id: id(99) } : proof);
  }
  if (url.pathname === '/api/admin/approvals/capability') return json(response, { session_approval_wire_version: 2, control: { version: 1, epoch: 1, authorization_relay_epoch: 1 } });
  if (url.pathname === '/api/admin/approvals/requests' && request.method === 'GET') return json(response, { items: status ? [{ approval_request_id: requestID, command_type: proof.command_type, target: { resource_type: proof.target_resource_type, resource_id: packageID }, status, version }] : [], has_more: false });
  if (url.pathname === '/api/admin/approvals/requests' && request.method === 'POST') {
    assert.equal(request.headers['x-csrf-token'], csrf);
    const chunks = []; for await (const chunk of request) chunks.push(chunk);
    const raw = Buffer.concat(chunks).toString(); const body = JSON.parse(raw);
    assert.match(raw, /"wallet_version":9223372036854775807[,}]/);
    assert.match(raw, /"amount_microcredits":9007199254740993[,}]/);
    assert.ok(body.idempotency_key.length <= 120);
    writes.push({ raw, actor, csrf });
    if (mode === 'create-denied') return json(response, { code: 'PERMISSION_DENIED' }, 403);
    if (writes.length > 1) { assert.equal(raw, writes[0].raw); assert.equal(actor, writes[0].actor); }
    else { effects++; status = mode === 'unknown' ? 'SUBMITTED' : 'DRAFT'; }
    if (mode === 'late') { createWaiting = true; await new Promise(resolve => { releaseCreate = resolve; }); }
    if (mode === 'unknown') {
      if (writes.length === 1) return json(response, { code: 'COMMAND_RESULT_UNKNOWN' }, 503);
      if (writes.length === 2) return json(response, { code: 'RECENT_AUTHENTICATION_REQUIRED' }, 401);
      if (writes.length === 3) return json(response, { code: 'PERMISSION_DENIED' }, 403);
      if (writes.length === 4) return json(response, { code: 'APPROVAL_FACTS_CHANGED' }, 409);
    }
    return json(response, { approval_request_id: requestID }, 201);
  }
  if (url.pathname === `/api/admin/approvals/requests/${requestID}` && request.method === 'PUT') {
    assert.equal(request.headers['x-csrf-token'], csrf);
    const chunks = []; for await (const chunk of request) chunks.push(chunk); const raw = Buffer.concat(chunks).toString();
    const body = JSON.parse(raw); assert.equal(body.expected_version, 7); updates.push(raw);
    if (updates.length === 1) { effects++; version = 8; if (mode !== 'edit-submit-denied') return json(response, { code: 'COMMAND_RESULT_UNKNOWN' }, 503); }
    else assert.equal(raw, updates[0], 'edit retry must preserve the original CAS and full exact input');
    response.writeHead(204); return response.end();
  }
  if (url.pathname === `/api/admin/approvals/requests/${requestID}` && request.method === 'GET') return json(response, { ...proof, approval_request_id: requestID, version, action_control_version: 1, detail_availability: 'AVAILABLE', target: { resource_type: proof.target_resource_type, resource_id: packageID }, status, allowed_actions: status === 'DRAFT' ? ['EDIT', 'SUBMIT'] : [] });
  if (url.pathname === `/api/admin/approvals/requests/${requestID}/actions/submit`) { assert.equal(request.method, 'POST'); assert.equal(request.headers['x-csrf-token'], csrf); submitCalls++; if (['submit-denied', 'edit-submit-denied'].includes(mode) && submitCalls === 1) return json(response, { code: 'PERMISSION_DENIED' }, 403); status = 'SUBMITTED'; version++; response.writeHead(204); return response.end(); }
  if (url.pathname.startsWith('/api/')) return json(response, { code: 'UNEXPECTED_TEST_ROUTE' }, 404);
  const file = path.resolve(staticRoot, `.${url.pathname}`);
  if (!file.startsWith(`${staticRoot}${path.sep}`)) return json(response, {}, 404);
  try { const contents = await readFile(file); response.writeHead(200, { 'Content-Type': file.endsWith('.html') ? 'text/html; charset=utf-8' : file.endsWith('.css') ? 'text/css' : 'text/javascript' }); response.end(contents); }
  catch { json(response, {}, 404); }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
const browser = await puppeteer.launch({ executablePath: browserPath, headless: true, args: ['--no-sandbox'] });
let passed = 0;
const reset = () => { actor = actorA; csrf = 'csrf-initial'; permission = true; writes = []; effects = 0; status = ''; mode = 'normal'; auths = 0; version = 1; updates = []; submitCalls = 0; };
const click = async (page, text) => {
  await page.waitForFunction(label => [...document.querySelectorAll('button')].some(button => button.textContent === label && !button.disabled), { timeout: 5000 }, text);
  await page.evaluate(label => [...document.querySelectorAll('button')].find(button => button.textContent === label).click(), text);
};
const open = async (language = 'en', setup) => {
  const page = await browser.newPage(); await page.evaluateOnNewDocument(locale => localStorage.setItem('pinvou.admin.language', locale), language);
  if (setup) await page.evaluateOnNewDocument(setup);
  await page.goto(`${origin}/admin.html?section=entitlement-correction`, { waitUntil: 'networkidle0' }); return page;
};
const prepare = async (page, language = 'en') => {
  await page.waitForSelector('section input'); const inputs = await page.$$('section input'); assert.equal(inputs.length, 3);
  await inputs[0].type(packageID); await inputs[1].type(orderID); await inputs[2].type('CORRECTION');
  await click(page, entitlementCorrectionCopy[language].precheck);
  await page.waitForSelector('section dl'); assert.equal(effects, 0);
  assert.ok(await page.evaluate(() => document.querySelector('section dl').textContent.includes('9007199254740993')));
  await page.type('section textarea', 'Verified original payment product split');
};
try {
  for (const language of ['zh', 'en', 'ja']) {
    reset(); const page = await open(language); await prepare(page, language); await click(page, entitlementCorrectionCopy[language].submit);
    await page.waitForSelector('section article'); assert.equal(effects, 1); assert.equal(status, 'SUBMITTED');
    assert.ok(await page.evaluate(text => document.body.textContent.includes(text), entitlementCorrectionCopy[language].noFinancialAck));
    passed++; console.log(`PASS compiled ${language} exact proof / precheck zero writes / confirmed approval is not funds execution`); await page.close();
  }
  reset(); mode = 'unknown'; const page = await open(); await prepare(page); await click(page, entitlementCorrectionCopy.en.submit);
  await page.waitForFunction(text => document.body.textContent.includes(text), {}, entitlementCorrectionCopy.en.recoveryPending);
  await page.reload({ waitUntil: 'networkidle0' }); assert.equal(writes.length, 1);
  await click(page, financeCopy.en.retry);
  await Promise.all([page.waitForNavigation({ waitUntil: 'networkidle0' }), click(page, entitlementCorrectionCopy.en.reauthenticate)]);
  await page.reload({ waitUntil: 'networkidle0' }); assert.equal(auths, 1);
  for (let count = 3; count <= 4; count++) { await click(page, financeCopy.en.retry); await page.waitForFunction(label => [...document.querySelectorAll('button')].some(button => button.textContent === label && !button.disabled), {}, financeCopy.en.retry); assert.equal(writes.length, count); await page.reload({ waitUntil: 'networkidle0' }); }
  await click(page, financeCopy.en.retry); await page.waitForSelector('section article'); assert.equal(effects, 1); assert.ok(writes.at(-1).csrf.startsWith('csrf-new-'));
  assert.equal(await page.evaluate(() => Object.keys(document.defaultView.sessionStorage).filter(key => key.startsWith('pinvou.entitlement-correction.intent.v1:')).length), 0);
  passed++; console.log('PASS compiled unknown 503 → actual reauthentication/new CSRF/client → 403/409/full reload → exact original key/body ACK'); await page.close();
  reset(); mode = 'unknown'; const foreign = await open(); await prepare(foreign); await click(foreign, entitlementCorrectionCopy.en.submit);
  await foreign.waitForFunction(text => document.body.textContent.includes(text), {}, entitlementCorrectionCopy.en.recoveryPending);
  actor = actorB; await foreign.reload({ waitUntil: 'networkidle0' }); assert.equal(writes.length, 1); assert.equal(await foreign.$('textarea'), null);
  passed++; console.log('PASS compiled another actor cannot recover or send the prior actor invocation'); await foreign.close();
  reset(); mode = 'submit-denied'; const denied = await open(); await prepare(denied); await click(denied, entitlementCorrectionCopy.en.submit);
  await denied.waitForFunction(text => document.body.textContent.includes(text), {}, entitlementCorrectionCopy.en.recoveryPending);
  assert.equal(effects, 1); assert.equal(status, 'DRAFT');
  // The first create already wrote a draft: a later 403 must retain its key.
  status = 'SUBMITTED'; version = 2;
  await denied.reload({ waitUntil: 'networkidle0' }); await click(denied, financeCopy.en.retry); await denied.waitForSelector('section article');
  assert.equal(effects, 1); assert.equal(submitCalls, 1); assert.equal(writes.length, 3);
  passed++; console.log('PASS compiled first create ACK followed by submit 403 retains original invocation across reload and confirms only same-key receipt'); await denied.close();
  reset(); mode = 'edit-submit-denied'; status = 'DRAFT'; version = 7; const editDenied = await open(); await prepare(editDenied); await click(editDenied, entitlementCorrectionCopy.en.submit);
  await editDenied.waitForFunction(text => document.body.textContent.includes(text), {}, entitlementCorrectionCopy.en.recoveryPending);
  assert.equal(effects, 1); assert.equal(version, 8); assert.equal(updates.length, 1);
  await editDenied.reload({ waitUntil: 'networkidle0' }); await click(editDenied, financeCopy.en.retry); await editDenied.waitForSelector('section article');
  assert.equal(effects, 1); assert.equal(updates.length, 2); assert.equal(submitCalls, 2);
  passed++; console.log('PASS compiled edit ACK then submit 403 preserves original version/key and replays its receipt after reload'); await editDenied.close();
  reset(); mode = 'create-denied'; const firstDenied = await open(); await prepare(firstDenied); await click(firstDenied, entitlementCorrectionCopy.en.submit);
  await firstDenied.waitForFunction(text => document.querySelector('[role="alert"]')?.textContent === text, {}, financeCopy.en.permissions);
  assert.equal(effects, 0); assert.equal(await firstDenied.evaluate(() => Object.keys(document.defaultView.sessionStorage).filter(key => key.startsWith('pinvou.entitlement-correction.intent.v1:')).length), 0);
  passed++; console.log('PASS compiled initial create 403 without any request receipt safely clears the unsent intent'); await firstDenied.close();
  reset(); mode = 'edit'; status = 'DRAFT'; version = 7; const edit = await open(); await prepare(edit); await click(edit, entitlementCorrectionCopy.en.submit);
  await edit.waitForFunction(text => document.body.textContent.includes(text), {}, entitlementCorrectionCopy.en.recoveryPending);
  assert.equal(version, 8); assert.equal(updates.length, 1); assert.equal(writes.length, 0);
  await edit.reload({ waitUntil: 'networkidle0' }); await click(edit, financeCopy.en.retry); await edit.waitForSelector('section article');
  assert.equal(updates.length, 2); assert.equal(effects, 1); assert.equal(status, 'SUBMITTED'); assert.equal(writes.length, 0);
  passed++; console.log('PASS compiled committed existing-draft edit → lost ACK → new client/full reload → original version 7 CAS and key replay despite current version 8'); await edit.close();
  reset(); mode = 'edit'; status = 'DRAFT'; version = 7;
  const rejectedCAS = await open('en', () => { const original = document.defaultView.Storage.prototype.setItem; document.defaultView.Storage.prototype.setItem = function(key, value) { if (key.startsWith('pinvou.entitlement-correction.intent.v1:') && JSON.parse(value).editReference) throw new Error('CAS storage denied'); return original.call(this, key, value); }; });
  await prepare(rejectedCAS); await click(rejectedCAS, entitlementCorrectionCopy.en.submit);
  await rejectedCAS.waitForFunction(label => [...document.querySelectorAll('button')].some(button => button.textContent === label && !button.disabled), {}, financeCopy.en.retry);
  assert.equal(updates.length, 0); assert.equal(effects, 0); passed++; console.log('PASS compiled existing edit CAS persistence failure sends zero PUT'); await rejectedCAS.close();
  for (const behavior of ['unwritable', 'mismatch']) {
    reset(); const storagePage = await open('en', behavior === 'unwritable' ? () => { const original = document.defaultView.Storage.prototype.setItem; document.defaultView.Storage.prototype.setItem = function(key, value) { if (key.startsWith('pinvou.entitlement-correction.intent.v1:')) throw new Error('denied'); return original.call(this, key, value); }; } : () => { const original = document.defaultView.Storage.prototype.setItem; document.defaultView.Storage.prototype.setItem = function(key, value) { if (!key.startsWith('pinvou.entitlement-correction.intent.v1:')) original.call(this, key, value); }; });
    await prepare(storagePage); await click(storagePage, entitlementCorrectionCopy.en.submit);
    await storagePage.waitForFunction(text => document.body.textContent.includes(text), {}, entitlementCorrectionCopy.en.recoveryUnavailable); assert.equal(writes.length, 0);
    passed++; console.log(`PASS compiled storage ${behavior} sends no approval command`); await storagePage.close();
  }
  reset(); const uncleared = await open('en', () => { const original = document.defaultView.Storage.prototype.removeItem; document.defaultView.Storage.prototype.removeItem = function(key) { if (!key.startsWith('pinvou.entitlement-correction.intent.v1:')) original.call(this, key); }; });
  await prepare(uncleared); await click(uncleared, entitlementCorrectionCopy.en.submit);
  await uncleared.waitForFunction(text => document.body.textContent.includes(text), {}, entitlementCorrectionCopy.en.recoveryUnavailable);
  assert.equal(effects, 1); assert.equal(await uncleared.$('section article'), null);
  assert.equal(await uncleared.evaluate(() => Object.keys(document.defaultView.sessionStorage).filter(key => key.startsWith('pinvou.entitlement-correction.intent.v1:')).length), 1);
  passed++; console.log('PASS compiled receipt with failed storage clear keeps evidence and blocks further commands'); await uncleared.close();
  reset(); mode = 'late'; createWaiting = false; const late = await open(); await prepare(late); await click(late, entitlementCorrectionCopy.en.submit);
  const deadline = Date.now() + 5000; while (!createWaiting && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 10)); assert.equal(createWaiting, true);
  await late.evaluate(() => { const storageKey = Object.keys(document.defaultView.sessionStorage).find(key => key.startsWith('pinvou.entitlement-correction.intent.v1:')); const intent = JSON.parse(document.defaultView.sessionStorage.getItem(storageKey)); intent.key = 'later-distinct-invocation'; document.defaultView.sessionStorage.setItem(storageKey, JSON.stringify(intent)); });
  releaseCreate(); await late.waitForSelector('section article');
  assert.equal(await late.evaluate(() => JSON.parse(document.defaultView.sessionStorage.getItem(Object.keys(document.defaultView.sessionStorage).find(key => key.startsWith('pinvou.entitlement-correction.intent.v1:')))).key), 'later-distinct-invocation');
  await late.reload({ waitUntil: 'networkidle0' }); await late.waitForFunction(text => document.body.textContent.includes(text), {}, entitlementCorrectionCopy.en.recoveryPending);
  assert.equal(writes.length, 1); passed++; console.log('PASS compiled late receipt cannot clear a newer durable key or automatically send after reload'); await late.close();
  reset(); permission = false; const readonly = await open(); assert.equal(await readonly.$('section input'), null); assert.equal(writes.length, 0); passed++; console.log('PASS compiled missing request permission has no correction form'); await readonly.close();
  reset(); mode = 'foreign-proof'; const invalid = await open();
  await invalid.waitForSelector('section input'); const inputs = await invalid.$$('section input'); await inputs[0].type(packageID); await inputs[1].type(orderID); await inputs[2].type('CORRECTION');
  await click(invalid, entitlementCorrectionCopy.en.precheck); await invalid.waitForFunction(text => document.body.textContent.includes(text), {}, entitlementCorrectionCopy.en.unavailable);
  assert.equal(await invalid.$('section textarea'), null); assert.equal(writes.length, 0); passed++; console.log('PASS compiled foreign owner proof cannot produce a draft'); await invalid.close();
  console.log(`ENTITLEMENT_COMPILED_PASS ${passed} FAIL 0 SKIP 0`);
} finally { await browser.close(); server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); }
