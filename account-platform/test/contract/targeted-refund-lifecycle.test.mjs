import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('ordinary refund is transaction and package targeted without amount input or proportional refund', async () => {
  const refunds = await read('internal/billing/refunds.go');
  const service = await read('internal/billing/service.go');
  assert.match(refunds, /CreateRefund\(ctx context\.Context, orderID, requesterID, transactionID, packageID uuid\.UUID/);
  assert.match(refunds, /transaction\.id=\$1 AND transaction\.payment_order_id=\$2 AND package\.id=\$3/);
  assert.match(refunds, /provider\.Refund\(ctx, merchantOrderNo, refund\.MerchantRefundNo, refund\.AmountFen\)/);
  assert.doesNotMatch(refunds, /proportionalCredits/);
  assert.doesNotMatch(service.match(/func \(s \*Service\) createRefund[\s\S]*?\n}/)?.[0] || '', /AmountFen|amount_fen/);
});

test('ordinary refund rejects consumed reserved expired revoked or anomalous entitlements', async () => {
  const refunds = await read('internal/billing/refunds.go');
  assert.match(refunds, /used_microcredits=0 AND reserved_microcredits=0 AND expired_microcredits=0 AND revoked_microcredits=0/);
  assert.match(refunds, /usage_hold_allocations/);
  assert.match(refunds, /chargeback_cases/);
  assert.match(refunds, /forced_refund_cases/);
  assert.match(refunds, /receivables/);
});

test('refund reservation atomically snapshots freezes and emits an outbox event', async () => {
  const refunds = await read('internal/billing/refunds.go');
  const migration = await read('migrations/billing/000027_targeted_refund_lifecycle.up.sql');
  assert.match(refunds, /pgx\.Serializable/);
  assert.match(refunds, /refund_component_reservations/);
  assert.match(refunds, /refund_grant_reservations/);
  assert.match(refunds, /status='FROZEN'/);
  assert.match(refunds, /billing\.refund\.requested\.v1/);
  assert.match(migration, /refunds_active_target_idx/);
});

test('refund provider execution is recovered by an idempotency-gated outbox consumer', async () => {
  const refunds = await read('internal/billing/refunds.go');
  const consumer = await read('internal/billing/refund_consumer.go');
  const provider = await read('internal/platform/payment/provider.go');
  const main = await read('cmd/billing-service/main.go');
  assert.match(refunds, /func \(p \*PaymentCoordinator\) ProcessRefund/);
  assert.match(refunds, /RefundIdempotencySupported/);
  assert.match(consumer, /billing\.refund\.requested\.v1/);
  assert.match(consumer, /ProcessRefund/);
  assert.match(provider, /RefundIdempotencySupported\s+bool/);
  assert.match(provider, /!c\.RefundSupported \|\| c\.RefundIdempotencySupported/);
  assert.match(main, /billing-refund-executor-v1/);
  assert.match(main, /NewRefundCommandConsumer/);
});

test('successful refund revokes all BASE and BONUS remainder with a balanced ledger entry', async () => {
  const refunds = await read('internal/billing/refunds.go');
  assert.match(refunds, /base_microcredits/);
  assert.match(refunds, /bonus_microcredits/);
  assert.match(refunds, /status='REVOKED'/);
  assert.match(refunds, /insertPosting\(ctx, tx, entryID, walletAccount, -amount/);
  assert.match(refunds, /insertPosting\(ctx, tx, entryID, refundClearingAccount, total/);
  assert.match(refunds, /billing\.refund\.succeeded\.v1/);
  assert.match(refunds, /conflicting refund evidence/);
  assert.doesNotMatch(refunds, /UPDATE payment_transactions SET status=.*REFUND/);
});

test('provider forced refund remains a separate evidence and review lifecycle', async () => {
  const forced = await read('internal/billing/forced_refunds.go');
  const migration = await read('migrations/billing/000027_targeted_refund_lifecycle.up.sql');
  assert.match(forced, /'FORCED_REFUND'/);
  assert.match(forced, /'FORCED_REFUND_CONFIRMED'/);
  assert.match(forced, /status='MANUAL_REVIEW'.*provider initiated a separate forced refund/);
  assert.match(forced, /attention_summary='REVIEW_REQUIRED'/);
  assert.doesNotMatch(forced, /UPDATE payment_transactions/);
  assert.match(migration, /CREATE TABLE forced_refund_cases/);
});

test('operations contracts expose exact targets and hide amount input', async () => {
  const billing = YAML.parse(await read('contracts/openapi/billing.yaml'));
  const admin = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const request = billing.components.schemas.TargetedRefundRequest;
  assert.deepEqual(request.required, ['requester_id', 'payment_transaction_id', 'entitlement_package_id', 'reason']);
  assert.equal(request.properties.amount_fen, undefined);
  assert.ok(billing.paths['/internal/v1/refunds/forced'].post);
  assert.ok(admin.paths['/billing/admin/v1/refunds'].get);
  assert.ok(admin.components.schemas.PaymentOrderDetail.properties.entitlement_packages);
});
