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
    if (outcome === 'failure' || outcome === 'cancel') {
      for (const event of [{ isComposing: true, keyCode: 0 }, { isComposing: false, keyCode: 229 }]) {
        await page.evaluate(event => document.activeElement.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, ...event })), event);
        assert.ok(await page.$('[role=dialog]'), 'IME Escape must keep loading/failed preview open');
        assert.equal(await page.evaluate(() => Boolean(document.activeElement?.closest('[role=dialog]'))), true);
      }
    }
    if (outcome === 'failure') {
      assert.equal(await page.evaluate(() => document.activeElement.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true, isComposing: true }))), false, 'the one-control focus boundary must prevent Tab even during IME');
      assert.equal(await page.evaluate(() => Boolean(document.activeElement?.closest('[role=dialog]'))), true, 'IME must not suspend the failed dialog Tab boundary');
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
  for (const state of ['connected', 'hidden', 'removed']) await run('protocol definition restores only an available trigger: ' + state, async page => {
    await page.goto(server.resolvedUrls.local[0] + 'tests/fixtures/admin-review-interactions.html');
    const trigger = '.adm-protocol-definition-open'; await page.waitForSelector(trigger);
    await page.focus(trigger); await page.keyboard.press('Enter'); await page.waitForSelector('.adm-protocol-definition');
    await page.evaluate(state => {
      const trigger = document.querySelector('.adm-protocol-definition-open'), focus = trigger.focus.bind(trigger);
      window.adminReview.returnFocusCalls = 0;
      trigger.focus = () => { window.adminReview.returnFocusCalls++; focus(); };
      if (state === 'hidden') trigger.closest('[role=tabpanel]').hidden = true;
      if (state === 'removed') trigger.remove();
    }, state);
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.equal(await page.evaluate(() => window.adminReview.returnFocusCalls), state === 'connected' ? 1 : 0);
    if (state === 'connected') assert.equal(await page.$eval(trigger, n => document.activeElement === n), true);
  });
  for (const blocked of [false, true]) await run('settings language feedback is announced with optional storage: ' + blocked, async page => {
    await page.evaluateOnNewDocument(blocked => {
      if (blocked) Object.defineProperty(window, 'localStorage', { get() { throw new Error('controlled unavailable storage'); } });
      else localStorage.setItem('pinvou.admin.language', 'en');
    }, blocked);
    await page.goto(server.resolvedUrls.local[0] + 'tests/fixtures/admin-review-interactions.html?language-settings=1');
    const selector = '.adm-language-settings select'; await page.waitForSelector(selector);
    if (!blocked) assert.equal(await page.$eval('.adm-language-settings [role=status]', n => n.textContent.includes(window.adminReview.languageWords.en.saved)), true, 'initial persisted preference is actually presented');
    for (const language of ['zh', 'en', 'ja']) {
      await page.select(selector, language);
      await page.waitForFunction(({language, blocked}) => document.querySelector('.adm-language-settings [role=status]')?.textContent.includes(window.adminReview.languageWords[language][blocked ? 'unsaved' : 'saved']), {}, {language, blocked});
      assert.equal(await page.$eval(selector, n => n.value), language);
    }
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
