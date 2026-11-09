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
let passed = 0;
try {
  await server.listen();
  browser = await launch({ executablePath, headless: true, args: ['--no-sandbox', '--disable-gpu'] });
  const run = async (query, test, viewport = { width: 1100, height: 700 }) => {
    const page = await browser.newPage(); const errors = [];
    page.on('pageerror', error => errors.push(error.message)); page.setDefaultTimeout(12000);
    try {
      await page.setViewport(viewport);
      await page.goto(server.resolvedUrls.local[0] + 'tests/fixtures/model-access.html?' + query);
      await page.waitForFunction(() => window.modelAccessFixture?.kind && window.modelAccessFixture.client);
      await test(page);
      assert.deepEqual(errors, []); passed++;
    } finally { await page.close(); }
  };
  for (const language of ['zh', 'en', 'ja']) for (const theme of ['light', 'dark']) await run(`language=${language}&theme=${theme}`, async page => {
    await page.waitForSelector('[data-testid=model-access-gate]');
    assert.equal(await page.$eval('[data-testid=workspace]', node => node.inert), true);
    const rect = await page.$eval('[data-testid=model-access-gate]', node => { const r = node.getBoundingClientRect(); return { x: r.x, y: r.y, width: r.width, right: r.right, bottom: r.bottom }; });
    assert.ok(rect.x >= 0 && rect.y >= 0 && rect.right <= 390 && rect.bottom <= 844);
    assert.ok(Math.abs(rect.x + rect.width / 2 - 195) < 2);
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !document.querySelector('[data-testid=model-access-gate]'));
    assert.equal(await page.$eval('[data-testid=workspace]', node => node.inert), false);
    await page.click('[data-testid=model-access-external]');
    await page.waitForFunction(() => document.querySelector('output').textContent === 'settings');
    assert.equal(await page.$('[data-testid=model-access-notice]'), null);
  }, { width: 390, height: 844 });
  for (const mode of ['local', 'authenticated', 'history', 'remote']) await run(`mode=${mode}`, async page => {
    await page.waitForFunction(() => !['checking', undefined].includes(window.modelAccessFixture.kind));
    assert.equal(await page.$('[data-testid=model-access-gate]'), null);
    if (mode !== 'history') assert.equal(await page.$('[data-testid=model-access-login]'), null);
    assert.equal(await page.$eval('[data-testid=workspace]', node => node.inert), false);
  });
  await run('mode=delayed', async page => {
    assert.equal(await page.$('[data-testid=model-access-gate]'), null);
    await page.click('#sidebar-login');
    await page.waitForFunction(() => window.modelAccessFixture.kind === 'select_model');
    await page.evaluate(() => window.modelAccessFixture.release());
    assert.equal(await page.$('[data-testid=model-access-login]'), null);
    assert.equal(await page.$('[data-testid=model-access-gate]'), null);
  });
  await run('', async page => {
    await page.waitForSelector('[data-testid=model-access-login]');
    await page.click('[data-testid=model-access-login]');
    await page.waitForFunction(() => window.modelAccessFixture.kind === 'select_model');
    assert.equal(await page.$('[data-testid=model-access-gate]'), null);
    await page.evaluate(async () => { await window.modelAccessFixture.client.logout(); await window.modelAccessFixture.client.loginSms('synthetic', 'synthetic', 'synthetic'); });
    await page.waitForFunction(() => window.modelAccessFixture.kind === 'select_model');
    assert.equal(await page.$('[data-testid=model-access-login]'), null);
  });
  await run('mode=error', async page => {
    await page.waitForSelector('[data-testid=model-access-retry]');
    assert.equal(await page.$('[data-testid=model-access-login]'), null);
    await page.evaluate(() => window.modelAccessFixture.recover());
    await page.waitForSelector('[data-testid=model-access-gate]');
  });
  await run('', async page => {
    await page.waitForSelector('[data-testid=model-access-gate]');
    await page.evaluate(() => window.modelAccessFixture.nested());
    await page.waitForFunction(() => document.activeElement.id === 'captcha-first');
    await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift');
    assert.equal(await page.evaluate(() => document.activeElement.id), 'captcha-last');
    await page.keyboard.press('Tab');
    assert.equal(await page.evaluate(() => document.activeElement.id), 'captcha-first');
    await page.keyboard.press('Escape');
    assert.ok(await page.$('[data-testid=model-access-gate]'));
  });
  console.log(`Model access UI: ${passed} scenarios passed`);
} finally { if (browser) await browser.close(); await server.close(); }
