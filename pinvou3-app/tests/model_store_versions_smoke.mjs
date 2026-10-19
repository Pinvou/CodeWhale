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
      enforce: 'pre',
      transform(source, id) {
        // Expose the real form only inside this isolated browser fixture.
        if (id.split('?')[0] === path.join(appRoot, 'src/features/settings/SettingsView.jsx')) {
          return `${source}\nexport { ModelFormModal as __TestModelForm };\n`;
        }
      },
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
  await page.reload();
  await page.evaluate(async () => {
    // The real settings form stays mounted while its model props refresh, just
    // like a background models:changed event in the application.
    /* eslint-disable import-x/no-unresolved, n/no-missing-import */
    const reactModule = await import('/@id/react');
    const { createElement } = reactModule.default || reactModule;
    const client = await import('/@id/react-dom/client');
    const { __TestModelForm } = await import('/features/settings/SettingsView.jsx');
    const { dictZh } = await import('/shared/i18n/zh.js');
    /* eslint-enable import-x/no-unresolved, n/no-missing-import */
    const createRoot = client.createRoot || client.default?.createRoot;
    const root = createRoot(document.getElementById('root'));
    window.savedFormPayloads = [];
    window.legacyModel = {
      id: 'isolated-legacy', name: 'Legacy model', preset: 'openai_compatible',
      model: 'old-wire-model', base_url: 'http://127.0.0.1:8911/v1',
      has_secret: true, credential_state: 'configured', context_window_tokens: 32000,
    };
    window.renderModelForm = (initial, key = 'existing') => root.render(createElement(__TestModelForm, {
      key, initial, isDark: false, t: dictZh,
      bs: { platformCapabilities: { localVllmSupported: true } },
      models: [initial], onCancel() {},
      onSave: async (payload) => { window.savedFormPayloads.push(payload); },
    }));
    window.renderModelForm(window.legacyModel);
  });
  await page.waitForSelector('[data-testid="model-form-alias"]');
  // A legacy form opened before migration has no provenance yet. Updating its
  // props must retain the original route snapshot and the user's form state.
  await page.evaluate(() => window.renderModelForm({
    ...window.legacyModel, model_store_card_id: 'isolated-card',
    model: 'new-wire-model', base_url: 'http://127.0.0.1:8912/v1',
  }));
  await page.type('[data-testid="model-form-alias"]', 'My local alias');
  await page.click('[data-testid="model-form-context-window"]');
  await page.keyboard.down('Control');
  await page.keyboard.press('A');
  await page.keyboard.up('Control');
  await page.type('[data-testid="model-form-context-window"]', '64000');
  await page.click('[data-testid="model-form-save"]');
  await page.waitForFunction(() => window.savedFormPayloads.length === 1);
  const unchangedRoute = await page.evaluate(() => window.savedFormPayloads[0]);
  assert.equal(unchangedRoute.preserve_model_store_route, true);
  assert.equal(unchangedRoute.model, 'old-wire-model');
  assert.equal(unchangedRoute.base_url, 'http://127.0.0.1:8911/v1');
  assert.equal(unchangedRoute.alias, 'My local alias');
  assert.equal(unchangedRoute.context_window_tokens, 64000);
  const routeInput = await page.$('input[value="http://127.0.0.1:8911/v1"]');
  assert.ok(routeInput, 'the existing form still displays its original route');
  await routeInput.click();
  await page.keyboard.down('Control');
  await page.keyboard.press('A');
  await page.keyboard.up('Control');
  await routeInput.type('http://127.0.0.1:8999/v1');
  await page.click('[data-testid="model-form-save"]');
  await page.waitForFunction(() => window.savedFormPayloads.length === 2);
  const editedRoute = await page.evaluate(() => window.savedFormPayloads[1]);
  assert.equal(editedRoute.preserve_model_store_route, false);
  assert.equal(editedRoute.base_url, 'http://127.0.0.1:8999/v1');
  // New drafts must never opt into retaining some existing persisted route,
  // even when the draft has an id and its route inputs remain unchanged.
  await page.evaluate(() => window.renderModelForm({
    ...window.legacyModel, __new: true, preset: 'local_vllm',
  }, 'new'));
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-form-alias"]'));
  await page.click('[data-testid="model-form-save"]');
  await page.waitForFunction(() => window.savedFormPayloads.length === 3);
  assert.equal(await page.evaluate(() => window.savedFormPayloads[2].preserve_model_store_route), false);
  assert.deepEqual(errors, []);
  console.log('PASS: versions refresh after success/failure/cancellation and ignore stale responses');
  console.log('PASS: existing settings forms preserve untouched routes after migration; edited routes and new drafts opt out');
} finally {
  await browser?.close();
  await vite?.close();
  fs.rmSync(profile, { recursive: true, force: true });
}
