// Projects the authoritative remote journal into ChatView's legacy item shape.
// ChatView stays responsible only for input/output in remote mode; connection,
// capabilities, permissions and artifacts remain owned by remote control.

export function isRemoteChatActive(connection) {
  return !!connection && connection.status === 'connected';
}

export function remoteChatItems(connection, transcript = []) {
  if (!isRemoteChatActive(connection)) return [];
  const sessionId = connection.sessionId ? String(connection.sessionId) : '';
  const entries = (Array.isArray(transcript) ? transcript : []).filter(entry => (
    entry
    && (!sessionId || String(entry.sessionId || '') === sessionId)
  ));

  const items = [];
  for (const entry of entries) {
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
      id: `remote-${key}:${entry.sequence || items.length}`,
      type: entry.role === 'user' ? 'user' : 'assistant',
      text,
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
