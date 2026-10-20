import assert from 'node:assert/strict';
import fs from 'node:fs';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
const executablePath = process.env.F17_BROWSER_EXECUTABLE || process.env.CHROME || [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/snap/bin/chromium', '/usr/bin/chromium', '/usr/bin/chromium-browser',
  '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
].find(fs.existsSync);
assert.ok(executablePath, 'Browser unavailable; UI validation not performed');
const server = await createServer({ root: import.meta.dirname + '/..', configFile: false, plugins: [react()], cacheDir: process.env.F17_BOUNDARY_VITE_CACHE || import.meta.dirname + '/../node_modules/.vite-f17-audit-boundaries', server: { host: '127.0.0.1', port: 0, fs: { allow: [import.meta.dirname + '/..'], deny: ['**/.env*', '**/*.{pem,crt,key}'] } } });
await server.listen();
const browser = await puppeteer.launch({ executablePath, headless: true, args: ['--no-sandbox', '--disable-gpu'] });
const results = [];
async function run(name, fn) {
  const page = await browser.newPage(); page.setDefaultTimeout(8000);
  const errors = []; page.on('pageerror', error => errors.push(error.message));
  try { await fn(page); assert.deepEqual(errors, []); results.push({ name, pass: true }); }
  catch (error) { results.push({ name, pass: false, error: error.message }); }
  finally { await page.close(); }
}
async function open(page, query) {
  await page.goto(server.resolvedUrls.local[0] + 'tests/account_platform_audit_fixture.html?section=audit&' + query);
  await page.waitForFunction(() => window.f17 && document.querySelector('.adm-panel'));
}
async function fill(page, reason = 'Controlled original audit justification') {
  await page.evaluate(reason => {
    const change = (input, value) => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, value); input.dispatchEvent(new Event('input', { bubbles: true })); };
    const dates = [...document.querySelectorAll('input[type=datetime-local]')];
    change(dates[0], '2026-10-06T00:00'); change(dates[1], '2026-10-07T00:00');
    change(document.querySelector('input[minlength]'), reason);
  }, reason);
}
async function button(page, key) { await page.evaluate(key => [...document.querySelectorAll('button')].find(button => button.textContent.trim() === window.f17.copy.audit[key]).click(), key); }
try {
  await run('export-only permission installs usable form without unauthorized log read', async page => {
    await open(page, 'exportOnly=1'); await page.waitForSelector('input[minlength]');
    assert.equal(await page.evaluate(() => window.f17.calls.some(call => call.url.includes('/audit-logs'))), false);
    await fill(page); await button(page, 'export'); await page.waitForFunction(() => document.body.textContent.includes('controlled-page-only-key'));
  });
  await run('PII permission alone cannot install or call base audit export', async page => {
    await page.goto(server.resolvedUrls.local[0] + 'tests/account_platform_audit_fixture.html?section=audit&piiOnly=1');
    await page.waitForFunction(() => window.f17 && document.querySelector('[role=alert]'));
    assert.equal(await page.$('input[minlength]'), null);
    assert.equal(await page.evaluate(() => window.f17.calls.some(call => call.url.includes('/audit-'))), false);
  });
  await run('base plus independent PII permission forwards explicit choice', async page => {
    await open(page, 'pii=1'); await fill(page); await page.click('input[type=checkbox]'); await button(page, 'export');
    await page.waitForFunction(() => document.body.textContent.includes('controlled-page-only-key'));
    assert.equal(await page.evaluate(() => JSON.parse(window.f17.calls.find(call => call.url.endsWith('/audit-exports')).body).include_pii), true);
  });
  for (const status of [400, 429]) await run('initial definite ' + status + ' permits corrected new intent', async page => {
    await open(page, 'initialReject=' + status); await fill(page); await button(page, 'export'); await page.waitForSelector('[role=alert]');
    await page.waitForFunction(() => !document.querySelector('input[minlength]').disabled);
    await fill(page, 'Controlled corrected audit justification'); await button(page, 'export');
    await page.waitForFunction(() => document.body.textContent.includes('controlled-page-only-key'));
    assert.ok(await page.evaluate(() => { const calls = window.f17.calls.filter(call => call.method === 'POST' && call.url.endsWith('/audit-exports')); return calls.length === 2 && calls[0].headers['Idempotency-Key'] !== calls[1].headers['Idempotency-Key'] && calls[0].body !== calls[1].body; }));
  });
  await run('409 retains key, filter, reason and PII until authority receipt', async page => {
    await open(page, 'initialReject=409&pii=1'); await fill(page); await page.click('input[type=checkbox]');
    await button(page, 'export'); await page.waitForSelector('[role=alert]');
    assert.equal(await page.$eval('input[minlength]', input => input.disabled), true);
    assert.equal(await page.$eval('input[type=checkbox]', input => input.disabled), true);
    assert.equal(await page.$eval('input[type=datetime-local]', input => input.matches(':disabled')), true);
    await button(page, 'retryExport'); await page.waitForFunction(() => document.body.textContent.includes('controlled-page-only-key'));
    assert.ok(await page.evaluate(() => { const calls = window.f17.calls.filter(call => call.method === 'POST' && call.url.endsWith('/audit-exports')); return calls.length === 2 && calls[0].headers['Idempotency-Key'] === calls[1].headers['Idempotency-Key'] && calls[0].body === calls[1].body && JSON.parse(calls[0].body).include_pii === true; }));
    await button(page, 'newExport'); await fill(page, 'Controlled explicitly new audit justification'); await button(page, 'export');
    await page.waitForFunction(() => window.f17.calls.filter(call => call.method === 'POST' && call.url.endsWith('/audit-exports')).length === 3);
    assert.ok(await page.evaluate(() => { const calls = window.f17.calls.filter(call => call.method === 'POST' && call.url.endsWith('/audit-exports')); return calls[2].headers['Idempotency-Key'] !== calls[1].headers['Idempotency-Key']; }));
  });
} finally { await browser.close(); await server.close(); }
const result = { pass: results.filter(item => item.pass).length, fail: results.filter(item => !item.pass).length, results, limits: ['Real AdminPlatformView/AdminPlatformClient with controlled BFF responses; no real issuer, worker, PostgreSQL or OS acceptance.'] };
if (process.env.F17_BOUNDARY_REACT_RESULT) fs.writeFileSync(process.env.F17_BOUNDARY_REACT_RESULT, JSON.stringify(result, null, 2));
console.log(JSON.stringify(result)); process.exitCode = result.fail ? 1 : 0;
