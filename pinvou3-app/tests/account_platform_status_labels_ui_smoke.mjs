import assert from 'node:assert/strict';
import fs from 'node:fs';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import { createServer } from './helpers/vite-fixture.mjs';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { navigateFixture } from './helpers/navigate-fixture.mjs';
import { adminLayoutCopy } from '../src/shared/i18n.js';

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
      await page.evaluate(() => { window.f01.status = 'DISABLED'; document.querySelector('.adm-filter-row form button').click(); });
      await page.waitForFunction(() => [...document.querySelectorAll('tbody tr button')].some(button => button.textContent === window.f01.copy().admin.accounts.enable));
      await page.evaluate(() => [...document.querySelectorAll('tbody tr button')].find(button => button.textContent === window.f01.copy().admin.accounts.enable).click());
      await page.waitForSelector('[role=dialog]');
      await page.evaluate(label => [...document.querySelectorAll('[role=dialog] button')].find(button => button.textContent === label).click(), adminLayoutCopy[language].confirmAction);
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
    await run(`${language}: real reconciliation list, detail and filter retain locale and raw query values`, async page => {
      const review = { zh: '复核中', en: 'Under review', ja: '確認中' };
      await navigateFixture(page, `${origin}/tests/fixtures/account-status-labels-ui.html?language=${language}&reconciliation=1`, signal => page.waitForFunction(() => window.statusLabelsProbe && document.querySelector('section nav button:nth-child(2)'), { signal }));
      await page.click('section nav button:nth-child(2)');
      await page.waitForFunction(label => document.querySelector('tbody tr td:nth-child(2)')?.textContent === label, {}, review[language]);
      const options = await page.$$eval('option', nodes => nodes.filter(node => ['OPEN', 'IN_REVIEW', 'RESOLVED', 'IGNORED'].includes(node.value)).map(node => ({ value: node.value, label: node.textContent })));
      assert.deepEqual(options.map(option => option.value), ['OPEN', 'IN_REVIEW', 'RESOLVED', 'IGNORED']);
      assert.ok(options.every(option => option.label && option.label !== option.value));
      assert.equal(options[1].label, review[language]);
      await page.evaluate(() => { const select = [...document.querySelectorAll('select')].find(node => [...node.options].some(option => option.value === 'IN_REVIEW')); select.value = 'IN_REVIEW'; select.dispatchEvent(new Event('change', { bubbles: true })); });
      await page.waitForFunction(() => window.statusLabelsProbe.reconciliationCalls.some(call => call.kind === 'cases' && call.status === 'IN_REVIEW'));
      await page.click('tbody tr button');
      assert.ok((await page.$eval('h3', node => node.nextElementSibling.textContent)).includes(review[language]));
      const nextLanguage = language === 'ja' ? 'zh' : 'ja';
      await page.select('#probe-language', nextLanguage);
      await page.waitForFunction(label => document.querySelector('tbody tr td:nth-child(2)')?.textContent === label && document.querySelector('h3')?.nextElementSibling.textContent.includes(label), {}, review[nextLanguage]);
    });
  }
} catch (error) { primary = error; }
const cleanup = await Promise.allSettled([...(browser ? [browser.close()] : []), ...(server ? [server.close()] : [])]);
const failures = [...(primary ? [primary] : []), ...cleanup.filter(result => result.status === 'rejected').map(result => result.reason)];
if (failures.length) throw new AggregateError(failures, 'Status UI verification or fixture retirement failed');
console.log(JSON.stringify({ passed, failed: 0, skipped: 0, scope: 'Real mounted React/BFF/native-client paths; downstream HTTP/native fixture boundaries only; no live Account or Keycloak' }));
