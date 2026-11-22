import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome'].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; schedule UI validation not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-late-schedule-'));
let vite; let browser;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage(); page.setDefaultTimeout(10000);
  const errors = []; page.on('pageerror', error => errors.push(error.message)); page.on('dialog', dialog => dialog.accept());
  for (const changeTimezone of [false, true]) {
    await page.emulateTimezone('UTC');
    errors.length = 0;
    await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f02-ui.html`);
    await page.waitForFunction(() => window.f02);
    await page.evaluate(() => {
      window.f02.render('admin');
      const original = window.fetch;
      const now = Date.now();
      window.scheduleProbe = { calls: [], plan: 'initial', originalReceipt: null, originalBody: '', deltas: [], zones: [], initialLocal: new Date(now + 72 * 60 * 60 * 1000).toISOString().slice(0, 16), laterUTC: new Date(now + 96 * 60 * 60 * 1000).toISOString(), changedLocal: new Date(now + 120 * 60 * 60 * 1000).toISOString().slice(0, 16) };
      window.fetch = async (url, options = {}) => {
        if (!String(url).endsWith('/schedule')) return original(url, options);
        const probe = window.scheduleProbe;
        probe.calls.push({ key: options.headers['Idempotency-Key'], body: options.body });
        const delta = Date.parse(JSON.parse(options.body).next_review_at) - Date.now();
        probe.deltas.push(delta); probe.zones.push(Intl.DateTimeFormat().resolvedOptions().timeZone);
        if (!(delta > 0 && delta <= 8 * 24 * 60 * 60 * 1000)) return new Response(JSON.stringify({ code: 'CONFLICT' }), { status: 409 });
        if (probe.calls.length === 1) {
          probe.originalBody = options.body;
          probe.originalReceipt = { case_id: window.f02.id, next_review_at: JSON.parse(options.body).next_review_at };
          probe.plan = probe.originalReceipt.next_review_at;
          throw new TypeError('controlled lost schedule acknowledgement');
        }
        if (options.body !== probe.originalBody) return new Response(JSON.stringify({ code: 'CONFLICT' }), { status: 409 });
        return new Response(JSON.stringify(probe.originalReceipt), { status: 200 });
      };
    });
    const click = async key => {
      const clicked = await page.evaluate(key => {
        const text = window.f02.languageCopy()[key];
        const button = [...document.querySelectorAll('button')].find(item => item.textContent.trim() === text && !item.disabled);
        if (!button) return false; button.click(); return true;
      }, key);
      assert.ok(clicked, `Unavailable schedule UI action: ${key}`);
    };
    await page.waitForSelector('tbody tr'); await click('sla'); await page.waitForSelector('section tbody tr'); await click('details');
    await page.waitForSelector('input[type=datetime-local]');
    await page.type('article textarea', 'controlled original schedule reason');
    await page.evaluate(() => {
      const input = document.querySelector('input[type=datetime-local]');
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, window.scheduleProbe.initialLocal);
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    await click('schedule');
    await page.waitForFunction(() => window.scheduleProbe.calls.length === 1 && [...document.querySelectorAll('button')].some(item => item.textContent.trim() === window.f02.languageCopy().retry && !item.disabled));
    assert.equal(await page.$eval('article textarea', element => element.disabled), true);
    await page.evaluate(() => {
      // The receiver's later plan represents a separately committed logical call.
      // PostgreSQL behavior is exercised separately by the permanent Go root.
      window.scheduleProbe.plan = window.scheduleProbe.laterUTC;
      const input = document.querySelector('input[type=datetime-local]');
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, window.scheduleProbe.changedLocal);
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    if (changeTimezone) await page.emulateTimezone('Asia/Tokyo');
    assert.equal(await page.$eval('input[type=datetime-local]', element => element.value), await page.evaluate(() => window.scheduleProbe.changedLocal), 'time-zone change mutated the local form display');
    await click('retry');
    await page.waitForFunction(() => window.scheduleProbe.calls.length === 2);
    const probe = await page.evaluate(() => window.scheduleProbe);
    assert.match(probe.calls[0].key, /^finance-/);
    assert.deepEqual(probe.calls[1], probe.calls[0]);
    assert.equal(JSON.parse(probe.calls[1].body).reason, 'controlled original schedule reason');
    assert.equal(probe.plan, probe.laterUTC);
    assert.equal(JSON.parse(probe.calls[0].body).next_review_at, `${probe.initialLocal}:00.000Z`, 'fresh UTC instant differs from the selected local form time in UTC');
    assert.ok(probe.deltas.length === 2 && probe.deltas.every(delta => delta > 0 && delta <= 8 * 24 * 60 * 60 * 1000), 'wire times must remain within the real eight-day admission window');
    assert.deepEqual(probe.zones, changeTimezone ? ['UTC', 'Asia/Tokyo'] : ['UTC', 'UTC']);
    await page.waitForFunction(() => ![...document.querySelectorAll('button')].some(item => item.textContent.trim() === window.f02.languageCopy().retry));
    assert.deepEqual(errors, []);
    console.log(`PASS actual React schedule ${changeTimezone ? 'UTC-to-Tokyo' : 'same-zone'} original UTC/key/body/receipt recovery and local form display`);

  }

  await page.evaluate(async () => {
    const module = await import(new URL('/tests/fixtures/late-review-protocol-legacy.jsx', window.location.origin).href);
    module.beginLegacyReviewProtocolProbe('schedule');
  });
  await page.waitForFunction(() => [...document.querySelectorAll('[data-legacy-review-protocol] button')].some(button => button.textContent.trim() === window.f02.languageCopy().retry && !button.disabled));
  await page.evaluate(() => [...document.querySelectorAll('[data-legacy-review-protocol] button')].find(button => button.textContent.trim() === window.f02.languageCopy().retry && !button.disabled).click());
  await page.waitForFunction(() => window.legacyReviewProtocolProbe.recoveryArguments.length === 1 && [...document.querySelectorAll('[data-legacy-review-protocol] button')].some(button => button.textContent.trim() === window.f02.languageCopy().retry && !button.disabled));
  const legacy = await page.evaluate(() => window.legacyReviewProtocolProbe);
  assert.equal(legacy.legacyHasMarker, false); assert.equal(legacy.recoveryArguments[0].at(-1) ?? null, null); // JSON serialization of undefined array slot.
  assert.equal(legacy.recoveryArguments[0].at(-2), legacy.originalKey); assert.deepEqual(legacy.writes, []);
  console.log('PASS actual React schedule legacy unknown keeps original key/facts, missing protocol, zero retry writes and unknown');
} finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
  assert.equal(path.dirname(path.resolve(profile)), path.resolve(os.tmpdir()));
  assert.ok(path.basename(profile).startsWith('pinvou-late-schedule-'));
  fs.rmSync(profile, { recursive: true, force: true });
}
