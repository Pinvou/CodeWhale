import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(file => fs.existsSync(file));
assert.ok(chrome, 'A browser is required for the diagnostics consent test');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-diagnostics-test-'));
let browser;
let server;
try {
  server = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await server.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, userDataDir: profile, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage();
  const errors = []; page.on('pageerror', error => errors.push(error.message));
  await page.goto(`http://127.0.0.1:${server.httpServer.address().port}/tests/fixtures/failure_diagnostics.html`, { waitUntil: 'networkidle0' });
  const click = async text => { await page.evaluate(label => { const button = [...document.querySelectorAll('button')].find(element => element.textContent === label); if (!button || button.disabled) throw new Error(`Unavailable button: ${label}`); button.click(); }, text); };
  await click('Failure diagnostics and reports');
  await page.waitForSelector('select');
  assert.deepEqual(await page.$$eval('select option', options => options.map(item => item.value)), ['', 'incident_fixture_1']);
  await page.select('select', 'incident_fixture_1');
  await page.type('textarea', 'reproduce with an empty stream');
  await click('Create redacted preview'); await page.waitForSelector('input[type=checkbox]');
  assert.equal(await page.$eval('input[type=checkbox]', item => item.checked), false);
  assert.equal(await page.$$eval('button', buttons => buttons.find(item => item.textContent === 'Confirm and queue').disabled), true);
  await page.click('input[type=checkbox]');
  await page.type('textarea', ' changed');
  await page.waitForFunction(() => !document.querySelector('input[type=checkbox]'));
  assert.equal(await page.evaluate(() => window.diagnosticTest.submitted.length), 0);
  await click('Create redacted preview'); await page.waitForSelector('input[type=checkbox]');
  assert.equal(await page.$eval('input[type=checkbox]', item => item.checked), false);
  await page.click('input[type=checkbox]'); await click('Confirm and queue');
  await page.waitForFunction(() => window.diagnosticTest.submitted.length === 1 && !document.querySelector('input[type=checkbox]'));
  await page.evaluate(() => { window.diagnosticTest.files = ['a.log', 'b.log', 'c.log', 'd.log']; });
  await click('Choose logs, text or screenshots');
  await page.waitForFunction(() => document.body.textContent.includes('The operation failed'));
  assert.equal(await page.evaluate(() => window.diagnosticTest.previews.length), 2);
  await page.evaluate(() => { window.diagnosticTest.previewError = 'diagnostic_preview_expired'; });
  await click('Create redacted preview');
  await page.waitForFunction(() => document.body.textContent.includes('The preview expired'));
  await page.evaluate(() => { window.diagnosticTest.previewError = ''; });
  await click('Create redacted preview'); await page.waitForSelector('input[type=checkbox]');
  await page.click('input[type=checkbox]');
  for (const [code, text] of [['diagnostic_authorization_changed', 'Analytics consent changed'], ['diagnostic_queue_full', 'The report queue is full']]) {
    await page.evaluate(code => { window.diagnosticTest.submitError = code; }, code);
    await click('Confirm and queue');
    await page.waitForFunction(text => document.body.textContent.includes(text), {}, text);
  }
  assert.equal(await page.evaluate(() => window.diagnosticTest.submitted.length), 1);
  await page.evaluate(() => {
    window.diagnosticTest.reports = [{ report_id: 'ended_report', incident_id: 'incident_fixture_1', status: 'rejected', reason: 'collection_inactive' }];
  });
  await click('Refresh');
  await page.waitForFunction(() => document.body.textContent.includes('The collection period ended and this report was not sent'));
  await page.evaluate(() => { window.diagnosticTest.enabled = false; }); await click('Refresh');
  await page.waitForFunction(() => document.body.textContent.includes('Usage analytics is off'));
  assert.equal(await page.$$eval('button', buttons => buttons.find(item => item.textContent === 'Create redacted preview').disabled), true);
  assert.deepEqual(errors, []);
  console.log('PASS: diagnostics turn isolation, exact preview consent, edits, attachment limit, queued receipt, actionable errors, ended-period state and opt-out');
} finally {
  await browser?.close(); await server?.close(); fs.rmSync(profile, { recursive: true, force: true });
}
