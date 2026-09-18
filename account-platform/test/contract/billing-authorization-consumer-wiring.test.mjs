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

test('Billing starts the dedicated entitlement durable only from an explicit trusted receipt', async () => {
  const source = await readFile(path.join(root, 'cmd/billing-service/main.go'), 'utf8');
  const durable = 'billing-entitlement-component-adjustment-executor-p2-v1';
  assert.match(source, /optionalBooleanEnvironment\("BILLING_ENTITLEMENT_CONSUMER_ENABLED", false\)/,
    'the dedicated durable must stay opt-in with a disabled default');
  assert.match(source, /ApprovalConsumerResources: approvalConsumerResources/,
    'the Q+1 start sequence must come from the verified resource binding');
  assert.match(source, new RegExp(`NewAuthorizationCommandConsumer\\(service, approvalGate, "${durable}"\\)`));
  assert.match(source, new RegExp(`ConsumeAuthorization\\(workerContext, "${durable}", entitlementConsumer\\.Handle\\)`));
  assert.match(source, /log\.Fatal\("BILLING_ENTITLEMENT_CONSUMER_RESOURCES_UNAVAILABLE"\)/,
    'a missing or untrusted binding must fail startup, not silently skip the durable');
  // The legacy shared durable must never become conditional on the new switch.
  const enabledBranch = source.slice(source.lastIndexOf('if entitlementConsumerEnabled {'));
  assert.ok(enabledBranch.includes(`ConsumeAuthorization(workerContext, "${durable}"`));
  assert.equal(enabledBranch.includes('"billing-command-executor-v1"'), false,
    'the legacy shared durable must keep starting unconditionally');

  const helper = await readFile(path.join(root, 'cmd/billing-service/approval_consumer.go'), 'utf8');
  for (const name of [
    'BILLING_APPROVAL_CONSUMER_PUBLIC_KEYS_FILE', 'BILLING_APPROVAL_CONSUMER_PUBLIC_KEYS_SHA256',
    'BILLING_APPROVAL_CONSUMER_MANIFEST_FILE', 'BILLING_APPROVAL_CONSUMER_MANIFEST_SHA256',
    'BILLING_APPROVAL_CONSUMER_RECEIPT_FILE', 'BILLING_APPROVAL_CONSUMER_RECEIPT_SHA256',
  ]) {
    assert.ok(helper.includes(`"${name}"`), `${name} must be a required consumer bootstrap input`);
  }
  assert.doesNotMatch(helper, /os\.Getenv\("(?!BILLING_APPROVAL_CONSUMER_|NATS_)/,
    'the bootstrap must not read unrelated configuration');
  assert.match(helper, /files\.Load\(now\)/);
});

test('the legacy generic authorization handler is permanently fail closed', async () => {
  const source = await readFile(path.join(root, 'internal/billing/approval_consumer.go'), 'utf8');
  assert.match(source, /generic approval authorization consumption is forbidden/);
  assert.doesNotMatch(source, /json\.Unmarshal/);
  assert.doesNotMatch(source, /NumDelivered|TermWithReason|consumer-dead-letter/);
});
