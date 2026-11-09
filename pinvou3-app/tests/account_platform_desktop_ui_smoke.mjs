import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import react from '@vitejs/plugin-react';
import { createServer } from './helpers/vite-fixture.mjs';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { accountDesktopCopy, accountPlatformCopy, financeCopy } from '../src/shared/i18n.js';

const chrome = [process.env.CHROME, 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files/Google/Chrome/Application/chrome.exe', '/usr/bin/google-chrome', '/usr/bin/chromium', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(value => value && fs.existsSync(value));
assert.ok(chrome, 'Chromium is required for desktop account UI validation');
const root = path.resolve(import.meta.dirname, '..');
const server = await createServer({ root, configFile: false, plugins: [react()], server: { fs: { allow: [root, fs.realpathSync(path.join(root, 'node_modules'))] } } });
let browser, cases = 0;
try {
  await server.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ['--disable-gpu', '--no-first-run', '--no-sandbox'] });
  const page = await browser.newPage(); page.setDefaultTimeout(12000);
  const errors = []; page.on('pageerror', error => errors.push(error.message));
  const url = `http://127.0.0.1:${server.httpServer.address().port}/tests/fixtures/account-desktop-ui.html`;
  const fresh = async (query = '', width = 1280, height = 900) => {
    errors.length = 0; await page.setViewport({ width, height }); await page.goto(`${url}?${query}`);
    await page.waitForSelector('.ap-auth-form, .ap-desktop-context');
  };
  const click = async text => page.evaluate(text => {
    const node = [...document.querySelectorAll('button')].find(node => (node.getAttribute('aria-label') || node.textContent.trim()) === text && !node.disabled && node.getClientRects().length);
    if (!node) throw new Error(`Missing available action: ${text}`);
    node.focus(); node.click();
  }, text);
  const geometry = async () => {
    const result = await page.evaluate(() => {
      const shell = document.querySelector('.ap-shell').getBoundingClientRect();
      const inputs = [...document.querySelectorAll('.ap-auth-form input')].map(node => node.getBoundingClientRect());
      return { centered: Math.abs(shell.left + shell.width / 2 - innerWidth / 2) < 2, fits: shell.left >= 0 && shell.right <= innerWidth && shell.height <= innerHeight, aligned: inputs.every(rect => Math.abs(rect.left - inputs[0].left) < 2 && Math.abs(rect.width - inputs[0].width) < 2), overflow: document.documentElement.scrollWidth > innerWidth };
    });
    assert.deepEqual(result, { centered: true, fits: true, aligned: true, overflow: false });
  };
  const screenshot = async name => {
    if (!process.env.PINVOU_ACCOUNT_SCREENSHOTS) return;
    fs.mkdirSync(process.env.PINVOU_ACCOUNT_SCREENSHOTS, { recursive: true });
    await page.screenshot({ path: path.join(process.env.PINVOU_ACCOUNT_SCREENSHOTS, `${name}.png`) });
  };
  const run = async (name, action) => {
    if (process.env.PINVOU_ACCOUNT_CASE && !name.includes(process.env.PINVOU_ACCOUNT_CASE)) return;
    try { await action(); assert.deepEqual(errors, []); cases++; console.log(`PASS ${name}`); }
    catch (error) { console.error(name, await page.evaluate(() => ({ text: document.body.innerText, calls: window.accountFixture?.calls }))); throw error; }
  };
  await run('compact profile excludes finance, invitations, closure, and full mobile number', async () => {
    await fresh();
    await page.waitForSelector('.ap-identity-card');
    assert.equal(await page.$('.ap-finance'), null);
    assert.equal(await page.$('table, .ap-danger-section, input[type=password]'), null);
    assert.equal(await page.evaluate(() => document.body.textContent.includes('+8613800000000')), false);
    assert.equal(await page.evaluate(() => document.body.textContent.includes('ACTIVE')), false);
    assert.equal(await page.evaluate(() => window.accountFixture.calls.some(call => typeof call === 'string' && /invitations|entitlement|usage|orders|products/.test(call))), false);
    await screenshot('profile-zh');
  });
  for (const language of ['zh', 'en', 'ja']) await run(`${language}: Web handoff uses current language and exposes recoverable failure`, async () => {
    await fresh(`language=${language}`); await click(accountDesktopCopy[language].openWeb);
    await page.waitForFunction(language => window.accountFixture.calls.includes(`portal:${language}`), {}, language);
    await page.evaluate(() => { window.accountFixture.portalFail = true; }); await click(accountDesktopCopy[language].openWeb); await page.waitForSelector('.ap-web-center [role=alert]');
    assert.equal(await page.$eval('.ap-web-center button', node => node.disabled), false);
  });
  await run('recharge retains QR, paid balance refresh, and excludes historical records and benefits', async () => {
    await fresh('language=zh'); await click(accountDesktopCopy.zh.recharge);
    await page.waitForFunction(() => document.querySelector('.ap-finance form button') && !document.querySelector('.ap-finance form button').disabled);
    assert.equal(await page.$('.ap-web-center'), null, 'Web navigation belongs only to personal information');
    assert.equal(await page.$('table, .ap-danger-section, .ap-usage'), null);
    assert.equal(await page.evaluate(() => window.accountFixture.calls.some(call => typeof call === 'string' && /entitlement|usage|invitations/.test(call))), false);
    await screenshot('recharge-zh');
    await click(financeCopy.zh.create); await page.waitForSelector('.ap-payment-dialog img');
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('.ap-payment-dialog'));
    assert.notEqual(await page.$('.ap-account-dialog'), null, 'Escape in payment must not close the account center');
    await click(financeCopy.zh.continuePayment); await page.waitForSelector('.ap-payment-dialog');
    await page.evaluate(() => { window.accountFixture.paid = true; }); await click(financeCopy.zh.query);
    await page.waitForFunction(() => !document.querySelector('.ap-payment-dialog'));
    await page.waitForFunction(() => document.querySelector('.ap-balance-hero strong')?.textContent === '25');
    assert.equal(await page.evaluate(() => window.accountFixture.calls.filter(call => Array.isArray(call) && call[0] === 'create').length), 1);
  });
  await run('returning to profile and recharge preserves an unfinished payment', async () => {
    await fresh('pending'); await click(accountDesktopCopy.zh.recharge); await page.waitForSelector('.ap-pending-payments button');
    await click(accountDesktopCopy.zh.profile); await click(accountDesktopCopy.zh.recharge); await click(financeCopy.zh.continuePayment); await page.waitForSelector('.ap-payment-dialog');
    assert.equal(await page.evaluate(() => window.accountFixture.calls.some(call => Array.isArray(call) && call[0] === 'create')), false);
  });
  await run('organization switch retains recharge without loading organization management', async () => {
    await fresh(); await click('Sample organization'); await page.waitForFunction(() => window.accountFixture.organization);
    await page.waitForFunction(() => document.querySelector('.ap-desktop-context button[aria-pressed=true]')?.textContent === 'Sample organization');
    await click(accountDesktopCopy.zh.recharge); await page.waitForSelector('[data-organization-purchase] option[value="00000000-0000-4000-8000-000000000003"]');
    assert.equal(await page.$('table, .ap-danger-section'), null);
    assert.equal(await page.evaluate(() => window.accountFixture.calls.some(call => typeof call === 'string' && /packages|grants|quota-members|units|usage|invitations/.test(call))), false);
    const selects = await page.$$('[data-organization-purchase] fieldset select');
    await selects[0].select('00000000-0000-4000-8000-000000000003'); await selects[1].select('ALIPAY');
    await page.click('[data-organization-purchase] button[type=submit]'); await page.waitForSelector('.ap-payment-dialog img');
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('.ap-payment-dialog'));
    await click(financeCopy.zh.continuePayment); await page.waitForSelector('.ap-payment-dialog');
    assert.equal(await page.evaluate(() => window.accountFixture.calls.filter(call => Array.isArray(call) && call[0] === 'org-create').length), 1);
  });
  await run('missing permission explains recharge availability', async () => {
    await fresh('no-permission'); await click(accountDesktopCopy.zh.recharge);
    assert.equal(await page.$eval('.ap-center-panel:not([hidden]) [role=status]', node => node.textContent), accountDesktopCopy.zh.rechargeUnavailable);
    assert.equal(await page.$('.ap-finance'), null);
  });
  await run('empty recharge catalog provides an actionable explanation', async () => {
    await fresh('empty-products'); await click(accountDesktopCopy.zh.recharge);
    await page.waitForFunction(text => document.querySelector('.ap-finance')?.textContent.includes(text), {}, accountDesktopCopy.zh.noRechargeProducts);
    assert.equal(await page.$('.ap-web-center'), null);
  });
  await run('compact dark profile stays within a narrow short viewport', async () => {
    await fresh('theme=dark', 390, 568); await geometry();
    assert.equal(await page.$eval('.ap-shell', node => getComputedStyle(node).backgroundColor), 'rgb(23, 28, 32)');
    await screenshot('profile-dark-390');
  });
  await run('dialog closes with Escape, restores focus, and traps Tab', async () => {
    await fresh(); await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('.ap-account-dialog'));
    await page.click('#background'); await page.waitForSelector('.ap-desktop-context');
    assert.equal(await page.$eval('#background', node => Boolean(node.closest('[inert]'))), true);
    await page.focus('.ap-account-close'); await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift');
    assert.equal(await page.evaluate(() => document.activeElement.textContent), accountPlatformCopy.zh.logout);
    await page.keyboard.press('Tab'); assert.equal(await page.evaluate(() => document.activeElement.className), 'ap-account-close');
    await page.keyboard.press('Escape'); assert.equal(await page.evaluate(() => document.activeElement.id), 'background');
    assert.equal(await page.$eval('#background', node => Boolean(node.closest('[inert]'))), false);
  });
  await run('sign out returns to the centered login form', async () => {
    await fresh(); await click(accountPlatformCopy.zh.logout); await page.waitForSelector('.ap-login form input[autocomplete=username]'); await geometry();
  });
  console.log(`Desktop account UI: ${cases} cases passed`);
} finally { try { if (browser) await browser.close(); } finally { await server.close(); } }
