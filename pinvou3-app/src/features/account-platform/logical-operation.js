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

export function requireRechargeAcknowledgement(result, productId, channel) {
  if (!result || !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/iu.test(result.order_id || '')
      || result.order_id === '00000000-0000-0000-0000-000000000000'
      || result.product_id !== productId || result.channel !== channel
      || !['CREATED','PENDING','PAID','PAID_LATE','FAILED','CLOSED','MANUAL_REVIEW'].includes(result.status)
      || typeof result.expires_at !== 'string' || !Number.isFinite(Date.parse(result.expires_at))) throw { code:'REQUEST_FAILED' };
  return result;
}
