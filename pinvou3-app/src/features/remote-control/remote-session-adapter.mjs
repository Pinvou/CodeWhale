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
  approval: 'system',
  error: 'system',
  attachment: 'system',
  artifact: 'system',
});

const REMOTE_TOOL_DISPLAY_SCHEMA = 'remote_tool_display/v1';
const TERMINAL_TASK_STATES = new Set(['succeeded', 'failed', 'cancelled', 'interrupted', 'result_unknown']);
const TASK_ACTIVITY_KINDS = new Set([
  'user_message', 'assistant_message', 'reasoning', 'tool', 'approval', 'error',
  'attachment', 'artifact',
]);

function remoteToolState(value) {
  const state = String(value || '').toLowerCase();
  if (['completed', 'complete', 'done', 'success', 'succeeded'].includes(state)) return 'done';
  if (['failed', 'failure', 'error'].includes(state)) return 'failed';
  return 'running';
}

function remoteToolDisplay(body) {
  if (typeof body !== 'string' || !body.trim().startsWith('{')) return null;
  try {
    const value = JSON.parse(body);
    const schema = value?.schema;
    const toolId = value?.toolId ?? value?.tool_id;
    const toolName = value?.toolName ?? value?.tool_name;
    const toolState = value?.toolState ?? value?.tool_state;
    if (schema != null && schema !== REMOTE_TOOL_DISPLAY_SCHEMA) return null;
    if (typeof toolId !== 'string' || !toolId
      || typeof toolName !== 'string' || !toolName
      || typeof toolState !== 'string' || !toolState) return null;
    const output = value?.output ?? value?.text;
    return {
      toolId,
      toolName,
      state: remoteToolState(toolState),
      output: typeof output === 'string' ? output : null,
    };
  } catch {
    return null;
  }
}

function remoteTaskProjectionState(entries) {
  const tasks = new Map();
  for (let index = 0; index < entries.length; index += 1) {
    const entry = entries[index];
    if (!entry || typeof entry.journalEpoch !== 'string'
      || !Number.isSafeInteger(entry.sequence) || entry.sequence < 0
      || typeof entry.eventId !== 'string') continue;
    const taskId = typeof entry?.taskId === 'string' && entry.taskId ? entry.taskId : null;
    if (!taskId) continue;
    const key = remoteTaskMapKey(entry.journalEpoch, taskId);
    const task = tasks.get(key) || {
      taskId,
      journalEpoch: entry.journalEpoch,
      state: 'running',
      lastActivity: index,
    };
    if (entry.kind === 'status') {
      task.state = String(entry.body || '').toLowerCase();
      task.lastActivity = index;
      tasks.set(key, task);
    } else if (TASK_ACTIVITY_KINDS.has(entry.kind)) {
      task.lastActivity = index;
      tasks.set(key, task);
    }
  }
  return tasks;
}

function remoteTaskMapKey(journalEpoch, taskId) {
  return JSON.stringify([journalEpoch, taskId]);
}

function remoteToolMapKey(journalEpoch, taskId, toolId) {
  return JSON.stringify([journalEpoch, taskId, toolId]);
}

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

// Poll only on relevant journal changes, never on streamed message identity.
export function remoteProjectionPollRevision(history, kinds) {
  return JSON.stringify((Array.isArray(history?.entries) ? history.entries : [])
    .filter(entry => kinds.includes(entry?.kind))
    .map(entry => [entry.journalEpoch, entry.sequence, entry.eventId, entry.taskId, entry.body]));
}

export function remoteProjectionLabelKey(kind) {
  return PROJECTION_LABEL_KEYS[kind] || 'projectionEvent';
}

export function remoteProxyChatItems(localSessionId, history) {
  const record = remoteProxyForSession(localSessionId, history);
  if (!record) return [];
  const tasks = remoteTaskProjectionState(record.entries);
  const items = [];
  const tools = new Map();
  for (const entry of record.entries) {
    if (!entry || typeof entry.journalEpoch !== 'string'
      || !Number.isSafeInteger(entry.sequence) || entry.sequence < 0
      || typeof entry.eventId !== 'string') continue;
    const attachments = Array.isArray(entry.attachments) ? entry.attachments : [];
    const taskId = typeof entry.taskId === 'string' && entry.taskId ? entry.taskId : null;
    if (entry.kind === 'tool') {
      const display = remoteToolDisplay(entry.body);
      if (!display) continue;
      const key = remoteToolMapKey(entry.journalEpoch, taskId, display.toolId);
      const previousTool = tools.get(key);
      if (previousTool) {
        previousTool.name = display.toolName;
        previousTool.state = display.state;
        previousTool.success = display.state === 'done' ? true
          : (display.state === 'failed' ? false : undefined);
        if (display.output !== null) previousTool.output = display.output;
        continue;
      }
      const tool = {
        id: `remote-proxy:${localSessionId}:${entry.journalEpoch}:${entry.sequence}:${entry.eventId}`,
        type: 'tool',
        name: display.toolName,
        args: null,
        output: display.output,
        state: display.state,
        success: display.state === 'done' ? true : (display.state === 'failed' ? false : undefined),
        remote: true,
        remoteProxy: true,
        remoteKind: entry.kind,
        remoteToolId: display.toolId,
        remoteTaskId: taskId,
        remoteJournalEpoch: entry.journalEpoch,
        remoteAttachments: attachments.slice(),
        createdAt: entry.sequence,
      };
      tools.set(key, tool);
      items.push(tool);
      continue;
    }
    // Task lifecycle and snapshot markers drive projection state, not visible
    // transcript rows. Showing each one as a message produced raw protocol
    // blocks and split one remote turn into multiple visual fragments.
    if (entry.kind === 'status' || entry.kind === 'snapshot_marker') continue;
    const type = PROJECTION_TYPES[entry.kind];
    if (!type) continue;
    const text = (typeof entry.body === 'string' && entry.body)
      || attachments.map(item => item?.displayName || '').filter(Boolean).join('\n');
    if (!text && !attachments.length && type !== 'system') continue;
    const taskState = taskId
      ? tasks.get(remoteTaskMapKey(entry.journalEpoch, taskId))?.state
      : null;
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
      && 'text' in previous
      && 'streaming' in previous
    ) {
      // The journal stores streamed deltas as independent durable events.
      // Coalesce only the uninterrupted lane of one authenticated task: a
      // reasoning, tool, approval or error event remains an ordering boundary
      // and therefore starts a new bubble after it. Hidden lifecycle entries
      // do not create a visual boundary.
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
  // Old persisted history may contain a structured tool start followed by a
  // raw legacy tool end that cannot be safely correlated. A terminal task
  // proves the tool is no longer running, but not whether that tool succeeded
  // or failed. The native ToolCard has no neutral terminal state, so hide the
  // unrecoverable card instead of leaking its raw body or inventing an outcome.
  const hiddenLegacyTools = new Set();
  for (const tool of tools.values()) {
    if (tool.state !== 'running') continue;
    const taskState = tool.remoteTaskId
      ? tasks.get(remoteTaskMapKey(tool.remoteJournalEpoch, tool.remoteTaskId))?.state
      : null;
    if (!TERMINAL_TASK_STATES.has(taskState)) continue;
    hiddenLegacyTools.add(tool);
  }
  return hiddenLegacyTools.size
    ? items.filter(item => !hiddenLegacyTools.has(item))
    : items;
}

export function activeRemoteTaskId(localSessionId, history) {
  const record = remoteProxyForSession(localSessionId, history);
  if (!record) return null;
  const tasks = remoteTaskProjectionState(record.entries);
  let activeTaskId = null;
  let activeIndex = -1;
  for (const task of tasks.values()) {
    if (!TERMINAL_TASK_STATES.has(task.state) && task.lastActivity >= activeIndex) {
      activeTaskId = task.taskId;
      activeIndex = task.lastActivity;
    }
  }
  return activeTaskId;
}

export function remoteProxyHandshakeBinding(localSessionId, history, connection) {
  if (history?.archived === true || !remoteProxyForSession(localSessionId, history)
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

export function activeLiveRemoteTaskId(localSessionId, history, connection) {
  return remoteProxyHandshakeBinding(localSessionId, history, connection)
    ? activeRemoteTaskId(localSessionId, history)
    : null;
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

// Drafts share the stable lease/session ownership of model selection. This
// identity must never replace the revision-bound dispatch identity above.
export function remoteProxyComposerIdentity(localSessionId, connection) {
  return remoteProxyModelSelectionIdentity(localSessionId, connection);
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
