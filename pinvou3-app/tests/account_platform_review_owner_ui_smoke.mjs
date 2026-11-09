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
assert.ok(chrome, 'Browser unavailable; review ownership UI was not validated');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-review-owner-'));
let vite; let browser; let passed = 0;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage(); page.setDefaultTimeout(10000);
  const errors = []; page.on('pageerror', error => errors.push(error.message)); page.on('dialog', dialog => dialog.accept());
  const click = async key => {
    const clicked = await page.evaluate(key => {
      const text = window.reviewOwnerProbe.copy[key];
      const button = [...document.querySelectorAll('button')].find(node => node.textContent.trim() === text && !node.disabled);
      if (!button) return false; button.click(); return true;
    }, key);
    assert.ok(clicked, `Missing enabled review action: ${key}`);
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
  console.log(`Review ownership UI: ${passed} passed; 0 failed (controlled backend, actual React and admin client)`);
} finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
  fs.rmSync(profile, { recursive: true, force: true, maxRetries: 3, retryDelay: 50 });
}
