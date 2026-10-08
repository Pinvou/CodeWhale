import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; order presentation validation not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-order-ui-'));
let vite; let browser; let passed = 0;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  for (const language of ['zh', 'en', 'ja']) {
    const page = await browser.newPage(); page.setDefaultTimeout(10000);
    const errors = []; page.on('pageerror', error => errors.push(error.message));
    await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f02-ui.html`);
    await page.waitForFunction(() => window.f02 && document.querySelector('form select'));
    await page.select('[aria-label=fixture-language]', language);
    await page.evaluate(() => {
      const original = globalThis.fetch;
      window.controlledOrderNow = Date.parse('2026-09-01T00:00:00Z');
      Date.now = () => window.controlledOrderNow;
      const item = { order_id: window.f02.id, product_id: 'personal-base', display_name_en: 'Controlled product', amount_fen: '100', status: 'PENDING', close_status: 'OPEN', created_at: '2026-08-31T00:00:00Z', paid_at: '2026-08-31T01:00:00Z', expires_at: '2026-09-01T00:10:00Z', credited_at: '2026-08-01T00:00:00Z', valid_until: '2026-08-31T00:00:00Z', payment_url: 'https://payments.example/pay', qr_data: 'data:image/png;base64,aGVsbG8=', qr_scan_apps: ['ALIPAY', ' alipay ', 'WECHAT'] };
      globalThis.fetch = async (url, options = {}) => {
        if (url.endsWith('/v1/recharge/orders') && (options.method || 'GET') === 'GET') return new Response(JSON.stringify({ items: [item] }), { status: 200 });
        if (url.endsWith(`/v1/recharge/orders/${window.f02.id}`)) return new Response(JSON.stringify(item), { status: 200 });
        return original(url, options);
      };
      const text = window.f02.languageCopy().refresh;
      [...document.querySelectorAll('button')].find(button => button.textContent.trim() === text && !button.disabled).click();
    });
    await page.waitForFunction(() => document.querySelector('tbody')?.textContent.includes('2026-08-31T01:00:00Z'));
    await page.evaluate(() => { const text = window.f02.languageCopy().details; [...document.querySelectorAll('button')].find(button => button.textContent.trim() === text && !button.disabled).click(); });
    await page.waitForSelector('article img');
    assert.ok(await page.evaluate(() => { const text = document.querySelector('article').textContent; const copy = window.f02.languageCopy(); return text.includes(copy.durationDays(30)) && text.includes(copy.scanToPayWithApps([copy.channels.ALIPAY, copy.channels.WECHAT])); }));
    await page.waitForFunction(() => document.querySelector('article').textContent.includes(window.f02.languageCopy().countdown(600)));
    await page.evaluate(() => { window.controlledOrderNow += 600000; });
    await page.waitForFunction(() => !document.querySelector('article img, article a') && ![...document.querySelectorAll('article button')].some(button => button.textContent.trim() === window.f02.languageCopy().close));
    assert.deepEqual(errors, []); passed++; console.log(`PASS ${language} actual order facts, provider prompt and expiry actions`);
    await page.close();
  }
  console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
} catch (error) { console.error(`FAIL after ${passed} PASS: ${error.message}`); process.exitCode = 1; }
finally { if (browser) await browser.close(); if (vite) await vite.close(); if (fs.existsSync(profile)) fs.rmSync(profile, { recursive: true, force: true }); }
