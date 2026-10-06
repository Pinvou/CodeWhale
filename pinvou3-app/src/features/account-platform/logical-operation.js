// Acknowledgement belongs to one logical call and its exact identity/facts.
// Unknown or partial outcomes retain the key; a completed call releases it.
export async function performLogicalOperation(cache, { kind, identity, createKey, action, acknowledged = () => true }) {
  let pending = cache.get(kind);
  if (!pending || pending.identity !== identity) {
    pending = { identity, key: createKey(kind) };
    cache.set(kind, pending);
  }
  const result = await action(pending.key);
  if (acknowledged(result) && cache.get(kind) === pending) cache.delete(kind);
  return result;
}

export function quotaBatchAcknowledged(result) {
  return Array.isArray(result?.items) && result.items.length > 0
    && result.items.every(item => item?.status === 'SUCCEEDED');
}
