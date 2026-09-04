import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const browserPath = [process.env.CHROME, 'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe', 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium or Edge is required');

const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const provisioningID = '0199a300-0001-7000-8000-000000000001';
const usageID = '0199a300-0001-7000-8000-000000000002';
const accountID = '0199a300-0001-7000-8000-000000000003';
const modelID = '0199a300-0001-7000-8000-000000000004';
const protocolID = '0199a300-0001-7000-8000-000000000005';
const caseID = '0199a300-0001-7000-8000-000000000006';
const attemptID = '0199a300-0001-7000-8000-000000000007';
const evidenceID = '0199a300-0001-7000-8000-000000000008';
const actorID = '0199a300-0001-7000-8000-000000000009';
const commandID = '0199a300-0001-7000-8000-000000000013';

const allPermissions = ['platform.tokens.read', 'platform.tokens.manage', 'platform.usage.read', 'platform.usage.reconcile.retry', 'platform.audit.read'];
let permissions = [...allPermissions];
let detailsEnabled = false;
let attemptMode = 'success';
let evidenceMode = 'success';
let usageDelay = false;
let retryMode = 'success';
const observations = { provisioningDetail: 0, attempts: 0, usageDetail: 0, usageAborted: 0, evidence: 0, retryCalls: [], repairCalls: [], precheckCalls: [], commandCalls: [], resultCalls: 0, modelRequests: 0 };

const now = '2026-09-04T12:00:00Z';
function envelope(items, more = false) {
  return { items, next_cursor: more ? 'signed-next' : '', has_more: more, limit: 50, as_of: now, snapshot_expires_at: '2026-09-04T12:15:00Z', request_id: 'request-smoke' };
}
function json(response, payload, status = 200) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}
async function body(request) {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  return JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}');
}

const provisioning = {
  provisioning_id: provisioningID, scope_type: 'PERSONAL', scope_id: actorID, model_id: modelID,
  protocol_code: 'CHAT', protocol_version_id: protocolID, desired_generation: 2, actual_generation: 1,
  status: 'FAILED', stage: 'VERIFY', policy_version: 1, attempt_count: 1, repair_round: 0, max_attempts: 3,
  sla_deadline_at: '2026-09-04T12:30:00Z', last_error_code: 'ROUTE_UNAVAILABLE', updated_at: now,
};
const usage = {
  usage_request_id: usageID, token_account_id: accountID, model: 'Flash', model_id: modelID, catalog_version: 3,
  model_capability_version_id: '0199a300-0001-7000-8000-000000000010', protocol_definition_id: protocolID,
  route_generation: 4, price_version_id: '0199a300-0001-7000-8000-000000000011', hold_id: '0199a300-0001-7000-8000-000000000012',
  status: 'REVIEW', execution_status: 'SUCCEEDED', delivery_status: 'COMPLETED', cancel_status: 'NOT_REQUESTED', settlement_status: 'REVIEW',
  maximum_microcredits: 100, actual_microcredits: 90, adapter_version: 'adapter-v3', attempt_status: 'SUCCEEDED',
  input_tokens: 5, cached_input_tokens: 2, output_tokens: 3, reconciliation_attempts: 2,
  reconciliation_last_error: 'RECONCILIATION_DIAGNOSTIC_REDACTED', last_persisted_sequence: 2,
  started_at: '2026-09-04T11:58:00Z', updated_at: now,
};

const dependencies = Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map((key) => [key, 0]));
function governedPrecheck(input) {
  const repair = input.command_type === 'TOKEN_ROUTE_PROVISIONING_REPAIR';
  return {
    action: input.command_type, precheck_fingerprint: 'a'.repeat(64), requires_cas_revalidation: true,
    snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', token_observed_at: now, blockers: [],
    evidence: { resource_type: input.target_resource_type, resource_id: input.target_resource_id, current_status: repair ? 'FAILED' : 'REVIEW', current_version: { evidence_etag: `"impact-${'b'.repeat(64)}"`, evidence_set_sha256: 'c'.repeat(64) }, dependencies },
    expected_versions: repair
      ? { provisioning_id: provisioningID, expected_provisioning_version: now, target_generation: 2 }
      : { usage_request_id: usageID, expected_usage_version: now, evidence_high_watermark: 2 },
  };
}
function commandResult(input, status) {
  return {
    command_reference: commandID, command_type: input.command_type, policy_version: 2, actor_user_id: actorID,
    actor_permission: input.command_type === 'TOKEN_ROUTE_PROVISIONING_REPAIR' ? 'platform.tokens.manage' : 'platform.usage.reconcile.retry',
    authorization_version: 1, idempotency_fingerprint: 'd'.repeat(64), target_resource_type: input.target_resource_type,
    target_resource_id: input.target_resource_id, status, redacted_result: {}, stable_error_code: null,
    approval_request_id: null, audit_request_id: 'operation-action-audit', created_at: now,
    completed_at: status === 'SUCCEEDED' ? '2026-09-04T12:00:01Z' : null,
  };
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/configure') {
    if (url.searchParams.has('gate')) detailsEnabled = url.searchParams.get('gate') === '1';
    if (url.searchParams.get('permissions') === 'all') permissions = [...allPermissions];
    if (url.searchParams.get('permissions') === 'tokens') permissions = ['platform.tokens.read'];
    if (url.searchParams.has('attempt')) attemptMode = url.searchParams.get('attempt');
    if (url.searchParams.has('evidence')) evidenceMode = url.searchParams.get('evidence');
    if (url.searchParams.has('usage-delay')) usageDelay = url.searchParams.get('usage-delay') === '1';
    if (url.searchParams.has('retry')) retryMode = url.searchParams.get('retry');
    return json(response, { permissions, detailsEnabled, attemptMode, evidenceMode, usageDelay, retryMode });
  }
  if (url.pathname === '/__test__/state') return json(response, observations);
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf-smoke', feature_flags: { admin_model_token_ia: true, admin_model_operations_details: detailsEnabled, admin_model_token_actions: true } });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: actorID, platform_permissions: permissions });
  if (url.pathname === '/api/admin/token/admin/v1/provisionings' && request.method === 'GET') return json(response, envelope([provisioning]));
  if (url.pathname === '/api/admin/token/admin/v1/usage-requests' && request.method === 'GET') return json(response, envelope([usage]));
  if (url.pathname === '/api/admin/token/admin/v1/reconciliation-cases') return json(response, envelope([{ case_id: caseID, case_type: 'USAGE_RECONCILIATION_REVIEW', resource_type: 'USAGE_REQUEST', resource_id: usageID, severity: 'P1', status: 'OPEN', claim_due_at: now, first_response_due_at: now, mitigation_due_at: now, resolution_due_at: '2026-09-05T12:00:00Z', escalation_level: 0, updated_at: now }]));
  if (url.pathname === `/api/admin/token/admin/v2/provisionings/${provisioningID}` && request.method === 'GET') { observations.provisioningDetail += 1; return json(response, provisioning); }
  if (url.pathname === `/api/admin/token/admin/v1/provisionings/${provisioningID}/attempts`) {
    observations.attempts += 1;
    if (attemptMode === 'fail') return json(response, { code: 'QUERY_TIMEOUT', request_id: 'attempt-timeout' }, 504);
    return json(response, envelope([{ attempt_id: attemptID, repair_round: 0, attempt_number: 1, status: 'FAILED', stage: 'VERIFY', error_code: 'ROUTE_UNAVAILABLE', started_at: now, ended_at: now }]));
  }
  if (url.pathname === `/api/admin/token/admin/v1/usage-requests/${usageID}` && request.method === 'GET') {
    observations.usageDetail += 1;
    if (usageDelay) {
      const timer = setTimeout(() => json(response, usage), 5000);
      const abort = () => { clearTimeout(timer); observations.usageAborted += 1; };
      request.once('aborted', abort);
      response.once('close', () => { if (!response.writableEnded) abort(); });
      return;
    }
    return json(response, usage);
  }
  if (url.pathname === `/api/admin/token/admin/v1/usage-requests/${usageID}/evidence`) {
    observations.evidence += 1;
    if (evidenceMode === 'fail') return json(response, { code: 'QUERY_TIMEOUT', request_id: 'evidence-timeout' }, 504);
    return json(response, envelope([{ source_kind: 'CONVERGENCE', source_id: evidenceID, evidence_type: 'SETTLEMENT', evidence: { decision: 'REVIEW', safe_count: 2 }, observed_at: now }]));
  }
  if (url.pathname === `/api/admin/token/admin/v1/usage-requests/${usageID}/retry-reconciliation` && request.method === 'POST') {
    const input = await body(request);
    observations.retryCalls.push({ key: request.headers['idempotency-key'], body: input });
    if (retryMode === 'unknown') return json(response, { code: 'INTERNAL_ERROR', request_id: 'retry-unknown' }, 500);
    return json(response, { ...usage, reconciliation_last_error: 'MANUAL_RECONCILIATION_REQUESTED', updated_at: '2026-09-04T12:01:00Z' });
  }
  if (url.pathname === `/api/admin/token/admin/v1/provisionings/${provisioningID}/repair` && request.method === 'POST') {
    observations.repairCalls.push({ key: request.headers['idempotency-key'], body: await body(request) });
    return json(response, { ...provisioning, status: 'PENDING', repair_round: 1, stage: 'MANUAL_REPAIR' }, 202);
  }
  if (url.pathname === '/api/admin/token/admin/v1/commands:precheck' && request.method === 'POST') {
    const input = await body(request);
    observations.precheckCalls.push(input);
    return json(response, governedPrecheck(input));
  }
  if (url.pathname === '/api/admin/token/admin/v1/commands' && request.method === 'POST') {
    const input = await body(request);
    observations.commandCalls.push({ key: request.headers['idempotency-key'], body: input });
    return json(response, commandResult(input, 'PENDING'), 202);
  }
  if (url.pathname === `/api/admin/token/admin/v1/commands/${commandID}` && request.method === 'GET') {
    observations.resultCalls += 1;
    return json(response, commandResult(observations.commandCalls.at(-1).body, 'SUCCEEDED'));
  }
  if (/model|inference|chat|completion/u.test(url.pathname) && url.pathname.startsWith('/api/')) observations.modelRequests += 1;
  if (url.pathname.startsWith('/api/admin/')) return json(response, { items: [] });
  const relative = url.pathname === '/admin.html' ? 'admin.html' : url.pathname.replace(/^\//, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot) || !existsSync(target)) { response.writeHead(404); return response.end('not found'); }
  const extension = path.extname(target);
  const type = extension === '.html' ? 'text/html' : extension === '.css' ? 'text/css' : 'text/javascript';
  response.writeHead(200, { 'Content-Type': `${type}; charset=utf-8`, 'Cache-Control': 'no-store' });
  response.end(await readFile(target));
});

await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
const browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });

async function configure(page, query) { await page.evaluate((value) => fetch(`/__test__/configure?${value}`), query); }
async function state(page) { return page.evaluate(() => fetch('/__test__/state').then((response) => response.json())); }
async function newPage() {
  const page = await browser.newPage();
  await page.evaluateOnNewDocument(() => {
    localStorage.setItem('pinvou.admin.language', 'en');
    globalThis.__operationsSmoke = { replaceCount: 0 };
    const replace = history.replaceState.bind(history);
    history.replaceState = (...args) => { globalThis.__operationsSmoke.replaceCount += 1; return replace(...args); };
  });
  return page;
}

try {
  const page = await newPage();
  const pageErrors = [];
  const requestedPaths = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  page.on('request', (request) => requestedPaths.push(new URL(request.url()).pathname));

  const beforeGateOffProvisioning = observations.provisioningDetail;
  await page.goto(`${origin}/admin.html?section=modelOperations&operations_tab=provisionings&provisioning_id=${provisioningID}&provisioning_attempt_status=FAILED&provisioning_attempt_cursor=signed`, { waitUntil: 'networkidle0' });
  assert.equal(await page.evaluate(() => globalThis.__operationsSmoke.replaceCount), 1, 'gate-off deep link is cleared with one replace');
  assert.doesNotMatch(page.url(), /provisioning_id|provisioning_attempt_/u);
  assert.equal((await state(page)).provisioningDetail, beforeGateOffProvisioning, 'gate-off performs no detail request');
  assert.equal(await page.$$eval('.adm-operation-detail', (nodes) => nodes.length), 0, 'gate-off renders no hidden detail');

  await configure(page, 'permissions=all&gate=1&attempt=fail');
  const safeDetailResponse = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/admin/token/admin/v2/provisionings/${provisioningID}` && response.status() === 200);
  await page.goto(`${origin}/admin.html?section=modelOperations&operations_tab=provisionings&provisioning_q=failed&provisioning_id=${provisioningID}`, { waitUntil: 'networkidle0' });
  const safeDetailPayload = await (await safeDetailResponse).json();
  await page.waitForSelector('.adm-operation-detail');
  await page.waitForSelector('.adm-operation-subsection .adm-source-error');
  const provisioningText = await page.$eval('.adm-operation-detail', (node) => node.textContent);
  assert.match(provisioningText, /ROUTE_UNAVAILABLE/u, 'detail remains visible when attempts fail');
  assert.doesNotMatch(provisioningText, /SECRET-WORKER|SECRET-DETAIL/u, 'worker identity and free-form detail never enter the DOM');
  for (const hidden of ['attempts', 'worker_id', 'error_detail', 'active_credential_id']) assert.doesNotMatch(JSON.stringify(safeDetailPayload), new RegExp(`"${hidden}"`, 'u'), `v2 detail network payload excludes ${hidden}`);
  assert.equal(requestedPaths.filter((path) => path === `/api/admin/token/admin/v1/provisionings/${provisioningID}`).length, 0, 'Portal never requests the legacy inline Provisioning detail');
  assert.equal(await page.$$eval('.adm-protocol-dialog-backdrop,.adm-drawer', (nodes) => nodes.length), 0, 'detail is full-width main content, not a drawer or overlay');
  await configure(page, 'attempt=success');
  const safeAttemptsResponse = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/admin/token/admin/v1/provisionings/${provisioningID}/attempts` && response.status() === 200);
  await page.click('.adm-operation-subsection .adm-source-error button');
  const safeAttemptsPayload = await (await safeAttemptsResponse).json();
  await page.waitForFunction(() => document.querySelector('.adm-operation-subsection tbody tr'));
  for (const hidden of ['worker_id', 'error_detail']) assert.doesNotMatch(JSON.stringify(safeAttemptsPayload), new RegExp(`"${hidden}"`, 'u'), `attempt network payload excludes ${hidden}`);
  assert.match(page.url(), /provisioning_q=failed/u, 'detail preserves its source-list query');

  const detailButtons = await page.$$eval('.adm-operation-detail header button', (nodes) => nodes.map((node) => node.textContent.trim()));
  assert.equal(detailButtons.some((label) => /repair/iu.test(label)), true, `governed repair button is visible: ${JSON.stringify(detailButtons)}`);
  await page.evaluate(() => [...document.querySelectorAll('.adm-operation-detail header button')].find((button) => /repair/iu.test(button.textContent)).click());
  await page.waitForSelector('.adm-high-risk-dialog textarea');
  await page.type('.adm-high-risk-dialog textarea', 'retry the bounded provisioning checks');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
  const repairBefore = (await state(page)).commandCalls.length;
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForFunction((count) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.commandCalls.length === count + 1), {}, repairBefore);
  await page.waitForSelector('.adm-high-risk-dialog [role="alert"]');
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('.adm-high-risk-dialog .adm-inline-message.success'));
  const afterRepair = await state(page);
  assert.equal(afterRepair.precheckCalls.at(-1).command_type, 'TOKEN_ROUTE_PROVISIONING_REPAIR');
  assert.deepEqual(afterRepair.precheckCalls.at(-1).command, { provisioning_id: provisioningID, expected_provisioning_version: now, target_generation: 2, replay_model_request: false });
  assert.equal(afterRepair.commandCalls.at(-1).body.command_type, 'TOKEN_ROUTE_PROVISIONING_REPAIR');
  assert.deepEqual(afterRepair.commandCalls.at(-1).body.command, { provisioning_id: provisioningID, expected_provisioning_version: now, target_generation: 2, replay_model_request: false });
  assert.equal(afterRepair.repairCalls.length, 0, 'governed repair never falls back to the legacy repair POST');
  await page.click('.adm-high-risk-dialog footer button:first-child');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));

  await configure(page, 'evidence=fail&retry=success');
  await page.goto(`${origin}/admin.html?section=modelOperations&operations_tab=reconciliation&reconciliation_status=OPEN`, { waitUntil: 'networkidle0' });
  await page.click('#model-operations-panel-reconciliation tbody button');
  await page.waitForSelector('.adm-operation-detail');
  assert.match(page.url(), new RegExp(`operations_tab=reconciliation.*usage_request_id=${usageID}`), 'case opens its authoritative usage request under the source tab');
  await page.waitForSelector('.adm-operation-subsection .adm-source-error');
  const usageDetailText = await page.$eval('.adm-operation-detail', (node) => node.textContent);
  assert.match(usageDetailText, /adapter-v3/u, 'usage detail remains visible when evidence fails');
  assert.match(usageDetailText, /RECONCILIATION_DIAGNOSTIC_REDACTED/u, 'DOM renders only the controlled redacted diagnostic');
  await configure(page, 'evidence=success');
  await page.click('.adm-operation-subsection .adm-source-error button');
  await page.waitForSelector('.adm-evidence-list article');
  await page.setViewport({ width: 320, height: 900, deviceScaleFactor: 1 });
  const reflow = await page.evaluate(() => { const main = document.querySelector('main'); return { fits: main.scrollWidth <= main.clientWidth, viewport: main.clientWidth, width: main.scrollWidth, offenders: [...main.querySelectorAll('*')].filter((node) => node.getBoundingClientRect().right > main.getBoundingClientRect().right + 1).slice(0, 8).map((node) => `${node.tagName}.${node.className}:${Math.round(node.getBoundingClientRect().right)}`) }; });
  assert.equal(reflow.fits, true, `operation detail reflows without page-level horizontal scrolling at 320px: ${JSON.stringify(reflow)}`);
  await page.setViewport({ width: 1280, height: 900, deviceScaleFactor: 1 });

  await page.evaluate(() => [...document.querySelectorAll('.adm-operation-detail header button')].find((button) => button.textContent.trim() === 'Reconcile existing evidence again').click());
  await page.waitForSelector('.adm-high-risk-dialog textarea');
  await page.type('.adm-high-risk-dialog textarea', 'reconcile only the existing usage and settlement evidence');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
  const reconcileBefore = (await state(page)).commandCalls.length;
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForFunction((count) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.commandCalls.length === count + 1), {}, reconcileBefore);
  await page.waitForSelector('.adm-high-risk-dialog [role="alert"]');
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('.adm-high-risk-dialog .adm-inline-message.success'));
  const governedState = await state(page);
  assert.equal(governedState.precheckCalls.at(-1).command_type, 'TOKEN_USAGE_RECONCILIATION_RETRY');
  assert.deepEqual(governedState.precheckCalls.at(-1).command, { usage_request_id: usageID, expected_usage_version: now, evidence_high_watermark: 2, replay_model_request: false });
  assert.equal(governedState.commandCalls.at(-1).body.command_type, 'TOKEN_USAGE_RECONCILIATION_RETRY');
  assert.deepEqual(governedState.commandCalls.at(-1).body.command, { usage_request_id: usageID, expected_usage_version: now, evidence_high_watermark: 2, replay_model_request: false });
  assert.equal(governedState.retryCalls.length, 0, 'governed reconciliation never falls back to the legacy retry POST');
  assert.equal(governedState.modelRequests, 0, 'reconciliation does not call a model or inference endpoint');
  await page.click('.adm-high-risk-dialog footer button:first-child');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));
  await page.waitForFunction(() => document.querySelector('.adm-operation-audit button')?.getClientRects().length > 0);
  await page.evaluate(() => document.querySelector('.adm-operation-audit button').click());
  await page.waitForFunction((id) => new URLSearchParams(location.search).get('section') === 'audit' && new URLSearchParams(location.search).get('audit_resource') === id, {}, usageID);

  await configure(page, 'permissions=all&usage-delay=1');
  await page.goto(`${origin}/admin.html?section=modelOperations&operations_tab=usage&usage_request_id=${usageID}`, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((value) => value.usageDetail > 0));
  await configure(page, 'permissions=tokens&usage-delay=0');
  await page.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
  await page.waitForFunction(() => !document.querySelector('.adm-operation-detail'));
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((value) => value.usageAborted > 0));
  assert.doesNotMatch(page.url(), /usage_request_id|usage_evidence_/u, 'permission revoke clears the hidden detail URL');
  assert.match(page.url(), /operations_tab=provisionings/u, 'permission revoke selects the remaining authorized source');
  assert.deepEqual(pageErrors, []);
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}

console.log('admin model operations UI smoke passed');
