import { requireFinanceCollection } from './finance-contract.js';

const SNAPSHOT_EXPIRED_CODES = new Set(['PAGINATION_SNAPSHOT_EXPIRED', 'PAGINATION_CURSOR_EXPIRED']);

function isFinanceSnapshotExpired(error) {
  return error?.status === 410 || SNAPSHOT_EXPIRED_CODES.has(String(error?.code || ''));
}

// A snapshot cursor expires independently of its filters. Retry only once,
// keeping the same filters and letting the owning panel clear its back stack.
export async function loadFinancePage(query, load, onRestart) {
  let current = { ...query };
  let value;
  try {
    value = await load(current);
  } catch (error) {
    if (!current.cursor || !isFinanceSnapshotExpired(error)) throw error;
    current = { ...current, cursor: '' };
    onRestart?.();
    value = await load(current);
  }
  requireFinanceCollection(value);
  return { value, query: current };
}
