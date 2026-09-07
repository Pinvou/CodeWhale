import { secureRequestID } from './request-id.js';
const commandReferencePattern = /^[0-9a-f]{64}$/;
const rfcUUIDPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const uuidV7Pattern = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const etagPattern = /^[0-9a-f]{64}$/;
const rfc3339Pattern = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|[+-](\d{2}):(\d{2}))$/;
const provenNoWritePairs = new Set([
  '400:INVALID_COMMAND',
  '400:INVALID_PROTOCOL_COMMAND',
  '400:INVALID_FORMULA_COMMAND',
  '401:LOGIN_REQUIRED',
  '401:RECENT_AUTHENTICATION_REQUIRED',
  '403:CSRF_REJECTED',
  '403:PERMISSION_DENIED',
  '404:PROTOCOL_FORMULA_NOT_FOUND',
  '409:PROTOCOL_VERSION_CONFLICT',
  '409:FORMULA_VERSION_CONFLICT',
  '412:DRAFT_ETAG_MISMATCH',
  '422:COMMAND_PRECHECK_FAILED',
  '422:FORMULA_SYNC_REJECTED',
  '422:FORMULA_UNIT_VALIDATION_REJECTED',
  '503:TOKEN_COMMAND_GOVERNANCE_INACTIVE',
  '503:PROTOCOL_COMMAND_UNAVAILABLE',
  '503:FORMULA_COMMAND_UNAVAILABLE',
  '503:FORMULA_UNIT_VALIDATION_UNAVAILABLE',
]);

export const safeFormulaInputs = Object.freeze([
  'estimated_input_tokens',
  'cached_input_tokens',
  'max_output_tokens',
  'image_count',
  'request_count',
  'document_count',
]);

export function createProtocolFormulaIdempotencyKey(command, randomUUID = () => secureRequestID()) {
  return `admin-${command}-${randomUUID()}`;
}

export function protocolDraftCommand({ protocolCode, requestSchema, expectedHeadVersion, reason }) {
  let parsed;
  try {
    parsed = JSON.parse(requestSchema);
  } catch {
    return { error: 'invalidSchema' };
  }
  if (!parsed || Array.isArray(parsed) || typeof parsed !== 'object') return { error: 'invalidSchema' };
  if (!String(protocolCode || '').trim()) return { error: 'protocolCodeRequired' };
  if (String(reason || '').trim().length < 10) return { error: 'reasonTooShort' };
  return {
    value: {
      protocol_code: String(protocolCode).trim().toUpperCase(),
      request_schema: parsed,
      expected_protocol_head_version: Math.max(0, Number(expectedHeadVersion || 0)),
      reason: String(reason).trim(),
    },
  };
}

export function formulaDraftCommand({ rows, expectedProtocolVersion, expectedFormulaHeadVersion, reason }) {
  const normalized = (rows || []).map((row) => ({
    unitCode: String(row.unitCode || ''),
    unitDefinitionID: String(row.unitDefinitionID || ''),
    input: String(row.input || ''),
  }));
  if (!normalized.length || normalized.some((row) => !canonicalUnitCode(row.unitCode) || !rfcUUIDPattern.test(row.unitDefinitionID) || !safeFormulaInputs.includes(row.input))) return { error: 'invalidFormulaRows' };
  if (new Set(normalized.map((row) => row.unitCode)).size !== normalized.length) return { error: 'duplicateUnitCode' };
  if (new Set(normalized.map((row) => row.unitDefinitionID.toLowerCase())).size !== normalized.length) return { error: 'invalidFormulaRows' };
  if (String(reason || '').trim().length < 10) return { error: 'reasonTooShort' };
  return {
    value: {
      required_unit_versions: Object.fromEntries(normalized.map((row) => [row.unitCode, row.unitDefinitionID])),
      expression: Object.fromEntries(normalized.map((row) => [row.unitCode, row.input])),
      expected_protocol_version: Number(expectedProtocolVersion),
      expected_formula_head_version: Math.max(0, Number(expectedFormulaHeadVersion || 0)),
      reason: String(reason).trim(),
    },
  };
}

export function safeProtocolFormulaFailure(error) {
  const code = String(error?.code || 'REQUEST_FAILED');
  const reference = String(error?.details?.command_reference || '');
  return {
    code,
    status: Number.isInteger(error?.status) ? error.status : 0,
    requestID: String(error?.requestID || ''),
    commandReference: commandReferencePattern.test(reference) ? reference : '',
    unknown: code === 'COMMAND_RESULT_UNKNOWN',
  };
}

export function safeProtocolFormulaWriteFailure(error, writeDispatched) {
  const failure = safeProtocolFormulaFailure(error);
  if (!writeDispatched) return failure;
  const provenNoWrite = failure.status !== 504 && provenNoWritePairs.has(`${failure.status}:${failure.code}`);
  return {
    ...failure,
    code: failure.unknown || provenNoWrite ? failure.code : 'COMMAND_RESULT_UNKNOWN',
    unknown: failure.unknown || !provenNoWrite,
  };
}

function safeCommandReference(payload) {
  const reference = String(payload?.command_reference || '');
  return commandReferencePattern.test(reference) ? reference : '';
}

function plainObject(value) {
  return Boolean(value) && typeof value === 'object' && !Array.isArray(value);
}

function onlyKeys(value, allowed) {
  return plainObject(value) && Object.keys(value).every((key) => allowed.has(key));
}

function stableJSON(value) {
  if (Array.isArray(value)) return value.map(stableJSON);
  if (!plainObject(value)) return value;
  return Object.fromEntries(Object.keys(value).sort().map((key) => [key, stableJSON(value[key])]));
}

function sameJSON(left, right) {
  return JSON.stringify(stableJSON(left)) === JSON.stringify(stableJSON(right));
}

function validDateTime(value) {
  if (typeof value !== 'string') return false;
  const match = value.match(rfc3339Pattern);
  if (!match) return false;
  const [, yearText, monthText, dayText, hourText, minuteText, secondText, zone, offsetHourText = '0', offsetMinuteText = '0'] = match;
  const [year, month, day, hour, minute, second, offsetHour, offsetMinute] = [yearText, monthText, dayText, hourText, minuteText, secondText, offsetHourText, offsetMinuteText].map(Number);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const monthDays = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return month >= 1 && month <= 12 && day >= 1 && day <= monthDays[month - 1]
    && hour <= 23 && minute <= 59 && second <= 59
    && (zone === 'Z' || (offsetHour <= 23 && offsetMinute <= 59))
    && Number.isFinite(Date.parse(value));
}

function canonicalUnitCode(value) {
  return typeof value === 'string' && value.length > 0 && value === value.trim() && value === value.toUpperCase();
}

function validRequiredUnitMap(value) {
  if (!plainObject(value) || Object.keys(value).length === 0) return false;
  const normalizedIDs = new Set();
  for (const [code, id] of Object.entries(value)) {
    if (!canonicalUnitCode(code) || typeof id !== 'string' || !rfcUUIDPattern.test(id)) return false;
    const normalizedID = id.toLowerCase();
    if (normalizedIDs.has(normalizedID)) return false;
    normalizedIDs.add(normalizedID);
  }
  return true;
}

function validExpressionMap(value, required) {
  if (!plainObject(value) || Object.keys(value).length !== Object.keys(required).length) return false;
  return Object.entries(value).every(([code, input]) => canonicalUnitCode(code)
    && Object.prototype.hasOwnProperty.call(required, code) && typeof input === 'string' && safeFormulaInputs.includes(input));
}

function validProtocolResource(item, expectedStatus) {
  const allowed = new Set(['protocol_definition_id', 'protocol_code', 'version', 'status', 'request_schema', 'etag', 'created_at', 'published_at']);
  return onlyKeys(item, allowed)
    && uuidV7Pattern.test(item.protocol_definition_id)
    && typeof item.protocol_code === 'string' && item.protocol_code.length > 0
    && Number.isInteger(item.version) && item.version >= 1
    && item.status === expectedStatus
    && plainObject(item.request_schema)
    && etagPattern.test(item.etag)
    && validDateTime(item.created_at)
    && (expectedStatus === 'PUBLISHED' ? validDateTime(item.published_at) : item.published_at === undefined);
}

function validFormulaResource(item, expectedStatus) {
  const allowed = new Set(['formula_version_id', 'protocol_definition_id', 'protocol_code', 'version', 'status', 'required_unit_versions', 'expression', 'evaluator_version', 'etag', 'created_at', 'published_at']);
  return onlyKeys(item, allowed)
    && uuidV7Pattern.test(item.formula_version_id)
    && uuidV7Pattern.test(item.protocol_definition_id)
    && typeof item.protocol_code === 'string' && item.protocol_code.length > 0
    && Number.isInteger(item.version) && item.version >= 1
    && item.status === expectedStatus
    && validRequiredUnitMap(item.required_unit_versions)
    && validExpressionMap(item.expression, item.required_unit_versions)
    && typeof item.evaluator_version === 'string' && item.evaluator_version === 'SAFE_PARAMETER_MAP_V1'
    && etagPattern.test(item.etag)
    && validDateTime(item.created_at)
    && (expectedStatus === 'PUBLISHED' ? validDateTime(item.published_at) : item.published_at === undefined);
}

function sameProtocolSnapshot(item, snapshot) {
  return item.protocol_definition_id.toLowerCase() === snapshot.protocol_definition_id.toLowerCase()
    && item.protocol_code === snapshot.protocol_code
    && item.version === snapshot.version
    && sameJSON(item.request_schema, snapshot.request_schema)
    && item.created_at === snapshot.created_at;
}

function sameFormulaSnapshot(item, snapshot) {
  return item.formula_version_id.toLowerCase() === snapshot.formula_version_id.toLowerCase()
    && item.protocol_definition_id.toLowerCase() === snapshot.protocol_definition_id.toLowerCase()
    && item.protocol_code === snapshot.protocol_code
    && item.version === snapshot.version
    && sameJSON(item.required_unit_versions, snapshot.required_unit_versions)
    && sameJSON(item.expression, snapshot.expression)
    && item.evaluator_version === snapshot.evaluator_version
    && item.created_at === snapshot.created_at;
}

function validFormulaDraftUnits(units, required) {
  const allowed = new Set(['unit_code', 'unit_definition_id', 'version', 'status']);
  if (!validRequiredUnitMap(required) || !Array.isArray(units) || units.length !== Object.keys(required).length) return false;
  const observed = new Map();
  const observedIDs = new Set();
  for (const unit of units) {
    const normalizedID = String(unit?.unit_definition_id || '').toLowerCase();
    if (!onlyKeys(unit, allowed) || !canonicalUnitCode(unit.unit_code)
      || typeof unit.unit_definition_id !== 'string' || !rfcUUIDPattern.test(unit.unit_definition_id)
      || !Number.isInteger(unit.version) || unit.version < 1 || unit.status !== 'ACTIVE'
      || observed.has(unit.unit_code) || observedIDs.has(normalizedID)) return false;
    observed.set(unit.unit_code, normalizedID);
    observedIDs.add(normalizedID);
  }
  return Object.entries(required).every(([code, id]) => observed.get(code) === String(id).toLowerCase());
}

export function validateProtocolFormulaCommandResult(payload, action) {
  const expectedTypes = {
    PROTOCOL_DRAFT: 'TOKEN_PROTOCOL_DRAFT_CREATE',
    PROTOCOL_PUBLISH: 'TOKEN_PROTOCOL_PUBLISH',
    FORMULA_DRAFT: 'TOKEN_FORMULA_DRAFT_CREATE',
    FORMULA_PUBLISH: 'TOKEN_FORMULA_PUBLISH',
  };
  const reference = safeCommandReference(payload);
  const topLevelKeys = new Set(['command_reference', 'command_type', 'policy_version', 'status', 'protocol', 'formula', 'precheck', 'billing_sync']);
  const commonValid = reference && onlyKeys(payload, topLevelKeys) && payload.command_type === expectedTypes[action?.type]
    && payload.policy_version === 1 && payload.status === 'EXECUTED' && plainObject(payload.precheck);
  let valid = Boolean(commonValid);
  const expectedHead = action?.body?.expected_protocol_head_version ?? action?.body?.expected_formula_head_version ?? 0;
  if (valid && action.type === 'PROTOCOL_DRAFT') {
    const precheckKeys = new Set(['protocol_code_conflict', 'published_reference_count', 'expected_protocol_head_version', 'before_status', 'after_status']);
    valid = Number.isInteger(expectedHead) && expectedHead >= 0
      && payload.formula === undefined && payload.billing_sync === undefined && validProtocolResource(payload.protocol, 'DRAFT')
      && payload.protocol.protocol_code === action.body.protocol_code && payload.protocol.version === expectedHead + 1
      && sameJSON(payload.protocol.request_schema, action.body.request_schema)
      && Number.isInteger(payload.precheck.expected_protocol_head_version) && payload.precheck.expected_protocol_head_version === expectedHead
      && payload.precheck.protocol_code_conflict === false && Number.isInteger(payload.precheck.published_reference_count) && payload.precheck.published_reference_count >= 0
      && onlyKeys(payload.precheck, precheckKeys) && payload.precheck.before_status === 'NONE' && payload.precheck.after_status === 'DRAFT';
  } else if (valid && action.type === 'PROTOCOL_PUBLISH') {
    const precheckKeys = new Set(['compatible_formula_count', 'verified_metering_unit_version_count', 'formula_set_sha256', 'model_draft_reference_count', 'protocol_code', 'protocol_version', 'request_schema_valid', 'before_status', 'after_status']);
    valid = payload.formula === undefined && payload.billing_sync === undefined && validProtocolResource(action.item, 'DRAFT')
      && validProtocolResource(payload.protocol, 'PUBLISHED') && sameProtocolSnapshot(payload.protocol, action.item)
      && Number.isInteger(payload.precheck.compatible_formula_count) && payload.precheck.compatible_formula_count >= 0
      && Number.isInteger(payload.precheck.verified_metering_unit_version_count) && payload.precheck.verified_metering_unit_version_count >= 0
      && etagPattern.test(payload.precheck.formula_set_sha256)
      && Number.isInteger(payload.precheck.model_draft_reference_count) && payload.precheck.model_draft_reference_count >= 0
      && payload.precheck.protocol_code === action.item.protocol_code && payload.precheck.protocol_version === action.item.version
      && payload.precheck.request_schema_valid === true && onlyKeys(payload.precheck, precheckKeys)
      && payload.precheck.before_status === 'DRAFT' && payload.precheck.after_status === 'PUBLISHED';
  } else if (valid && action.type === 'FORMULA_DRAFT') {
    const unitCount = Object.keys(action.body.required_unit_versions || {}).length;
    const precheckKeys = new Set(['protocol_version', 'expected_formula_head_version', 'metering_unit_version_count', 'metering_unit_versions', 'parameter_mapping_valid', 'before_status', 'after_status']);
    valid = Number.isInteger(expectedHead) && expectedHead >= 0 && Number.isInteger(action.body.expected_protocol_version) && action.body.expected_protocol_version >= 1
      && validRequiredUnitMap(action.body.required_unit_versions) && validExpressionMap(action.body.expression, action.body.required_unit_versions)
      && payload.protocol === undefined && payload.billing_sync === undefined && validFormulaResource(payload.formula, 'DRAFT')
      && payload.formula.protocol_definition_id.toLowerCase() === String(action.item.protocol_definition_id || '').toLowerCase()
      && payload.formula.protocol_code === action.item.protocol_code && payload.formula.version === expectedHead + 1
      && sameJSON(payload.formula.required_unit_versions, action.body.required_unit_versions)
      && sameJSON(payload.formula.expression, action.body.expression)
      && payload.precheck.protocol_version === action.body.expected_protocol_version
      && payload.precheck.expected_formula_head_version === expectedHead
      && Number.isInteger(payload.precheck.metering_unit_version_count) && payload.precheck.metering_unit_version_count === unitCount
      && validFormulaDraftUnits(payload.precheck.metering_unit_versions, action.body.required_unit_versions)
      && payload.precheck.parameter_mapping_valid === true && onlyKeys(payload.precheck, precheckKeys)
      && payload.precheck.before_status === 'NONE' && payload.precheck.after_status === 'DRAFT';
  } else if (valid && action.type === 'FORMULA_PUBLISH') {
    const formula = payload.formula;
    const sync = payload.precheck.billing_sync;
    const precheckKeys = new Set(['deterministic_preview', 'metering_unit_version_count', 'billing_sync', 'before_status', 'after_status']);
    const syncKeys = new Set(['formula_version_id', 'status', 'protocol_code', 'required_unit_version_ids', 'formula_snapshot_sha256', 'published_at']);
    const requiredIDs = Object.values(formula?.required_unit_versions || {}).map(String).sort();
    const receiptIDs = Array.isArray(sync?.required_unit_version_ids) ? sync.required_unit_version_ids.map(String).sort() : [];
    valid = payload.protocol === undefined && validFormulaResource(action.item, 'DRAFT')
      && validFormulaResource(formula, 'PUBLISHED') && sameFormulaSnapshot(formula, action.item)
      && payload.billing_sync === 'SYNCHRONIZED' && onlyKeys(payload.precheck, precheckKeys)
      && payload.precheck.deterministic_preview === true && payload.precheck.after_status === 'PUBLISHED'
      && payload.precheck.before_status === 'DRAFT'
      && Number.isInteger(payload.precheck.metering_unit_version_count) && payload.precheck.metering_unit_version_count === requiredIDs.length && requiredIDs.length > 0
      && onlyKeys(sync, syncKeys) && sync.status === 'REGISTERED' && uuidV7Pattern.test(sync.formula_version_id)
      && sync.formula_version_id.toLowerCase() === String(action.item.formula_version_id || '').toLowerCase()
      && sync.protocol_code === action.item.protocol_code && sync.protocol_code === formula.protocol_code
      && etagPattern.test(String(sync.formula_snapshot_sha256 || '')) && !Number.isNaN(Date.parse(sync.published_at))
      && validDateTime(sync.published_at) && sync.published_at === formula.published_at
      && receiptIDs.every((id) => rfcUUIDPattern.test(id)) && new Set(receiptIDs.map((id) => id.toLowerCase())).size === receiptIDs.length
      && JSON.stringify(requiredIDs) === JSON.stringify(receiptIDs);
  }
  if (!valid) return { ok: false, failure: { code: 'COMMAND_RESULT_UNKNOWN', status: 0, requestID: '', commandReference: reference, unknown: true } };
  return { ok: true, receipt: protocolFormulaCommandReceipt(payload) };
}

export function safeFormulaPreviewSummary(payload = {}) {
  const formula = payload.formula || {};
  return {
    formulaVersionID: String(formula.formula_version_id || ''),
    protocolCode: String(formula.protocol_code || ''),
    version: Number(formula.version || 0),
    unitCodes: Object.keys(formula.required_unit_versions || {}).sort(),
    deterministic: payload.deterministic === true,
    externalAccess: payload.external_access === true,
    optionalParameterCount: Object.keys(payload.sample_missing_optional_parameters || {}).length,
  };
}

export function protocolFormulaCommandReceipt(payload = {}) {
  const precheck = payload.precheck || {};
  return {
    commandReference: commandReferencePattern.test(String(payload.command_reference || '')) ? payload.command_reference : '',
    commandType: String(payload.command_type || ''),
    status: String(payload.status || ''),
    billingSync: String(payload.billing_sync || ''),
    billingValidation: payload.command_type === 'TOKEN_FORMULA_DRAFT_CREATE' && Number.isInteger(precheck.metering_unit_version_count),
    verifiedUnitCount: Number(precheck.metering_unit_version_count ?? 0),
  };
}
