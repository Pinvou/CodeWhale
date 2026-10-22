import assert from 'node:assert/strict';
import test from 'node:test';
import { updateScriptsWithCleanup } from '../src/features/model-store/update-scripts-workflow.js';
import { cancelTask, dismissTask, getTasks } from '../src/features/model-store/useLmmTasks.js';
import { dict } from './helpers/i18n-all.js';

const record = { card_id: 'workflow-card', card_name: 'Workflow model' };
const copy = dict.zh.uiModelStore;

// Only the IPC boundary is stubbed: the production workflow, API wrappers,
// runTask, task state and terminal polling all execute.
function backend(t) {
  const previous = globalThis.__TAURI__;
  const starts = [];
  const runs = new Map();
  const workflows = [];
  let closing = false;
  globalThis.__TAURI__ = { core: { invoke: async (command, payload) => {
    if (command === 'model_store_run_log') {
      const run = runs.get(payload.runId);
      run.polled = true;
      return run.promise;
    }
    if (command === 'model_store_cancel_run') {
      runs.get(payload.runId).finish({ ok: false, cancelled: true });
      return { ok: true };
    }
    assert.ok(['model_store_start_lifecycle', 'model_store_start_card_scripts'].includes(command));
    const kind = payload.action || 'card_scripts';
    const id = `run-${starts.length}`;
    const deferred = Promise.withResolvers();
    const run = {
      ...deferred, polled: false,
      finish: (outcome = { ok: true }) => deferred.resolve({
        lines: [], next: 0, done: true, cancelled: false, ...outcome,
      }),
    };
    runs.set(id, run);
    starts.push({ command, payload, kind, id });
    if (closing) run.finish({ ok: false, cancelled: true });
    return { run_id: id };
  } } };
  t.after(async () => {
    closing = true;
    for (const run of runs.values()) run.finish({ ok: false, cancelled: true });
    await Promise.allSettled(workflows);
    for (const task of [...getTasks()]) dismissTask(task.id);
    if (previous === undefined) delete globalThis.__TAURI__;
    else globalThis.__TAURI__ = previous;
  });
  return {
    starts,
    start(wasRunning = true) {
      const promise = updateScriptsWithCleanup({ record, wasRunning, copy });
      workflows.push(promise);
      return promise;
    },
    async pending(index) {
      for (let attempt = 0; attempt < 200; attempt += 1) {
        const run = runs.get(starts[index]?.id);
        if (run?.polled) return run;
        await new Promise((resolve) => setTimeout(resolve, 5));
      }
      assert.fail(`step ${index} did not start polling`);
    },
  };
}

test('run_id 返回后仍等待终态，stop → scripts 两步串行（2026-10-22 根治：不再卸载）', async (t) => {
  const api = backend(t);
  const workflow = api.start();
  for (const [index, kind, title] of [
    [0, 'stop', copy.taskStop(record.card_name)],
    [1, 'card_scripts', copy.cardScriptsTask(record.card_name)],
  ]) {
    const pending = await api.pending(index);
    await new Promise((resolve) => setTimeout(resolve, 10));
    assert.equal(api.starts.length, index + 1, '未终态时不得发起后续步骤');
    assert.equal(api.starts[index].kind, kind);
    const task = getTasks().find((item) => item.runId === api.starts[index].id);
    assert.equal(task.title, title);
    assert.equal(task.status, 'running');
    pending.finish();
  }
  assert.equal((await workflow).ok, true);
  assert.deepEqual(api.starts.map(({ command, payload }) => ({ command, payload })), [
    { command: 'model_store_start_lifecycle', payload: { cardId: record.card_id, action: 'stop' } },
    { command: 'model_store_start_card_scripts', payload: { cardId: record.card_id, confirmed: true } },
  ]);
});

for (const failedIndex of [0]) {
  test(`${failedIndex === 0 ? 'stop' : 'card_scripts'} 终态失败不继续且保留错误`, async (t) => {
    const api = backend(t);
    const workflow = api.start();
    if (failedIndex === 1) (await api.pending(0)).finish();
    (await api.pending(failedIndex)).finish({ ok: false, error: 'operation failed' });
    assert.deepEqual(await workflow, { ok: false, error: 'operation failed' });
    assert.equal(api.starts.length, failedIndex + 1);
    assert.equal(getTasks().at(-1).error, 'operation failed');
  });
}

for (const cancelledIndex of [0, 1]) {
  test(`取消第 ${cancelledIndex + 1} 步后不继续`, async (t) => {
    const api = backend(t);
    const workflow = api.start();
    if (cancelledIndex === 1) (await api.pending(0)).finish();
    await api.pending(cancelledIndex);
    await cancelTask(getTasks().find((task) => task.runId === api.starts[cancelledIndex].id).id);
    assert.deepEqual(await workflow, { ok: false, cancelled: true });
    assert.equal(api.starts.length, cancelledIndex + 1);
  });
}

test('重复工作流被真实 runTask 拒绝，不发起任何额外步骤', async (t) => {
  const api = backend(t);
  const first = api.start();
  const pending = await api.pending(0);
  assert.deepEqual(await api.start(), { ok: false, duplicate: true });
  assert.equal(api.starts.length, 1);
  pending.finish({ ok: false, cancelled: true });
  await first;
  assert.equal(api.starts.length, 1);
});

test('未运行时直接更新脚本（无卸载步骤）', async (t) => {
  const api = backend(t);
  const workflow = api.start(false);
  const scripts = await api.pending(0);
  assert.deepEqual(api.starts.map((step) => step.kind), ['card_scripts']);
  scripts.finish();
  assert.equal((await workflow).ok, true);
  assert.deepEqual(api.starts.map((step) => step.kind), ['card_scripts']);
});
