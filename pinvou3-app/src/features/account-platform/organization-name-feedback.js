// Match the existing frontend trim followed by the account owner's Go
// strings.TrimSpace/rune-count contract. This measures; it never rewrites wire data.
const goEdgeSpace = /^[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g;
export function organizationNameInputError(value) {
  const normalized = value.trim().replace(goEdgeSpace, '');
  if (!normalized) return 'nameRequired';
  return [...normalized].length > 120 ? 'nameTooLong' : '';
}

// A confirmed write is distinct from a subsequently confirmed directory read.
// Only the same canonical unit, at least as new as its receipt, is highlighted.
export function acceptedRenamedUnit(change, units) {
  if (!change || !units || change.result?.unit?.organization_id !== change.organizationID || change.result.unit.unit_id !== change.target) return undefined;
  const acknowledged = change.result.unit;
  if (!Number.isSafeInteger(acknowledged.version) || acknowledged.version < 1) return undefined;
  const unit = units.items?.find(item => item.organization_id === change.organizationID && item.unit_id === change.target);
  return unit && Number.isSafeInteger(unit.version) && unit.version >= acknowledged.version && typeof unit.name === 'string' ? unit : undefined;
}
