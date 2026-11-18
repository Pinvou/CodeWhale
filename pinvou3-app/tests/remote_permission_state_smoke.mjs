import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';

// These are captured actual Rust authority/production wire encoder outputs.
// Closed states test defensive consumption, not normal list reachability.
const producer = JSON.parse(fs.readFileSync(process.env.PINVOU_PERMISSION_PROJECTION_FILE
  || new URL('./fixtures/remote_permission_states.json', import.meta.url), 'utf8'));
assert.deepEqual(producer.permissions.map(item => item.state), ['pending', 'allowed', 'denied', 'expired']);
const chrome = process.env.CHROME || [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/chromium', '/usr/bin/google-chrome',
].find(candidate => fs.existsSync(candidate));
assert.ok(chrome, 'A test Chrome/Chromium executable is required');
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
let browser, vite;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()] });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/remote_permission_state_smoke.html`;
  async function load(lang) {
    errors.length = 0;
    await page.goto(`${url}?lang=${lang}`, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => window.dispatchFixture?.permissionRequests?.length === 1);
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
        await page.waitForFunction(() => window.dispatchFixture.permissionRequests.length === 3);
        await page.evaluate(() => window.dispatchFixture.permissionRequests[2].resolve({ permissions: [] }));
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
  const successor = { ...producer.permissions[0], permission_request_id: 'approval-successor' };
  for (const trigger of ['journal', 'renewal']) {
    await load('zh');
    await supply(0, producer.permissions[0]);
    await page.click('[data-testid="chat-remote-permission-allow"]');
    await page.waitForFunction(() => window.dispatchFixture.permissionRequests.length === 2);
    await page.evaluate(({ trigger, successor }) => {
      if (trigger === 'journal') window.dispatchFixture.projectPendingApproval(successor);
      else window.dispatchFixture.renewPermissionCapability();
    }, { trigger, successor });
    await page.waitForFunction(() => window.dispatchFixture.permissionRequests.length === 3);
    await page.evaluate(() => window.dispatchFixture.permissionRequests[1].resolve({ ok: true }));
    await page.waitForFunction(() => window.dispatchFixture.permissionRequests.length === 4);
    const ids = await page.evaluate(() => window.dispatchFixture.permissionRequests.slice(2).map(item => item.args.requestId));
    assert.notEqual(ids[0], ids[1], `${trigger}: post-ack refresh uses a new RPC`);
    await page.evaluate(({ trigger, pending, successor }) => {
      // A renewed snapshot can still have read the old pending card before
      // the decision committed; a successor journal is already post-commit.
      window.dispatchFixture.permissionRequests[2].resolve({ permissions: [trigger === 'renewal' ? pending : successor] });
    }, { trigger, pending: producer.permissions[0], successor });
    await supply(3, successor);
    assert.equal(await page.evaluate(() => document.querySelectorAll('[data-testid="chat-remote-permission-allow"]').length), 1);
    assert.equal(await page.evaluate(() => window.dispatchFixture.permissionState.desktopRemoteControl.remotePermissions[0].permissionRequestId), successor.permission_request_id);
    assert.deepEqual(errors, []);
  }
  await load('zh');
  await page.evaluate(() => window.dispatchFixture.renewPermissionCapability());
  await page.waitForFunction(() => window.dispatchFixture.permissionRequests.length === 2);
  await supply(1, successor);
  await page.evaluate(entry => window.dispatchFixture.permissionRequests[0].resolve({ permissions: [entry] }), producer.permissions[1]);
  await page.evaluate(() => new Promise(requestAnimationFrame));
  assert.equal(await page.evaluate(() => window.dispatchFixture.permissionState.desktopRemoteControl.remotePermissions[0].permissionRequestId), successor.permission_request_id);
  for (const leave of ['lease', 'unmount']) {
    await load('zh');
    await supply(0, producer.permissions[0]);
    await page.click('[data-testid="chat-remote-permission-allow"]');
    await page.waitForFunction(() => window.dispatchFixture.permissionRequests.length === 2);
    if (leave === 'lease') {
      await page.evaluate(() => window.dispatchFixture.select('remote-a', 2));
      await page.waitForFunction(() => window.dispatchFixture.permissionRequests.length === 3);
      await supply(2, successor);
    } else await page.evaluate(() => window.dispatchFixture.unmount());
    await page.evaluate(() => window.dispatchFixture.permissionRequests[1].resolve({ ok: true }));
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await page.evaluate(() => window.dispatchFixture.permissionRequests.length), leave === 'lease' ? 3 : 2,
      `${leave}: old acknowledgement cannot refresh another owner`);
    assert.deepEqual(errors, []);
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
  console.log('remote_permission_state_smoke: Rust projections through desktop bridge to mounted ChatView; 3 languages, successor refresh, capability renewal and stale owners passed');
} finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
}
