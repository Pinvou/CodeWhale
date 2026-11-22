import assert from 'node:assert/strict';
import test from 'node:test';
import { orderHistoryTimestamp, orderDurationDays, paymentOrderRemainingSeconds, isPayablePaymentOrder, paymentScanPrompt } from '../src/features/account-platform/order-view.js';
import { financeCopy } from '../src/shared/finance-i18n.js';

test('payment history uses the actual paid timestamp and otherwise the creation timestamp', () => {
  const created = '2026-09-01T00:00:00Z'; const paid = '2026-09-02T00:00:00Z';
  assert.equal(orderHistoryTimestamp({ created_at: created, paid_at: paid }), paid);
  assert.equal(orderHistoryTimestamp({ created_at: created, paid_at: 'invalid' }), created);
});
test('order duration comes from credited and validity facts and cannot invent missing validity', () => {
  assert.equal(orderDurationDays({ credited_at: '2026-09-01T00:00:00Z', valid_until: '2026-10-01T00:00:00Z' }), 30);
  assert.equal(orderDurationDays({ credited_at: '2026-09-01T00:00:00Z' }), null);
  assert.equal(orderDurationDays({ credited_at: '2026-09-02T00:00:00Z', valid_until: '2026-09-01T00:00:00Z' }), null);
});
test('payment countdown and actual action availability agree on the expiry boundary', () => {
  const now = Date.parse('2026-09-01T00:00:00Z');
  const item = { status: 'PENDING', close_status: 'OPEN', expires_at: '2026-09-01T00:10:00Z' };
  assert.equal(paymentOrderRemainingSeconds(item, now), 600);
  assert.equal(isPayablePaymentOrder(item, now), true);
  assert.equal(paymentOrderRemainingSeconds(item, now + 600000), 0);
  assert.equal(isPayablePaymentOrder(item, now + 600000), false);
  assert.equal(paymentOrderRemainingSeconds({ expires_at: 'invalid' }, now), 0);
});
for (const language of ['zh', 'en', 'ja']) test(`${language} scan prompt uses current provider apps without duplicate app labels`, () => {
  const copy = financeCopy[language];
  const prompt = paymentScanPrompt(copy, [' alipay ', 'ALIPAY', 'WECHAT']);
  assert.equal(prompt, copy.scanToPayWithApps([copy.channels.ALIPAY, copy.channels.WECHAT]));
  assert.equal(paymentScanPrompt(copy, []), copy.scanToPay);
  assert.equal(paymentScanPrompt(copy, null), copy.scanToPay);
});
