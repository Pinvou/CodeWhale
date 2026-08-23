import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('model admission atomically covers every mandatory dimension before Hold and upstream dispatch', async () => {
  const [admission, repository, service, contract] = await Promise.all([
    read('internal/token/admission.go'),
    read('internal/token/postgres.go'),
    read('internal/token/service.go'),
    read('contracts/openapi/token-gateway.yaml').then(YAML.parse),
  ]);
  for (const fragment of [
    'userMinute >= 60', 'userInflight >= 5', 'memberInflight >= 5',
    'organizationMinute >= 600', 'organizationInflight >= 100',
    'modelMinute >= 300', 'routeMinute >= 300', 'globalMinute >= 1000',
  ]) assert.match(admission, new RegExp(fragment));
  assert.match(admission, /pg_advisory_xact_lock/);
  assert.doesNotMatch(admission.replace(/usageInflightStates[^\n]+/, ''), /Hold|upstream/i);
  assert.ok(repository.indexOf('admitUsage(') < repository.indexOf('INSERT INTO usage_requests'));
  assert.match(service, /ErrRateLimited/);
  assert.match(service, /StatusTooManyRequests/);
  assert.match(service, /Retry-After/);
  assert.ok(contract.paths['/chat/completions'].post.responses['429']);
});

test('payment admission enforces payer organization source query refund and provider dimensions', async () => {
  const [admission, service, contract] = await Promise.all([
    read('internal/billing/payment_admission.go'),
    read('internal/billing/service.go'),
    read('contracts/openapi/billing.yaml').then(YAML.parse),
  ]);
  for (const fragment of [
    'payerMinute >= 5', 'payerDay >= 20', 'organizationHour >= 30',
    'sourceMinute >= 20', 'providerMinute >= 80', 'payerHour >= 3', 'count >= 12',
  ]) assert.match(admission, new RegExp(fragment));
  assert.match(admission, /max\(5\*time\.Second, time\.Duration\(p\.Capabilities\(\)\.MinimumQueryIntervalSeconds\)\*time\.Second\)/);
  assert.match(service, /ErrPaymentRateLimited[\s\S]*Retry-After/);
  assert.ok(contract.paths['/recharge/orders'].post.responses['429']);
  assert.ok(contract.paths['/recharge/orders/{order_id}'].get.responses['429']);
  assert.ok(contract.paths['/internal/v1/recharge/orders/{order_id}/refunds'].post.responses['429']);
});

test('valid provider callbacks persist before acknowledgement and process outside user limits', async () => {
  const [payments, service, worker, migration, contract] = await Promise.all([
    read('internal/billing/payments.go'),
    read('internal/billing/service.go'),
    read('internal/billing/payment_notification_worker.go'),
    read('migrations/billing/000031_payment_risk_admission.up.sql'),
    read('contracts/openapi/billing.yaml').then(YAML.parse),
  ]);
  assert.match(payments, /ConstantTimeCompare[\s\S]*INSERT INTO payment_notifications[\s\S]*action='CALLBACK'/);
  assert.match(service, /PersistNotification[\s\S]*http\.StatusAccepted/);
  assert.doesNotMatch(service.match(/func \(s \*Service\) paymentNotification[\s\S]*?\n}/)?.[0] ?? '', /AdmitCreate|AdmitRefund|AdmitOrderQuery/);
  assert.match(worker, /result='RECEIVED'[\s\S]*LIMIT 200[\s\S]*ProcessPersistedNotification/);
  assert.match(migration, /action IN \('CREATE','ORDER_QUERY','REFUND','CALLBACK'\)[\s\S]*provider_dispatched/);
  assert.equal(contract.paths['/internal/payment/notifications'].post.responses['202'].description.includes('durably persisted'), true);
});
