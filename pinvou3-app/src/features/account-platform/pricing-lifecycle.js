import { integerMoney } from './finance-contract.js';
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

export function createSynchronousMutationLock() {
  let locked = false;
  return {
    tryAcquire() {
      if (locked) return false;
      locked = true;
      return true;
    },
    release() { locked = false; },
    isLocked() { return locked; },
  };
}

export const pricingLifecycleMutationLock = createSynchronousMutationLock();

export function newModelPriceAllowed(form, canReadModels, models = [], formula = null) {
  if (form?.price_version_id || form?.scope_type !== 'MODEL') return true;
  return Boolean(canReadModels && formula && models.some((item) => item.model_id === form.scope_id && item.lifecycle_status !== 'REMOVED' && item.protocol_code === formula.protocol_code));
}

export function shanghaiDate(value) {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  const parts = new Intl.DateTimeFormat('en-CA', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).formatToParts(date);
  const part = (type) => parts.find((item) => item.type === type)?.value || '';
  return `${part('year')}-${part('month')}-${part('day')}`;
}

export function shanghaiBoundary(value) {
  return /^\d{4}-\d{2}-\d{2}$/.test(String(value || '')) ? `${value}T00:00:00+08:00` : '';
}

export function publishedPricingFormulas(payload = {}) {
  return (payload.formulas || [])
    .filter((item) => item.status === 'PUBLISHED' && UUID.test(String(item.formula_version_id || '')))
    .sort((left, right) => left.protocol_code.localeCompare(right.protocol_code) || Number(right.version) - Number(left.version));
}

export function priceSpaceQuery(form, formula) {
  return {
    scope_type: form.scope_type === 'GLOBAL' ? 'GLOBAL' : 'MODEL',
    scope_id: form.scope_type === 'GLOBAL' ? '' : String(form.scope_id || '').trim(),
    protocol_code: formula?.protocol_code || form.protocol_code || '',
    formula_version_id: formula?.formula_version_id || form.formula_version_id || '',
    credit_unit: 'MICROCREDIT',
  };
}

export function priceDraftForm(item = null) {
  if (!item) return {
    price_version_id: '', scope_type: 'MODEL', scope_id: '', formula_version_id: '', protocol_code: '',
    effective_from: '', effective_to: '', expected_etag: 1, units: {},
  };
  return {
    price_version_id: item.price_version_id,
    scope_type: item.scope_type,
    scope_id: item.scope_id || '',
    formula_version_id: item.formula_version_id,
    protocol_code: item.protocol_code,
    effective_from: shanghaiDate(item.effective_from),
    effective_to: shanghaiDate(item.effective_to),
    expected_etag: item.space_etag,
    units: Object.fromEntries((item.units || []).map((unit) => [unit.unit_definition_id, {
      config_type: unit.config_type,
      fixed_price_per_denominator: unit.fixed_price_per_denominator == null ? '' : String(unit.fixed_price_per_denominator),
    }])),
  };
}

export function buildPriceDraftInput(form, formula, currentSpaceETag = 1) {
  const errors = {};
  const scopeType = form.scope_type === 'GLOBAL' ? 'GLOBAL' : 'MODEL';
  const scopeID = String(form.scope_id || '').trim();
  if (scopeType === 'MODEL' && !scopeID) errors.scope_id = 'required';
  if (!formula || formula.formula_version_id !== form.formula_version_id || formula.status !== 'PUBLISHED') errors.formula_version_id = 'required';
  const effectiveFrom = shanghaiBoundary(form.effective_from);
  const effectiveTo = form.effective_to ? shanghaiBoundary(form.effective_to) : null;
  if (!effectiveFrom) errors.effective_from = 'invalid';
  if (effectiveTo && effectiveTo <= effectiveFrom) errors.effective_to = 'range';
  const units = Object.entries(formula?.required_unit_versions || {}).map(([, unitDefinitionID]) => {
    const value = form.units?.[unitDefinitionID] || {};
    const configType = scopeType === 'GLOBAL' ? 'FIXED_PRICE' : value.config_type === 'INHERIT_GLOBAL' ? 'INHERIT_GLOBAL' : 'FIXED_PRICE';
    if (configType === 'INHERIT_GLOBAL') return { unit_definition_id: unitDefinitionID, config_type: configType };
    const raw = String(value.fixed_price_per_denominator ?? '').trim();
    let exact = '';
    try {
      if (!/^\d+$/.test(raw)) throw new Error('invalid price');
      exact = integerMoney(BigInt(raw).toString()).toString();
      if (BigInt(exact) < 0n) throw new Error('negative price');
    } catch { errors[unitDefinitionID] = 'nonnegative_integer'; }
    return { unit_definition_id: unitDefinitionID, config_type: configType, fixed_price_per_denominator: exact };
  });
  if (!units.length) errors.units = 'required';
  let expectedETag;
  try {
    expectedETag = integerMoney(form.price_version_id ? form.expected_etag : currentSpaceETag).toString();
    if (BigInt(expectedETag) < 1n) throw new Error('invalid etag');
  } catch { errors.expected_etag = 'required'; }
  return {
    errors,
    input: {
      scope_type: scopeType,
      scope_id: scopeType === 'MODEL' ? scopeID : null,
      protocol_code: formula?.protocol_code || '',
      formula_version_id: formula?.formula_version_id || '',
      credit_unit: 'MICROCREDIT',
      effective_from: effectiveFrom,
      effective_to: effectiveTo,
      expected_etag: expectedETag,
      units,
    },
  };
}

export function priceTransitionModes(item) {
  if (item?.derived_status !== 'ACTIVE') return [];
  return item.scope_type === 'MODEL' ? ['FALLBACK_GLOBAL', 'CONTINUATION', 'BLOCK'] : ['CONTINUATION'];
}

export function matchingContinuationDrafts(item, prices = []) {
  return prices.filter((candidate) => candidate.price_version_id !== item.price_version_id
    && candidate.lifecycle_status === 'DRAFT'
    && candidate.scope_type === item.scope_type
    && String(candidate.scope_id || '') === String(item.scope_id || '')
    && candidate.protocol_code === item.protocol_code
    && candidate.formula_version_id === item.formula_version_id
    && candidate.credit_unit === item.credit_unit);
}
