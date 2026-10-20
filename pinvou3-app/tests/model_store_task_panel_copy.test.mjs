import assert from 'node:assert/strict';
import test from 'node:test';
import React, { act } from 'react';
import { createRoot } from 'react-dom/client';
import { createWindow } from '@mixmark-io/domino';
import { loadJsx } from './fixtures/jsx-module-hooks.mjs';

const { ModelStoreTaskPanel } = await loadJsx('../src/features/model-store/ModelStoreTaskPanel.jsx', import.meta.url);
const { dict } = await import('./helpers/i18n-all.js');
const RAW_FIXTURE = 'PRIVATE_TEST_BUFFER_MUST_NOT_REACH_CLIPBOARD';
const SNAPSHOT = 'ERROR: [HOST] request failed [REDACTED]\n';

async function fixture(t, { language = 'zh', runId = 'run-copy', response = { text: SNAPSHOT }, reject = false, clipboardReject = false } = {}) {
  const saved = new Map(['window', 'document', 'navigator', '__TAURI__', 'IS_REACT_ACT_ENVIRONMENT']
    .map((key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)]));
  const window = createWindow('<!doctype html><html><body><div id="root"></div></body></html>');
  const calls = [];
  const clipboard = [];
  globalThis.window = window;
  globalThis.document = window.document;
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  Object.defineProperty(globalThis, 'navigator', { configurable: true, value: {
    clipboard: { writeText: async (value) => {
      if (clipboardReject) throw new Error('Clipboard denied');
      clipboard.push(value);
    } },
  } });
  globalThis.__TAURI__ = { core: { invoke: async (name, args) => {
    calls.push([name, args]);
    assert.equal(name, 'model_store_export_run_log');
    if (reject) throw new Error('No safe export available');
    return response;
  } } };
  const container = window.document.getElementById('root');
  const root = createRoot(container);
  const copy = dict[language].uiModelStore;
  const task = {
    id: 1, runId, key: 'card:copy', kind: 'enable', title: 'Test model',
    status: 'error', expanded: true, lines: [RAW_FIXTURE], error: RAW_FIXTURE,
    progress: null, prompt: null, samples: [],
  };
  const noop = () => {};
  await act(async () => root.render(React.createElement(ModelStoreTaskPanel, {
    tasks: [task], copy, onCancel: noop, onDismiss: noop, onToggle: noop,
    onAnswer: noop, onForceRemove: noop, onStopRetained: noop,
  })));
  t.after(async () => {
    await act(async () => root.unmount());
    for (const [key, descriptor] of saved) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor);
      else delete globalThis[key];
    }
  });
  return { container, calls, clipboard, copy, async click(selector) {
    const button = container.querySelector(`[data-testid="${selector}"]`);
    assert.ok(button, `Missing ${selector}`);
    await act(async () => button.click());
  } };
}

for (const language of ['zh', 'en', 'ja']) {
  test(`${language}: copy uses only the backend-sanitized export snapshot`, async (t) => {
    const ui = await fixture(t, { language });
    assert.ok(ui.container.textContent.includes(RAW_FIXTURE), 'Local diagnostics remain visible');
    await ui.click('model-store-task-tools');
    await ui.click('model-store-task-copy-log');
    assert.deepEqual(ui.calls, [['model_store_export_run_log', { runId: 'run-copy' }]]);
    assert.deepEqual(ui.clipboard, [SNAPSHOT]);
    await ui.click('model-store-task-tools');
    assert.equal(ui.container.querySelector('[data-testid="model-store-task-copy-log"]').textContent, ui.copy.copied);
  });
}

for (const scenario of [
  { label: 'export rejection', reject: true },
  { label: 'old backend without sanitized text', response: { path: '/test/export.log' } },
  { label: 'empty snapshot', response: { text: '' } },
  { label: 'missing run', runId: null },
  { label: 'clipboard unavailable', clipboardReject: true },
]) {
  test(`copy fails closed without raw fallback: ${scenario.label}`, async (t) => {
    const ui = await fixture(t, scenario);
    await ui.click('model-store-task-tools');
    await ui.click('model-store-task-copy-log');
    assert.deepEqual(ui.clipboard, []);
    if (scenario.runId === null) assert.deepEqual(ui.calls, []);
    await ui.click('model-store-task-tools');
    assert.equal(ui.container.querySelector('[data-testid="model-store-task-copy-log"]').textContent, ui.copy.copyFailed);
  });
}

test('duplicate copy clicks share one pending sanitized export', async (t) => {
  const pending = Promise.withResolvers();
  const ui = await fixture(t, { response: pending.promise });
  await ui.click('model-store-task-tools');
  const button = ui.container.querySelector('[data-testid="model-store-task-copy-log"]');
  await act(async () => { button.click(); button.click(); });
  assert.equal(ui.calls.length, 1);
  assert.deepEqual(ui.clipboard, []);
  await ui.click('model-store-task-tools');
  assert.equal(ui.container.querySelector('[data-testid="model-store-task-copy-log"]').disabled, true);
  await act(async () => pending.resolve({ text: SNAPSHOT }));
  assert.deepEqual(ui.clipboard, [SNAPSHOT]);
});
