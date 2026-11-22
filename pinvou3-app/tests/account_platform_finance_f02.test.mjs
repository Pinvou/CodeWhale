import assert from 'node:assert/strict';
import test from 'node:test';
import { parseFinanceJSON, encodeFinanceJSON, integerMoney, formatFen, safePaymentURL, wholeRefundTargets, safePaymentQR } from '../src/features/account-platform/finance-contract.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { loadFinancePage } from '../src/features/account-platform/admin-finance-pagination.js';
import { walletReplacementEligible, walletAdjustmentApprovalDraft, currentPendingAdjustment } from '../src/features/account-platform/admin-finance.js';
import { createAndSubmitFreshGovernedApproval } from '../src/features/account-platform/admin-governed-approval-workflow.js';
import { paymentAnomalyResolutions } from '../src/features/account-platform/payment-anomaly-resolutions.js';

test('unsupported anomaly types fail closed and callers cannot change other case choices', () => {
  for (const type of [undefined, 'UNKNOWN', 'toString', '__proto__', 'constructor']) assert.deepEqual(paymentAnomalyResolutions(type), []);
  const choices = paymentAnomalyResolutions('ILLEGAL_TRANSITION');
  choices.push('CONFIRM_PAYMENT');
  assert.deepEqual(paymentAnomalyResolutions('ILLEGAL_TRANSITION'), ['CONFIRM_PROVIDER_FAILED', 'ESCALATE_FINANCIAL_REPAIR']);
});

const ID = '11111111-1111-4111-8111-111111111111';
const orderText = `{"order_id":"${ID}","amount_fen":9007199254740993,"status":"PENDING","close_status":"OPEN"}`;
test('lost submitted approval ACK reuses creation identity and never adopts a different request', async () => {
  const draft = { command_type: 'PAYMENT_PROVIDER_RESUME', target_resource_type: 'PAYMENT_PROVIDER_CONTROL', target_resource_id: 'controlled', idempotency_key: 'controlled-same-logical-key', reason: 'Controlled recovery reason' };
  const detail = { approval_request_id: ID, command_type: draft.command_type, target: { resource_type: draft.target_resource_type, resource_id: draft.target_resource_id }, status: 'SUBMITTED', version: 2, action_control_version: 1, detail_availability: 'AVAILABLE', allowed_actions: [] };
  const creates = []; let submits = 0;
  const client = { governedApprovalRequests: async () => ({ items: [detail] }), createGovernedApprovalDraft: async value => { creates.push(value); return ID; }, governedApprovalRequest: async () => detail, submitGovernedApprovalRequest: async () => { submits++; } };
  assert.equal((await createAndSubmitFreshGovernedApproval(client, draft)).approval_request_id, ID);
  // The active replay rechecks the original immutable intent with the same key.
  assert.deepEqual(creates, [draft, draft]); assert.equal(submits, 0);
  client.createGovernedApprovalDraft = async () => { throw Object.assign(new Error('another invocation is blocked'), { status: 409 }); };
  await assert.rejects(createAndSubmitFreshGovernedApproval(client, { ...draft, idempotency_key: 'different-logical-key' }), error => error.status === 409);
});
test('personal nested financial JSON retains source int64 digits', () => {
  const actual = parseFinanceJSON('{"items":[{"amount_fen":9007199254740993,"points_remaining_microcredits":9223372036854775807}],"count":1}');
  assert.equal(actual.items[0].amount_fen, '9007199254740993');
  assert.equal(actual.items[0].points_remaining_microcredits, '9223372036854775807');
  assert.equal(actual.count, 1);
  assert.equal(formatFen(actual.items[0].amount_fen, 'en'), '90,071,992,547,409.93');
  assert.throws(() => parseFinanceJSON('{"amount_fen":9223372036854775808}'));
  assert.throws(() => integerMoney(Number.MAX_SAFE_INTEGER + 1));
  assert.throws(() => integerMoney('1e3'));
});
test('reconciliation order-binding evidence preserves int64 through the actual admin client', async t => {
  const previous = globalThis.fetch; t.after(() => { globalThis.fetch = previous; });
  const binding = '"amount_fen":9007199254740993,"credit_microcredits":9223372036854775807,"base_points":9007199254740993,"bonus_points":0';
  const body = `{"items":[{"internal_snapshot":{"order_binding":{${binding}}}}],"next_cursor":null}`;
  globalThis.fetch = async () => new Response(body, { status: 200 });
  const client = new AdminPlatformClient({ baseURL: 'https://controlled.example' });
  const parsed = parseFinanceJSON(body);
  assert.equal(parsed.items[0].internal_snapshot.order_binding.amount_fen, '9007199254740993');
  assert.equal(integerMoney(parsed.items[0].internal_snapshot.order_binding.credit_microcredits), 9223372036854775807n);
  assert.equal(integerMoney(parsed.items[0].internal_snapshot.order_binding.base_points), 9007199254740993n);
  const actual = await client.reconciliationCases();
  assert.deepEqual(actual.items[0].internal_snapshot, parsed.items[0].internal_snapshot);
});

test('payment links reject credentials, script schemes and nonlocal plaintext', () => {
  for (const value of ['javascript:alert(1)', 'https://user:pass@payments.example/', 'http://payments.example/', 'https://payments.example/#fragment']) assert.equal(safePaymentURL(value), '');
  assert.equal(safePaymentURL('https://payments.example/pay'), 'https://payments.example/pay');
  assert.equal(safePaymentURL('http://127.0.0.1/pay'), 'http://127.0.0.1/pay');
});
test('BFF creates same immutable recharge facts and source digits on a repeated key', async t => {
  const previous = globalThis.fetch; t.after(() => { globalThis.fetch = previous; });
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return new Response(orderText, { status: 201 }); };
  const client = new AccountPlatformBffClient(); client.csrfToken = 'PUBLIC_SYNTHETIC_CSRF';
  const key = 'recharge-11111111-1111-4111-8111-111111111111';
  for (let i = 0; i < 2; i++) assert.equal((await client.createRecharge('personal-base', 'ALIPAY', key)).amount_fen, '9007199254740993');
  assert.equal(calls.length, 2);
  for (const call of calls) {
    assert.equal(call.url, '/api/token/v1/recharge/orders');
    assert.equal(call.options.headers['Idempotency-Key'], key);
    assert.deepEqual(JSON.parse(call.options.body), { product_id: 'personal-base', channel: 'ALIPAY' });
    assert.equal(call.options.credentials, 'same-origin');
  }
  globalThis.fetch = async () => new Response('{"order_id":"' + ID + '"}', { status: 201 });
  await assert.rejects(client.createRecharge('personal-base', 'ALIPAY', key), /INVALID_FINANCE/);
});
test('desktop consumes original finance bytes instead of rounded JS transport body', async () => {
  const calls = [];
  const client = new AccountPlatformDesktopClient({ request: async input => { calls.push(input); return { status: 201, bodyText: orderText, body: JSON.parse(orderText) }; } });
  const key = 'recharge-11111111-1111-4111-8111-111111111111';
  assert.equal((await client.createRecharge('personal-base', 'WECHAT', key)).amount_fen, '9007199254740993');
  assert.equal(calls[0].idempotencyKey, key);
  assert.deepEqual(calls[0].body, { product_id: 'personal-base', channel: 'WECHAT' });
  await assert.rejects(client.rechargeOrder('22222222-2222-4222-8222-222222222222'), /INVALID_FINANCE_ACK/);
});
test('payment claim uses the real statement DTO with no partial refund contract', async t => {
  const previous = globalThis.fetch; t.after(() => { globalThis.fetch = previous; });
  let call;
  globalThis.fetch = async (url, options) => { call = { url, options }; return new Response('{}', { status: 201 }); };
  const client = new AccountPlatformBffClient(); client.csrfToken = 'PUBLIC_SYNTHETIC_CSRF';
  await client.addPaymentClaim(ID, 'Controlled payment statement');
  assert.equal(call.url, `/api/token/v1/recharge/orders/${ID}/payment-claims`);
  assert.deepEqual(JSON.parse(call.options.body), { statement: 'Controlled payment statement' });
  assert.equal('refundPaymentOrder' in client, false);
});

test('typed financial proof serialization preserves high int64 numeric tokens and null', () => {
  const source = '{"normalized_command":{"amount_microcredits":9007199254740993},"financial_snapshot":{"delta_microcredits":-9223372036854775808,"amount_fen":null},"version":2,"reason":"controlled"}';
  assert.equal(encodeFinanceJSON(parseFinanceJSON(source)), source);
  assert.throws(() => encodeFinanceJSON({ amount_microcredits: Number.MAX_SAFE_INTEGER + 1 }));
  assert.throws(() => encodeFinanceJSON({ amount_fen: '9223372036854775808' }));
  assert.equal(safePaymentQR('data:image/svg+xml;base64,PHN2Zz4='), '');
  assert.equal(safePaymentQR('data:image/png;base64,AAAA'), 'data:image/png;base64,AAAA');
});

test('refund targets require a credited source transaction and complete LEGACY components', () => {
  const transaction = { payment_transaction_id: ID, payment_order_id: ID, status: 'PAID', fulfillment_status: 'CREDITED', amount_fen: '9007199254740993' };
  const entitlement = { package_id: ID, source_payment_transaction_id: ID, status: 'ACTIVE', components: [{ component_type: 'BASE' }, { component_type: 'BONUS' }] };
  const detail = { order_id: ID, transactions: [transaction], entitlement_packages: [entitlement] };
  assert.equal(wholeRefundTargets(detail).length, 1);
  for (const components of [[], [{ component_type: 'PLAN' }], [{ component_type: 'BASE' }, { component_type: 'PLAN' }]]) assert.equal(wholeRefundTargets({ ...detail, entitlement_packages: [{ ...entitlement, components }] }).length, 0);
  assert.equal(wholeRefundTargets({ ...detail, transactions: [{ ...transaction, fulfillment_status: 'MANUAL_REVIEW' }] }).length, 0);
  assert.equal(wholeRefundTargets({ ...detail, transactions: [{ ...transaction, payment_order_id: '22222222-2222-4222-8222-222222222222' }] }).length, 0);
});

test('admin refund sends only whole immutable target facts, preserves receipt digits and rejects false ACK', async t => {
  const previous = globalThis.fetch; t.after(() => { globalThis.fetch = previous; });
  const client = new AdminPlatformClient(); client.csrfToken = 'PUBLIC_SYNTHETIC_CSRF';
  const calls = []; const key = 'refund-11111111-1111-4111-8111-111111111111';
  const receipt = { refund_id: ID, payment_order_id: ID, payment_transaction_id: ID, entitlement_package_id: ID, reason: 'controlled full refund' };
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return new Response(JSON.stringify(receipt).slice(0, -1) + ',"amount_fen":9007199254740993}', { status: 201 }); };
  for (let i = 0; i < 2; i++) assert.equal((await client.refundPaymentOrder(ID, ID, ID, receipt.reason, key)).amount_fen, '9007199254740993');
  assert.equal(calls[0].options.body, calls[1].options.body);
  assert.equal(calls[0].options.headers['Idempotency-Key'], calls[1].options.headers['Idempotency-Key']);
  assert.deepEqual(JSON.parse(calls[0].options.body), { payment_transaction_id: ID, entitlement_package_id: ID, reason: receipt.reason });
  for (const status of [200, 202]) {
    globalThis.fetch = async () => new Response(JSON.stringify(receipt), { status });
    await assert.rejects(client.refundPaymentOrder(ID, ID, ID, receipt.reason, key), /INVALID_MUTATION_ACK/);
  }
  globalThis.fetch = async () => new Response(JSON.stringify({ ...receipt, entitlement_package_id: '22222222-2222-4222-8222-222222222222' }), { status: 201 });
  await assert.rejects(client.refundPaymentOrder(ID, ID, ID, receipt.reason, key), /INVALID_FINANCE_ACK/);
});

test('expired finance snapshot retries once with identical filters and a cleared cursor', async () => {
  const filters = { q: 'merchant-needle', status: 'PAID', cursor: 'old-snapshot', limit: 50 };
  const calls = []; let resets = 0;
  const page = { items: [], next_cursor: '', snapshot_as_of: '2099-01-01T00:00:00Z' };
  const result = await loadFinancePage(filters, async query => { calls.push(query); if (query.cursor) throw Object.assign(new Error('expired snapshot'), { status: 410, code: 'PAGINATION_CURSOR_EXPIRED' }); return page; }, () => resets++);
  assert.deepEqual(calls, [filters, { ...filters, cursor: '' }]); assert.equal(resets, 1);
  assert.equal(result.value, page); assert.equal(filters.cursor, 'old-snapshot');
  let attempts = 0;
  await assert.rejects(loadFinancePage(filters, async () => { attempts++; throw Object.assign(new Error('expired snapshot'), { status: 410 }); }));
  assert.equal(attempts, 2);
  attempts = 0;
  await assert.rejects(loadFinancePage(filters, async () => { attempts++; throw Object.assign(new Error('forbidden'), { status: 403 }); }));
  assert.equal(attempts, 1);
});
test('wallet replacement requires original requester, terminal request, adjustment and wallet binding', () => {
  const other = '22222222-2222-4222-8222-222222222222';
  const detail = { approval_request_id: ID, requester: { user_id: ID }, command_type: 'WALLET_ADJUSTMENT', target: { resource_type: 'MANUAL_ADJUSTMENT', resource_id: ID }, command: { adjustment_id: ID, wallet_id: ID }, detail_availability: 'AVAILABLE' };
  for (const status of ['REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED']) assert.equal(walletReplacementEligible({ ...detail, status }, ID, ID, ID), true);
  for (const status of ['DRAFT', 'SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTED', 'EXECUTION_FAILED', 'UNKNOWN']) assert.equal(walletReplacementEligible({ ...detail, status }, ID, ID, ID), false);
  const terminal = { ...detail, status: 'REJECTED' };
  assert.equal(walletReplacementEligible(terminal, other, ID, ID), false);
  assert.equal(walletReplacementEligible(terminal, ID, other, ID), false);
  assert.equal(walletReplacementEligible(terminal, ID, ID, other), false);
  assert.equal(walletReplacementEligible({ ...terminal, detail_availability: 'UNAVAILABLE' }, ID, ID, ID), false);
  const draft = walletAdjustmentApprovalDraft({ adjustment_id: other, wallet_id: ID, requested_by: ID, amount_microcredits: '9007199254740993', version: 1 }, 'Controlled replacement reason', `replacement:${ID}:${other}`);
  assert.equal(draft.supersedes_request_id, ID); assert.equal(draft.target_resource_id, other);
  assert.equal(draft.normalized_command.wallet_id, ID);
});
test('adjustment recovery requires complete current owner facts and keeps the committed key', async () => {
  const item = { adjustment_id: ID, wallet_id: ID, requested_by: ID, request_idempotency_key: 'controlled-committed-key', reason: 'Controlled original reason', amount_microcredits: '9007199254740993', status: 'PENDING_APPROVAL', version: 1 };
  const calls = []; const client = { adjustments: async query => { calls.push(query); return { items: [item] }; } };
  assert.equal(await currentPendingAdjustment(client, item, ID), item);
  assert.deepEqual(calls, [{ wallet_id: ID, status: 'PENDING_APPROVAL', requested_by: ID, limit: 200 }]);
  for (const change of [{ amount_microcredits: '9007199254740992' }, { reason: 'Changed current reason' }, { requested_by: '22222222-2222-4222-8222-222222222222' }, { status: 'EXECUTED' }, { request_idempotency_key: 'changed-key' }]) {
    client.adjustments = async () => ({ items: [{ ...item, ...change }] });
    await assert.rejects(currentPendingAdjustment(client, item, ID));
  }
  client.adjustments = async () => ({ items: [item], next_cursor: 'another-page' });
  await assert.rejects(currentPendingAdjustment(client, item, ID));
});
