import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || [
  '/opt/google/chrome/chrome', '/usr/bin/chromium', '/usr/bin/google-chrome',
  '/snap/bin/chromium',
].find((candidate) => fs.existsSync(candidate));
assert.ok(chrome, 'Set CHROME to run the model versions browser regression');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-model-versions-'));
let browser;
let vite;
try {
  vite = await createServer({
    root: path.join(appRoot, 'src'), configFile: false, appType: 'mpa', logLevel: 'error',
    plugins: [{
      name: 'model-versions-regression',
      configureServer(server) {
        server.middlewares.use('/versions.html', (_request, response) => {
          response.setHeader('content-type', 'text/html; charset=utf-8');
          response.end(`<!doctype html><html><body><div id="root"></div><script>
            window.$RefreshReg$ = () => {};
            window.$RefreshSig$ = () => type => type;
            window.__vite_plugin_react_preamble_installed__ = true;
          </script></body></html>`);
        });
      },
    }, react()],
    server: { host: '127.0.0.1', port: 0 },
  });
  await vite.listen();
  browser = await puppeteer.launch({
    executablePath: chrome, headless: true, userDataDir: profile,
    args: ['--no-sandbox', '--disable-gpu', '--no-first-run'],
  });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/versions.html`);
  await page.evaluate(async () => {
    // These imports execute in the browser and are served by the Vite fixture.
    /* eslint-disable import-x/no-unresolved, n/no-missing-import */
    const reactModule = await import('/@id/react');
    const { createElement } = reactModule.default || reactModule;
    const client = await import('/@id/react-dom/client');
    const createRoot = client.createRoot || client.default?.createRoot;
    const { ModelVersionsDialog } = await import('/features/model-store/ModelVersionsDialog.jsx');
    const { dictZh } = await import('/shared/i18n/zh.js');
    /* eslint-enable import-x/no-unresolved, n/no-missing-import */
    const root = createRoot(document.getElementById('root'));
    const record = { card_id: 'isolated-test', card_name: 'Test', running: false };
    window.pendingVersions = [];
    window.__TAURI__ = { core: { invoke: (command) => {
      if (command !== 'model_store_versions') throw Error(`Unexpected command: ${command}`);
      return new Promise((resolve) => window.pendingVersions.push(resolve));
    } } };
    window.renderVersions = (busy) => root.render(createElement(ModelVersionsDialog, {
      record, copy: dictZh.uiModelStore, theme: 'light', busy,
      onClose() {}, onAction() {},
    }));
    window.answerVersions = (state) => window.pendingVersions.shift()([{
      version_id: 'test-version', filename: 'test.weights', state,
      can_switch: state === 'ready', can_retry: state === 'failed',
      can_delete: state !== 'downloading', size_bytes: 128,
    }]);
    window.renderVersions(true);
  });
  const waitRequest = () => page.waitForFunction(() => window.pendingVersions.length > 0);
  const answer = async (state) => {
    await waitRequest();
    await page.evaluate((value) => window.answerVersions(value), state);
  };
  const expectActions = async (count) => {
    await page.waitForFunction((expected) => {
      const buttons = [...document.querySelectorAll('[data-testid="model-version-row"] button')];
      return buttons.length === expected && buttons.every((button) => !button.disabled);
    }, {}, count);
  };
  // The dialog is opened while a download is still running. Success and failure
  // must expose their actions in the same mounted dialog, including cancellation.
  for (const state of ['ready', 'failed', 'failed']) {
    await answer('downloading');
    await page.waitForSelector('[data-testid="model-version-row"]');
    await page.evaluate(() => window.renderVersions(false));
    await answer(state);
    await expectActions(2);
    await page.evaluate(() => window.renderVersions(true));
  }
  // A response from the previous busy state cannot overwrite the newer result.
  await waitRequest();
  await page.evaluate(() => {
    window.staleVersions = window.pendingVersions.shift();
    window.renderVersions(false);
  });
  await answer('ready');
  await expectActions(2);
  await page.evaluate(() => window.staleVersions([{
    version_id: 'test-version', state: 'downloading', can_switch: false, can_delete: false,
  }]));
  await expectActions(2);
  assert.deepEqual(errors, []);
  console.log('PASS: versions refresh after success/failure/cancellation and ignore stale responses');
} finally {
  await browser?.close();
  await vite?.close();
  fs.rmSync(profile, { recursive: true, force: true });
}
