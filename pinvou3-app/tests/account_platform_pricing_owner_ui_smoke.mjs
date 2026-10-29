import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';

const root = path.resolve(import.meta.dirname, '..');
const chrome = [process.env.CHROME, 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome'].find(value => value && fs.existsSync(value));
assert.ok(chrome, 'Chromium required');
const server = await createServer({ root, configFile: false, logLevel: 'error', plugins: [react()], css: { postcss: { plugins: [] } }, server: { host: '127.0.0.1', port: 0 } });
await server.listen();
let browser;
try {
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  for (const storageAvailable of [true, false]) {
    const page = await browser.newPage(), errors = [];
    page.on('pageerror', error => errors.push(error.message));
    if (!storageAvailable) await page.evaluateOnNewDocument(() => { Object.defineProperty(window, 'sessionStorage', { get() { throw new Error('CONTROLLED_STORAGE_UNAVAILABLE'); } }); });
    await page.goto(`http://127.0.0.1:${server.httpServer.address().port}/tests/fixtures/account-platform-pricing-owner.html?section=models&model_id=22222222-2222-4222-8222-222222222222&model_detail_tab=pricing&model_price_manage=1`, { waitUntil: 'domcontentloaded', timeout: 60000 });
    await page.waitForSelector('.adm-model-pricing-editor .adm-pricing-workspace');
    await page.click('.adm-sidebar [data-section=products]');
    await page.waitForSelector('[data-workspace=products]:not([hidden]) [data-workspace=prices]');
    const click = async (selector, label) => {
      const handle = await page.evaluateHandle(({ selector, label }) => [...document.querySelectorAll(selector)].find(node => node.textContent.trim() === label && node.getClientRects().length && !node.disabled), { selector, label });
      assert.ok(handle.asElement(), label); await handle.asElement().click(); await handle.dispose();
    };
    await click('[data-workspace=products] button', await page.evaluate(() => probe.labels.createUnitVersion));
    await page.type('[data-workspace=products] .adm-pricing-unit-form input', 'CONTROLLED_UNIT');
    await page.$eval('[data-workspace=products] .adm-pricing-unit-form', form => form.requestSubmit());
    await page.waitForFunction(() => probe.calls.length === 1 && document.querySelector('[data-workspace=products]').textContent.includes(probe.labels.resultUnknown));
    const original = await page.evaluate(() => probe.calls[0]);
    await page.click('.adm-sidebar [data-section=models]');
    await page.waitForSelector('[data-workspace=management]:not([hidden]) .adm-model-pricing-editor');
    await page.waitForFunction(() => document.querySelector('.adm-model-pricing-editor').textContent.includes(probe.labels.resultUnknown));
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('.adm-model-pricing-editor button')].find(node => node.textContent.trim() === probe.labels.createDraft)?.disabled), true);
    if (storageAvailable) assert.deepEqual(await page.evaluate(() => JSON.parse(sessionStorage.getItem(Object.keys(sessionStorage).find(key => key.includes('pricing-unknown'))))), { phase: 'DISPATCHED', kind: 'UNIT_CREATE', input: original.input, idempotencyKey: original.key, beforeIDs: ['44444444-4444-4444-8444-444444444444'] });
    await click('.adm-model-pricing-editor button', await page.evaluate(() => probe.labels.verifyResult));
    await page.waitForFunction(() => probe.calls.length === 2 && document.querySelector('.adm-model-pricing-editor').textContent.includes(probe.labels.recoveryConfirmed));
    assert.deepEqual(await page.evaluate(() => probe.calls[1]), original, 'another presenter must recover the original command, not create a new one');
    assert.deepEqual(errors, []);
    console.log(`PASS pricing sibling owners retain original unknown identity and recover explicitly; optional storage ${storageAvailable ? 'available' : 'unavailable'}`);
    await page.close();
  }
  console.log('RESULT 2 PASS 0 FAIL 0 SKIP');
} finally { await browser?.close(); await server.close(); }
