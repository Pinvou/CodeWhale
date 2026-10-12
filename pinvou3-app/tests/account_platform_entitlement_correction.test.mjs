import assert from 'node:assert/strict';
import test from 'node:test';
import { entitlementCorrectionPrecheck, entitlementCorrectionApprovalDraft, parseEntitlementCorrectionJSON, encodeEntitlementCorrectionJSON } from '../src/features/account-platform/admin-entitlement-correction.js';
import { entitlementIntentStorage, entitlementEditReferences } from '../src/features/account-platform/entitlement-correction-intent.js';
import { createAndSubmitFreshGovernedApproval } from '../src/features/account-platform/admin-governed-approval-workflow.js';

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

test('approval identity accepts exactly 120 characters and rejects 121', () => {
  assert.equal(entitlementCorrectionApprovalDraft(fixture(), 'PRODUCT_SPLIT_CORRECTION', 'Verified original payment product split', 'x'.repeat(111)).idempotency_key.length, 120);
  assert.throws(() => entitlementCorrectionApprovalDraft(fixture(), 'PRODUCT_SPLIT_CORRECTION', 'Verified original payment product split', 'x'.repeat(112)));
});

test('wallet version and monetary proof round trip as exact numeric wire tokens', () => {
  const value = fixture(); value.critical_resource_versions.wallet_version = '9223372036854775807';
  const raw = encodeEntitlementCorrectionJSON(value);
  assert.match(raw, /"wallet_version":9223372036854775807[,}]/);
  assert.match(raw, /"amount_microcredits":9007199254740993[,}]/);
  const decoded = parseEntitlementCorrectionJSON(raw);
  assert.deepEqual(decoded, value);
  assert.ok(entitlementCorrectionPrecheck(decoded));
});

test('durable entitlement proof is actor bound and late receipt cannot erase a newer invocation', () => {
  const values = new Map();
  const previous = Reflect.get(globalThis, 'sessionStorage');
  Reflect.set(globalThis, 'sessionStorage', { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: key => values.delete(key) });
  const intent = { actor: packageID, scope: 'entitlement-correction', key: 'entitlement-stable-request', facts: { precheck: fixture(), reasonCode: 'PRODUCT_SPLIT_CORRECTION', reason: 'Verified original payment product split' } };
  try {
    entitlementIntentStorage.save(intent);
    assert.deepEqual(entitlementIntentStorage.read(packageID, intent.scope), intent);
    assert.equal(entitlementIntentStorage.read(orderID, intent.scope), null);
    assert.throws(() => entitlementIntentStorage.save({ ...intent, key: 'different-stable-request' }));
    entitlementIntentStorage.clear(packageID, intent.scope, intent.key);
    entitlementIntentStorage.save({ ...intent, key: 'different-stable-request' });
    entitlementIntentStorage.clear(packageID, intent.scope, intent.key);
    assert.equal(entitlementIntentStorage.read(packageID, intent.scope).key, 'different-stable-request');
    const storageKey = [...values.keys()][0]; values.set(storageKey, '{');
    assert.throws(() => entitlementIntentStorage.read(packageID, intent.scope));
  } finally { Reflect.set(globalThis, 'sessionStorage', previous); }
});

test('storage writes and clearing require actual readback, never a fabricated receipt', () => {
  const previous = Reflect.get(globalThis, 'sessionStorage');
  const intent = { actor: packageID, scope: 'entitlement-correction', key: 'entitlement-stable-request', facts: { precheck: fixture(), reasonCode: 'PRODUCT_SPLIT_CORRECTION', reason: 'Verified original payment product split' } };
  try {
    Reflect.set(globalThis, 'sessionStorage', { getItem: () => null, setItem: () => { throw new Error('denied'); } });
    assert.throws(() => entitlementIntentStorage.save(intent));
    Reflect.set(globalThis, 'sessionStorage', { getItem: () => null, setItem: () => {} });
    assert.throws(() => entitlementIntentStorage.save(intent));
    const raw = JSON.stringify(intent);
    Reflect.set(globalThis, 'sessionStorage', { getItem: () => raw, removeItem: () => {} });
    assert.throws(() => entitlementIntentStorage.clear(packageID, intent.scope, intent.key));
  } finally { Reflect.set(globalThis, 'sessionStorage', previous); }
});

test('durable edit reference reconstructs original CAS after a different client and epoch', async () => {
  const previous = Reflect.get(globalThis, 'sessionStorage'), values = new Map();
  Reflect.set(globalThis, 'sessionStorage', { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: key => values.delete(key) });
  const intent = { actor: packageID, scope: 'entitlement-correction', key: 'entitlement-stable-request', facts: { precheck: fixture(), reasonCode: 'PRODUCT_SPLIT_CORRECTION', reason: 'Verified original payment product split' } };
  const draft = entitlementCorrectionApprovalDraft(intent.facts.precheck, intent.facts.reasonCode, intent.facts.reason, intent.key);
  let state = { approval_request_id: orderID, command_type: draft.command_type, target: { resource_type: draft.target_resource_type, resource_id: packageID }, detail_availability: 'AVAILABLE', action_control_version: 1, version: 7, status: 'DRAFT', allowed_actions: ['EDIT', 'SUBMIT'] };
  const calls = [];
  const client = epoch => ({ financeSessionEpoch: epoch, governedApprovalRequests: async () => ({ items: [structuredClone(state)] }), governedApprovalRequest: async () => structuredClone(state),
    updateGovernedApprovalDraft: async (_id, detail, input) => { calls.push({ version: detail.version, input }); assert.equal(detail.version, 7); assert.deepEqual(input, draft); state.version = 8; if (calls.length === 1) throw new TypeError('lost committed ACK'); },
    submitGovernedApprovalRequest: async () => { state.status = 'SUBMITTED'; state.version = 9; },
  });
  try {
    entitlementIntentStorage.save(intent);
    await assert.rejects(createAndSubmitFreshGovernedApproval(client(1), draft, undefined, '', entitlementEditReferences(packageID, intent.key)), TypeError);
    assert.equal(entitlementIntentStorage.read(packageID, intent.scope).editReference.existing.version, 7);
    // The hook persists the same facts again before replay; it must keep the CAS.
    entitlementIntentStorage.save(intent);
    const result = await createAndSubmitFreshGovernedApproval(client(2), draft, undefined, '', entitlementEditReferences(packageID, intent.key));
    assert.equal(result.status, 'SUBMITTED'); assert.equal(calls.length, 2);
    assert.throws(() => entitlementEditReferences(orderID, intent.key).get());
    await assert.rejects(createAndSubmitFreshGovernedApproval(client(3), { ...draft, reason: 'another immutable operator reason' }, undefined, '', entitlementEditReferences(packageID, intent.key)), { code: 'APPROVAL_FACTS_CHANGED' });
    assert.equal(calls.length, 2);
  } finally { Reflect.set(globalThis, 'sessionStorage', previous); }
});
