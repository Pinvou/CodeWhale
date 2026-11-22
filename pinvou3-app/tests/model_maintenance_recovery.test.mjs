import assert from 'node:assert/strict';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { readFile } from 'node:fs/promises';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from './helpers/vite-fixture.mjs';
import react from '@vitejs/plugin-react';

const previousWindow = globalThis.window;
globalThis.window ||= { TauriBridge: undefined };
globalThis.__maintenanceTestCalls = [];
const vite = await createServer({
  configFile: false,
  root: fileURLToPath(new URL('..', import.meta.url)),
  logLevel: 'error',
  server: { middlewareMode: true, watch: null, hmr: false },
  optimizeDeps: { noDiscovery: true },
  plugins: [react(), {
    name: 'maintenance-api-fixture',
    enforce: 'pre',
    resolveId(source, importer) {
      if (importer?.endsWith('/model-store-api.js') && source.endsWith('/platform/tauri/client.js')) return '\0maintenance-api-client';
    },
    load(id) {
      if (id === '\0maintenance-api-client') return 'export function invokeTauri(command,args){globalThis.__maintenanceTestCalls.push({command,args});return Promise.resolve({run_id:"fixture"});}';
    },
  }],
});
const { MaintenanceRecoveryPanel } = await vite.ssrLoadModule('/src/features/model-store/MaintenanceRecoveryPanel.jsx');
const { ModelRow } = await vite.ssrLoadModule('/src/features/model-store/ModelRow.jsx');
const { IncompleteInstallPanel } = await vite.ssrLoadModule('/src/features/model-store/IncompleteInstallPanel.jsx');
const api = await vite.ssrLoadModule('/src/features/model-store/model-store-api.js');
const dictionaries = await Promise.all(['zh', 'en', 'ja'].map((lang) => vite.ssrLoadModule(`/src/shared/i18n/${lang}.js`)));
const copies = [dictionaries[0].dictZh.uiModelStore, dictionaries[1].dictEn.uiModelStore, dictionaries[2].dictJa.uiModelStore];

after(async () => {
  await vite.close();
  if (previousWindow === undefined) delete globalThis.window;
  else globalThis.window = previousWindow;
  delete globalThis.__maintenanceTestCalls;
});

test('recovery remains available when a purged model no longer has a library row', () => {
  for (const copy of copies) {
    const html = renderToStaticMarkup(React.createElement(MaintenanceRecoveryPanel, {
      transactions: [{ cardId: 'removed-card', cardName: 'Removed fixture', phase: 'recovery_required', workerActive: false, owner: 'must-not-be-rendered' }],
      copy, onRecover() {}, onRefresh() {},
    }));
    assert.ok(html.includes('Removed fixture'));
    assert.ok(html.includes(copy.maintenanceRecover));
    assert.ok(!html.includes('must-not-be-rendered'));
    assert.ok(!html.includes('disabled=""'));
  }
});

test('active workers and corrupt records cannot use the release button', () => {
  for (const transaction of [{ workerActive: true, phase: 'active' }, { workerActive: false, phase: 'record_error' }]) {
    const html = renderToStaticMarkup(React.createElement(MaintenanceRecoveryPanel, {
      transactions: [{ cardId: 'model-a', cardName: 'Fixture', ...transaction }],
      copy: copies[0], onRecover() {}, onRefresh() {},
    }));
    assert.ok(html.includes('disabled=""'));
  }
});

test('a recovery started by this view disables repeats before the library refreshes', () => {
  const html = renderToStaticMarkup(React.createElement(MaintenanceRecoveryPanel, {
    transactions: [{ cardId: 'model-a', phase: 'recovery_required', workerActive: false }],
    copy: copies[0], onRecover() {}, onRefresh() {}, isRecovering: (cardId) => cardId === 'model-a',
  }));
  assert.ok(html.includes('disabled=""'));
});

test('recovery uses the existing background lifecycle operation without an owner input', async () => {
  const result = await api.startMaintenanceRecovery('model-a', 'forged-owner');
  assert.equal(result.run_id, 'fixture');
  assert.deepEqual(globalThis.__maintenanceTestCalls.at(-1), {
    command: 'model_store_start_lifecycle', args: { cardId: 'model-a', action: 'recover_maintenance' },
  });
});

test('an empty healthy maintenance index renders no interruption banner', () => {
  assert.equal(renderToStaticMarkup(React.createElement(MaintenanceRecoveryPanel, {
    copy: copies[0], onRecover() {}, onRefresh() {},
  })), '');
});

test('failed installations remain visible without a library row and grant no ownership', () => {
  for (const copy of copies) {
    const html = renderToStaticMarkup(React.createElement(IncompleteInstallPanel, {
      installations: [{ cardId: 'incomplete-card', runId: 'failed-run', terminal: true, canPreview: true, error: 'installation_preview_changed', owner: 'private-owner' }],
      copy, onRetry() {}, onRefresh() {},
    }));
    assert.ok(html.includes('incomplete-card'));
    assert.ok(html.includes(copy.incompletePreview));
    assert.ok(html.includes(copy.incompleteErrors.installation_preview_changed));
    assert.ok(!html.includes('private-owner'));
    assert.ok(!html.includes('disabled=""'));
  }
});

test('unfinished tasks and older downloads without source context cannot retry directly', () => {
  for (const copy of copies) {
    for (const terminal of [false, true]) {
      const html = renderToStaticMarkup(React.createElement(IncompleteInstallPanel, {
        installations: [{ cardId: 'incomplete-card', runId: 'failed-run', terminal, canPreview: false }],
        copy, onRetry() {}, onRefresh() {},
      }));
      assert.ok(html.includes('disabled=""'));
      assert.ok(html.includes(terminal ? copy.incompleteUnknownSource : copy.incompletePending));
    }
  }
});

test('incomplete-install retry passes only the original run reference and reviewed digest', async () => {
  const digest = 'a'.repeat(64);
  await api.previewIncomplete('failed-run');
  await api.retryIncomplete('failed-run', digest);
  assert.deepEqual(globalThis.__maintenanceTestCalls.slice(-2), [
    { command: 'model_store_preview_incomplete', args: { runId: 'failed-run' } },
    { command: 'model_store_retry_incomplete', args: { runId: 'failed-run', expectedSha256: digest } },
  ]);
});

test('interactive recovery copy exists in all languages, including the preview cancel action', async () => {
  const source = await readFile(new URL('../src/features/model-store/IncompleteInstallPanel.jsx', import.meta.url), 'utf8');
  const keys = [...source.matchAll(/copy\.([A-Za-z][A-Za-z0-9_]*)/g)].map(match => match[1]);
  for (const copy of copies) {
    for (const key of keys) assert.notEqual(copy[key], undefined, `missing recovery copy: ${key}`);
  }
});

test('reservation summaries explain their limited authority in all three languages', () => {
  for (const copy of copies) {
    const html = renderToStaticMarkup(React.createElement(MaintenanceRecoveryPanel, {
      transactions: [{ cardId: 'model-a', reservationOnly: true, workerActive: false }],
      copy, onRecover() {}, onRefresh() {},
    }));
    assert.ok(html.includes(copy.maintenanceReservation));
    assert.ok(html.includes(copy.maintenanceReservationRecover));
  }
});

test('observed model rows cannot stop services and only explicit adoptable rows can enable', () => {
  for (const copy of copies) {
    for (const [state, canAdopt, disabled] of [['running', true, true], ['deployed', false, true], ['deployed', true, false]]) {
      const html = renderToStaticMarkup(React.createElement(ModelRow, {
        model: { id: 'model-a', name: 'Observed fixture', state, record: { card_id: 'model-a', cmsd: { managed: false, canAdopt } } },
        copy, busyKind: null,
      }));
      assert.ok(html.includes(copy.cmsdObserved));
      const tag = html.match(new RegExp(`<button[^>]+data-testid="model-store-${state === 'running' ? 'stop' : 'enable'}"[^>]*>`))?.[0];
      assert.ok(tag);
      assert.equal(tag.includes('disabled=""'), disabled);
    }
  }
});
