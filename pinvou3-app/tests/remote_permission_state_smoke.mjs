import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';

// These are captured actual Rust authority/production wire encoder outputs.
// Closed states test defensive consumption, not normal list reachability.
const producer = JSON.parse(fs.readFileSync(process.env.PINVOU_PERMISSION_PROJECTION_FILE, 'utf8'));
assert.deepEqual(producer.permissions.map(item => item.state), ['pending', 'allowed', 'denied', 'expired']);
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
let browser, vite;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()] });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: process.env.CHROME, headless: true, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/remote_permission_state_smoke.html`;
  async function load(lang) {
    errors.length = 0;
    await page.goto(`${url}?lang=${lang}`, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => window.dispatchFixture?.permissionRequests.length === 1);
    assert.deepEqual(errors, []);
  }
  async function supply(index, entry) {
    await page.evaluate(({ index, entry }) => window.dispatchFixture.permissionRequests[index].resolve({ permissions: [entry] }), { index, entry });
    await page.waitForFunction(id => document.querySelector(`[data-coordination-id="${id}"][role="alert"]`), {}, entry.permission_request_id);
  }
  for (const lang of ['zh', 'en', 'ja']) {
    for (const entry of producer.permissions) {
      await load(lang);
      await supply(0, entry);
      const result = await page.evaluate(() => {
        const card = document.querySelector('[role="alert"][data-coordination-id]');
        const copy = window.dispatchFixture.copy.remoteConversation;
        return { text: card.textContent, buttons: card.querySelectorAll('button').length, copy };
      });
      if (entry.state === 'pending') {
        assert.equal(result.buttons, 2);
        await page.click('[data-testid="chat-remote-permission-allow"]');
        await page.waitForFunction(() => window.dispatchFixture.permissionRequests.length === 2);
        const intent = await page.evaluate(() => window.dispatchFixture.permissionRequests[1].args.intent);
        assert.equal(intent.operation, 'permission_decide');
        assert.equal(intent.permissionRequestId, entry.permission_request_id);
        assert.equal(intent.decision, 'allow');
        await page.evaluate(() => window.dispatchFixture.permissionRequests[1].resolve({ ok: true }));
        await page.waitForFunction(() => !document.querySelector('[role="alert"][data-coordination-id]'));
      } else {
        assert.equal(result.buttons, 0);
        const key = { allowed: 'permissionAllowed', denied: 'permissionDenied', expired: 'permissionTimedOut' }[entry.state];
        assert.ok(result.text.includes(result.copy[key]), `${lang} ${entry.state} truthful label`);
        if (entry.state !== 'expired') assert.equal(result.text.includes(result.copy.permissionTimedOut), false);
      }
      assert.deepEqual(errors, []);
    }
  }
  await load('zh');
  await page.evaluate(() => window.dispatchFixture.select('remote-a', 2));
  await page.waitForFunction(() => window.dispatchFixture.permissionRequests.length === 2);
  await supply(1, producer.permissions[0]);
  await page.evaluate(entry => window.dispatchFixture.permissionRequests[0].resolve({ permissions: [entry] }), producer.permissions[1]);
  await page.evaluate(() => new Promise(requestAnimationFrame));
  const retained = await page.evaluate(() => ({
    mirror: window.dispatchFixture.permissionState.desktopRemoteControl.remotePermissions,
    allowButtons: document.querySelectorAll('[data-testid="chat-remote-permission-allow"]').length,
  }));
  assert.equal(retained.mirror[0].state, 'pending');
  assert.equal(retained.allowButtons, 1, 'late old-lease read cannot replace successor card');
  assert.deepEqual(errors, []);
  console.log('remote_permission_state_smoke: actual Rust projections through desktop bridge to mounted ChatView; 3 languages, decisions, late lease response passed');
} finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
}
