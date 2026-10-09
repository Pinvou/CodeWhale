import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const browserPath = process.env.CHROME || process.env.CHROMIUM || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge', '/Applications/Chromium.app/Contents/MacOS/Chromium'].find(fs.existsSync);
assert.ok(browserPath, 'Browser unavailable; usage UI validation not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-f05-usage-')); const ownedProfile = fs.realpathSync(profile);
let vite, browser, page, passed = 0; const errors = [];
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } }); await vite.listen();
  browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  page = await browser.newPage(); page.setDefaultTimeout(10000); page.on('pageerror', error => errors.push(error.message));
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f05-usage-governance.html`;
  const fresh = async () => { errors.length = 0; await page.goto(url); await page.waitForSelector('#usage-requests-title'); await page.waitForSelector('tbody'); };
  const click = async key => assert.ok(await page.evaluate(key => {
    const label = window.f05Usage.labels()[key]; const button = [...document.querySelectorAll('button')].find(item => (key === 'backToList' ? item.textContent.trim().endsWith(label) : item.textContent.trim() === label) && !item.disabled);
    if (!button) return false; button.click(); return true;
  }, key), `Unavailable ${key}`);
  const detail = async () => { await click('viewDetail'); await page.waitForSelector('#usage-detail-title'); await page.waitForSelector('.adm-evidence-list'); };
  const open = async () => { await detail(); await click('retryReconciliation'); await page.waitForSelector('[role=dialog]'); await page.waitForFunction(() => document.querySelector('[role=dialog] .adm-high-risk-impact') || document.querySelector('[role=dialog] [role=alert]')); };
  const reason = () => page.$eval('[role=dialog] textarea', input => {
    Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(input, 'Controlled reconciliation reason'); input.dispatchEvent(new Event('input', { bubbles: true }));
  });
  const submit = () => page.$eval('[role=dialog]', form => form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })));
  const caseRun = async (name, fn) => { await fn(); assert.deepEqual(errors, [], 'Unexpected React exception'); passed++; console.log(`PASS ${name}`); };
  await caseRun('Actual management entry renders usage, detail and exact int64 evidence', async () => {
    await fresh(); await detail(); assert.ok(await page.evaluate(() => document.querySelector('.adm-operation-facts').textContent.includes('9007199254740993') && document.querySelector('.adm-evidence-list').textContent.includes('9007199254740993')));
    assert.ok(await page.evaluate(() => window.f05Usage.calls.some(call => call.path.endsWith(`/usage-requests/${window.f05Usage.id}/evidence`))));
  });
  await caseRun('Read-only permission never advertises or loads retry readiness', async () => {
    await fresh(); await page.evaluate(() => { window.f05Usage.canRetry = false; window.f05Usage.render(); }); await detail();
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].some(button => button.textContent === window.f05Usage.labels().retryReconciliation)), false);
    assert.equal(await page.evaluate(() => window.f05Usage.calls.some(call => call.path.endsWith('/readiness'))), false);
  });
  await caseRun('Inactive governance exposes real reason and sends no precheck or write', async () => {
    await fresh(); await page.evaluate(() => { window.f05Usage.ready = false; }); await detail();
    await page.waitForFunction(() => document.body.textContent.includes(window.f05Usage.labels().commandReadinessReasons.GOVERNANCE_INACTIVE));
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].some(button => button.textContent === window.f05Usage.labels().retryReconciliation)), false);
    assert.equal(await page.evaluate(() => window.f05Usage.calls.filter(call => call.method === 'POST').length), 0);
  });
  await caseRun('Authoritative precheck blocker prevents governed submission', async () => {
    await fresh(); await page.evaluate(() => { window.f05Usage.blocker = true; }); await open(); await reason();
    assert.equal(await page.$eval('[role=dialog] button[type=submit]', button => button.disabled), true);
    await submit(); assert.equal(await page.evaluate(() => window.f05Usage.calls.filter(call => call.path.endsWith('/commands')).length), 0);
  });
  await caseRun('Foreign precheck target fails closed before any command', async () => {
    await fresh(); await page.evaluate(() => { window.f05Usage.foreignPrecheck = true; }); await open(); await reason();
    assert.equal(await page.$eval('[role=dialog] button[type=submit]', button => button.disabled), true);
    assert.equal(await page.evaluate(() => window.f05Usage.calls.filter(call => call.path.endsWith('/commands')).length), 0);
  });
  await caseRun('Lost submit response queries original key without duplicate execution or model replay', async () => {
    await fresh(); await page.evaluate(() => { window.f05Usage.unknown = true; }); await open(); await reason(); await submit(); await page.waitForSelector('.adm-command-unknown');
    assert.equal(await page.$eval('[role=dialog] textarea', input => input.disabled), true);
    await page.evaluate(() => { window.f05Usage.language = 'ja'; window.f05Usage.render(); }); await page.waitForSelector('.adm-command-unknown'); await submit();
    await page.waitForFunction(() => document.querySelector('[role=dialog]').textContent.includes(window.f05Usage.labels().actionAccepted));
    assert.ok(await page.evaluate(() => {
      const calls = window.f05Usage.calls; const writes = calls.filter(call => call.method === 'POST' && call.path.endsWith('/commands')); const lookup = calls.find(call => call.path.endsWith('/commands/by-idempotency-key'));
      return writes.length === 1 && lookup?.key === writes[0].key && writes[0].body.includes('"evidence_high_watermark":9007199254740993') && writes[0].body.includes('"replay_model_request":false') && writes[0].csrf === 'CONTROLLED_SYNTHETIC_CSRF';
    }));
  });
  await caseRun('Reconciliation queue opens the actual usage target and back preserves selected queue', async () => {
    await fresh(); await click('reconciliationQueue'); await page.waitForSelector('#reconciliation-title'); await page.waitForSelector('tbody'); await click('reviewEvidence'); await page.waitForSelector('#usage-detail-title'); await click('backToList'); await page.waitForSelector('#reconciliation-title');
    assert.ok(await page.evaluate(() => window.f05Usage.calls.some(call => call.path.endsWith('/reconciliation-cases'))));
  });
  await caseRun('Expired list snapshot remains error and refresh clears cursor instead of false empty success', async () => {
    await fresh(); await page.evaluate(() => { window.f05Usage.listFailure = 410; history.replaceState({}, '', `${location.pathname}?usage_cursor=controlled-expired`); dispatchEvent(new PopStateEvent('popstate')); }); await page.waitForSelector('[role=alert]');
    await page.evaluate(() => { window.f05Usage.listFailure = 0; }); await click('retry'); await page.waitForSelector('tbody');
    assert.equal(await page.evaluate(() => new URLSearchParams(location.search).has('usage_cursor')), false);
  });
  await caseRun('No usage read authority removes all entry and no extra future page is advertised', async () => {
    await fresh(); await page.evaluate(() => { window.f05Usage.canRead = false; window.f05Usage.canRetry = false; window.f05Usage.render(); }); await page.waitForFunction(() => !document.querySelector('#usage-requests-title'));
    assert.equal(await page.evaluate(() => window.f05Usage.calls.filter(call => /organization|provisioning|audit/.test(call.path)).length), 0);
  });
  console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
} catch (error) { console.error(`FAIL after ${passed} PASS: ${error.stack}`); process.exitCode = 1; }
finally {
  if (browser) await browser.close(); if (vite) await vite.close();
  if (fs.existsSync(profile)) { assert.equal(fs.realpathSync(profile), ownedProfile); assert.equal(path.dirname(ownedProfile), fs.realpathSync(os.tmpdir())); assert.ok(path.basename(ownedProfile).startsWith('pinvou-f05-usage-')); fs.rmSync(ownedProfile, { recursive: true }); }
}
