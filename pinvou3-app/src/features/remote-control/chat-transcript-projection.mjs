// Projects B's authoritative controlled-session journal into ChatView items.
// A's history comes only from SessionStore.RemoteProxy; this module is not an
// alternative A-side transcript or compatibility read path.

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
        controlledContentLane: 'output',
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
        controlledContentLane: 'error',
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
      controlledContentLane: 'terminal',
      remote: true,
      controlled: true,
      streaming: false,
      createdAt: items.length,
    });
  }
  return items;
}

// The native producer appends exactly one JSON attachment display line to the
// complete prompt. Do not treat arbitrary prefixes or legacy loose filename
// lists as task identity. Kept aligned with the classic bridge state matcher.
function controlledPromptMatches(prompt, display) {
  if (display === prompt) return true;
  const prefix = `${prompt}\n\n📎 `;
  if (!display.startsWith(prefix)) return false;
  const suffix = display.slice(prefix.length);
  try {
    const names = JSON.parse(suffix);
    return Array.isArray(names) && names.length > 0
      && names.every(name => typeof name === 'string' && name.length > 0)
      && JSON.stringify(names) === suffix;
  } catch (_) { return false; }
}

/**
 * Keeps the real, durable session timeline and adds only remote-control items
 * that have not already arrived through the ordinary chat event stream.
 */
export function mergeControlledTaskChatItems(chatItems = [], taskItems = []) {
  const durable = Array.isArray(chatItems) ? chatItems.slice() : [];
  const groups = new Map();
  for (const item of Array.isArray(taskItems) ? taskItems : []) {
    if (!item?.controlledTaskId) continue;
    const taskId = String(item.controlledTaskId);
    if (!groups.has(taskId)) groups.set(taskId, []);
    groups.get(taskId).push(item);
  }
  const insertions = new Map();
  const insert = (index, item) => {
    if (!insertions.has(index)) insertions.set(index, []);
    insertions.get(index).push(item);
  };
  const belongsTo = (item, taskId) => !item?.remoteTaskId
    || String(item.remoteTaskId) === taskId;
  const anchors = new Map();
  let lastAnchor = -1;
  // applyTaskEventTo preserves admission order, including later status updates.
  for (const [taskId, items] of groups) {
    const prompt = items.find(item => item.type === 'user');
    if (!prompt) continue;
    const start = prompt.controlledMessageStart;
    // Prefer identity/position over prompt text: repeated prompts and hidden
    // transcript messages make display-array offsets unsuitable identities.
    let anchor = durable.findIndex(item => item?.type === 'user'
      && String(item.remoteTaskId || '') === taskId);
    if (anchor < 0 && Number.isInteger(start)) {
      anchor = durable.findIndex(item => item?.type === 'user'
        && belongsTo(item, taskId) && item.messageIndex === start);
    }
    if (anchor < 0) {
      anchor = durable.findIndex((item, index) => index > lastAnchor
        && item?.type === 'user' && !item.steeredMidTurn
        && belongsTo(item, taskId) && controlledPromptMatches(prompt.text, String(item.text || ''))
        && (!Number.isInteger(start) || !Number.isInteger(item.messageIndex)
          || item.messageIndex === start));
    }
    anchors.set(taskId, anchor);
    if (anchor >= 0) lastAnchor = anchor;
  }
  const taskIds = [...groups.keys()];
  for (const [taskId, items] of groups) {
    const prompt = items.find(item => item.type === 'user');
    if (!prompt) continue;
    const start = prompt.controlledMessageStart;
    const anchor = anchors.get(taskId) ?? -1;
    let lower = anchor + 1;
    if (anchor < 0) {
      const firstEvent = durable.findIndex(item => String(item?.remoteTaskId || '') === taskId);
      const nextPosition = Number.isInteger(start) ? durable.findIndex(item => (
        item?.type === 'user' && Number.isInteger(item.messageIndex) && item.messageIndex >= start
      )) : -1;
      const nextTaskId = taskIds.slice(taskIds.indexOf(taskId) + 1)
        .find(id => anchors.get(id) >= 0);
      lower = firstEvent >= 0 ? firstEvent : (nextPosition >= 0
        ? nextPosition : (nextTaskId ? anchors.get(nextTaskId) : durable.length));
      insert(lower, prompt);
    }
    const nextTurn = durable.findIndex((item, index) => index >= lower && (
      (item?.remoteTaskId && String(item.remoteTaskId) !== taskId)
      || (item?.type === 'user' && !item.steeredMidTurn
        && String(item.remoteTaskId || '') !== taskId)
    ));
    const upper = nextTurn < 0 ? durable.length : nextTurn;
    const candidates = [];
    for (let index = lower; index < upper; index += 1) {
      if (durable[index]?.type === 'assistant' && belongsTo(durable[index], taskId)) {
        candidates.push(index);
      }
    }
    // Reserve persisted failures before joining the output lane. A terminal
    // error following a partially hydrated answer is not model text.
    const failureIndices = new Set();
    const output = items.find(item => item.controlledContentLane === 'output');
    const modelIndices = new Set();
    let modelPrefix = '';
    if (output) {
      for (const index of candidates) {
        const next = modelPrefix + String(durable[index].text || '');
        if (!output.text.startsWith(next) && !next.startsWith(output.text)) break;
        modelPrefix = next;
        modelIndices.add(index);
        // A prefix can end in the middle of a newer durable message. Protect
        // the whole message, but leave later messages available for failures.
        if (modelPrefix.length >= output.text.length) break;
      }
    }
    for (const item of items.filter(candidate => candidate.controlledContentLane !== 'output'
      && candidate.type === 'assistant')) {
      for (let position = candidates.length - 1; position >= 0; position -= 1) {
        const index = candidates[position];
        if (modelIndices.has(index) || failureIndices.has(index)) continue;
        if (String(durable[index].text || '') === item.text
          || (item.controlledFailureReason
            && String(durable[index].text || '') === item.controlledFailureReason)) {
          failureIndices.add(index);
          break;
        }
      }
    }
    const outputBoundary = failureIndices.size ? Math.min(...failureIndices) : upper;
    const used = new Set();
    for (const item of items) {
      if (item.type === 'user') continue;
      if (item.controlledContentLane === 'output') {
        // The task lane accumulates every text delta, whereas the ordinary
        // timeline splits text at tools/reasoning. Compare the ordered lane
        // inside this turn only; never search substrings across other turns.
        const outputIndices = candidates.filter(index => !failureIndices.has(index));
        const text = outputIndices.map(index => String(durable[index].text || '')).join('');
        if (text.startsWith(item.text)) {
          outputIndices.forEach(index => used.add(index));
          continue; // durable lane is equal or ahead
        }
        if (item.text.startsWith(text)) {
          outputIndices.forEach(index => used.add(index));
          const suffix = item.text.slice(text.length);
          if (suffix) insert(outputBoundary, { ...item, text: suffix });
          continue;
        }
      }
      const match = candidates.find(index => !used.has(index) && (
        String(durable[index].text || '') === item.text
        || (item.controlledFailureReason
          && String(durable[index].text || '') === item.controlledFailureReason)
      ));
      if (match !== undefined) {
        used.add(match);
        if (String(durable[match].text || '') !== item.text) {
          // Only enrich the failure label; task-side metadata is not durable
          // message metadata, and must not replace its identity or timestamps.
          durable[match] = { ...durable[match], text: item.text };
        }
      } else {
        insert(item.controlledContentLane === 'output' ? outputBoundary : upper, item);
      }
    }
  }
  // Keep original indices stable while reconciling multiple tasks. Missing
  // output belongs before the next admission, not at the conversation bottom.
  const merged = [];
  for (let index = 0; index <= durable.length; index += 1) {
    merged.push(...(insertions.get(index) || []));
    if (index < durable.length) merged.push(durable[index]);
  }
  return merged;
}
