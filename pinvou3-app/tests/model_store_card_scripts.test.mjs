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

test('script changes require explicit boolean confirmation', async () => {
  const previous = globalThis.__TAURI__;
  const calls = [];
  globalThis.__TAURI__ = { core: { invoke: async (name, args) => { calls.push([name, args]); return {}; } } };
  try {
    for (const value of [undefined, null, false, 'true', 'false', {}, true]) {
      await startCardScripts('card', value);
      assert.deepEqual(calls.at(-1), ['model_store_start_card_scripts', { cardId: 'card', confirmed: value === true }]);
    }
  } finally {
    if (previous === undefined) delete globalThis.__TAURI__;
    else globalThis.__TAURI__ = previous;
  }
});

async function fixture(t, { running = false, status = 'deployed', activeRuns = [], scriptSha256 = { enable: 'new' } } = {}) {
  const saved = new Map(['window', 'document', '__TAURI__', 'IS_REACT_ACT_ENVIRONMENT']
    .map((key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)]));
  const window = createWindow('<!doctype html><html><body><div id="root"></div></body></html>');
  const record = { card_id: 'card', card_name: 'Test model', running, status, weights_ready_at: '2026-10-01T00:00:00Z', local_script_hashes: { enable: 'old' } };
  const starts = [];
  const runs = new Map();
  const recoveringRuns = [...activeRuns];
  const cancellations = [];
  for (const run of activeRuns) runs.set(run.run_id, Promise.withResolvers());
  const unexpected = [];
  const nativeConfirmCalls = [];
  window.confirm = (message) => {
    nativeConfirmCalls.push(message);
    throw new Error('Native confirmation must not be used');
  };
  globalThis.window = window;
  globalThis.document = window.document;
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  globalThis.__TAURI__ = { core: { invoke: async (command, payload) => {
    switch (command) {
      case 'model_store_availability': return { available: true };
      case 'model_store_cards': return [{ id: 'card', name: record.card_name, script_sha256: scriptSha256 }];
      case 'model_store_library': return { models: [record], corrected: [] };
      case 'model_store_model_detail': return { record, runtime_lines: [] };
      case 'model_store_api_keys': return [];
      case 'model_store_lan_status': return {
        enabled: false, available: false, reason: 'lan_model_unavailable',
        interfaces: [], port: 8090, model_id: 'pinvou:card',
      };
      case 'model_store_active_runs': return { runs: recoveringRuns };
      case 'model_store_cancel_run': cancellations.push(payload); return { ok: true };
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
      recoveringRuns.length = 0;
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
    assert.deepEqual(nativeConfirmCalls, []);
  });
  return {
    record, container, starts, finish, cancellations,
    async escape() {
      const event = new window.Event('keydown');
      event.key = 'Escape';
      await act(async () => window.dispatchEvent(event));
    },
    async doubleClick(selector) {
      const button = container.querySelector(selector);
      assert.ok(button, `Missing ${selector}`);
      await act(async () => { button.click(); button.click(); });
    },
    async render(element) { await act(async () => root.render(element)); },
    async click(selector) {
      const button = container.querySelector(selector);
      assert.ok(button, `Missing ${selector}`);
      await act(async () => button.click());
    },
  };
}

for (const language of ['zh', 'en', 'ja']) {
  test(`${language}: reload recovers a script update and its real cancel control`, async (t) => {
    const runId = `reattach-script-update-${language}`;
    const ui = await fixture(t, {
      status: 'downloaded', activeRuns: [{ run_id: runId, card_id: 'card', kind: 'card_scripts' }],
    });
    const view = () => React.createElement(ModelStoreView, { theme: 'light', t: dict[language], bs: {} });
    await ui.render(view());
    const task = getTasks().find((entry) => entry.runId === runId);
    assert.ok(task, 'The running update must remain visible after reload');
    assert.equal(task.reattached, true);
    assert.equal(task.key, 'card:card');
    const copy = dict[language].uiModelStore;
    assert.ok([copy.cardScriptsTask('card'), copy.cardScriptsTask(ui.record.card_name)].includes(task.title));
    assert.ok(ui.container.querySelector('[data-task-kind="card_scripts"]'));
    await ui.render(null);
    await ui.render(view());
    assert.equal(getTasks().filter((entry) => entry.runId === runId).length, 1);
    assert.equal(ui.starts.length, 0, 'Recovery must not start a second update');
    await ui.click('[data-testid="model-store-task-cancel"]');
    assert.deepEqual(ui.cancellations, [{ runId }]);
    assert.equal(getTasks().find((entry) => entry.runId === runId).status, 'cancelling');
  });
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
      assert.equal(ui.container.querySelectorAll('[data-testid="model-store-detail-action-uninstall"]').length, 0);
      assert.equal(ui.container.querySelectorAll('[data-testid="model-store-detail-action-deploy"]').length, 0);
      await ui.click('[data-testid="model-store-detail-action-update"]');
      assert.deepEqual(actions, []);
      if (busy) {
        assert.equal(ui.container.querySelectorAll('[data-testid="model-store-update-confirm"]').length, 0);
      } else {
        const confirmation = ui.container.querySelector('[data-testid="model-store-update-confirm"]');
        assert.ok(confirmation);
        assert.ok(confirmation.textContent.includes(running ? copy.updateConfirmRunning : copy.updateConfirmStopped));
        assert.equal(ui.container.querySelector('[data-testid="model-store-update-cancel"]').textContent, copy.cardScriptsCancel);
        assert.equal(ui.container.querySelector('[data-testid="model-store-update-confirm-submit"]').textContent, copy.cardScriptsConfirm);
        await ui.click('[data-testid="model-store-update-cancel"]');
        assert.deepEqual(actions, []);
        assert.ok(ui.container.querySelector('[data-testid="model-store-detail-dialog"]'));
        await ui.click('[data-testid="model-store-detail-action-update"]');
        await ui.click('[data-testid="model-store-update-confirm-submit"]');
        assert.deepEqual(actions, ['close', [ui.record, running]]);
      }
    });
  }
}

for (const running of [false, true]) {
  test(`real view detail click reaches serial workflow, running=${running}`, async (t) => {
    const ui = await fixture(t, { running });
    await ui.render(React.createElement(ModelStoreView, { theme: 'light', t: dict.zh, bs: {} }));
    await ui.click('[data-testid="model-store-open-detail"]');
    await ui.click('[data-testid="model-store-detail-action-update"]');
    assert.equal(ui.starts.length, 0);
    await ui.click('[data-testid="model-store-update-cancel"]');
    assert.equal(ui.starts.length, 0);
    assert.ok(ui.container.querySelector('[data-testid="model-store-detail-dialog"]'));
    await ui.click('[data-testid="model-store-detail-action-update"]');
    assert.equal(ui.starts.length, 0);
    await ui.click('[data-testid="model-store-update-confirm-submit"]');
    assert.equal(ui.container.querySelectorAll('[data-testid="model-store-detail-dialog"]').length, 0);
    const expected = [
      ...(running ? [{ cardId: 'card', action: 'stop' }] : []),
      { cardId: 'card', confirmed: true },
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

test('confirmation rechecks busy state and ignores a double submit', async (t) => {
  const ui = await fixture(t);
  const actions = [];
  const render = (cardBusy) => ui.render(React.createElement(ModelDetailDialog, {
    cardId: 'card', copy: dict.zh.uiModelStore, theme: 'light', cardBusy, scriptUpdates: { card: true },
    onClose: () => actions.push('close'),
    onUpdateWithCleanup: (record, running) => actions.push([record, running]),
  }));
  await render(false);
  await ui.click('[data-testid="model-store-detail-action-update"]');
  await render(true);
  assert.equal(ui.container.querySelector('[data-testid="model-store-update-confirm-submit"]').disabled, true);
  await ui.click('[data-testid="model-store-update-confirm-submit"]');
  assert.deepEqual(actions, []);
  // Cancellation remains available even if the card becomes busy.
  await ui.click('[data-testid="model-store-update-cancel"]');
  assert.ok(ui.container.querySelector('[data-testid="model-store-detail-dialog"]'));
  await render(false);
  await ui.click('[data-testid="model-store-detail-action-update"]');
  await ui.doubleClick('[data-testid="model-store-update-confirm-submit"]');
  assert.deepEqual(actions, ['close', [ui.record, false]]);
});

for (const dismiss of ['escape', 'backdrop', 'close']) {
  test(`${dismiss} dismisses only the custom confirmation without starting work`, async (t) => {
    const ui = await fixture(t);
    const actions = [];
    await ui.render(React.createElement(ModelDetailDialog, {
      cardId: 'card', copy: dict.zh.uiModelStore, theme: 'light', scriptUpdates: { card: true },
      onClose: () => actions.push('close'), onUpdateWithCleanup: () => actions.push('update'),
    }));
    await ui.click('[data-testid="model-store-detail-action-update"]');
    if (dismiss === 'escape') await ui.escape();
    else if (dismiss === 'backdrop') await ui.click('[role="presentation"]');
    else await ui.click('[data-testid="model-store-update-confirm"] button[aria-label]');
    assert.deepEqual(actions, []);
    assert.ok(ui.container.querySelector('[data-testid="model-store-detail-dialog"]'));
    assert.equal(ui.container.querySelectorAll('[data-testid="model-store-update-confirm"]').length, 0);
  });
}

test('closing and reopening the detail does not reuse an old confirmation', async (t) => {
  const ui = await fixture(t);
  const actions = [];
  const render = (cardId) => ui.render(React.createElement(ModelDetailDialog, {
    cardId, copy: dict.zh.uiModelStore, theme: 'light', scriptUpdates: { card: true },
    onClose() {}, onUpdateWithCleanup: () => actions.push('update'),
  }));
  await render('card');
  await ui.click('[data-testid="model-store-detail-action-update"]');
  await render(null);
  await render('card');
  assert.ok(ui.container.querySelector('[data-testid="model-store-detail-dialog"]'));
  assert.equal(ui.container.querySelectorAll('[data-testid="model-store-update-confirm"]').length, 0);
  assert.deepEqual(actions, []);
});

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

// 评审 P2-1：权重未就绪（incomplete/缺 weights_ready_at）的记录不给可点的
// 「部署模型」——发起必然失败的部署只会产生看不懂的报错。
test('no update: 权重未就绪时部署按钮禁用（不发起注定失败的部署）', async (t) => {
  const ui = await fixture(t, { status: 'downloaded', running: false });
  // 夹具默认带 weights_ready_at；这里模拟缺凭据的记录
  const saved = ui.record;
  saved.weights_ready_at = null;
  await ui.render(React.createElement(ModelDetailDialog, {
    cardId: 'card', copy: dict.zh.uiModelStore, theme: 'light', scriptUpdates: {},
    onClose() {}, onUninstall: () => {}, onRedeploy: () => { throw new Error('不应发起部署'); },
  }));
  const btn = ui.container.querySelector('[data-testid="model-store-detail-action-deploy"]');
  assert.ok(btn);
  assert.equal(btn.disabled, true, '权重未就绪：部署按钮必须禁用');
});

// r21 P1-1：cleanup_required 的主按钮。mark_cleanup_required 不清 weights_ready_at
// （db.rs），此前弹窗因此落进 final else——weights 在时给**可点的「部署模型」**，
// weights 缺时给 disabled 部署 + title「需继续下载」，两个子情形都与状态行
// 「需继续清理」自相矛盾；行卡同态一直是 danger「继续清理」。测试注释曾声称
// 覆盖 cleanup_required，但矩阵里一个案例都没有——这里补真实夹具：主按钮必须是
// 「继续清理」且走 onUninstall（由 uninstallOptionsFor 给出 cleanup-only 确认框），
// 不得出现「部署模型」。
for (const weightsReady of [true, false]) {
  test(`no update: cleanup_required(weights_ready_at=${weightsReady ? 'set' : 'null'}) 主按钮是「继续清理」不是部署`, async (t) => {
    const ui = await fixture(t, { status: 'cleanup_required', running: false });
    if (!weightsReady) ui.record.weights_ready_at = null;
    const actions = [];
    await ui.render(React.createElement(ModelDetailDialog, {
      cardId: 'card', copy: dict.zh.uiModelStore, theme: 'light', scriptUpdates: {},
      onClose() {}, onUninstall: () => actions.push('uninstall'),
      onRedeploy: () => { throw new Error('cleanup_required 不得发起部署'); },
    }));
    assert.equal(
      ui.container.querySelectorAll('[data-testid="model-store-detail-action-deploy"]').length,
      0,
      'cleanup_required 不得出现「部署模型」按钮'
    );
    const cleanup = ui.container.querySelector('[data-testid="model-store-detail-action-cleanup"]');
    assert.ok(cleanup, '必须有「继续清理」主按钮');
    assert.equal(cleanup.textContent, dict.zh.uiModelStore.continueCleanup);
    assert.equal(cleanup.disabled, false);
    await ui.click('[data-testid="model-store-detail-action-cleanup"]');
    assert.deepEqual(actions, ['uninstall'], '走卸载链（cleanup-only 确认框由 uninstallOptionsFor 决定）');
    // 「删除模型」次级入口不出现：cleanup-only 确认框里没有删除选项
    assert.equal(
      ui.container.querySelectorAll('[data-testid="model-store-detail-secondary-delete"]').length,
      0,
      'cleanup-only 确认框没有删除选项，弹窗不得摆「删除模型」次级按钮'
    );
  });
}

// 评审 P2-2：老后台不下发 script_sha256 时，后端 serde 缺省成空对象 `{}`——truthy，
// 判空挡不住，放行进比对后本地任何非空哈希都算「服务器版本不同」，该卡全部已装
// 模型挂上假「有可用更新」；用户确认更新会先停运行中模型、再去一个根本没有脚本
// 的服务端拉新版。空 map 必须按「无更新」处理（本地与列表的 scriptUpdates 同源）。
test('服务端空的 script_sha256 不产生「有可用更新」徽标', async (t) => {
  const ui = await fixture(t, { scriptSha256: {} });
  await ui.render(React.createElement(ModelStoreView, { theme: 'light', t: dict.zh, bs: {} }));
  assert.equal(
    ui.container.querySelectorAll('[data-testid="model-store-script-update-badge"]').length,
    0,
    '服务端没有脚本版本信息时，行卡不得挂「有可用更新」徽标'
  );
  // 详情弹窗的「更新脚本」入口与徽标共用 scriptUpdates，也不得出现。
  await ui.click('[data-testid="model-store-open-detail"]');
  assert.equal(
    ui.container.querySelectorAll('[data-testid="model-store-detail-action-update"]').length,
    0,
    '服务端空的 script_sha256 时，详情弹窗不得给「更新脚本」入口'
  );
});

// 评审 P2-6：deployed 但 weights_ready_at 缺失的记录（升级前的老库、部署时发现
// 权重缺失被清掉凭据），列表行卡按 libraryStateOf 判成 incomplete 显示「需继续
// 下载」，详情弹窗却曾按 raw status==='deployed' 摆出「卸载部署」+「重新部署」；
// 点「卸载部署」后卸载确认框又按 delete-only 只剩「彻底删除」。弹窗与行卡必须
// 同一判据。
test('deployed+缺 weights_ready_at：弹窗显示「需继续下载」且无「卸载部署」', async (t) => {
  const ui = await fixture(t, { status: 'deployed', running: false });
  ui.record.weights_ready_at = null;
  const actions = [];
  await ui.render(React.createElement(ModelDetailDialog, {
    cardId: 'card', copy: dict.zh.uiModelStore, theme: 'light', scriptUpdates: {},
    onClose() {},
    onUninstall: () => actions.push('uninstall'),
    onRedeploy: () => actions.push('redeploy'),
  }));
  // 状态行走 libraryStateOf → incomplete，与行卡徽章同一句文案
  assert.ok(
    ui.container.textContent.includes(dict.zh.uiModelStore.statusIncomplete),
    '状态行必须显示「需继续下载」而不是「已部署」'
  );
  assert.equal(
    ui.container.querySelectorAll('[data-testid="model-store-detail-action-uninstall"]').length,
    0,
    '权重未就绪时不得给「卸载部署」'
  );
  assert.equal(
    ui.container.querySelectorAll('[data-testid="model-store-detail-secondary-redeploy"]').length,
    0,
    '权重未就绪时不得给「重新部署」'
  );
  // 与 downloaded+缺凭据（P2-1）一致：部署按钮禁用、次级动作只剩删除
  const deploy = ui.container.querySelector('[data-testid="model-store-detail-action-deploy"]');
  assert.ok(deploy);
  assert.equal(deploy.disabled, true);
  assert.deepEqual(actions, []);
});
