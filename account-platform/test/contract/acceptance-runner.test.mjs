import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { runInNewContext } from 'node:vm';
import { integerMoney } from '../../../pinvou3-app/src/features/account-platform/finance-contract.js';
import { shanghaiBusinessDate } from '../integration/shanghai-business-date.mjs';

const root = path.resolve(import.meta.dirname, '../../..');

test('E2E runner generates secrets and exercises financial idempotency', async () => {
  const runner = await readFile(path.join(root, 'scripts/account-platform.mjs'), 'utf8');
  const acceptance = await readFile(path.join(root, 'account-platform/test/integration/compose-acceptance.mjs'), 'utf8');
  const fault = await readFile(path.join(root, 'account-platform/test/integration/compose-fault.mjs'), 'utf8');

  assert.match(runner, /generatedComposeEnvironment/);
  assert.match(runner, /randomBytes\(32\)/);
  assert.match(runner, /const workloadCredential = readTestWorkloadCredential\(\)/);
  assert.match(runner, /INTERNAL_SERVICE_TOKEN: workloadCredential/);
  assert.doesNotMatch(runner, /INTERNAL_SERVICE_TOKEN: secret/);
  assert.match(acceptance, /authorization: `Bearer \$\{readTestWorkloadCredential\(\)\}`/);
  assert.match(fault, /authorization: `Bearer \$\{readTestWorkloadCredential\(\)\}`/);
  assert.match(runner, /compose-smoke\.mjs', 'compose-acceptance\.mjs/);
  assert.match(runner, /action === 'e2e'/);
  assert.match(acceptance, /\['UNIONPAY', 'ALIPAY', 'WECHAT'\]/);
  assert.match(acceptance, /length: 100/);
  assert.match(acceptance, /concurrent reserves must create one hold/);
  assert.match(acceptance, /concurrent commits must debit once/);
  assert.match(acceptance, /!Object\.hasOwn\(product, 'sku_code'\)/);
  assert.match(acceptance, /owner_id: organizationID/);
  assert.match(acceptance, /organizations\/\$\{organizationID\}\/entitlement-packages/);
  assert.match(acceptance, /refundablePackage\.points_used_microcredits\), 0n/);
  assert.doesNotMatch(acceptance, /refundablePackage\.used_microcredits/);
  assert.match(acceptance, /automated whole LEGACY refund acceptance/);
  assert.match(acceptance, /payment_transaction_id: refundablePackage\.source_payment_transaction_id/);
  assert.match(acceptance, /entitlement_package_id: refundablePackage\.package_id/);
  assert.match(acceptance, /'idempotency-key': refundKey/);
  assert.match(acceptance, /refund\.refund_id, requestedRefund\.refund_id/);
  assert.match(acceptance, /assert\.deepEqual\(refundReplay, refund/);
  assert.match(acceptance, /integerMoney\(refund\.amount_fen\), integerMoney\(refundableOrder\.amount_fen\)/);
  assert.match(acceptance, /whole refund must debit the selected unconsumed package exactly once/);
  assert.match(acceptance, /payment_transaction_id: packages\.items\[0\]\.source_payment_transaction_id/);
  assert.match(acceptance, /entitlement_package_id: packages\.items\[0\]\.package_id/);
  assert.match(acceptance, /statuses: \[409\]/);
  assert.match(acceptance, /deniedRefund\.code, 'BILLING_CONFLICT'/);
  assert.doesNotMatch(acceptance, /amount_fen: 1|partial-refund acceptance|statuses: \[402\]/);
  assert.match(acceptance, /reconciliation must close without mismatches/);
  assert.match(acceptance, /shanghaiBusinessDate\(new Date\(\)\)/);
  for (const [instant, expected] of [
    ['2026-10-06T15:59:59.999Z','2026-10-06'],
    ['2026-10-06T16:00:00.000Z','2026-10-07'],
    ['2026-10-07T15:59:59.999Z','2026-10-07'],
    ['2026-10-07T16:00:00.000Z','2026-10-08'],
  ]) assert.equal(shanghaiBusinessDate(new Date(instant)),expected);
  assert.doesNotMatch(acceptance, /replace-with-|Bearer [A-Za-z0-9_-]{20,}/);
  assert.match(fault, /stop', 'billing-service/);
  assert.match(fault, /start', 'billing-service/);
  assert.match(fault, /stop', 'nats/);
  assert.match(fault, /payment callback must converge after Billing restarts/);
  assert.doesNotMatch(fault, /replace-with-|Bearer [A-Za-z0-9_-]{20,}/);
});

test('acceptance full refund assertion compares exact amounts across order and refund DTO encodings', async () => {
  const acceptance = await readFile(path.join(root, 'account-platform/test/integration/compose-acceptance.mjs'), 'utf8');
  // Execute the whole receipt assertion block, including its DTO type guards,
  // without requiring a live payment service.
  const start = acceptance.indexOf('assert.equal(refund.refund_id,');
  const end = acceptance.indexOf('\nconst refundReplay', start);
  assert.ok(start >= 0 && end > start, 'runner must retain its refund receipt assertions');
  const statements = acceptance.slice(start, end);
  // PaymentOrder.AmountFen has json:"amount_fen"; Refund.AmountFen has
  // json:"amount_fen,string" (internal/billing/payments.go and refunds.go).
  const refundableOrder = JSON.parse('{"amount_fen":100}');
  const refund = JSON.parse('{"refund_id":"fixture-refund","payment_transaction_id":"fixture-transaction","entitlement_package_id":"fixture-package","amount_fen":"100"}');
  const refundablePackage = { source_payment_transaction_id: refund.payment_transaction_id, package_id: refund.entitlement_package_id };
  const compare = (receipt, order) => runInNewContext(statements, { assert, integerMoney, refund: { ...refund, ...receipt }, refundableOrder: order, requestedRefund: refund, refundablePackage });
  assert.doesNotThrow(() => compare(refund, refundableOrder));
  for (const amount of ['99', '101', '9007199254740993']) {
    assert.throws(() => compare({ amount_fen: amount }, refundableOrder), { code: 'ERR_ASSERTION' });
  }
  for (const amount of [[100], ['100'], [], {}, { value: 100 }, true, false, null]) {
    assert.throws(() => compare({ amount_fen: amount }, refundableOrder), { code: 'ERR_ASSERTION' });
    assert.throws(() => compare(refund, { amount_fen: amount }), { code: 'ERR_ASSERTION' });
  }
  assert.throws(() => compare({ amount_fen: 100 }, refundableOrder), { code: 'ERR_ASSERTION' });
  assert.throws(() => compare(refund, { amount_fen: '100' }), { code: 'ERR_ASSERTION' });
  // Reject an already rounded order number rather than accepting a false match.
  assert.throws(() => compare({ amount_fen: '9007199254740992' }, JSON.parse('{"amount_fen":9007199254740993}')), /INVALID_FINANCE_AMOUNT/);
});
