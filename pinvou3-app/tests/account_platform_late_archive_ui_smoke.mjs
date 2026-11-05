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
assert.ok(chrome, 'Browser unavailable; archive UI validation not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-late-archive-'));
let vite; let browser;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage(); page.setDefaultTimeout(10000);
  const errors = []; page.on('pageerror', error => errors.push(error.message)); page.on('dialog', dialog => dialog.accept());
  await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f02-ui.html`);
  await page.waitForFunction(() => window.f02);
  await page.evaluate(() => {
    window.f02.render('admin'); const original = window.fetch;
    window.archiveProbe = { calls: [], receipt: null };
    window.fetch = async (url, options = {}) => {
      if (!String(url).endsWith('/archive-command')) return original(url, options);
      const probe = window.archiveProbe;
      probe.calls.push({ url: String(url), key: options.headers['Idempotency-Key'], body: options.body });
      if (probe.calls.length === 1) {
        probe.receipt = { case_id: window.f02.id, review_status: 'ARCHIVED_CONFIRMED_UNPAID' };
        throw new TypeError('controlled lost archive acknowledgement');
      }
      // A generic terminal conflict cannot prove the original invocation.
      if (probe.calls.length === 2) return new Response(JSON.stringify({ code: 'CONFLICT' }), { status: 409 });
      return new Response(JSON.stringify(probe.receipt), { status: 200 });
    };
  });
  const click = async key => {
    const clicked = await page.evaluate(key => {
      const text = window.f02.languageCopy()[key]; const button = [...document.querySelectorAll('button')].find(item => item.textContent.trim() === text && !item.disabled);
      if (!button) return false; button.click(); return true;
    }, key); assert.ok(clicked, `Unavailable archive UI action: ${key}`);
  };
  const unknownReady = async calls => page.waitForFunction(calls => window.archiveProbe.calls.length === calls && [...document.querySelectorAll('button')].some(item => item.textContent.trim() === window.f02.languageCopy().retry && !item.disabled), {}, calls);
  await page.waitForSelector('tbody tr'); await click('sla'); await page.waitForSelector('section tbody tr'); await click('details'); await page.waitForSelector('article textarea');
  await click('archive'); await unknownReady(1); await click('retry'); await unknownReady(2);
  assert.equal(await page.$eval('article textarea', element => element.disabled), true);
  await click('retry');
  await page.waitForFunction(() => window.archiveProbe.calls.length === 3 && ![...document.querySelectorAll('button')].some(item => item.textContent.trim() === window.f02.languageCopy().retry));
  const probe = await page.evaluate(() => window.archiveProbe);
  assert.match(probe.calls[0].key, /^finance-/); assert.deepEqual(probe.calls[1], probe.calls[0]); assert.deepEqual(probe.calls[2], probe.calls[0]); assert.deepEqual(JSON.parse(probe.calls[0].body), {}); assert.deepEqual(errors, []);
  console.log('PASS actual React archive UI/hook/client retain original invocation after lost ACK and 409; exact receipt resolves unknown');

  await page.evaluate(async () => {
    const module = await import(new URL('/tests/fixtures/late-review-protocol-legacy.jsx', globalThis.location.origin).href);
    module.beginLegacyReviewProtocolProbe('archive');
  });
  await page.waitForFunction(() => [...document.querySelectorAll('[data-legacy-review-protocol] button')].some(button => button.textContent.trim() === window.f02.languageCopy().retry && !button.disabled));
  await page.evaluate(() => [...document.querySelectorAll('[data-legacy-review-protocol] button')].find(button => button.textContent.trim() === window.f02.languageCopy().retry && !button.disabled).click());
  await page.waitForFunction(() => window.legacyReviewProtocolProbe.recoveryArguments.length === 1 && [...document.querySelectorAll('[data-legacy-review-protocol] button')].some(button => button.textContent.trim() === window.f02.languageCopy().retry && !button.disabled));
  const legacy = await page.evaluate(() => window.legacyReviewProtocolProbe);
  assert.equal(legacy.legacyHasMarker, false); assert.equal(legacy.recoveryArguments[0].at(-1) ?? null, null); // JSON serialization of undefined array slot.
  assert.equal(legacy.recoveryArguments[0].at(-2), legacy.originalKey); assert.deepEqual(legacy.writes, []);
  console.log('PASS actual React archive legacy unknown keeps original key/facts, missing protocol, zero retry writes and unknown');
} finally {
  if (browser) await browser.close(); if (vite) await vite.close();
  assert.equal(path.dirname(path.resolve(profile)), path.resolve(os.tmpdir())); assert.ok(path.basename(profile).startsWith('pinvou-late-archive-'));
  fs.rmSync(profile, { recursive: true, force: true });
}
