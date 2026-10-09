import { secureRequestID } from './request-id.js';
const commandReferencePattern = /^[0-9a-f]{64}$/;
const rfcUUIDPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

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

function canonicalUnitCode(value) {
  return typeof value === 'string' && value.length > 0 && value === value.trim() && value === value.toUpperCase();
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
