import assert from 'node:assert/strict';
import test from 'node:test';
import { parseFinanceJSON, encodeFinanceJSON } from '../src/features/account-platform/finance-contract.js';
import { buildPriceDraftInput, priceDraftForm } from '../src/features/account-platform/pricing-lifecycle.js';
import { replayPricingMutation, pricingUnknownStorageKey } from '../src/features/account-platform/pricing-mutation.js';
import { currentApprovalEditDraft } from '../src/features/account-platform/admin-finance.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { tokenManagementCopy } from '../src/shared/i18n.js';

const id = '11111111-1111-4111-8111-111111111111';
const formula = { formula_version_id: id, protocol_code: 'CONTROLLED', status: 'PUBLISHED', required_unit_versions: { INPUT: id } };
const draft = { ...priceDraftForm(), scope_type: 'GLOBAL', formula_version_id: id, effective_from: '2099-01-01', units: { [id]: { config_type: 'FIXED_PRICE', fixed_price_per_denominator: '9007199254740993' } } };

test('Pricing keeps exact int64 price, unit precision, and space CAS through read/edit/write', () => {
  const wire = '{"fixed_price_per_denominator":9007199254740993,"price_denominator":9223372036854775807,"minimum_precision":1,"current_space_etag":9007199254740993}';
  const parsed = parseFinanceJSON(wire);
  assert.equal(parsed.fixed_price_per_denominator, '9007199254740993');
  assert.equal(parsed.price_denominator, '9223372036854775807');
  assert.equal(encodeFinanceJSON(parsed), wire);
  const result = buildPriceDraftInput(draft, formula, parsed.current_space_etag);
  assert.deepEqual(result.errors, {});
  assert.equal(result.input.expected_etag, '9007199254740993');
  assert.equal(result.input.units[0].fixed_price_per_denominator, '9007199254740993');
  assert.ok(encodeFinanceJSON(result.input).includes('"fixed_price_per_denominator":9007199254740993'));
  assert.ok(encodeFinanceJSON(result.input).includes('"expected_etag":9007199254740993'));
});

test('Pricing rejects overflow, decimals, negative price and absent CAS before dispatch', () => {
  for (const value of ['9223372036854775808', '-1', '1.5', '', 'Infinity']) {
    const form = { ...draft, units: { [id]: { config_type: 'FIXED_PRICE', fixed_price_per_denominator: value } } };
    assert.equal(buildPriceDraftInput(form, formula).errors[id], 'nonnegative_integer');
  }
  assert.equal(buildPriceDraftInput(draft, formula, 0).errors.expected_etag, 'required');
  assert.throws(() => parseFinanceJSON('{"space_etag":9007199254740993.0}'));
});

for (const [kind, method] of [['DRAFT_CREATE', 'createPriceDraft'], ['DRAFT_UPDATE', 'updatePriceDraft'], ['UNIT_CREATE', 'createMeteringUnit'], ['UNIT_DEACTIVATE', 'deactivateMeteringUnit'], ['PRICE_PUBLISH', 'publishPrice'], ['PRICE_TRANSITION', 'transitionPrice']]) {
  test(`${kind} uncertain replay preserves the original identity and frozen facts`, async () => {
    const calls = [];
    const client = { [method]: async (...args) => { calls.push(args); if (calls.length === 1) throw Object.assign(new Error('lost response'), { status: 503 }); return { status: 'CONFIRMED' }; } };
    const operation = Object.freeze({ phase: 'DISPATCHED', kind, targetID: id, idempotencyKey: 'controlled-original-invocation', input: Object.freeze({ expected_etag: '9007199254740993', reason: 'original reason', minimum_precision: '1' }) });
    await assert.rejects(replayPricingMutation(client, operation));
    await replayPricingMutation(client, operation);
    assert.deepEqual(calls[0], calls[1]);
    assert.equal(calls[0].at(-1), operation.idempotencyKey);
  });
}

test('Unknown recovery state is separated by owner and logout session epoch', () => {
  assert.notEqual(pricingUnknownStorageKey(id, 0), pricingUnknownStorageKey('other-owner', 0));
  assert.notEqual(pricingUnknownStorageKey(id, 0), pricingUnknownStorageKey(id, 1));
  assert.throws(() => replayPricingMutation({}, { phase: 'DISPATCHED', kind: 'UNIT_CREATE' }), /identity/);
});

test('Actual Admin client sends exact pricing facts and the invocation key on all mutations', async () => {
  const calls = []; const oldFetch = globalThis.fetch;
  globalThis.fetch = async (url, options) => {
    calls.push({ url, ...options }); const input = parseFinanceJSON(options.body);
    const created = url.endsWith('/drafts') || url.endsWith('/metering-units');
    const item = url.includes('/metering-units') ? { ...input, unit_definition_id: id, status: url.endsWith('/deactivate') ? 'INACTIVE' : 'ACTIVE' } : { ...input, price_version_id: id, lifecycle_status: 'DRAFT' };
    return new Response(encodeFinanceJSON(item), { status: created ? 201 : 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AdminPlatformClient(); client.csrfToken = 'CONTROLLED_SYNTHETIC_CSRF';
    for (const kind of ['DRAFT_CREATE', 'DRAFT_UPDATE', 'UNIT_CREATE', 'UNIT_DEACTIVATE', 'PRICE_PUBLISH', 'PRICE_TRANSITION']) {
      await replayPricingMutation(client, { phase: 'DISPATCHED', kind, targetID: id, idempotencyKey: `controlled-${kind}`, input: { expected_etag: '9007199254740993', fixed_price_per_denominator: '9007199254740993', minimum_precision: '1', price_denominator: '9007199254740993', unit_code: 'CONTROLLED', rounding_rule: 'CEILING', scope_type: 'GLOBAL', scope_id: null, protocol_code: 'CONTROLLED', formula_version_id: id } });
    }
    assert.equal(calls.length, 6);
    for (const call of calls) {
      assert.match(call.headers['Idempotency-Key'], /^controlled-/);
      assert.equal(call.headers['X-CSRF-Token'], client.csrfToken);
      if (!call.url.endsWith('/deactivate')) assert.ok(call.body.includes(':9007199254740993'));
      assert.ok(call.url.startsWith('/api/admin/billing/admin/v1/'));
    }
  } finally { globalThis.fetch = oldFetch; }
});

test('Pricing cannot confirm accepted, malformed or mismatched mutation acknowledgements', async () => {
  const oldFetch = globalThis.fetch;
  try {
    const client = new AdminPlatformClient();
    globalThis.fetch = async () => new Response('{"price_version_id":"'+id+'"}', { status: 202 });
    await assert.rejects(client.publishPrice(id, '1', 'controlled-original-invocation'), /INVALID_MUTATION_ACK/);
    globalThis.fetch = async () => new Response('{"price_version_id":"22222222-2222-4222-8222-222222222222"}', { status: 200 });
    await assert.rejects(client.publishPrice(id, '1', 'controlled-original-invocation'), /INVALID_MUTATION_ACK/);
    globalThis.fetch = async () => new Response('{}', { status: 201 });
    await assert.rejects(client.createPriceDraft(draft, 'controlled-original-invocation'), /INVALID_FINANCE_ACK/);
  } finally { globalThis.fetch = oldFetch; }
});

test('Closed typed approval command versions preserve exact numeric wire tokens', () => {
  assert.equal(encodeFinanceJSON({ normalized_command: { lifecycle_plan_version: '9007199254740993', expected_account_version: '2', credential_version: '3', source_catalog_version: '4' } }), '{"normalized_command":{"lifecycle_plan_version":9007199254740993,"expected_account_version":2,"credential_version":3,"source_catalog_version":4}}');
});

for (const type of ['TOKEN_MODEL_LIFECYCLE_CHANGE', 'TOKEN_MODEL_CATALOG_ROLLBACK', 'TOKEN_CREDENTIAL_REVOKE']) {
  test(`${type} draft edit retains its exact intent for server authority re-observation`, async () => {
    const command = { model_id: id, lifecycle_plan_id: id, lifecycle_plan_version: 3, effective_at: '2099-01-01T00:00:00+08:00', existing_session_cutoff_at: '2099-01-01T00:00:00+08:00', source_catalog_version: 2, credential_id: id };
    const detail = { approval_request_id: id, status: 'DRAFT', allowed_actions: ['EDIT'], command_type: type, command, target: { resource_type: 'CONTROLLED', resource_id: id }, critical_resource_versions: { policy_version: 2 }, financial_snapshot: {} };
    const result = await currentApprovalEditDraft({}, detail, 'Changed explanation only');
    assert.deepEqual(result.normalized_command, command);
    assert.deepEqual(result.critical_resource_versions, detail.critical_resource_versions);
    assert.equal(result.reason, 'Changed explanation only');
    await assert.rejects(currentApprovalEditDraft({}, { ...detail, status: 'SUBMITTED' }, 'Changed explanation only'));
  });
}

test('Management navigation and pricing labels exist in Chinese, English and Japanese', () => {
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(tokenManagementCopy[language].title);
    assert.ok(tokenManagementCopy[language].modelToken.modelDirectory);
    assert.ok(tokenManagementCopy[language].pricingLifecycle.priceVersions);
  }
});
