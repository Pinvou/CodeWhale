import { integerMoney } from './finance-contract.js';
// The installed ledger repair command are closed policy v1 / snapshot schema v1
// contracts. The business page never derives domain facts: it renders the
// projection returned by the read-only precheck and submits exactly the closed
// key set with the one fixed reason/resolution code the command owns. Every
// caller-supplied extra field is dropped instead of being forwarded, so a page
// bug can never widen the wire contract.

const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const datePattern = /^\d{4}-\d{2}-\d{2}$/;

const PRECHECK_INVALID = 'FINANCIAL_REPAIR_PRECHECK_INVALID';

// financialRepairCommands is the closed per-command descriptor. `reasonKey` is
// the wire key that carries the command's single fixed code, `targetKey` names
// the normalized field the target resource id must equal, and `fields` is the
// exact closed key set of the normalized command the owner precheck returns.
export const financialRepairCommands = Object.freeze({
  LEDGER_REPAIR: {
    targetResourceType: 'LEDGER_POSTING',
    reasonKey: 'reason_code',
    reasonCode: 'ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT',
    targetKey: 'target_posting_id',
    fields: [
      { key: 'target_journal_entry_id', kind: 'uuid' },
      { key: 'target_posting_id', kind: 'uuid' },
      { key: 'wrong_ledger_account_id', kind: 'uuid' },
      { key: 'correct_ledger_account_id', kind: 'uuid' },
      { key: 'amount_microcredits', kind: 'integer' },
    ],
  },

});

function closedObject(value) {
  return value && typeof value === 'object' && !Array.isArray(value) ? value : null;
}

function closedString(value) {
  return typeof value === 'string' ? value.trim() : '';
}

function validField(value, field) {
  switch (field.kind) {
    case 'integer': {
      if (typeof value !== 'number' && typeof value !== 'string') return false;
      try { return integerMoney(value) > 0n; } catch { return false; }
    }
    default:
      break;
  }
  const text = closedString(value);
  if (!text || text !== value) {
    return false;
  }
  if (field.kind === 'uuid') return uuidPattern.test(text);
  if (field.kind === 'date') return datePattern.test(text);
  return field.kind === 'enum' && field.allowed.includes(text);
}

// financialRepairPrecheck validates the read-only precheck projection the
// server owns and returns the closed projection the page may submit. It returns
// null for any unusable or inconsistent projection, so the page fails closed
// instead of assembling a request from partial facts.
export function financialRepairPrecheck(value) {
  const command = financialRepairCommands[value?.command_type];
  const normalized = closedObject(value?.normalized_command);
  const critical = closedObject(value?.critical_resource_versions);
  const financial = closedObject(value?.financial_snapshot);
  if (!command || value?.policy_version !== 1 || value?.snapshot_schema_version !== 1 ||
    !normalized || !critical || !financial ||
    value?.target_resource_type !== command.targetResourceType) {
    return null;
  }
  const targetResourceID = closedString(value?.target_resource_id);
  if (!uuidPattern.test(targetResourceID) || closedString(normalized[command.targetKey]) !== targetResourceID) {
    return null;
  }
  for (const field of command.fields) {
    if (!validField(normalized[field.key], field)) {
      return null;
    }
  }
  return {
    commandType: value.command_type,
    command,
    normalized,
    critical,
    financial,
    targetResourceType: command.targetResourceType,
    targetResourceID,
  };
}

// financialRepairApprovalDraft builds the closed Account draft. The only
// operator inputs are the free-text reason and the request key: the normalized
// command is rebuilt from exactly the closed key set plus the command's fixed
// code, and critical/financial projections are copied verbatim.
export function financialRepairApprovalDraft(precheck, operatorReason, requestKey, options = {}) {
  const projection = financialRepairPrecheck(precheck);
  const reason = closedString(operatorReason);
  const key = closedString(requestKey);
  if (!projection || reason.length < 10 || reason.length > 2000 || !key || new TextEncoder().encode(key).length > 111) {
    throw Object.assign(new Error('financial repair precheck is not usable'), { code: PRECHECK_INVALID });
  }
  const normalized = {};
  for (const field of projection.command.fields) {
    const value = projection.normalized[field.key];
    if (field.kind === 'enum' && options[field.key] !== undefined) {
      // The evidence kind is the only field the operator may narrow, and only
      // inside the command's closed allowed set.
      const requested = closedString(options[field.key]);
      if (!field.allowed.includes(requested)) {
        throw Object.assign(new Error('financial repair evidence kind is outside the closed set'), { code: PRECHECK_INVALID });
      }
      normalized[field.key] = requested;
      continue;
    }
    normalized[field.key] = value;
  }
  normalized[projection.command.reasonKey] = projection.command.reasonCode;
  return {
    command_type: projection.commandType,
    idempotency_key: `approval:${key}`,
    normalized_command: normalized,
    target_resource_type: projection.targetResourceType,
    target_resource_id: projection.targetResourceID,
    reason,
    redacted_evidence: [],
    expected_impact: {},
    critical_resource_versions: projection.critical,
    financial_snapshot: projection.financial,
  };
}
