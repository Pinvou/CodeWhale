/** Presentation acknowledgements only. Never alter device facts or control rights. */
export const cmsNotice = (group, id, detail) => ({ group, id, detail: String(detail) });
export const modelIssueNotice = (card, reason) => cmsNotice('model-issue', card, reason);
export const isFailedCmsTask = task => ['failed', 'interrupted'].includes(task.status);
export const hasCmsTaskIssue = task => isFailedCmsTask(task) || !!task.error;
export const taskIssueNotice = task => cmsNotice('task-failure',
  JSON.stringify([task.source || 'cmsd', task.id || task.run_id]),
  JSON.stringify([task.status, task.created_at ?? task.started_at ?? null,
    task.error || task.last_event?.message || task.last_line || null]));

export function splitCmsTasks(tasks, isRead) {
  const history = [], attention = [];
  for (const task of tasks || []) {
    (['succeeded', 'cancelled'].includes(task.status)
      || (isFailedCmsTask(task) && isRead(taskIssueNotice(task))) ? history : attention).push(task);
  }
  return { history, attention };
}
export const noticeIdentity = notice => JSON.stringify([notice.group, notice.id]);
export const noticeReadKey = scope => /^[a-f0-9]{64}$/.test(scope || '')
  ? `pinvou.megacube.notice-read.v1.${scope}` : null;
const validNotice = n => n && typeof n.group === 'string' && n.group.length <= 64
  && typeof n.id === 'string' && n.id.length <= 512
  && typeof n.detail === 'string' && n.detail.length <= 16384;

export function readCmsNotices(storage, key) {
  if (!key) return [];
  try {
    const raw = storage.getItem(key);
    if (raw === null) return [];
    if (raw.length > 524288) return null;
    const value = JSON.parse(raw);
    if (value.version !== 1 || !Array.isArray(value.read) || value.read.length > 128) return null;
    if (!value.read.every(validNotice)) return null;
    return value.read;
  } catch { return null; }
}

export function saveCmsNotices(storage, key, read) {
  if (!key) return false;
  try {
    if (read.length > 128 || !read.every(validNotice)) return false;
    const raw = JSON.stringify({ version: 1, read });
    if (raw.length > 524288) return false;
    storage.setItem(key, raw);
    return storage.getItem(key) === raw;
  } catch { return false; }
}

export function isCmsNoticeRead(read, notice) {
  return read.some(n => noticeIdentity(n) === noticeIdentity(notice) && n.detail === notice.detail);
}

export function markCmsNoticeRead(read, notice) {
  return [...read.filter(n => noticeIdentity(n) !== noticeIdentity(notice)), notice].slice(-128);
}

/** Missing groups are unknown, never evidence that an earlier fault recovered. */
export function reconcileCmsNotices(read, observedGroups) {
  return read.filter(n => {
    if (!Object.hasOwn(observedGroups, n.group)) return true;
    const group = observedGroups[n.group];
    // Task lists may be paginated/truncated: absence does not mean recovery.
    if (!Array.isArray(group) && !group.ids.includes(n.id)) return true;
    return (Array.isArray(group) ? group : group.notices).some(current =>
      noticeIdentity(current) === noticeIdentity(n) && current.detail === n.detail);
  });
}

export function engineIssueNotice(engines) {
  const failures = (engines || []).filter(e => e.instances.some(i => i.alive && i.healthy === false));
  return cmsNotice('engine-health', 'engines', JSON.stringify(failures.map(e => [e.id,
    e.instances.filter(i => i.alive && i.healthy === false).map(i => [i.pid, i.port]).sort(),
  ]).sort()));
}

/** Only successful, definite observations reset an acknowledgement. */
export function cmsNoticeObservations({ status, models, engines, tasks, partial, unavailable }) {
  if (unavailable) return { connection: [cmsNotice('connection', 'cmsd', unavailable)] };
  if (!status) return {};
  const groups = { connection: [], query: partial ? [cmsNotice('query', 'partial', partial)] : [] };
  if (tasks) groups['task-failure'] = {
    ids: tasks.map(task => taskIssueNotice(task).id),
    notices: tasks.filter(hasCmsTaskIssue).map(taskIssueNotice),
  };
  if (models) {
    groups['model-issue'] = [...status.degraded.map(d => modelIssueNotice(d.card, d.reason)),
      ...models.models.filter(m => m.degradedReason).map(m => modelIssueNotice(m.card, m.degradedReason))];
    if (!models.models.some(m => m.status === 'running' && m.healthy === null)) {
      groups['model-health'] = models.models.filter(m => m.status === 'running' && m.healthy === false)
        .map(m => cmsNotice('model-health', m.card, 'unhealthy'));
    }
    const active = models.models.find(m => m.card === status.activeModel);
    groups['default-ready'] = active && (active.status !== 'running' || active.healthy !== true)
      ? [cmsNotice('default-ready', active.card, 'not-ready')] : [];
  }
  if (engines && !engines.some(e => e.instances.some(i => i.alive && i.healthy === null))) {
    groups['engine-health'] = engines.some(e => e.instances.some(i => i.alive && i.healthy === false))
      ? [engineIssueNotice(engines)] : [];
  }
  return groups;
}
