import assert from 'node:assert/strict';

/** Exercise the real async uninstall handler, including a props update while it waits. */
export async function exerciseLatestLinkedCleanup(page) {
  for (const scenario of ['migration-with-props', 'migration-without-props', 'force-migration-with-props', 'changed-after-read', 'read-failed', 'force-read-failed', 'invalid-snapshot', 'no-links']) {
    await page.reload();
    await page.evaluate(async (mode) => {
      /* eslint-disable import-x/no-unresolved, n/no-missing-import */
      const reactModule = await import('/@id/react');
      const { createElement } = reactModule.default || reactModule;
      const client = await import('/@id/react-dom/client');
      const { ModelStoreView } = await import('/features/model-store/ModelStoreView.jsx');
      const { dictZh } = await import('/shared/i18n/zh.js');
      /* eslint-enable import-x/no-unresolved, n/no-missing-import */
      const createRoot = client.createRoot || client.default?.createRoot;
      const root = createRoot(document.getElementById('root'));
      const record = { card_id: 'cleanup-card', card_name: 'Cleanup', model_name: 'cleanup-wire',
        status: 'deployed', running: false, size_bytes: 128, deploy_topology: 'single' };
      const legacy = { id: 'legacy-link', name: 'Legacy', model: 'cleanup-wire', base_url: 'http://127.0.0.1:8911/v1' };
      const linked = { ...legacy, model_store_card_id: record.card_id };
      const fallback = { id: 'user-default', name: 'User default', model: 'other', base_url: 'https://example.test/v1' };
      const forceRemove = mode.startsWith('force-');
      window.cleanupFixture = { reads: 0, deletes: [], removed: [], rows: [legacy, fallback], active: fallback.id, finished: false };
      const fixture = window.cleanupFixture;
      window.__TAURI__ = { core: { invoke: async (command, payload) => {
        if (command === 'model_store_availability') return { available: true };
        if (command === 'model_store_cards' || command === 'model_store_active_runs') return [];
        if (command === 'model_store_library') return { models: fixture.finished ? [] : [record], corrected: [] };
        if (command === 'model_store_test_store_state') return { enabled: false };
        if (command === 'model_store_take_startup_cleanup') return null;
        if (command === 'model_store_start_lifecycle' && payload.action === 'uninstall') return { run_id: 'cleanup-uninstall' };
        if (command === 'model_store_run_log' && forceRemove) return { done: true, ok: false, lines: [], next: 0, error: 'uninstall failed' };
        if (command === 'model_store_force_remove') return new Promise((resolve) => { fixture.finishUninstall = () => {
          fixture.finished = true;
          resolve({ residual_paths: ['/cleanup/retained.weights'] });
        }; });
        if (command === 'model_store_run_log' && fixture.finished) return { done: true, ok: true, lines: [], next: 0 };
        if (command === 'model_store_run_log') return new Promise((resolve) => { fixture.finishUninstall = () => {
          fixture.finished = true;
          resolve({ done: true, ok: true, lines: [], next: 0, result: { purged_weights: false } });
        }; });
        if (command === 'list_models') {
          if (payload?.requireReadable !== true) throw Error('cleanup requires a readable settings snapshot');
          fixture.reads += 1;
          if (mode.endsWith('read-failed')) throw Error('fixture read failed');
          if (mode === 'invalid-snapshot') return {};
          const snapshot = { models: structuredClone(fixture.rows), active_model_id: fixture.active };
          // These edits happen after candidate selection. The conditional mutation
          // must retain the latest settings ownership rather than trusting the read.
          if (mode === 'changed-after-read') fixture.rows = fixture.rows
            .filter((row) => row.id !== 'removed-link')
            .map((row) => row.id === 'legacy-link' ? { ...row, model_store_card_id: 'another-card' }
              : row.id === 'detached-link' ? { ...row, model_store_link_detached: true } : row);
          return snapshot;
        }
        throw Error(`Unexpected command: ${command}`);
      } } };
      window.TauriBridge = { models: { deleteModelIfStoreLink: async (id, cardId) => {
        fixture.deletes.push({ id, cardId });
        const current = fixture.rows.find((row) => row.id === id);
        if (!current || current.model_store_card_id !== cardId || current.model_store_link_detached) return false;
        fixture.rows = fixture.rows.filter((row) => row.id !== id);
        fixture.removed.push(id);
        return true;
      } } };
      const render = (savedModels) => root.render(createElement(ModelStoreView, {
        theme: 'light', t: dictZh, bs: { savedModels },
      }));
      render([legacy, fallback]);
      fixture.migrate = () => {
        fixture.rows = mode === 'no-links' ? [fallback] : [linked, fallback];
        if (mode === 'changed-after-read') fixture.rows.push(
          { ...linked, id: 'removed-link' }, { ...linked, id: 'detached-link' }, { ...linked, id: 'valid-link' },
        );
        if (mode.endsWith('with-props') || mode === 'changed-after-read') render(structuredClone(fixture.rows));
      };
      fixture.expectedWarning = forceRemove
        ? dictZh.uiModelStore.linkRowStale(record.card_name, dictZh.uiModelStore.linkSnapshotReadFailed)
        : dictZh.uiModelStore.uninstallLinksLeft(record.card_name);
    }, scenario);
    await page.waitForSelector('[data-testid="model-store-uninstall"]');
    await page.click('[data-testid="model-store-uninstall"]');
    await page.waitForSelector('[data-testid="model-store-uninstall-keep"]');
    await page.click('[data-testid="model-store-uninstall-keep"]');
    if (scenario.startsWith('force-')) {
      await page.waitForSelector('[data-testid="model-store-task-force-remove"]');
      await page.click('[data-testid="model-store-task-force-remove"]');
      await page.waitForSelector('[data-testid="model-store-force-remove-confirm"]');
      await page.click('[data-testid="model-store-force-remove-confirm"]');
    }
    await page.waitForFunction(() => typeof window.cleanupFixture.finishUninstall === 'function');
    await page.evaluate(() => window.cleanupFixture.migrate());
    // Commit the refreshed props before releasing the already-running handler.
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.evaluate(() => window.cleanupFixture.finishUninstall());
    await page.waitForFunction(() => window.cleanupFixture.finished && !document.querySelector('[data-testid="model-store-task-panel"]'));
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    const result = await page.evaluate(() => ({
      reads: window.cleanupFixture.reads, deletes: window.cleanupFixture.deletes,
      removed: window.cleanupFixture.removed, rows: window.cleanupFixture.rows, active: window.cleanupFixture.active,
      toast: document.querySelector('[data-testid="model-store-toast"]')?.textContent,
      expectedWarning: window.cleanupFixture.expectedWarning,
    }));
    assert.equal(result.reads, 1, `${scenario}: cleanup must read authoritative settings after uninstall`);
    assert.equal(result.active, 'user-default', `${scenario}: preserve the unrelated default`);
    if (scenario.includes('migration-')) {
      assert.deepEqual(result.deletes, [{ id: 'legacy-link', cardId: 'cleanup-card' }], scenario);
      assert.deepEqual(result.removed, ['legacy-link'], scenario);
    } else if (scenario === 'changed-after-read') {
      assert.deepEqual(result.deletes.map((call) => call.id), ['legacy-link', 'removed-link', 'detached-link', 'valid-link']);
      assert.deepEqual(result.removed, ['valid-link']);
      assert.equal(result.rows.find((row) => row.id === 'legacy-link').model_store_card_id, 'another-card');
      assert.equal(result.rows.find((row) => row.id === 'detached-link').model_store_link_detached, true);
      assert.ok(result.rows.some((row) => row.id === 'user-default'));
    } else {
      assert.deepEqual(result.deletes, [], scenario);
      if (scenario !== 'no-links') assert.equal(result.toast, result.expectedWarning, `${scenario}: do not report cleanup success`);
    }
  }
  console.log('PASS: uninstall reads latest linked settings after migration and preserves concurrent ownership edits and read failures');
  await page.reload();
}
