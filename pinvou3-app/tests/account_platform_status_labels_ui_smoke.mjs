import assert from 'node:assert/strict';
import fs from 'node:fs';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import { createServer } from './helpers/vite-fixture.mjs';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { navigateFixture } from './helpers/navigate-fixture.mjs';

const chrome = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; status UI verification not performed');
const expected = { zh: { active: '有效', unknown: '未知状态' }, en: { active: 'Active', unknown: 'Unknown status' }, ja: { active: '有効', unknown: '不明な状態' } };
let server, browser, primary;
let passed = 0;
try {
  server = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()] });
  await server.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
  async function run(name, action) {
    const page = await browser.newPage(), errors = [];
    page.setDefaultTimeout(5000);
    page.on('pageerror', error => errors.push(error.message));
    page.on('dialog', async dialog => { errors.push(`Unexpected browser dialog: ${dialog.type()}`); await dialog.dismiss(); });
    try { await action(page); assert.deepEqual(errors, []); passed++; console.log(`PASS ${name}`); }
    finally { await page.close(); }
  }
  const main = async page => navigateFixture(page, `${origin}/tests/fixtures/account-platform-f01-ui.html`, signal => page.waitForFunction(() => window.f01 && document.body.textContent.includes('account-a'), { signal }));
  const chooseLanguage = (page, language) => page.select('.ap-sidebar-foot select, .adm-sidebar-foot select, header select', language);
  for (const language of ['zh', 'en', 'ja']) {
    await run(`${language}: routed order results retain legacy refund labels and raw identities`, async page => {
      await navigateFixture(page, `${origin}/tests/fixtures/account-status-labels-ui.html?language=${language}&orders=1`, signal => page.waitForFunction(() => document.querySelectorAll('.adm-order-list tbody tr').length === 3, { signal }));
      const labels = { zh: ['待支付', '部分退款', '未知状态 (FUTURE_REVIEW)'], en: ['Awaiting payment', 'Partially refunded', 'Unknown status (FUTURE_REVIEW)'], ja: ['支払い待ち', '一部返金済み', '不明な状態 (FUTURE_REVIEW)'] };
      assert.deepEqual(await page.$$eval('.adm-order-list tbody td:nth-child(7)', nodes => nodes.map(node => node.textContent)), labels[language]);
      assert.deepEqual(await page.evaluate(() => window.statusLabelsProbe.orders.map(item => item.status)), ['PENDING', 'PARTIALLY_REFUNDED', 'FUTURE_REVIEW']);
      assert.deepEqual(await page.evaluate(() => window.statusLabelsProbe.viewed), []);
      await page.click('.adm-order-list tbody tr:nth-child(2) button');
      assert.deepEqual(await page.evaluate(() => window.statusLabelsProbe.viewed), ['00000000-0000-4000-8000-000000000022']);
    });
    await run(`${language}: production account BFF/native views preserve safe unknown text and language changes`, async page => {
      await main(page); await chooseLanguage(page, language);
      await page.click('[data-section=accountSettings]');
      await page.evaluate(() => { window.f01.userStatus = 'constructor'; window.f01.switchIdentity(); });
      await page.waitForFunction(label => document.querySelector('.ap-account-information .ap-status')?.textContent.includes(`${label} (constructor)`), {}, expected[language].unknown);
      await page.evaluate(() => { window.f01.userStatus = 'ACTIVE'; window.f01.render('desktop'); });
      await page.waitForFunction(label => [...document.querySelectorAll('dd')].some(node => node.textContent === label), {}, expected[language].active);
      await page.evaluate(() => { window.f01.userStatus = 'constructor'; window.f01.render('desktop'); });
      await page.waitForFunction(label => [...document.querySelectorAll('dd')].some(node => node.textContent === `${label} (constructor)`), {}, expected[language].unknown);
      await page.evaluate(() => { window.f01.userStatus = '<img src=x onerror=alert(1)>'; window.f01.render('desktop'); });
      await page.waitForFunction(label => [...document.querySelectorAll('dd')].some(node => node.textContent === `${label} (<img src=x onerror=alert(1)>)` && node.childElementCount === 0), {}, expected[language].unknown);
      const next = language === 'ja' ? 'en' : 'ja'; await chooseLanguage(page, next);
      await page.waitForFunction(label => document.body.textContent.includes(`${label} (<img src=x onerror=alert(1)>)`), {}, expected[next].unknown);
      // Both real clients bootstrap with POST when mounted. Keep that existing
      // initialization boundary; status rendering must not add other writes.
      assert.deepEqual(await page.evaluate(() => window.f01.calls.filter(call => call.method !== 'GET' && !(call.method === 'POST' && call.url === '/api/account/v1/account/bootstrap')).map(call => ({ method: call.method, url: call.url }))), []);
      await page.evaluate(() => { window.f01.authenticated = false; window.f01.registration = { registration_operation_id: window.f01.id, username: 'controlled-registration', status: 'REPAIR_REQUIRED' }; window.f01.render('desktop'); });
      await page.waitForFunction(label => document.querySelector('.ap-login [role=status]')?.textContent.includes(label), {}, { zh: '需要人工修复', en: 'Manual repair required', ja: '手動修復が必要' }[next]);
    });
    await run(`${language}: registered admin user entry keeps unknowns read-only and submits the original status enum`, async page => {
      await main(page); await chooseLanguage(page, language);
      await page.evaluate(() => { window.f01.status = 'constructor'; history.replaceState({}, '', '?section=users'); window.f01.render('admin'); });
      await page.waitForFunction(label => [...document.querySelectorAll('tbody td')].some(node => node.textContent === `${label} (constructor)`), {}, expected[language].unknown);
      assert.equal(await page.evaluate(() => window.f01.calls.filter(call => call.url.endsWith('/status')).length), 0);
      await page.evaluate(() => { window.f01.status = 'DISABLED'; document.querySelector('form[data-user-directory-search]').requestSubmit(); });
      await page.waitForFunction(() => [...document.querySelectorAll('tbody tr button')].some(button => button.textContent === window.f01.copy().admin.accounts.enable));
      await page.evaluate(() => [...document.querySelectorAll('tbody tr button')].find(button => button.textContent === window.f01.copy().admin.accounts.enable).click());
      await page.waitForSelector('[role=dialog]');
      await page.click('[role=dialog] [data-user-status-confirm=ACTIVE]');
      await page.waitForFunction(() => window.f01.status === 'ACTIVE');
      const commands = await page.evaluate(() => window.f01.calls.filter(call => call.url.endsWith('/status')).map(call => ({ method: call.method, body: JSON.parse(call.body) })));
      assert.deepEqual(commands, [{ method: 'PATCH', body: { status: 'ACTIVE' } }]);
    });
    await run(`${language}: real token status and role list/detail do not inherit prototype labels or translate filter values`, async page => {
      await navigateFixture(page, `${origin}/tests/fixtures/account-status-labels-ui.html?language=${language}`, signal => page.waitForFunction(() => window.statusLabelsProbe && document.querySelector('tbody tr'), { signal }));
      assert.equal(await page.$eval('#probe-status', node => node.textContent), `${expected[language].unknown} (constructor)`);
      assert.equal(await page.$eval('#probe-status .adm-status', node => node.classList.contains('ok') || node.classList.contains('warn')), false);
      for (const code of ['PERSONAL', 'P0']) {
        await page.evaluate(value => window.statusLabelsProbe.setValue(value), code);
        await page.waitForFunction(value => window.statusLabelsProbe.renderedValue === value, {}, code);
        assert.equal(await page.$eval('#probe-status', node => node.textContent), `${expected[language].unknown} (${code})`);
      }
      await page.select('.adm-role-toolbar select', 'ACTIVE');
      assert.equal(await page.$$eval('tbody tr', rows => rows.length), 1);
      assert.equal(await page.$eval('.adm-role-toolbar select', node => node.value), 'ACTIVE');
      await page.select('.adm-role-toolbar select', '');
      await page.click('tbody tr:nth-child(2) button');
      await page.waitForFunction(label => document.querySelector('[role=dialog]')?.textContent.includes(`${label} (FUTURE_REVIEW)`), {}, expected[language].unknown);
      assert.deepEqual(await page.evaluate(() => window.statusLabelsProbe.viewed), ['FUTURE_REVIEW']);
      await page.evaluate(() => window.statusLabelsProbe.setValue('<svg onload=alert(1)>'));
      await page.waitForFunction(label => document.querySelector('#probe-status')?.textContent === `${label} (<svg onload=alert(1)>)`, {}, expected[language].unknown);
      assert.equal(await page.$('#probe-status svg'), null);
    });
    await run(`${language}: operation priorities, evidence kinds and delivery status retain their own semantics`, async page => {
      await navigateFixture(page, `${origin}/tests/fixtures/account-status-labels-ui.html?language=${language}&operations=1&operations_tab=reconciliation`, signal => page.waitForFunction(() => document.querySelectorAll('#operations-probe tbody tr').length === 3, { signal }));
      const labels = await page.evaluate(() => window.statusLabelsProbe.operationsCopy().enumLabels);
      assert.deepEqual(await page.$$eval('#operations-probe tbody tr td:nth-child(3)', cells => cells.map(cell => cell.textContent)), ['P0', 'P1', 'P2'].map(code => labels[code]));
      const select = await page.evaluateHandle(() => [...document.querySelectorAll('#operations-probe select')].find(node => [...node.options].some(option => option.value === 'P0')));
      await select.asElement().select('P1');
      await page.click('#operations-probe form button[type=submit]');
      await page.waitForFunction(() => window.statusLabelsProbe.operationsCalls.some(query => query.severity === 'P1') && document.querySelectorAll('#operations-probe tbody tr').length === 1);
      await page.click('#operations-probe tbody tr button');
      await page.waitForFunction(() => document.querySelectorAll('#operations-probe .adm-evidence-list article').length === 2);
      assert.deepEqual(await page.$$eval('#operations-probe .adm-evidence-list article .adm-status', nodes => nodes.map(node => node.textContent)), ['CANCELLATION', 'CONVERGENCE'].map(code => labels[code]));
      for (const code of ['NOT_STARTED', 'DELIVERING', 'DISCONNECTED', 'DELIVERED', 'USER_STOPPED']) {
        await page.evaluate(value => { window.statusLabelsProbe.delivery = value; [...document.querySelectorAll('#operations-probe header button')].find(button => button.textContent === window.statusLabelsProbe.operationsCopy().refreshSource).click(); }, code);
        const expectedLabel = { zh: { NOT_STARTED: '尚未开始交付', DELIVERING: '交付中', DISCONNECTED: '交付连接已断开', DELIVERED: '已交付' }, en: { NOT_STARTED: 'Delivery not started', DELIVERING: 'Delivering', DISCONNECTED: 'Delivery disconnected', DELIVERED: 'Delivered' }, ja: { NOT_STARTED: '配信未開始', DELIVERING: '配信中', DISCONNECTED: '配信接続が切断', DELIVERED: '配信済み' } }[language][code] || labels[code];
        await page.waitForFunction(label => [...document.querySelectorAll('#operations-probe .adm-operation-facts dd')].some(node => node.textContent === label), {}, expectedLabel);
      }
      for (const code of ['STARTED', 'STREAMING', 'SUCCEEDED', 'FAILED', 'CANCELLED', null]) {
        await page.evaluate(value => { window.statusLabelsProbe.attempt = value; [...document.querySelectorAll('#operations-probe header button')].find(button => button.textContent === window.statusLabelsProbe.operationsCopy().refreshSource).click(); }, code);
        const expectedLabel = code === null ? '—' : code === 'STARTED' ? { zh: '已开始', en: 'Started', ja: '開始済み' }[language] : labels[code];
        await page.waitForFunction(label => [...document.querySelectorAll('#operations-probe .adm-operation-facts > div')].find(node => node.querySelector('dt')?.textContent === window.statusLabelsProbe.operationsCopy().attemptStatus)?.querySelector('dd')?.textContent === label, {}, expectedLabel);
      }
    });
    await run(`${language}: batch candidates and selected summary use the safe status label`, async page => {
      await navigateFixture(page, `${origin}/tests/fixtures/account-status-labels-ui.html?language=${language}&reconciliation=1&batch=1`, signal => page.waitForSelector('.adm-reconciliation-navigation button:nth-child(2)', { signal }));
      await page.click('.adm-reconciliation-navigation button:nth-child(2)');
      await page.waitForSelector('.adm-case-batch-picker');
      await page.$eval('.adm-case-batch-picker input[type="date"]', input => {
        Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, '2026-10-08');
        input.dispatchEvent(new Event('input', { bubbles: true }));
        input.dispatchEvent(new Event('change', { bubbles: true }));
      });
      await page.click('.adm-case-batch-picker button[type="submit"]');
      await page.waitForSelector('.adm-case-batch-candidates button');
      const candidate = await page.$eval('.adm-case-batch-candidates button span', node => node.textContent);
      const unknownStatus = `${expected[language].unknown} (FUTURE_REVIEW)`;
      assert.equal(candidate, `WECHAT · 2026-10-08 · ${unknownStatus}`);
      await page.click('.adm-case-batch-candidates button');
      await page.waitForFunction(label => [...document.querySelectorAll('.adm-case-batch-summary dd')].some(node => node.textContent === label), {}, unknownStatus);
      const id = await page.$eval('.adm-case-batch-summary code', node => node.textContent);
      assert.equal(id, '11111111-1111-4111-8111-111111111111');
      await page.waitForFunction(runID => window.statusLabelsProbe.reconciliationCalls.some(call => call.kind === 'cases' && call.run_id === runID), {}, id);
    });
    await run(`${language}: real reconciliation list, detail and filter retain locale and raw query values`, async page => {
      const review = { zh: '复核中', en: 'Under review', ja: '確認中' };
      await navigateFixture(page, `${origin}/tests/fixtures/account-status-labels-ui.html?language=${language}&reconciliation=1`, signal => page.waitForFunction(() => window.statusLabelsProbe && document.querySelector('section nav button:nth-child(2)'), { signal }));
      await page.click('section nav button:nth-child(2)');
      await page.waitForSelector('.adm-case-scope button:first-child', { visible: true });
      await page.click('.adm-case-scope button:first-child');
      await page.waitForFunction(label => document.querySelector('tbody tr td:nth-child(2)')?.textContent === label, {}, review[language]);
      const options = await page.$$eval('option', nodes => nodes.filter(node => ['OPEN', 'IN_REVIEW', 'RESOLVED', 'IGNORED'].includes(node.value)).map(node => ({ value: node.value, label: node.textContent })));
      assert.deepEqual(options.map(option => option.value), ['OPEN', 'IN_REVIEW', 'RESOLVED', 'IGNORED']);
      assert.ok(options.every(option => option.label && option.label !== option.value));
      assert.equal(options[1].label, review[language]);
      await page.evaluate(() => { const select = [...document.querySelectorAll('select')].find(node => [...node.options].some(option => option.value === 'IN_REVIEW')); select.value = 'IN_REVIEW'; select.dispatchEvent(new Event('change', { bubbles: true })); });
      await page.waitForFunction(() => window.statusLabelsProbe.reconciliationCalls.some(call => call.kind === 'cases' && call.status === 'IN_REVIEW'));
      await page.waitForSelector('.adm-reconciliation-workspace tbody tr button', { visible: true });
      await page.click('.adm-reconciliation-workspace tbody tr button');
      await page.waitForFunction(label => document.querySelector('.adm-reconciliation-workspace > section.adm-panel h3')?.nextElementSibling.textContent.includes(label), {}, review[language]);
      const nextLanguage = language === 'ja' ? 'zh' : 'ja';
      await page.select('#probe-language', nextLanguage);
      await page.waitForFunction(label => document.querySelector('tbody tr td:nth-child(2)')?.textContent === label && document.querySelector('.adm-reconciliation-workspace > section.adm-panel h3')?.nextElementSibling.textContent.includes(label), {}, review[nextLanguage]);
    });
  }
} catch (error) { primary = error; }
const cleanup = await Promise.allSettled([...(browser ? [browser.close()] : []), ...(server ? [server.close()] : [])]);
const failures = [...(primary ? [primary] : []), ...cleanup.filter(result => result.status === 'rejected').map(result => result.reason)];
if (failures.length) throw new AggregateError(failures, 'Status UI verification or fixture retirement failed');
console.log(JSON.stringify({ passed, failed: 0, skipped: 0, scope: 'Real mounted React/BFF/native-client paths; downstream HTTP/native fixture boundaries only; no live Account or Keycloak' }));
