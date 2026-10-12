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
assert.ok(chrome, 'Browser unavailable; real React validation was not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-f02-dispatch-'));
let vite; let browser; let passed = 0; let failed = 0;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage(); page.setDefaultTimeout(5000);
  const errors = []; page.on('pageerror', error => errors.push(error.message));
  const fresh = async (kind = 'hook', language = 'en') => {
    errors.length = 0;
    await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f02-dispatch.html`);
    await page.waitForFunction(() => window.f02Dispatch?.operation);
    await page.evaluate((kind, language) => window.f02Dispatch.render(kind, language), kind, language);
    await page.waitForFunction(kind => kind === 'hook' ? window.f02Dispatch.operation && !window.f02Dispatch.operation.busy : kind === 'approvals' ? document.querySelectorAll('tbody tr').length === 2 : document.querySelector('form textarea'), {}, kind);
  };
  const run = async (name, fn) => { try { await fn(); assert.deepEqual(errors, []); passed++; console.log(`PASS ${name}`); } catch (error) { failed++; console.error(`FAIL ${name}: ${error.stack}`); } };
  for (const language of ['zh', 'en', 'ja']) {
    await run(`${language}: mismatched approval detail is visible as an error without replacing the known detail`, async () => {
      await fresh('approvals', language);
      await page.$$eval('tbody button', buttons => buttons[0].click());
      await page.waitForFunction(() => document.querySelector('article h4')?.textContent === window.f02Dispatch.id);
      await page.waitForFunction(() => !document.querySelector('[role=status]'));
      await page.$$eval('tbody button', buttons => buttons[1].click());
      await page.waitForSelector('[role=alert]');
      assert.equal(await page.$eval('[role=alert]', node => node.textContent), await page.evaluate(() => window.f02Dispatch.copy().unavailable));
      assert.equal(await page.$eval('article h4', node => node.textContent), await page.evaluate(() => window.f02Dispatch.id));
      assert.equal(await page.evaluate(() => document.body.textContent.includes('33333333-3333-4333-8333-333333333333')), false);
      assert.equal(await page.evaluate(() => window.f02Dispatch.calls.filter(call => call.method !== 'GET').length), 0);
    });
    await run(`${language}: readonly 403 distinguishes permissions from 503 without retaining a mutation`, async () => {
      await fresh('hook', language);
      await page.evaluate(async () => { const s = window.f02Dispatch; s.readStatus = 403; await s.operation.read(() => s.client.wallets()); });
      await page.waitForFunction(() => window.f02Dispatch.operation.error);
      assert.equal(await page.$eval('[role=alert]', node => node.textContent), await page.evaluate(() => window.f02Dispatch.copy().permissions));
      assert.equal(await page.$eval('[data-unknown]', node => node.textContent), 'false');
      await page.evaluate(async () => { const s = window.f02Dispatch; s.readStatus = 503; await s.operation.read(() => s.client.wallets()); });
      await page.waitForFunction(() => window.f02Dispatch.operation.error === window.f02Dispatch.copy().unavailable);
      assert.equal(await page.$eval('[data-unknown]', node => node.textContent), 'false');
      assert.equal(await page.evaluate(() => window.f02Dispatch.calls.filter(call => call.method !== 'GET').length), 0);
    });
    await run(`${language}: local product int64 rejection unlocks the real hook`, async () => {
      await fresh('hook', language);
      await page.evaluate(async () => {
        const s = window.f02Dispatch;
        await s.operation.mutate({ base_points: '1.5' }, (facts, key) => s.client.createRechargeProduct(facts, key));
      });
      await page.waitForFunction(() => window.f02Dispatch.operation.error);
      assert.equal(await page.$eval('[data-unknown]', node => node.textContent), 'false');
      assert.equal(await page.evaluate(() => window.f02Dispatch.calls.length), 0);
      assert.ok(await page.$eval('[role=alert]', node => node.textContent.length > 0));
    });
    await run(`${language}: invalid wallet UUID remains editable with no write`, async () => {
      await fresh('wallet', language);
      await page.type('form input:not([pattern])', 'not-a-wallet');
      await page.type('form input[pattern]', '100');
      await page.type('form textarea', 'Controlled invalid wallet reason');
      await page.$eval('form', form => form.requestSubmit());
      await page.waitForFunction(() => document.querySelector('[role=alert]'));
      assert.equal(await page.$eval('form input:not([pattern])', node => node.disabled), false);
      assert.equal(await page.evaluate(() => window.f02Dispatch.calls.filter(call => call.method === 'POST').length), 0);
    });
  }
  await run('GET-only control mismatch is refreshable, not UNKNOWN', async () => {
    await fresh();
    await page.evaluate(async () => {
      const s = window.f02Dispatch;
      await s.operation.mutate({ detail: { approval_request_id: s.id, version: 1, action_control_version: 1 } }, facts => s.client.decideGovernedApproval(s.id, facts.detail, 'APPROVE', 'Controlled reason'));
    });
    await page.waitForFunction(() => window.f02Dispatch.operation.error);
    assert.equal(await page.$eval('[data-unknown]', node => node.textContent), 'false');
    assert.equal(await page.evaluate(() => window.f02Dispatch.calls.filter(call => call.method === 'POST').length), 0);
  });
  await run('network-ambiguous POST plus replay denial retains original key and facts', async () => {
    await fresh();
    const result = await page.evaluate(async () => {
      const s = window.f02Dispatch; const keys = []; const factsSeen = [];
      s.transportFailure = true;
      await s.operation.mutate({ base_points: '100' }, (facts, key) => { keys.push(key); factsSeen.push(facts); return s.client.createRechargeProduct(facts, key); });
      await new Promise(resolve => setTimeout(resolve, 0));
      await s.operation.mutate({ base_points: '999' }, (facts, key) => { keys.push(key); factsSeen.push(facts); throw Object.assign(new Error('Controlled denial'), { status: 403 }); });
      return { keys, values: factsSeen.map(facts => facts.base_points) };
    });
    await page.waitForFunction(() => window.f02Dispatch.operation.unknown);
    assert.equal(result.keys[0], result.keys[1]); assert.deepEqual(result.values, ['100', '100']);
  });
  await run('post-POST invalid ACK stays UNKNOWN', async () => {
    await fresh();
    await page.evaluate(async () => {
      const s = window.f02Dispatch; s.badAck = true;
      await s.operation.mutate({ detail: { approval_request_id: s.id, version: 1, action_control_version: 2 } }, facts => s.client.decideGovernedApproval(s.id, facts.detail, 'APPROVE', 'Controlled reason'));
    });
    await page.waitForFunction(() => window.f02Dispatch.operation.unknown);
    assert.equal(await page.evaluate(() => window.f02Dispatch.calls.filter(call => call.method === 'POST').length), 1);
  });
  await run('created adjustment survives incomplete history and recovery avoids a duplicate create', async () => {
    await fresh('wallet');
    await page.type('form input:not([pattern])', '11111111-1111-4111-8111-111111111111');
    await page.type('form input[pattern]', '100'); await page.type('form textarea', 'Controlled original wallet reason');
    await page.$eval('form', form => form.requestSubmit());
    await page.waitForFunction(() => window.f02Dispatch.created && document.querySelector('[role=alert]') && ![...document.querySelectorAll('[role=status]')].some(node => node.textContent.includes('loading') || node.textContent.includes('Loading')));
    await page.evaluate(() => { window.f02Dispatch.historyIncomplete = false; });
    await page.$eval('form', form => form.requestSubmit());
    await page.waitForFunction(() => window.f02Dispatch.submitted);
    assert.equal(await page.evaluate(() => window.f02Dispatch.calls.filter(call => call.url.endsWith('/adjustments') && call.method === 'POST').length), 1);
    assert.ok(await page.evaluate(() => window.f02Dispatch.calls.some(call => call.url.includes('/adjustments?'))));
    assert.ok(await page.evaluate(() => window.f02Dispatch.calls.filter(call => call.url.endsWith('/api/admin/approvals/requests') && call.method === 'POST').every(call => JSON.parse(call.body).target_resource_id === window.f02Dispatch.created.adjustment_id)));
  });
  console.log(`RESULT ${passed} PASS ${failed} FAIL 0 SKIP`);
  if (failed) process.exitCode = 1;
} finally {
  await browser?.close(); await vite?.close(); assert.equal(path.dirname(profile), path.resolve(os.tmpdir())); fs.rmSync(profile, { recursive: true, force: true });
}
