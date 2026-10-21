import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import puppeteer from 'puppeteer-core';
import { createServer } from 'vite';
import { matchesViteModule } from './helpers/vite-fixture-module.mjs';

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
    // Worktrees can share node_modules; optimizer caches must stay fixture-local.
    cacheDir: path.join(profile, 'vite-cache'),
    plugins: [{
      name: 'model-versions-regression',
      enforce: 'pre',
      transform(source, id) {
        // Expose the real form only inside this isolated browser fixture.
        if (matchesViteModule(id, path.join(appRoot, 'src/features/settings/SettingsView.jsx'))) {
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
    window.answerVersions = ({ state, error = null }) => window.pendingVersions.shift()([{
      version_id: 'test-version', filename: 'test.weights', state,
      can_switch: state === 'ready', can_retry: state === 'failed',
      can_delete: state !== 'downloading', size_bytes: 128, error,
    }]);
    window.interruptedCopy = dictZh.uiModelStore.versionInterrupted;
    window.renderVersions(true);
  });
  const waitRequest = () => page.waitForFunction(() => window.pendingVersions.length > 0);
  const answer = async (state, error = null) => {
    await waitRequest();
    await page.evaluate((value) => window.answerVersions(value), { state, error });
  };
  const expectActions = async (count) => {
    await page.waitForFunction((expected) => {
      const buttons = [...document.querySelectorAll('[data-testid="model-version-row"] button')];
      return buttons.length === expected && buttons.every((button) => !button.disabled);
    }, {}, count);
  };
  // The dialog is opened while a download is still running. Success and failure
  // must expose their actions in the same mounted dialog, including cancellation.
  for (const [state, error] of [['ready', null], ['failed', 'test download failure'], ['failed', 'version_download_interrupted']]) {
    await answer('downloading');
    await page.waitForSelector('[data-testid="model-version-row"]');
    await page.evaluate(() => window.renderVersions(false));
    await answer(state, error);
    await expectActions(2);
    if (error === 'version_download_interrupted') {
      assert.equal(await page.$eval('[data-testid="model-version-row"] [role="alert"]', (element) => element.textContent),
        await page.evaluate(() => window.interruptedCopy));
    }
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
    /* eslint-disable import-x/no-unresolved, n/no-missing-import */
    const reactModule = await import('/@id/react');
    const { createElement } = reactModule.default || reactModule;
    const client = await import('/@id/react-dom/client');
    const { CardUpdateDialog } = await import('/features/model-store/CardUpdateDialog.jsx');
    const { dictZh } = await import('/shared/i18n/zh.js');
    /* eslint-enable import-x/no-unresolved, n/no-missing-import */
    window.__TAURI__ = { core: { invoke: async (command) => {
      if (command !== 'model_store_check_card_update') throw Error(`Unexpected command: ${command}`);
      return { changed: true, compatibility: { kind: 'unverified', reason: 'future_unknown_reason', can_update: false, can_stage: false } };
    } } };
    window.unknownCompatibilityCopy = dictZh.uiModelStore.compatibilityReasonUnknown;
    const createRoot = client.createRoot || client.default?.createRoot;
    createRoot(document.getElementById('root')).render(createElement(CardUpdateDialog, {
      record: { card_id: 'isolated-card', card_name: 'Test' }, copy: dictZh.uiModelStore,
      theme: 'light', busy: false, onClose() {}, onUpdate() {}, onStage() {},
    }));
  });
  await page.waitForSelector('[data-testid="card-update-compatibility"]');
  assert.ok(await page.evaluate(() => document.querySelector('[data-testid="card-update-compatibility"]').textContent.includes(window.unknownCompatibilityCopy)));
  await page.reload();
  await page.evaluate(async () => {
    /* eslint-disable import-x/no-unresolved, n/no-missing-import */
    const reactModule = await import('/@id/react');
    const { createElement } = reactModule.default || reactModule;
    const client = await import('/@id/react-dom/client');
    const { ModelStoreView } = await import('/features/model-store/ModelStoreView.jsx');
    const { dictZh } = await import('/shared/i18n/zh.js');
    /* eslint-enable import-x/no-unresolved, n/no-missing-import */
    const record = { card_id: 'isolated-card', card_name: 'Test', model_name: 'test-wire',
      status: 'deployed', running: false, size_bytes: 128, deploy_topology: 'single' };
    window.cleanupFinished = false;
    window.uninstallFinished = false;
    window.__TAURI__ = { core: { invoke: async (command, payload) => {
      if (command === 'model_store_availability') return { available: true };
      if (command === 'model_store_cards' || command === 'model_store_active_runs') return [];
      if (command === 'model_store_library') return { models: window.uninstallFinished ? [] : [record], corrected: [] };
      if (command === 'model_store_test_store_state') return { enabled: false };
      if (command === 'model_store_take_startup_cleanup') return null;
      if (command === 'model_store_start_lifecycle' && payload.action === 'uninstall') return { run_id: 'isolated-uninstall' };
      if (command === 'model_store_run_log') {
        window.uninstallFinished = true;
        return { done: true, ok: true, lines: [], next: 0, result: { link_sync_error: 'test sync failure', purged_weights: false } };
      }
      throw Error(`Unexpected command: ${command}`);
    } } };
    window.TauriBridge = { models: { deleteModelIfStoreLink: async (id, cardId) => {
      if (id !== 'isolated-linked-row' || cardId !== record.card_id) throw Error('cleanup ownership mismatch');
      window.cleanupFinished = true;
      return true;
    } } };
    window.syncFailureCopy = dictZh.uiModelStore.linkSyncFailed('test sync failure');
    const createRoot = client.createRoot || client.default?.createRoot;
    createRoot(document.getElementById('root')).render(createElement(ModelStoreView, {
      theme: 'light', t: dictZh,
      bs: { savedModels: [{ id: 'isolated-linked-row', model_store_card_id: record.card_id, base_url: 'http://127.0.0.1:8911/v1' }] },
    }));
  });
  await page.waitForSelector('[data-testid="model-store-uninstall"]');
  await page.click('[data-testid="model-store-uninstall"]');
  await page.waitForSelector('[data-testid="model-store-uninstall-keep"]');
  await page.click('[data-testid="model-store-uninstall-keep"]');
  await page.waitForFunction(() => window.cleanupFinished);
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.$eval('[data-testid="model-store-toast"]', (element) => element.textContent),
    await page.evaluate(() => window.syncFailureCopy));
  await page.reload();
  await page.evaluate(async () => {
    /* eslint-disable import-x/no-unresolved, n/no-missing-import */
    const reactModule = await import('/@id/react');
    const { createElement } = reactModule.default || reactModule;
    const client = await import('/@id/react-dom/client');
    const { ModelDetailDialog } = await import('/features/model-store/ModelDetailDialog.jsx');
    const { dictZh } = await import('/shared/i18n/zh.js');
    /* eslint-enable import-x/no-unresolved, n/no-missing-import */
    const createRoot = client.createRoot || client.default?.createRoot;
    const root = createRoot(document.getElementById('root'));
    const record = { card_id: 'isolated-card', card_name: 'Test', model_name: 'old-wire',
      endpoint_model: 'old-wire', endpoint_url: 'http://127.0.0.1:8911/v1',
      running: true, status: 'deployed' };
    window.savedDetailPayloads = [];
    window.detailActiveCalls = 0;
    window.detailToast = null;
    window.staleLinkError = 'model store endpoint changed or is unavailable; refresh the card and retry';
    window.staleLinkCopy = dictZh.uiModelStore.linkEndpointChanged;
    window.contextRequests = [];
    window.failContext = true;
    window.__TAURI__ = { core: { invoke: async (command, payload) => {
      if (command === 'model_store_follow_deployment_context') {
        window.contextRequests.push(payload);
        if (window.failContext) throw Error(window.contextError || 'stale endpoint');
        window.renderDetail('old-wire', record.endpoint_url, null);
        return { ok: true };
      }
      if (command === 'model_store_model_detail') return { record };
      if (command === 'model_store_api_keys') return [];
      throw Error(`Unexpected command: ${command}`);
    } } };
    window.TauriBridge = { models: {
      saveModel: async (model) => {
        window.savedDetailPayloads.push(model);
        throw Error(window.staleLinkError);
      },
      setActiveModel: async () => { window.detailActiveCalls += 1; },
    } };
    window.renderDetail = (model, baseUrl, contextWindow = 32768) => root.render(createElement(ModelDetailDialog, {
      cardId: record.card_id, copy: dictZh.uiModelStore, theme: 'light', onClose() {},
      onToast: (message) => { window.detailToast = message; },
      savedModels: [{ id: 'existing-store-link', model_store_card_id: record.card_id,
        model, base_url: baseUrl, provider_kind: 'custom', context_window_tokens: contextWindow }],
    }));
    window.renderDetail('old-wire', record.endpoint_url);
  });
  await page.waitForSelector('[data-testid="model-store-use-in-pinvou"]');
  // Background route synchronization updates props while the same detail dialog
  // still displays the reviewed old endpoint. Reusing the id is an explicit link.
  await page.evaluate(() => window.renderDetail('new-wire', 'http://127.0.0.1:8912/v1'));
  await page.click('[data-testid="model-store-use-in-pinvou"]');
  await page.waitForFunction(() => window.detailToast === window.staleLinkCopy);
  const staleDetailLink = await page.evaluate(() => window.savedDetailPayloads[0]);
  assert.equal(staleDetailLink.id, 'existing-store-link');
  assert.equal(staleDetailLink.model, 'old-wire');
  assert.equal(staleDetailLink.base_url, 'http://127.0.0.1:8911/v1');
  assert.equal(staleDetailLink.model_store_link_requested, true);
  assert.equal(await page.evaluate(() => window.detailActiveCalls), 0);
  // Restoring automatic context is a narrow backend patch. It must not re-import
  // the model, replace other options, or change global/session selection.
  await page.click('[data-testid="model-store-follow-context"]');
  await page.waitForFunction(() => window.detailToast?.includes('无法恢复跟随部署'));
  assert.equal(await page.$eval('[data-testid="model-store-follow-context"]', (button) => button.disabled), false);
  await page.evaluate(() => { window.contextError = 'store_context_follow_unsupported'; });
  await page.click('[data-testid="model-store-follow-context"]');
  await page.waitForFunction(() => window.detailToast?.includes('后台商店需要更新'));
  await page.evaluate(() => { window.contextError = 'store_protocol: incompatible host'; window.detailToast = null; });
  await page.click('[data-testid="model-store-follow-context"]');
  await page.waitForFunction(() => window.detailToast?.includes('后台商店需要更新'));
  await page.evaluate(() => { window.failContext = false; });
  await page.click('[data-testid="model-store-follow-context"]');
  await page.waitForFunction(() => window.detailToast?.includes('已恢复跟随部署'));
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-store-follow-context"]'));
  assert.deepEqual(await page.evaluate(() => window.contextRequests), [
    { cardId: 'isolated-card', modelId: 'existing-store-link' },
    { cardId: 'isolated-card', modelId: 'existing-store-link' },
    { cardId: 'isolated-card', modelId: 'existing-store-link' },
    { cardId: 'isolated-card', modelId: 'existing-store-link' },
  ]);
  assert.equal(await page.evaluate(() => window.detailActiveCalls), 0);
  assert.equal(await page.evaluate(() => window.savedDetailPayloads.length), 1);
  assert.match(await page.$eval('[data-testid="model-store-context-settings"]', (node) => node.textContent), /上下文自动跟随部署/);
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
  console.log('PASS: unknown compatibility reasons remain visible; uninstall cleanup preserves sync failures');
  console.log('PASS: stale details retain explicit re-link intent and surface rejection without changing the active model');
} finally {
  await browser?.close();
  await vite?.close();
  fs.rmSync(profile, { recursive: true, force: true });
}
