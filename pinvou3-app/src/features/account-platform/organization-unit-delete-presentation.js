import { organizationID, organizationVersion } from './organization-client.js';

const statuses = new Set(['ACTIVE', 'DISABLED', 'DELETED']);
const knownAccountBlockers = new Map([
  ['PROTECTED_CHILD_UNIT:organization_unit', 'child'],
  ['MEMBER_ATTACHED:membership', 'member'],
  ['ROLE_BINDING_ATTACHED:role_binding', 'role'],
  ['PENDING_APPROVAL_REQUEST:approval_request', 'approval'],
]);
const knownRemoteBlockers = new Map([
  ['BILLING:WALLET_BALANCE_NOT_ZERO:wallet', 'walletBalance'],
  ['BILLING:WALLET_RESERVED_BALANCE:wallet', 'walletReserved'],
  ['BILLING:WALLET_RECEIVABLE_OPEN:receivable', 'receivable'],
  ['BILLING:EFFECTIVE_ENTITLEMENT_REMAINS:entitlement_package', 'entitlement'],
  ['BILLING:UNSETTLED_USAGE_HOLD:usage_hold', 'usageHold'],
  ['BILLING:UNFINISHED_PAYMENT:payment_order', 'payment'],
  ['BILLING:UNFINISHED_REFUND:refund', 'refund'],
  ['BILLING:UNRESOLVED_CHARGEBACK:chargeback_case', 'chargeback'],
  ['BILLING:UNRESOLVED_FORCED_REFUND:forced_refund_case', 'forcedRefund'],
  ['BILLING:OPEN_RECONCILIATION_DIFFERENCE:reconciliation_case', 'reconciliation'],
  ['BILLING:PENDING_USAGE_REVIEW:usage_loss_review', 'usageReview'],
  ['BILLING:PENDING_FINANCIAL_ADJUSTMENT:manual_adjustment', 'adjustment'],
  ['TOKEN:TOKEN_ACCOUNT_ATTACHED:token_account', 'tokenAccount'],
  ['TOKEN:UNSETTLED_UNIT_USAGE:usage_request', 'tokenUsage'],
]);
const domains = new Set(['ACCOUNT', 'BILLING', 'TOKEN']);

export function currentDirectUnitCount(organization, detail, unitID) {
  try {
    const org = organizationID(organization?.organization_id);
    const source = organizationID(unitID);
    const tree = detail?.units;
    if (detail?.unitsError || detail?.summary?.organization_id !== org || !Array.isArray(tree?.items) || tree.has_more || tree.next_cursor) return null;
    organizationVersion(tree.structure_version);
    const units = new Map();
    for (const unit of tree.items) {
      const id = organizationID(unit.unit_id);
      if (unit.organization_id !== org || units.has(id) || !statuses.has(unit.status)) return null;
      if (unit.parent_id !== null) organizationID(unit.parent_id);
      units.set(id, unit);
    }
    if (!units.has(source)) return null;
    const verified = new Set();
    for (const id of units.keys()) {
      const chain = new Set();
      let next = id;
      while (next !== null && !verified.has(next)) {
        if (chain.has(next) || !units.has(next)) return null;
        chain.add(next);
        next = units.get(next).parent_id;
      }
      for (const entry of chain) verified.add(entry);
    }
    return tree.items.filter(unit => unit.parent_id === source && unit.status !== 'DELETED').length;
  } catch { return null; }
}

export function confirmedUnitDeleteBlockers(error, orgID, unitID) {
  if (error?.status !== 409 || error.code !== 'ORGANIZATION_UNIT_LIFECYCLE_BLOCKED') return null;
  const details = error.details;
  if (details?.organization_id !== orgID || details.unit_id !== unitID || details.requested_status !== 'DELETION_PENDING' ||
    !Number.isSafeInteger(details.structure_version) || details.structure_version < 1 ||
    !Array.isArray(details.blockers) || !details.blockers.length) return null;
  const groups = new Set();
  for (const item of details.blockers) {
    if (!domains.has(item?.domain) || typeof item.code !== 'string' || !item.code.trim() ||
      typeof item.resource_type !== 'string' || !item.resource_type.trim() ||
      typeof item.resource_id !== 'string' || !item.resource_id.trim()) return null;
    groups.add(item.domain === 'ACCOUNT'
      ? knownAccountBlockers.get(item.code + ':' + item.resource_type) || 'account'
      : knownRemoteBlockers.get(item.domain + ':' + item.code + ':' + item.resource_type) || (item.domain === 'BILLING' ? 'billing' : 'token'));
  }
  return [...groups];
}
