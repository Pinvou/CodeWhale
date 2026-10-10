import assert from 'node:assert/strict';
import fs from 'node:fs';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
const executablePath = process.env.F18_STATUS_BROWSER_EXECUTABLE || process.env.CHROME || [
  'C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/snap/bin/chromium', '/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
].find(fs.existsSync);
assert.ok(executablePath, 'Browser unavailable; system status UI validation not performed');
const server = await createServer({ root: import.meta.dirname + '/..', configFile: false, plugins: [react()], cacheDir: process.env.F18_STATUS_VITE_CACHE || import.meta.dirname + '/../node_modules/.vite-status-test', server: { host: '127.0.0.1', port: 0, fs: { allow: [import.meta.dirname + '/..'], deny: ['**/.env*', '**/*.{pem,crt,key}'] } } });
await server.listen();
let browser;
try { browser = await puppeteer.launch({ executablePath, headless: true, args: ['--no-sandbox', '--disable-gpu'] }); }
catch (error) { await server.close(); throw error; }
const results = [];
const run = async (name, fn) => {
  const page = await browser.newPage(), errors = [];
  page.setDefaultTimeout(8000);
  page.on('pageerror', error => errors.push(error.message));
  try { await fn(page); assert.deepEqual(errors, []); results.push({ name, pass: true }); }
  catch (error) { results.push({ name, pass: false, error: error.message }); }
  finally { await page.close(); }
};
const open = async (page, query = '') => {
  await page.goto(server.resolvedUrls.local[0] + `tests/account_platform_system_status_fixture.html?section=system&${query}`);
  await page.waitForFunction(() => window.statusFixture);
};
try {
  for (const language of ['zh', 'en', 'ja']) await run(`actual admin permission navigation, status, int64, readonly runbooks ${language}`, async page => {
    await open(page, `language=${language}`);
    await page.waitForSelector('.adm-system-workspace .adm-metrics');
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('nav button')].some(button => button.textContent === window.statusFixture.copy.nav.system)), true);
    const text = await page.evaluate(() => document.querySelector('.adm-system-workspace').textContent);
    assert.ok(text.includes('9,007,199,254,740,993'));
    assert.ok(text.includes('9,223,372,036,854,775,807'));
    for (const key of ['services', 'relays', 'recentErrors']) assert.ok(text.includes(await page.evaluate(key => window.statusFixture.copy.system[key], key)));
    assert.ok(text.includes('HEALTHY'));
    await page.click('.adm-runbook-panel summary');
    assert.ok(text.includes(await page.evaluate(() => window.statusFixture.copy.runbooks.noExecution)));
    assert.deepEqual(await page.evaluate(() => window.statusFixture.calls.filter(call => call.url === '/api/admin/system/status').map(call => call.method)), ['GET']);
    assert.equal(await page.evaluate(() => document.querySelectorAll('.adm-runbook-panel button').length), 0);
    await page.evaluate(() => { window.statusFixture.mode = 'unhealthy'; });
    await page.click('.adm-system-workspace > button');
    await page.waitForFunction(() => document.querySelector('.adm-system-workspace')?.textContent.includes('controlled pending error'));
    const expectedUpdated = await page.evaluate(() => window.statusFixture.copy.system.updated);
    assert.equal(typeof expectedUpdated, 'string');
    assert.ok(await page.evaluate(label => [...document.querySelectorAll('.adm-system-workspace th')].some(cell => cell.textContent === label), expectedUpdated));
    assert.deepEqual(await page.evaluate(() => window.statusFixture.calls.filter(call => call.url === '/api/admin/system/status').map(call => call.method)), ['GET', 'GET']);
  });
  await run('unhealthy, degraded relay and recent errors remain visible', async page => {
    await open(page, 'mode=unhealthy'); await page.waitForSelector('.adm-system-workspace .adm-metrics');
    const text = await page.evaluate(() => document.querySelector('.adm-system-workspace').textContent);
    for (const value of ['UNHEALTHY', 'DEGRADED', 'connection failed', 'controlled-resource', 'controlled pending error']) assert.ok(text.includes(value));
    assert.equal(await page.evaluate(() => document.querySelector('.adm-metrics article strong').textContent), '0');
  });
  await run('missing response displays error with no fabricated health', async page => {
    await open(page, 'mode=missing'); await page.waitForSelector('.adm-system-workspace [role=alert]');
    assert.equal(await page.$('.adm-metrics'), null);
  });
  await run('HTTP unavailable displays error and retry can load current status', async page => {
    await open(page, 'mode=error'); await page.waitForSelector('.adm-system-workspace [role=alert]');
    assert.equal(await page.$('.adm-metrics'), null);
    await page.evaluate(() => { window.statusFixture.mode = 'healthy'; });
    await page.click('.adm-system-workspace [role=alert] button'); await page.waitForSelector('.adm-metrics');
  });
  await run('denied permission hides nav and performs no status request', async page => {
    await open(page, 'mode=deny'); await page.waitForSelector('main [role=alert]');
    assert.equal(await page.$('.adm-system-workspace'), null);
    assert.equal(await page.evaluate(() => window.statusFixture.calls.some(call => call.url === '/api/admin/system/status')), false);
  });
  await run('client replacement prevents late old status from overwriting current error', async page => {
    await open(page, 'mode=delay'); await page.waitForFunction(() => window.statusFixture.resolve);
    await page.evaluate(async () => { const { AdminPlatformClient } = await import('../src/features/account-platform/admin-client.js'); window.statusFixture.mode = 'error'; window.statusFixture.render(new AdminPlatformClient()); });
    await page.waitForSelector('.adm-system-workspace [role=alert]');
    await page.evaluate(() => { window.statusFixture.resolve(); });
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.waitForFunction(() => !document.querySelector('.adm-metrics') && !!document.querySelector('.adm-system-workspace [role=alert]'));
    assert.equal(await page.$('.adm-metrics'), null);
  });
} finally { await browser.close(); await server.close(); }
const result = { pass: results.filter(row => row.pass).length, fail: results.filter(row => !row.pass).length, results, limits: ['Real AdminPlatformView, independent SystemStatusWorkspace and AdminPlatformClient with controlled HTTP. No real aggregator probes, deployment, canonical Portal build, full F18 parent, or E2E.'] };
fs.writeFileSync(process.env.F18_STATUS_REACT_RESULT || 'system-status-smoke-result.json', JSON.stringify(result, null, 2));
console.log(JSON.stringify(result)); process.exitCode = result.fail ? 1 : 0;
