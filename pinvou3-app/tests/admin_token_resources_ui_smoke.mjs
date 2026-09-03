import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const browserPath = [
  process.env.CHROME,
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium or Edge is required');

const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const accountID = '0199a100-0001-7000-8000-000000000101';
const ownerID = '0199a100-0001-7000-8000-000000000102';
const modelID = '0199a100-0001-7000-8000-000000000103';
const protocolID = '0199a100-0001-7000-8000-000000000104';
const routeID = '0199a100-0001-7000-8000-000000000105';
const allPermissions = ['platform.tokens.read', 'platform.tokens.manage', 'platform.tokens.owner-candidates.read', 'platform.models.read'];

let permissions = [...allPermissions];
let formsEnabled = true;
let candidateDelay = false;
let modelDelay = false;
let routeMode = 'success';
const observations = { candidateCalls: 0, candidateAborted: 0, modelCalls: 0, modelAborted: 0, routeCalls: [] };

function envelope(items) {
  return { items, next_cursor: '', has_more: false, limit: 50, as_of: '2026-09-04T12:00:00Z', snapshot_expires_at: '2026-09-04T12:15:00Z', request_id: 'request-test' };
}

function json(response, payload, status = 200) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}

function delayed(request, response, payload, onAbort) {
  const timer = setTimeout(() => json(response, payload), 5000);
  const abort = () => {
    clearTimeout(timer);
    onAbort();
  };
  request.once('aborted', abort);
  response.once('close', () => { if (!response.writableEnded) abort(); });
}

const account = {
  token_account_id: accountID,
  owner_type: 'USER',
  owner_id: ownerID,
  wallet_id: '0199a100-0001-7000-8000-000000000106',
  name: 'Production account',
  status: 'ACTIVE',
  share_scope: 'OWNER_UNIT_ONLY',
  management_mode: 'PLATFORM',
  active_credential_count: 1,
  route_count: 1,
  account_version: 2,
  updated_at: '2026-09-04T11:59:00Z',
};
const accountDetail = {
  ...account,
  as_of: '2026-09-04T12:00:00Z',
  credentials: [],
  routes: [{
    route_id: routeID,
    token_account_id: accountID,
    model: 'flash',
    model_id: modelID,
    protocol_version_id: protocolID,
    protocol_code: 'CHAT',
    priority: 100,
    default_scope: 'NONE',
    reference_status: 'RESOLVED',
    health_status: 'HEALTHY',
    health_diagnostic_code: 'PROBE_SUCCEEDED',
    health_observed_at: '2026-09-04T11:59:00Z',
    health_expires_at: '2026-09-04T12:04:00Z',
    route_version: 1,
    route_generation: 1,
    updated_at: '2026-09-04T11:59:00Z',
  }],
};
const model = {
  model_id: modelID,
  catalog_version: 1,
  display_name: 'Flash',
  description: '',
  aliases: [],
  lifecycle_status: 'PUBLISHED',
  protocol_code: 'CHAT',
  protocol_version: 1,
  protocol_definition_id: protocolID,
  model_capability_version_id: '0199a100-0001-7000-8000-000000000107',
  capability_version: 1,
  context_length: 128000,
  maximum_output_tokens: 8192,
  supported_modalities: ['TEXT'],
  tokenizer_version: 'v1',
  source_available: true,
  source_missing_confirmations: 0,
  force_cancel_inflight: false,
};

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/configure') {
    const nextPermissions = url.searchParams.get('permissions');
    if (nextPermissions === 'all') permissions = [...allPermissions];
    if (nextPermissions === 'no-owner') permissions = allPermissions.filter((value) => value !== 'platform.tokens.owner-candidates.read');
    if (nextPermissions === 'no-model') permissions = allPermissions.filter((value) => value !== 'platform.models.read');
    if (url.searchParams.has('gate')) formsEnabled = url.searchParams.get('gate') === '1';
    if (url.searchParams.has('candidate-delay')) candidateDelay = url.searchParams.get('candidate-delay') === '1';
    if (url.searchParams.has('model-delay')) modelDelay = url.searchParams.get('model-delay') === '1';
    if (url.searchParams.has('route-mode')) routeMode = url.searchParams.get('route-mode');
    return json(response, { permissions, formsEnabled, routeMode });
  }
  if (url.pathname === '/__test__/state') return json(response, observations);
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf-test', feature_flags: { admin_model_token_ia: true, admin_token_resource_forms: formsEnabled } });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: ownerID, platform_permissions: permissions });
  if (url.pathname === '/api/admin/token/admin/v1/token-accounts') return json(response, envelope([account]));
  if (url.pathname === `/api/admin/token/admin/v1/token-accounts/${accountID}`) return json(response, accountDetail);
  if (url.pathname === '/api/admin/token/admin/v1/owner-candidates') {
    observations.candidateCalls += 1;
    const payload = envelope([{ id: ownerID, type: 'USER', display_label: 'Alice', status: 'ACTIVE' }]);
    if (candidateDelay) return delayed(request, response, payload, () => { observations.candidateAborted += 1; });
    return json(response, payload);
  }
  if (url.pathname === '/api/admin/token/admin/v1/models') {
    observations.modelCalls += 1;
    const payload = envelope([model]);
    if (modelDelay) return delayed(request, response, payload, () => { observations.modelAborted += 1; });
    return json(response, payload);
  }
  if (url.pathname === `/api/admin/token/admin/v2/token-accounts/${accountID}/routes` && request.method === 'PUT') {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    observations.routeCalls.push({ key: request.headers['idempotency-key'], body: JSON.parse(Buffer.concat(chunks).toString('utf8')) });
    if (routeMode === 'unknown') return json(response, { code: 'INTERNAL_ERROR', request_id: 'unknown-request' }, 500);
    if (routeMode === 'mismatch') return json(response, { code: 'INTERNAL_ERROR', request_id: 'mismatch-request' }, 409);
    if (routeMode === 'safe') return json(response, { code: 'TOKEN_ROUTE_VERSION_CONFLICT', request_id: 'safe-request' }, 409);
    if (routeMode === 'delay-success') return setTimeout(() => json(response, { route_id: routeID }), 250);
    return json(response, { route_id: routeID });
  }
  const relative = url.pathname === '/admin.html' ? 'admin.html' : url.pathname.replace(/^\//, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot) || !existsSync(target)) { response.writeHead(404); return response.end('not found'); }
  const extension = path.extname(target);
  const type = extension === '.html' ? 'text/html' : extension === '.css' ? 'text/css' : 'text/javascript';
  response.writeHead(200, { 'Content-Type': `${type}; charset=utf-8`, 'Cache-Control': 'no-store' });
  response.end(await readFile(target));
});

await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
const { port } = server.address();
const origin = `http://127.0.0.1:${port}`;
const browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });

async function configure(page, query) {
  await page.evaluate((value) => fetch(`/__test__/configure?${value}`), query);
}

async function state(page) {
  return page.evaluate(() => fetch('/__test__/state').then((response) => response.json()));
}

async function openRouteDialog(page) {
  if (!await page.$('.adm-token-health')) {
    await page.goto(`${origin}/admin.html?section=tokenResources&token_account_id=${accountID}&token_detail_tab=routes`, { waitUntil: 'networkidle0' });
  }
  await page.waitForSelector('.adm-token-health .adm-row-actions button:first-child');
  await page.click('.adm-token-health .adm-row-actions button:first-child');
  await page.waitForSelector('[aria-labelledby="token-route-dialog-title"] select');
  await page.waitForFunction((value) => [...document.querySelectorAll('[aria-labelledby="token-route-dialog-title"] option')].some((option) => option.value === value), {}, modelID);
  await page.evaluate((value) => {
    const select = [...document.querySelectorAll('[aria-labelledby="token-route-dialog-title"] select')].find((item) => [...item.options].some((option) => option.value === value));
    select.value = value;
    select.dispatchEvent(new Event('change', { bubbles: true }));
  }, modelID);
  await page.waitForFunction((value) => [...document.querySelectorAll('[aria-labelledby="token-route-dialog-title"] select')].some((item) => [...item.options].some((option) => option.value === value)), {}, protocolID);
  await page.evaluate((value) => {
    const select = [...document.querySelectorAll('[aria-labelledby="token-route-dialog-title"] select')].find((item) => [...item.options].some((option) => option.value === value));
    select.value = value;
    select.dispatchEvent(new Event('change', { bubbles: true }));
  }, protocolID);
  await page.type('[aria-labelledby="token-route-dialog-title"] textarea', 'configure stable route safely');
}

async function submitRoute(page, twice = false) {
  await page.evaluate((double) => {
    const button = document.querySelector('[aria-labelledby="token-route-dialog-title"] footer button:last-child');
    button.click();
    if (double) button.click();
  }, twice);
}

try {
  const page = await browser.newPage();
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.evaluateOnNewDocument(() => {
    localStorage.setItem('pinvou.admin.language', 'en');
    globalThis.__tokenResourceTest = { replaceCount: 0 };
    const replace = history.replaceState.bind(history);
    history.replaceState = (...args) => {
      globalThis.__tokenResourceTest.replaceCount += 1;
      return replace(...args);
    };
  });

  await page.goto(`${origin}/admin.html?section=tokenResources`, { waitUntil: 'networkidle0' });
  await configure(page, 'permissions=all&gate=1&candidate-delay=1');
  await page.evaluate(() => { globalThis.__tokenResourceTest.replaceCount = 0; });
  await page.click('.adm-source-head .adm-row-actions button:first-child');
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((value) => value.candidateCalls >= 1));
  await configure(page, 'permissions=no-owner');
  await page.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
  await page.waitForFunction(() => !document.querySelector('[aria-labelledby="token-owner-dialog-title"]'));
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((value) => value.candidateAborted >= 1));
  assert.equal(await page.evaluate(() => globalThis.__tokenResourceTest.replaceCount), 1, 'permission revoke cleans owner URL in one replace');
  assert.doesNotMatch(page.url(), /owner_candidate_/);
  const revokedCandidateCalls = (await state(page)).candidateCalls;
  await new Promise((resolve) => setTimeout(resolve, 80));
  assert.equal((await state(page)).candidateCalls, revokedCandidateCalls, 'revoked UI issues no hidden candidate request');

  await configure(page, 'permissions=all&candidate-delay=0&model-delay=1');
  await page.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
  await page.waitForFunction(() => document.querySelectorAll('.adm-source-head .adm-row-actions button').length === 2);
  await page.click('tbody tr button');
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.match(page.url(), /token_account_id=/, `route action did not update URL: ${await page.$eval('main', (node) => node.textContent)}`);
  await page.waitForSelector('.adm-token-health');
  assert.equal(await page.$eval('.adm-token-health tbody tr td:last-child', (node) => node.textContent.trim()), 'Yes', 'route health uses the response as_of boundary');
  await page.click('.adm-token-health .adm-row-actions button:first-child');
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((value) => value.modelCalls >= 1));
  await configure(page, 'permissions=no-model');
  await page.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
  await page.waitForFunction(() => !document.querySelector('[aria-labelledby="token-route-dialog-title"]'));
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((value) => value.modelAborted >= 1));
  const revokedModelCalls = (await state(page)).modelCalls;
  await new Promise((resolve) => setTimeout(resolve, 80));
  assert.equal((await state(page)).modelCalls, revokedModelCalls, 'revoked UI issues no hidden model request');

  await configure(page, 'permissions=all&gate=0&model-delay=0');
  const gateCandidateCalls = (await state(page)).candidateCalls;
  await page.goto(`${origin}/admin.html?section=tokenResources&owner_candidate_type=USER&owner_candidate_status=ACTIVE`, { waitUntil: 'networkidle0' });
  assert.equal(await page.evaluate(() => globalThis.__tokenResourceTest.replaceCount), 1, 'gate-off deep link is normalized in one replace');
  assert.doesNotMatch(page.url(), /owner_candidate_/);
  assert.equal(await page.$$eval('[aria-labelledby="token-owner-dialog-title"]', (nodes) => nodes.length), 0);
  assert.equal((await state(page)).candidateCalls, gateCandidateCalls, 'gate-off deep link performs no candidate request');

  await configure(page, 'permissions=all&gate=1&route-mode=delay-success');
  await openRouteDialog(page);
  const beforeDouble = (await state(page)).routeCalls.length;
  await submitRoute(page, true);
  await page.waitForFunction((before) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.routeCalls.length === before + 1), {}, beforeDouble);
  await page.waitForFunction(() => !document.querySelector('[aria-labelledby="token-route-dialog-title"]'));
  assert.equal((await state(page)).routeCalls.length, beforeDouble + 1, 'same-tick double click dispatches one stable route write');

  await configure(page, 'route-mode=unknown');
  await openRouteDialog(page);
  const beforeUnknown = (await state(page)).routeCalls.length;
  await submitRoute(page);
  await page.waitForSelector('[aria-labelledby="token-route-dialog-title"] [role="alert"]');
  const unknownCall = (await state(page)).routeCalls.at(-1);
  await submitRoute(page);
  await new Promise((resolve) => setTimeout(resolve, 80));
  const afterUnknown = await state(page);
  assert.equal(afterUnknown.routeCalls.length, beforeUnknown + 1, 'unknown outcome remains locked and cannot blind-retry');
  assert.equal(afterUnknown.routeCalls.at(-1).key, unknownCall.key, 'unknown outcome retains the frozen command key');
  await page.click('[aria-labelledby="token-route-dialog-title"] header button');

  await configure(page, 'route-mode=mismatch');
  await openRouteDialog(page);
  const beforeMismatch = (await state(page)).routeCalls.length;
  await submitRoute(page);
  await page.waitForSelector('[aria-labelledby="token-route-dialog-title"] [role="alert"]');
  const mismatchCall = (await state(page)).routeCalls.at(-1);
  await page.$eval('[aria-labelledby="token-route-dialog-title"] input[type="number"]', (input) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, '101');
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
  await submitRoute(page);
  await new Promise((resolve) => setTimeout(resolve, 80));
  const afterMismatch = await state(page);
  assert.equal(afterMismatch.routeCalls.length, beforeMismatch + 1, 'mismatched 409 status/code locks even a changed payload');
  assert.equal(afterMismatch.routeCalls.at(-1).key, mismatchCall.key, 'mismatched response retains the original key');
  await page.click('[aria-labelledby="token-route-dialog-title"] header button');

  await configure(page, 'route-mode=safe');
  await openRouteDialog(page);
  const beforeSafe = (await state(page)).routeCalls.length;
  await submitRoute(page);
  await page.waitForSelector('[aria-labelledby="token-route-dialog-title"] [role="alert"]');
  await submitRoute(page);
  await page.waitForFunction((before) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.routeCalls.length === before + 2), {}, beforeSafe);
  await page.$eval('[aria-labelledby="token-route-dialog-title"] input[type="number"]', (input) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, '101');
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
  await submitRoute(page);
  await page.waitForFunction((before) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.routeCalls.length === before + 3), {}, beforeSafe);
  const safeCalls = (await state(page)).routeCalls.slice(beforeSafe);
  assert.equal(safeCalls[0].key, safeCalls[1].key, 'exact zero-write status/code reuses key for the same payload');
  assert.notEqual(safeCalls[1].key, safeCalls[2].key, 'payload change after proven zero-write gets a new key');
  assert.equal(safeCalls[2].body.priority, 101);

  assert.deepEqual(pageErrors, []);
  console.log('Admin Token resources UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
