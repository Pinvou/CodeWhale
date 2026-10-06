import assert from 'node:assert/strict';
import { randomFillSync } from 'node:crypto';
import test from 'node:test';
import { createAdminOperationScope, createAdminMutationOwner } from '../src/features/account-platform/admin-operation-scope.js';
import { parseScaledAmount, formatMoney } from '../src/features/account-platform/credit-units.js';
import { auditCSV } from '../src/features/account-platform/audit-csv.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';

test('a view change fences late reads and cancels only read work', () => {
  const scope = createAdminOperationScope();
  const first = scope.begin();
  assert.equal(scope.isCurrent(first), true);
  scope.invalidate();
  assert.equal(first.controller.signal.aborted, true);
  assert.equal(scope.isCurrent(first), false);
  const second = scope.begin();
  assert.ok(second);
  scope.finish(first);
  assert.equal(scope.busy, true, 'an older finally cannot release a newer read');
  assert.equal(scope.isCurrent(second), true);
  scope.finish(second);
  assert.equal(scope.busy, false);
});

test('submitted mutations remain locked across view changes and duplicate clicks', () => {
  const scope = createAdminOperationScope();
  const submitted = scope.begin(true);
  assert.equal(scope.begin(true), null);
  scope.invalidate();
  assert.equal(submitted.controller.signal.aborted, false, 'navigation cannot cancel server work');
  assert.equal(scope.isCurrent(submitted), false, 'late results cannot update a new view');
  assert.equal(scope.begin(true), null, 'navigation cannot silently release a mutation lock');
  scope.finish(submitted);
  assert.ok(scope.begin(true));
});

test('main navigation cannot abandon a submitted mutation or release its replacement owner', () => {
  const owner = createAdminMutationOwner();
  const firstView = createAdminOperationScope(owner, 'billing');
  const submitted = firstView.begin(true);
  firstView.invalidate();
  const reopened = createAdminOperationScope(owner, 'billing');
  assert.equal(reopened.busy, true);
  assert.equal(reopened.begin(true), null);
  assert.equal(firstView.isCurrent(submitted), false);
  firstView.finish(submitted);
  const next = reopened.begin(true);
  assert.ok(next);
  firstView.finish(submitted);
  assert.equal(reopened.busy, true, 'an old owner cannot release a newer write');
  reopened.finish(next);
  assert.equal(reopened.busy, false);
});

test('decimal input preserves both declared scales and safe integer boundary exactly', () => {
  assert.equal(parseScaledAmount('0.0001', 4), 1);
  assert.equal(parseScaledAmount('900719925474.0991', 4), Number.MAX_SAFE_INTEGER);
  assert.equal(parseScaledAmount('900719925474.0992', 4), null);
  assert.equal(parseScaledAmount('900719925474.0993', 4), null);
  assert.equal(parseScaledAmount('90071992547409.91', 2), Number.MAX_SAFE_INTEGER);
  assert.equal(parseScaledAmount('90071992547409.93', 2), null);
  assert.equal(parseScaledAmount('0.29', 2), 29);
  assert.equal(parseScaledAmount('.29', 2), 29);
  assert.equal(parseScaledAmount('1.', 2), 100);
  assert.equal(parseScaledAmount(' 001.20 ', 2), 120);
  assert.equal(parseScaledAmount('0', 4), 0);
  for (const value of ['0.00001', '-1', 'NaN', 'Infinity', '1e3', '', '1.2.3']) assert.equal(parseScaledAmount(value, 4), null, value);
  assert.equal(parseScaledAmount('1.001', 2), null);
  assert.equal(formatMoney('9007199254740993', 'en'), '¥90,071,992,547,409.93');
});

test('audit CSV quotes data and neutralizes spreadsheet formula prefixes', () => {
  for (const value of ['=1+1', '+cmd', '-1+2', '@sum(1)', '\t=1+1', '\r=1+1', '\n=1+1', '  =1+1', '＝1+1', '＋cmd', '－1', '＠cmd']) {
    const csv = auditCSV([{ actor_display_name:value }]);
    assert.ok(csv.includes(`"'${value}"`), JSON.stringify(value));
  }
  const csv = auditCSV([{ actor_display_name:'A "quoted", name', action:'ordinary', change_summary:{ before:'x', after:'y' } }]);
  assert.ok(csv.startsWith('\uFEFF'));
  assert.ok(csv.includes('"A ""quoted"", name"'));
  assert.ok(csv.includes('"ordinary"'));
  assert.ok(csv.includes('"{""before"":""x"",""after"":""y""}"'));
});

test('refund retries preserve identity and payment facts; a known success starts a new logical request', async t => {
  const client = new AdminPlatformClient();
  client.csrfToken = 'test-csrf';
  const captured = [];
  let attempt = 0;
  t.mock.method(globalThis, 'fetch', async (url, options) => {
    captured.push({ url, options });
    if (++attempt === 1) throw new Error('response lost');
    return new Response(JSON.stringify({ refund_id:'12345678-1234-4234-8234-123456789abc', payment_order_id:'test-order', payment_transaction_id:'test-transaction', entitlement_package_id:'test-package', reason:'duplicate charge' }), { status:200 });
  });
  const facts = ['test-order', 'test-transaction', 'test-package', 'duplicate charge'];
  const key = client.refundRequestKey(...facts);
  assert.match(key, /^[0-9a-f-]{36}$/u);
  await assert.rejects(client.refundPaymentOrder(...facts, key), /response lost/u);
  assert.equal(client.refundRequestKey(...facts), key);
  assert.notEqual(client.refundRequestKey('another-order', 'test-transaction', 'test-package', 'duplicate charge'), key);
  assert.notEqual(client.refundRequestKey('test-order', 'another-transaction', 'test-package', 'duplicate charge'), key);
  assert.notEqual(client.refundRequestKey('test-order', 'test-transaction', 'another-package', 'duplicate charge'), key);
  assert.notEqual(client.refundRequestKey('test-order', 'test-transaction', 'test-package', 'other reason'), key);
  await client.refundPaymentOrder(...facts, key);
  for (const { url, options } of captured) {
    assert.equal(url, '/api/admin/billing/admin/v1/payment-orders/test-order/refunds');
    assert.equal(options.headers['Idempotency-Key'], key);
    assert.equal(options.headers['X-CSRF-Token'], 'test-csrf');
    assert.deepEqual(JSON.parse(options.body), { payment_transaction_id:'test-transaction', entitlement_package_id:'test-package', reason:'duplicate charge' });
  }
  assert.notEqual(client.refundRequestKey(...facts), key);
  await assert.rejects(client.refundPaymentOrder(...facts, ''), /idempotency key required/u);
});

test('refund identity works when Safari 14 provides getRandomValues without randomUUID', t => {
  const safariCrypto = { getRandomValues: randomFillSync };
  t.mock.getter(globalThis, 'crypto', () => safariCrypto);
  assert.equal(safariCrypto.randomUUID, undefined);
  const client = new AdminPlatformClient();
  const key = client.refundRequestKey('order', 'transaction', 'package', 'reason');
  assert.match(key, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u);
  assert.equal(client.refundRequestKey('order', 'transaction', 'package', 'reason'), key);
  assert.notEqual(client.refundRequestKey('another-order', 'transaction', 'package', 'reason'), key);
});
