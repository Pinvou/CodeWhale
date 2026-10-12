// Presentation-only selectors for the B panel. The authoritative generation,
// session and task lifecycle remain in the Rust-backed remote-control domain.
const ACTIVE_TASK_STATES = new Set(['admitted', 'running', 'waiting_permission']);
export function permissionPollingTaskIds(tasks) {
  return JSON.stringify((Array.isArray(tasks) ? tasks : [])
    .filter(task => task.status === 'waiting_permission').map(task => task.taskId).sort());
}

// One read in flight; teardown suppresses late replies. A stable waiting task
// can receive more approvals, so membership alone is not a refresh trigger.
export function startPermissionPolling({ read, onSuccess, onError,
  schedule = setInterval, cancel = clearInterval, intervalMs = 2000 }) {
  let closed = false;
  let reading = false;
  const refresh = async () => {
    if (closed || reading) return;
    reading = true;
    try {
      const result = await read();
      if (!closed) onSuccess(result);
    } catch (error) {
      if (!closed) onError(error);
    } finally { reading = false; }
  };
  void refresh();
  const timer = schedule(() => { void refresh(); }, intervalMs);
  return () => { closed = true; cancel(timer); };
}
export const CONTROLLED_BOUND_STATUSES = new Set([
  'connected', 'reconnecting', 'resume_provisional', 'resume_activated_pending_liveness',
]);

export function countdownAnnouncement(seconds) {
  if (!Number.isSafeInteger(seconds) || seconds <= 0) return null;
  return seconds <= 10 || seconds % 30 === 0 ? seconds : null;
}

// B reports a monotonic-clock duration, never a wall-clock timestamp.
export function countdownSeconds(remainingMs, observedAtMs, nowMs) {
  if (!Number.isSafeInteger(remainingMs) || remainingMs <= 0) return 0;
  const elapsed = Number.isFinite(observedAtMs) && Number.isFinite(nowMs)
    ? Math.max(0, nowMs - observedAtMs) : 0;
  return Math.max(0, Math.ceil((remainingMs - elapsed) / 1000));
}

// PermissionView exposes only redaction sentinels. Never retain or render an
// unexpected payload value: it could contain a path or another sensitive fact.
export function permissionRedactionCopyKey(value) {
  if (value == null || value === '') return null;
  if (value === '[PATH]') return 'permissionPathHidden';
  if (value === '[REDACTED]') return 'permissionSensitiveHidden';
  return 'permissionDetailHidden';
}

export function permissionOperationCopyKey(value) {
  switch (value) {
    case 'file_write': return 'permissionOperationFileWrite';
    case 'file_delete': return 'permissionOperationFileDelete';
    case 'shell_command': return 'permissionOperationShellCommand';
    case 'network_request': return 'permissionOperationNetworkRequest';
    case 'tool_call': return 'permissionOperationToolCall';
    case 'other': return 'permissionOperationOther';
    default: return 'permissionOperationUnknown';
  }
}

export function normalizeSelectedWorkspaces(selected) {
  const paths = Array.isArray(selected) ? selected : [selected];
  const seen = new Set();
  const workspaces = [];
  for (const candidate of paths) {
    if (typeof candidate !== 'string' || !candidate.trim()) continue;
    const path = candidate.trim();
    if (seen.has(path)) continue;
    seen.add(path);
    const displayName = Array.from(path.split(/[\\/]/).filter(Boolean).at(-1) || '').slice(0, 120).join('');
    if (!displayName) continue;
    workspaces.push({ path, displayName, codeAllowed: true });
    if (workspaces.length >= 16) break;
  }
  return workspaces;
}

export function currentControlledTasks(device, session, tasks) {
  if (!CONTROLLED_BOUND_STATUSES.has(device?.status)
    || !device?.generation || device.generation !== session?.generation
    || !session?.sessionId || !Array.isArray(tasks)) return [];
  return tasks.filter(task => task && task.sessionId === session.sessionId
    && typeof task.taskId === 'string' && ACTIVE_TASK_STATES.has(task.status)).slice(0, 64);
}
