// RemoteProxy is a SessionStore session kind, not another conversation store.
// This adapter only shapes an already allowlisted, Rust-verified history read
// for the existing ChatView. In particular, a `remote-` prefix alone is never
// proof that a local session is a RemoteProxy.

export function remoteProxyForSession(localSessionId, history) {
  if (typeof localSessionId !== 'string' || !localSessionId
    || !history || history.localSessionId !== localSessionId
    || !Array.isArray(history.entries)) return null;
  return history;
}

export function mergeSessionStoreHistory(localSessions, remoteProxies) {
  const result = (Array.isArray(localSessions) ? localSessions : [])
    .filter(item => item && typeof item.id === 'string')
    .map(item => ({ ...item, kind: item.kind || 'Local' }));
  const seen = new Set(result.map(item => item.id));
  for (const record of Array.isArray(remoteProxies) ? remoteProxies : []) {
    if (!record || typeof record.localSessionId !== 'string' || !record.localSessionId
      || seen.has(record.localSessionId)) continue;
    seen.add(record.localSessionId);
    result.push({
      id: record.localSessionId,
      kind: 'RemoteProxy',
      title: record.title || '',
      pinned: record.pinned === true,
      updated_at: record.lastOpenedAt || null,
      created_at: record.lastOpenedAt || null,
      projectedEntryCount: record.projectedEntryCount || 0,
    });
  }
  return result;
}

const PROJECTION_TYPES = Object.freeze({
  user_message: 'user',
  assistant_message: 'assistant',
  reasoning: 'reasoning',
  tool: 'system',
  approval: 'system',
  status: 'system',
  error: 'system',
  attachment: 'system',
  artifact: 'system',
  snapshot_marker: 'system',
});

const PROJECTION_LABEL_KEYS = Object.freeze({
  user_message: 'projectionUser',
  assistant_message: 'projectionAssistant',
  tool: 'projectionTool',
  approval: 'projectionApproval',
  status: 'projectionStatus',
  error: 'projectionError',
  attachment: 'projectionAttachment',
  artifact: 'projectionArtifact',
  snapshot_marker: 'projectionSnapshot',
});

export function remoteProjectionLabelKey(kind) {
  return PROJECTION_LABEL_KEYS[kind] || 'projectionEvent';
}

export function remoteProxyChatItems(localSessionId, history) {
  const record = remoteProxyForSession(localSessionId, history);
  if (!record) return [];
  const latestTaskStates = new Map();
  for (const entry of record.entries) {
    if (entry?.kind === 'status' && typeof entry.taskId === 'string' && entry.taskId) {
      latestTaskStates.set(entry.taskId, String(entry.body || ''));
    }
  }
  const items = [];
  for (const entry of record.entries) {
    if (!entry || typeof entry.journalEpoch !== 'string'
      || !Number.isSafeInteger(entry.sequence) || entry.sequence < 0
      || typeof entry.eventId !== 'string') continue;
    const type = PROJECTION_TYPES[entry.kind];
    if (!type) continue;
    const attachments = Array.isArray(entry.attachments) ? entry.attachments : [];
    const text = (typeof entry.body === 'string' && entry.body)
      || attachments.map(item => item?.displayName || '').filter(Boolean).join('\n');
    if (!text && !attachments.length && type !== 'system') continue;
    const taskId = typeof entry.taskId === 'string' && entry.taskId ? entry.taskId : null;
    const taskState = taskId ? latestTaskStates.get(taskId) : null;
    const streaming = (type === 'assistant' || type === 'reasoning')
      && !!taskId && !TERMINAL_TASK_STATES.has(taskState);
    const previous = items[items.length - 1];
    if (
      taskId
      && (type === 'assistant' || type === 'reasoning')
      && previous?.type === type
      && previous.remoteKind === entry.kind
      && previous.remoteTaskId === taskId
      && previous.remoteJournalEpoch === entry.journalEpoch
    ) {
      // The journal stores streamed deltas as independent durable events.
      // Coalesce only the uninterrupted lane of one authenticated task: a
      // reasoning, tool, status, approval or error event remains an ordering
      // boundary and therefore starts a new bubble after it.
      previous.text += text;
      previous.remoteAttachments.push(...attachments);
      previous.streaming = streaming;
      continue;
    }
    items.push({
      id: `remote-proxy:${localSessionId}:${entry.journalEpoch}:${entry.sequence}:${entry.eventId}`,
      type,
      text,
      remote: true,
      remoteProxy: true,
      remoteKind: entry.kind,
      remoteTaskId: taskId,
      remoteJournalEpoch: entry.journalEpoch,
      remoteAttachments: attachments.slice(),
      streaming,
      createdAt: entry.sequence,
    });
  }
  return items;
}

const TERMINAL_TASK_STATES = new Set(['succeeded', 'failed', 'cancelled', 'interrupted', 'result_unknown']);

export function activeRemoteTaskId(localSessionId, history) {
  const record = remoteProxyForSession(localSessionId, history);
  if (!record) return null;
  const states = new Map();
  for (const entry of record.entries) {
    if (entry?.kind === 'status' && typeof entry.taskId === 'string' && entry.taskId) {
      states.set(entry.taskId, String(entry.body || ''));
    }
  }
  return [...states].reverse().find(([, state]) => !TERMINAL_TASK_STATES.has(state))?.[0] || null;
}

export function remoteProxyHandshakeBinding(localSessionId, history, connection) {
  if (!remoteProxyForSession(localSessionId, history)
    || !connection || connection.status !== 'connected'
    || !connection.sessionRef
    || connection.sessionMode !== 'chat'
    || !connection.connectionId || !connection.controllerLeaseId
    || !connection.leaseEpoch || !connection.relayEpoch || !connection.generation) return null;
  // The WebView never infers a backend binding from connection status or a
  // remote ref alone. Dispatch still performs the authoritative Rust check.
  if (connection.localSessionId !== localSessionId) return null;
  return { localSessionId, remoteRef: connection.sessionRef };
}

export function liveRemoteProxyBinding(localSessionId, history, connection) {
  if (connection?.mappingConfirmed !== true || connection?.capabilityRevision == null
    || connection?.capabilityStatus !== 'ready') return null;
  return remoteProxyHandshakeBinding(localSessionId, history, connection);
}

export function remoteProxyPresentationState(localSessionId, history, connection) {
  if (!remoteProxyForSession(localSessionId, history)) return 'absent';
  if (!connection || connection.localSessionId !== localSessionId) return 'offline';
  if (connection.status === 'reconnecting') return 'reconnecting';
  if (!remoteProxyHandshakeBinding(localSessionId, history, connection)) return 'offline';
  if (connection.capabilityStatus === 'error') return 'capability_error';
  return liveRemoteProxyBinding(localSessionId, history, connection) ? 'ready' : 'preparing';
}

// Dispatch state includes the exact capability revision. An old async
// completion therefore cannot write into a renewed snapshot or lease.
export function remoteProxyLeaseIdentity(localSessionId, connection) {
  if (!localSessionId) return '';
  return JSON.stringify([
    localSessionId,
    connection?.sessionRef || '',
    connection?.connectionId || '',
    connection?.controllerLeaseId || '',
    connection?.leaseEpoch || '',
    connection?.relayEpoch || '',
    connection?.generation || '',
    connection?.capabilityRevision ?? null,
  ]);
}

// A model choice belongs to the authenticated connection/lease, not to one
// short-lived capability snapshot. Snapshot renewal must preserve both an
// explicit model and an explicit empty selection.
/**
 * @param {string | null | undefined} localSessionId
 * @param {{sessionRef?: string, connectionId?: string, controllerLeaseId?: string,
 *   leaseEpoch?: string | number, relayEpoch?: string, generation?: string} | null | undefined} connection
 */
export function remoteProxyModelSelectionIdentity(localSessionId, connection) {
  if (!localSessionId) return '';
  return JSON.stringify([
    localSessionId,
    connection?.sessionRef || '',
    connection?.connectionId || '',
    connection?.controllerLeaseId || '',
    connection?.leaseEpoch || '',
    connection?.relayEpoch || '',
    connection?.generation || '',
  ]);
}

export function sameRemoteComposerOwner(sentSessionId, sentIdentity, currentSessionId, currentIdentity) {
  return !!sentSessionId && sentSessionId === currentSessionId
    && !!sentIdentity && sentIdentity === currentIdentity;
}

export function selectedExportableRemoteModelId(bindingKey, listing, selection, defaultModelId = null) {
  if (!bindingKey || listing?.key !== bindingKey) return null;
  const modelId = selection?.key === bindingKey ? selection.modelId : defaultModelId;
  return Array.isArray(listing.models) && listing.models.some(model => (
    model?.exportable === true && model.modelId === modelId
  )) ? modelId : null;
}

export function newRemoteTaskIntent(
  text,
  modelSource,
  selectedModelId,
  mode = 'chat',
  workspaceId = null,
  skillIds = [],
) {
  if (mode !== 'chat' && mode !== 'code') return null;
  if (mode === 'chat' && workspaceId) return null;
  if (mode === 'code' && !workspaceId) return null;
  const selectedSkills = Array.isArray(skillIds)
    ? [...new Set(skillIds.filter(id => typeof id === 'string' && id))].slice(0, 32)
    : [];
  if (modelSource === 'controlled_device') {
    return {
      kind: 'new', text, mode, workspaceId: mode === 'code' ? workspaceId : null,
      skillIds: selectedSkills, modelSource, selectedModelId: null,
    };
  }
  if (mode === 'chat' && modelSource === 'controller_temporary' && selectedModelId) {
    return {
      kind: 'new', text, mode, workspaceId: null, skillIds: selectedSkills,
      modelSource, selectedModelId,
    };
  }
  return null;
}
