import assert from 'node:assert/strict';
import test from 'node:test';
import { entitlementCorrectionPrecheck, entitlementCorrectionApprovalDraft } from '../src/features/account-platform/admin-entitlement-correction.js';

const packageID = '00000000-0000-4000-8000-000000000001';
const orderID = '00000000-0000-4000-8000-000000000002';
const fixture = () => ({
  command_type: 'ENTITLEMENT_COMPONENT_ADJUSTMENT', policy_version: 2, snapshot_schema_version: 2,
  target_resource_type: 'ENTITLEMENT_PACKAGE', target_resource_id: packageID,
  normalized_command: { package_id: packageID, payment_order_id: orderID, reason_code: 'PRODUCT_SPLIT_CORRECTION' },
  critical_resource_versions: {
    package_id: packageID, wallet_id: '00000000-0000-4000-8000-000000000003', wallet_version: 7,
    payment_order_id: orderID, payment_transaction_id: '00000000-0000-4000-8000-000000000004',
    source_component_id: '00000000-0000-4000-8000-000000000005', source_lot_id: '00000000-0000-4000-8000-000000000006',
    target_component_id: null, target_lot_id: null, source_type: 'BASE', target_type: 'BONUS',
    amount_microcredits: '9007199254740993', source_evidence_digest: 'a'.repeat(64), facts_digest: 'b'.repeat(64), policy_version: 2,
  },
  financial_snapshot: { package_total_microcredits: '9223372036854775807', source_original_microcredits: '9223372036854775807',
    target_original_microcredits: '0', effective_total_microcredits: '9223372036854775807' },
});

test('owner frozen int64 facts and null target IDs survive the draft unchanged', () => {
  const value = fixture();
  const draft = entitlementCorrectionApprovalDraft(value, value.normalized_command.reason_code, 'Verified original payment product split', 'finance-stable-request-1');
  assert.deepEqual(draft.critical_resource_versions, value.critical_resource_versions);
  assert.deepEqual(draft.financial_snapshot, value.financial_snapshot);
  assert.equal(draft.critical_resource_versions.amount_microcredits, '9007199254740993');
  assert.equal(draft.financial_snapshot.package_total_microcredits, '9223372036854775807');
  assert.equal(draft.idempotency_key, 'approval:finance-stable-request-1');
});

for (const [name, mutate] of [
  ['wrong policy/schema', value => { value.snapshot_schema_version = 1; }],
  ['foreign package', value => { value.critical_resource_versions.package_id = orderID; }],
  ['foreign order', value => { value.critical_resource_versions.payment_order_id = packageID; }],
  ['missing owner evidence', value => { delete value.critical_resource_versions.facts_digest; }],
  ['caller extra snapshot', value => { value.financial_snapshot.extra = 1; }],
  ['unsafe rounded money', value => { value.critical_resource_versions.amount_microcredits = Number.MAX_SAFE_INTEGER + 2; }],
  ['int64 overflow', value => { value.financial_snapshot.package_total_microcredits = '9223372036854775808'; }],
  ['nonconserving amounts', value => { value.financial_snapshot.target_original_microcredits = '1'; }],
  ['zero correction', value => { value.critical_resource_versions.amount_microcredits = '0'; }],
  ['negative target original', value => { value.financial_snapshot.target_original_microcredits = '-1'; }],
]) test(name + ' cannot create an approval draft', () => {
  const value = fixture(); mutate(value);
  assert.equal(entitlementCorrectionPrecheck(value), null);
  assert.throws(() => entitlementCorrectionApprovalDraft(value, 'PRODUCT_SPLIT_CORRECTION', 'Verified original payment product split', 'finance-stable-request-1'));
});

test('reason change requires a new owner read instead of replacing frozen normalized facts', () => {
  assert.throws(() => entitlementCorrectionApprovalDraft(fixture(), 'ANOTHER_REASON', 'Verified original payment product split', 'finance-stable-request-1'));
});

test('reason code uses the existing proof byte bound and request key is closed', () => {
  const value = fixture(); value.normalized_command.reason_code = '中'.repeat(27);
  assert.equal(entitlementCorrectionPrecheck(value), null);
  assert.throws(() => entitlementCorrectionApprovalDraft(fixture(), 'PRODUCT_SPLIT_CORRECTION', 'Verified original payment product split', ''));
});
