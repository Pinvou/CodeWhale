/** Same-window notification transport; callers validate accepted task facts. */
const listeners = new Set();
export function emitRuntimeTask(task) {
  for (const listener of listeners) listener(task);
}
export function subscribeRuntimeTasks(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}
