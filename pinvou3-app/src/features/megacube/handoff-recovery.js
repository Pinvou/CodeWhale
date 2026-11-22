/** Merge a reviewed recovery receipt with browser-persisted accepted handoffs. */
export function mergeHandoffRecovery(browser, recovery, completed = new Set()) {
  const result = new Map();
  for (const value of [...recovery, ...browser]) {
    if (value?.task?.id && !completed.has(value.task.id)) result.set(value.task.id, value);
  }
  return [...result.values()];
}
