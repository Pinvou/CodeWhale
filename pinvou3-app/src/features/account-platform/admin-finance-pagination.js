const SNAPSHOT_EXPIRED_CODES = new Set(['PAGINATION_SNAPSHOT_EXPIRED', 'PAGINATION_CURSOR_EXPIRED']);

export function normalizeFinanceCollection(result, query = {}) {
  const payload = result && typeof result === 'object' ? result : {};
  const hasSnapshotContract = Object.prototype.hasOwnProperty.call(payload, 'next_cursor') && Object.prototype.hasOwnProperty.call(payload, 'snapshot_as_of');
  return {
    items: Array.isArray(payload.items) ? payload.items : [],
    page: {
      cursor: String(query.cursor || ''),
      nextCursor: typeof payload.next_cursor === 'string' ? payload.next_cursor : '',
      snapshotAsOf: typeof payload.snapshot_as_of === 'string' ? payload.snapshot_as_of : '',
      legacy: !hasSnapshotContract,
      expired: false,
    },
  };
}

export function financeCursorContext(tab, query = {}, scope = '') {
  const filters = Object.entries(query)
    .filter(([key]) => key !== 'cursor')
    .sort(([left], [right]) => left.localeCompare(right));
  return JSON.stringify([tab, scope, filters]);
}

export function rememberFinanceCursor(cursorBack, tab, query, nextCursor, scope = '') {
  if (!nextCursor) return;
  const context = financeCursorContext(tab, query, scope);
  cursorBack.set(`${context}:${nextCursor}`, String(query.cursor || ''));
}

export function previousFinanceCursor(cursorBack, tab, query, scope = '') {
  const context = financeCursorContext(tab, query, scope);
  return cursorBack.get(`${context}:${String(query.cursor || '')}`) ?? null;
}

export function isFinanceSnapshotExpired(error) {
  return error?.status === 410 || SNAPSHOT_EXPIRED_CODES.has(String(error?.code || ''));
}

export async function runFinanceDetailRequest({ requestTracker, requestKey, load, isRouteCurrent = () => true }) {
  const generation = requestTracker.begin(requestKey);
  try {
    const value = await load();
    if (!requestTracker.isCurrent(requestKey, generation) || !isRouteCurrent()) return { status: 'stale' };
    return { status: 'ok', value };
  } catch (error) {
    if (!requestTracker.isCurrent(requestKey, generation) || !isRouteCurrent()) return { status: 'stale' };
    return { status: isFinanceSnapshotExpired(error) ? 'expired' : 'error', error };
  }
}

export function createFinanceDetailRecovery({ query, load, onRestart }) {
  let currentQuery = { ...query };
  let restartRequired = false;
  return async () => {
    if (restartRequired) {
      restartRequired = false;
      onRestart(currentQuery);
    }
    try {
      return await load(currentQuery);
    } catch (error) {
      if (isFinanceSnapshotExpired(error)) {
        currentQuery = resetExpiredFinanceQuery(currentQuery);
        restartRequired = true;
      }
      throw error;
    }
  };
}

export function financeDetailRouteIsCurrent({ canRead, current, type, id, query, normalizeQuery = (value) => value }) {
  return Boolean(canRead)
    && current?.type === type
    && current?.id === id
    && JSON.stringify(normalizeQuery(current?.query || {})) === JSON.stringify(query);
}

export function resetExpiredFinanceQuery(query = {}) {
  return { ...query, cursor: '' };
}
