// Projects the authoritative remote journal into ChatView's legacy item shape.
// ChatView stays responsible only for input/output in remote mode; connection,
// capabilities, permissions and artifacts remain owned by remote control.

export function isRemoteChatActive(connection) {
  return !!connection && connection.status === 'connected';
}

const CONTROLLED_TERMINAL = new Set([
  'succeeded',
  'failed',
  'cancelled',
  'interrupted',
  'result_unknown',
]);

export function remoteChatItems(connection, transcript = [], tasks = []) {
  if (!isRemoteChatActive(connection)) return [];
  const sessionId = connection.sessionId ? String(connection.sessionId) : '';
  const entries = (Array.isArray(transcript) ? transcript : []).filter(entry => (
    entry
    && (!sessionId || String(entry.sessionId || '') === sessionId)
  ));
  const sessionTasks = (Array.isArray(tasks) ? tasks : []).filter(task => (
    task
    && task.taskId
    && (!sessionId || !task.sessionId || String(task.sessionId) === sessionId)
  ));
  const runningTaskIds = new Set(sessionTasks
    .filter(task => !CONTROLLED_TERMINAL.has(String(task.status || '')))
    .map(task => String(task.taskId)));
  const journaledTaskIds = new Set(entries
    .filter(entry => entry.role === 'user' && entry.taskId)
    .map(entry => String(entry.taskId)));
  const promptFallbacks = sessionTasks
    .filter(task => (
      task.text
      && !journaledTaskIds.has(String(task.taskId))
    ))
    .map(task => ({
      sessionId: String(task.sessionId || sessionId || ''),
      epoch: 'task-intent',
      sequence: 0,
      taskId: String(task.taskId),
      role: 'user',
      text: String(task.text),
    }));

  // A status acknowledgement can beat the authoritative user journal. Insert
  // that fallback next to the same task's first event, never at the end of the
  // whole transcript where it would split a later turn from its answer.
  const orderedEntries = entries.slice();
  for (const fallback of promptFallbacks) {
    const taskIndex = orderedEntries.findIndex(entry => (
      String(entry?.taskId || '') === fallback.taskId
    ));
    if (taskIndex >= 0) {
      orderedEntries.splice(taskIndex, 0, fallback);
    } else {
      orderedEntries.push(fallback);
    }
  }

  const items = [];
  for (const entry of orderedEntries) {
    const taskId = entry.taskId ? String(entry.taskId) : '';
    const type = entry.role === 'user'
      ? 'user'
      : (entry.role === 'reasoning' ? 'reasoning' : 'assistant');
    const key = `${entry.sessionId || ''}:${entry.epoch || ''}:${taskId}:${type}`;
    const text = String(entry.text || '');
    if (!text) continue;
    if (
      type !== 'user'
      && items.length
      && items[items.length - 1].type === type
      && items[items.length - 1].remoteKey === key
    ) {
      // Text/reasoning deltas merge only inside the same task and content
      // lane. Epoch alone is not a turn identity and previously joined two
      // consecutive answers together.
      const item = items[items.length - 1];
      item.text += text;
      item.streaming = !!taskId && runningTaskIds.has(taskId);
      continue;
    }
    items.push({
      id: entry.taskId && entry.epoch === 'task-intent'
        ? `remote-task-${entry.taskId}`
        : `remote-${key}:${entry.sequence || items.length}`,
      type,
      text,
      remoteTaskId: taskId || null,
      remote: true,
      remoteKey: key,
      streaming: type !== 'user' && !!taskId && runningTaskIds.has(taskId),
      createdAt: Number(entry.sequence || 0),
    });
  }
  return items;
}

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
