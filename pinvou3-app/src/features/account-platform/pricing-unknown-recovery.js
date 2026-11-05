// Retained UNKNOWN is local recovery evidence, never a server audit result.
export function pricingUnknownHistoryKey(storageKey) {
  return `${storageKey}:history`;
}

export function readPricingUnknownHistory(storageKey, storage = globalThis.sessionStorage) {
  const raw = storage.getItem(pricingUnknownHistoryKey(storageKey));
  if (!raw) return [];
  const records = JSON.parse(raw);
  if (
    !Array.isArray(records) ||
    records.some(
      (record) =>
        record?.status !== 'UNKNOWN' ||
        !record.operation?.idempotencyKey ||
        record.operation.phase !== 'DISPATCHED',
    )
  )
    throw new Error('INVALID_UNKNOWN_HISTORY');
  return records;
}

export function archivePricingUnknown(storageKey, operation, storage = globalThis.sessionStorage) {
  if (!storage || operation?.phase !== 'DISPATCHED' || !operation.idempotencyKey)
    throw new Error('UNKNOWN_PRESERVATION_FAILED');
  const records = readPricingUnknownHistory(storageKey, storage);
  const current = storage.getItem(storageKey);
  if (current && current !== JSON.stringify(operation)) throw new Error('UNKNOWN_IDENTITY_CHANGED');
  const existing = records.find(
    (record) => record.operation.idempotencyKey === operation.idempotencyKey,
  );
  if (existing && JSON.stringify(existing.operation) !== JSON.stringify(operation))
    throw new Error('UNKNOWN_IDENTITY_CHANGED');
  const next = existing ? records : [...records, { status: 'UNKNOWN', operation }];
  const encoded = JSON.stringify(next);
  storage.setItem(pricingUnknownHistoryKey(storageKey), encoded);
  if (storage.getItem(pricingUnknownHistoryKey(storageKey)) !== encoded)
    throw new Error('UNKNOWN_PRESERVATION_FAILED');
  try {
    storage.removeItem(storageKey);
    if (storage.getItem(storageKey) !== null) throw new Error('UNKNOWN_PRESERVATION_FAILED');
  } catch (error) {
    // History is already durable. Restore the active marker when storage still
    // permits writes, and always keep the in-memory operation locked on error.
    storage.setItem(storageKey, JSON.stringify(operation));
    throw error;
  }
  return next;
}

// Read the original target/namespace. A current projection cannot prove whether
// the original command executed; it only qualifies explicit subsequent work.
export async function readPricingUnknownConfiguration(client, operation, signal) {
  const epoch = client.financeSessionEpoch;
  const current = () => client.requireCurrentSession(epoch, signal);
  if (!operation?.idempotencyKey || operation.phase !== 'DISPATCHED')
    throw new Error('UNKNOWN_IDENTITY_MISSING');
  if (['UNIT_CREATE', 'UNIT_DEACTIVATE'].includes(operation.kind)) {
    const payload = await client.meteringUnits(signal);
    current();
    if (
      !Array.isArray(payload?.items) ||
      payload.items.some((unit) => !unit.unit_definition_id || !unit.unit_code || !unit.status)
    )
      throw new Error('INVALID_UNIT_CONFIGURATION');
    if (
      operation.kind === 'UNIT_DEACTIVATE' &&
      !payload.items.some((unit) => unit.unit_definition_id === operation.targetID)
    )
      throw new Error('UNKNOWN_TARGET_UNAVAILABLE');
    return {
      units: payload.items,
      unit_code: operation.input?.unit_code,
      targetID: operation.targetID,
    };
  }
  let target;
  if (operation.targetID) {
    const payload = await client.managedPrice(operation.targetID, signal);
    current();
    target = payload?.item;
    if (target?.price_version_id !== operation.targetID)
      throw new Error('UNKNOWN_TARGET_UNAVAILABLE');
  }
  const namespace = target || (operation.kind === 'DRAFT_CREATE' ? operation.input : null);
  if (
    !namespace ||
    !['GLOBAL', 'MODEL'].includes(namespace.scope_type) ||
    !namespace.protocol_code ||
    !namespace.formula_version_id ||
    !namespace.credit_unit ||
    (namespace.scope_type === 'MODEL' && !namespace.scope_id)
  )
    throw new Error('UNKNOWN_NAMESPACE_UNAVAILABLE');
  const query = {
    scope_type: namespace.scope_type,
    scope_id: namespace.scope_id,
    protocol_code: namespace.protocol_code,
    formula_version_id: namespace.formula_version_id,
    credit_unit: namespace.credit_unit,
  };
  const space = await client.managedPriceSpace(query, signal);
  current();
  if (
    !Array.isArray(space?.items) ||
    space.has_more !== false ||
    !/^[1-9]\d*$/.test(String(space.current_space_etag))
  )
    throw new Error('UNKNOWN_CONFIGURATION_INCOMPLETE');
  if (
    space.items.some(
      (item) =>
        item.scope_type !== query.scope_type ||
        String(item.scope_id || '') !== String(query.scope_id || '') ||
        item.protocol_code !== query.protocol_code ||
        item.formula_version_id !== query.formula_version_id ||
        item.credit_unit !== query.credit_unit,
    )
  )
    throw new Error('UNKNOWN_NAMESPACE_MISMATCH');
  return {
    target,
    namespace: query,
    current_space_etag: space.current_space_etag,
    items: space.items,
  };
}

// Intent ordering is synchronous; React loading updates cannot grant authority.
export function createPricingUnknownRecoveryGate() {
  let generation = 0;
  let replayTicket = null;
  return {
    get generation() {
      return generation;
    },
    get replaying() {
      return replayTicket !== null;
    },
    beginRead() {
      if (replayTicket !== null) throw new Error('UNKNOWN_REPLAY_IN_PROGRESS');
      return ++generation;
    },
    readIsCurrent(ticket) {
      return replayTicket === null && ticket === generation;
    },
    beginReplay() {
      if (replayTicket !== null) throw new Error('UNKNOWN_REPLAY_IN_PROGRESS');
      replayTicket = ++generation;
      return replayTicket;
    },
    endReplay(ticket) {
      if (ticket === replayTicket) replayTicket = null;
    },
    retire() {
      generation += 1;
    },
  };
}
