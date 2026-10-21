// Completed operations outlive ModelStoreView just like the task pump.
// Keep every result until explicitly acknowledged: a second cleanup must never
// replace the first operation's remaining paths while the view is unmounted.
let results = [];
const listeners = new Set();

export function getModelStoreOperationResults() {
  return results;
}

export function subscribeModelStoreOperationResults(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function replaceResults(next) {
  results = next;
  for (const listener of listeners) listener();
}

export function rememberTaskOperationResult(task, outcome) {
  if (!outcome?.ok || !task) return;
  if (!['version_stage', 'version_switch', 'version_delete'].includes(task.kind)
    && !(task.kind === 'uninstall' && outcome.result?.remaining_paths?.length)) return;
  const id = task.runId || task.id;
  if (results.some((result) => result.id === id)) return;
  replaceResults([...results, {
    id,
    title: task.title,
    kind: task.kind,
    result: {
      ...outcome.result,
      remaining_paths: Array.isArray(outcome.result?.remaining_paths) ? [...outcome.result.remaining_paths] : [],
    },
  }]);
}

export function rememberForceRemoveResult(target, result) {
  const id = `force-remove:${target.taskId}`;
  if (results.some((entry) => entry.id === id)) return id;
  replaceResults([...results, {
    id,
    title: target.cardName,
    kind: 'force_remove',
    result: { ...result, remaining_paths: Array.isArray(result?.residual_paths) ? [...result.residual_paths] : [] },
  }]);
  return id;
}

export function dismissModelStoreOperationResult(id) {
  replaceResults(results.filter((result) => result.id !== id));
}
