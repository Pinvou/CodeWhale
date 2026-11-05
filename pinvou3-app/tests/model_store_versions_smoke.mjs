import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';
import { fixtureScope } from './helpers/fixture-scope.mjs';
import { matchesViteModule } from './helpers/vite-fixture-module.mjs';
import { exerciseLatestLinkedCleanup } from './helpers/model-store-cleanup-smoke.mjs';

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || [
  '/opt/google/chrome/chrome', '/usr/bin/chromium', '/usr/bin/google-chrome',
  '/snap/bin/chromium',
].find((candidate) => fs.existsSync(candidate));
assert.ok(chrome, 'Set CHROME to run the model versions browser regression');
let browser;
let vite;
let primaryFailure;
let hasPrimaryFailure = false;
try {
  vite = await createServer({
    root: path.join(appRoot, 'src'), configFile: false, appType: 'mpa', logLevel: 'error',
    // The shared factory owns an isolated optimizer cache and loopback listener.
    plugins: [{
      name: 'model-versions-regression',
      enforce: 'pre',
      transform(source, id) {
        // Expose the real form only inside this isolated browser fixture.
        if (matchesViteModule(id, path.join(appRoot, 'src/features/model-store/useModelStoreStartupUpdates.js'))) {
          // Use the hook's optimized React instance in the browser probe too.
          return `${source}\nimport * as __TestReact from 'react'; export { __TestReact };\n`;
        }
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
    executablePath: chrome, headless: true,
    args: ['--no-sandbox', '--disable-gpu', '--no-first-run'],
  });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/versions.html`);
  await exerciseLatestLinkedCleanup(page);
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
    window.versionCostCopy = dictZh.uiModelStore.versionCostHint;
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
    assert.equal(await page.$eval('[data-testid="model-version-cost-hint"]', (node) => node.textContent), await page.evaluate(() => window.versionCostCopy));
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
  assert.equal(await page.$('[data-testid="model-version-mtp-network-hint"]'), null,
    'other cards must not inherit the built-in MTP template disclosure');
  // Both entry points must show the active language's disclosure without
  // implying that redeployment migrated an existing .env or a remote recipe.
  const networkHintCopies = [];
  for (const [language, marker] of [['zh', '内置 MTP 256K'], ['en', 'built-in MTP 256K'], ['ja', '内蔵 MTP 256K']]) {
    await page.reload();
    const expectedHint = await page.evaluate(async (locale) => {
      /* eslint-disable import-x/no-unresolved, n/no-missing-import */
      const reactModule = await import('/@id/react');
      const { createElement } = reactModule.default || reactModule;
      const client = await import('/@id/react-dom/client');
      const { ModelStoreView } = await import('/features/model-store/ModelStoreView.jsx');
      const { dictZh } = await import('/shared/i18n/zh.js');
      const { dictEn } = await import('/shared/i18n/en.js');
      const { dictJa } = await import('/shared/i18n/ja.js');
      /* eslint-enable import-x/no-unresolved, n/no-missing-import */
      const t = { zh: dictZh, en: dictEn, ja: dictJa }[locale];
      const copy = t.uiModelStore;
      if (!Object.hasOwn(copy, 'mtpNetworkHint')) throw Error(`${locale}: missing network disclosure`);
      const records = ['qwen38-27b-nvfp4-mtp', 'network-neutral'].map((id) => ({
        card_id: id, card_name: id, model_name: id, status: 'deployed', running: false,
        size_bytes: 128, deploy_topology: 'single', card_update_pending: true,
        default_profile_id: 'production-sampling', profiles: [
          { id: 'benchmark', name: 'Benchmark' }, { id: 'production-sampling', name: 'Production' },
        ],
      }));
      window.__TAURI__ = { core: { invoke: async (command) => {
        if (command === 'model_store_availability') return { available: true };
        if (command === 'model_store_cards' || command === 'model_store_active_runs' || command === 'model_store_versions') return [];
        if (command === 'model_store_library') return { models: records, corrected: [] };
        if (command === 'model_store_test_store_state') return { enabled: false };
        if (command === 'model_store_take_startup_cleanup') return null;
        throw Error(`Unexpected command: ${command}`);
      } } };
      window.openMtpNetworkHistory = () => {
        const row = document.querySelector('[data-card-id="qwen38-27b-nvfp4-mtp"]');
        [...row.querySelectorAll('button')].find((button) => button.textContent === copy.versionHistory).click();
      };
      const createRoot = client.createRoot || client.default?.createRoot;
      createRoot(document.getElementById('root')).render(createElement(ModelStoreView, {
        theme: 'light', t, bs: { savedModels: [] },
      }));
      return copy.mtpNetworkHint;
    }, language);
    assert.ok(expectedHint.includes(marker), `${language}: must use its own translated disclosure`);
    for (const fact of ['127.0.0.1', '0.0.0.0', '.env', 'API']) assert.ok(expectedHint.includes(fact));
    networkHintCopies.push(expectedHint);
    await page.waitForSelector('[data-card-id="qwen38-27b-nvfp4-mtp"] [data-testid="model-store-deploy"]');
    await page.click('[data-card-id="qwen38-27b-nvfp4-mtp"] [data-testid="model-store-deploy"]');
    await page.waitForSelector('[data-testid="model-store-mtp-network-hint"]');
    assert.equal(await page.$eval('[data-testid="model-store-mtp-network-hint"]', (node) => node.textContent), expectedHint);
    assert.equal(await page.$eval('[data-testid="model-store-profile-dialog"] input:checked', (input) => input.closest('label').textContent.includes('256K')), true,
      'the network disclosure must preserve the existing 256K production default');
    await page.keyboard.press('Escape');
    await page.evaluate(() => window.openMtpNetworkHistory());
    await page.waitForSelector('[data-testid="model-version-mtp-network-hint"]');
    assert.equal(await page.$eval('[data-testid="model-version-mtp-network-hint"]', (node) => node.textContent), expectedHint);
    await page.keyboard.press('Escape');
    await page.click('[data-card-id="network-neutral"] [data-testid="model-store-deploy"]');
    await page.waitForSelector('[data-testid="model-store-profile-dialog"]');
    assert.equal(await page.$('[data-testid="model-store-mtp-network-hint"]'), null,
      'other deployment profiles must not claim the built-in MTP network defaults');
  }
  assert.equal(new Set(networkHintCopies).size, 3, 'all three disclosures must be translated without fallback');
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
  // Legacy source matching intentionally retains configuration-only updates,
  // including zero/unknown size metadata, without claiming verified weights.
  const legacyCopies = [];
  for (const [locale, oldSize, nextSize] of [['zh', 0, 23], ['en', 23, 0], ['ja', 0, 0]]) {
    await page.reload();
    const notice = await page.evaluate(async ({ locale, oldSize, nextSize }) => {
      /* eslint-disable import-x/no-unresolved, n/no-missing-import */
      const reactModule = await import('/@id/react');
      const { createElement } = reactModule.default || reactModule;
      const client = await import('/@id/react-dom/client');
      const { CardUpdateDialog } = await import('/features/model-store/CardUpdateDialog.jsx');
      const { dictZh } = await import('/shared/i18n/zh.js');
      const { dictEn } = await import('/shared/i18n/en.js');
      const { dictJa } = await import('/shared/i18n/ja.js');
      /* eslint-enable import-x/no-unresolved, n/no-missing-import */
      const copy = { zh: dictZh, en: dictEn, ja: dictJa }[locale].uiModelStore;
      window.legacyUpdate = null;
      window.__TAURI__ = { core: { invoke: async (command) => {
        if (command !== 'model_store_check_card_update') throw Error(`Unexpected command: ${command}`);
        return { changed: true, revision: 'legacy-next', compatibility: {
          kind: 'compatible', reason: 'unchanged_legacy_identity', can_update: true, can_stage: false,
          changes: [{ field: 'size_bytes', current: oldSize, next: nextSize }],
        } };
      } } };
      const createRoot = client.createRoot || client.default?.createRoot;
      createRoot(document.getElementById('root')).render(createElement(CardUpdateDialog, {
        record: { card_id: 'legacy-card', card_name: 'Legacy', size_bytes: oldSize }, copy,
        theme: 'light', busy: false, onClose() {}, onUpdate(...args) { window.legacyUpdate = args; }, onStage() {},
      }));
      return { unverified: copy.compatibilityLegacyUnverified, verified: copy.compatibilityKinds.compatible };
    }, { locale, oldSize, nextSize });
    await page.waitForSelector('[data-testid="card-update-compatibility"]');
    const text = await page.$eval('[data-testid="card-update-compatibility"]', (node) => node.textContent);
    assert.ok(text.includes(notice.unverified));
    assert.ok(!text.includes(notice.verified), `${locale}: legacy metadata must not claim verified weight identity`);
    assert.equal(await page.$('[data-testid="model-store-stage-version"]'), null);
    assert.equal(await page.$eval('[data-testid="model-store-apply-card-update"]', (button) => button.disabled), false);
    await page.click('[data-testid="model-store-apply-card-update"]');
    assert.deepEqual(await page.evaluate(() => [window.legacyUpdate[0].card_id, window.legacyUpdate[1]]), ['legacy-card', 'legacy-next']);
    legacyCopies.push(notice.unverified);
  }
  assert.equal(new Set(legacyCopies).size, 3);

  await page.reload();
  await page.evaluate(async () => {
    /* eslint-disable import-x/no-unresolved, n/no-missing-import */
    const reactModule = await import('/@id/react');
    const { createElement } = reactModule.default || reactModule;
    const client = await import('/@id/react-dom/client');
    const { ModelStoreOperationResults } = await import('/features/model-store/ModelStoreOperationResults.jsx');
    const { rememberTaskOperationResult, rememberForceRemoveResult } = await import('/features/model-store/operation-results.js');
    const { dictZh } = await import('/shared/i18n/zh.js');
    await import('/styles/tailwind.css');
    /* eslint-enable import-x/no-unresolved, n/no-missing-import */
    for (let i = 0; i < 40; i += 1) rememberTaskOperationResult({ runId: `scroll-${i}`, title: `Card ${i}`, kind: 'version_switch' }, { ok: true, result: {} });
    rememberForceRemoveResult({ taskId: 'scroll-residual', cardName: 'Residual' }, { residual_paths: ['/retained/last.weights'], warning: 'force_remove_residual_paths' });
    const createRoot = client.createRoot || client.default?.createRoot;
    createRoot(document.getElementById('root')).render(createElement(ModelStoreOperationResults, { copy: dictZh.uiModelStore }));
  });
  await page.waitForSelector('[data-testid="model-version-results"]');
  assert.equal(await page.$$eval('[data-testid="model-version-operation-result"]', (nodes) => nodes.length), 41);
  const region = await page.$eval('[data-testid="model-version-results"]', (node) => ({
    height: node.clientHeight, content: node.scrollHeight, overflow: getComputedStyle(node).overflowY,
  }));
  assert.ok(region.height <= 320 && region.content > region.height);
  assert.equal(region.overflow, 'auto');
  // The semantic region adds no extra tab stop: navigate actual actions and
  // let the browser scroll the focused acknowledgement into view.
  await page.focus('[data-testid="model-version-operation-result"] button');
  for (let index = 0; index < 40; index += 1) await page.keyboard.press('Tab');
  assert.ok(await page.$eval('[data-testid="model-version-operation-result"]:last-child button', (button) => document.activeElement === button));
  assert.ok(await page.$eval('[data-testid="model-version-results"]', (node) => node.scrollTop > 0));
  await page.keyboard.press('Enter');
  assert.equal(await page.$$eval('[data-testid="model-version-operation-result"]', (nodes) => nodes.length), 40);
  assert.equal(await page.$('[data-testid="model-version-cleanup-result"]'), null, 'the last residual acknowledgement must remain reachable by scrolling');

  await page.reload();
  await page.evaluate(async () => {
    /* eslint-disable import-x/no-unresolved, n/no-missing-import */
    const reactModule = await import('/@id/react');
    const { createElement } = reactModule.default || reactModule;
    const client = await import('/@id/react-dom/client');
    const { StorageSettingsDialog } = await import('/features/model-store/StorageSettingsDialog.jsx');
    const { dictZh } = await import('/shared/i18n/zh.js');
    /* eslint-enable import-x/no-unresolved, n/no-missing-import */
    window.settingsPolicy = { models_dir: '/safe/models', import_scan_dirs: ['/keep/import'], scan_dirs: [], startup_update_check_enabled: false };
    window.settingsSaveFails = true;
    window.settingsWrites = [];
    window.__TAURI__ = { core: { invoke: async (command, payload) => {
      if (command === 'model_store_storage_settings') return { ...window.settingsPolicy };
      if (command === 'model_store_set_storage_settings') {
        window.settingsWrites.push(payload);
        if (window.settingsSaveFails) throw Error('permission denied');
        window.settingsPolicy = { ...window.settingsPolicy, models_dir: payload.modelsDir, import_scan_dirs: payload.importScanDirs,
          startup_update_check_enabled: payload.startupUpdateCheckEnabled === null ? window.settingsPolicy.startup_update_check_enabled : payload.startupUpdateCheckEnabled };
        return { ...window.settingsPolicy };
      }
      throw Error(`Unexpected command: ${command}`);
    } } };
    const createRoot = client.createRoot || client.default?.createRoot;
    const root = createRoot(document.getElementById('root'));
    window.renderStartupSettings = (open) => root.render(createElement(StorageSettingsDialog, {
      open, copy: dictZh.uiModelStore, theme: 'light', onClose() { window.renderStartupSettings(false); },
    }));
    window.renderStartupSettings(true);
  });
  await page.waitForFunction(() => { const input = document.querySelector('[data-testid="model-store-startup-update-check"]'); return input && !input.disabled; });
  assert.equal(await page.$eval('[data-testid="model-store-startup-update-check"]', (input) => input.checked), false);
  await page.click('[data-testid="model-store-startup-update-check"]');
  await page.click('[data-testid="model-store-storage-save"]');
  await page.waitForSelector('[data-testid="model-store-storage-error"]');
  assert.equal(await page.$eval('[data-testid="model-store-startup-update-check"]', (input) => input.checked), true);
  assert.equal(await page.$eval('[data-testid="model-store-storage-save"]', (button) => button.disabled), false);
  await page.evaluate(() => { window.settingsSaveFails = false; });
  await page.click('[data-testid="model-store-startup-update-check"]');
  await page.click('[data-testid="model-store-storage-save"]');
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-store-storage-dialog"]'));
  assert.deepEqual(await page.evaluate(() => window.settingsWrites.map((entry) => entry.startupUpdateCheckEnabled)), [true, false]);
  await page.evaluate(() => window.renderStartupSettings(true));
  await page.waitForFunction(() => { const input = document.querySelector('[data-testid="model-store-startup-update-check"]'); return input && !input.disabled; });
  assert.equal(await page.$eval('[data-testid="model-store-startup-update-check"]', (input) => input.checked), false);
  assert.equal(await page.$eval('[data-testid="model-store-import-dirs"]', (input) => input.value), '/keep/import');
  // Window A loads true; another window saves false before A edits only a path.
  // A must submit no policy intent and preserve the newer persisted false.
  await page.evaluate(() => window.renderStartupSettings(false));
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-store-storage-dialog"]'));
  await page.evaluate(() => { window.settingsPolicy.startup_update_check_enabled = true; window.renderStartupSettings(true); });
  await page.waitForFunction(() => { const input = document.querySelector('[data-testid="model-store-startup-update-check"]'); return input && !input.disabled; });
  assert.equal(await page.$eval('[data-testid="model-store-startup-update-check"]', (input) => input.checked), true);
  await page.evaluate(() => { window.settingsPolicy.startup_update_check_enabled = false; });
  await page.click('[data-testid="model-store-models-dir"]', { clickCount: 3 });
  await page.type('[data-testid="model-store-models-dir"]', '/safe/updated-models');
  await page.click('[data-testid="model-store-storage-save"]');
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-store-storage-dialog"]'));
  assert.equal(await page.evaluate(() => window.settingsWrites.at(-1).startupUpdateCheckEnabled), null);
  assert.equal(await page.evaluate(() => window.settingsPolicy.startup_update_check_enabled), false);
  await page.evaluate(() => window.renderStartupSettings(true));
  await page.waitForFunction(() => { const input = document.querySelector('[data-testid="model-store-startup-update-check"]'); return input && !input.disabled; });
  assert.equal(await page.$eval('[data-testid="model-store-startup-update-check"]', (input) => input.checked), false);
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
      if (command === 'list_models') return { models: [{ id: 'isolated-linked-row', model_store_card_id: record.card_id,
        base_url: 'http://127.0.0.1:8911/v1' }] };
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
    const { ModelStoreView } = await import('/features/model-store/ModelStoreView.jsx');
    const { dictZh } = await import('/shared/i18n/zh.js');
    /* eslint-enable import-x/no-unresolved, n/no-missing-import */
    const createRoot = client.createRoot || client.default?.createRoot;
    const root = createRoot(document.getElementById('root'));
    const records = ['alpha', 'beta'].map((id) => ({ card_id: id, card_name: id,
      model_name: id, status: 'deployed', running: false, size_bytes: 128, deploy_topology: 'single' }));
    const completed = new Map();
    const pending = new Map();
    let serial = 0;
    window.versionRuns = [];
    window.versionCancelRequests = [];
    window.versionLibraryLoads = 0;
    window.versionCanSwitch = false;
    window.versionTaskCopy = dictZh.uiModelStore;
    window.finishVersionRun = (runId, result = {}, cancelled = false) => {
      const chunk = { done: true, ok: true, cancelled, lines: [], next: 0, result };
      completed.set(runId, chunk);
      pending.get(runId)?.(chunk);
    };
    window.__TAURI__ = { core: { invoke: async (command, payload) => {
      if (command === 'model_store_availability') return { available: true };
      if (command === 'model_store_cards' || command === 'model_store_active_runs') return [];
      if (command === 'model_store_library') {
        window.versionLibraryLoads += 1;
        return { models: records, corrected: [] };
      }
      if (command === 'list_models') return { models: [] };
      if (command === 'model_store_test_store_state') return { enabled: false };
      if (command === 'model_store_take_startup_cleanup') return null;
      if (command === 'model_store_check_card_update') return { changed: true, revision: 'new',
        compatibility: { kind: 'weights_changed', can_stage: true, can_update: false } };
      if (command === 'model_store_versions') return [{ version_id: `${payload.cardId}-old`,
        filename: 'old.weights', state: window.versionCanSwitch ? 'ready' : 'history',
        can_switch: window.versionCanSwitch, can_delete: true, size_bytes: 128 }];
      if (command === 'model_store_start_version_action') {
        const runId = `${payload.cardId}-${payload.action}-${++serial}`;
        window.versionRuns.push(runId);
        return { run_id: runId };
      }
      if (command === 'model_store_cancel_run') {
        window.versionCancelRequests.push(payload.runId);
        return { ok: true };
      }
      if (command === 'model_store_start_lifecycle' && payload.action === 'uninstall') return { run_id: `failed-uninstall-${payload.cardId}` };
      if (command === 'model_store_force_remove') {
        return new Promise((resolve) => { window.finishForceRemove = () => resolve({
          residual_paths: [`/${payload.cardId}/force-retained.weights`], warning: 'files retained',
        }); });
      }
      if (command === 'model_store_run_log') {
        if (payload.runId.startsWith('failed-uninstall-')) return { done: true, ok: false, lines: [], next: 0, error: 'uninstall failed' };
        if (completed.has(payload.runId)) return completed.get(payload.runId);
        return new Promise((resolve) => pending.set(payload.runId, resolve));
      }
      throw Error(`Unexpected command: ${command}`);
    } } };
    window.renderVersionStore = (visible) => root.render(visible ? createElement(ModelStoreView, {
      theme: 'light', t: dictZh, bs: { savedModels: [] },
    }) : null);
    window.openVersionHistory = (cardId) => {
      const row = document.querySelector(`[data-card-id="${cardId}"]`);
      [...row.querySelectorAll('button')].find((button) => button.textContent === dictZh.uiModelStore.versionHistory).click();
    };
    window.renderVersionStore(true);
  });
  const openStage = async () => {
    await page.waitForSelector('[data-card-id="alpha"] [data-testid="model-store-check-card-update"]');
    await page.click('[data-card-id="alpha"] [data-testid="model-store-check-card-update"]');
    await page.waitForSelector('[data-testid="model-store-stage-version"]');
    await page.click('[data-testid="model-store-stage-version"]');
  };
  const openHistory = async (cardId) => {
    await page.evaluate((id) => window.openVersionHistory(id), cardId);
    await page.waitForSelector('[data-testid="model-version-row"]');
  };
  // Long staging must preserve both kinds of dialog the user opened meanwhile.
  await openStage();
  await page.click('[data-card-id="beta"] [data-testid="model-store-check-card-update"]');
  await page.waitForSelector('[data-testid="model-store-card-update-dialog"]');
  await page.evaluate(() => window.finishVersionRun(window.versionRuns[0]));
  await page.waitForSelector('[data-testid="model-version-operation-result"]');
  assert.ok(await page.$eval('#card-update-title', (element) => element.textContent.includes('beta')));
  assert.equal(await page.$('[data-testid="model-store-versions-dialog"]'), null);
  await page.keyboard.press('Escape');
  await openStage();
  await openHistory('beta');
  await page.evaluate(() => window.finishVersionRun(window.versionRuns[1]));
  await page.waitForFunction(() => document.querySelectorAll('[data-testid="model-version-operation-result"]').length === 2);
  assert.ok(await page.$eval('#model-versions-title', (element) => element.textContent.includes('beta')));
  assert.equal(await page.$('[data-testid="model-store-card-update-dialog"]'), null);
  await page.keyboard.press('Escape');
  await page.$$eval('[data-testid="model-version-operation-result"] button', (buttons) => buttons.forEach((button) => button.click()));
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-version-results"]'));
  // Two independent cleanup results arrive while ModelStoreView is unmounted.
  // Remounting must disclose both path lists; acknowledging one cannot erase the other.
  for (const cardId of ['alpha', 'beta']) {
    await openHistory(cardId);
    await page.click('[data-testid="model-version-row"] button');
    await page.waitForSelector('[data-testid="model-version-delete-confirm"]');
    await page.$eval('[data-testid="model-version-delete-confirm"]', (element) => element.querySelectorAll('button')[1].click());
  }
  await page.evaluate(() => window.renderVersionStore(false));
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-store-view"]'));
  await page.evaluate(() => {
    window.finishVersionRun(window.versionRuns[2], { freed_bytes: 128, remaining_paths: ['/alpha/retained.weights'] });
    window.finishVersionRun(window.versionRuns[3], { freed_bytes: 256, remaining_paths: ['/beta/retained.weights'] });
  });
  // Let both task pumps settle before bringing the view back.
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  await page.evaluate(() => window.renderVersionStore(true));
  await page.waitForFunction(() => document.querySelectorAll('[data-testid="model-version-cleanup-result"]').length === 2);
  assert.deepEqual(await page.$$eval('[data-testid="model-version-cleanup-result"] li',
    (items) => items.map((item) => item.textContent)), ['/alpha/retained.weights', '/beta/retained.weights']);
  await page.click('[data-testid="model-version-operation-result"] button');
  await page.waitForFunction(() => document.querySelectorAll('[data-testid="model-version-cleanup-result"]').length === 1);
  await page.evaluate(() => window.renderVersionStore(false));
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-store-view"]'));
  await page.evaluate(() => window.renderVersionStore(true));
  await page.waitForSelector('[data-testid="model-version-cleanup-result"]');
  assert.deepEqual(await page.$$eval('[data-testid="model-version-cleanup-result"] li',
    (items) => items.map((item) => item.textContent)), ['/beta/retained.weights']);
  const beginForceRemove = async (cardId) => {
    await page.click(`[data-card-id="${cardId}"] [data-testid="model-store-uninstall"]`);
    await page.waitForSelector('[data-testid="model-store-uninstall-keep"]');
    await page.click('[data-testid="model-store-uninstall-keep"]');
    await page.waitForSelector('[data-testid="model-store-task-force-remove"]');
    await page.click('[data-testid="model-store-task-force-remove"]');
    await page.waitForSelector('[data-testid="model-store-force-remove-confirm"]');
    await page.click('[data-testid="model-store-force-remove-confirm"]');
  };
  await beginForceRemove('alpha');
  await page.waitForFunction(() => typeof window.finishForceRemove === 'function');
  await page.evaluate(() => window.renderVersionStore(false));
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-store-view"]'));
  await page.evaluate(() => window.finishForceRemove());
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  await page.evaluate(() => window.renderVersionStore(true));
  await page.waitForSelector('[data-operation-kind="force_remove"]');
  assert.deepEqual(await page.$$eval('[data-testid="model-version-cleanup-result"] li',
    (items) => items.map((item) => item.textContent)), ['/beta/retained.weights', '/alpha/force-retained.weights']);
  await page.click('[data-operation-kind="force_remove"] button');
  await page.evaluate(() => { window.finishForceRemove = null; });
  await beginForceRemove('beta');
  await page.waitForFunction(() => typeof window.finishForceRemove === 'function');
  await page.evaluate(() => window.finishForceRemove());
  await page.waitForSelector('[data-testid="model-store-force-remove-done"]');
  await page.click('[data-testid="model-store-force-remove-done"]');
  assert.deepEqual(await page.$$eval('[data-testid="model-version-cleanup-result"] li',
    (items) => items.map((item) => item.textContent)), ['/beta/retained.weights'],
  'closing the force-remove result only acknowledges its own queued disclosure');
  await page.$$eval('[data-testid="model-version-operation-result"] button', (buttons) => buttons.forEach((button) => button.click()));
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-version-results"]'));
  await page.evaluate(() => { window.versionCanSwitch = true; });
  const beginVersionAction = async (action) => {
    if (action === 'stage') await openStage();
    else {
      await openHistory('alpha');
      await page.evaluate((requested) => {
        const label = requested === 'switch' ? window.versionTaskCopy.versionSwitch : window.versionTaskCopy.versionDelete;
        [...document.querySelectorAll('[data-testid="model-version-row"] button')]
          .find((button) => button.textContent === label).click();
      }, action);
      if (action === 'delete') {
        await page.waitForSelector('[data-testid="model-version-delete-confirm"]');
        await page.$eval('[data-testid="model-version-delete-confirm"]', (element) => element.querySelectorAll('button')[1].click());
      }
    }
    await page.waitForSelector(`[data-task-kind="version_${action}"][data-task-status="running"]`);
    return page.evaluate(() => window.versionRuns.at(-1));
  };
  // The real task card exposes Close only after a terminal state. Calling the
  // low-level dismiss helper during a running operation is not a user UI path.
  for (const action of ['stage', 'delete', 'switch']) {
    const runId = await beginVersionAction(action);
    const taskSelector = `[data-testid="model-store-task"][data-task-kind="version_${action}"]`;
    const actions = () => page.$eval(taskSelector, (element) => ({
      close: [...element.querySelectorAll('button')].some((button) => button.textContent === window.versionTaskCopy.taskClose),
      cancel: element.querySelector('[data-testid="model-store-task-cancel"]')?.disabled ?? null,
    }));
    assert.deepEqual(await actions(), { close: false, cancel: false }, `${action}: running tasks can only be cancelled`);
    await page.click(`${taskSelector} [data-testid="model-store-task-cancel"]`);
    await page.waitForSelector(`${taskSelector}[data-task-status="cancelling"]`);
    assert.deepEqual(await actions(), { close: false, cancel: true }, `${action}: cancelling tasks cannot be dismissed`);
    await page.waitForFunction((id) => window.versionCancelRequests.includes(id), {}, runId);
    const before = await page.evaluate(() => ({ loads: window.versionLibraryLoads,
      toast: document.querySelector('[data-testid="model-store-toast"]')?.textContent ?? null }));
    // A late cancel can coexist with ok=true; the real task driver must still
    // treat it as cancelled and never publish a success disclosure.
    await page.evaluate((id) => window.finishVersionRun(id, {
      freed_bytes: 128, remaining_paths: ['/cancelled/must-not-report-success'],
    }, true), runId);
    await page.waitForSelector(`${taskSelector}[data-task-status="cancelled"]`);
    await page.waitForFunction((loads) => window.versionLibraryLoads > loads, {}, before.loads);
    assert.deepEqual(await actions(), { close: true, cancel: null });
    assert.equal(await page.$('[data-testid="model-version-operation-result"]'), null);
    const toast = await page.$eval('[data-testid="model-store-view"]', () =>
      document.querySelector('[data-testid="model-store-toast"]')?.textContent ?? null);
    assert.ok(toast === null || toast === before.toast, `${action}: cancellation must not emit a success toast`);
    await page.$eval(taskSelector, (element) => [...element.querySelectorAll('button')]
      .find((button) => button.textContent === window.versionTaskCopy.taskClose).click());
    await page.waitForFunction((selector) => !document.querySelector(selector), {}, taskSelector);
    assert.equal(await page.$('[data-testid="model-version-operation-result"]'), null);
  }
  for (const action of ['stage', 'delete', 'switch']) {
    const runId = await beginVersionAction(action);
    const loads = await page.evaluate(() => window.versionLibraryLoads);
    await page.evaluate(({ id, kind }) => window.finishVersionRun(id, {
      freed_bytes: 3 * 1024 ** 3, remaining_paths: [`/${kind}/retained.weights`],
    }), { id: runId, kind: action });
    const resultSelector = `[data-testid="model-version-operation-result"][data-operation-kind="version_${action}"]`;
    await page.waitForSelector(resultSelector);
    await page.waitForFunction((before) => window.versionLibraryLoads > before, {}, loads);
    assert.deepEqual(await page.$$eval(`${resultSelector} li`, (items) => items.map((item) => item.textContent)),
      [`/${action}/retained.weights`], `${action}: completion must retain its actual result`);
    assert.equal(await page.$eval(`${resultSelector} [role="status"]`, (element) => element.textContent),
      await page.evaluate((kind) => kind === 'delete' ? window.versionTaskCopy.versionDeleted(3)
        : (kind === 'stage' ? window.versionTaskCopy.versionStaged : window.versionTaskCopy.versionSwitched), action));
    assert.equal(await page.$(`[data-task-kind="version_${action}"]`), null);
    await page.click(`${resultSelector} button`);
    await page.waitForFunction((selector) => !document.querySelector(selector), {}, resultSelector);
  }
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
  assert.equal(staleDetailLink.model_store_link_only, true);
  assert.equal(await page.evaluate(() => window.detailActiveCalls), 0);
  // Restoring automatic context is a narrow backend patch. It must not re-import
  // the model, replace other options, or change global/session selection.
  await page.click('[data-testid="model-store-follow-context"]');
  await page.waitForFunction(() => window.detailToast?.includes('无法恢复跟随部署'));
  assert.equal(await page.$eval('[data-testid="model-store-follow-context"]', (button) => button.disabled), false);
  await page.evaluate(() => {
    window.contextError = 'model store endpoint changed or is unavailable; refresh the card and retry';
    window.detailToast = null;
  });
  await page.click('[data-testid="model-store-follow-context"]');
  await page.waitForFunction(() => window.detailToast === window.staleLinkCopy);
  for (const unavailable of ['Command model_store_follow_deployment_context not found', 'Tauri invoke is unavailable']) {
    await page.evaluate((message) => { window.contextError = message; window.detailToast = null; }, unavailable);
    await page.click('[data-testid="model-store-follow-context"]');
    await page.waitForFunction(() => window.detailToast?.includes('当前宿主无法执行此操作'));
  }
  await page.evaluate(() => { window.contextError = 'private backend path not found: credential'; window.detailToast = null; });
  await page.click('[data-testid="model-store-follow-context"]');
  await page.waitForFunction(() => window.detailToast?.includes('无法恢复跟随部署'));
  assert.doesNotMatch(await page.evaluate(() => window.detailToast), /private|credential|当前宿主/);
  await page.evaluate(() => { window.failContext = false; });
  await page.click('[data-testid="model-store-follow-context"]');
  await page.waitForFunction(() => window.detailToast?.includes('已恢复跟随部署'));
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-store-follow-context"]'));
  assert.deepEqual(await page.evaluate(() => window.contextRequests), [
    { cardId: 'isolated-card', modelId: 'existing-store-link' },
    { cardId: 'isolated-card', modelId: 'existing-store-link' },
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
    /* eslint-disable import-x/no-unresolved, n/no-missing-import */
    const reactModule = await import('/@id/react');
    const { createElement } = reactModule.default || reactModule;
    const client = await import('/@id/react-dom/client');
    const { ModelDetailDialog } = await import('/features/model-store/ModelDetailDialog.jsx');
    const { dictZh } = await import('/shared/i18n/zh.js');
    /* eslint-enable import-x/no-unresolved, n/no-missing-import */
    const createRoot = client.createRoot || client.default?.createRoot;
    const root = createRoot(document.getElementById('root'));
    window.detailLoads = [];
    window.detailMutations = [];
    window.detailToasts = [];
    window.detailActivations = [];
    const deferMutation = (kind, cardId) => new Promise((resolve, reject) => {
      window.detailMutations.push({ kind, cardId, resolve, reject });
    });
    window.finishDetailMutation = (cardId, kind, fail) => {
      const index = window.detailMutations.findIndex((operation) => operation.cardId === cardId && operation.kind === kind);
      const [operation] = window.detailMutations.splice(index, 1);
      if (fail) operation.reject(Error('mutation failed'));
      else operation.resolve({ ok: true });
    };
    window.__TAURI__ = { core: { invoke: async (command, payload) => {
      if (command === 'model_store_model_detail') {
        window.detailLoads.push(payload.cardId);
        return { record: { card_id: payload.cardId, card_name: payload.cardId, model_name: payload.cardId,
          endpoint_model: payload.cardId, endpoint_url: 'http://127.0.0.1:8911/v1', running: true, status: 'deployed' } };
      }
      if (command === 'model_store_api_keys') return [];
      if (command === 'model_store_follow_deployment_context') return deferMutation('context', payload.cardId);
      if (command === 'model_store_create_api_key') return deferMutation('key', payload.cardId);
      throw Error(`Unexpected command: ${command}`);
    } } };
    window.TauriBridge = { models: {
      saveModel: (model) => deferMutation('link', model.model_store_card_id),
      setActiveModel: async (id) => { window.detailActivations.push(id); },
    } };
    window.renderScopedDetail = (cardId) => root.render(createElement(ModelDetailDialog, {
      cardId, copy: dictZh.uiModelStore, theme: 'light', onClose() {},
      onToast: (message) => window.detailToasts.push(message),
      savedModels: ['alpha', 'beta'].map((id) => ({ id: `${id}-linked`, model_store_card_id: id,
        model: id, base_url: 'http://127.0.0.1:8911/v1', context_window_tokens: 32768 })),
    }));
  });
  const detailActions = [
    ['context', 'model-store-follow-context'], ['key', 'model-store-add-key'], ['link', 'model-store-use-in-pinvou'],
  ];
  for (const [kind, testId] of detailActions) {
    for (const fail of [false, true]) {
      await page.evaluate(() => window.renderScopedDetail('alpha'));
      await page.waitForFunction(() => document.querySelector('#model-store-detail-title')?.textContent === 'alpha');
      await page.click(`[data-testid="${testId}"]`);
      await page.waitForFunction(() => window.detailMutations.some((operation) => operation.cardId === 'alpha'));
      await page.evaluate(() => window.renderScopedDetail(null));
      await page.waitForFunction(() => !document.querySelector('[data-testid="model-store-detail-dialog"]'));
      await page.evaluate(() => window.renderScopedDetail('beta'));
      await page.waitForFunction(() => document.querySelector('#model-store-detail-title')?.textContent === 'beta');
      await page.click('[data-testid="model-store-add-key"]');
      await page.waitForFunction(() => window.detailMutations.some((operation) => operation.cardId === 'beta'));
      const before = await page.evaluate(() => ({ loads: [...window.detailLoads], toasts: window.detailToasts.length }));
      await page.evaluate(({ action, shouldFail }) => window.finishDetailMutation('alpha', action, shouldFail), { action: kind, shouldFail: fail });
      await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      assert.equal(await page.$eval('#model-store-detail-title', (node) => node.textContent), 'beta');
      assert.deepEqual(await page.evaluate(() => window.detailLoads), before.loads, `${kind} must not start a stale alpha refresh`);
      assert.equal(await page.evaluate(() => window.detailToasts.length), before.toasts, 'old result must not toast over the new card');
      assert.equal(await page.$eval('[data-testid="model-store-add-key"]', (button) => button.disabled), true,
        'old finally must not clear the new card’s in-flight mutation');
      await page.evaluate(() => window.finishDetailMutation('beta', 'key', false));
      await page.waitForFunction(() => !document.querySelector('[data-testid="model-store-add-key"]').disabled);
      await page.evaluate(() => window.renderScopedDetail(null));
      await page.waitForFunction(() => !document.querySelector('[data-testid="model-store-detail-dialog"]'));
    }
  }
  assert.deepEqual(await page.evaluate(() => window.detailActivations), ['alpha-linked'],
    'a committed explicit link retains its requested global activation without repainting another card');
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
  await page.click('[data-testid="model-form-save"]');
  await page.waitForFunction(() => window.savedFormPayloads.length === 1);
  const aliasOnly = await page.evaluate(() => window.savedFormPayloads[0]);
  assert.equal(aliasOnly.context_window_tokens, 32000);
  assert.equal(aliasOnly.preserve_context_window_tokens, true);
  assert.notEqual(aliasOnly.context_window_edit_requested, true,
    'an untouched context snapshot must not become an explicit choice after an alias save');
  // DOM selection works across Chrome platforms while typing still fires real input events.
  await page.$eval('[data-testid="model-form-context-window"]', (input) => { input.focus(); input.select(); });
  await page.type('[data-testid="model-form-context-window"]', '64000');
  await page.click('[data-testid="model-form-save"]');
  await page.waitForFunction(() => window.savedFormPayloads.length === 2);
  const unchangedRoute = await page.evaluate(() => window.savedFormPayloads[1]);
  assert.equal(unchangedRoute.preserve_model_store_route, true);
  assert.equal(unchangedRoute.model, 'old-wire-model');
  assert.equal(unchangedRoute.base_url, 'http://127.0.0.1:8911/v1');
  assert.equal(unchangedRoute.alias, 'My local alias');
  assert.equal(unchangedRoute.context_window_tokens, 64000);
  assert.equal(unchangedRoute.context_window_edit_requested, true);
  assert.equal(unchangedRoute.preserve_context_window_tokens, false);
  const routeInput = await page.$('input[value="http://127.0.0.1:8911/v1"]');
  assert.ok(routeInput, 'the existing form still displays its original route');
  await routeInput.evaluate((input) => { input.focus(); input.select(); });
  await routeInput.type('http://127.0.0.1:8999/v1');
  await page.click('[data-testid="model-form-save"]');
  await page.waitForFunction(() => window.savedFormPayloads.length === 3);
  const editedRoute = await page.evaluate(() => window.savedFormPayloads[2]);
  assert.equal(editedRoute.preserve_model_store_route, false);
  assert.equal(editedRoute.base_url, 'http://127.0.0.1:8999/v1');
  // New drafts must never opt into retaining some existing persisted route,
  // even when the draft has an id and its route inputs remain unchanged.
  await page.evaluate(() => window.renderModelForm({
    ...window.legacyModel, __new: true, preset: 'local_vllm',
  }, 'new'));
  await page.waitForFunction(() => !document.querySelector('[data-testid="model-form-alias"]'));
  await page.click('[data-testid="model-form-save"]');
  await page.waitForFunction(() => window.savedFormPayloads.length === 4);
  const newDraft = await page.evaluate(() => window.savedFormPayloads[3]);
  assert.equal(newDraft.preserve_model_store_route, false);
  assert.notEqual(newDraft.context_window_edit_requested, true);
  // A first 256K store probe can be in flight while a user confirms the same
  // legacy 32K number already shown in the form. The input event, rather than a
  // value comparison, must distinguish that choice from an untouched snapshot.
  await page.evaluate(() => window.renderModelForm({
    ...window.legacyModel, context_window_tokens: 32768,
  }, 'explicit-context'));
  await page.waitForSelector('[data-testid="model-form-context-window"]');
  await page.click('[data-testid="model-form-save"]');
  await page.waitForFunction(() => window.savedFormPayloads.length === 5);
  const untouched32k = await page.evaluate(() => window.savedFormPayloads[4]);
  assert.equal(untouched32k.context_window_tokens, 32768);
  assert.equal(untouched32k.preserve_context_window_tokens, true);
  assert.notEqual(untouched32k.context_window_edit_requested, true);
  for (const [value, count] of [['32768', 6], ['', 7]]) {
    await page.$eval('[data-testid="model-form-context-window"]', (input) => { input.focus(); input.select(); });
    if (value) await page.type('[data-testid="model-form-context-window"]', value);
    else await page.keyboard.press('Backspace');
    await page.click('[data-testid="model-form-save"]');
    await page.waitForFunction((expected) => window.savedFormPayloads.length === expected, {}, count);
    const explicitChoice = await page.evaluate(() => window.savedFormPayloads.at(-1));
    assert.equal(explicitChoice.context_window_tokens, value ? 32768 : null);
    assert.equal(explicitChoice.context_window_edit_requested, true);
    assert.equal(explicitChoice.preserve_context_window_tokens, false);
  }
  // Exercise the actual hook and lazy module in Chrome, including resource
  // loading boundaries that pure scheduler/controller tests cannot observe.
  async function waitForStartupProbe(predicate) {
    const deadline = Date.now() + 5000;
    while (!predicate() && Date.now() < deadline) await new Promise((resolve) => { setTimeout(resolve, 10); });
    assert.ok(predicate(), 'startup probe event must arrive before the timeout');
  }
  async function startupProbe({ available = true, holdRuntime = false, failRuntime = false } = {}) {
    const probe = await browser.newPage();
    await probe.setCacheEnabled(false);
    const pending = [];
    const warnings = [];
    let runtimeRequests = 0;
    probe.on('pageerror', (error) => errors.push(error.message));
    probe.on('console', (message) => { if (message.type() === 'warn') warnings.push(message.text()); });
    await probe.setRequestInterception(true);
    probe.on('request', (request) => {
      if (new URL(request.url()).pathname.endsWith('/startup-update-runtime.js')) {
        runtimeRequests += 1;
        if (failRuntime) { void request.abort('failed'); return; }
        if (holdRuntime) { pending.push(request); return; }
      }
      void request.continue();
    });
    await probe.goto(`http://127.0.0.1:${vite.httpServer.address().port}/versions.html`);
    await probe.evaluate(async (initialAvailable) => {
      /* eslint-disable import-x/no-unresolved, n/no-missing-import */
      const client = await import('/@id/react-dom/client');
      const createRoot = client.createRoot || client.default?.createRoot;
      const { useModelStoreStartupUpdates, __TestReact: React } = await import('/features/model-store/useModelStoreStartupUpdates.js');
      /* eslint-enable import-x/no-unresolved, n/no-missing-import */
      localStorage.clear();
      localStorage.setItem('pinvou.model-store.viewed-updates.v1', JSON.stringify([JSON.stringify(['a', 'v1'])]));
      window.startupCalls = 0;
      window.startupReplies = [];
      window.startupDelays = [];
      window.startupPolicyEnabled = true;
      window.startupPolicyReads = 0;
      window.holdStartupTimer = false;
      window.queuedStartupCallbacks = [];
      const nativeTimeout = window.setTimeout.bind(window);
      window.setTimeout = (callback, delay, ...args) => {
        window.startupDelays.push(delay);
        if (window.holdStartupTimer && delay >= 0 && delay <= 15000) {
          window.queuedStartupCallbacks.push(callback);
          return 100000 + window.queuedStartupCallbacks.length;
        }
        // Keep tests fast; exact 15s and deadline arithmetic have Node coverage.
        return nativeTimeout(callback, delay > 10000 && delay <= 15000 ? 20 : delay, ...args);
      };
      window.__TAURI__ = { core: { invoke: (command) => {
        if (command === 'model_store_storage_settings') {
          window.startupPolicyReads += 1;
          return Promise.resolve({ startup_update_check_enabled: window.startupPolicyEnabled });
        }
        if (command !== 'model_store_startup_update_check') throw Error(`Unexpected command: ${command}`);
        window.startupCalls += 1;
        return new Promise((resolve) => { window.startupReplies.push(resolve); });
      } } };
      function Probe() {
        const [available, setAvailable] = React.useState(initialAvailable);
        const [viewing, setViewing] = React.useState(false);
        const updates = useModelStoreStartupUpdates(available, viewing);
        React.useEffect(() => {
          window.startupState = { updates: updates.updates, hasUnread: updates.hasUnread, available, viewing };
          window.startupControl = { setAvailable, setViewing, reconcile: updates.reconcile };
        });
        return null;
      }
      const root = createRoot(document.getElementById('root'));
      window.unmountStartupProbe = () => root.unmount();
      root.render(React.createElement(React.StrictMode, null, React.createElement(Probe)));
    }, available);
    await probe.waitForFunction(() => !!window.startupControl).catch((error) => { throw new Error(`Startup probe did not mount: ${errors.join('; ')}`, { cause: error }); });
    return { page: probe, pending, warnings, runtimeRequests: () => runtimeRequests };
  }
  const report = { updates: [
    { card_id: 'a', local_revision: 'installed', revision: 'v1' },
    { card_id: 'b', local_revision: 'installed', revision: 'v2' },
  ] };
  const unavailable = await startupProbe({ available: false });
  await unavailable.page.evaluate(() => new Promise((resolve) => { setTimeout(resolve, 80); }));
  assert.equal(unavailable.runtimeRequests(), 0);
  assert.equal(await unavailable.page.evaluate(() => window.startupCalls), 0);
  await unavailable.page.close();

  const normal = await startupProbe();
  await normal.page.waitForFunction(() => window.startupCalls === 1);
  await normal.page.evaluate((value) => window.startupReplies[0](value), report);
  await normal.page.waitForFunction(() => window.startupState.hasUnread && window.startupState.updates.length === 2);
  // Controller state remains live while availability hides the feature.
  await normal.page.evaluate(() => window.startupControl.setAvailable(false));
  await normal.page.waitForFunction(() => !window.startupState.available);
  await normal.page.evaluate(() => window.startupControl.setViewing(true));
  await normal.page.waitForFunction(() => window.startupState.viewing);
  await normal.page.evaluate(() => window.startupControl.setViewing(false));
  await normal.page.waitForFunction(() => !window.startupState.viewing);
  await normal.page.evaluate(() => window.startupControl.setAvailable(true));
  await normal.page.waitForFunction(() => window.startupState.available && window.startupCalls === 2);
  assert.equal(await normal.page.evaluate(() => window.startupState.hasUnread), false);
  const saved = await normal.page.evaluate(() => JSON.parse(localStorage.getItem('pinvou.model-store.viewed-updates.v1')));
  assert.deepEqual(new Set(saved), new Set([JSON.stringify(['a', 'v1']), JSON.stringify(['b', 'v2'])]));
  await normal.page.evaluate(() => window.startupControl.setAvailable(false));
  await normal.page.waitForFunction(() => !window.startupState.available);
  await normal.page.evaluate(() => window.startupControl.reconcile([]));
  await normal.page.evaluate(() => window.startupControl.setAvailable(true));
  await normal.page.waitForFunction(() => window.startupState.available && window.startupCalls === 3);
  assert.deepEqual(await normal.page.evaluate(() => window.startupState.updates), []);
  // A result from an earlier availability generation cannot restore updates.
  await normal.page.evaluate((value) => window.startupReplies[1](value), report);
  await normal.page.evaluate(() => new Promise((resolve) => { setTimeout(resolve, 50); }));
  assert.deepEqual(await normal.page.evaluate(() => window.startupState.updates), []);
  await normal.page.close();

  for (const queued of [false, true]) {
    const disabled = await startupProbe({ holdRuntime: true });
    await waitForStartupProbe(() => disabled.pending.length > 0);
    await disabled.page.evaluate((queued) => {
      window.startupPolicyEnabled = queued;
      window.holdStartupTimer = queued;
    }, queued);
    for (const request of disabled.pending.splice(0)) await request.continue();
    await disabled.page.waitForFunction(() => window.startupPolicyReads === 1);
    if (queued) {
      await disabled.page.waitForFunction(() => window.queuedStartupCallbacks.length === 1);
      await disabled.page.evaluate(() => {
        window.startupPolicyEnabled = false;
        window.holdStartupTimer = false;
        window.queuedStartupCallbacks.shift()();
      });
      await disabled.page.waitForFunction(() => window.startupPolicyReads === 2);
    }
    await disabled.page.evaluate(() => new Promise((resolve) => { setTimeout(resolve, 80); }));
    assert.equal(await disabled.page.evaluate(() => window.startupCalls), 0,
      queued ? 'queued startup timer must reread a newly disabled preference' : 'delayed module loading must obey an already disabled preference');
    await disabled.page.close();
  }

  const slow = await startupProbe({ holdRuntime: true });
  await waitForStartupProbe(() => slow.pending.length > 0);
  assert.equal(await slow.page.evaluate(() => window.startupCalls), 0);
  // Simulate a local chunk arriving after the original lower-bound deadline.
  await slow.page.evaluate(() => {
    const now = performance.now.bind(performance);
    performance.now = () => now() + 20000;
  });
  for (const request of slow.pending.splice(0)) await request.continue();
  await slow.page.waitForFunction(() => window.startupCalls === 1);
  assert.ok(await slow.page.evaluate(() => window.startupDelays.includes(0)));
  await slow.page.evaluate(() => window.startupControl.setViewing(true));
  await slow.page.waitForFunction(() => window.startupState.viewing);
  await slow.page.evaluate((value) => window.startupReplies[0](value), report);
  await slow.page.waitForFunction(() => window.startupState.updates.length === 2);
  assert.equal(await slow.page.evaluate(() => window.startupState.hasUnread), false);
  await slow.page.close();

  for (const unmount of [false, true]) {
    const cancelled = await startupProbe({ holdRuntime: true });
    await waitForStartupProbe(() => cancelled.pending.length > 0);
    await cancelled.page.evaluate((remove) => {
      if (remove) window.unmountStartupProbe();
      else window.startupControl.setAvailable(false);
    }, unmount);
    if (!unmount) await cancelled.page.waitForFunction(() => !window.startupState.available);
    for (const request of cancelled.pending.splice(0)) await request.continue();
    await cancelled.page.evaluate(() => new Promise((resolve) => { setTimeout(resolve, 80); }));
    assert.equal(await cancelled.page.evaluate(() => window.startupCalls), 0);
    await cancelled.page.close();
  }
  const broken = await startupProbe({ failRuntime: true });
  await waitForStartupProbe(() => broken.warnings.length > 0);
  await broken.page.evaluate(() => new Promise((resolve) => { setTimeout(resolve, 80); }));
  assert.equal(await broken.page.evaluate(() => window.startupCalls), 0);
  assert.equal(broken.runtimeRequests(), 1);
  assert.deepEqual(broken.warnings, ['model_store_startup_update_check failed']);
  await broken.page.close();
  console.log('PASS: startup hook lazy loading preserves unavailable-state acknowledgement, library reconciliation, cancellation, deadlines and private failure handling in real Chrome');
  assert.deepEqual(errors, []);
  console.log('PASS: versions refresh after success/failure/cancellation and ignore stale responses');
  console.log('PASS: MTP deployment and version dialogs disclose network defaults and retained .env in all three languages without changing the 256K default');
  console.log('PASS: existing settings forms preserve untouched routes after migration; edited routes and new drafts opt out');
  console.log('PASS: alias-only and untouched drafts do not claim context edits; explicit 32K and automatic choices retain their intent');
  console.log('PASS: unknown compatibility reasons remain visible; uninstall cleanup preserves sync failures');
  console.log('PASS: zero-size legacy metadata retains update actions with translated unverified-weight notices; bounded results keep every completion and scroll to individual residual acknowledgements');
  console.log('PASS: startup update preference loads persisted false and preserves the draft and retry controls after a failed save');
  console.log('PASS: stale details retain explicit re-link intent and surface rejection without changing the active model');
  console.log('PASS: staging preserves other cards’ dialogs; concurrent cleanup disclosures survive navigation and individual acknowledgement');
  console.log('PASS: force-remove results survive navigation; closing one result preserves other unread cleanup paths');
  console.log('PASS: late context/key/link results cannot refresh another card, overwrite its busy state, or show stale toasts');
  console.log('PASS: version stage/delete/switch expose Close only after cancellation; cancellation never reports success and normal completion retains results and refreshes the library');
} catch (error) {
  hasPrimaryFailure = true;
  primaryFailure = error;
}
try {
  // Close every registered resource before retiring any owned directory.
  await fixtureScope.close();
} catch (cleanupError) {
  if (hasPrimaryFailure) {
    throw new AggregateError(
      [primaryFailure, cleanupError],
      'Model-store smoke failed and fixture retirement was not confirmed',
      { cause: cleanupError },
    );
  }
  throw cleanupError;
}
if (hasPrimaryFailure) throw primaryFailure;
