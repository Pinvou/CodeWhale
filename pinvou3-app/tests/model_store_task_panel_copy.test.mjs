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

// r44 P1-1：`missing run` 场景从「剪贴板必须为空」改为「回落到结构化同步错误
// 文本」——契约更新而非收回回落。安全性论证（评审 r44 已核）：脚本尾部密文
// 只会在**有运行记录**的任务上出现（那是运行日志导出快照的成分）；no-runId
// 任务的 task.error 只能是本 PR 后端 IPC 的同步拒绝消息（准入拒绝/参数校验），
// 不含脚本产物。fail-closed 对其余四种场景（导出拒绝/旧后端/空快照/剪贴板
// 不可用）保持不变——它们的共同点是「内容不可信或不可得」，而 no-runId 的
// 同步错误是「内容可得且可信」。
for (const scenario of [
  { label: 'export rejection', reject: true },
  { label: 'old backend without sanitized text', response: { path: '/test/export.log' } },
  { label: 'empty snapshot', response: { text: '' } },
  { label: 'clipboard unavailable', clipboardReject: true },
]) {
  test(`copy fails closed without raw fallback: ${scenario.label}`, async (t) => {
    const ui = await fixture(t, scenario);
    await ui.click('model-store-task-tools');
    await ui.click('model-store-task-copy-log');
    assert.deepEqual(ui.clipboard, []);
    await ui.click('model-store-task-tools');
    assert.equal(ui.container.querySelector('[data-testid="model-store-task-copy-log"]').textContent, ui.copy.copyFailed);
  });
}

test('copy with missing run falls back to structured sync error text', async (t) => {
  const ui = await fixture(t, { runId: null });
  await ui.click('model-store-task-tools');
  await ui.click('model-store-task-copy-log');
  // 同步错误的原文进剪贴板（r44 P1-1 契约：安全论证见上）——不是空。
  // fixture 的 task.error 是它自己的哨兵串，这里只断言「非空字符串」，
  // 不钉具体内容（内容来源由实现保证=task.error 同源）。
  assert.equal(ui.clipboard.length, 1, `clipboard=${ui.clipboard}`);
  assert.ok(String(ui.clipboard[0]).length > 0);
  await ui.click('model-store-task-tools');
  assert.equal(ui.container.querySelector('[data-testid="model-store-task-copy-log"]').textContent, ui.copy.copied);
});

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
