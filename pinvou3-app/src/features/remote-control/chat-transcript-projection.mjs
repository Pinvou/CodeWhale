// Projects the authoritative remote journal into ChatView's legacy item shape.
// ChatView stays responsible only for input/output in remote mode; connection,
// capabilities, permissions and artifacts remain owned by remote control.

export function isRemoteChatActive(connection) {
  return !!connection && connection.status === 'connected';
}

export function remoteChatItems(connection, transcript = [], tasks = []) {
  if (!isRemoteChatActive(connection)) return [];
  const sessionId = connection.sessionId ? String(connection.sessionId) : '';
  const entries = (Array.isArray(transcript) ? transcript : []).filter(entry => (
    entry
    && (!sessionId || String(entry.sessionId || '') === sessionId)
  ));
  const journaledTaskIds = new Set(entries
    .filter(entry => entry.role === 'user' && entry.taskId)
    .map(entry => String(entry.taskId)));
  const promptFallbacks = (Array.isArray(tasks) ? tasks : [])
    .filter(task => (
      task
      && task.taskId
      && task.text
      && !journaledTaskIds.has(String(task.taskId))
      && (!sessionId || !task.sessionId || String(task.sessionId) === sessionId)
    ))
    .map((task, index) => ({
      sessionId: String(task.sessionId || sessionId || ''),
      epoch: 'task-intent',
      sequence: index + 1,
      taskId: String(task.taskId),
      role: 'user',
      text: String(task.text),
    }));

  const items = [];
  for (const entry of entries.concat(promptFallbacks)) {
    const key = `${entry.sessionId || ''}:${entry.epoch || ''}`;
    const text = String(entry.text || '');
    if (!text) continue;
    if (
      entry.role === 'assistant'
      && items.length
      && items[items.length - 1].type === 'assistant'
      && items[items.length - 1].remoteKey === key
    ) {
      // assistant_delta entries are ordered chunks of one remote turn.
      const item = items[items.length - 1];
      item.text += text;
      item.streaming = true;
      continue;
    }
    items.push({
      id: entry.taskId && entry.epoch === 'task-intent'
        ? `remote-task-${entry.taskId}`
        : `remote-${key}:${entry.sequence || items.length}`,
      type: entry.role === 'user' ? 'user' : 'assistant',
      text,
      remoteTaskId: entry.taskId ? String(entry.taskId) : null,
      remote: true,
      remoteKey: key,
      streaming: entry.role === 'assistant',
      createdAt: Number(entry.sequence || 0),
    });
  }

  const last = items[items.length - 1];
  if (last && last.type === 'assistant') last.streaming = false;
  return items;
}

const CONTROLLED_TERMINAL = new Set([
  'succeeded',
  'failed',
  'cancelled',
  'interrupted',
  'result_unknown',
]);

const CONTROLLED_FAILURE = new Set([
  'failed',
  'cancelled',
  'interrupted',
  'result_unknown',
]);

const STATUS_COPY_KEYS = {
  succeeded: 'statusSucceeded',
  failed: 'statusFailed',
  cancelled: 'statusCancelled',
  interrupted: 'statusInterrupted',
  result_unknown: 'statusResultUnknown',
};

/**
 * B-side projection of accepted remote tasks. The accepted prompt is a user
 * message even when execution never starts; terminal failures are assistant
 * messages so they remain in the conversation instead of disappearing with
 * the running-task banner.
 */
export function controlledTaskChatItems(tasks = [], copy = {}, sessionId = '') {
  const items = [];
  for (const task of Array.isArray(tasks) ? tasks : []) {
    if (!task || !task.taskId || !task.text) continue;
    if (!sessionId || String(task.sessionId || '') !== String(sessionId)) continue;
    const taskId = String(task.taskId);
    items.push({
      id: `controlled-${taskId}:user`,
      type: 'user',
      text: String(task.text),
      controlledTaskId: taskId,
      controlledMessageStart: Number.isInteger(task.messageIndex) ? task.messageIndex : null,
      remote: true,
      controlled: true,
      createdAt: items.length,
    });

    const assistantText = String(task.assistantText || '');
    if (assistantText) {
      items.push({
        id: `controlled-${taskId}:assistant`,
        type: 'assistant',
        text: assistantText,
        controlledTaskId: taskId,
        remote: true,
        controlled: true,
        streaming: !CONTROLLED_TERMINAL.has(task.status),
        createdAt: items.length,
      });
    }

    const errorText = String(task.errorText || '');
    if (errorText) {
      items.push({
        id: `controlled-${taskId}:error`,
        type: 'assistant',
        text: errorText,
        controlledTaskId: taskId,
        remote: true,
        controlled: true,
        streaming: false,
        createdAt: items.length,
      });
    }

    if (!CONTROLLED_FAILURE.has(task.status) || errorText) continue;
    const status = String(task.status);
    const label = String(copy[STATUS_COPY_KEYS[status]] || status);
    const reason = String(task.terminalReason || '').trim();
    items.push({
      id: `controlled-${taskId}:terminal`,
      type: 'assistant',
      text: reason && reason !== status ? `${label}: ${reason}` : label,
      controlledTaskId: taskId,
      controlledFailureReason: reason || status,
      remote: true,
      controlled: true,
      streaming: false,
      createdAt: items.length,
    });
  }
  return items;
}

/**
 * Keeps the real, durable session timeline and adds only remote-control items
 * that have not already arrived through the ordinary chat event stream.
 */
export function mergeControlledTaskChatItems(chatItems = [], taskItems = []) {
  const merged = Array.isArray(chatItems) ? chatItems.slice() : [];
  const available = new Map();
  for (let index = 0; index < merged.length; index += 1) {
    const item = merged[index];
    if (!item || (item.type !== 'user' && item.type !== 'assistant')) continue;
    const key = `${item.type}:${String(item.text || '')}`;
    if (!available.has(key)) available.set(key, []);
    available.get(key).push(index);
  }
  const taskAnchors = new Map();
  let lastTaskUserIndex = -1;
  for (const item of Array.isArray(taskItems) ? taskItems : []) {
    const key = `${item.type}:${String(item.text || '')}`;
    const taskId = String(item.controlledTaskId || '');
    const anchor = taskAnchors.get(taskId) ?? lastTaskUserIndex;
    const nextUserIndex = merged.findIndex((candidate, index) => (
      index > anchor && candidate?.type === 'user'
    ));
    const upperBound = item.type === 'user' || nextUserIndex < 0
      ? Number.POSITIVE_INFINITY
      : nextUserIndex;
    const persistedFailureKey = item.controlledFailureReason
      ? `assistant:${String(item.controlledFailureReason)}`
      : '';
    const eligibleIndex = (candidateKey) => {
      const indices = available.get(candidateKey) || [];
      let position = -1;
      if (taskId) {
        position = indices.findIndex(index => (
          String(merged[index]?.remoteTaskId || '') === taskId
        ));
      }
      if (item.type === 'user' && Number.isInteger(item.controlledMessageStart)) {
        if (position < 0) {
          position = indices.findIndex(index => (
            index >= item.controlledMessageStart
            && Number(merged[index]?.messageIndex) === item.controlledMessageStart
          ));
        }
      }
      if (position < 0) {
        position = indices.findIndex((index) => {
          if (index <= anchor || index >= upperBound) return false;
          if (item.type !== 'user' || !Number.isInteger(item.controlledMessageStart)) return true;
          const candidateMessageIndex = Number(merged[index]?.messageIndex);
          return Number.isInteger(candidateMessageIndex)
            ? candidateMessageIndex >= item.controlledMessageStart
            : index >= item.controlledMessageStart;
        });
      }
      if (position < 0) return -1;
      return indices.splice(position, 1)[0];
    };
    let matchingKey = key;
    let index = eligibleIndex(key);
    if (index < 0 && persistedFailureKey) {
      matchingKey = persistedFailureKey;
      index = eligibleIndex(persistedFailureKey);
    }
    if (index >= 0) {
      if (matchingKey !== key) {
        merged[index] = { ...merged[index], ...item, id: merged[index].id };
      }
    } else {
      merged.push(item);
      index = merged.length - 1;
    }
    if (item.type === 'user' && taskId) {
      taskAnchors.set(taskId, index);
      lastTaskUserIndex = index;
    }
  }
  return merged;
}
