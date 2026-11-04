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
assert.ok(chrome, 'Browser unavailable; direct repair UI validation not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-direct-repair-'));
let vite; let browser;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage(); page.setDefaultTimeout(10000);
  const errors = []; page.on('pageerror', error => errors.push(error.message)); page.on('dialog', dialog => dialog.accept());
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f02-ui.html`;
  const click = async key => {
    assert.ok(await page.evaluate(key => {
      const text = window.f02.languageCopy()[key]; const button = [...document.querySelectorAll('button')].find(item => item.textContent.trim() === text && !item.disabled);
      if (!button) return false; button.click(); return true;
    }, key), `Unavailable direct repair action: ${key}`);
  };
  const settled = async () => page.waitForFunction(() => ![...document.querySelectorAll('[role=status]')].some(item => item.textContent === window.f02.languageCopy().loading));
  const prepare = async () => {
    await page.goto(url); await page.waitForFunction(() => window.f02);
    await page.evaluate(() => {
      window.directRepairProbe = { calls: [], effects: 0, committed: false, responses: [0, 409, 503, 200] };
      const original = window.fetch;
      window.fetch = async (url, options = {}) => {
        const probe = window.directRepairProbe; const target = String(url);
        const order = { order_id: window.f02.id, amount_fen: '100', status: probe.committed ? 'PAID' : 'MANUAL_REVIEW', fulfillment_status: probe.committed ? 'CREDITED' : 'MANUAL_REVIEW' };
        if (target.includes('/payment-orders?')) return new Response(JSON.stringify({ items: [order] }), { status: 200 });
        if (target.endsWith('/admin/v1/overview')) return new Response(JSON.stringify({ payment_capabilities: { normal_refund_creation_enabled: false } }), { status: 200 });
        if (target.endsWith(`/payment-orders/${window.f02.id}`)) return new Response(JSON.stringify({ ...order, transactions: [], entitlement_packages: [] }), { status: 200 });
        if (!target.endsWith('/repair')) return original(url, options);
        probe.calls.push({ url: target, key: options.headers['Idempotency-Key'], body: options.body });
        const status = probe.responses.shift() ?? 200;
        if (!probe.committed) { probe.committed = true; probe.effects++; }
        if (status === 0) throw new TypeError('controlled committed direct repair ACK lost');
        return new Response(JSON.stringify(status === 200 ? { ...order, status: 'PAID', fulfillment_status: 'CREDITED' } : { code: 'CONFLICT' }), { status });
      };
      window.f02.render('admin');
    });
    await page.waitForSelector('tbody tr'); await settled();
  };
  await prepare(); await click('details'); await page.waitForSelector('article textarea'); await page.type('article textarea', 'Controlled original repair facts');
  await click('orderRepair'); await settled(); await page.waitForSelector('[role=alert]');
  await page.select('[aria-label=fixture-language]', 'ja');
  await click('wallet'); await settled(); await click('orders'); await settled();
  for (const count of [2, 3, 4]) {
    await click('retry'); await page.waitForFunction(count => window.directRepairProbe.calls.length === count, {}, count); await settled();
    if (count < 4) assert.equal(await page.$eval('[role=alert]', item => item.textContent), await page.evaluate(() => window.f02.languageCopy().unknown));
  }
  const probe = await page.evaluate(() => window.directRepairProbe);
  assert.equal(probe.effects, 1); assert.equal(probe.calls.length, 4); assert.match(probe.calls[0].key, /^finance-/);
  assert.ok(probe.calls.every(item => item.key === probe.calls[0].key && item.body === probe.calls[0].body));
  assert.deepEqual(JSON.parse(probe.calls[0].body), { reason: 'Controlled original repair facts', command_protocol: 1 });
  assert.equal(await page.$('[role=alert]'), null);
  console.log('PASS actual Payments hook/client lost ACK and 409/503 retain original repair key/facts across tab/language with refund disabled and no targets; exact ACK resolves');

  await prepare(); await click('details'); await page.waitForSelector('article textarea'); await page.type('article textarea', 'Controlled original session repair'); await click('orderRepair'); await settled();
  await page.evaluate(() => window.f02.retireAdminSession()); await settled();
  assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].some(item => item.textContent.trim() === window.f02.languageCopy().retry)), false);
  assert.equal(await page.evaluate(() => window.directRepairProbe.calls.length), 1);
  await page.evaluate(() => window.f02.switchIdentity()); await settled();
  assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].some(item => item.textContent.trim() === window.f02.languageCopy().retry)), false);
  assert.equal(await page.evaluate(() => window.directRepairProbe.calls.length), 1);
  console.log('PASS actual Payments retained repair is isolated from a retired session and different owner with zero automatic replay');

  await page.goto(url); await page.waitForFunction(() => window.f02);
  await page.evaluate(async () => { const module = await import(new URL('/tests/fixtures/direct-repair-legacy.jsx', globalThis.location.origin).href); module.beginLegacyDirectRepairProbe(); });
  await page.waitForFunction(() => [...document.querySelectorAll('[data-legacy-direct-repair] button')].some(item => item.textContent.trim() === window.f02.languageCopy().retry && !item.disabled));
  await page.evaluate(() => [...document.querySelectorAll('[data-legacy-direct-repair] button')].find(item => item.textContent.trim() === window.f02.languageCopy().retry && !item.disabled).click());
  await page.waitForFunction(() => window.legacyDirectRepairProbe.recoveryArguments.length === 1 && [...document.querySelectorAll('[data-legacy-direct-repair] button')].some(item => item.textContent.trim() === window.f02.languageCopy().retry && !item.disabled));
  const legacy = await page.evaluate(() => window.legacyDirectRepairProbe);
  assert.equal(legacy.legacyHasMarker, false); assert.equal(legacy.recoveryArguments[0][2], legacy.originalKey); assert.equal(legacy.recoveryArguments[0][3], null); assert.deepEqual(legacy.writes, []); assert.deepEqual(errors, []);
  console.log('PASS actual Payments legacy UNKNOWN keeps original key and missing marker, zero retry writes and UNKNOWN');
} finally {
  if (browser) await browser.close(); if (vite) await vite.close();
  assert.equal(path.dirname(path.resolve(profile)), path.resolve(os.tmpdir())); assert.ok(path.basename(profile).startsWith('pinvou-direct-repair-'));
  fs.rmSync(profile, { recursive: true, force: true });
}
