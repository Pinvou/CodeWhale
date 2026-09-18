// Entitlement component correction (`ENTITLEMENT_COMPONENT_ADJUSTMENT`) is a
// closed policy v2 / snapshot schema v2 command. The business page never
// derives domain facts: it renders the projection returned by the read-only
// precheck and submits exactly those fields. Amounts, component/lot ids and
// digests are copied verbatim, and any caller-supplied extra field is dropped
// instead of being forwarded to the approval contract.

export const ENTITLEMENT_CORRECTION_COMMAND = 'ENTITLEMENT_COMPONENT_ADJUSTMENT';

const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

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
    value.target_resource_type !== 'ENTITLEMENT_PACKAGE' || closedUUID(value.target_resource_id) !== packageID) {
    return null;
  }
  return { packageID, orderID, critical, financial, targetResourceType: value.target_resource_type, targetResourceID: packageID };
}

// entitlementCorrectionApprovalDraft builds the closed Account draft. The only
// operator input is the reason: the reason code travels inside the closed
// normalized command, and every other field is copied from the precheck.
export function entitlementCorrectionApprovalDraft(precheck, reasonCode, operatorReason, requestKey) {
  const projection = entitlementCorrectionPrecheck(precheck);
  const code = String(reasonCode || '').trim();
  const reason = String(operatorReason || '').trim();
  if (!projection || !code || code.length > 80 || reason.length < 10 || reason.length > 2000) {
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
