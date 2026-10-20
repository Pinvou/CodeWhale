import { startCardScripts, startStop, startUninstall } from './model-store-api.js';
import { lifecycleTaskKey } from './model-store-logic.js';
import { runTask } from './useLmmTasks.js';

/** Wait for each job's terminal result before starting the next lifecycle step. */
export async function updateScriptsWithCleanup({ record, wasRunning, copy }) {
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
      start: () => startCardScripts(cardId, false, true),
    },
  ];
  let result;
  for (const step of steps) {
    result = await runTask({ ...step, key: lifecycleTaskKey(cardId) });
    if (!result.ok) return result;
  }
  return result;
}
