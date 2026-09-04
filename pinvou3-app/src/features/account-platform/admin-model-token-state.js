export function adminModelTokenOperationSources(permissions = []) {
  const granted = new Set(permissions);
  return [
    granted.has('platform.tokens.read') ? 'provisionings' : '',
    granted.has('platform.usage.read') ? 'usage' : '',
    granted.has('platform.usage.read') ? 'reconciliation' : '',
  ].filter(Boolean);
}

export function planAdminModelTokenOperationRequests(permissions = [], client) {
  return adminModelTokenOperationSources(permissions).map((source) => ({
    source,
    load: source === 'provisionings'
      ? (query, signal) => client.provisionings(query, signal)
      : source === 'usage'
        ? (query, signal) => client.usageRequests(query, signal)
        : (query, signal) => client.tokenReconciliationCases(query, signal),
  }));
}

export function initialAdminSourceState() {
  return { items: [], loading: false, error: null, page: null, updatedAt: null, queryKey: '', requestID: 0 };
}

export function visibleAdminSourceState(state, queryKey, enabled) {
  if (state.queryKey === queryKey) return state;
  return {
    ...state,
    items: [],
    page: null,
    updatedAt: null,
    loading: Boolean(enabled),
    error: null,
    queryKey,
  };
}

export function reduceAdminSourceState(state, event) {
  if (event.type === 'start') {
    const sameQuery = state.queryKey === event.queryKey;
    return {
      ...state,
      items: sameQuery ? state.items : [],
      page: sameQuery ? state.page : null,
      updatedAt: sameQuery ? state.updatedAt : null,
      queryKey: event.queryKey,
      requestID: event.requestID,
      loading: true,
      error: null,
    };
  }
  if (event.requestID !== state.requestID || event.queryKey !== state.queryKey) return state;
  if (event.type === 'success') {
    const payload = event.payload || {};
    return {
      ...state,
      items: Array.isArray(payload.items) ? payload.items : [],
      loading: false,
      error: null,
      page: {
        nextCursor: typeof payload.next_cursor === 'string' ? payload.next_cursor : '',
        hasMore: Boolean(payload.has_more),
        limit: Number(payload.limit || 50),
        asOf: payload.as_of || null,
        expiresAt: payload.snapshot_expires_at || null,
      },
      updatedAt: payload.as_of || event.completedAt,
    };
  }
  if (event.type === 'failure') return { ...state, loading: false, error: event.error };
  return state;
}

export async function executeAdminSourceRequest({ requestID, queryKey, query, load, signal, transition, now = () => new Date().toISOString() }) {
  transition({ type: 'start', requestID, queryKey });
  try {
    const payload = await load(query, signal);
    if (signal?.aborted) return;
    transition({ type: 'success', requestID, queryKey, payload, completedAt: now() });
  } catch (error) {
    if (signal?.aborted || error?.name === 'AbortError') return;
    transition({ type: 'failure', requestID, queryKey, error });
  }
}
