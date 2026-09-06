import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import puppeteer from 'puppeteer-core';
import { listenOnChromiumSafePort } from './chromium-safe-port.mjs';

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
let actionsEnabled = true;
let candidateDelay = false;
let modelDelay = false;
let routeMode = 'success';
let accountStatus = 'ACTIVE';
let accountCreateMode = 'success';
let persistedQueue = false;
let queueFailure = false;
const statusCommandID = '0199a100-0001-7000-8000-000000000108';
const recoveryActionID = '0199a100-0001-7000-8000-000000000109';
const recoveryAuditID = '0199a100-0001-7000-8000-000000000110';
const observations = { candidateCalls: 0, candidateAborted: 0, modelCalls: 0, modelAborted: 0, impactCalls: 0, routeCalls: [], commandCalls: [], recoveryCalls: [], queueCalls: [] };

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
const impactDependencies = Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map((key) => [key, 0]));
function routeImpact(action) {
  return {
    action: `TOKEN_ROUTE_${action}`,
    precheck_fingerprint: 'a'.repeat(64),
    requires_cas_revalidation: true,
    snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS',
    token_observed_at: '2026-09-04T12:00:00Z',
    blockers: [],
    evidence: { resource_type: 'TOKEN_ROUTE', resource_id: accountID, current_status: 'ACTIVE', current_version: { evidence_etag: `"impact-${'b'.repeat(64)}"`, evidence_set_sha256: 'c'.repeat(64) }, dependencies: impactDependencies },
  };
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/configure') {
    const nextPermissions = url.searchParams.get('permissions');
    if (nextPermissions === 'all') permissions = [...allPermissions];
    if (nextPermissions === 'no-owner') permissions = allPermissions.filter((value) => value !== 'platform.tokens.owner-candidates.read');
    if (nextPermissions === 'no-model') permissions = allPermissions.filter((value) => value !== 'platform.models.read');
    if (url.searchParams.has('gate')) formsEnabled = url.searchParams.get('gate') === '1';
    if (url.searchParams.has('action-gate')) actionsEnabled = url.searchParams.get('action-gate') === '1';
    if (url.searchParams.has('candidate-delay')) candidateDelay = url.searchParams.get('candidate-delay') === '1';
    if (url.searchParams.has('model-delay')) modelDelay = url.searchParams.get('model-delay') === '1';
    if (url.searchParams.has('route-mode')) routeMode = url.searchParams.get('route-mode');
    if (url.searchParams.has('account-status')) accountStatus = url.searchParams.get('account-status');
    if (url.searchParams.has('account-create-mode')) accountCreateMode = url.searchParams.get('account-create-mode');
    if (url.searchParams.has('queue')) persistedQueue = url.searchParams.get('queue') === '1';
    if (url.searchParams.has('queue-failure')) queueFailure = url.searchParams.get('queue-failure') === '1';
    return json(response, { permissions, formsEnabled, actionsEnabled, routeMode, accountStatus, accountCreateMode });
  }
  if (url.pathname === '/__test__/state') return json(response, observations);
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf-test', feature_flags: { admin_model_token_ia: true, admin_token_resource_forms: formsEnabled, admin_model_token_actions: actionsEnabled } });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: ownerID, platform_permissions: permissions });
  if (url.pathname === '/api/admin/token/admin/v1/token-accounts') return json(response, envelope([{ ...account, status: accountStatus }]));
  if (url.pathname === '/api/admin/token/admin/v1/commands/recovery-queue') {
    observations.queueCalls.push(url.search);
    if (queueFailure) return json(response, { code: 'SOURCE_UNAVAILABLE', request_id: 'recovery-queue-failure' }, 503);
    const nextPage = Boolean(url.searchParams.get('cursor'));
    const item = { command_reference: nextPage ? recoveryActionID : statusCommandID, command_type: 'TOKEN_ACCOUNT_CREATE', status: 'PENDING', recovery_state: 'MANUAL_REVIEW', recovery_epoch: 3, created_at: '2026-09-04T12:00:00Z', updated_at: '2026-09-04T12:00:01Z' };
    return json(response, { ...envelope(persistedQueue ? [item] : []), has_more: persistedQueue && !nextPage, next_cursor: persistedQueue && !nextPage ? 'opaque-account-recovery-next-page-cursor' : '' });
  }
  if (url.pathname === `/api/admin/token/admin/v1/token-accounts/${accountID}`) return json(response, { ...accountDetail, status: accountStatus });
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
  if (url.pathname === `/api/admin/token/admin/v2/token-accounts/${accountID}/impact/route`) {
    observations.impactCalls += 1;
    return json(response, routeImpact(url.searchParams.get('action')));
  }
  if (url.pathname === `/api/admin/token/admin/v1/token-accounts/${accountID}/impact/status`) {
    return json(response, { ...routeImpact('UPSERT'), action: `TOKEN_ACCOUNT_STATUS_${url.searchParams.get('target_status')}`, evidence: { ...routeImpact('UPSERT').evidence, resource_type: 'TOKEN_ACCOUNT', resource_id: accountID } });
  }
  if (url.pathname === '/api/admin/token/admin/v1/commands:precheck' && request.method === 'POST') {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const input = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    return json(response, {
      ...routeImpact('UPSERT'), action: input.command_type,
      evidence: { ...routeImpact('UPSERT').evidence, resource_type: input.target_resource_type, resource_id: input.target_resource_id, current_status: accountStatus },
      expected_versions: { token_account_id: accountID, expected_account_version: 2, target_status: input.command.target_status },
    });
  }
  if (url.pathname === '/api/admin/token/admin/v1/commands' && request.method === 'POST') {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const input = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    observations.commandCalls.push({ key: request.headers['idempotency-key'], body: input });
    const manualReview = input.command_type === 'TOKEN_ACCOUNT_CREATE' && accountCreateMode === 'manual-review';
    return json(response, {
      command_reference: statusCommandID, command_type: input.command_type, policy_version: 2,
      actor_user_id: ownerID, actor_permission: 'platform.tokens.manage', authorization_version: 3,
      idempotency_fingerprint: 'd'.repeat(64), target_resource_type: input.target_resource_type, target_resource_id: input.target_resource_id,
      status: manualReview ? 'PENDING' : 'SUCCEEDED', redacted_result: {}, stable_error_code: null, approval_request_id: null,
      audit_request_id: 'token-status-audit', created_at: '2026-09-04T12:00:00Z', completed_at: manualReview ? null : '2026-09-04T12:00:01Z',
      ...(manualReview ? { recovery_state: 'MANUAL_REVIEW', recovery_diagnostic_code: 'ACCOUNT_PROVISIONING_MANUAL_REVIEW', recovery_epoch: 3 } : {}),
    });
  }
  if (url.pathname === `/api/admin/token/admin/v1/commands/${statusCommandID}/recovery-actions` && request.method === 'POST') {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    observations.recoveryCalls.push({ path: url.pathname, method: request.method, key: request.headers['idempotency-key'], body });
    return json(response, {
      action_id: recoveryActionID, command_reference: statusCommandID, command_type: 'TOKEN_ACCOUNT_CREATE', action: body.action,
      status: 'SUCCEEDED', recovery_state: body.action === 'REQUEUE' ? 'PENDING' : 'FAILED', recovery_epoch: body.action === 'REQUEUE' ? 4 : 3,
      stable_error_code: null, audit_reference: recoveryAuditID, created_at: '2026-09-04T12:01:00Z', completed_at: '2026-09-04T12:01:01Z',
    });
  }
  if (url.pathname === `/api/admin/token/admin/v2/token-accounts/${accountID}/routes` && request.method === 'PUT') {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    observations.routeCalls.push({ key: request.headers['idempotency-key'], body });
    const stableRoute = {
      route_id: routeID,
      token_account_id: accountID,
      model_id: body.model_id,
      protocol_version_id: body.protocol_version_id,
      protocol_code: 'CHAT',
      priority: body.priority,
      default_scope: body.default_scope,
      health_status: 'UNKNOWN',
      health_diagnostic_code: 'ROUTE_CONFIGURATION_CHANGED',
      health_observed_at: null,
      health_expires_at: null,
      route_version: body.expected_route_version + 1,
      route_generation: 2,
      account_version: body.expected_account_version + 1,
      updated_at: '2026-09-04T12:00:01Z',
    };
    if (routeMode === 'unknown') return json(response, { code: 'INTERNAL_ERROR', request_id: 'unknown-request' }, 500);
    if (routeMode === 'mismatch') return json(response, { code: 'INTERNAL_ERROR', request_id: 'mismatch-request' }, 409);
    if (routeMode === 'safe') return json(response, { code: 'TOKEN_ROUTE_VERSION_CONFLICT', request_id: 'safe-request' }, 409);
    if (routeMode === 'delay-success') return setTimeout(() => json(response, stableRoute), 250);
    return json(response, stableRoute);
  }
  const relative = url.pathname === '/admin.html' ? 'admin.html' : url.pathname.replace(/^\//, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot) || !existsSync(target)) { response.writeHead(404); return response.end('not found'); }
  const extension = path.extname(target);
  const type = extension === '.html' ? 'text/html' : extension === '.css' ? 'text/css' : 'text/javascript';
  response.writeHead(200, { 'Content-Type': `${type}; charset=utf-8`, 'Cache-Control': 'no-store' });
  response.end(await readFile(target));
});

const { origin } = await listenOnChromiumSafePort(server);
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
  await page.waitForFunction(() => [...document.querySelectorAll('.adm-token-health .adm-row-actions button')].some((button) => button.textContent.includes('Configure stable route')));
  await page.evaluate(() => [...document.querySelectorAll('.adm-token-health .adm-row-actions button')].find((button) => button.textContent.includes('Configure stable route')).click());
  await page.waitForSelector('.adm-high-risk-dialog select');
  await page.waitForFunction((value) => [...document.querySelectorAll('.adm-high-risk-dialog option')].some((option) => option.value === value), {}, modelID);
  await page.evaluate((value) => {
    const select = [...document.querySelectorAll('.adm-high-risk-dialog select')].find((item) => [...item.options].some((option) => option.value === value));
    select.value = value;
    select.dispatchEvent(new Event('change', { bubbles: true }));
  }, modelID);
  await page.waitForFunction((value) => [...document.querySelectorAll('.adm-high-risk-dialog select')].some((item) => [...item.options].some((option) => option.value === value)), {}, protocolID);
  await page.evaluate((value) => {
    const select = [...document.querySelectorAll('.adm-high-risk-dialog select')].find((item) => [...item.options].some((option) => option.value === value));
    select.value = value;
    select.dispatchEvent(new Event('change', { bubbles: true }));
  }, protocolID);
  await page.type('.adm-high-risk-dialog textarea', 'configure stable route safely');
  await page.waitForFunction(() => document.querySelector('.adm-high-risk-dialog button[type="submit"]') && !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
}

async function submitRoute(page, twice = false) {
  await page.evaluate((double) => {
    const button = document.querySelector('.adm-high-risk-dialog footer button:last-child');
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
  await configure(page, 'permissions=all&gate=1&action-gate=1&candidate-delay=1');
  await page.evaluate(() => { globalThis.__tokenResourceTest.replaceCount = 0; });
  await page.click('#admin-root .adm-source-head .adm-row-actions button:first-child');
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
  await page.waitForFunction(() => document.querySelectorAll('#admin-root .adm-source-head .adm-row-actions button').length === 2);
  await page.click('#admin-root .adm-source-card .adm-table tbody tr td:last-child button');
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.match(page.url(), /token_account_id=/, `route action did not update URL: ${await page.$eval('main', (node) => node.textContent)}`);
  await page.waitForSelector('.adm-token-health');
  assert.equal(await page.$eval('.adm-token-health tbody tr td[data-label="Routable now"]', (node) => node.textContent.trim()), 'Yes', 'route health uses the response as_of boundary');
  await page.evaluate(() => [...document.querySelectorAll('.adm-token-health .adm-row-actions button')].find((button) => button.textContent.includes('Configure stable route')).click());
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

  await configure(page, 'permissions=all&gate=1&action-gate=0');
  await page.goto(`${origin}/admin.html?section=tokenResources&token_account_id=${accountID}&token_detail_tab=routes`, { waitUntil: 'networkidle0' });
  assert.equal(await page.$$eval('.adm-token-health button.danger,.adm-token-health .adm-row-actions button:not(:last-child)', (nodes) => nodes.length), 0, 'action gate off leaves Token resources read-only');
  assert.equal(await page.$$eval('.adm-high-risk-dialog', (nodes) => nodes.length), 0, 'action gate off renders no command dialog');

  // The queue is server-backed: a new page/operator can discover work without
  // opening a create dialog or replaying the original creation command.
  await configure(page, 'permissions=all&gate=1&action-gate=1&queue=1');
  await page.goto(`${origin}/admin.html?section=tokenResources`, { waitUntil: 'networkidle0' });
  const queueSelector = '[aria-labelledby="account-recovery-queue-title"]';
  await page.waitForSelector(`${queueSelector} tbody tr`);
  const commandsBeforeQueue = (await state(page)).commandCalls.length;
  await page.click(`${queueSelector} .adm-pagination button:last-child`);
  await page.waitForFunction((value) => document.querySelector('[aria-labelledby="account-recovery-queue-title"] tbody')?.textContent.includes(value), {}, recoveryActionID);
  assert.match(page.url(), /account_recovery_cursor=/);
  await page.reload({ waitUntil: 'networkidle0' });
  assert.ok(await page.$eval(`${queueSelector} tbody`, (node, value) => node.textContent.includes(value), recoveryActionID), 'reload restores the server-backed queue page');
  await page.goto(`${origin}/admin.html?section=tokenResources`, { waitUntil: 'networkidle0' });
  await page.click(`${queueSelector} tbody button`);
  await page.waitForSelector('[aria-labelledby="token-account-recovery-title"]');
  assert.equal((await state(page)).commandCalls.length, commandsBeforeQueue, 'reviewing persisted work never replays account creation');
  await page.click('[aria-labelledby="token-account-recovery-title"] header button');
  await configure(page, 'queue-failure=1');
  await page.click(`${queueSelector} .adm-source-head button`);
  await page.waitForSelector(`${queueSelector} [role="alert"]`);
  assert.ok(await page.$eval('.adm-source-card tbody', node => node.textContent.includes('Production account')), 'queue failure leaves account list usable');
  await configure(page, 'queue-failure=0');
  await page.click(`${queueSelector} [role="alert"] button`);
  await page.waitForSelector(`${queueSelector} tbody tr`);
  await configure(page, 'permissions=no-owner');
  await page.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
  await page.waitForFunction(() => !document.querySelector('[aria-labelledby="account-recovery-queue-title"]'));
  const queueCallsAfterRevoke = (await state(page)).queueCalls.length;
  await new Promise(resolve => setTimeout(resolve, 80));
  assert.equal((await state(page)).queueCalls.length, queueCallsAfterRevoke, 'revoked permission stops hidden queue requests');

  await configure(page, 'permissions=all&gate=1&action-gate=1&account-create-mode=manual-review&queue=0');
  await page.goto(`${origin}/admin.html?section=tokenResources`, { waitUntil: 'networkidle0' });
  await page.click('#admin-root .adm-source-head .adm-row-actions button:first-child');
  await page.waitForSelector('[aria-labelledby="token-owner-dialog-title"] [role="option"]');
  await page.click('[aria-labelledby="token-owner-dialog-title"] [role="option"]');
  await page.type('[aria-labelledby="token-owner-dialog-title"] input:not([type="search"]):not([type="number"])', 'Manual review account');
  await page.click('[aria-labelledby="token-owner-dialog-title"] button[type="submit"]');
  await page.waitForSelector('.adm-high-risk-dialog textarea');
  await page.type('.adm-high-risk-dialog textarea', 'create after checking owner authority and capacity');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
  const beforeManualCommand = (await state(page)).commandCalls.length;
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  try {
    await page.waitForSelector('[aria-labelledby="token-account-recovery-title"]', { timeout: 10000 });
  } catch (error) {
    const diagnostic = await page.evaluate(() => ({ text: document.querySelector('main')?.textContent || '', dialogs: [...document.querySelectorAll('[role="dialog"]')].map((node) => node.getAttribute('aria-labelledby')) }));
    throw new Error(`manual recovery dialog did not open: ${JSON.stringify({ diagnostic, observations: await state(page) })}`, { cause: error });
  }
  assert.equal((await state(page)).commandCalls.length, beforeManualCommand + 1, 'manual review preserves one account-create command');
  await page.type('[aria-labelledby="token-account-recovery-title"] textarea', 'requeue after reviewing the current authority evidence');
  await page.waitForFunction(() => !document.querySelector('[aria-labelledby="token-account-recovery-title"] button[type="submit"]').disabled);
  await page.click('[aria-labelledby="token-account-recovery-title"] button[type="submit"]');
  await page.waitForFunction(() => !document.querySelector('[aria-labelledby="token-account-recovery-title"]'));
  const recoveryCall = (await state(page)).recoveryCalls.at(-1);
  assert.equal(recoveryCall.body.action, 'REQUEUE');
  assert.equal(recoveryCall.body.expected_recovery_epoch, 3);
  assert.match(recoveryCall.key, /^token-account-recovery-/);

  await configure(page, 'account-create-mode=success&account-status=DISABLED');
  await page.goto(`${origin}/admin.html?section=tokenResources&token_account_id=${accountID}&token_detail_tab=routes`, { waitUntil: 'networkidle0' });
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForFunction(() => [...document.querySelectorAll('.adm-token-health .adm-row-actions button')].some((button) => button.textContent.includes('Re-enable Token Account')));
  await page.evaluate(() => [...document.querySelectorAll('.adm-token-health .adm-row-actions button')].find((button) => button.textContent.includes('Re-enable Token Account')).click());
  await page.waitForSelector('.adm-high-risk-dialog textarea');
  await page.type('.adm-high-risk-dialog textarea', 're-enable after reviewing authoritative health evidence');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
  const beforeEnable = (await state(page)).commandCalls.length;
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForSelector('.adm-high-risk-dialog .adm-inline-message.success');
  const enabledCommand = (await state(page)).commandCalls.at(-1);
  assert.equal((await state(page)).commandCalls.length, beforeEnable + 1, 're-enable dispatches one governed command');
  assert.equal(enabledCommand.body.command_type, 'TOKEN_ACCOUNT_STATUS_CHANGE');
  assert.equal(enabledCommand.body.command.target_status, 'ACTIVE');
  assert.equal(enabledCommand.body.expected_versions.target_status, 'ACTIVE');
  await configure(page, 'account-status=ACTIVE');
  await page.click('.adm-high-risk-dialog footer button:first-child');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));

  await openRouteDialog(page);
  const beforeGateRevocation = (await state(page)).routeCalls.length;
  await configure(page, 'action-gate=0');
  await page.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));
  assert.equal((await state(page)).routeCalls.length, beforeGateRevocation, 'gate revocation unloads the open route command before any write');
  await configure(page, 'action-gate=1');
  await page.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
  await new Promise((resolve) => setTimeout(resolve, 80));
  assert.equal(await page.$$eval('.adm-high-risk-dialog', (nodes) => nodes.length), 0, 'permission restoration never revives a cleared command dialog');
  assert.equal((await state(page)).routeCalls.length, beforeGateRevocation, 'permission restoration performs no deferred write');

  await configure(page, 'permissions=all&gate=1&action-gate=1&route-mode=delay-success');
  await page.reload({ waitUntil: 'networkidle0' });
  await openRouteDialog(page);
  const beforeDouble = (await state(page)).routeCalls.length;
  await submitRoute(page, true);
  await page.waitForFunction((before) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.routeCalls.length === before + 1), {}, beforeDouble);
  await page.waitForSelector('.adm-high-risk-dialog .adm-inline-message.success');
  await page.click('.adm-high-risk-dialog footer button:first-child');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));
  assert.equal((await state(page)).routeCalls.length, beforeDouble + 1, 'same-tick double click dispatches one stable route write');

  await configure(page, 'route-mode=unknown');
  await openRouteDialog(page);
  const beforeUnknown = (await state(page)).routeCalls.length;
  await submitRoute(page);
  await page.waitForSelector('.adm-high-risk-dialog [role="alert"]');
  const unknownCall = (await state(page)).routeCalls.at(-1);
  await configure(page, 'route-mode=success');
  await submitRoute(page);
  await new Promise((resolve) => setTimeout(resolve, 80));
  const afterUnknown = await state(page);
  assert.equal(afterUnknown.routeCalls.length, beforeUnknown + 1, 'unknown direct write is never repeated without a result-query reference');
  assert.equal(afterUnknown.routeCalls.at(-1).key, unknownCall.key, 'unknown outcome retains the frozen command key for manual correlation');
  assert.equal(await page.$eval('.adm-high-risk-dialog button[type="submit"]', (button) => button.disabled), true, 'unknown direct write fails closed when recovery has no trusted reference');
  await page.reload({ waitUntil: 'networkidle0' });

  await configure(page, 'route-mode=mismatch');
  await openRouteDialog(page);
  const beforeMismatch = (await state(page)).routeCalls.length;
  await submitRoute(page);
  await page.waitForSelector('.adm-high-risk-dialog [role="alert"]');
  const mismatchCall = (await state(page)).routeCalls.at(-1);
  await page.$eval('.adm-high-risk-dialog input[type="number"]', (input) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, '101');
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
  await submitRoute(page);
  await new Promise((resolve) => setTimeout(resolve, 80));
  const afterMismatch = await state(page);
  assert.equal(afterMismatch.routeCalls.length, beforeMismatch + 1, 'mismatched 409 status/code locks even a changed payload');
  assert.equal(afterMismatch.routeCalls.at(-1).key, mismatchCall.key, 'mismatched response retains the original key');
  assert.equal(await page.$eval('.adm-high-risk-dialog header button', (button) => button.disabled), true, 'unknown result cannot be dismissed into a fresh command');
  await page.reload({ waitUntil: 'networkidle0' });

  await configure(page, 'route-mode=safe');
  await openRouteDialog(page);
  const beforeSafe = (await state(page)).routeCalls.length;
  await submitRoute(page);
  await page.waitForSelector('.adm-high-risk-dialog [role="alert"]');
  await submitRoute(page);
  await page.waitForFunction((before) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.routeCalls.length === before + 2), {}, beforeSafe);
  await page.$eval('.adm-high-risk-dialog input[type="number"]', (input) => {
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
