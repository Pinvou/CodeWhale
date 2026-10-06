const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function invalidAdminAcknowledgement() {
  const error = new Error('REQUEST_FAILED');
  error.code = 'REQUEST_FAILED';
  return error;
}

export function requireAdminRecord(value, idField, expectedID) {
  if (!value || typeof value !== 'object' || Array.isArray(value)
      || !uuidPattern.test(value[idField] || '') || value[idField] === '00000000-0000-0000-0000-000000000000'
      || (expectedID && value[idField] !== expectedID)) {
    throw invalidAdminAcknowledgement();
  }
  return value;
}

export function requireAdminItems(value) {
  if (!value || !Array.isArray(value.items)) throw invalidAdminAcknowledgement();
  return value.items;
}

export function requireAdminFacts(value, facts) {
  for (const [key, expected] of Object.entries(facts)) {
    if (canonicalFact(value[key] ?? null) !== canonicalFact(expected ?? null)) throw invalidAdminAcknowledgement();
  }
  return value;
}

export function requirePriceDraftAcknowledgement(value, input, expectedID) {
  requireAdminFacts(requireAdminRecord(value, 'price_version_id', expectedID), {
    scope_type:input.scope_type, scope_id:input.scope_id, protocol_code:input.protocol_code,
    formula_version_id:input.formula_version_id, credit_unit:input.credit_unit, lifecycle_status:'DRAFT',
  });
  for (const field of ['effective_from','effective_to']) {
    const expected = input[field] ?? null;
    const actual = value[field] ?? null;
    if (expected === null ? actual !== null : typeof actual !== 'string' || !Number.isFinite(Date.parse(actual)) || Date.parse(actual) !== Date.parse(expected)) throw invalidAdminAcknowledgement();
  }
  const expectedETag = input.expected_etag + (expectedID ? 1 : 0);
  if (!Number.isSafeInteger(input.expected_etag) || input.expected_etag < 1 || !Number.isSafeInteger(expectedETag)
      || value.space_etag !== expectedETag || !Number.isSafeInteger(value.version) || value.version < 1
      || !Array.isArray(value.units) || value.units.length !== input.units.length) throw invalidAdminAcknowledgement();
  const returned = new Map(value.units.map(unit => [unit.unit_definition_id, unit]));
  if (returned.size !== input.units.length) throw invalidAdminAcknowledgement();
  for (const unit of input.units) {
    const actual = returned.get(unit.unit_definition_id);
    if (!actual || actual.config_type !== unit.config_type) throw invalidAdminAcknowledgement();
    if (unit.config_type === 'FIXED_PRICE') {
      if (actual.fixed_price_per_denominator !== String(unit.fixed_price_per_denominator)) throw invalidAdminAcknowledgement();
    } else if (actual.fixed_price_per_denominator != null) throw invalidAdminAcknowledgement();
  }
  return value;
}

function canonicalFact(value) {
  if (Array.isArray(value)) return JSON.stringify(value.map(item => JSON.parse(canonicalFact(item))));
  if (value && typeof value === 'object') return JSON.stringify(Object.fromEntries(Object.keys(value).sort().map(key => [key, JSON.parse(canonicalFact(value[key]))])));
  return JSON.stringify(value);
}
