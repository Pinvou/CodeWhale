import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const executable = process.env.CHROME || process.env.CHROMIUM || ['/usr/bin/chromium', '/usr/bin/google-chrome', 'C:/Program Files/Google/Chrome/Application/chrome.exe'].find(fs.existsSync);
assert.ok(executable && !/msedge(?:\.exe)?$/i.test(executable), 'A Chrome/Chromium executable is required for this smoke');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-force-cancel-'));
let vite, browser;
try {
  vite = await createServer({ root, cacheDir: path.join(profile, 'vite-cache'), configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null, fs: { allow: [root, fs.realpathSync(path.join(root, 'node_modules'))], deny: [] } } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: executable, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  for (const language of ['zh', 'en', 'ja']) {
    const page = await browser.newPage();
    const errors = []; page.on('pageerror', error => errors.push(error.message));
    await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-force-cancel.html?model_detail_tab=lifecycle&language=${language}`);
    await page.waitForSelector('.adm-lifecycle-plan-form');
    await page.select('.adm-lifecycle-plan-fields select', 'REMOVED');
    await page.waitForSelector('.adm-lifecycle-plan-form input[type=checkbox]');
    assert.equal(await page.$eval('.adm-lifecycle-plan-form input[type=checkbox]', el => el.checked), false);
    await page.select('.adm-lifecycle-plan-fields select[required]', '22222222-2222-4222-8222-222222222222');
    await page.$eval('.adm-lifecycle-plan-form textarea', el => { Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(el, 'reviewed removal and actual usage settlement'); el.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.click('.adm-lifecycle-plan-form input[type=checkbox]');
    await page.click('.adm-lifecycle-plan-form button[type=submit]');
    await page.waitForSelector('.adm-danger-confirm input');
    await page.waitForFunction(() => document.body.textContent.includes(window.forceCancelFixture.copy.forceCancelInflightHelp));
    await page.click('.adm-danger-confirm input');
    await page.click('[role=dialog] button[type=submit]');
    await page.waitForFunction(() => window.forceCancelFixture.submitted === true);
    const facts = await page.evaluate(() => ({ calls: window.forceCancelFixture.calls, draft: window.forceCancelFixture.draft }));
    assert.equal(facts.calls[0].input.force_cancel_inflight, true);
    assert.equal(facts.draft.normalized_command.force_cancel_inflight, true);
    assert.equal(facts.draft.critical_resource_versions.force_cancel_inflight, true);
    assert.equal(facts.draft.critical_resource_versions.policy_version, 3);
    assert.deepEqual(errors, []);
    await page.close();
    console.log(`PASS ${language} removal plan freezes explicit cancellation through approval submission`);
  }
} finally {
  await browser?.close(); await vite?.close();
  const resolved = path.resolve(profile);
  assert.ok(resolved.startsWith(path.resolve(os.tmpdir()) + path.sep) && path.basename(resolved).startsWith('pinvou-force-cancel-'));
  fs.rmSync(resolved, { recursive: true, force: true });
}
