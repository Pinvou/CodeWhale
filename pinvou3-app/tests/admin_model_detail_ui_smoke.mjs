import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import puppeteer from 'puppeteer-core';
import { listenOnChromiumSafePort } from './chromium-safe-port.mjs';

const browserPath = [process.env.CHROME, 'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe', 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium or Edge is required');
const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const actorID = '0199a200-0001-7000-8000-000000000201';
const modelID = '0199a200-0001-7000-8000-000000000202';
const protocolID = '0199a200-0001-7000-8000-000000000203';
const formulaID = '0199a200-0001-7000-8000-000000000204';
const unitID = '0199a200-0001-7000-8000-000000000205';
const planID = '0199a200-0001-7000-8000-000000000217';
const replacementFirstID = '0199a200-0001-7000-8000-000000000218';
const replacementSecondID = '0199a200-0001-7000-8000-000000000219';
const commandID = '0199a200-0001-7000-8000-000000000220';
let detailsEnabled = true;
let permissions = ['platform.models.read', 'platform.models.manage', 'platform.models.lifecycle.request', 'platform.tokens.read', 'platform.pricing.read', 'platform.audit.read'];
let auditFailure = false;
let auditDelay = false;
let planStatus = 'PENDING_APPROVAL';
let planVersion = 1;
let planTarget = 'DEPRECATED';
let lastPlanInput = null;
let planCreateUnknownOnce = false;
const calls = { detail: 0, history: 0, historyCursor: '', candidates: 0, candidateCursor: '', candidateQuery: '', provisionings: 0, pricing: 0, audit: 0, auditAborted: 0, impact: 0, plans: 0, planCreate: 0, planCreateKeys: [], planCancel: 0, publishPrecheck: 0, publishCommands: 0 };

function json(response, value, status = 200) {
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(value));
}
const envelope = items => ({ items, next_cursor: '', has_more: false, limit: 50, as_of: '2026-09-04T12:00:00Z', snapshot_expires_at: '2026-09-04T12:15:00Z', request_id: 'model-detail-smoke' });
const model = { model_id: modelID, catalog_version: 7, display_name: 'Flash model', description: 'Read-only model asset', aliases: [], lifecycle_status: 'PUBLISHED', protocol_code: 'CHAT', protocol_version: 2, protocol_definition_id: protocolID, model_capability_version_id: '0199a200-0001-7000-8000-000000000206', capability_version: 3, context_length: 128000, maximum_output_tokens: 8192, supported_modalities: ['TEXT'], tokenizer_version: 'v2', source_available: true, source_missing_confirmations: 0, force_cancel_inflight: false };
const detail = { as_of: '2026-09-04T12:00:00Z', model, protocol: { protocol_definition_id: protocolID, protocol_code: 'CHAT', version: 2, status: 'PUBLISHED', schema_field_count: 4, etag: 'a'.repeat(64), created_at: '2026-09-01T00:00:00Z', published_at: '2026-09-02T00:00:00Z' }, formula: { formula_version_id: formulaID, protocol_definition_id: protocolID, protocol_code: 'CHAT', version: 4, status: 'PUBLISHED', required_unit_versions: { INPUT_TOKEN: unitID }, evaluator_version: 'SAFE_V1', etag: 'b'.repeat(64), created_at: '2026-09-02T00:00:00Z', published_at: '2026-09-03T00:00:00Z' } };
const dependencies = Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map((key) => [key, 0]));
const historicalVersion = { catalog_version: 6, display_name: 'Flash model v2', lifecycle_status: 'PUBLISHED', model_capability_version_id: '0199a200-0001-7000-8000-000000000216', capability_version: 2, protocol_definition_id: protocolID, protocol_code: 'CHAT', protocol_version: 2, schema_field_count: 4, context_length: 64000, maximum_output_tokens: 4096, supported_modalities: ['TEXT'], tokenizer_version: 'v1', source_available: true, published_at: '2026-08-01T00:00:00Z' };
const lifecyclePlan = () => ({ lifecycle_plan_id: planID, model_id: modelID, lifecycle_plan_version: planVersion, expected_catalog_version: 7, current_lifecycle: model.lifecycle_status, target_lifecycle: planTarget, effective_at: lastPlanInput?.effective_at || '2026-09-06T12:00:00Z', existing_session_cutoff_at: lastPlanInput?.existing_session_cutoff_at ?? null, replacement_model_id: lastPlanInput?.replacement_model_id ?? null, force_cancel_inflight: false, status: planStatus, reason: lastPlanInput?.reason || 'deprecate after reviewed dependency window', execution_attempt: 0, created_by: actorID, created_at: '2026-09-05T12:00:00Z', updated_at: '2026-09-05T12:01:00Z' });

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/configure') {
    if (url.searchParams.has('gate')) detailsEnabled = url.searchParams.get('gate') === '1';
    if (url.searchParams.get('permissions') === 'models') permissions = ['platform.models.read'];
    if (url.searchParams.get('permissions') === 'full') permissions = ['platform.models.read', 'platform.models.manage', 'platform.models.lifecycle.request', 'platform.tokens.read', 'platform.pricing.read', 'platform.audit.read'];
    if (url.searchParams.get('permissions') === 'none') permissions = [];
    auditFailure = url.searchParams.get('audit-failure') === '1';
    auditDelay = url.searchParams.get('audit-delay') === '1';
    if (url.searchParams.get('plan-create-unknown') === '1') planCreateUnknownOnce = true;
    if (url.searchParams.has('model-lifecycle')) {
      model.lifecycle_status = url.searchParams.get('model-lifecycle');
      planStatus = 'CANCELLED';
      lastPlanInput = null;
    }
    return json(response, { detailsEnabled, permissions, auditFailure, auditDelay });
  }
  if (url.pathname === '/__test__/state') return json(response, calls);
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf', feature_flags: { admin_model_token_ia: true, admin_model_details: detailsEnabled, admin_model_token_actions: true } });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: actorID, platform_permissions: permissions });
  if (url.pathname === '/api/admin/token/admin/v1/models' && url.searchParams.get('lifecycle_status') === 'PUBLISHED') {
    calls.candidates += 1;
    calls.candidateCursor = url.searchParams.get('cursor') || '';
    calls.candidateQuery = url.searchParams.get('q') || '';
    if (calls.candidateCursor === 'expired-candidate-page') return json(response, { code: 'PAGINATION_SNAPSHOT_EXPIRED', request_id: 'candidate-expired' }, 410);
    const candidate = calls.candidateCursor === 'candidate-page-2'
      ? { ...model, model_id: replacementSecondID, display_name: 'Replacement beyond first page' }
      : { ...model, model_id: replacementFirstID, display_name: 'Replacement first page' };
    return json(response, { ...envelope([candidate]), next_cursor: calls.candidateCursor ? '' : 'candidate-page-2', has_more: !calls.candidateCursor });
  }
  if (url.pathname === '/api/admin/token/admin/v1/models') return json(response, envelope([model]));
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}`) { calls.detail += 1; return json(response, detail); }
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}/versions`) {
    calls.history += 1;
    calls.historyCursor = url.searchParams.get('cursor') || '';
    return json(response, { ...envelope([historicalVersion]), next_cursor: calls.historyCursor ? '' : 'history-page-2', has_more: !calls.historyCursor });
  }
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}/lifecycle-plans` && request.method === 'GET') {
    calls.plans += 1;
    return json(response, envelope([lifecyclePlan()]));
  }
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}/lifecycle-plans` && request.method === 'POST') {
    calls.planCreate += 1;
    assert.match(String(request.headers['idempotency-key'] || ''), /^model-lifecycle-plan-/);
    calls.planCreateKeys.push(String(request.headers['idempotency-key']));
    const body = await new Promise((resolve, reject) => { let value = ''; request.on('data', chunk => { value += chunk; }); request.on('end', () => resolve(JSON.parse(value))); request.on('error', reject); });
    assert.equal(body.force_cancel_inflight, false);
    assert.equal(body.expected_plan_version, 0);
    if (planCreateUnknownOnce) {
      planCreateUnknownOnce = false;
      return json(response, { code: 'COMMAND_RESULT_UNKNOWN', message: 'query the same idempotency key', request_id: 'plan-create-unknown' }, 503);
    }
    if (model.lifecycle_status === 'DRAFT') {
      assert.equal(body.target_lifecycle, 'PUBLISHED');
      assert.equal(body.existing_session_cutoff_at, null);
      assert.equal(body.replacement_model_id, null);
    }
    planTarget = body.target_lifecycle;
    lastPlanInput = body;
    planVersion += 1;
    planStatus = 'PENDING_APPROVAL';
    return json(response, lifecyclePlan(), 201);
  }
  if (url.pathname === '/api/admin/token/admin/v1/commands:precheck' && request.method === 'POST') {
    calls.publishPrecheck += 1;
    const body = await new Promise((resolve, reject) => { let value = ''; request.on('data', chunk => { value += chunk; }); request.on('end', () => resolve(JSON.parse(value))); request.on('error', reject); });
    assert.equal(body.command_type, 'TOKEN_MODEL_PUBLISH');
    assert.equal(body.target_resource_type, 'MODEL');
    assert.equal(body.target_resource_id, modelID);
    assert.equal(body.command.model_id, modelID);
    assert.equal(body.command.publish_mode, 'SCHEDULED');
    assert.match(body.command.lifecycle_plan_id, /^[0-9a-f-]{36}$/);
    assert.equal(body.command.lifecycle_plan_version, planVersion + 1);
    assert.equal(Number.isNaN(Date.parse(body.command.effective_at)), false);
    lastPlanInput = { ...body.command, reason: 'publish after reviewed serving and pricing readiness' };
    return json(response, {
      action: 'TOKEN_MODEL_PUBLISH', blockers: [], requires_cas_revalidation: true,
      snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', precheck_fingerprint: '9'.repeat(64),
      evidence: { resource_type: 'MODEL', resource_id: modelID, current_status: 'DRAFT', current_version: { catalog_version: 7, evidence_etag: `"impact-${'8'.repeat(64)}"`, evidence_set_sha256: '7'.repeat(64) }, dependencies },
      expected_versions: { model_id: modelID, expected_catalog_version: 7, expected_draft_version: 3, draft_fingerprint: '6'.repeat(64), protocol_definition_id: protocolID, protocol_version: 2, stable_route_count: 1, stable_route_set_fingerprint: '5'.repeat(64), provisioning_count: 1, ready_route_count: 1, provisioning_set_fingerprint: '4'.repeat(64), financial_fact_revision: 9, price_set_fingerprint: '3'.repeat(64) },
      token_observed_at: '2026-09-05T12:00:00Z', billing_observed_at: '2026-09-05T12:00:00Z',
    });
  }
  if (url.pathname === '/api/admin/token/admin/v1/commands' && request.method === 'POST') {
    calls.publishCommands += 1;
    assert.match(String(request.headers['idempotency-key'] || ''), /^admin-token-action-[0-9a-f-]{36}$/);
    const body = await new Promise((resolve, reject) => { let value = ''; request.on('data', chunk => { value += chunk; }); request.on('end', () => resolve(JSON.parse(value))); request.on('error', reject); });
    assert.equal(body.command_type, 'TOKEN_MODEL_PUBLISH');
    assert.equal(body.command.publish_mode, 'SCHEDULED');
    assert.equal(body.command.lifecycle_plan_id, lastPlanInput.lifecycle_plan_id);
    assert.equal(body.command.lifecycle_plan_version, lastPlanInput.lifecycle_plan_version);
    assert.equal(body.command.effective_at, new Date(lastPlanInput.effective_at).toISOString().replace('.000Z', 'Z'), 'scheduled command uses the same UTC whole-second instant that Token canonicalizes');
    assert.match(body.expected_versions.request_fingerprint, /^[0-9a-f]{64}$/);
    planTarget = 'PUBLISHED';
    planVersion = body.command.lifecycle_plan_version;
    planStatus = 'SCHEDULED';
    return json(response, { command_reference: commandID, command_type: 'TOKEN_MODEL_PUBLISH', policy_version: 1, actor_user_id: actorID, actor_permission: 'platform.models.manage', authorization_version: 3, idempotency_fingerprint: '2'.repeat(64), target_resource_type: 'MODEL', target_resource_id: modelID, status: 'SUCCEEDED', redacted_result: { lifecycle_plan_id: body.command.lifecycle_plan_id, lifecycle_plan_version: body.command.lifecycle_plan_version, status: 'SCHEDULED' }, stable_error_code: null, approval_request_id: null, audit_request_id: 'model-publish-schedule-audit', created_at: '2026-09-05T12:00:00Z', completed_at: '2026-09-05T12:00:01Z' }, 201);
  }
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}/lifecycle-plans/${planID}/cancel` && request.method === 'POST') {
    calls.planCancel += 1;
    assert.match(String(request.headers['idempotency-key'] || ''), /^admin-token-action-[0-9a-f-]{36}$/);
    planStatus = 'CANCELLED';
    return json(response, lifecyclePlan());
  }
  if (url.pathname === '/api/admin/token/admin/v1/provisionings') { calls.provisionings += 1; return json(response, envelope([{ provisioning_id: '0199a200-0001-7000-8000-000000000207', model_id: modelID, status: 'READY', stage: 'VERIFIED', desired_generation: 2, actual_generation: 2, updated_at: '2026-09-04T12:00:00Z' }])); }
  if (url.pathname === `/api/admin/billing/admin/v1/models/${modelID}/price-projection`) { calls.pricing += 1; return json(response, { model_id: modelID, protocol_code: 'CHAT', formula_version_id: formulaID, credit_unit: 'MICROCREDIT', as_of: '2026-09-04T12:00:00Z', source: 'BILLING', snapshot_semantics: 'BILLING_REPEATABLE_READ_ONLY; TOKEN_CATALOG_NOT_READ; NO_CROSS_DOMAIN_ATOMICITY', current: { effective_at: '2026-09-04T12:00:00Z', coverage_status: 'AVAILABLE', price: { price_version_id: '0199a200-0001-7000-8000-000000000208', scope_type: 'MODEL', space_etag: 2, effective_from: '2026-09-01T00:00:00Z', effective_to: null, units: [{ unit_code: 'INPUT_TOKEN', unit_definition_id: unitID, unit_version: 1, minimum_precision: 1, price_denominator: 1000000, rounding_rule: 'CEILING', resolved_price: 3, configuration_type: 'FIXED_PRICE', price_source_scope: 'MODEL', source_price_version_id: '0199a200-0001-7000-8000-000000000208' }] }, }, next_change: null }); }
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}/impact/lifecycle`) {
    calls.impact += 1;
    const target = url.searchParams.get('target_status') || 'DEPRECATED';
    const frozenVersion = url.searchParams.has('lifecycle_plan_id') ? planVersion : 0;
    return json(response, { action: `MODEL_LIFECYCLE_${target}`, blockers: [], requires_cas_revalidation: true, snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', precheck_fingerprint: 'd'.repeat(64), evidence: { resource_type: 'MODEL', resource_id: modelID, current_status: model.lifecycle_status, current_version: { catalog_version: 7, evidence_etag: `"impact-${'e'.repeat(64)}"`, evidence_set_sha256: 'f'.repeat(64) }, dependencies }, price_coverage: { models_checked: 1, models_covered: 1, fully_covered: true, versions: [{ model_id: modelID, protocol_code: 'CHAT', formula_version_id: formulaID, credit_unit: 'MICROCREDIT', covered: true, price_version_id: '0199a200-0001-7000-8000-000000000208', price_version: 2, effective_from: '2026-09-01T00:00:00Z' }], billing_observed_at: '2026-09-04T11:59:59Z', financial_fact_revision: 9, price_set_fingerprint: 'c'.repeat(64) }, critical_resource_versions: { model_id: modelID, catalog_version: 7, current_lifecycle: model.lifecycle_status, target_lifecycle: target, lifecycle_plan_version: frozenVersion, effective_at: lifecyclePlan().effective_at, existing_session_cutoff_at: null, replacement_model_id: null, billing_price_version: 9, route_set_fingerprint: '1'.repeat(64), provisioning_set_fingerprint: '2'.repeat(64), inflight_request_set_fingerprint: '3'.repeat(64), policy_version: 2 }, financial_snapshot: { billing_price_revision: 9, price_set_fingerprint: 'c'.repeat(64), models_checked: 1, models_covered: 1, fully_covered: true, billing_as_of: '2026-09-04T11:59:59Z' }, token_observed_at: '2026-09-04T12:00:00Z', billing_observed_at: '2026-09-04T11:59:59Z' });
  }
  if (url.pathname === '/api/admin/token/admin/v1/resource-audit') {
    calls.audit += 1;
    const fail = auditFailure;
    const delayed = auditDelay;
    if (delayed) {
      let settled = false;
      const timer = setTimeout(() => {
        if (settled) return;
        settled = true;
        json(response, envelope([{ audit_id: '0199a200-0001-7000-8000-000000000209', action: 'PUBLISHED', resource_type: 'MODEL', resource_id: modelID, actor_id: actorID, summary: { before_status: 'DRAFT', after_status: 'PUBLISHED', catalog_version: 7 }, occurred_at: '2026-09-04T12:00:00Z' }]));
      }, 1500);
      request.once('aborted', () => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        calls.auditAborted += 1;
      });
      return;
    }
    return fail ? json(response, { code: 'AUDIT_UNAVAILABLE', request_id: 'audit-down' }, 503) : json(response, envelope([{ audit_id: '0199a200-0001-7000-8000-000000000209', action: 'PUBLISHED', resource_type: 'MODEL', resource_id: modelID, actor_id: actorID, summary: { before_status: 'DRAFT', after_status: 'PUBLISHED', catalog_version: 7 }, occurred_at: '2026-09-04T12:00:00Z' }]));
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
try {
  const page = await browser.newPage();
  await page.setViewport({ width: 320, height: 720, deviceScaleFactor: 2 });
  await page.evaluateOnNewDocument(() => localStorage.setItem('pinvou.admin.language', 'en'));
  const listURL = `${origin}/admin.html?section=models&model_tab=directory&model_q=flash&model_cursor=list-page`;
  await page.goto(listURL, { waitUntil: 'networkidle0' });
  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'View').click());
  await page.waitForSelector('.adm-model-detail');
  assert.equal(new URL(page.url()).searchParams.get('model_id'), modelID);
  assert.deepEqual(await page.$$eval('.adm-model-detail-tabs button', (nodes) => nodes.map((node) => node.textContent.trim())), ['Overview', 'Capabilities, protocol & formula', 'Routes & runtime', 'Pricing & metering', 'Lifecycle & dependencies', 'Audit']);
  assert.equal((await page.$eval('.adm-model-detail', (node) => node.textContent)).includes('private-provider'), false);

  await page.evaluate(() => [...document.querySelectorAll('.adm-model-detail-tabs button')].find((button) => button.textContent.trim() === 'Pricing & metering').click());
  await page.waitForFunction(() => document.body.textContent.includes('Independent Billing read snapshot'));
  assert.equal(calls.pricing, 1);
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForFunction(() => document.body.textContent.includes('Independent Billing read snapshot'));
  assert.equal(new URL(page.url()).searchParams.get('model_detail_tab'), 'pricing');
  assert.equal(calls.pricing, 2);
  await page.evaluate(() => [...document.querySelectorAll('.adm-model-detail-tabs button')].find((button) => button.textContent.trim() === 'Capabilities, protocol & formula').click());
  await page.waitForFunction(() => document.body.textContent.includes('SAFE_V1') && document.body.textContent.includes('Version history'));
  assert.equal(calls.history, 1);
  await page.evaluate(() => [...document.querySelector('#model-history-heading').closest('section').querySelectorAll('button')].find((button) => button.textContent.trim() === 'Next').click());
  await page.waitForFunction(() => new URL(location.href).searchParams.get('model_history_cursor') === 'history-page-2');
  assert.equal(calls.historyCursor, 'history-page-2');
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForFunction(() => document.body.textContent.includes('Version history'));
  assert.equal(new URL(page.url()).searchParams.get('model_history_cursor'), 'history-page-2');
  await page.evaluate(() => [...document.querySelectorAll('.adm-model-detail-tabs button')].find((button) => button.textContent.trim() === 'Lifecycle & dependencies').click());
  await page.waitForFunction(() => document.body.textContent.includes('Dependency snapshot'));
  assert.equal(calls.impact, 1);
  const lifecycleText = await page.$eval('#model-dependencies-heading', (heading) => heading.closest('section').textContent);
  for (const label of ['Related routes', 'Healthy routes', 'Provisioning records', 'Ready Provisioning', 'Active leases', 'In-flight requests', 'Active sessions', 'Alternative credentials', 'Alternative Token accounts', 'Affected coverage tuples', 'Remaining ready coverage', 'Coverage gaps', 'Token observed at', 'Billing observed at', 'Price coverage', 'Financial fact revision']) assert.equal(lifecycleText.includes(label), true, `missing translated lifecycle fact: ${label}`);
  for (const internalKey of Object.keys(dependencies).filter((key) => key.includes('_'))) assert.equal(lifecycleText.includes(internalKey), false, `internal dependency key leaked: ${internalKey}`);
  await page.waitForFunction(() => document.body.textContent.includes('Lifecycle plans') && document.body.textContent.includes('v1'));
  assert.ok(calls.plans >= 2, 'visible and authoritative active-plan sources must load independently');
  await page.select('.adm-lifecycle-plan-fields select', 'REMOVED');
  await page.waitForFunction(() => document.body.textContent.includes('Replacement candidates'));
  await page.waitForFunction(() => document.body.textContent.includes('Replacement first page'));
  await page.select('.adm-lifecycle-plan-fields label:last-child select', replacementFirstID);
  await page.evaluate(() => [...document.querySelector('#model-lifecycle-candidates-heading').closest('section').querySelectorAll('button')].find((button) => button.textContent.trim() === 'Next').click());
  await page.waitForFunction(() => new URL(location.href).searchParams.get('model_lifecycle_candidate_cursor') === 'candidate-page-2');
  await page.waitForFunction(() => document.body.textContent.includes('Replacement beyond first page'));
  assert.equal(await page.$eval('.adm-lifecycle-plan-fields label:last-child select', (node) => node.value), replacementFirstID, 'stable selected candidate must survive pagination');
  await page.evaluate(() => {
    const url = new URL(location.href);
    url.searchParams.set('model_lifecycle_candidate_cursor', 'expired-candidate-page');
    history.pushState({}, '', url);
    dispatchEvent(new PopStateEvent('popstate'));
  });
  await page.waitForFunction(() => document.querySelector('#model-lifecycle-candidates-heading').closest('section').querySelector('.adm-source-error'));
  await page.evaluate(() => document.querySelector('#model-lifecycle-candidates-heading').closest('section').querySelector('.adm-source-error button').click());
  await page.waitForFunction(() => !new URL(location.href).searchParams.has('model_lifecycle_candidate_cursor'));
  await page.waitForFunction(() => document.body.textContent.includes('Replacement first page'));
  assert.ok(calls.candidates >= 4, 'candidate source must independently rebuild an expired snapshot');
  await page.select('.adm-lifecycle-plan-fields select', 'DEPRECATED');
  await page.evaluate(() => {
    const section = document.querySelector('#model-lifecycle-plans-heading').closest('section');
    [...section.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Cancel').click();
  });
  await page.waitForFunction(() => document.querySelector('[role="dialog"]')?.textContent.includes('Cancel lifecycle plan'));
  await page.waitForFunction(() => [...document.querySelectorAll('[role="dialog"] footer button')].some((button) => button.textContent.trim() === 'Confirm' && !button.disabled));
  await page.evaluate(() => [...document.querySelectorAll('[role="dialog"] footer button')].find((button) => button.textContent.trim() === 'Confirm').click());
  await page.waitForFunction(() => document.querySelector('[role="dialog"]')?.textContent.includes('Operation accepted'));
  assert.equal(calls.planCancel, 1);
  await page.evaluate(() => [...document.querySelectorAll('[role="dialog"] footer button')].find((button) => button.textContent.trim() === 'Close').click());
  await page.waitForFunction(() => document.body.textContent.includes('Cancelled'));
  await page.type('.adm-lifecycle-plan-form textarea', 'schedule after reviewed dependency window');
  const directCreatesBefore = calls.planCreate;
  await page.evaluate(() => {
    const button = [...document.querySelectorAll('.adm-lifecycle-plan-form button')].find((candidate) => candidate.textContent.trim() === 'Save plan and review impact');
    button.click();
    button.click();
  });
  await page.waitForFunction(() => document.querySelector('[role="dialog"]')?.textContent.includes('Request lifecycle-plan approval'));
  assert.equal(calls.planCreate, directCreatesBefore + 1, 'same-tick double submit creates one approval-required plan');
  const firstCreatedKey = calls.planCreateKeys.at(-1);
  assert.equal(new URL(page.url()).searchParams.get('model_detail_tab'), 'lifecycle');
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => [...document.querySelector('#model-lifecycle-plans-heading').closest('section').querySelectorAll('button')].some((button) => button.textContent.trim() === 'Cancel'));
  await page.evaluate(() => [...document.querySelector('#model-lifecycle-plans-heading').closest('section').querySelectorAll('button')].find((button) => button.textContent.trim() === 'Cancel').click());
  await page.waitForFunction(() => document.querySelector('[role="dialog"]')?.textContent.includes('Cancel lifecycle plan'));
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
  await page.click('.adm-high-risk-dialog button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('.adm-high-risk-dialog .adm-inline-message.success'));
  await page.click('.adm-high-risk-dialog footer button:first-child');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));
  const recreateBefore = calls.planCreate;
  await page.evaluate(() => fetch('/__test__/configure?plan-create-unknown=1'));
  await page.click('.adm-lifecycle-plan-form button[type="submit"]');
  await page.waitForFunction((minimum) => fetch('/__test__/state').then((response) => response.json()).then((state) => state.planCreate === minimum + 1), {}, recreateBefore);
  await page.waitForSelector('.adm-lifecycle-plan-form [role="alert"]');
  const unknownCreateKey = calls.planCreateKeys.at(-1);
  await page.click('.adm-lifecycle-plan-form button[type="submit"]');
  await page.waitForFunction((minimum) => fetch('/__test__/state').then((response) => response.json()).then((state) => state.planCreate === minimum + 2), {}, recreateBefore);
  await page.waitForFunction(() => document.querySelector('[role="dialog"]')?.textContent.includes('Request lifecycle-plan approval'));
  assert.notEqual(calls.planCreateKeys.at(-1), firstCreatedKey, 'successful create clears its idempotency key so cancel and same payload create a new plan');
  assert.equal(calls.planCreateKeys.at(-1), unknownCreateKey, 'unknown create retains the same idempotency key for an exact-payload retry');
  await page.keyboard.press('Escape');
  await page.evaluate(() => [...document.querySelectorAll('.adm-model-detail-tabs button')].find((button) => button.textContent.trim() === 'Routes & runtime').click());
  await page.waitForFunction(() => document.body.textContent.includes('Ready'));
  assert.equal(calls.provisionings, 1);
  await page.evaluate(() => [...document.querySelectorAll('.adm-model-detail-tabs button')].find((button) => button.textContent.trim() === 'Audit').click());
  await page.waitForFunction(() => document.body.textContent.includes('PUBLISHED'));
  assert.equal(calls.audit, 1);
  await page.goBack({ waitUntil: 'networkidle0' });
  await page.waitForFunction(() => new URL(location.href).searchParams.get('model_detail_tab') === 'operations' && document.body.textContent.includes('Ready'));
  await page.goForward({ waitUntil: 'networkidle0' });
  await page.waitForFunction(() => new URL(location.href).searchParams.get('model_detail_tab') === 'audit' && document.body.textContent.includes('PUBLISHED'));
  await page.evaluate(() => document.querySelector('.adm-model-detail-header .adm-back').click());
  await page.waitForSelector('#model-directory-title');
  const returned = new URL(page.url());
  assert.equal(returned.searchParams.get('model_q'), 'flash');
  assert.equal(returned.searchParams.get('model_cursor'), 'list-page');
  assert.equal(returned.searchParams.has('model_id'), false);

  await page.evaluate(() => fetch('/__test__/configure?permissions=full&audit-failure=1'));
  const pricingBeforeAuditFailure = calls.pricing;
  await page.goto(`${origin}/admin.html?section=models&model_tab=directory&model_id=${modelID}&model_detail_tab=audit`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-source-error');
  assert.equal(await page.$eval('.adm-model-detail-header', (node) => node.textContent.includes('Flash model')), true);
  assert.equal(calls.pricing, pricingBeforeAuditFailure, 'an audit failure must not request or invalidate Billing');
  await page.evaluate(() => fetch('/__test__/configure?permissions=full'));
  await page.evaluate(() => document.querySelector('.adm-source-error button').click());
  await page.waitForFunction(() => document.body.textContent.includes('PUBLISHED'));

  await page.evaluate(() => fetch('/__test__/configure?permissions=full&audit-delay=1'));
  const auditBeforeRevoke = calls.audit;
  await page.goto(`${origin}/admin.html?section=models&model_tab=directory&model_id=${modelID}&model_detail_tab=audit`, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction((minimum) => fetch('/__test__/state').then((response) => response.json()).then((state) => state.audit > minimum), {}, auditBeforeRevoke);
  await page.evaluate(() => fetch('/__test__/configure?permissions=models'));
  await page.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
  await page.waitForFunction(() => new URL(location.href).searchParams.get('model_detail_tab') === 'overview');
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.auditAborted > 0));
  assert.equal(await page.$$eval('#model-audit-heading', (nodes) => nodes.length), 0);

  await page.evaluate(() => fetch('/__test__/configure?permissions=models'));
  const beforePricing = calls.pricing;
  const beforeAudit = calls.audit;
  await page.goto(`${origin}/admin.html?section=models&model_tab=directory&model_id=${modelID}&model_detail_tab=pricing`, { waitUntil: 'networkidle0' });
  assert.equal(new URL(page.url()).searchParams.get('model_detail_tab'), 'overview');
  assert.equal(await page.$$eval('.adm-model-detail-tabs button', (nodes) => nodes.some((node) => node.textContent.includes('Routes') || node.textContent.includes('Pricing') || node.textContent.includes('Audit'))), false);
  assert.equal(calls.pricing, beforePricing);
  assert.equal(calls.audit, beforeAudit);

  await page.evaluate(() => fetch('/__test__/configure?permissions=full&model-lifecycle=DRAFT'));
  await page.goto(`${origin}/admin.html?section=models&model_tab=directory&model_id=${modelID}&model_detail_tab=lifecycle`, { waitUntil: 'networkidle0' });
  await page.waitForFunction(() => document.body.textContent.includes('Lifecycle plans'));
  assert.equal(await page.$eval('.adm-lifecycle-plan-fields select', (node) => node.value), 'PUBLISHED');
  await page.type('.adm-lifecycle-plan-form textarea', 'publish after reviewed serving and pricing readiness');
  await page.waitForFunction(() => ![...document.querySelectorAll('.adm-lifecycle-plan-form button')].find((button) => button.textContent.trim() === 'Save plan and review impact').disabled);
  const prechecksBeforePublish = calls.publishPrecheck;
  const commandsBeforePublish = calls.publishCommands;
  const lifecycleCreatesBeforePublish = calls.planCreate;
  const expectedPublishPlanVersion = planVersion + 1;
  await page.evaluate(() => [...document.querySelectorAll('.adm-lifecycle-plan-form button')].find((button) => button.textContent.trim() === 'Save plan and review impact').click());
  await page.waitForFunction((minimum) => fetch('/__test__/state').then((response) => response.json()).then((state) => state.publishPrecheck > minimum), {}, prechecksBeforePublish);
  await page.waitForFunction(() => document.querySelector('[role="dialog"]')?.textContent.includes('Publish'));
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled);
  await page.evaluate(() => {
    const button = document.querySelector('.adm-high-risk-dialog button[type="submit"]');
    button.click();
    button.click();
  });
  await page.waitForFunction((minimum) => fetch('/__test__/state').then((response) => response.json()).then((state) => state.publishCommands === minimum + 1), {}, commandsBeforePublish);
  await page.waitForFunction(() => document.querySelector('.adm-high-risk-dialog .adm-inline-message.success')?.textContent.includes('model-publish-schedule-audit'));
  assert.equal(calls.planCreate, lifecycleCreatesBeforePublish, 'scheduled publish never uses the approval-required lifecycle-plan endpoint');
  assert.equal(calls.publishCommands, commandsBeforePublish + 1, 'same-tick double submit creates one governed scheduled publish command');
  assert.equal(lastPlanInput.publish_mode, 'SCHEDULED');
  assert.equal(lastPlanInput.lifecycle_plan_version, expectedPublishPlanVersion);
  await page.click('.adm-high-risk-dialog footer button:first-child');
  await page.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));
  await page.evaluate(() => fetch('/__test__/configure?permissions=full&model-lifecycle=PUBLISHED'));

  await page.evaluate(() => fetch('/__test__/configure?permissions=none&gate=1'));
  const beforeForbiddenDetail = calls.detail;
  await page.goto(`${origin}/admin.html?section=models&model_tab=directory&model_id=${modelID}&model_detail_tab=overview`, { waitUntil: 'networkidle0' });
  assert.equal(new URL(page.url()).searchParams.has('model_id'), false);
  assert.equal(calls.detail, beforeForbiddenDetail);
  assert.equal((await page.$eval('body', (node) => node.textContent)).includes(modelID), false);

  await page.evaluate(() => fetch('/__test__/configure?permissions=full&gate=0'));
  const beforeDetail = calls.detail;
  await page.goto(`${origin}/admin.html?section=models&model_tab=directory&model_id=${modelID}&model_detail_tab=audit&model_audit_cursor=secret&model_history_cursor=secret-history`, { waitUntil: 'networkidle0' });
  assert.equal(new URL(page.url()).searchParams.has('model_id'), false);
  assert.equal(new URL(page.url()).searchParams.has('model_audit_cursor'), false);
  assert.equal(new URL(page.url()).searchParams.has('model_history_cursor'), false);
  assert.equal(calls.detail, beforeDetail);
  assert.equal(await page.$$eval('.adm-model-detail', (nodes) => nodes.length), 0);
  console.log('Admin model detail UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
