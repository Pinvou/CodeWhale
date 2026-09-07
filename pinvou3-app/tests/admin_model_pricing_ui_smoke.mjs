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
const actorID = '0199a500-0001-7000-8000-000000000101';
const modelID = '0199a500-0001-7000-8000-000000000102';
const protocolID = '0199a500-0001-7000-8000-000000000103';
const formulaID = '0199a500-0001-7000-8000-000000000104';
const unitID = '0199a500-0001-7000-8000-000000000105';
const priceID = '0199a500-0001-7000-8000-000000000106';
const globalPriceID = '0199a500-0001-7000-8000-000000000108';
let gate = true;
let iaGate = true;
let permissions = ['platform.models.read', 'platform.pricing.read', 'platform.pricing.manage'];
let delayRead = '';
let projectionMode = 'available';
const calls = { globalPrices: 0, exactPrices: 0, exactAborted: 0, previewReads: 0, previewAborted: 0, transitionAborted: 0, recoveryAborted: 0, protocols: 0, catalogs: 0, modelDetails: 0, writes: 0 };
const model = { model_id: modelID, catalog_version: 7, display_name: 'Shared-price model', description: 'Billing-owned price context', aliases: [], lifecycle_status: 'PUBLISHED', protocol_code: 'CHAT', protocol_version: 2, protocol_definition_id: protocolID, model_capability_version_id: '0199a500-0001-7000-8000-000000000107', capability_version: 3, context_length: 128000, maximum_output_tokens: 8192, supported_modalities: ['TEXT'], tokenizer_version: 'v2', source_available: true, source_missing_confirmations: 0, force_cancel_inflight: false };
const formula = { formula_version_id: formulaID, protocol_definition_id: protocolID, protocol_code: 'CHAT', version: 4, status: 'PUBLISHED', required_unit_versions: { INPUT_TOKEN: unitID }, evaluator_version: 'SAFE_V1', etag: 'b'.repeat(64), created_at: '2026-09-02T00:00:00Z', published_at: '2026-09-03T00:00:00Z' };
const detail = { as_of: '2026-09-05T01:00:00Z', model, protocol: { protocol_definition_id: protocolID, protocol_code: 'CHAT', version: 2, status: 'PUBLISHED', schema_field_count: 4, etag: 'a'.repeat(64), created_at: '2026-09-01T00:00:00Z', published_at: '2026-09-02T00:00:00Z' }, formula };
const managedPrice = { price_version_id: priceID, scope_type: 'MODEL', scope_id: modelID, protocol_code: 'CHAT', formula_version_id: formulaID, credit_unit: 'MICROCREDIT', lifecycle_status: 'PUBLISHED', derived_status: 'ACTIVE', version: 2, space_etag: 7, current_space_etag: 7, effective_from: '2026-09-01T00:00:00+08:00', published_at: '2026-08-31T12:00:00Z', units: [{ unit_definition_id: unitID, config_type: 'INHERIT_GLOBAL' }] };
const meteringUnit = { unit_definition_id: unitID, unit_code: 'INPUT_TOKEN', version: 3, minimum_precision: 1, price_denominator: 1000000, rounding_rule: 'CEILING', status: 'ACTIVE', created_at: '2026-08-01T00:00:00Z' };
const projection = { model_id: modelID, protocol_code: 'CHAT', formula_version_id: formulaID, credit_unit: 'MICROCREDIT', as_of: '2026-09-05T01:00:00Z', source: 'BILLING', snapshot_semantics: 'BILLING_REPEATABLE_READ_ONLY; TOKEN_CATALOG_NOT_READ; NO_CROSS_DOMAIN_ATOMICITY', current: { effective_at: '2026-09-05T01:00:00Z', coverage_status: 'AVAILABLE', price: { price_version_id: priceID, scope_type: 'MODEL', space_etag: 7, effective_from: '2026-09-01T00:00:00Z', effective_to: null, units: [{ unit_code: 'INPUT_TOKEN', unit_definition_id: unitID, unit_version: 3, minimum_precision: 1, price_denominator: 1000000, rounding_rule: 'CEILING', resolved_price: 5, configuration_type: 'INHERIT_GLOBAL', price_source_scope: 'GLOBAL', source_price_version_id: globalPriceID }] } }, next_change: { effective_at: '2026-10-01T00:00:00Z', coverage_status: 'MISSING' } };

function json(response, payload, status = 200) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/configure') {
    if (url.searchParams.has('gate')) gate = url.searchParams.get('gate') === '1';
    if (url.searchParams.has('ia')) iaGate = url.searchParams.get('ia') === '1';
    if (url.searchParams.has('manage')) permissions = url.searchParams.get('manage') === '1' ? ['platform.models.read', 'platform.pricing.read', 'platform.pricing.manage'] : ['platform.models.read', 'platform.pricing.read'];
    if (url.searchParams.has('delay')) delayRead = url.searchParams.get('delay');
    if (url.searchParams.has('projection')) projectionMode = url.searchParams.get('projection');
    return json(response, { gate, iaGate, permissions, delayRead, projectionMode });
  }
  if (url.pathname === '/__test__/state') return json(response, calls);
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf', feature_flags: { admin_model_token_ia: iaGate, admin_model_details: true, admin_model_pricing_edit: gate } });
  if (url.pathname === '/api/account/v1/account/bootstrap' || url.pathname === '/api/account/v1/me/authorization-context') return json(response, { user_id: actorID, platform_permissions: permissions });
  if (url.pathname.endsWith('/finance-overviews/pricing')) return json(response, { as_of: '2026-09-05T01:00:00Z', window: null, metrics: [] });
  if (url.pathname === '/api/admin/billing/admin/v1/pricing') {
    if (request.method !== 'GET') { calls.writes += 1; return json(response, { code: 'WRITE_NOT_EXPECTED' }, 500); }
    if (url.searchParams.get('scope_type') === 'MODEL') {
      calls.exactPrices += 1;
      assert.equal(url.searchParams.get('scope_id'), modelID);
      assert.equal(url.searchParams.get('protocol_code'), 'CHAT');
      assert.equal(url.searchParams.get('formula_version_id'), formulaID);
      if (delayRead) {
        const readKind = delayRead;
        let settled = false;
        const timer = setTimeout(() => { settled = true; json(response, { current_space_etag: 7, items: [managedPrice], has_more: false }); }, 1200);
        const aborted = () => { if (!settled) { settled = true; clearTimeout(timer); calls.exactAborted += 1; if (readKind === 'transition') calls.transitionAborted += 1; if (readKind === 'recovery') calls.recoveryAborted += 1; } };
        request.once('aborted', aborted);
        response.once('close', () => { if (!response.writableEnded) aborted(); });
        return;
      }
      return json(response, { current_space_etag: 7, items: [managedPrice], has_more: false });
    }
    calls.globalPrices += 1;
    return json(response, { items: [managedPrice] });
  }
  if (url.pathname === `/api/admin/billing/admin/v1/pricing/${priceID}/preview`) {
    calls.previewReads += 1;
    if (delayRead === 'preview') {
      let settled = false;
      const timer = setTimeout(() => { settled = true; json(response, { price_version_id: priceID, scope_type: 'MODEL', model: modelID, protocol_code: 'CHAT', formula_version_id: formulaID, resolved_at: '2026-09-05T01:00:00Z', units: [] }); }, 1200);
      const aborted = () => { if (!settled) { settled = true; clearTimeout(timer); calls.previewAborted += 1; } };
      request.once('aborted', aborted);
      response.once('close', () => { if (!response.writableEnded) aborted(); });
      return;
    }
    return json(response, { price_version_id: priceID, scope_type: 'MODEL', model: modelID, protocol_code: 'CHAT', formula_version_id: formulaID, resolved_at: '2026-09-05T01:00:00Z', units: [] });
  }
  if (url.pathname === '/api/admin/billing/admin/v1/metering-units') return json(response, { items: [meteringUnit] });
  if (url.pathname === '/api/admin/token/admin/v1/protocols') { calls.protocols += 1; return json(response, { protocols: [], formulas: [formula] }); }
  if (url.pathname === '/api/admin/token/admin/v1/models') { calls.catalogs += 1; return json(response, { items: [model], next_cursor: '', has_more: false, limit: 50, as_of: '2026-09-05T01:00:00Z', snapshot_expires_at: '2026-09-05T01:15:00Z', request_id: 'pricing-models' }); }
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}`) { calls.modelDetails += 1; return json(response, detail); }
  if (url.pathname === `/api/admin/billing/admin/v1/models/${modelID}/price-projection`) {
    if (projectionMode === 'forbidden') return json(response, { code: 'FORBIDDEN', message: 'private billing detail must not be rendered' }, 403);
    if (projectionMode === 'missing') return json(response, { ...projection, current: { effective_at: projection.current.effective_at, coverage_status: 'MISSING' }, next_change: null });
    return json(response, projection);
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
  await page.goto(`${origin}/admin.html?section=products&finance_tab=prices`, { waitUntil: 'networkidle0' });
  await page.waitForFunction((id) => document.body.textContent.includes('Shared-price model') || document.body.textContent.includes(id), {}, modelID);
  assert.equal(calls.globalPrices, 1);
  assert.equal(calls.protocols, 1);
  assert.equal(calls.catalogs, 1);
  await page.click('.adm-pricing-workspace .adm-name-link');
  await page.waitForFunction((id) => new URL(location.href).searchParams.get('model_id') === id && document.body.textContent.includes('Independent Billing read snapshot'), {}, modelID);
  assert.equal(new URL(page.url()).searchParams.get('model_price_return'), 'products');
  const projectionText = await page.$eval('#model-pricing-heading', (heading) => heading.closest('section').textContent);
  for (const text of ['Current prices and metering units', 'INPUT_TOKEN · v3', '5 / 1000000', 'GLOBAL', 'Future prices and metering units', 'Pricing-space ETag7']) assert.equal(projectionText.replaceAll(/\s/g, '').includes(text.replaceAll(/\s/g, '')), true, `missing authoritative projection fact: ${text}`);
  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Manage model prices').click());
  await page.waitForFunction(() => document.body.textContent.includes('Shared pricing lifecycle workspace') && document.body.textContent.includes('No model-specific price versions') === false);
  assert.equal(calls.exactPrices, 1);
  assert.equal(calls.protocols, 1, 'model context must not load a second Token formula directory');
  assert.equal(calls.catalogs, 1, 'model context must not scan the model directory');
  const editorText = await page.$eval('.adm-model-pricing-editor', (node) => node.textContent);
  assert.equal(editorText.includes('v2'), true);
  assert.equal(editorText.includes('ETag'), true);
  assert.equal(editorText.includes('7'), true, 'the shared editor must expose the same authoritative pricing-space ETag');
  await page.click('.adm-model-detail-header .adm-back');
  await page.waitForFunction(() => new URL(location.href).searchParams.get('section') === 'products' && new URL(location.href).searchParams.get('finance_tab') === 'prices');
  await page.evaluate(() => fetch('/__test__/configure?ia=0').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => !document.querySelector('.adm-pricing-workspace .adm-name-link'));
  const beforeHiddenNavigation = calls.modelDetails;
  assert.equal((await page.$eval('.adm-pricing-workspace tbody', (node) => node.textContent)).includes(modelID), true, 'the stable model ID remains readable without becoming an inert button');
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.equal(new URL(page.url()).searchParams.get('section'), 'products');
  assert.equal(calls.modelDetails, beforeHiddenNavigation, 'parent IA off must not issue a model-detail request');
  await page.evaluate(() => fetch('/__test__/configure?ia=1').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForSelector('.adm-pricing-workspace .adm-name-link');

  await page.goto(`${origin}/admin.html?section=models&model_id=${modelID}&model_detail_tab=pricing&model_price_manage=1`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-model-pricing-editor');
  await page.evaluate(() => fetch('/__test__/configure?gate=1&manage=1&delay=list'));
  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Refresh' && button.closest('.adm-pricing-workspace')).click());
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.exactPrices >= 3));
  await page.evaluate(() => fetch('/__test__/configure?gate=0&manage=1').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => !new URL(location.href).searchParams.has('model_price_manage') && !document.querySelector('.adm-model-pricing-editor'));
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.exactAborted >= 1));
  assert.equal(calls.writes, 0);

  await page.evaluate(() => fetch('/__test__/configure?gate=1&manage=1&delay=').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => document.body.textContent.includes('Manage model prices'));
  assert.equal(await page.$('.adm-model-pricing-editor'), null, 'restoring the gate must not resurrect a dismissed editor');
  await page.evaluate(() => fetch('/__test__/configure?gate=1&manage=0').then(() => dispatchEvent(new Event('focus'))));
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.equal(new URL(page.url()).searchParams.has('model_price_manage'), false);
  assert.equal(await page.$('.adm-model-pricing-editor'), null);
  assert.equal(calls.writes, 0);

  // Every transient Billing read is aborted when the contextual editor loses its gate or permission.
  await page.evaluate(() => fetch('/__test__/configure?gate=1&manage=1&delay=').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => document.body.textContent.includes('Manage model prices'));
  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Manage model prices').click());
  await page.waitForSelector('.adm-model-pricing-editor');
  await page.evaluate(() => fetch('/__test__/configure?delay=preview'));
  await page.evaluate(() => [...document.querySelectorAll('.adm-model-pricing-editor button')].find((button) => button.textContent.trim() === 'Resolve preview').click());
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.previewReads >= 1));
  await page.evaluate(() => fetch('/__test__/configure?gate=0&manage=1').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.previewAborted >= 1));
  assert.equal(await page.$('.adm-pricing-preview'), null, 'an aborted preview must not resurrect after gate removal');

  await page.evaluate(() => fetch('/__test__/configure?gate=1&manage=1&delay=').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => document.body.textContent.includes('Manage model prices'));
  await page.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Manage model prices').click());
  await page.waitForSelector('.adm-model-pricing-editor');
  await page.evaluate(() => fetch('/__test__/configure?delay=transition'));
  await page.evaluate(() => [...document.querySelectorAll('.adm-model-pricing-editor button')].find((button) => button.textContent.trim() === 'End active version').click());
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.exactPrices >= 5));
  await page.evaluate(() => fetch('/__test__/configure?gate=1&manage=0').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.transitionAborted >= 1));
  assert.equal(await page.$('.adm-pricing-action'), null, 'an aborted continuation lookup must not open a transition dialog');

  await page.evaluate((unknown) => {
    // eslint-disable-next-line n/no-unsupported-features/node-builtins -- This callback runs in Chromium, not in Node.
    window.sessionStorage.setItem('pinvou.admin.pricing-unknown-mutation', JSON.stringify(unknown));
  }, { phase: 'DISPATCHED', kind: 'DRAFT_CREATE', input: { scope_type: 'MODEL', scope_id: modelID, protocol_code: 'CHAT', formula_version_id: formulaID, credit_unit: 'MICROCREDIT' }, beforeIDs: [priceID] });
  await page.evaluate(() => fetch('/__test__/configure?gate=1&manage=1&delay='));
  await page.goto(`${origin}/admin.html?section=models&model_id=${modelID}&model_detail_tab=pricing&model_price_manage=1`, { waitUntil: 'networkidle0' });
  await page.waitForFunction(() => document.body.textContent.includes('Verify authoritative state'));
  await page.evaluate(() => fetch('/__test__/configure?delay=recovery'));
  await page.evaluate(() => [...document.querySelectorAll('.adm-model-pricing-editor button')].find((button) => button.textContent.trim() === 'Verify authoritative state').click());
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.exactPrices >= 7));
  await page.evaluate(() => fetch('/__test__/configure?gate=0&manage=1').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.recoveryAborted >= 1));
  assert.equal(await page.$('.adm-model-pricing-editor'), null);
  assert.equal(calls.writes, 0, 'aborted transient reads must never dispatch a mutation');
  // eslint-disable-next-line n/no-unsupported-features/node-builtins -- This callback runs in Chromium, not in Node.
  await page.evaluate(() => window.sessionStorage.removeItem('pinvou.admin.pricing-unknown-mutation'));

  await page.evaluate(() => fetch('/__test__/configure?projection=forbidden'));
  await page.evaluate(() => document.querySelector('#model-pricing-heading').closest('.adm-source-head').querySelector('button').click());
  await page.waitForFunction(() => document.querySelector('#model-pricing-heading').closest('section').querySelector('.adm-source-error'));
  const forbiddenText = await page.$eval('#model-pricing-heading', (heading) => heading.closest('section').textContent);
  assert.equal(forbiddenText.includes('private billing detail'), false);
  assert.equal(forbiddenText.includes('5 / 1000000'), false, 'a failed current query cannot retain a stale price as current');
  await page.evaluate(() => fetch('/__test__/configure?projection=missing'));
  await page.evaluate(() => document.querySelector('#model-pricing-heading').closest('section').querySelector('.adm-source-error button').click());
  await page.waitForFunction(() => document.querySelector('#model-pricing-heading').closest('section').textContent.includes('No price can currently be resolved'));
  const missingText = await page.$eval('#model-pricing-heading', (heading) => heading.closest('section').textContent);
  assert.equal(missingText.includes('0 /'), false, 'missing prices must not be rendered as zero');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
