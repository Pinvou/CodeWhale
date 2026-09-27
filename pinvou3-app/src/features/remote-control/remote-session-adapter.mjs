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

const MESSAGE_KINDS = Object.freeze({
  user_message: 'user',
  assistant_message: 'assistant',
  reasoning: 'reasoning',
});

export function remoteProxyChatItems(localSessionId, history) {
  const record = remoteProxyForSession(localSessionId, history);
  if (!record) return [];
  return record.entries.flatMap((entry) => {
    if (!entry || typeof entry.journalEpoch !== 'string'
      || !Number.isSafeInteger(entry.sequence) || entry.sequence < 0
      || typeof entry.eventId !== 'string') return [];
    const type = MESSAGE_KINDS[entry.kind] || 'assistant';
    const attachments = Array.isArray(entry.attachments) ? entry.attachments : [];
    const text = (typeof entry.body === 'string' && entry.body)
      || attachments.map(item => item?.displayName || '').filter(Boolean).join('\n');
    if (!text && !attachments.length) return [];
    return [{
      id: `remote-proxy:${localSessionId}:${entry.journalEpoch}:${entry.sequence}:${entry.eventId}`,
      type,
      text,
      remote: true,
      remoteKind: entry.kind,
      remoteTaskId: entry.taskId || null,
      remoteAttachments: attachments,
      streaming: false,
      createdAt: entry.sequence,
    }];
  });
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

// The same complete lease identity scopes model selection, send errors and
// composer recovery. An old async completion cannot write into a new lease.
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

export function sameRemoteComposerOwner(sentSessionId, sentIdentity, currentSessionId, currentIdentity) {
  return !!sentSessionId && sentSessionId === currentSessionId
    && !!sentIdentity && sentIdentity === currentIdentity;
}

export function selectedExportableRemoteModelId(bindingKey, listing, selection) {
  if (!bindingKey || listing?.key !== bindingKey || selection?.key !== bindingKey) return null;
  const modelId = selection.modelId;
  return Array.isArray(listing.models) && listing.models.some(model => (
    model?.exportable === true && model.modelId === modelId
  )) ? modelId : null;
}

export function newRemoteTaskIntent(text, modelSource, selectedModelId) {
  if (modelSource === 'controlled_device') {
    return { kind: 'new', text, modelSource, selectedModelId: null };
  }
  if (modelSource === 'controller_temporary' && selectedModelId) {
    return { kind: 'new', text, modelSource, selectedModelId };
  }
  return null;
}
