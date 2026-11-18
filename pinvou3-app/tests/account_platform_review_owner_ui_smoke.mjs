import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';
import { findBrowserExecutable } from './helpers/browser-executable.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = findBrowserExecutable();
assert.ok(chrome, 'Browser unavailable; review ownership UI was not validated');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-review-owner-'));
let vite; let browser; let passed = 0;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage(); page.setDefaultTimeout(10000);
  const errors = []; page.on('pageerror', error => errors.push(error.message)); page.on('dialog', dialog => dialog.accept());
  const click = async (key, selector = 'button') => {
    const clicked = await page.evaluate(({ key, selector }) => {
      const text = window.reviewOwnerProbe.copy[key];
      const button = [...document.querySelectorAll(selector)].find(node => node.textContent.trim() === text && !node.disabled);
      if (!button) return false; button.click(); return true;
    }, { key, selector });
    assert.ok(clicked, `Missing enabled review action: ${key}`);
  };
  const enabledAction = key => page.evaluate(key => [...document.querySelectorAll('article button')]
    .some(node => node.textContent.trim() === window.reviewOwnerProbe.copy[key] && !node.disabled), key);
  const exerciseHandling = async anomaly => {
    const reason = 'Controlled ownership UI handling';
    if (anomaly) {
      assert.equal(await enabledAction('resolve'), false, 'empty resolution and reason must not be actionable');
      await page.select('article select', 'DISMISS');
      assert.equal(await enabledAction('resolve'), false, 'resolution without a reason must remain disabled');
      await page.type('article textarea', reason);
    }
    const action = anomaly ? 'resolve' : 'query';
    await page.waitForFunction(key => [...document.querySelectorAll('article button')]
      .some(node => node.textContent.trim() === window.reviewOwnerProbe.copy[key] && !node.disabled), {}, action);
    assert.equal(await enabledAction(action), true);
    await click(action, 'article button');
    if (!anomaly) {
      await page.waitForSelector('[role="alertdialog"]');
      assert.equal(await page.evaluate(() => window.reviewOwnerProbe.calls.filter(call => call.url.endsWith('/query-commands')).length), 0);
      await click('confirmQueryAction', '[role="alertdialog"] button');
      await page.waitForFunction(() => [...document.querySelectorAll('[role="status"]')]
        .some(node => node.textContent === window.reviewOwnerProbe.copy.queryRecorded));
    } else {
      await page.waitForFunction(() => ![...document.querySelectorAll('article button')]
        .some(node => node.textContent.trim() === window.reviewOwnerProbe.copy.resolve));
    }
    const suffix = anomaly ? '/resolve' : '/query-commands';
    const writes = await page.evaluate(suffix => window.reviewOwnerProbe.calls.filter(call => call.method === 'POST' && call.url.endsWith(suffix)), suffix);
    assert.equal(writes.length, 1); assert.ok(writes[0].key);
    assert.ok(writes[0].url.endsWith(`/11111111-1111-4111-8111-111111111111${suffix}`));
    assert.deepEqual(JSON.parse(writes[0].body), anomaly ? { resolution: 'DISMISS', reason } : {
      command_protocol: 1, payment_order_id: '11111111-1111-4111-8111-111111111111',
      provider_id: '44444444-4444-4444-8444-444444444444', provider_version: 'fixture-version-1', merchant_order_no: 'review-owner-order',
    });
    assert.equal(await page.$('[data-retained-finance-command]'), null);
  };
  const hasAction = key => page.evaluate(key => [...document.querySelectorAll('article button')].some(node => node.textContent.trim() === window.reviewOwnerProbe.copy[key]), key);
  for (const language of ['zh', 'en', 'ja']) for (const anomaly of [false, true]) {
    for (const owner of ['other', 'none', 'self']) {
      await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-review-owner.html?${new URLSearchParams({ language, anomaly, owner })}`);
      await page.waitForSelector('tbody tr'); await click('details'); await page.waitForSelector('article');
      assert.equal(await hasAction('claimCase'), owner === 'none');
      if (owner === 'other') {
        const explanation = await page.evaluate(() => window.reviewOwnerProbe.copy.reviewAssignedOther);
        assert.ok(await page.$eval('article', (node, explanation) => node.textContent.includes(explanation), explanation));
        assert.equal(await hasAction(anomaly ? 'resolve' : 'query'), false);
      } else if (owner === 'none') {
        await click('claimCase');
        await page.waitForFunction(() => document.querySelector('article')?.textContent.includes('22222222-2222-4222-8222-222222222222'));
        assert.equal(await hasAction('claimCase'), false);
        assert.equal(await hasAction(anomaly ? 'resolve' : 'query'), true);
      } else assert.equal(await hasAction(anomaly ? 'resolve' : 'query'), true);
      if (owner !== 'other') await exerciseHandling(anomaly);
      assert.deepEqual(errors, []); passed++;
    }
    for (const claim of ['conflict', 'transport']) {
      await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-review-owner.html?${new URLSearchParams({ language, anomaly, owner: 'none', claim })}`);
      await page.waitForSelector('tbody tr'); await click('details'); await page.waitForSelector('article'); await click('claimCase');
      const key = claim === 'conflict' ? 'reviewClaimConflict' : 'unknown';
      await page.waitForFunction(key => [...document.querySelectorAll('[role=alert]')].some(node => node.textContent === window.reviewOwnerProbe.copy[key]), {}, key);
      const writes = await page.evaluate(() => window.reviewOwnerProbe.calls.filter(call => call.method === 'POST').length);
      assert.equal(writes, 1);
      assert.equal(await page.$('[data-retained-finance-command]') !== null, claim === 'transport');
      assert.deepEqual(errors, []); passed++;
    }
  }
  for (const language of ['zh', 'en', 'ja']) {
    for (const archive of ['unsupported', 'conflict', 'transport']) {
      await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-review-owner.html?${new URLSearchParams({ language, owner: 'self', archive })}`);
      await page.waitForSelector('tbody tr'); await click('details'); await page.waitForSelector('article');
      if (archive === 'unsupported') {
        assert.ok(await page.evaluate(() => [...document.querySelectorAll('article button')].some(node => node.textContent === window.reviewOwnerProbe.copy.archive && node.disabled)));
        assert.ok(await page.evaluate(() => document.querySelector('article').textContent.includes(window.reviewOwnerProbe.copy.reviewArchiveUnsupported)));
        assert.equal(await page.evaluate(() => window.reviewOwnerProbe.calls.filter(call => call.method === 'POST').length), 0);
      } else {
        await click('archive');
        const message = archive === 'conflict' ? 'reviewArchiveConflict' : 'unknown';
        await page.waitForFunction(key => [...document.querySelectorAll('[role=alert]')].some(node => node.textContent === window.reviewOwnerProbe.copy[key]), {}, message);
        assert.equal(await page.$('[data-retained-finance-command]') !== null, archive === 'transport');
        if (archive === 'transport') {
          await click('retry');
          await page.waitForFunction(() => document.querySelector('article')?.textContent.includes('ARCHIVED_CONFIRMED_UNPAID'));
          const writes = await page.evaluate(() => window.reviewOwnerProbe.calls.filter(call => call.method === 'POST'));
          assert.equal(writes.length, 2); assert.ok(writes[0].key);
          assert.equal(writes[0].key, writes[1].key); assert.equal(writes[0].body, writes[1].body);
          assert.equal(await page.$('[data-retained-finance-command]'), null);
        }
      }
      assert.deepEqual(errors, []); passed++;
    }
  }
  console.log(`Review ownership UI: ${passed} passed; 0 failed (controlled backend, actual React and admin client)`);
} finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
  fs.rmSync(profile, { recursive: true, force: true, maxRetries: 3, retryDelay: 50 });
}
