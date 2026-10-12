// UI-only notices for the durable Rust outbox. These never decide whether a
// task was sent; only a RemoteDispatchReport or an authenticated history read
// may do that. Keeping them by local SessionStore ID preserves context when
// ChatView unmounts or the user switches conversations in this process.
const casesBySession = new Map();
const retiredBySession = new Map();
let unverifiedSequence = 0;

function validHandle(handle) {
  return handle && typeof handle === 'object'
    && /^[A-Za-z0-9._:-]{1,128}$/.test(handle.taskId || '')
    && /^[A-Za-z0-9._:-]{1,128}$/.test(handle.clientRequestId || '')
    && /^[a-fA-F0-9]{64}$/.test(handle.requestFingerprint || '');
}

export function remoteDispatchCases(localSessionId, history) {
  const entries = history?.localSessionId === localSessionId && Array.isArray(history.entries)
    ? history.entries : [];
  const terminalTasks = new Set(entries
    .filter(entry => entry?.kind === 'status' && entry.taskId
      && ['succeeded', 'failed', 'cancelled', 'interrupted'].includes(entry.body))
    .map(entry => entry.taskId));
  const cases = casesBySession.get(localSessionId);
  for (const [taskId, item] of cases || []) {
    if (terminalTasks.has(taskId)) {
      cases.delete(taskId);
      continue;
    }
    // The history visible before dispatch may have count=0. Only a later
    // authenticated read can reconcile this report against durable pending.
    if (!history || history.localSessionId !== localSessionId || history === item.staleHistory) continue;
    if (item.state === 'prepared_not_sent') {
      const matched = (history.unattemptedPending || []).some(pending =>
        pending.taskId === item.handle?.taskId
          && pending.clientRequestId === item.handle?.clientRequestId
          && pending.requestFingerprint === item.handle?.requestFingerprint);
      if (!matched) cases.delete(taskId);
    } else if (item.state === 'outcome_unknown') {
      // The count carries no task IDs. A zero proves retirement; a nonzero
      // cannot justify retaining any particular process-local task identity.
      cases.delete(taskId);
    }
  }
  if (cases?.size === 0) casesBySession.delete(localSessionId);
  const local = [...(cases?.values() || [])];
  if (history?.localSessionId !== localSessionId) return local;
  const retired = retiredBySession.get(localSessionId) || new Set();
  const stillPending = new Set((history.unattemptedPending || []).map(item => item.taskId));
  for (const taskId of retired) if (!stillPending.has(taskId)) retired.delete(taskId);
  if (!retired.size) retiredBySession.delete(localSessionId);
  const known = new Set(local.map(item => item.taskId));
  const unattempted = (history.unattemptedPending || []).filter(item =>
    !known.has(item.taskId) && !retired.has(item.taskId) && !terminalTasks.has(item.taskId))
    .map(item => ({
      localSessionId, remoteRef: null, identity: null, text: null,
      handle: {
        taskId: item.taskId, clientRequestId: item.clientRequestId,
        requestFingerprint: item.requestFingerprint,
      },
      state: 'prepared_not_sent', origin: 'history', taskId: item.taskId,
    }));
  const coveredUnknown = local.filter(item => item.state === 'outcome_unknown' && item.handle).length;
  const remainingUnknown = Math.max(0, Number(history.pendingUnknownCount || 0) - coveredUnknown);
  return local.concat(unattempted, remainingUnknown ? [{
    localSessionId, remoteRef: null, identity: null, text: null, handle: null,
    state: 'outcome_unknown', origin: 'history', taskId: 'history-unknown', count: remainingUnknown,
  }] : []);
}

export function forgetRemoteDispatch(localSessionId, taskId) {
  const cases = casesBySession.get(localSessionId);
  const removed = cases?.delete(taskId) || false;
  if (cases?.size === 0) casesBySession.delete(localSessionId);
  const retired = retiredBySession.get(localSessionId) || new Set();
  retired.add(taskId);
  retiredBySession.set(localSessionId, retired);
  return removed;
}

export function markRemoteDispatchPayloadLost(localSessionId, taskId) {
  const item = casesBySession.get(localSessionId)?.get(taskId);
  if (!item || item.state !== 'prepared_not_sent') return false;
  // Abandon drops the ephemeral payload before persisting retirement. A
  // failed command can leave the durable handle, but same-ID retry is gone.
  item.origin = 'history';
  item.text = null;
  item.remoteRef = null;
  item.identity = null;
  return true;
}

export function recordRemoteDispatch(localSessionId, remoteRef, identity, text, report, staleHistory = null) {
  const handle = validHandle(report?.retryHandle) ? {
    taskId: report.retryHandle.taskId,
    clientRequestId: report.retryHandle.clientRequestId,
    requestFingerprint: report.retryHandle.requestFingerprint,
  } : null;
  const state = report?.state;
  const cases = casesBySession.get(localSessionId) || new Map();
  if (state === 'queued' && handle) {
    forgetRemoteDispatch(localSessionId, handle.taskId);
    return { state: 'queued', handle };
  }
  // An unrecognized success-shaped response is not proof of dispatch. Do not
  // restore it as a fresh draft, which would manufacture a second task ID.
  const safeState = handle && state === 'prepared_not_sent'
    ? 'prepared_not_sent' : 'outcome_unknown';
  const taskId = handle?.taskId || `unverified-${++unverifiedSequence}`;
  cases.set(taskId, {
    localSessionId, remoteRef, identity,
    text: safeState === 'prepared_not_sent' ? text : null,
    handle,
    state: safeState, origin: 'process', taskId, staleHistory,
  });
  casesBySession.set(localSessionId, cases);
  return { state: safeState, handle };
}
