import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';
import { fixtureScope } from './helpers/fixture-scope.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || [
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/chromium', '/usr/bin/google-chrome',
].find(candidate => fs.existsSync(candidate));
assert.ok(chrome, 'Chrome/Edge is required for the mounted UI contract');
let browser;
let vite;
const results = [];
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()] });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  for (const [language, failure, scenario = 'cleanup'] of [
    ['zh', false], ['en', false], ['ja', false], ['zh', true],
    ['zh', false, 'cleanup-local-failure'], ['zh', false, 'cleanup-history'],
  ]) {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/remote_dispatch_ownership_smoke.html?scenario=${scenario}&lang=${language}`, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => document.querySelector('[data-testid="chat-remote-pending-dispatches"]'));
    assert.equal(await page.evaluate(async () => {
      try { await window.dispatchFixture.manualUpload(); return 'unexpected-success'; } catch (error) { return error.code; }
    }), 'remote_attachment_fenced');
    const clickAbandon = () => page.evaluate(() => {
      const fixture = window.dispatchFixture;
      const button = [...document.querySelectorAll('[data-testid="chat-remote-pending-dispatches"] button')]
        .find(item => item.textContent === fixture.copy.remoteConversation.abandonPreparedTask);
      if (!button || button.disabled) throw new Error('abandon action unavailable');
      button.click();
    });
    await clickAbandon();
    await page.waitForFunction(() => window.dispatchFixture.cleanups.length === 1);
    assert.equal(await page.evaluate(() => window.dispatchFixture.calls.filter(call => call.kind === 'abandon').length), 0,
      'local retirement waits for physical cleanup outcome');
    await page.evaluate(failed => {
      const cleanup = window.dispatchFixture.cleanups[0];
      if (failed) cleanup.reject(new Error('transfer_staging_cleanup_failed'));
      else cleanup.resolve({ transfer_id: cleanup.transferId, cleanup_confirmed: true });
    }, failure);
    await page.waitForFunction(() => window.dispatchFixture.calls.filter(call => call.kind === 'abandon').length === 1);
    if (scenario === 'cleanup-local-failure') {
      await page.waitForFunction(() => {
        const fixture = window.dispatchFixture;
        const button = [...document.querySelectorAll('[data-testid="chat-remote-pending-dispatches"] button')]
          .find(item => item.textContent === fixture.copy.remoteConversation.abandonPreparedTask);
        return button && !button.disabled
          && document.querySelector('[data-testid="chat-remote-pending-dispatches"]').textContent.includes(fixture.copy.remoteConversation.preparedPayloadLost);
      });
      await clickAbandon();
      await page.waitForFunction(() => window.dispatchFixture.calls.filter(call => call.kind === 'abandon').length === 2);
      assert.equal(await page.evaluate(() => window.dispatchFixture.cleanups.length), 1,
        'explicit retry reuses the exact previously acknowledged cleanup proof');
    }
    await page.waitForFunction(() => !document.querySelector('[data-testid="chat-remote-pending-dispatches"]'));
    assert.equal(await page.evaluate(() => window.dispatchFixture.calls.filter(call => ['manualBegin', 'commit', 'reserve', 'attachmentBegin'].includes(call.kind)).length), 0,
      'abandon does not automatically upload or submit');
    const outcome = await page.evaluate(async () => {
      try { return await window.dispatchFixture.manualUpload(); } catch (error) { return error.code; }
    });
    assert.deepEqual(outcome, failure ? 'remote_attachment_fenced' : ['manual-new-handle']);
    assert.deepEqual(errors, []);
    results.push({ language, cleanupFailure: failure, scenario, outcome: 'pass', mounted: 'actual ChatView; controlled host bridge, real upload helper' });
    await page.close();
  }
} finally {
  await fixtureScope.close();
}
console.log(JSON.stringify({ scenarios: results, limits: 'Controlled host bridge and no Relay/native end-to-end claim; native physical cleanup and desktop bridge contracts validated separately' }));
