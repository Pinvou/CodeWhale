import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import puppeteer from 'puppeteer-core';
import { listenOnChromiumSafePort } from './chromium-safe-port.mjs';
import { governedTokenEnvelope } from '../src/features/account-platform/admin-high-risk-action-state.js';

const browserPath = [
  process.env.CHROME,
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium or Edge is required');

const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const actorID = '0199a100-0001-7000-8000-000000000201';
const modelID = '0199a100-0001-7000-8000-000000000202';
const protocolID = '0199a100-0001-7000-8000-000000000203';
const commandID = '0199a100-0001-7000-8000-000000000204';
const fullPermissions = ['platform.models.read', 'platform.models.manage', 'platform.models.sync', 'platform.models.lifecycle.request', 'platform.models.catalog.rollback.request', 'platform.pricing.read', 'platform.pricing.manage'];
let permissions = [...fullPermissions];
let actionsEnabled = true;
let impactDelay = false;
let commandResultMode = 'SUCCEEDED';
const observations = { impactCalls: 0, impactAborted: 0, precheckCalls: [], commandCalls: [], resultCalls: 0, approvalCreates: [], approvalSubmits: 0, approvalReads: 0, legacyProtocolWrites: 0 };

const portalFingerprintFixture = await governedTokenEnvelope({
  commandType: 'TOKEN_MODEL_PUBLISH',
  targetResourceType: 'MODEL',
  targetResourceID: '018f4f72-9b3a-7abc-8def-1234567890ab',
  reason: 'publish reviewed immediate draft',
  impact: {
    precheck_fingerprint: '5'.repeat(64),
    expected_versions: {
      model_id: '018f4f72-9b3a-7abc-8def-1234567890ab', expected_catalog_version: 4, expected_draft_version: 2,
      draft_fingerprint: '1'.repeat(64), protocol_definition_id: '018f4f72-9b3a-7abc-8def-1234567890ac', protocol_version: 1,
      stable_route_count: 1, stable_route_set_fingerprint: '2'.repeat(64), provisioning_count: 1, ready_route_count: 1,
      provisioning_set_fingerprint: '3'.repeat(64), financial_fact_revision: 7, price_set_fingerprint: '4'.repeat(64),
    },
  },
  command: { model_id: '018f4f72-9b3a-7abc-8def-1234567890ab', publish_mode: 'IMMEDIATE' },
});
assert.equal(portalFingerprintFixture.expected_versions.request_fingerprint, '2f4f93843cf8f35a065298fb6239977ccc2b8e9dacaac88c3a434ac0225f551b', 'Portal and Token use the same canonical request fingerprint fixture');
const scheduledPortalFingerprintFixture = await governedTokenEnvelope({
  commandType: 'TOKEN_MODEL_PUBLISH', targetResourceType: 'MODEL', targetResourceID: '018f4f72-9b3a-7abc-8def-1234567890ab', reason: 'schedule reviewed model publication',
  impact: portalFingerprintFixture.expected_versions && { precheck_fingerprint: '5'.repeat(64), expected_versions: Object.fromEntries(Object.entries(portalFingerprintFixture.expected_versions).filter(([key]) => !['request_fingerprint', 'precheck_fingerprint'].includes(key))) },
  command: { model_id: '018f4f72-9b3a-7abc-8def-1234567890ab', publish_mode: 'SCHEDULED', lifecycle_plan_id: '018f4f72-9b3a-7abc-8def-1234567890ad', lifecycle_plan_version: 3, effective_at: '2026-09-05T12:30:00.000Z' },
});
assert.equal(scheduledPortalFingerprintFixture.command.effective_at, '2026-09-05T12:30:00Z', 'scheduled commands freeze UTC whole-second time before hashing');
assert.equal(scheduledPortalFingerprintFixture.expected_versions.request_fingerprint, '859a6a99a84e58655a4fc89e139ee01eac31835f13e7783d5980150afdfd86e1', 'scheduled Portal and Token fingerprints share the fixed time-canonical fixture');

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

function envelope(items) {
  return { items, next_cursor: '', has_more: false, limit: 50, as_of: '2026-09-04T12:00:00Z', snapshot_expires_at: '2026-09-04T12:15:00Z', request_id: 'model-actions-test' };
}

const model = {
  model_id: modelID,
  catalog_version: 7,
  display_name: 'Flash draft',
  description: '',
  aliases: [],
  lifecycle_status: 'PUBLISHED',
  protocol_code: 'CHAT',
  protocol_version: 1,
  protocol_definition_id: protocolID,
  model_capability_version_id: '0199a100-0001-7000-8000-000000000205',
  capability_version: 1,
  context_length: 128000,
  maximum_output_tokens: 8192,
  supported_modalities: ['TEXT'],
  tokenizer_version: 'v1',
  source_available: true,
  source_missing_confirmations: 0,
  force_cancel_inflight: false,
  draft: { draft_version: 3 },
};
const dependencies = Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map((key) => [key, 0]));
const impact = {
  action: 'MODEL_LIFECYCLE_PUBLISHED',
  precheck_fingerprint: 'a'.repeat(64),
  requires_cas_revalidation: true,
  snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS',
  token_observed_at: '2026-09-04T12:00:00Z',
  blockers: [],
  evidence: { resource_type: 'MODEL', resource_id: modelID, current_status: 'PUBLISHED', current_version: { evidence_etag: `"impact-${'b'.repeat(64)}"`, evidence_set_sha256: 'c'.repeat(64) }, dependencies },
};

function lifecycleImpact(targetLifecycle = 'DEPRECATED') {
  return {
    ...impact,
    action: `MODEL_LIFECYCLE_${targetLifecycle}`,
    critical_resource_versions: {
      model_id: modelID, catalog_version: 7, current_lifecycle: 'PUBLISHED', target_lifecycle: targetLifecycle,
      lifecycle_plan_version: 0, effective_at: '2026-09-04T12:05:00Z', existing_session_cutoff_at: null,
      replacement_model_id: null, billing_price_version: 4, route_set_fingerprint: '1'.repeat(64),
      provisioning_set_fingerprint: '2'.repeat(64), inflight_request_set_fingerprint: '3'.repeat(64), policy_version: 2,
    },
    financial_snapshot: { billing_price_revision: 4, price_set_fingerprint: '9'.repeat(64), models_checked: 1, models_covered: 1, fully_covered: true, billing_as_of: '2026-09-04T12:00:00Z' },
  };
}

function rollbackImpact(sourceVersion = 3) {
  return {
    ...impact,
    action: 'MODEL_CATALOG_ROLLBACK',
    evidence: { ...impact.evidence, resource_type: 'MODEL_CATALOG', resource_id: String(sourceVersion) },
    critical_resource_versions: {
      current_catalog_version: 7, source_catalog_version: sourceVersion, catalog_diff_fingerprint: '4'.repeat(64),
      billing_price_version: 4, route_set_fingerprint: '1'.repeat(64), provisioning_set_fingerprint: '2'.repeat(64),
      inflight_request_set_fingerprint: '3'.repeat(64), policy_version: 2,
    },
    financial_snapshot: { billing_price_revision: 4, price_set_fingerprint: '9'.repeat(64), models_checked: 1, models_covered: 1, fully_covered: true, billing_as_of: '2026-09-04T12:00:00Z' },
  };
}

function governedPrecheck(commandType, resourceType, resourceID) {
  const expectedVersions = commandType === 'TOKEN_PROTOCOL_DRAFT_CREATE'
    ? { protocol_code: resourceID, expected_protocol_head_version: 0 }
    : commandType === 'TOKEN_MODEL_PUBLISH'
      ? {
          model_id: resourceID, expected_catalog_version: 7, expected_draft_version: 3,
          draft_fingerprint: '6'.repeat(64), protocol_definition_id: protocolID, protocol_version: 1,
          stable_route_count: 1, stable_route_set_fingerprint: '7'.repeat(64),
          provisioning_count: 1, ready_route_count: 1, provisioning_set_fingerprint: '8'.repeat(64),
          financial_fact_revision: 4, price_set_fingerprint: '9'.repeat(64),
        }
    : { expected_catalog_version: 7, adapter_observed_version: 'adapter-v7', catalog_diff_fingerprint: '4'.repeat(64) };
  return {
    ...impact,
    action: commandType,
    evidence: { ...impact.evidence, resource_type: resourceType, resource_id: resourceID, current_status: 'READY' },
    expected_versions: expectedVersions,
  };
}

function commandResult(status, commandType = 'TOKEN_MODEL_PUBLISH', resourceType = 'MODEL', resourceID = modelID) {
  return {
    command_reference: commandID,
    command_type: commandType,
    policy_version: 2,
    actor_user_id: actorID,
    actor_permission: 'platform.models.manage',
    authorization_version: 1,
    idempotency_fingerprint: 'd'.repeat(64),
    target_resource_type: resourceType,
    target_resource_id: resourceID,
    status,
    redacted_result: commandType === 'TOKEN_MODEL_PUBLISH' && status === 'SUCCEEDED'
      ? { command_reference: commandID, command_type: commandType, model_id: resourceID, catalog_version: 8, status: 'PUBLISHED' }
      : undefined,
    stable_error_code: ['REJECTED', 'FAILED'].includes(status) ? 'COMMAND_PRECHECK_FAILED' : null,
    approval_request_id: null,
    audit_request_id: 'model-action-audit',
    created_at: '2026-09-04T12:00:00Z',
    completed_at: ['SUCCEEDED', 'REJECTED', 'FAILED'].includes(status) ? '2026-09-04T12:00:01Z' : null,
  };
}

function approvalResult(draft, status = 'DRAFT', version = 1) {
  return {
    ...draft, approval_request_id: '0199a100-0001-7000-8000-000000000206', requester_user_id: actorID,
    policy_version: 2, approval_mode: 'APPROVAL_REQUIRED', command_fingerprint: '5'.repeat(64), status, version,
    expires_at: '2026-09-05T12:00:00Z', created_at: '2026-09-04T12:00:00Z', updated_at: '2026-09-04T12:00:01Z',
    allowed_actions: status === 'DRAFT' ? ['EDIT', 'SUBMIT'] : [],
  };
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/configure') {
    if (url.searchParams.get('permissions') === 'full') permissions = [...fullPermissions];
    if (url.searchParams.get('permissions') === 'read') permissions = ['platform.models.read'];
    if (url.searchParams.has('without')) permissions = fullPermissions.filter((permission) => permission !== url.searchParams.get('without'));
    if (url.searchParams.has('gate')) actionsEnabled = url.searchParams.get('gate') === '1';
    if (url.searchParams.has('impact-delay')) impactDelay = url.searchParams.get('impact-delay') === '1';
    if (url.searchParams.has('result')) commandResultMode = url.searchParams.get('result').toUpperCase();
    return json(response, { permissions, actionsEnabled, impactDelay, commandResultMode });
  }
  if (url.pathname === '/__test__/state') return json(response, observations);
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf-test', feature_flags: { admin_model_token_ia: true, admin_model_token_actions: actionsEnabled } });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: actorID, platform_permissions: permissions });
  if (url.pathname === '/api/admin/token/admin/v1/models') return json(response, envelope([model]));
  if ((url.pathname === '/api/admin/token/admin/v1/protocols' || url.pathname === '/api/admin/token/admin/v1/formulas') && request.method === 'GET') return json(response, envelope([]));
  if ((url.pathname === '/api/admin/token/admin/v1/protocols' || /\/api\/admin\/token\/admin\/v1\/(protocols|formulas)\/.+/u.test(url.pathname)) && request.method === 'POST') {
    observations.legacyProtocolWrites += 1;
    return json(response, { code: 'LEGACY_DIRECT_WRITE_FORBIDDEN' }, 500);
  }
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}/impact/lifecycle`) {
    observations.impactCalls += 1;
    const projected = lifecycleImpact(url.searchParams.get('target_status') || 'PUBLISHED');
    if (!impactDelay) return json(response, projected);
    const timer = setTimeout(() => json(response, projected), 5000);
    const aborted = () => {
      clearTimeout(timer);
      observations.impactAborted += 1;
    };
    request.once('aborted', aborted);
    response.once('close', () => { if (!response.writableEnded) aborted(); });
    return;
  }
  if (url.pathname === '/api/admin/token/admin/v1/model-catalog/impact/rollback') {
    observations.impactCalls += 1;
    const projected = rollbackImpact(Number(url.searchParams.get('source_catalog_version')));
    if (!impactDelay) return json(response, projected);
    const timer = setTimeout(() => json(response, projected), 5000);
    const aborted = () => {
      clearTimeout(timer);
      observations.impactAborted += 1;
    };
    request.once('aborted', aborted);
    response.once('close', () => { if (!response.writableEnded) aborted(); });
    return;
  }
  if (url.pathname === '/api/admin/token/admin/v1/commands:precheck' && request.method === 'POST') {
    const input = await body(request);
    observations.precheckCalls.push(input);
    const projected = governedPrecheck(input.command_type, input.target_resource_type, input.target_resource_id);
    observations.impactCalls += 1;
    if (!impactDelay) return json(response, projected);
    const timer = setTimeout(() => json(response, projected), 5000);
    const aborted = () => {
      clearTimeout(timer);
      observations.impactAborted += 1;
    };
    request.once('aborted', aborted);
    response.once('close', () => { if (!response.writableEnded) aborted(); });
    return;
  }
  if (url.pathname === '/api/admin/token/admin/v1/commands' && request.method === 'POST') {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    observations.commandCalls.push({ key: request.headers['idempotency-key'], body });
    return json(response, commandResult('PENDING', body.command_type, body.target_resource_type, body.target_resource_id), 202);
  }
  if (url.pathname === `/api/admin/token/admin/v1/commands/${commandID}` && request.method === 'GET') {
    observations.resultCalls += 1;
    const prior = observations.commandCalls.at(-1)?.body || {};
    return json(response, commandResult(commandResultMode, prior.command_type, prior.target_resource_type, prior.target_resource_id));
  }
  if (url.pathname === '/api/admin/account/admin/v1/approval-requests' && request.method === 'POST') {
    const draft = await body(request);
    observations.approvalCreates.push(draft);
    return json(response, approvalResult(draft), 201);
  }
  if (url.pathname === '/api/admin/account/admin/v1/approval-requests/0199a100-0001-7000-8000-000000000206/actions/submit' && request.method === 'POST') {
    observations.approvalSubmits += 1;
    const submitted = await body(request);
    return json(response, approvalResult(observations.approvalCreates.at(-1), 'SUBMITTED', submitted.expected_version + 1));
  }
  if (url.pathname === '/api/admin/account/admin/v1/approval-requests/0199a100-0001-7000-8000-000000000206' && request.method === 'GET') {
    observations.approvalReads += 1;
    return json(response, approvalResult(observations.approvalCreates.at(-1), 'EXECUTED', 3));
  }
  const relative = url.pathname === '/admin.html' ? 'admin.html' : url.pathname.replace(/^\//, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot) || !existsSync(target)) { response.writeHead(404); response.end('not found'); return; }
  const extension = path.extname(target);
  response.writeHead(200, { 'Content-Type': `${extension === '.html' ? 'text/html' : extension === '.css' ? 'text/css' : 'text/javascript'}; charset=utf-8`, 'Cache-Control': 'no-store' });
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

async function openDelayedAction(page, label) {
  const before = await state(page);
  await page.waitForFunction((text) => [...document.querySelectorAll('button')].some((button) => button.textContent.trim() === text), {}, label);
  await page.evaluate((text) => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === text).click(), label);
  await page.waitForSelector('.adm-high-risk-dialog');
  if (label === 'Rollback model catalog') {
    await page.type('.adm-high-risk-dialog input[type="number"]', '3');
  }
  await page.waitForFunction((count) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.impactCalls === count + 1), {}, before.impactCalls);
  return before;
}

try {
  const page = await browser.newPage();
  await page.setViewport({ width: 320, height: 720, deviceScaleFactor: 2 });
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.evaluateOnNewDocument(() => localStorage.setItem('pinvou.admin.language', 'en'));
  await page.goto(`${origin}/admin.html?section=models`, { waitUntil: 'networkidle0' });

  for (const scenario of [
    { label: 'Publish', permission: 'platform.models.manage' },
    { label: 'Sync model catalog', permission: 'platform.models.sync' },
    { label: 'Deprecate model', permission: 'platform.models.lifecycle.request' },
    { label: 'Rollback model catalog', permission: 'platform.models.catalog.rollback.request' },
  ]) {
    await configure(page, 'permissions=full&gate=1&impact-delay=1');
    await page.reload({ waitUntil: 'networkidle0' });
    const before = await openDelayedAction(page, scenario.label);
    await configure(page, `without=${encodeURIComponent(scenario.permission)}`);
    await page.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
    await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));
    await page.waitForFunction((count) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.impactAborted > count), {}, before.impactAborted);
    const revoked = await state(page);
    assert.equal(revoked.commandCalls.length, before.commandCalls.length, `${scenario.label} permission revocation emits no Token command`);
    assert.equal(revoked.approvalCreates.length, before.approvalCreates.length, `${scenario.label} permission revocation emits no approval request`);
    await configure(page, 'permissions=full&gate=1&impact-delay=0');
    await page.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
    await page.waitForFunction((text) => [...document.querySelectorAll('button')].some((button) => button.textContent.trim() === text), {}, scenario.label);
    await new Promise((resolve) => setTimeout(resolve, 50));
    const restored = await state(page);
    assert.equal(await page.$$eval('.adm-high-risk-dialog', (nodes) => nodes.length), 0, `${scenario.label} does not resurrect after its permission is restored`);
    assert.equal(restored.commandCalls.length, revoked.commandCalls.length, `${scenario.label} permission restoration emits no delayed Token command`);
    assert.equal(restored.approvalCreates.length, revoked.approvalCreates.length, `${scenario.label} permission restoration emits no delayed approval request`);
  }

  await configure(page, 'permissions=full&gate=0&impact-delay=0');
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForFunction(() => document.querySelector('.adm-inline-message')?.textContent.includes('read-only'));
  assert.equal(await page.$$eval('.adm-source-card button', (nodes) => nodes.filter((node) => ['Publish', 'Deprecate model', 'Remove model', 'Rollback model catalog', 'Sync model catalog'].includes(node.textContent.trim())).length), 0, 'action gate off leaves the model directory read-only');

  await configure(page, 'permissions=full&gate=1');
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForFunction(() => [...document.querySelectorAll('button')].some((button) => button.textContent.trim() === 'Publish'));
  const visibleButtons = await page.$$eval('button', (nodes) => nodes.map((node) => node.textContent.trim()).filter(Boolean));
  for (const label of ['Deprecate model', 'Remove model', 'Rollback model catalog', 'Sync model catalog']) {
    assert.equal(visibleButtons.includes(label), true, `${label} is restored behind the governed action gate: ${JSON.stringify(visibleButtons)}`);
  }
  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Publish').click());
  await page.waitForSelector('.adm-high-risk-dialog textarea');
  assert.equal(await page.$eval('.adm-high-risk-dialog', (node) => node.getBoundingClientRect().width <= globalThis.innerWidth), true, 'dialog reflows inside a 320px viewport at 200% device scale');
  assert.equal(await page.$eval('.adm-high-risk-dialog', (node) => node.contains(document.activeElement)), true, 'dialog moves keyboard focus inside');
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));
  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Publish').click());
  await page.waitForSelector('.adm-high-risk-dialog textarea');
  await page.type('.adm-high-risk-dialog textarea', 'publish this validated model draft');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
  const before = (await state(page)).commandCalls.length;
  await page.evaluate(() => {
    const button = document.querySelector('.adm-high-risk-dialog button[type="submit"]');
    button.click();
    button.click();
  });
  await page.waitForFunction((count) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.commandCalls.length === count + 1), {}, before);
  await page.waitForSelector('.adm-high-risk-dialog [role="alert"]');
  assert.match(await page.$eval('.adm-high-risk-dialog [role="alert"]', (node) => node.textContent), new RegExp(commandID), 'unknown state exposes only the stable command reference required for recovery');
  await page.evaluate(() => document.querySelector('.adm-high-risk-dialog button[type="submit"]').click());
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((value) => value.resultCalls === 1));
  await page.waitForFunction(() => document.querySelector('.adm-high-risk-dialog .adm-inline-message.success')?.textContent.includes('model-action-audit'));
  await page.click('.adm-high-risk-dialog footer button:first-child');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));
  const command = (await state(page)).commandCalls.at(-1);
  assert.match(command.key, /^admin-token-action-/);
  assert.equal(command.body.command_type, 'TOKEN_MODEL_PUBLISH');
  assert.equal(command.body.command.publish_mode, 'IMMEDIATE', 'legacy immediate UI intent is canonicalized before fingerprint and persistence');
  assert.equal(command.body.reason, 'publish this validated model draft');
  assert.equal(command.body.expected_versions.expected_catalog_version, 7);
  assert.equal(command.body.expected_versions.expected_draft_version, 3);
  assert.equal(command.body.precheck_fingerprint, 'a'.repeat(64));
  assert.match(command.body.expected_versions.request_fingerprint, /^[0-9a-f]{64}$/);
  assert.equal((await state(page)).commandCalls.length, before + 1, 'same-tick double submit dispatches one model publish command');
  assert.equal((await state(page)).resultCalls, 1, 'unknown command recovery queries the frozen command reference instead of issuing another write');

  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Deprecate model').click());
  await page.waitForSelector('.adm-high-risk-dialog textarea');
  await page.type('.adm-high-risk-dialog textarea', 'deprecate after dependency and price review');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('.adm-high-risk-dialog .adm-inline-message.success'));
  const approvalState = await state(page);
  assert.equal(approvalState.approvalCreates.length, 1, 'lifecycle action creates one approval from the frozen thirteen-field snapshot');
  assert.equal(approvalState.approvalSubmits, 1, 'lifecycle draft is submitted once');
  assert.deepEqual(Object.keys(approvalState.approvalCreates[0].critical_resource_versions).sort(), ['billing_price_version', 'catalog_version', 'current_lifecycle', 'effective_at', 'existing_session_cutoff_at', 'inflight_request_set_fingerprint', 'lifecycle_plan_version', 'model_id', 'policy_version', 'provisioning_set_fingerprint', 'replacement_model_id', 'route_set_fingerprint', 'target_lifecycle'].sort());
  assert.deepEqual(approvalState.approvalCreates[0].critical_resource_versions, lifecycleImpact('DEPRECATED').critical_resource_versions, 'lifecycle approval preserves the exact authoritative thirteen-field snapshot');
  assert.deepEqual(Object.keys(approvalState.approvalCreates[0].financial_snapshot).sort(), ['billing_as_of', 'billing_price_revision', 'fully_covered', 'models_checked', 'models_covered', 'price_set_fingerprint'].sort());
  assert.deepEqual(approvalState.approvalCreates[0].financial_snapshot, lifecycleImpact('DEPRECATED').financial_snapshot, 'lifecycle approval preserves the exact authoritative six-field financial snapshot');
  await page.click('.adm-high-risk-dialog footer button:first-child');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));

  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Rollback model catalog').click());
  await page.waitForSelector('.adm-high-risk-dialog input[type="number"]');
  await page.type('.adm-high-risk-dialog input[type="number"]', '3');
  await page.type('.adm-high-risk-dialog textarea', 'rollback after complete dependency and financial review');
  await page.click('.adm-high-risk-dialog .adm-danger-confirm input');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('.adm-high-risk-dialog .adm-inline-message.success'));
  const rollbackApproval = (await state(page)).approvalCreates.at(-1);
  assert.deepEqual(Object.keys(rollbackApproval.critical_resource_versions).sort(), ['billing_price_version', 'catalog_diff_fingerprint', 'current_catalog_version', 'inflight_request_set_fingerprint', 'policy_version', 'provisioning_set_fingerprint', 'route_set_fingerprint', 'source_catalog_version'].sort());
  assert.deepEqual(rollbackApproval.critical_resource_versions, rollbackImpact(3).critical_resource_versions, 'rollback approval preserves the exact authoritative eight-field snapshot');
  assert.deepEqual(Object.keys(rollbackApproval.financial_snapshot).sort(), ['billing_as_of', 'billing_price_revision', 'fully_covered', 'models_checked', 'models_covered', 'price_set_fingerprint'].sort());
  assert.deepEqual(rollbackApproval.financial_snapshot, rollbackImpact(3).financial_snapshot, 'rollback approval preserves the exact authoritative six-field financial snapshot');
  await page.click('.adm-high-risk-dialog footer button:first-child');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));

  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Sync model catalog').click());
  await page.waitForSelector('.adm-high-risk-dialog textarea');
  await page.type('.adm-high-risk-dialog textarea', 'synchronize the reviewed provider catalog');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
  const beforeSync = (await state(page)).commandCalls.length;
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForFunction((count) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.commandCalls.length === count + 1), {}, beforeSync);
  assert.equal((await state(page)).precheckCalls.at(-1).command_type, 'TOKEN_MODEL_CATALOG_SYNC');
  assert.equal((await state(page)).commandCalls.at(-1).body.command_type, 'TOKEN_MODEL_CATALOG_SYNC');
  await page.waitForSelector('.adm-high-risk-dialog .adm-command-unknown');
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('.adm-high-risk-dialog .adm-inline-message.success'));
  await page.click('.adm-high-risk-dialog footer button:first-child');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));

  await configure(page, 'result=rejected');
  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Sync model catalog').click());
  await page.waitForSelector('.adm-high-risk-dialog textarea');
  await page.type('.adm-high-risk-dialog textarea', 'verify terminal rejection recovery');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForSelector('.adm-high-risk-dialog .adm-command-unknown');
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('.adm-high-risk-dialog [role="alert"]') && !document.querySelector('.adm-high-risk-dialog button[type="submit"]'));
  assert.equal(await page.$$eval('.adm-high-risk-dialog button[type="submit"]', (nodes) => nodes.length), 0, 'a trusted REJECTED lookup is terminal and cannot dispatch or recover again');
  await page.click('.adm-high-risk-dialog footer button:first-child');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));
  await configure(page, 'result=succeeded');

  await page.goto(`${origin}/admin.html?section=models&model_tab=protocols`, { waitUntil: 'networkidle0' });
  assert.equal(await page.$$eval('button', (nodes) => nodes.some((node) => node.textContent.trim() === 'New protocol draft')), true, 'protocol draft action is restored through the governed command path');
  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'New protocol draft').click());
  await page.waitForSelector('.adm-protocol-dialog input');
  await page.type('.adm-protocol-dialog input', 'CHAT_V2');
  const protocolTextareas = await page.$$('.adm-protocol-dialog textarea');
  await protocolTextareas.at(-1).type('create protocol through governed command');
  const protocolPrecheckBefore = (await state(page)).precheckCalls.length;
  await page.click('.adm-protocol-dialog button[type="submit"]');
  try {
    await page.waitForFunction((count) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.precheckCalls.length === count + 1), { timeout: 5000 }, protocolPrecheckBefore);
  } catch (error) {
    const diagnostic = {
      remote: await state(page),
      dialog: await page.$eval('.adm-protocol-dialog', (dialog) => ({ text: dialog.textContent, valid: dialog.checkValidity?.(), active: document.activeElement?.outerHTML })),
    };
    error.message = `${error.message}: ${JSON.stringify(diagnostic)}`;
    throw error;
  }
  await page.click('.adm-protocol-dialog button[type="submit"]');
  await page.waitForFunction(() => [...document.querySelectorAll('.adm-protocol-dialog button')].some((button) => button.textContent.trim() === 'Query original command' && !button.disabled));
  assert.equal(await page.$$eval('.adm-protocol-dialog input, .adm-protocol-dialog textarea, .adm-protocol-dialog select', (nodes) => nodes.every((node) => node.disabled)), true, 'an unknown protocol command freezes all mutable inputs while retaining the original reference');
  assert.equal(await page.$eval('.adm-protocol-dialog', (dialog) => dialog.textContent.includes('0199a100-0001-7000-8000-000000000204')), true, 'recovery displays the frozen server command reference');
  await page.click('.adm-protocol-dialog button[type="submit"]');
  try {
    await page.waitForFunction(() => document.querySelector('.adm-inline-message.success')?.textContent.includes('model-action-audit'), { timeout: 5000 });
  } catch (error) {
    const remote = await state(page);
    const diagnostic = {
      precheckCount: remote.precheckCalls.length,
      commandCount: remote.commandCalls.length,
      resultCount: remote.resultCalls,
      latestCommand: remote.commandCalls.at(-1),
      dialog: await page.$eval('.adm-protocol-dialog', (dialog) => ({ text: dialog.textContent, errorCode: dialog.querySelector('[data-error-code]')?.dataset.errorCode })),
    };
    error.message = `${error.message}: ${JSON.stringify(diagnostic)}`;
    throw error;
  }
  const protocolState = await state(page);
  assert.equal(protocolState.precheckCalls.at(-1).command_type, 'TOKEN_PROTOCOL_DRAFT_CREATE');
  assert.equal(protocolState.commandCalls.at(-1).body.command_type, 'TOKEN_PROTOCOL_DRAFT_CREATE');
  assert.equal(protocolState.commandCalls.at(-1).body.target_resource_id, 'CHAT_V2');
  assert.equal(protocolState.legacyProtocolWrites, 0, 'protocol UI never falls back to the legacy direct POST');
  assert.deepEqual(pageErrors, []);
  console.log('Admin model actions UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
