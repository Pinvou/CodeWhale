import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import react from '@vitejs/plugin-react';
import { createServer } from './helpers/vite-fixture.mjs';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { accountAuthCopy, accountPlatformCopy } from '../src/shared/i18n.js';

const chrome = [process.env.CHROME, 'C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/google-chrome', '/usr/bin/chromium', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(value => value && fs.existsSync(value));
assert.ok(chrome, 'Chromium is required for desktop account UI validation');
const root = path.resolve(import.meta.dirname, '..');
const server = await createServer({ root, configFile: false, plugins: [react()], server: { fs: { allow: [root, fs.realpathSync(path.join(root, 'node_modules'))] } } });
let browser, cases = 0;
try {
  await server.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ['--disable-gpu', '--no-first-run', '--no-sandbox'] });
  const page = await browser.newPage(); page.setDefaultTimeout(12000);
  const errors = []; page.on('pageerror', error => errors.push(error.message));
  const url = `http://127.0.0.1:${server.httpServer.address().port}/tests/fixtures/account-auth-ui.html`;
  const fresh = async (query = '', width = 1280, height = 900) => {
    errors.length = 0; await page.setViewport({ width, height }); await page.goto(`${url}?${query}`);
    await page.waitForSelector('.ap-auth-form, .ap-desktop-context');
  };
  const click = async text => page.evaluate(text => {
    const node = [...document.querySelectorAll('button')].find(node => node.textContent.trim() === text && !node.disabled && node.getClientRects().length);
    if (!node) throw new Error(`Missing available action: ${text}`);
    node.focus(); node.click();
  }, text);
  const geometry = async () => {
    const result = await page.evaluate(() => {
      const shell = document.querySelector('.ap-shell').getBoundingClientRect();
      const inputs = [...document.querySelectorAll('.ap-auth-form input')].map(node => (node.closest('.ap-auth-code') || node).getBoundingClientRect());
      return { centered: Math.abs(shell.left + shell.width / 2 - innerWidth / 2) < 2, fits: shell.left >= 0 && shell.right <= innerWidth && shell.height <= innerHeight, aligned: inputs.every(rect => Math.abs(rect.left - inputs[0].left) < 2 && Math.abs(rect.width - inputs[0].width) < 2), overflow: document.documentElement.scrollWidth > innerWidth };
    });
    assert.deepEqual(result, { centered: true, fits: true, aligned: true, overflow: false });
  };
  const screenshot = async name => {
    if (!process.env.PINVOU_ACCOUNT_SCREENSHOTS) return;
    fs.mkdirSync(process.env.PINVOU_ACCOUNT_SCREENSHOTS, { recursive: true });
    await page.screenshot({ path: path.join(process.env.PINVOU_ACCOUNT_SCREENSHOTS, `${name}.png`) });
  };
  const run = async (name, action) => { await action(); assert.deepEqual(errors, []); cases++; console.log(`PASS ${name}`); };
  for (const language of ['zh', 'en', 'ja']) for (const width of [1920, 390]) await run(`${language} ${width}px: centered and aligned authentication flows`, async () => {
    await fresh(`guest&language=${language}`, width);
    const words = accountAuthCopy[language], copy = accountPlatformCopy[language];
    for (const mode of ['password', 'sms', 'register', 'forgot']) {
      if (mode === 'sms') await click(copy.authModes.sms);
      if (mode === 'register') await click(copy.register);
      if (mode === 'forgot') { await click(words.backToLogin); await click(copy.authModes.forgot); }
      await geometry();
      assert.equal(await page.$eval('.ap-auth-heading h1', node => node.textContent), mode === 'register' ? words.registerTitle : mode === 'forgot' ? words.resetTitle : words.loginTitle);
      assert.equal(await page.$eval('.ap-auth-form button:not([type=button])', node => getComputedStyle(node).backgroundColor), 'rgb(15, 118, 110)');
      if (mode !== 'password') assert.equal(await page.$eval('.ap-auth-hint', node => node.textContent), words.mobileHint);
      if (language === 'zh') await screenshot(`auth-${mode}-${width}`);
    }
  });
  console.log(`Authentication layout: ${cases} cases passed`);
} finally { if (browser) await browser.close(); await server.close(); }
