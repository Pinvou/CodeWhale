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
const observations = { createOptionsReads: 0, catalogQueries: [], createPosts: 0, actionPosts: 0, draftUpdates: [], replacementCreates: [] };
let permissions = ['platform.products.read', 'platform.products.create', 'platform.products.publish', 'platform.products.unpublish', 'platform.products.delete'];
let authorizationDelay = 0;
let createOptionsFailure = false;
let managementFailure = false;
let createMode = 'success';
const products = [
  { catalog_item_id: '0199a100-0001-7000-8000-000000000101', product_id: 'ON_SALE_100', display_name_zh_cn: '在售一百积分', display_name_en: 'On-sale 100 points', display_name_ja: '販売中100ポイント', display_order: 1, base_points: 100, bonus_points: 0, validity_days: 30, invoice_product_name: '信息服务费', lifecycle_status: 'ON_SALE', version: 3, admission_version: 2, ever_published: true, mapping: { draft_version: 2, provider_id: 'test-provider', provider_version: 'v1', sku_bom: 'ON-100', amount_fen: 100, currency: 'CNY', channels: ['ALIPAY'], admission_class: 'FORMAL' } },
  { catalog_item_id: '0199a100-0001-7000-8000-000000000102', product_id: 'DRAFT_200', display_name_zh_cn: '草稿二百积分', display_name_en: 'Draft 200 points', display_name_ja: '下書き200ポイント', display_order: 2, base_points: 180, bonus_points: 20, validity_days: 30, invoice_product_name: '信息服务费', lifecycle_status: 'DRAFT', version: 1, admission_version: 1, ever_published: false, mapping: { draft_version: 1, provider_id: 'test-provider', provider_version: 'v1', sku_bom: 'DRAFT-200', amount_fen: 200, currency: 'CNY', channels: ['ALIPAY'], admission_class: 'TEST_ONLY' } },
  { catalog_item_id: '0199a100-0001-7000-8000-000000000103', product_id: 'OFF_SALE_300', display_name_zh_cn: '下架三百积分', display_name_en: 'Off-sale 300 points', display_name_ja: '非公開300ポイント', display_order: 3, base_points: 300, bonus_points: 0, validity_days: 30, invoice_product_name: '信息服务费', lifecycle_status: 'OFF_SALE', version: 4, admission_version: 2, ever_published: true, mapping: { draft_version: 2, provider_id: 'test-provider', provider_version: 'v1', sku_bom: 'OFF-300', amount_fen: 300, currency: 'CNY', channels: ['ALIPAY'], admission_class: 'FORMAL' } },
];

function json(response, payload, status = 200) {
  response.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/configure') {
    const mode = url.searchParams.get('mode');
    if (mode === 'concurrent-draft') {
      const draft = products.find((item) => item.product_id === 'DRAFT_200');
      draft.version += 1;
      draft.mapping = { ...draft.mapping, draft_version: draft.mapping.draft_version + 1 };
    }
    if (mode === 'readonly') {
      permissions = ['platform.products.read'];
      authorizationDelay = 250;
    } else if (mode === 'no-read') {
      permissions = [];
      authorizationDelay = 250;
    } else {
      permissions = ['platform.products.read', 'platform.products.create', 'platform.products.publish', 'platform.products.unpublish', 'platform.products.delete'];
      authorizationDelay = 0;
    }
    createOptionsFailure = mode === 'create-options-failure';
    managementFailure = mode === 'management-failure';
    createMode = ['conflict', 'unknown', 'stepup'].includes(mode) ? mode : 'success';
    return json(response, { mode });
  }
  if (url.pathname === '/test.html') {
    response.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    return response.end('<!doctype html><html><head><meta name="viewport" content="width=device-width"><link rel="stylesheet" href="/assets/product-admin.css"></head><body><main class="adm-extension-stack"><div id="product-admin-root"></div></main><script type="module" src="/assets/product-admin.js"></script></body></html>');
  }
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf-test' });
  if (url.pathname === '/api/account/v1/me/authorization-context') {
    if (authorizationDelay) await new Promise((resolve) => setTimeout(resolve, authorizationDelay));
    return json(response, { platform_permissions: permissions });
  }
  if (url.pathname === '/api/admin/billing/admin/v1/recharge-product-create-options') {
    observations.createOptionsReads += 1;
    if (createOptionsFailure) return json(response, { code: 'CREATE_OPTIONS_UNAVAILABLE', message: 'create options unavailable' }, 503);
    return json(response, { adapters: [{ provider_id: 'test-provider', provider_version: 'v1', channels: ['ALIPAY'] }], invoice_product_names: ['信息服务费'] });
  }
  if (url.pathname === '/api/admin/billing/admin/v1/recharge-products') {
    observations.catalogQueries.push(url.search);
    if (request.method === 'POST') {
      observations.createPosts += 1;
      if (createMode === 'conflict') return json(response, { code: 'BILLING_CONFLICT', message: 'conflict' }, 409);
      if (createMode === 'unknown') return json(response, { code: 'BILLING_UNAVAILABLE', message: 'unavailable' }, 503);
      if (createMode === 'stepup') return json(response, { code: 'RECENT_AUTHENTICATION_REQUIRED', message: 'reauthenticate' }, 401);
      const chunks = [];
      for await (const chunk of request) chunks.push(chunk);
      const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
      if (body.replacement_product_id) observations.replacementCreates.push(body);
      const created = { catalog_item_id: `0199a100-0001-7000-8000-${String(products.length + 100).padStart(12, '0')}`, product_id: body.product_id, display_name_zh_cn: body.display_name_zh_cn, display_name_en: body.display_name_en, display_name_ja: body.display_name_ja, display_order: body.display_order, base_points: body.base_points, bonus_points: body.bonus_points, validity_days: body.validity_days, invoice_product_name: body.invoice_product_name, replacement_product_id: body.replacement_product_id, lifecycle_status: 'DRAFT', version: 1, admission_version: 0, ever_published: false, mapping: { draft_version: 1, ...body.mapping, external_sku_bom: body.mapping.sku_bom } };
      products.push(created);
      return json(response, created, 201);
    }
    const status = url.searchParams.get('lifecycle_status');
    if (managementFailure && !status) return json(response, { code: 'BILLING_UNAVAILABLE', message: 'catalog unavailable' }, 503);
    return json(response, { items: status ? products.filter((item) => item.lifecycle_status === status) : products, next_cursor: '' });
  }
  const draftUpdate = url.pathname.match(/^\/api\/admin\/billing\/admin\/v1\/recharge-products\/([^/]+)\/draft$/);
  if (draftUpdate && request.method === 'PUT') {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    observations.draftUpdates.push(body);
    if (createMode === 'stepup') return json(response, { code: 'RECENT_AUTHENTICATION_REQUIRED', message: 'reauthenticate' }, 401);
    const item = products.find((value) => value.catalog_item_id === draftUpdate[1]);
    if (!item || item.lifecycle_status !== 'DRAFT' || item.ever_published || item.version !== body.expected_version || item.mapping.draft_version !== body.expected_draft_version) return json(response, { code: 'BILLING_CONFLICT' }, 409);
    Object.assign(item, { display_name_zh_cn: body.display_name_zh_cn, display_name_en: body.display_name_en, display_name_ja: body.display_name_ja, display_order: body.display_order, base_points: body.base_points, bonus_points: body.bonus_points, validity_days: body.validity_days, invoice_product_name: body.invoice_product_name, version: item.version + 1, mapping: { draft_version: item.mapping.draft_version + 1, ...body.mapping } });
    return json(response, item);
  }
  if (url.pathname === '/auth/login') {
    response.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    return response.end('<!doctype html><title>reauthentication</title>');
  }
  if (/^\/api\/admin\/billing\/admin\/v1\/recharge-products\/[^/]+\/admission-verifications$/.test(url.pathname)) {
    if (request.method === 'POST') {
      observations.actionPosts += 1;
      if (createMode === 'stepup') return json(response, { code: 'RECENT_AUTHENTICATION_REQUIRED', message: 'reauthenticate' }, 401);
      return json(response, { status: 'PENDING' }, 201);
    }
    return json(response, { items: [] });
  }
  const relative = url.pathname.replace(/^\//, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot) || !existsSync(target)) {
    response.writeHead(404);
    return response.end('not found');
  }
  const type = path.extname(target) === '.css' ? 'text/css' : 'text/javascript';
  response.writeHead(200, { 'Content-Type': `${type}; charset=utf-8` });
  response.end(await readFile(target));
});

await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
const { port } = server.address();
const configure = async (mode) => fetch(`http://127.0.0.1:${port}/__test__/configure?mode=${encodeURIComponent(mode)}`);
async function fillCreateForm(page, suffix) {
  const values = {
    product_id: `PRODUCT_${suffix}`,
    display_name_zh_cn: `商品${suffix}`,
    display_name_en: `Product ${suffix}`,
    display_name_ja: `商品${suffix}`,
    base_points: '100',
    bonus_points: '10',
    validity_days: '30',
    sku_bom: `SKU-${suffix}`,
    amount_fen: '100',
    reason: `创建商品${suffix}用于受控目录验证`,
  };
  await page.evaluate((input) => {
    const form = document.querySelector('#product-form');
    for (const [name, value] of Object.entries(input)) {
      const control = form.elements[name];
      control.value = value;
      control.dispatchEvent(new Event('input', { bubbles: true }));
    }
  }, values);
  await page.select('#product-form select[name="invoice_product_name"]', '信息服务费');
  await page.select('#product-form select[name="provider_adapter"]', '0');
  await page.click('#product-channel-options input[value="ALIPAY"]');
}
const browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 1000, deviceScaleFactor: 1 });
  await page.evaluateOnNewDocument(() => { if (!localStorage.getItem('pinvou.admin.language')) localStorage.setItem('pinvou.admin.language', 'zh'); });
  await page.goto(`http://127.0.0.1:${port}/test.html`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('#product-current-list .product-item');
  const currentText = await page.$eval('#product-current-list', (node) => node.textContent);
  assert.match(currentText, /在售一百积分/);
  assert.doesNotMatch(currentText, /草稿二百积分|下架三百积分/);
  assert.deepEqual(observations.catalogQueries, ['?limit=200&lifecycle_status=ON_SALE']);
  assert.equal(observations.createOptionsReads, 0, 'create options must be loaded lazily');

  await page.click('#product-create-open');
  await page.waitForSelector('#product-create-dialog[open]');
  assert.equal(observations.createOptionsReads, 1);
  await page.click('[data-product-create-close]');
  assert.equal(await page.$eval('#product-create-dialog', (node) => node.open), false);

  await page.click('#product-manage-open');
  await page.waitForSelector('#product-manage-dialog[open] #product-list .product-item');
  const managedText = await page.$eval('#product-list', (node) => node.textContent);
  assert.match(managedText, /在售一百积分/);
  assert.match(managedText, /草稿二百积分/);
  assert.match(managedText, /下架三百积分/);

  await page.evaluate(() => {
    const item = [...document.querySelectorAll('#product-list .product-item')].find((node) => node.textContent.includes('草稿二百积分'));
    item?.querySelector('button[data-edit-mode="draft"]')?.click();
  });
  await page.waitForSelector('#product-create-dialog[open]');
  assert.equal(await page.$eval('#product-form input[name="product_id"]', (node) => node.readOnly), true);
  assert.equal(await page.$eval('#product-form input[name="product_id"]', (node) => node.value), 'DRAFT_200');
  await page.evaluate(() => {
    const form = document.querySelector('#product-form');
    form.elements.display_name_zh_cn.value = '已编辑草稿二百积分';
    form.elements.reason.value = '编辑未发布充值商品草稿用于验证';
  });
  await page.click('#product-editor-submit');
  await page.waitForFunction(() => document.querySelector('#product-manage-message')?.textContent.includes('草稿已更新'));
  assert.match(await page.$eval('#product-list', (node) => node.textContent), /已编辑草稿二百积分/);
  assert.equal(observations.draftUpdates.length, 1);
  assert.equal(observations.draftUpdates[0].expected_version, 1);
  assert.equal(observations.draftUpdates[0].expected_draft_version, 1);
  assert.equal(observations.draftUpdates[0].product_id, undefined);

  await page.select('#product-filter select[name="lifecycle_status"]', 'ON_SALE');
  await page.click('#product-filter button:not([type])');
  await page.waitForFunction(() => document.querySelector('#product-list')?.textContent.includes('在售一百积分'));
  await page.evaluate(() => {
    const item = [...document.querySelectorAll('#product-list .product-item')].find((node) => node.textContent.includes('在售一百积分'));
    item?.querySelector('button[data-edit-mode="replacement"]')?.click();
  });
  await page.waitForSelector('#product-create-dialog[open]');
  assert.equal(await page.$eval('#product-form input[name="product_id"]', (node) => node.readOnly), false);
  assert.equal(await page.$eval('#product-form input[name="product_id"]', (node) => node.value), '');
  assert.match(await page.$eval('#product-editor-help', (node) => node.textContent), /历史事实不可原地修改/);
  await page.evaluate(() => {
    const form = document.querySelector('#product-form');
    form.elements.product_id.value = 'ON_SALE_100_V2';
    form.elements.sku_bom.value = 'ON-100-V2';
    form.elements.reason.value = '为已发布充值商品创建替代草稿';
  });
  await page.click('#product-editor-submit');
  await page.waitForFunction(() => document.querySelector('#product-manage-message')?.textContent.includes('草稿已创建'));
  assert.equal(observations.replacementCreates.length, 1);
  assert.equal(observations.replacementCreates[0].replacement_product_id, 'ON_SALE_100');
  assert.equal(observations.replacementCreates[0].replacement_expected_version, 3);
  assert.match(await page.$eval('#product-list', (node) => node.textContent), /ON_SALE_100_V2/);
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => !document.querySelector('#product-manage-dialog')?.open);
  assert.equal(await page.evaluate(() => document.activeElement?.id), 'product-manage-open');

  await page.evaluate(() => localStorage.setItem('pinvou.admin.language', 'en'));
  await page.reload({ waitUntil: 'networkidle0' });
  assert.match(await page.$eval('#product-current-list', (node) => node.textContent), /On-sale 100 points/);
  assert.doesNotMatch(await page.$eval('#product-current-list', (node) => node.textContent), /在售一百积分/);
  await page.evaluate(() => localStorage.setItem('pinvou.admin.language', 'ja'));
  await page.reload({ waitUntil: 'networkidle0' });
  assert.match(await page.$eval('#product-current-list', (node) => node.textContent), /販売中100ポイント/);
  await page.evaluate(() => localStorage.setItem('pinvou.admin.language', 'zh'));

  const catalogReadsBeforeNoRead = observations.catalogQueries.length;
  await configure('no-read');
  await page.goto(`http://127.0.0.1:${port}/test.html`, { waitUntil: 'domcontentloaded' });
  assert.equal(await page.$eval('#product-header-actions', (node) => node.hidden), true);
  await page.evaluate(() => {
    document.querySelector('#product-manage-open')?.click();
    document.querySelector('#product-refresh')?.click();
  });
  await new Promise((resolve) => setTimeout(resolve, 350));
  assert.equal(await page.$('#product-admin-root'), null, 'the product area must be removed without read permission');
  assert.equal(observations.catalogQueries.length, catalogReadsBeforeNoRead, 'no catalog request may start before read permission is confirmed');

  const optionReadsBeforeReadonly = observations.createOptionsReads;
  await configure('readonly');
  await page.goto(`http://127.0.0.1:${port}/test.html`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#product-create-open', { hidden: true });
  await page.waitForFunction(() => document.querySelector('#product-header-actions')?.hidden === false);
  assert.equal(await page.$eval('#product-create-open', (node) => node.hidden), true);
  await new Promise((resolve) => setTimeout(resolve, 350));
  assert.equal(await page.$eval('#product-create-open', (node) => node.hidden), true);
  assert.equal(observations.createOptionsReads, optionReadsBeforeReadonly, 'read-only authorization must not load create options');

  await configure('create-options-failure');
  await page.reload({ waitUntil: 'networkidle0' });
  await page.click('#product-create-open');
  await page.waitForFunction(() => document.querySelector('#product-create-message')?.textContent.includes('create options unavailable'));
  assert.equal(await page.$eval('#product-create-dialog', (node) => node.open), true);
  await page.click('[data-product-create-close]');

  await configure('management-failure');
  await page.reload({ waitUntil: 'networkidle0' });
  await page.click('#product-manage-open');
  await page.waitForFunction(() => document.querySelector('#product-manage-message')?.textContent.includes('catalog unavailable'));
  assert.equal(await page.$eval('#product-manage-dialog', (node) => node.open), true);
  await page.click('[data-product-manage-close]');

  await configure('conflict');
  await page.reload({ waitUntil: 'networkidle0' });
  await page.click('#product-create-open');
  await page.waitForSelector('#product-form select[name="provider_adapter"] option[value="0"]');
  await fillCreateForm(page, 'CONFLICT');
  await page.click('#product-form button:not([type])');
  await page.waitForFunction(() => document.querySelector('#product-create-message')?.textContent.includes('BILLING_CONFLICT'));
  await page.click('[data-product-create-close]');
  assert.equal(await page.$eval('#product-create-dialog', (node) => node.open), false);

  await configure('unknown');
  await page.reload({ waitUntil: 'networkidle0' });
  await page.click('#product-create-open');
  await page.waitForSelector('#product-form select[name="provider_adapter"] option[value="0"]');
  await fillCreateForm(page, 'UNKNOWN');
  await page.click('#product-form button:not([type])');
  await page.waitForFunction(() => document.querySelector('#product-create-message')?.textContent.includes('无法确认'));
  await page.click('[data-product-create-close]');
  assert.equal(await page.$eval('#product-create-dialog', (node) => node.open), true, 'unknown create result must keep the dialog locked');

  await configure('full');
  await page.goto(`http://127.0.0.1:${port}/test.html`, { waitUntil: 'networkidle0' });
  await page.click('#product-create-open');
  await page.waitForSelector('#product-form select[name="provider_adapter"] option[value="0"]');
  await fillCreateForm(page, 'SUCCESS');
  await page.click('#product-form button:not([type])');
  await page.waitForSelector('#product-manage-dialog[open]');
  await page.waitForFunction(() => document.querySelector('#product-manage-message')?.textContent.includes('草稿已创建'));
  assert.match(await page.$eval('#product-list', (node) => node.textContent), /商品SUCCESS/);
  await page.click('[data-product-manage-close]');

  await page.evaluate(() => {
    history.pushState({}, '', '/test.html?product_lifecycle_status=OFF_SALE');
    dispatchEvent(new PopStateEvent('popstate'));
  });
  await page.click('#product-manage-open');
  await page.waitForFunction(() => document.querySelector('#product-list')?.textContent.includes('下架三百积分'));
  assert.doesNotMatch(await page.$eval('#product-list', (node) => node.textContent), /草稿二百积分|商品SUCCESS/);
  await page.click('[data-product-manage-close]');

  await page.evaluate(() => {
    const root = document.querySelector('#product-admin-root');
    root.dispatchEvent(new CustomEvent('pinvou-admin-extension-visibility', { detail: { visible: false, authorizationVersion: 'v1' } }));
    history.pushState({}, '', '/test.html?product_lifecycle_status=DRAFT');
    dispatchEvent(new PopStateEvent('popstate'));
    root.dispatchEvent(new CustomEvent('pinvou-admin-extension-visibility', { detail: { visible: true, authorizationVersion: 'v1' } }));
  });
  await page.click('#product-manage-open');
  await page.waitForFunction(() => document.querySelector('#product-list')?.textContent.includes('草稿二百积分'));
  assert.doesNotMatch(await page.$eval('#product-list', (node) => node.textContent), /下架三百积分/);
  await page.click('[data-product-manage-close]');

  await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 1 });
  await page.click('#product-create-open');
  await page.waitForSelector('#product-create-dialog[open]');
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  assert.ok(overflow <= 1, `mobile page must not overflow horizontally: ${overflow}`);
  const dialogOverflow = await page.$eval('#product-create-dialog', (node) => node.scrollWidth - node.clientWidth);
  assert.ok(dialogOverflow <= 1, `mobile create dialog must not overflow horizontally: ${dialogOverflow}`);

  await configure('stepup');
  await page.goto(`http://127.0.0.1:${port}/test.html`, { waitUntil: 'networkidle0' });
  await page.click('#product-create-open');
  await page.waitForSelector('#product-form select[name="provider_adapter"] option[value="0"]');
  await fillCreateForm(page, 'STEPUP');
  const createPostsBeforeStepup = observations.createPosts;
  await page.click('#product-form button:not([type])');
  await page.waitForFunction(() => location.pathname === '/auth/login');
  assert.match(page.url(), /reauthenticate=1/);
  assert.match(page.url(), /return_to=/);
  assert.equal(observations.createPosts, createPostsBeforeStepup + 1);
  const createReturnTo = new URL(page.url()).searchParams.get('return_to');
  await configure('full');
  await page.goto(`http://127.0.0.1:${port}${createReturnTo}`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('#product-create-dialog[open]');
  assert.equal(await page.$eval('#product-form input[name="product_id"]', (node) => node.value), 'PRODUCT_STEPUP');
  assert.match(await page.$eval('#product-create-message', (node) => node.textContent), /草稿已恢复/);
  assert.match(await page.$eval('#product-create-message', (node) => node.textContent), /不会自动提交/);
  assert.equal(observations.createPosts, createPostsBeforeStepup + 1, 'restoring a create draft must not replay POST');
  await page.click('[data-product-create-close]');

  await configure('stepup');
  if (!await page.$eval('#product-manage-dialog', (node) => node.open)) await page.click('#product-manage-open');
  await page.waitForSelector('#product-manage-dialog[open] #product-list .product-item');
  await page.evaluate(() => {
    const item = [...document.querySelectorAll('#product-list .product-item')].find((node) => node.textContent.includes('已编辑草稿二百积分'));
    item?.querySelector('button[data-edit-mode="draft"]')?.click();
  });
  await page.waitForSelector('#product-create-dialog[open]');
  await page.evaluate(() => {
    const form = document.querySelector('#product-form');
    form.elements.display_name_en.value = 'Restored draft edit';
    form.elements.reason.value = '重新认证后继续编辑未发布商品草稿';
  });
  const draftUpdatesBeforeStepup = observations.draftUpdates.length;
  await page.click('#product-editor-submit');
  await page.waitForFunction(() => location.pathname === '/auth/login');
  const draftReturnTo = new URL(page.url()).searchParams.get('return_to');
  assert.equal(observations.draftUpdates.length, draftUpdatesBeforeStepup + 1);
  await configure('full');
  await page.goto(`http://127.0.0.1:${port}${draftReturnTo}`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('#product-create-dialog[open]');
  assert.equal(await page.$eval('#product-form input[name="display_name_en"]', (node) => node.value), 'Restored draft edit');
  assert.equal(await page.$eval('#product-form input[name="reason"]', (node) => node.value), '重新认证后继续编辑未发布商品草稿');
  assert.match(await page.$eval('#product-create-message', (node) => node.textContent), /不会自动提交/);
  assert.equal(observations.draftUpdates.length, draftUpdatesBeforeStepup + 1, 'restoring a draft edit must not replay PUT');
  await page.click('[data-product-create-close]');

  await configure('stepup');
  if (!await page.$eval('#product-manage-dialog', (node) => node.open)) await page.click('#product-manage-open');
  await page.waitForSelector('#product-manage-dialog[open] #product-list .product-item');
  await page.evaluate(() => {
    const item = [...document.querySelectorAll('#product-list .product-item')].find((node) => node.textContent.includes('DRAFT_200'));
    item?.querySelector('button[data-edit-mode="draft"]')?.click();
  });
  await page.waitForSelector('#product-create-dialog[open]');
  await page.evaluate(() => {
    const form = document.querySelector('#product-form');
    form.elements.display_name_en.value = 'Stale restored draft edit';
    form.elements.reason.value = 'Do not overwrite a concurrent administrator update';
  });
  const staleDraftUpdates = observations.draftUpdates.length;
  await page.click('#product-editor-submit');
  await page.waitForFunction(() => location.pathname === '/auth/login');
  const staleReturnTo = new URL(page.url()).searchParams.get('return_to');
  assert.equal(observations.draftUpdates.length, staleDraftUpdates + 1);
  await configure('concurrent-draft');
  await page.goto(`http://127.0.0.1:${port}${staleReturnTo}`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('#product-manage-dialog[open]');
  assert.equal(await page.$eval('#product-create-dialog', (node) => node.open), false, 'a stale restored edit must not reopen against newer versions');
  assert.ok((await page.$eval('#product-manage-message', (node) => node.textContent)).length > 0);
  assert.equal(observations.draftUpdates.length, staleDraftUpdates + 1, 'a stale restored edit must not replay PUT');

  await configure('stepup');
  if (!await page.$eval('#product-manage-dialog', (node) => node.open)) await page.click('#product-manage-open');
  await page.waitForSelector('#product-manage-dialog[open] #product-list .product-item');
  await page.evaluate(() => {
    const item = [...document.querySelectorAll('#product-list .product-item')].find((node) => node.textContent.includes('草稿二百积分'));
    item?.querySelector('button[data-action="verify"]')?.click();
  });
  await page.waitForSelector('#product-action-dialog[open]');
  const restoredReason = '重新认证后继续验证商品草稿';
  await page.type('#product-action-form textarea[name="reason"]', restoredReason);
  const actionPostsBeforeStepup = observations.actionPosts;
  await page.click('#product-action-submit');
  await page.waitForFunction(() => location.pathname === '/auth/login');
  const actionReturnTo = new URL(page.url()).searchParams.get('return_to');
  assert.equal(observations.actionPosts, actionPostsBeforeStepup + 1);
  await configure('full');
  await page.goto(`http://127.0.0.1:${port}${actionReturnTo}`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('#product-action-dialog[open]');
  assert.equal(await page.$eval('#product-action-form textarea[name="reason"]', (node) => node.value), restoredReason);
  assert.match(await page.$eval('#product-action-restore-message', (node) => node.textContent), /草稿已恢复/);
  assert.match(await page.$eval('#product-action-restore-message', (node) => node.textContent), /不会自动提交/);
  assert.equal(observations.actionPosts, actionPostsBeforeStepup + 1, 'restoring an action draft must not replay POST');
  console.log('Admin recharge product catalog UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
