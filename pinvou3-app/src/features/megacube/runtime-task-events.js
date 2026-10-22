/** Same-webview/profile notifications of known enable task IDs; no control authority. */
const listeners = new Set();
const id = value => typeof value === 'string' && /^[A-Za-z0-9_.-]{1,128}$/.test(value) && !['.', '..'].includes(value);
export function normalizeRuntimeTask(value, fallback = {}) {
  if (!value || value.observation === 'unconfirmed' || (value.error && typeof value.error === 'object')) return null;
  const task = { ...fallback, ...value, id: value.id || value.task_id };
  if (task.kind !== 'enable' || !id(task.id) || !id(task.card)) return null;
  const result = { id: task.id, kind: 'enable', card: task.card, status: task.status || 'unknown' };
  for (const key of ['requested_card', 'logical_card', 'selected_version', 'deployment_revision']) {
    if (task[key] != null) { if (!id(task[key])) return null; result[key] = task[key]; }
  }
  return result;
}
export function publishRuntimeTask(value, fallback) {
  const task = normalizeRuntimeTask(value, fallback);
  if (!task) return;
  for (const listener of listeners) listener(task);
}
export function subscribeRuntimeTasks(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}
export function runtimeTasksFromOutput(output, fallback = {}, depth = 0) {
  try {
    if (depth > 4) return [];
    if (typeof output === 'string') {
      if (output.length > 262144) return [];
      output = JSON.parse(output);
    }
    const value = output?.result || output;
    if (value?.isError) return [];
    if (Array.isArray(value?.content)) {
      const text = value.content.find(item => item.type === 'text')?.text;
      return runtimeTasksFromOutput(text, fallback, depth + 1);
    }
    const data = value?.data;
    const rows = Array.isArray(data?.tasks) ? data.tasks.slice(0, 8) : [data];
    return rows.map(row => normalizeRuntimeTask(row, fallback)).filter(Boolean);
  } catch { return []; }
}
