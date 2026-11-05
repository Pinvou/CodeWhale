import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || ['C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium'].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; provider-blind UI validation not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-provider-blind-'));
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
    window.f02.render('admin');
    const original = window.fetch;
    window.blindProbe = { calls: [], enabled: true, version: 0, receipt: null, body: '' };
    window.fetch = async (url, options = {}) => {
      if (!String(url).endsWith('/provider-blind')) return original(url, options);
      const probe = window.blindProbe;
      probe.calls.push({ key: options.headers['Idempotency-Key'], body: options.body });
      const body = JSON.parse(options.body);
      if (body.command_protocol !== 1 || !options.headers['Idempotency-Key']) return new Response(JSON.stringify({ code: 'INVALID_REQUEST' }), { status: 400 });
      if (probe.calls.length === 1) {
        probe.enabled = false; probe.version++; probe.body = options.body;
        probe.receipt = { case_id: window.f02.id, review_status: 'PROVIDER_BLIND', blind_reason: body.reason };
        throw new TypeError('controlled committed provider-blind ACK lost');
      }
      if (probe.calls.length === 2) {
        if (options.body !== probe.body || options.headers['Idempotency-Key'] !== probe.calls[0].key) return new Response(JSON.stringify({ code: 'CONFLICT' }), { status: 409 });
        return new Response(JSON.stringify(probe.receipt), { status: 200 });
      }
      probe.enabled = false; probe.version++;
      return new Response(JSON.stringify({ case_id: window.f02.id, review_status: 'PROVIDER_BLIND' }), { status: 200 });
    };
  });
  const click = async key => {
    assert.ok(await page.evaluate(key => {
      const label = window.f02.languageCopy()[key];
      const button = [...document.querySelectorAll('button')].find(item => item.textContent.trim() === label && !item.disabled);
      if (!button) return false; button.click(); return true;
    }, key), `Unavailable provider-blind action ${key}`);
  };
  await page.waitForSelector('tbody tr'); await click('sla'); await page.waitForSelector('section tbody tr'); await click('details');
  await page.waitForSelector('article textarea'); await page.type('article textarea', 'controlled original blind observation'); await click('escalate');
  await page.waitForFunction(() => window.blindProbe.calls.length === 1 && [...document.querySelectorAll('button')].some(item => item.textContent.trim() === window.f02.languageCopy().retry && !item.disabled));
  assert.equal(await page.$eval('article textarea', element => element.disabled), true);
  await page.evaluate(() => { window.blindProbe.enabled = true; window.blindProbe.version++; });
  await click('retry');
  await page.waitForFunction(() => window.blindProbe.calls.length === 2 && !document.querySelector('article textarea').disabled);
  const replay = await page.evaluate(() => window.blindProbe);
  assert.deepEqual(replay.calls[0], replay.calls[1]); assert.equal(replay.enabled, true); assert.equal(replay.version, 2);
  await page.type('article textarea', 'controlled distinct explicit blind command'); await click('escalate');
  await page.waitForFunction(() => window.blindProbe.calls.length === 3);
  const fresh = await page.evaluate(() => window.blindProbe);
  assert.notEqual(fresh.calls[0].key, fresh.calls[2].key); assert.equal(fresh.enabled, false); assert.equal(fresh.version, 3);
  assert.deepEqual(errors, []);
  console.log('PASS real React/client retains provider-blind facts/key after lost ACK, preserves later recovery, and permits explicit new command');
} finally {
  await browser?.close(); await vite?.close();
  const owned = path.resolve(profile); assert.equal(path.dirname(owned), path.resolve(os.tmpdir())); assert.ok(path.basename(owned).startsWith('pinvou-provider-blind-'));
  if (fs.existsSync(owned)) fs.rmSync(owned, { recursive: true, force: true });
}
