import assert from 'node:assert/strict';
import fs from 'node:fs';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import { createServer } from './helpers/vite-fixture.mjs';
import { launch } from './helpers/puppeteer-fixture.mjs';

const executablePath = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome'].find(fs.existsSync);
assert.ok(executablePath, 'A browser is required');
const server = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), configFile: false, plugins: [react()], logLevel: 'error' });
let browser;
try {
  await server.listen(); browser = await launch({ executablePath, headless: true, args: ['--no-sandbox', '--disable-gpu'] });
  for (const language of ['zh', 'en', 'ja']) for (const dark of [false, true]) {
    const page = await browser.newPage(); const errors = [];
    page.on('pageerror', error => errors.push(error.name));
    try {
      await page.goto(server.resolvedUrls.local[0] + 'tests/fixtures/account-platform-f05-native-code.html');
      await page.waitForFunction(() => window.nativeCode && document.querySelector('button'));
      await page.evaluate(async ({language, dark}) => {
        // These are browser-served Vite URLs, rather than Node imports.
        for (const stylePath of ['/src/styles/tailwind.css', '/src/styles/base.css']) await import(stylePath);
        document.documentElement.classList.toggle('dark', dark);
        document.getElementById('root').style.cssText = 'position:relative;height:100vh;padding:500px 24px 24px;box-sizing:border-box';
        window.nativeCode.language = language; await window.nativeCode.render();
      }, {language, dark});
      await page.click('#root button');
      await page.waitForFunction(() => document.body.textContent.includes('Platform blocked'));
      const enabledHandle = await page.evaluateHandle(() => [...document.querySelectorAll('button')].find(node => node.textContent.includes('Platform B')));
      const enabledRow = enabledHandle.asElement(); assert.ok(enabledRow);
      await enabledRow.hover();
      await page.waitForFunction(() => { const row = [...document.querySelectorAll('button')].find(node => node.textContent.includes('Platform B')); return getComputedStyle(row.querySelector('span.block')).color === 'rgb(255, 255, 255)' && getComputedStyle(row).backgroundColor === 'rgb(0, 122, 255)'; });
      await enabledHandle.dispose();
      const handle = await page.evaluateHandle(() => [...document.querySelectorAll('button')].find(node => node.textContent.includes('Platform blocked')));
      const row = handle.asElement(); assert.ok(row);
      assert.equal(await row.evaluate(node => node.disabled), true);
      await page.mouse.move(0, 0);
      const colors = node => [...node.querySelectorAll('span.block')].map(span => getComputedStyle(span).color);
      const before = await row.evaluate(colors); await row.hover(); const after = await row.evaluate(colors);
      assert.deepEqual(after, before); assert.ok(after.length >= 2); assert.notEqual(after[0], 'rgb(255, 255, 255)');
      await row.click(); assert.deepEqual(await page.evaluate(() => window.nativeCode.calls), []);
      await handle.dispose(); assert.deepEqual(errors, []);
    } finally { await page.close(); }
  }
  console.log('Disabled model hover: 6 scenarios passed');
} finally { if (browser) await browser.close(); await server.close(); }
