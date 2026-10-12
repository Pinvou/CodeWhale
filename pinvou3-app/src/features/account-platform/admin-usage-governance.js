import { integerMoney, parseFinanceJSON, requireFinanceID } from './finance-contract.js';
import { requireGovernedTokenPrecheck, requireGovernedTokenCommandResult } from './admin-high-risk-action-state.js';

const integerKey = /^(?:last_persisted_sequence|last_client_confirmed_sequence|evidence_high_watermark|input_tokens|output_tokens|cached_input_tokens)$/;
const commandType = 'TOKEN_USAGE_RECONCILIATION_RETRY';
const invalid = () => new Error('INVALID_USAGE_RESPONSE');

// Preserve database int64 digits before JSON.parse. Only the usage projection
// uses this codec; the established model-management wire contract is unchanged.
export function parseUsageJSON(text) {
  let output = '';
  for (let i = 0; i < text.length;) {
    if (text[i] !== '"') { output += text[i++]; continue; }
    const start = i++;
    while (i < text.length) {
      if (text[i] === '\\') { i += 2; continue; }
      if (text[i++] === '"') break;
    }
    const token = text.slice(start, i);
    output += token;
    let cursor = i;
    while (/\s/.test(text[cursor] || '') && cursor < text.length) cursor++;
    if (text[cursor] !== ':' || !integerKey.test(JSON.parse(token))) continue;
    cursor++;
    while (/\s/.test(text[cursor] || '') && cursor < text.length) cursor++;
    const match = text.slice(cursor).match(/^(?:0|[1-9]\d*)(?=\s*[,}\]])/);
    if (!match) {
      if (text[cursor] !== '"' && !/^null(?=\s*[,}\]])/.test(text.slice(cursor))) throw invalid();
      continue;
    }
    integerMoney(match[0]);
    output += text.slice(i, cursor) + JSON.stringify(match[0]);
    i = cursor + match[0].length;
  }
  const value = parseFinanceJSON(output);
  const validate = item => {
    if (!item || typeof item !== 'object') return;
    for (const [key, child] of Object.entries(item)) {
      if (integerKey.test(key) && child !== null) {
        if (integerMoney(child) < 0n) throw invalid();
      } else validate(child);
    }
  };
  validate(value);
  return value;
}

export function encodeUsageJSON(value) {
  const seen = new Set();
  const encode = (item, key = '') => {
    if (integerKey.test(key)) {
      const number = integerMoney(item);
      if (number < 0n) throw invalid();
      return number.toString();
    }
    if (item === null || typeof item !== 'object') {
      if (typeof item === 'number' && !Number.isSafeInteger(item)) throw invalid();
      return JSON.stringify(item);
    }
    if (seen.has(item)) throw invalid();
    seen.add(item);
    const result = Array.isArray(item) ? `[${item.map(child => encode(child) ?? 'null').join(',')}]`
      : `{${Object.entries(item).filter(([, child]) => child !== undefined).map(([name, child]) => `${JSON.stringify(name)}:${encode(child, name)}`).join(',')}}`;
    seen.delete(item);
    return result;
  };
  return encode(value);
}

export function requireUsageDetail(value, id) {
  requireFinanceID(id);
  if (!value || value.usage_request_id !== id || typeof value.updated_at !== 'string' || !Number.isFinite(Date.parse(value.updated_at))
    || typeof value.status !== 'string' || typeof value.execution_status !== 'string' || typeof value.settlement_status !== 'string') throw invalid();
  if (integerMoney(value.last_persisted_sequence) < 0n || integerMoney(value.maximum_microcredits) < 0n) throw invalid();
  if (value.actual_microcredits != null && integerMoney(value.actual_microcredits) < 0n) throw invalid();
  return value;
}

export function requireUsageReadiness(value) {
  if (!value || value.schema_version !== 1 || value.command_type !== commandType || typeof value.commands_enabled !== 'boolean'
    || !Number.isSafeInteger(value.governance_epoch) || value.governance_epoch < 0
    || (value.commands_enabled ? value.governance_state !== 'ACTIVE' || value.governance_epoch < 1 || value.unavailable_reason != null
      : !['GOVERNANCE_UNAVAILABLE', 'GOVERNANCE_INACTIVE'].includes(value.unavailable_reason))) throw invalid();
  return value;
}

export function requireUsagePrecheck(value, expected) {
  requireGovernedTokenPrecheck(value, expected);
  const facts = value.expected_versions;
  if (facts.usage_request_id !== expected.command.usage_request_id || facts.expected_usage_version !== expected.command.expected_usage_version
    || integerMoney(facts.evidence_high_watermark) !== integerMoney(expected.command.evidence_high_watermark)) throw invalid();
  return value;
}

export function requireUsageCommandResult(value, id, highWatermark) {
  const result = requireGovernedTokenCommandResult(value, { commandType, targetResourceType: 'USAGE_REQUEST', targetResourceID: id });
  const facts = result.redacted_result;
  if (!facts || Object.keys(facts).length !== 4 || facts.usage_request_id !== id || facts.status !== 'QUEUED' || facts.replay_model_request !== false
    || integerMoney(facts.evidence_high_watermark) !== integerMoney(highWatermark)) {
    throw Object.assign(invalid(), { code: 'INVALID_COMMAND_RESPONSE', commandReference: result.command_reference });
  }
  return result;
}
