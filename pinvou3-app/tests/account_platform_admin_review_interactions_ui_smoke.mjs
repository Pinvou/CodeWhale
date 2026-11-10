import assert from 'node:assert/strict';
import fs from 'node:fs';
import { createServer } from './helpers/vite-fixture.mjs';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';

const executablePath = [process.env.CHROME, 'C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(value => value && fs.existsSync(value));
assert.ok(executablePath, 'Chromium required; native focus validation cannot be skipped');
const server = await createServer({ root: import.meta.dirname + '/..', configFile: false, plugins: [react()], css: { postcss: { plugins: [] } }, server: { host: '127.0.0.1', port: 0 } });
const results = [];
let browser;
try {
  await server.listen();
  browser = await puppeteer.launch({ executablePath, headless: true, args: ['--no-sandbox', '--disable-gpu'] });
  const run = async (name, body) => {
    if (process.env.ADMIN_REVIEW_CASE && !name.includes(process.env.ADMIN_REVIEW_CASE)) return;
    const page = await browser.newPage(), errors = [];
    page.on('pageerror', error => errors.push(error.message));
    page.setDefaultTimeout(8000);
    try { await body(page); assert.deepEqual(errors, []); results.push({ name, pass: true }); }
    catch (error) { results.push({ name, pass: false, error: error.message }); }
    finally { await page.close(); }
  };
  for (const outcome of ['success', 'failure', 'cancel']) await run('formula preview keyboard focus: ' + outcome, async page => {
    await page.goto(server.resolvedUrls.local[0] + 'tests/fixtures/admin-review-interactions.html?protocol_formula_pane=formulas');
    const trigger = '[role=tabpanel]:not([hidden]) tbody button';
    await page.waitForSelector(trigger);
    await page.focus(trigger); await page.keyboard.press('Enter');
    await page.waitForFunction(() => typeof window.adminReview.release === 'function' && document.activeElement?.closest('[role=dialog]'));
    if (outcome !== 'cancel') {
      await page.evaluate(failure => window.adminReview.release(failure), outcome === 'failure');
      await page.waitForSelector(outcome === 'success' ? '.adm-formula-explained' : '[role=dialog] [role=alert]');
      await page.waitForFunction(() => document.activeElement?.closest('[role=dialog]'));
    }
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.equal(await page.$eval(trigger, node => document.activeElement === node), true, 'closing restores the original connected keyboard trigger');
    if (outcome === 'cancel') {
      await page.evaluate(() => window.adminReview.release(false));
      await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    }
    await page.evaluate(() => { window.adminReview.release = null; });
    await page.focus(trigger); await page.keyboard.press('Enter');
    await page.waitForFunction(() => typeof window.adminReview.release === 'function');
    await page.evaluate(() => window.adminReview.release(false));
    await page.waitForSelector('.adm-formula-explained');
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.equal(await page.$eval(trigger, node => document.activeElement === node), true, 'the next preview remains usable after the previous phase');
    assert.equal(await page.evaluate(() => window.adminReview.calls.some(call => call.method !== 'GET')), false);
  });
  await run('SMS native required fields cannot block an original unknown replay', async page => {
    await page.goto(server.resolvedUrls.local[0] + 'tests/fixtures/admin-review-interactions.html?sms=1');
    await page.waitForSelector('.adm-sms-reason textarea');
    await page.type('.adm-sms-reason textarea', 'Synthetic original reason');
    const save = '.adm-sms-form-actions button[type=submit]';
    await page.click(save);
    await page.waitForFunction(() => window.adminReview.smsWrites.length === 1 && !document.querySelector('.adm-sms-reason textarea').disabled);
    await page.evaluate(() => {
      for (const input of [document.querySelector('.adm-sms-reason textarea'), document.querySelector('.adm-sms-template input')]) {
        Object.getOwnPropertyDescriptor(input.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype, 'value').set.call(input, '');
        input.dispatchEvent(new Event('input', { bubbles: true }));
      }
    });
    assert.equal(await page.$eval('.adm-sms-form', form => form.checkValidity()), false, 'the edited draft is natively invalid');
    assert.equal(await page.$eval(save, button => button.disabled), false);
    await page.click(save);
    await page.waitForFunction(() => window.adminReview.smsWrites.length === 2);
    assert.equal(await page.evaluate(() => { const [first, second] = window.adminReview.smsWrites; return first.body === second.body && first.key === second.key; }), true);
    await page.waitForFunction(() => !document.querySelector('.adm-sms-form-actions button[type=submit]').formNoValidate);
  });
} finally {
  try { if (browser) await browser.close(); } finally { await server.close(); }
}
const result = { pass: results.filter(item => item.pass).length, fail: results.filter(item => !item.pass).length, results, limits: ['Real components and AdminPlatformClient with controlled HTTP DTOs; no real service or database.'] };
if (process.env.ADMIN_REVIEW_RESULT) fs.writeFileSync(process.env.ADMIN_REVIEW_RESULT, JSON.stringify(result, null, 2));
console.log(JSON.stringify(result));
process.exitCode = result.fail || !results.length ? 1 : 0;
