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
const actorID = '0199a740-0001-7000-8000-000000000001';
const modelID = '0199a740-0001-7000-8000-000000000002';
const tokenID = '0199a740-0001-7000-8000-000000000003';
let permissions = ['platform.models.read', 'platform.tokens.read', 'platform.usage.read'];
let authorizationVersion = 1;
let mode = 'success';
const calls = { models: 0, tokens: 0, operations: 0, legacy: 0, aborted: 0, operationQueries: [] };

const metric = (key, value, aggregation, statuses, denominator = null) => ({ key, value, unit: 'COUNT', aggregation, included_statuses: statuses, denominator_key: denominator });
const modelOverview = () => ({ schema_version: '1', as_of: '2026-09-11T12:00:00Z', metrics: [metric('published_model_count', 2, 'POINT_IN_TIME', ['PUBLISHED']), metric('deprecated_model_count', 1, 'POINT_IN_TIME', ['DEPRECATED']), metric('model_draft_count', 3, 'POINT_IN_TIME', ['DRAFT'])] });
const routeOverview = () => { const stable = mode === 'zero' ? 0 : 4; return { schema_version: '1', as_of: '2026-09-11T12:00:00Z', metrics: [metric('stable_route_count', stable, 'POINT_IN_TIME', ['RESOLVED']), metric('healthy_route_count', stable ? 3 : 0, 'POINT_IN_TIME', ['HEALTHY'], 'stable_route_count'), metric('route_attention_count', stable ? 1 : 0, 'POINT_IN_TIME', ['UNKNOWN', 'PROBING', 'DEGRADED', 'UNHEALTHY', 'STALE', 'EXPIRED_HEALTH'], 'stable_route_count')] }; };
function usageGroup(windowValue = '24h') {
  const days = windowValue === '30d' ? 30 : windowValue === '7d' ? 7 : 1;
  const seconds = days === 1 ? 3600 : 86400;
  const count = days === 1 ? 24 : days;
  const to = Date.parse('2026-09-11T12:00:00Z');
  const from = to - days * 86400e3;
  const points = Array.from({ length: count }, (_, index) => ({ from: new Date(from + index * seconds * 1000).toISOString(), to: new Date(from + (index + 1) * seconds * 1000).toISOString(), usage_request_count: index === 0 ? 5 : 0, reconciliation_attention_count: index === 1 ? 1 : 0 }));
  return { window: { from: new Date(from).toISOString(), to: new Date(to).toISOString(), timezone: 'UTC' }, metrics: [metric('usage_request_count', 5, 'WINDOW', ['CREATED', 'RESERVED', 'UPSTREAM_ACTIVE', 'SETTLEMENT_PENDING', 'COMMITTED', 'RELEASED', 'REVIEW']), metric('reconciliation_pending_count', 2, 'POINT_IN_TIME', ['SETTLEMENT_PENDING', 'REVIEW']), metric('reconciliation_stopped_count', 1, 'POINT_IN_TIME', ['SETTLEMENT_PENDING', 'REVIEW'])], trend: { bucket_seconds: seconds, points } };
}
const provisioning = { metrics: [metric('provisioning_pending_count', 2, 'POINT_IN_TIME', ['PENDING']), metric('provisioning_attention_count', 1, 'POINT_IN_TIME', ['FAILED', 'REPAIR_REQUIRED'])] };
const envelope = (items) => ({ items, next_cursor: '', has_more: false, limit: 50, as_of: '2026-09-11T12:00:00Z', snapshot_expires_at: '2026-09-11T12:15:00Z' });
const model = { model_id: modelID, catalog_version: 1, display_name: 'Page overview model', description: '', aliases: [], lifecycle_status: 'PUBLISHED', protocol_code: 'CHAT', protocol_version: 1, protocol_definition_id: modelID, model_capability_version_id: modelID, capability_version: 1, context_length: 1000, maximum_output_tokens: 100, supported_modalities: ['TEXT'], tokenizer_version: 'v1', source_available: true, source_missing_confirmations: 0, force_cancel_inflight: false };
const token = { token_account_id: tokenID, display_name: 'Page overview Token', owner_type: 'PLATFORM', owner_id: actorID, status: 'ACTIVE', health_status: 'HEALTHY', credential_status: 'VALID', version: 1, updated_at: '2026-09-11T12:00:00Z' };
const modelDetail = { as_of: '2026-09-11T12:00:00Z', model, protocol: null, formula: null };

function json(response, payload, status = 200, headers = {}) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', ...headers });
  response.end(JSON.stringify(payload));
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/configure') {
    const value = url.searchParams.get('permissions');
    if (value === 'models') permissions = ['platform.models.read'];
    if (value === 'tokens') permissions = ['platform.tokens.read'];
    if (value === 'usage') permissions = ['platform.usage.read'];
    if (value === 'both') permissions = ['platform.tokens.read', 'platform.usage.read'];
    if (value === 'all') permissions = ['platform.models.read', 'platform.tokens.read', 'platform.usage.read'];
    if (url.searchParams.has('mode')) mode = url.searchParams.get('mode');
    authorizationVersion += 1;
    return json(response, { permissions, mode, authorizationVersion });
  }
  if (url.pathname === '/__test__/state') return json(response, calls);
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf', feature_flags: { admin_model_token_ia: true, admin_model_details: true, admin_token_details: true, admin_model_operations_details: true, admin_model_operations_overview: true, admin_model_token_page_overviews: true, admin_model_token_actions: false } });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: actorID, platform_permissions: permissions, active_context: { platform_authorization_version: authorizationVersion } }, 200, { 'X-Platform-Authorization-Version': String(authorizationVersion) });
  if (url.pathname === '/api/admin/token/admin/v1/models') { calls.models += 1; return json(response, envelope([model])); }
  if (url.pathname === `/api/admin/token/admin/v1/models/${modelID}`) return json(response, modelDetail);
  if (url.pathname === '/api/admin/token/admin/v1/token-accounts') { calls.tokens += 1; return json(response, envelope([token])); }
  if (url.pathname === '/api/admin/token/admin/v1/provisionings' || url.pathname === '/api/admin/token/admin/v1/usage-requests' || url.pathname === '/api/admin/token/admin/v1/reconciliation-cases') return json(response, envelope([]));
  if (url.pathname === '/api/admin/token/admin/v1/operations-overview') { calls.legacy += 1; return json(response, {}, 500); }
  if (url.pathname === '/api/admin/token/admin/v1/overviews/model-catalog') {
    if (mode === 'forbidden') return json(response, { code: 'MODEL_CATALOG_OVERVIEW_FORBIDDEN', request_id: 'catalog-forbidden' }, 403);
    if (mode === 'failed') return json(response, { code: 'MODEL_CATALOG_OVERVIEW_UNAVAILABLE', request_id: 'catalog-failed' }, 503);
    if (mode === 'timeout') return json(response, { code: 'MODEL_CATALOG_OVERVIEW_TIMEOUT', request_id: 'catalog-timeout' }, 504);
    calls.models += 100;
    return json(response, modelOverview());
  }
  if (url.pathname === '/api/admin/token/admin/v1/overviews/token-resources') {
    calls.tokens += 100;
    return json(response, routeOverview());
  }
  if (url.pathname === '/api/admin/token/admin/v1/overviews/model-operations') {
    calls.operations += 1;
    calls.operationQueries.push(url.search);
    const tokenAllowed = permissions.includes('platform.tokens.read');
    const usageAllowed = permissions.includes('platform.usage.read');
    const payload = { schema_version: '1', as_of: '2026-09-11T12:00:00Z', permission_view: tokenAllowed && usageAllowed ? 'TOKEN_AND_USAGE' : tokenAllowed ? 'TOKEN_ONLY' : 'USAGE_ONLY', ...(tokenAllowed ? { provisioning } : {}), ...(usageAllowed ? { usage: usageGroup(url.searchParams.get('window') || '24h') } : {}) };
    if (mode === 'rate') return json(response, { code: 'TOKEN_OVERVIEW_RATE_LIMITED', request_id: 'rate-limited' }, 429, { 'Retry-After': '2' });
    if (mode === 'delayed') {
      let complete = false;
      const timer = setTimeout(() => { complete = true; json(response, payload); }, 2000);
      const abort = () => { if (!complete) { complete = true; clearTimeout(timer); calls.aborted += 1; } };
      request.once('aborted', abort); response.once('close', () => { if (!response.writableEnded) abort(); });
      return;
    }
    return json(response, payload);
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
  const errors = [];
  page.on('pageerror', (error) => errors.push(String(error?.stack || error)));
  await page.setViewport({ width: 320, height: 720, deviceScaleFactor: 2 });
  await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }]);
  await page.evaluateOnNewDocument(() => {
    if (!localStorage.getItem('pinvou.admin.language')) localStorage.setItem('pinvou.admin.language', 'en');
  });

  await page.goto(`${origin}/admin.html?section=models&model_tab=directory&model_q=prior&model_cursor=old`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-model-page-overview');
  assert.match(await page.$eval('.adm-model-page-overview h2', (node) => node.textContent), /Model catalog overview/);
  assert.equal(await page.$('.adm-model-overview-actions select'), null);
  assert.equal(await page.$eval('.adm-model-page-overview', (node) => node.scrollWidth <= node.clientWidth + 1), true);
  const nonInteractive = await page.$$eval('.adm-model-overview-metric', (nodes) => nodes.find((node) => node.textContent.includes('Drafts awaiting publish'))?.tagName);
  assert.equal(nonInteractive, 'DIV');
  for (const [language, title] of [['zh', '模型目录概览'], ['ja', 'モデルカタログ概要'], ['en', 'Model catalog overview']]) {
    await page.evaluate((value) => localStorage.setItem('pinvou.admin.language', value), language);
    await page.reload({ waitUntil: 'networkidle0' });
    await page.waitForSelector('.adm-model-page-overview');
    assert.equal(await page.$eval('.adm-model-page-overview h2', (node) => node.textContent), title);
  }
  await page.$eval('button.adm-model-overview-metric', (node) => node.focus());
  assert.match(await page.evaluate(() => document.activeElement?.textContent || ''), /Published models/);
  await page.keyboard.press('Enter');
  await page.waitForFunction(() => new URL(location.href).searchParams.get('model_lifecycle_status') === 'PUBLISHED');
  let current = new URL(page.url());
  assert.equal(current.searchParams.get('section'), 'models'); assert.equal(current.searchParams.has('model_q'), false); assert.equal(current.searchParams.has('model_cursor'), false);
  await page.goBack({ waitUntil: 'networkidle0' });
  current = new URL(page.url()); assert.equal(current.searchParams.get('model_q'), 'prior'); assert.equal(current.searchParams.get('model_cursor'), 'old');

  await page.goto(`${origin}/admin.html?section=models&model_tab=directory&model_id=${modelID}`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-model-detail');
  assert.equal(await page.$('.adm-model-page-overview'), null, 'model detail must unload the list overview');

  await page.evaluate(() => fetch('/__test__/configure?permissions=tokens&mode=zero'));
  await page.goto(`${origin}/admin.html?section=tokenResources`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-model-page-overview');
  assert.match(await page.$eval('.adm-model-page-overview', (node) => node.textContent), /Route health overview/);
  assert.match(await page.$eval('.adm-model-page-overview', (node) => node.textContent), /Health rate unavailable/);
  assert.equal(await page.$$eval('.adm-model-overview-metric', (nodes) => nodes.filter((node) => node.textContent.includes('Stable routes') && node.tagName === 'DIV').length), 1);

  await page.evaluate(() => fetch('/__test__/configure?permissions=tokens&mode=success'));
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForSelector('button.adm-model-overview-metric');
  await page.evaluate(() => [...document.querySelectorAll('button.adm-model-overview-metric')].find((node) => node.textContent.includes('Healthy routes')).click());
  await page.waitForFunction(() => document.querySelector('.adm-overview-related-result'));
  assert.equal(new URL(page.url()).searchParams.get('section'), 'tokenResources');
  assert.equal(new URL(page.url()).searchParams.get('token_health_status'), 'HEALTHY');

  await page.evaluate(() => fetch('/__test__/configure?permissions=tokens&mode=success'));
  await page.goto(`${origin}/admin.html?section=modelOperations&operations_tab=provisionings&model_operations_window=30d`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-model-page-overview');
  assert.equal(await page.$('.adm-model-overview-actions select'), null);
  assert.equal(new URL(page.url()).searchParams.has('model_operations_window'), false);
  assert.equal(calls.operationQueries.at(-1), '', 'token-only request must omit window');

  await page.evaluate(() => fetch('/__test__/configure?permissions=usage&mode=success'));
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-model-overview-actions select');
  await page.select('.adm-model-overview-actions select', '7d');
  await page.waitForFunction(() => new URL(location.href).searchParams.get('model_operations_window') === '7d');
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.operationQueries.at(-1) === '?window=7d'));
  assert.equal(await page.$$eval('.adm-model-overview-groups h3', (nodes) => nodes.some((node) => node.textContent === 'Provisioning')), false);

  await page.evaluate(() => fetch('/__test__/configure?permissions=all&mode=success'));
  await page.goto(`${origin}/admin.html?section=modelOperations&operations_tab=provisionings&model_operations_window=24h&provisioning_q=prior&provisioning_status=FAILED&provisioning_model_id=${modelID}&provisioning_token_account_id=${tokenID}&provisioning_scope_type=ORG&provisioning_scope_id=${actorID}&provisioning_sla_overdue=true&provisioning_cursor=old-page&usage_q=preserved`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-model-page-overview .adm-model-overview-trend');
  assert.deepEqual(await page.$$eval('.adm-model-overview-groups h3', (nodes) => nodes.map((node) => node.textContent)), ['Provisioning', 'Usage and reconciliation']);
  assert.equal(await page.$$eval('.adm-model-page-overview .adm-model-overview-metric', (nodes) => nodes.length), 5, 'TOKEN_AND_USAGE must mount all five metric cards');
  const cdp = await page.createCDPSession();
  await cdp.send('Emulation.setPageScaleFactor', { pageScaleFactor: 2 });
  await page.waitForFunction(() => window.visualViewport?.scale === 2);
  const accessibility = await page.evaluate(() => {
    const overview = document.querySelector('.adm-model-page-overview');
    const refresh = overview?.querySelector('.adm-model-overview-actions button');
    const windowSelect = overview?.querySelector('.adm-model-overview-actions select');
    const metric = [...overview.querySelectorAll('button.adm-model-overview-metric')].find((node) => node.textContent.includes('Pending Provisioning'));
    const trend = overview?.querySelector('.adm-model-trend-bucket>button');
    refresh?.scrollIntoView({ block: 'center', inline: 'nearest' });
    refresh?.focus();
    const dimensions = node => { const box = node.getBoundingClientRect(); return { width: box.width, height: box.height }; };
    return {
      mounted: Boolean(overview?.isConnected),
      visualScale: window.visualViewport?.scale,
      overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      columns: getComputedStyle(overview.querySelector('.adm-model-overview-groups>article>div')).gridTemplateColumns.split(' ').filter(Boolean).length,
      refresh: dimensions(refresh),
      windowSelect: dimensions(windowSelect),
      metric: dimensions(metric),
      trend: dimensions(trend),
      focused: document.activeElement === refresh,
      animationDuration: getComputedStyle(document.querySelector('.adm-page')).animationDuration,
      transitionDuration: getComputedStyle(metric).transitionDuration,
    };
  });
  assert.equal(accessibility.mounted, true);
  assert.equal(accessibility.visualScale, 2);
  assert.ok(accessibility.overflow <= 1, `200% layout overflowed by ${accessibility.overflow}px`);
  assert.equal(accessibility.columns, 1, '760px rule must override the three-column page overview rule');
  for (const [control, box] of Object.entries({ refresh: accessibility.refresh, windowSelect: accessibility.windowSelect, metric: accessibility.metric, trend: accessibility.trend })) {
    assert.ok(box.width >= 44 && box.height >= 44, `${control} must compute to at least 44x44 CSS pixels`);
  }
  assert.equal(accessibility.focused, true, 'refresh remains keyboard reachable at 200% while the overview is mounted');
  assert.ok(Number.parseFloat(accessibility.animationDuration) <= 0.00001, `reduced-motion animation remained ${accessibility.animationDuration}`);
  assert.ok(Number.parseFloat(accessibility.transitionDuration) <= 0.00001, `reduced-motion transition remained ${accessibility.transitionDuration}`);
  await cdp.send('Emulation.setPageScaleFactor', { pageScaleFactor: 1 });

  await page.evaluate(() => {
    const originalPushState = history.pushState.bind(history);
    window.__overviewPushStateCalls = 0;
    history.pushState = (...args) => {
      window.__overviewPushStateCalls += 1;
      return originalPushState(...args);
    };
  });
  const exactPushCalls = await page.evaluate(() => window.__overviewPushStateCalls);
  await page.evaluate(() => [...document.querySelectorAll('button.adm-model-overview-metric')].find((node) => node.textContent.includes('Pending Provisioning')).click());
  await page.waitForFunction(() => new URL(location.href).searchParams.get('provisioning_status') === 'PENDING');
  current = new URL(page.url());
  assert.equal(current.searchParams.get('section'), 'modelOperations');
  assert.equal(current.searchParams.get('operations_tab'), 'provisionings');
  assert.equal(current.searchParams.get('provisioning_status'), 'PENDING');
  for (const key of ['provisioning_q', 'provisioning_model_id', 'provisioning_token_account_id', 'provisioning_scope_type', 'provisioning_scope_id', 'provisioning_sla_overdue', 'provisioning_cursor', 'operations_overview_result']) assert.equal(current.searchParams.has(key), false, `EXACT must clear ${key}`);
  assert.equal(current.searchParams.get('usage_q'), 'preserved', 'EXACT replaces only its target source');
  assert.equal(await page.evaluate(() => window.__overviewPushStateCalls), exactPushCalls + 1, 'EXACT drilldown writes exactly one pushState');
  await page.goBack({ waitUntil: 'networkidle0' });
  current = new URL(page.url());
  assert.equal(current.searchParams.get('provisioning_q'), 'prior');
  assert.equal(current.searchParams.get('provisioning_status'), 'FAILED');
  assert.equal(current.searchParams.get('provisioning_cursor'), 'old-page');

  const relatedPushCalls = await page.evaluate(() => window.__overviewPushStateCalls);
  await page.evaluate(() => [...document.querySelectorAll('button.adm-model-overview-metric')].find((node) => node.textContent.includes('Provisioning issues')).click());
  await page.waitForFunction(() => new URL(location.href).searchParams.get('operations_overview_result') === 'provisioning_attention_count');
  current = new URL(page.url());
  assert.equal(current.searchParams.get('section'), 'modelOperations');
  assert.equal(current.searchParams.get('operations_tab'), 'provisionings');
  assert.equal(current.searchParams.has('provisioning_status'), false);
  assert.equal(current.searchParams.has('provisioning_cursor'), false);
  assert.equal(await page.evaluate(() => window.__overviewPushStateCalls), relatedPushCalls + 1, 'RELATED drilldown writes exactly one pushState');
  const relatedURL = page.url();
  const tabPushCalls = await page.evaluate(() => window.__overviewPushStateCalls);
  await page.click('#model-operations-tab-usage');
  await page.waitForFunction(() => new URL(location.href).searchParams.get('operations_tab') === 'usage');
  assert.equal(new URL(page.url()).searchParams.has('operations_overview_result'), false, 'manual tab switch must clear an incompatible RELATED marker');
  assert.equal(await page.evaluate(() => window.__overviewPushStateCalls), tabPushCalls + 1, 'manual tab switch writes exactly one pushState');
  await page.goBack({ waitUntil: 'networkidle0' });
  assert.equal(page.url(), relatedURL, 'back restores the RELATED marker and its matching tab');
  assert.equal(await page.evaluate(() => window.__overviewPushStateCalls), tabPushCalls + 1, 'popstate must not write another history entry');
  assert.ok(await page.$('.adm-overview-related-result'));

  await page.evaluate(() => fetch('/__test__/configure?permissions=usage&mode=rate'));
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-model-overview-state.error');
  assert.match(await page.$eval('.adm-model-overview-state.error', (node) => node.textContent), /Retry in 2s/);
  assert.equal(await page.$eval('.adm-model-overview-state.error button', (node) => node.disabled), true);

  for (const [failureMode, expectedCode] of [['forbidden', 'MODEL_CATALOG_OVERVIEW_FORBIDDEN'], ['failed', 'MODEL_CATALOG_OVERVIEW_UNAVAILABLE'], ['timeout', 'MODEL_CATALOG_OVERVIEW_TIMEOUT']]) {
    await page.evaluate((value) => fetch(`/__test__/configure?permissions=all&mode=${value}`), failureMode);
    await page.goto(`${origin}/admin.html?section=models&model_tab=directory`, { waitUntil: 'networkidle0' });
    await page.waitForSelector('.adm-model-overview-state.error');
    assert.match(await page.$eval('.adm-model-overview-state.error', (node) => node.textContent), new RegExp(expectedCode));
    assert.equal((await page.$eval('body', (node) => node.textContent)).includes('Page overview model'), true);
  }

  await page.evaluate(() => fetch('/__test__/configure?permissions=usage&mode=delayed'));
  await page.goto(`${origin}/admin.html?section=modelOperations&operations_tab=usage`, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.operations > 0));
  await page.evaluate(() => fetch('/__test__/configure?permissions=models&mode=success').then(() => dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => !document.querySelector('.adm-model-page-overview'));
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.aborted > 0));

  assert.equal(calls.legacy, 0, 'new page overview gate must never call the legacy aggregate');
  assert.deepEqual(errors, []);
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}

console.log('admin model and Token page overviews UI smoke: PASS');
