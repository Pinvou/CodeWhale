import { startCardScripts, startStop, startUninstall } from './model-store-api.js';
import { lifecycleTaskKey } from './model-store-logic.js';
import { runTask } from './useLmmTasks.js';

/**
 * Wait for each job's terminal result before starting the next lifecycle step.
 * `onUninstalled` 在卸载部署成功后调用一次：部署已经不在了，品悟模型列表里指向
 * 它的行成了死数据——清理时机与手动卸载同一条判据（评审 P2：更新失败/不再重
 * 部署时残留死行且无提示，2026-09-22 真机事故的同款形状）。
 */
export async function updateScriptsWithCleanup({ record, wasRunning, copy, onUninstalled }) {
  const cardId = record.card_id;
  const name = record.card_name;
  const steps = [
    ...(wasRunning ? [{
      kind: 'stop', title: copy.taskStop(name), start: () => startStop(cardId),
    }] : []),
    {
      kind: 'uninstall', title: copy.taskUninstall(name),
      start: () => startUninstall(cardId, false),
    },
    {
      kind: 'card_scripts', title: copy.cardScriptsTask(name),
      start: () => startCardScripts(cardId, true),
    },
  ];
  let result;
  for (const step of steps) {
    result = await runTask({ ...step, key: lifecycleTaskKey(cardId) });
    if (!result.ok) return result;
    if (step.kind === 'uninstall' && onUninstalled) await onUninstalled();
  }
  return result;
}
