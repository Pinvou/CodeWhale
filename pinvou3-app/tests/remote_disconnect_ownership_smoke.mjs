import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/chromium', '/usr/bin/google-chrome',
].find(candidate => fs.existsSync(candidate));
if (!chrome) { console.error('SKIP: Chrome/Edge unavailable'); process.exit(2); }
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-remote-disconnect-'));
let browser, vite;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error',
    plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, userDataDir: profile,
    args: [...(process.env.CI === 'true' ? ['--no-sandbox'] : []), '--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/remote_disconnect_ownership_smoke.html`;
  const flush = () => page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  const click = (scope, key) => page.evaluate(({ scope, key }) => {
    const button = [...document.querySelectorAll(`${scope} button`)]
      .find(item => item.textContent.trim() === window.disconnectFixture.copy[key]);
    if (!button || button.disabled) throw new Error(`Unavailable ${scope} ${key}`);
    button.click();
  }, { scope, key });
  async function load() {
    await page.goto(url, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="remote-conversation-open"]');
  }
  async function open() {
    await page.click('[data-testid="remote-conversation-open"]');
    await page.waitForSelector('[role="dialog"]');
  }
  async function resolveOld(failure = false) {
    await page.evaluate(failure => {
      const request = window.disconnectFixture.requests[0];
      if (failure) request.reject(Object.assign(new Error('old failure'), { code: 'relay_offline' }));
      else request.resolve({ status: 'disconnected' });
    }, failure);
    await flush();
    assert.equal(await page.$$eval('[role="dialog"]', nodes => nodes.length), 1);
    assert.equal(await page.$$eval('[role="alert"]', nodes => nodes.length), 0);
    assert.equal(await page.$eval('[data-testid="remote-conversation-open"]', button => button.disabled), false);
  }
  for (const failure of [false, true]) {
    await load();
    await click('[data-testid="remote-controller-panel"]', 'disconnect');
    await page.waitForFunction(() => window.disconnectFixture.requests.length === 1);
    await page.evaluate(() => window.disconnectFixture.setConnection({ status: 'disconnected', connectionId: 'connection-1' }));
    await flush();
    await page.evaluate(() => window.disconnectFixture.setConnection({ status: 'connected', connectionId: 'connection-2' }));
    await flush();
    await open();
    await resolveOld(failure);
    await click('[role="dialog"]', 'close');
    await page.waitForSelector('[role="dialog"]', { hidden: true });
  }
  // Escape closes the original shell while its disconnect is still pending.
  // Reopening on the same connection must create a different dialog owner.
  await load();
  await open();
  await click('[role="dialog"]', 'disconnect');
  await page.waitForFunction(() => window.disconnectFixture.requests.length === 1);
  await page.keyboard.press('Escape');
  await page.waitForSelector('[role="dialog"]', { hidden: true });
  await open();
  await resolveOld();
  await click('[role="dialog"]', 'close');
  await page.waitForSelector('[role="dialog"]', { hidden: true });
  assert.deepEqual(errors, []);
  console.log('remote_disconnect_ownership_smoke passed three mounted UI interleavings (controlled host IPC)');
} finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
  const resolved = fs.realpathSync(profile);
  if (path.dirname(resolved) !== fs.realpathSync(os.tmpdir())) throw new Error('Unexpected profile parent');
  fs.rmSync(resolved, { recursive: true, force: true });
}
