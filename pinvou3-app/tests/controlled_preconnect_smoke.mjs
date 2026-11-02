#!/usr/bin/env node
// Real reducer/component recovery regression; bridge and IPC are simulated.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const requireFromApp = createRequire(path.join(appRoot, 'package.json'));
const dependency = async name => import(pathToFileURL(requireFromApp.resolve(name)).href);
const [{ createServer }, { default: react }, puppeteer] = await Promise.all([
  dependency('vite'), dependency('@vitejs/plugin-react'), dependency('puppeteer-core'),
]);
const chrome = process.env.CHROME || [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/chromium', '/usr/bin/google-chrome',
].find(candidate => fs.existsSync(candidate));
if (!chrome) { console.error('SKIP: Chrome/Edge unavailable'); process.exit(2); }
const expected = {
  zh: {
    pairing: '连接码已过期，请重新开启远程控制并使用新的连接码重试。',
    handshake: '安全握手超时，请重新开启远程控制并使用新的连接码重试。',
  },
  en: {
    pairing: 'The code expired. Start remote control again and retry with a new code.',
    handshake: 'The secure connection timed out. Start remote control again and retry with a new code.',
  },
  ja: {
    pairing: '接続コードの有効期限が切れました。リモート操作を開始し直し、新しい接続コードで再試行してください。',
    handshake: '安全な接続がタイムアウトしました。リモート操作を開始し直し、新しい接続コードで再試行してください。',
  },
};
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-controlled-preconnect-smoke-'));
let browser;
let vite;
try {
  vite = await createServer({ root: appRoot, configFile: false, appType: 'mpa', logLevel: 'error',
    plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true,
    userDataDir: profile, args: ['--disable-gpu', '--no-first-run', '--no-default-browser-check'] });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const base = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/controlled_preconnect_smoke.html`;
  const panel = '[data-testid="controlled-device-panel"]';
  const toggle = '[data-testid="controlled-device-toggle"]';
  const refresh = '[data-testid="controlled-device-refresh"]';
  for (const language of ['zh', 'en', 'ja']) {
    await page.goto(`${base}?lang=${language}`, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => window.preconnectFixture?.ready
      && document.querySelector('output')?.textContent === '2345 678');
    for (const [phase, code, nextGeneration, nextCode] of [
      ['pairing', 'desktop_remote_pairing_expired', 'generation-renderer-2', 'ABCD EFG'],
      ['handshake', 'desktop_remote_handshake_timeout', 'generation-renderer-3', 'HJKM NPQ'],
    ]) {
      await page.evaluate(value => window.preconnectFixture.expire(value), code);
      await page.waitForFunction(selector => {
        const node = document.querySelector(selector);
        return node?.dataset.uiState === 'error';
      }, {}, panel);
      const message = await page.$eval(panel, node => [...node.querySelectorAll('[role="alert"]')].map(alert => alert.innerText).join('\n'));
      assert.ok(message.includes(expected[language][phase]), `Expected ${language}/${phase} recovery guidance, received: ${message}`);
      const terminal = await page.evaluate((switchSelector, refreshSelector) => ({
        device: window.preconnectFixture.status(),
        refreshDisabled: document.querySelector(refreshSelector).disabled,
        toggleDisabled: document.querySelector(switchSelector).disabled,
        checked: document.querySelector(switchSelector).getAttribute('aria-checked'),
        codes: [...document.querySelectorAll('output')].map(node => node.textContent),
      }), toggle, refresh);
      assert.equal(terminal.device.lastError, code);
      assert.equal(terminal.device.pairingCode, null);
      assert.deepEqual(terminal.codes, []);
      assert.equal(terminal.refreshDisabled, true, 'terminal cannot call controlledRefresh');
      assert.equal(terminal.toggleDisabled, false, 'instruction to restart requires available toggle');
      assert.equal(terminal.checked, 'false');
      await page.click(toggle);
      await page.waitForFunction((generation, formatted) => {
        const state = window.preconnectFixture.status();
        return state.status === 'waiting_for_pairing' && state.generation === generation
          && document.querySelector('output')?.textContent === formatted;
      }, {}, nextGeneration, nextCode);
      assert.equal(await page.$eval(toggle, node => node.getAttribute('aria-checked')), 'true');
      assert.equal(await page.$eval(refresh, node => node.disabled), false);
      assert.equal(await page.evaluate(() => window.preconnectFixture.status().lastError), null);
      const calls = await page.evaluate(() => window.preconnectFixture.calls);
      assert.equal(calls.filter(call => call[0] === 'start').length, phase === 'pairing' ? 1 : 2);
      assert.equal(calls.filter(call => ['refresh', 'stop'].includes(call[0])).length, 0);
    }
  }
  assert.deepEqual(errors, []);
  console.log('controlled_preconnect_smoke: three-language recovery passed (real reducer/component, mock bridge; not native IPC or Relay)');
} finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
  const resolvedProfile = path.resolve(profile);
  const intendedParent = path.resolve(os.tmpdir());
  assert.equal(path.dirname(resolvedProfile), intendedParent);
  assert.ok(path.basename(resolvedProfile).startsWith('pinvou-controlled-preconnect-smoke-'));
  fs.rmSync(resolvedProfile, { recursive: true, force: true });
}
