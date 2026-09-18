import assert from 'node:assert/strict';
import test from 'node:test';

import {
  ENTITLEMENT_CORRECTION_COMMAND,
  entitlementCorrectionApprovalDraft,
  entitlementCorrectionPrecheck,
} from '../../../pinvou3-app/src/features/account-platform/admin-entitlement-correction.js';

const packageID = '0198f110-f538-7f30-a8b1-f3984fd459a0';
const orderID = '0198f110-f538-7f30-a8b1-f3984fd459a1';
const precheck = () => ({
  command_type: ENTITLEMENT_CORRECTION_COMMAND,
  policy_version: 2,
  snapshot_schema_version: 2,
  target_resource_type: 'ENTITLEMENT_PACKAGE',
  target_resource_id: packageID,
  normalized_command: { package_id: packageID, payment_order_id: orderID, reason_code: 'ORIGINAL_SPLIT_MISMATCH' },
  critical_resource_versions: { package_id: packageID, amount_microcredits: '20', policy_version: '2' },
  financial_snapshot: { package_total_microcredits: '100', effective_total_microcredits: '100' },
});

test('the business entry submits only the closed projection the precheck owns', () => {
  const draft = entitlementCorrectionApprovalDraft(precheck(), 'ORIGINAL_SPLIT_MISMATCH', 'original split mismatch', 'case-1');
  assert.deepEqual(Object.keys(draft).sort(), [
    'command_type', 'critical_resource_versions', 'expected_impact', 'financial_snapshot', 'idempotency_key',
    'normalized_command', 'reason', 'redacted_evidence', 'target_resource_id', 'target_resource_type',
  ]);
  assert.equal(draft.command_type, ENTITLEMENT_CORRECTION_COMMAND);
  assert.equal(draft.idempotency_key, 'approval:case-1');
  assert.deepEqual(draft.normalized_command, { package_id: packageID, payment_order_id: orderID, reason_code: 'ORIGINAL_SPLIT_MISMATCH' });
  assert.equal(draft.target_resource_type, 'ENTITLEMENT_PACKAGE');
  assert.equal(draft.target_resource_id, packageID);
  // Owner-domain facts are copied verbatim, never recomputed by the page.
  assert.deepEqual(draft.critical_resource_versions, precheck().critical_resource_versions);
  assert.deepEqual(draft.financial_snapshot, precheck().financial_snapshot);
});

test('caller-supplied facts and amounts are dropped, not forwarded', () => {
  const tampered = precheck();
  tampered.normalized_command.amount_microcredits = '999999';
  tampered.normalized_command.package_id = packageID;
  tampered.financial_snapshot.package_total_microcredits = '999999';
  const projection = entitlementCorrectionPrecheck(tampered);
  assert.ok(projection);
  assert.deepEqual(Object.keys(projection.critical).sort(), Object.keys(precheck().critical_resource_versions).sort());
  const draft = entitlementCorrectionApprovalDraft(tampered, 'ORIGINAL_SPLIT_MISMATCH', 'attempted amount tamper', 'case-2');
  assert.equal('amount_microcredits' in draft.normalized_command, false);
  assert.equal(draft.financial_snapshot.package_total_microcredits, '999999');
  assert.equal(draft.normalized_command.package_id, packageID);
});

test('an unusable or mismatched precheck fails closed', () => {
  for (const mutate of [
    (value) => { value.policy_version = 1; },
    (value) => { value.snapshot_schema_version = 1; },
    (value) => { value.command_type = 'WALLET_ADJUSTMENT'; },
    (value) => { value.target_resource_type = 'MANUAL_ADJUSTMENT'; },
    (value) => { value.target_resource_id = '0198f110-f538-7f30-a8b1-f3984fd459a2'; },
    (value) => { value.normalized_command.package_id = 'not-a-uuid'; },
    (value) => { delete value.critical_resource_versions; },
    (value) => { delete value.financial_snapshot; },
  ]) {
    const tampered = precheck();
    mutate(tampered);
    assert.equal(entitlementCorrectionPrecheck(tampered), null);
    assert.throws(
      () => entitlementCorrectionApprovalDraft(tampered, 'ORIGINAL_SPLIT_MISMATCH', 'closed precheck required', 'case-3'),
      (error) => error.code === 'ENTITLEMENT_CORRECTION_PRECHECK_INVALID');
  }
  assert.throws(
    () => entitlementCorrectionApprovalDraft(precheck(), '', 'missing reason code', 'case-4'),
    (error) => error.code === 'ENTITLEMENT_CORRECTION_PRECHECK_INVALID');
  assert.throws(
    () => entitlementCorrectionApprovalDraft(precheck(), 'ORIGINAL_SPLIT_MISMATCH', 'short', 'case-5'),
    (error) => error.code === 'ENTITLEMENT_CORRECTION_PRECHECK_INVALID');
});
