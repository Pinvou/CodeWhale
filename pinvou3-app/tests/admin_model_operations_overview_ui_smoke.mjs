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
const actorID = '0199a700-0001-7000-8000-000000000001';
const modelID = '0199a700-0001-7000-8000-000000000002';
const protocolID = '0199a700-0001-7000-8000-000000000003';
const capabilityID = '0199a700-0001-7000-8000-000000000004';
const allPermissions = ['platform.models.read', 'platform.tokens.read', 'platform.usage.read'];
let gate = true;
let permissions = [...allPermissions];
let overviewMode = 'success';
const calls = { overview: 0, overviewAborted: 0, models: 0, usage: 0, writes: 0, windows: [] };

function json(response, payload, status = 200) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}

const model = { model_id: modelID, catalog_version: 7, display_name: 'Overview model', description: '', aliases: [], lifecycle_status: 'PUBLISHED', protocol_code: 'CHAT', protocol_version: 1, protocol_definition_id: protocolID, model_capability_version_id: capabilityID, capability_version: 1, context_length: 128000, maximum_output_tokens: 4096, supported_modalities: ['TEXT'], tokenizer_version: 'v1', source_available: true, source_missing_confirmations: 0, force_cancel_inflight: false };
const envelope = (items) => ({ items, next_cursor: '', has_more: false, limit: 50, as_of: '2026-09-08T12:00:00Z', snapshot_expires_at: '2026-09-08T12:15:00Z', request_id: 'overview-smoke' });
const metricKeys = ['published_model_count', 'deprecated_model_count', 'model_draft_count', 'stable_route_count', 'healthy_route_count', 'route_attention_count', 'provisioning_pending_count', 'provisioning_attention_count', 'usage_request_count', 'reconciliation_pending_count', 'reconciliation_stopped_count'];
function overview(url) {
  const window = url.searchParams.get('window');
  const days = window === '30d' ? 30 : window === '7d' ? 7 : 1;
  const bucketSeconds = days === 1 ? 3600 : 86400;
  const pointCount = days === 1 ? 24 : days;
  const to = '2026-09-08T12:00:00.000Z';
  const fromMillis = Date.parse(to) - days * 86_400_000;
  const from = new Date(fromMillis).toISOString();
  const points = Array.from({ length: pointCount }, (_, index) => ({
    from: new Date(fromMillis + index * bucketSeconds * 1000).toISOString(),
    to: new Date(fromMillis + (index + 1) * bucketSeconds * 1000).toISOString(),
    usage_request_count: overviewMode === 'empty' ? 0 : index === pointCount - 1 ? 8 : 0,
    reconciliation_attention_count: overviewMode === 'empty' ? 0 : index === pointCount - 1 ? 2 : 0,
  }));
  return {
    as_of: to, window: { from, to, timezone: 'UTC' },
    metrics: metricKeys.map((key, index) => ({ key, value: overviewMode === 'empty' ? 0 : index, unit: 'COUNT', aggregation: key === 'usage_request_count' ? 'WINDOW' : 'POINT_IN_TIME', included_statuses: ['KNOWN'], ...(['healthy_route_count', 'route_attention_count'].includes(key) ? { denominator_key: 'stable_route_count' } : {}) })),
    trend: { bucket_seconds: bucketSeconds, points: overviewMode === 'malformed' ? points.slice(0, 1) : points },
  };
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/configure') {
    if (url.searchParams.has('gate')) gate = url.searchParams.get('gate') === '1';
    if (url.searchParams.get('permissions') === 'all') permissions = [...allPermissions];
    if (url.searchParams.get('permissions') === 'models') permissions = ['platform.models.read'];
    if (url.searchParams.has('mode')) overviewMode = url.searchParams.get('mode');
    return json(response, { gate, permissions, overviewMode });
  }
  if (url.pathname === '/__test__/state') return json(response, calls);
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf', feature_flags: { admin_model_token_ia: true, admin_model_operations_overview: gate } });
  if (url.pathname === '/api/account/v1/account/bootstrap' || url.pathname === '/api/account/v1/me/authorization-context') return json(response, { user_id: actorID, platform_permissions: permissions });
  if (url.pathname === '/api/admin/token/admin/v1/models') { calls.models += 1; return json(response, envelope([model])); }
  if (url.pathname === '/api/admin/token/admin/v1/operations-overview') {
    if (request.method !== 'GET') { calls.writes += 1; return json(response, { code: 'WRITE_NOT_EXPECTED' }, 500); }
    calls.overview += 1;
    calls.windows.push({ window: url.searchParams.get('window'), from: url.searchParams.get('from'), to: url.searchParams.get('to') });
    if (overviewMode === 'failed') return json(response, { code: 'TOKEN_OPERATIONS_OVERVIEW_UNAVAILABLE', message: 'unavailable', request_id: 'overview-failed' }, 503);
    if (overviewMode === 'delayed') {
      let settled = false;
      const timer = setTimeout(() => { settled = true; json(response, overview(url)); }, 3000);
      const abort = () => { if (!settled) { settled = true; clearTimeout(timer); calls.overviewAborted += 1; } };
      request.once('aborted', abort);
      response.once('close', () => { if (!response.writableEnded) abort(); });
      return;
    }
    return json(response, overview(url));
  }
  if (url.pathname === '/api/admin/token/admin/v1/usage-requests') { calls.usage += 1; return json(response, envelope([])); }
  if (url.pathname === '/api/admin/token/admin/v1/provisionings' || url.pathname === '/api/admin/token/admin/v1/reconciliation-cases') return json(response, envelope([]));
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
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(String(error?.stack || error)));
  await page.setViewport({ width: 320, height: 720, deviceScaleFactor: 2 });
  await page.evaluateOnNewDocument(() => {
    localStorage.setItem('pinvou.admin.language', 'en');
    const NativeDate = Date;
    const skewedNow = NativeDate.parse('2099-01-01T00:00:00Z');
    globalThis.Date = class extends NativeDate {
      constructor(...args) { super(...(args.length ? args : [skewedNow])); }
      static now() { return skewedNow; }
      static parse(value) { return NativeDate.parse(value); }
      static UTC(...args) { return NativeDate.UTC(...args); }
    };
  });
  await page.goto(`${origin}/admin.html?section=models&model_tab=directory&model_operations_window=24h`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-model-operations-overview', { timeout: 10000 }).catch(async (error) => {
    throw new Error(`overview did not render; url=${page.url()} body=${(await page.$eval('body', (node) => node.textContent)).slice(0, 1200)} calls=${JSON.stringify(calls)} pageErrors=${JSON.stringify(pageErrors)}`, { cause: error });
  });
  assert.equal((await page.$eval('.adm-model-operations-overview', (node) => node.textContent)).includes('Catalog and runtime health'), true);
  assert.equal((await page.$eval('.adm-model-operations-overview', (node) => node.textContent)).includes('not cross-domain atomic with Billing'), true);
  assert.equal(await page.$eval('.adm-model-operations-overview', (node) => node.scrollWidth <= node.clientWidth + 1), true, 'overview must not overflow at 320px');
  assert.equal(calls.models, 1, 'overview must coexist with the independent paginated model source');
  assert.equal(calls.overview, 1);

  await page.select('.adm-model-overview-actions select', '7d');
  await page.waitForFunction(() => new URL(location.href).searchParams.get('model_operations_window') === '7d');
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((value) => value.overview >= 2));
  const sevenDay = calls.windows.at(-1);
  assert.deepEqual(sevenDay, { window: '7d', from: null, to: null }, 'browser clock skew must not enter the request');
  await page.waitForSelector('.adm-model-overview-groups');

  await page.evaluate(() => fetch('/__test__/configure?mode=empty'));
  await page.click('.adm-model-overview-actions button');
  await page.waitForSelector('.adm-model-trend-empty');
  assert.equal(await page.$('.adm-model-trend-chart'), null, 'an all-zero series must not render synthetic minimum bars');

  await page.evaluate(() => fetch('/__test__/configure?mode=malformed'));
  await page.click('.adm-model-overview-actions button');
  await page.waitForSelector('.adm-model-overview-state.error');
  await page.evaluate(() => fetch('/__test__/configure?mode=success'));
  await page.click('.adm-model-overview-state.error button');
  await page.waitForSelector('.adm-model-overview-groups');

  await page.evaluate(() => [...document.querySelectorAll('.adm-model-overview-groups button')].find((node) => node.textContent.includes('Usage requests in window')).click());
  await page.waitForFunction(() => new URL(location.href).searchParams.get('section') === 'modelOperations' && new URL(location.href).searchParams.get('operations_tab') === 'usage');
  const drilldown = new URL(page.url());
  assert.equal(drilldown.searchParams.get('model_operations_window'), '7d');
  assert.ok(drilldown.searchParams.get('usage_started_from'));
  assert.ok(drilldown.searchParams.get('usage_started_to'));

  await page.evaluate(() => fetch('/__test__/configure?mode=failed&gate=1&permissions=all'));
  await page.goto(`${origin}/admin.html?section=models&model_tab=directory`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-model-overview-state.error');
  assert.equal((await page.$eval('body', (node) => node.textContent)).includes('Overview model'), true, 'overview failure must not erase the model list');

  await page.evaluate(() => fetch('/__test__/configure?mode=delayed&gate=1&permissions=all'));
  const beforeDelayed = calls.overview;
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForFunction((before) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.overview > before), {}, beforeDelayed);
  await page.evaluate(() => fetch('/__test__/configure?permissions=models').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => !document.querySelector('.adm-model-operations-overview'));
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((value) => value.overviewAborted >= 1));
  assert.equal(new URL(page.url()).searchParams.has('model_operations_window'), false);
  const beforeRestore = calls.overview;
  await page.evaluate(() => fetch('/__test__/configure?permissions=all&mode=success').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForSelector('.adm-model-operations-overview');
  assert.equal(calls.overview, beforeRestore + 1);

  await page.evaluate(() => fetch('/__test__/configure?gate=0&permissions=all').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => !document.querySelector('.adm-model-operations-overview'));
  const beforeGateOffReload = calls.overview;
  await page.reload({ waitUntil: 'networkidle0' });
  assert.equal(await page.$('.adm-model-operations-overview'), null);
  assert.equal(calls.overview, beforeGateOffReload, 'gate-off reload must not request the overview');
  assert.equal(calls.writes, 0);
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}

console.log('admin model operations overview UI smoke: PASS');
