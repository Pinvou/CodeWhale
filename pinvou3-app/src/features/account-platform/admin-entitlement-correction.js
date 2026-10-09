import { integerMoney, parseFinanceJSON, encodeFinanceJSON } from './finance-contract.js';
// Entitlement component correction (`ENTITLEMENT_COMPONENT_ADJUSTMENT`) is a
// closed policy v2 / snapshot schema v2 command. The business page never
// derives domain facts: it renders the projection returned by the read-only
// precheck and submits exactly those fields. Amounts, component/lot ids and
// digests are copied verbatim, and any caller-supplied extra field is dropped
// instead of being forwarded to the approval contract.

export const ENTITLEMENT_CORRECTION_COMMAND = 'ENTITLEMENT_COMPONENT_ADJUSTMENT';

const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const digestPattern = /^[0-9a-f]{64}$/;
const criticalKeys = ['package_id', 'wallet_id', 'wallet_version', 'payment_order_id', 'payment_transaction_id',
  'source_component_id', 'source_lot_id', 'target_component_id', 'target_lot_id', 'source_type', 'target_type',
  'amount_microcredits', 'source_evidence_digest', 'facts_digest', 'policy_version'];
const financialKeys = ['package_total_microcredits', 'source_original_microcredits', 'target_original_microcredits', 'effective_total_microcredits'];

function exactKeys(value, names) {
  return value && Object.keys(value).length === names.length && names.every(name => Object.prototype.hasOwnProperty.call(value, name));
}

function amount(value) {
  if (typeof value === 'number' && !Number.isSafeInteger(value)) return null;
  if (!['number', 'string'].includes(typeof value) || !/^(?:0|[1-9]\d*)$/.test(String(value))) return null;
  const result = BigInt(value);
  return result <= (1n << 63n) - 1n ? result : null;
}

function reasonCodeValid(value) {
  return typeof value === 'string' && value.length > 0 && value.trim() === value &&
    new TextEncoder().encode(value).length <= 80;
}

function closedObject(value) {
  return value && typeof value === 'object' && !Array.isArray(value) ? value : null;
}

function closedUUID(value) {
  return typeof value === 'string' && uuidPattern.test(value) ? value : '';
}

// entitlementCorrectionPrecheck validates the read-only precheck projection the
// server owns. It returns null when the projection is not a usable closed
// policy v2 / schema v2 statement, so the page can fail closed instead of
// assembling a request from partial facts.
export function entitlementCorrectionPrecheck(value) {
  if (value?.command_type !== ENTITLEMENT_CORRECTION_COMMAND || value.policy_version !== 2 || value.snapshot_schema_version !== 2) {
    return null;
  }
  const normalized = closedObject(value.normalized_command);
  const critical = closedObject(value.critical_resource_versions);
  const financial = closedObject(value.financial_snapshot);
  const packageID = closedUUID(normalized?.package_id);
  const orderID = closedUUID(normalized?.payment_order_id);
  if (!normalized || !critical || !financial || !packageID || !orderID ||
    !exactKeys(normalized, ['package_id', 'payment_order_id', 'reason_code']) || !reasonCodeValid(normalized.reason_code) ||
    !exactKeys(critical, criticalKeys) || !exactKeys(financial, financialKeys) ||
    value.target_resource_type !== 'ENTITLEMENT_PACKAGE' || closedUUID(value.target_resource_id) !== packageID) {
    return null;
  }
  const total = amount(financial.package_total_microcredits);
  const source = amount(financial.source_original_microcredits);
  const target = amount(financial.target_original_microcredits);
  const effective = amount(financial.effective_total_microcredits);
  const correction = amount(critical.amount_microcredits);
  if (critical.package_id !== packageID || critical.payment_order_id !== orderID ||
    !['wallet_id', 'payment_transaction_id', 'source_component_id', 'source_lot_id'].every(name => closedUUID(critical[name])) ||
    !['target_component_id', 'target_lot_id'].every(name => critical[name] === null || closedUUID(critical[name])) ||
    amount(critical.wallet_version) === null || amount(critical.wallet_version) < 1n || critical.policy_version !== 2 ||
    !digestPattern.test(critical.source_evidence_digest) || !digestPattern.test(critical.facts_digest) ||
    !((critical.source_type === 'BASE' && critical.target_type === 'BONUS') ||
      (critical.source_type === 'BONUS' && critical.target_type === 'BASE')) ||
    [total, source, target, effective, correction].some(value => value === null) ||
    total < 1n || source < 1n || correction < 1n || correction > source || source + target !== total || effective !== total) return null;
  return { packageID, orderID, reasonCode: normalized.reason_code, critical, financial,
    targetResourceType: value.target_resource_type, targetResourceID: packageID };
}

// entitlementCorrectionApprovalDraft builds the closed Account draft. The only
// operator input is the reason: the reason code travels inside the closed
// normalized command, and every other field is copied from the precheck.
export function entitlementCorrectionApprovalDraft(precheck, reasonCode, operatorReason, requestKey) {
  const projection = entitlementCorrectionPrecheck(precheck);
  const code = String(reasonCode || '').trim();
  const reason = String(operatorReason || '').trim();
  if (!projection || !reasonCodeValid(code) || code !== projection.reasonCode ||
    [...reason].length < 10 || [...reason].length > 2000 || typeof requestKey !== 'string' ||
    `approval:${requestKey}`.length < 16 || `approval:${requestKey}`.length > 128) {
    throw Object.assign(new Error('entitlement correction precheck is not usable'), {
      code: 'ENTITLEMENT_CORRECTION_PRECHECK_INVALID',
    });
  }
  return {
    command_type: ENTITLEMENT_CORRECTION_COMMAND,
    idempotency_key: `approval:${requestKey}`,
    normalized_command: { package_id: projection.packageID, payment_order_id: projection.orderID, reason_code: code },
    target_resource_type: projection.targetResourceType,
    target_resource_id: projection.targetResourceID,
    reason,
    redacted_evidence: [],
    expected_impact: {},
    critical_resource_versions: projection.critical,
    financial_snapshot: projection.financial,
  };
}

// This closed owner response adds wallet_version to the established financial
// int64 wire fields. Preserve its source token before JSON.parse, then restore
// the same integer token when the frozen proof travels to Account.
function walletVersionJSON(text, quoted) {
  let output = '';
  for (let i = 0; i < text.length;) {
    if (text[i] !== '"') { output += text[i++]; continue; }
    const start = i++;
    while (i < text.length) {
      if (text[i] === '\\') { i += 2; continue; }
      if (text[i++] === '"') break;
    }
    const token = text.slice(start, i); output += token;
    let cursor = i;
    while (cursor < text.length && /\s/.test(text[cursor])) cursor++;
    if (text[cursor] !== ':' || JSON.parse(token) !== 'wallet_version') continue;
    cursor++;
    while (cursor < text.length && /\s/.test(text[cursor])) cursor++;
    const match = text.slice(cursor).match(/^(?:"(?:0|[1-9]\d*)"|(?:0|[1-9]\d*))(?=\s*[,}\]])/);
    if (!match) throw new Error('INVALID_ENTITLEMENT_PRECHECK');
    const digits = match[0][0] === '"' ? JSON.parse(match[0]) : match[0];
    if (integerMoney(digits) < 1n) throw new Error('INVALID_ENTITLEMENT_PRECHECK');
    output += text.slice(i, cursor) + (quoted ? JSON.stringify(digits) : digits);
    i = cursor + match[0].length;
  }
  return output;
}

export function parseEntitlementCorrectionJSON(text) {
  return parseFinanceJSON(walletVersionJSON(text, true));
}

export function encodeEntitlementCorrectionJSON(value) {
  const seen = new Set();
  const validate = item => {
    if (!item || typeof item !== 'object') return;
    if (seen.has(item)) throw new Error('INVALID_FINANCE_ACK');
    seen.add(item);
    for (const [key, child] of Object.entries(item)) {
      if (key === 'wallet_version' && child !== undefined) {
        if (integerMoney(child) < 1n) throw new Error('INVALID_ENTITLEMENT_PRECHECK');
      } else validate(child);
    }
    seen.delete(item);
  };
  validate(value);
  return walletVersionJSON(encodeFinanceJSON(value), false);
}

// Approval collections may contain several command types. Locate each object's
// command before parsing its numeric tokens, even when it follows the facts.
// Only ENT's wallet_version receives the closed owner codec; other commands keep
// the established financial decoder and their existing wallet field types.
export function parseApprovalEntitlementJSON(text, decode = parseFinanceJSON) {
  let cursor = 0;
  const space = () => { while (/\s/.test(text[cursor] || '') && cursor < text.length) cursor++; };
  const invalid = () => { throw new SyntaxError('Invalid approval JSON'); };
  const string = () => {
    if (text[cursor++] !== '"') invalid();
    while (cursor < text.length) {
      if (text[cursor] === '\\') { cursor += 2; continue; }
      if (text[cursor++] === '"') return;
    }
    invalid();
  };
  const value = () => {
    space();
    const node = { start: cursor, properties: [], children: [] };
    if (text[cursor] === '{') {
      cursor++; space();
      if (text[cursor] !== '}') for (;;) {
        const start = cursor; string();
        const name = JSON.parse(text.slice(start, cursor));
        space(); if (text[cursor++] !== ':') invalid();
        node.properties.push({ name, value: value() });
        space(); if (text[cursor] !== ',') break;
        cursor++; space();
      }
      if (text[cursor++] !== '}') invalid();
    } else if (text[cursor] === '[') {
      cursor++; space();
      if (text[cursor] !== ']') for (;;) {
        node.children.push(value()); space();
        if (text[cursor] !== ',') break;
        cursor++;
      }
      if (text[cursor++] !== ']') invalid();
    } else if (text[cursor] === '"') string();
    else {
      while (cursor < text.length && !/[\s,}\]]/.test(text[cursor])) cursor++;
      JSON.parse(text.slice(node.start, cursor));
    }
    node.end = cursor;
    return node;
  };
  const root = value(); space(); if (cursor !== text.length) invalid();
  const replacements = [];
  const visit = (node, inherited) => {
    let command = inherited;
    for (const property of node.properties) {
      if (property.name === 'command_type') command = JSON.parse(text.slice(property.value.start, property.value.end));
    }
    for (const property of node.properties) {
      if (command === ENTITLEMENT_CORRECTION_COMMAND && property.name === 'wallet_version') {
        const raw = text.slice(property.value.start, property.value.end);
        if (!/^(?:"(?:0|[1-9]\d*)"|(?:0|[1-9]\d*))$/.test(raw)) throw new Error('INVALID_ENTITLEMENT_PRECHECK');
        const digits = raw[0] === '"' ? JSON.parse(raw) : raw;
        if (integerMoney(digits) < 1n) throw new Error('INVALID_ENTITLEMENT_PRECHECK');
        replacements.push({ ...property.value, text: JSON.stringify(digits) });
      } else {
        if (command === ENTITLEMENT_CORRECTION_COMMAND && !property.value.properties.length && !property.value.children.length) {
          const raw = text.slice(property.value.start, property.value.end);
          const decoded = parseFinanceJSON(`{${JSON.stringify(property.name)}:${raw}}`)[property.name];
          if (typeof decoded === 'string' && raw[0] !== '"') replacements.push({ ...property.value, text: JSON.stringify(decoded) });
        }
        visit(property.value, command);
      }
    }
    for (const child of node.children) visit(child, command);
  };
  visit(root, null);
  for (const replacement of replacements.sort((a, b) => b.start - a.start)) {
    text = text.slice(0, replacement.start) + replacement.text + text.slice(replacement.end);
  }
  return decode(text);
}
