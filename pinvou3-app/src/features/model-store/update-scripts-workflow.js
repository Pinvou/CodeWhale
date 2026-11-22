import { startCardScripts, startStop } from './model-store-api.js';
import { lifecycleTaskKey } from './model-store-logic.js';
import { runTask } from './useLmmTasks.js';

/**
 * Wait for each job's terminal result before starting the next lifecycle step.
 *
 * 2026-10-22 根治：链路只剩「停止 → 更新脚本」两步，**不再卸载**。原卸载步骤是
 * 死锁之源——本地旧 uninstall 可能被危险脚本拦截器拦、可能依赖新版运行时结构、
 * 可能被组可写目录拒绝，三种形态都把「拿新脚本」的唯一通路堵死（用户设备实锤
 * 三连）。卸载本就只对跨引擎场景必要，而部署时会按新脚本自动补装正确引擎，
 * 「更新脚本 → 重新部署」即安全路径；不卸载也不会留下指向已停服务的死行
 * （部署态保留，服务确实还在）。
 */
export async function updateScriptsWithCleanup({ record, wasRunning, copy }) {
  const cardId = record.card_id;
  const name = record.card_name;
  const steps = [
    ...(wasRunning ? [{
      kind: 'stop', title: copy.taskStop(name), start: () => startStop(cardId),
    }] : []),
    {
      kind: 'card_scripts', title: copy.cardScriptsTask(name),
      start: () => startCardScripts(cardId, true),
    },
  ];
  let result;
  for (const step of steps) {
    result = await runTask({ ...step, key: lifecycleTaskKey(cardId) });
    if (!result.ok) return result;
  }
  return result;
}
