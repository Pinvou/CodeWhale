import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');

test('Billing wires the legacy authorization durable only through the raw consumer', async () => {
  const source = await readFile(path.join(root, 'cmd/billing-service/main.go'), 'utf8');
  assert.match(source, /NewAuthorizationCommandConsumer\(service, approvalGate, "billing-command-executor-v1"\)/);
  assert.match(source, /ConsumeAuthorization\(workerContext, "billing-command-executor-v1", approvalConsumer\.Handle\)/);
  assert.doesNotMatch(source, /NewApprovalCommandConsumer\(/);
  assert.doesNotMatch(source, /\.Consume\([^\n]*billing-command-executor-v1/);

  for (const durable of [
    'billing-wallet-adjustment-executor-p1-v1',
    'billing-payment-provider-resume-executor-p2-v1',
    'billing-payment-product-resume-executor-p1-v1',
  ]) {
    assert.equal(source.includes(`ConsumeAuthorization(workerContext, "${durable}"`), false,
      `${durable} must remain inactive until a trusted Q+1 receipt is supplied`);
  }

  assert.match(source, /\.Consume\(workerContext, "billing-refund-executor-v1", "billing\.refund\.requested\.v1"/,
    'the non-authorization refund consumer must retain its generic transport');
});

test('the legacy generic authorization handler is permanently fail closed', async () => {
  const source = await readFile(path.join(root, 'internal/billing/approval_consumer.go'), 'utf8');
  assert.match(source, /generic approval authorization consumption is forbidden/);
  assert.doesNotMatch(source, /json\.Unmarshal/);
  assert.doesNotMatch(source, /NumDelivered|TermWithReason|consumer-dead-letter/);
});
