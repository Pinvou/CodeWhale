import assert from 'node:assert/strict';
import test from 'node:test';
import { createWindow } from '@mixmark-io/domino';
import React, { act } from 'react';
import { createRoot } from 'react-dom/client';
import { loadJsx } from './fixtures/jsx-module-hooks.mjs';
import { dict } from './helpers/i18n-all.js';

const { ModelStoreView } = await loadJsx('../src/features/model-store/ModelStoreView.jsx', import.meta.url);

test('ModelStoreView 首次挂载完成真实 hooks 初始化并显示加载引导', async () => {
  const saved = new Map(['window', 'document', '__TAURI__', 'IS_REACT_ACT_ENVIRONMENT']
    .map((key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)]));
  const window = createWindow('<!doctype html><html><body><div id="root"></div></body></html>');
  const calls = [];
  let root;
  try {
    globalThis.window = window;
    globalThis.document = window.document;
    globalThis.IS_REACT_ACT_ENVIRONMENT = true;
    globalThis.__TAURI__ = { core: { invoke: (command) => {
      calls.push(command);
      assert.equal(command, 'model_store_availability');
      // Keep availability pending to inspect the first committed loading view.
      return new Promise(() => {});
    } } };
    const container = window.document.getElementById('root');
    root = createRoot(container);
    await act(async () => {
      root.render(React.createElement(ModelStoreView, { theme: 'light', t: dict.zh, bs: {} }));
    });
    assert.ok(container.querySelector('[data-testid="model-store-view"]'));
    assert.ok(container.querySelector('[data-testid="model-store-guide"][data-guide-state="loading"]'));
    assert.ok(container.textContent.includes(dict.zh.uiModelStore.guideLoadingTitle));
    assert.deepEqual(calls, ['model_store_availability']);
  } finally {
    if (root) await act(async () => root.unmount());
    for (const [key, descriptor] of saved) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor);
      else delete globalThis[key];
    }
  }
});
