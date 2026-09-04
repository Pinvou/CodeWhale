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
let detailsEnabled = true;
let permissions = ['platform.models.read', 'platform.tokens.read', 'platform.pricing.read', 'platform.audit.read'];
let auditFailure = false;
let auditDelay = false;
const calls = { detail: 0, history: 0, historyCursor: '', provisionings: 0, pricing: 0, audit: 0, auditAborted: 0, impact: 0 };

function json(response, value, status = 200) {
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(value));
}
const envelope = items => ({ items, next_cursor: '', has_more: false, limit: 50, as_of: '2026-09-04T12:00:00Z', snapshot_expires_at: '2026-09-04T12:15:00Z', request_id: 'model-detail-smoke' });
const model = { model_id: modelID, catalog_version: 7, display_name: 'Flash model', description: 'Read-only model asset', aliases: [], lifecycle_status: 'PUBLISHED', protocol_code: 'CHAT', protocol_version: 2, protocol_definition_id: protocolID, model_capability_version_id: '0199a200-0001-7000-8000-000000000206', capability_version: 3, context_length: 128000, maximum_output_tokens: 8192, supported_modalities: ['TEXT'], tokenizer_version: 'v2', source_available: true, source_missing_confirmations: 0, force_cancel_inflight: false };
const detail = { as_of: '2026-09-04T12:00:00Z', model, protocol: { protocol_definition_id: protocolID, protocol_code: 'CHAT', version: 2, status: 'PUBLISHED', schema_field_count: 4, etag: 'a'.repeat(64), created_at: '2026-09-01T00:00:00Z', published_at: '2026-09-02T00:00:00Z' }, formula: { formula_version_id: formulaID, protocol_definition_id: protocolID, protocol_code: 'CHAT', version: 4, status: 'PUBLISHED', required_unit_versions: { INPUT_TOKEN: unitID }, evaluator_version: 'SAFE_V1', etag: 'b'.repeat(64), created_at: '2026-09-02T00:00:00Z', published_at: '2026-09-03T00:00:00Z' } };
const dependencies = Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map((key) => [key, 0]));
const historicalVersion = { catalog_version: 6, display_name: 'Flash model v2', lifecycle_status: 'PUBLISHED', model_capability_version_id: '0199a200-0001-7000-8000-000000000216', capability_version: 2, protocol_definition_id: protocolID, protocol_code: 'CHAT', protocol_version: 2, schema_field_count: 4, context_length: 64000, maximum_output_tokens: 4096, supported_modalities: ['TEXT'], tokenizer_version: 'v1', source_available: true, published_at: '2026-08-01T00:00:00Z' };

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/configure') {
    if (url.searchParams.has('gate')) detailsEnabled = url.searchParams.get('gate') === '1';
    if (url.searchParams.get('permissions') === 'models') permissions = ['platform.models.read'];
    if (url.searchParams.get('permissions') === 'full') permissions = ['platform.models.read', 'platform.tokens.read', 'platform.pricing.read', 'platform.audit.read'];
    if (url.searchParams.get('permissions') === 'none') permissions = [];
    auditFailure = url.searchParams.get('audit-failure') === '1';
    auditDelay = url.searchParams.get('audit-delay') === '1';
    return json(response, { detailsEnabled, permissions, auditFailure, auditDelay });
  }
  if (url.pathname === '/__test__/state') return json(response, calls);
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf', feature_flags: { admin_model_token_ia: true, admin_model_details: detailsEnabled, admin_model_token_actions: false } });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: actorID, platform_permissions: permissions });
  if (url.pathname === '/api/admin/token/admin/v1/models') return json(response, envelope([model]));
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}`) { calls.detail += 1; return json(response, detail); }
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}/versions`) {
    calls.history += 1;
    calls.historyCursor = url.searchParams.get('cursor') || '';
    return json(response, { ...envelope([historicalVersion]), next_cursor: calls.historyCursor ? '' : 'history-page-2', has_more: !calls.historyCursor });
  }
  if (url.pathname === '/api/admin/token/admin/v1/provisionings') { calls.provisionings += 1; return json(response, envelope([{ provisioning_id: '0199a200-0001-7000-8000-000000000207', model_id: modelID, status: 'READY', stage: 'VERIFIED', desired_generation: 2, actual_generation: 2, updated_at: '2026-09-04T12:00:00Z' }])); }
  if (url.pathname === `/api/admin/billing/admin/v1/models/${modelID}/price-projection`) { calls.pricing += 1; return json(response, { model_id: modelID, protocol_code: 'CHAT', formula_version_id: formulaID, credit_unit: 'MICROCREDIT', as_of: '2026-09-04T12:00:00Z', source: 'BILLING', snapshot_semantics: 'BILLING_REPEATABLE_READ_ONLY; TOKEN_CATALOG_NOT_READ; NO_CROSS_DOMAIN_ATOMICITY', current: { effective_at: '2026-09-04T12:00:00Z', coverage_status: 'AVAILABLE', price: { price_version_id: '0199a200-0001-7000-8000-000000000208', scope_type: 'MODEL', space_etag: 2, effective_from: '2026-09-01T00:00:00Z', effective_to: null, units: [{ unit_code: 'INPUT_TOKEN', unit_definition_id: unitID, unit_version: 1, minimum_precision: 1, price_denominator: 1000000, rounding_rule: 'CEILING', resolved_price: 3, configuration_type: 'FIXED_PRICE', price_source_scope: 'MODEL', source_price_version_id: '0199a200-0001-7000-8000-000000000208' }] }, }, next_change: null }); }
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}/impact/lifecycle`) { calls.impact += 1; return json(response, { blockers: [], evidence: { dependencies }, price_coverage: { models_checked: 1, models_covered: 1, fully_covered: true, versions: [{ model_id: modelID, protocol_code: 'CHAT', formula_version_id: formulaID, credit_unit: 'MICROCREDIT', covered: true, price_version_id: '0199a200-0001-7000-8000-000000000208', price_version: 2, effective_from: '2026-09-01T00:00:00Z' }], billing_observed_at: '2026-09-04T11:59:59Z', financial_fact_revision: 9, price_set_fingerprint: 'c'.repeat(64) }, token_observed_at: '2026-09-04T12:00:00Z', billing_observed_at: '2026-09-04T11:59:59Z' }); }
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
