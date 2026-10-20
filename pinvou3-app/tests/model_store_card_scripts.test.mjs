import assert from 'node:assert/strict';
import test from 'node:test';
import React, { act } from 'react';
import { createRoot } from 'react-dom/client';
import { createWindow } from '@mixmark-io/domino';
import { loadJsx } from './fixtures/jsx-module-hooks.mjs';
import { startCardScripts } from '../src/features/model-store/model-store-api.js';
import { dismissTask, getTasks } from '../src/features/model-store/useLmmTasks.js';
const { ModelDetailDialog } = await loadJsx('../src/features/model-store/ModelDetailDialog.jsx', import.meta.url);
const { ModelStoreView } = await loadJsx('../src/features/model-store/ModelStoreView.jsx', import.meta.url);
const { dict } = await import('./helpers/i18n-all.js');

test('script changes require explicit boolean confirmation and rollback choice', async () => {
  const previous = globalThis.__TAURI__;
  const calls = [];
  globalThis.__TAURI__ = { core: { invoke: async (name, args) => { calls.push([name, args]); return {}; } } };
  try {
    for (const value of [undefined, null, false, 'true', 'false', {}, true]) {
      await startCardScripts('card', value, value);
      assert.deepEqual(calls.at(-1), ['model_store_start_card_scripts', { cardId: 'card', rollback: value === true, confirmed: value === true }]);
    }
  } finally {
    if (previous === undefined) delete globalThis.__TAURI__;
    else globalThis.__TAURI__ = previous;
  }
});

async function fixture(t, { running = false, status = 'deployed' } = {}) {
  const saved = new Map(['window', 'document', '__TAURI__', 'IS_REACT_ACT_ENVIRONMENT']
    .map((key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)]));
  const window = createWindow('<!doctype html><html><body><div id="root"></div></body></html>');
  const record = { card_id: 'card', card_name: 'Test model', running, status, local_script_hashes: { enable: 'old' } };
  const starts = [];
  const runs = new Map();
  const unexpected = [];
  let confirmed = true;
  const confirmations = [];
  window.confirm = (message) => { confirmations.push(message); return confirmed; };
  globalThis.window = window;
  globalThis.document = window.document;
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  globalThis.__TAURI__ = { core: { invoke: async (command, payload) => {
    switch (command) {
      case 'model_store_availability': return { available: true };
      case 'model_store_cards': return [{ id: 'card', name: record.card_name, script_sha256: { enable: 'new' } }];
      case 'model_store_library': return { models: [record], corrected: [] };
      case 'model_store_model_detail': return { record, runtime_lines: [] };
      case 'model_store_api_keys': return [];
      case 'model_store_active_runs': return { runs: [] };
      case 'model_store_take_startup_cleanup': return null;
      case 'model_store_run_log': return runs.get(payload.runId).promise;
      case 'model_store_start_lifecycle':
      case 'model_store_start_card_scripts': {
        const id = `run-${starts.length}`;
        runs.set(id, Promise.withResolvers());
        starts.push({ command, payload, id });
        return { run_id: id };
      }
      default: unexpected.push(command); throw new Error(`Unexpected IPC: ${command}`);
    }
  } } };
  const container = window.document.getElementById('root');
  const root = createRoot(container);
  const finish = async (index, ok = true) => {
    await act(async () => {
      runs.get(starts[index].id).resolve({ lines: [], next: 0, done: true, ok, cancelled: !ok });
    });
  };
  t.after(async () => {
    await act(async () => {
      for (const run of runs.values()) run.resolve({ lines: [], next: 0, done: true, ok: false, cancelled: true });
    });
    await act(async () => {
      root.unmount();
      for (const task of [...getTasks()]) dismissTask(task.id);
    });
    for (const [key, descriptor] of saved) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor);
      else delete globalThis[key];
    }
    assert.deepEqual(unexpected, []);
  });
  return {
    record, container, starts, confirmations, finish,
    confirm(value) { confirmed = value; },
    async render(element) { await act(async () => root.render(element)); },
    async click(selector) {
      const button = container.querySelector(selector);
      assert.ok(button, `Missing ${selector}`);
      await act(async () => button.click());
    },
  };
}

for (const language of ['zh', 'en', 'ja']) {
  const copy = dict[language].uiModelStore;
  for (const [running, busy] of [[false, false], [true, false], [false, true], [true, true]]) {
    test(`${language}: real detail update running=${running} busy=${busy}`, async (t) => {
      const ui = await fixture(t, { running });
      const actions = [];
      await ui.render(React.createElement(ModelDetailDialog, {
        cardId: 'card', copy, theme: 'light', cardBusy: busy, scriptUpdates: { card: true },
        onClose: () => actions.push('close'),
        onUpdateWithCleanup: (record, wasRunning) => actions.push([record, wasRunning]),
      }));
      const button = ui.container.querySelector('[data-testid="model-store-detail-action-update"]');
      assert.ok(button);
      assert.equal(button.textContent, copy.actionUpdate);
      assert.equal(button.disabled, busy);
      assert.equal(ui.container.querySelectorAll('[data-testid="model-store-rollback-scripts"]').length, 0);
      assert.ok(!ui.container.textContent.includes(copy.cardScriptsRollback));
      assert.equal(ui.container.querySelectorAll('[data-testid="model-store-detail-action-uninstall"]').length, 0);
      assert.equal(ui.container.querySelectorAll('[data-testid="model-store-detail-action-deploy"]').length, 0);
      await ui.click('[data-testid="model-store-detail-action-update"]');
      assert.deepEqual(actions, busy ? [] : ['close', [ui.record, running]]);
      assert.deepEqual(ui.confirmations, busy ? [] : [running ? copy.updateConfirmRunning : copy.updateConfirmStopped]);
    });
  }
}

for (const running of [false, true]) {
  test(`real view detail click reaches serial workflow, running=${running}`, async (t) => {
    const ui = await fixture(t, { running });
    await ui.render(React.createElement(ModelStoreView, { theme: 'light', t: dict.zh, bs: {} }));
    await ui.click('[data-testid="model-store-open-detail"]');
    ui.confirm(false);
    await ui.click('[data-testid="model-store-detail-action-update"]');
    assert.equal(ui.starts.length, 0);
    assert.ok(ui.container.querySelector('[data-testid="model-store-detail-dialog"]'));
    ui.confirm(true);
    await ui.click('[data-testid="model-store-detail-action-update"]');
    assert.equal(ui.container.querySelectorAll('[data-testid="model-store-detail-dialog"]').length, 0);
    const expected = [
      ...(running ? [{ cardId: 'card', action: 'stop' }] : []),
      { cardId: 'card', action: 'uninstall', purgeWeights: false },
      { cardId: 'card', rollback: false, confirmed: true },
    ];
    for (let index = 0; index < expected.length; index += 1) {
      // The backend has returned run_id, but run_log remains pending.
      await act(async () => { await new Promise((resolve) => setTimeout(resolve, 5)); });
      assert.equal(ui.starts.length, index + 1);
      assert.deepEqual(ui.starts[index].payload, expected[index]);
      assert.equal(ui.starts[index].command, index === expected.length - 1
        ? 'model_store_start_card_scripts' : 'model_store_start_lifecycle');
      await ui.finish(index);
    }
    assert.equal(ui.starts.length, expected.length);
    assert.equal(getTasks().length, 0);
  });
}

for (const [status, running, action, disabled] of [
  ['deployed', false, 'uninstall', false],
  ['deployed', true, 'uninstall', true],
  ['downloaded', false, 'deploy', false],
]) {
  test(`no update: ${status}, running=${running} keeps ${action} action`, async (t) => {
    const ui = await fixture(t, { status, running });
    const actions = [];
    await ui.render(React.createElement(ModelDetailDialog, {
      cardId: 'card', copy: dict.zh.uiModelStore, theme: 'light', scriptUpdates: {},
      onClose() {}, onUninstall: () => actions.push('uninstall'), onRedeploy: () => actions.push('deploy'),
    }));
    assert.equal(ui.container.querySelectorAll('[data-testid="model-store-detail-action-update"]').length, 0);
    const selector = `[data-testid="model-store-detail-action-${action}"]`;
    assert.equal(ui.container.querySelector(selector).disabled, disabled);
    await ui.click(selector);
    assert.deepEqual(actions, disabled ? [] : [action]);
  });
}
