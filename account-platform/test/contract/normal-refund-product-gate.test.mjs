import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('normal refund creation is guarded independently from provider capability', async () => {
  const [payments, refunds, service, admin] = await Promise.all([
    read('internal/billing/payments.go'),
    read('internal/billing/refunds.go'),
    read('internal/billing/service.go'),
    read('internal/billing/admin.go'),
  ]);

  assert.match(payments, /normalRefundCreationEnabled bool/);
  assert.match(payments, /NormalRefundCreationEnabled bool `json:"normal_refund_creation_enabled"`/);
  assert.match(refunds.match(/func \(p \*PaymentCoordinator\) CreateRefund[\s\S]*?\n}/)?.[0] || '', /NormalRefundCreationEnabled/);
  assert.match(service.match(/func \(s \*Service\) createRefund[\s\S]*?\n}/)?.[0] || '', /ErrNormalRefundCreationDisabled/);
  assert.match(admin.match(/func \(s \*Service\) adminRefundPaymentOrder[\s\S]*?\n}/)?.[0] || '', /NormalRefundCreationEnabled/);
  assert.match(service, /NORMAL_REFUND_CREATION_DISABLED/);
});

test('default-off gate does not stop existing refund convergence or forced-refund evidence', async () => {
  const [refunds, consumer, forced, service] = await Promise.all([
    read('internal/billing/refunds.go'),
    read('internal/billing/refund_consumer.go'),
    read('internal/billing/forced_refunds.go'),
    read('internal/billing/service.go'),
  ]);

  const processRefund = refunds.match(/func \(p \*PaymentCoordinator\) ProcessRefund[\s\S]*?\n}/)?.[0] || '';
  assert.doesNotMatch(processRefund, /NormalRefundCreationEnabled|normalRefundCreationEnabled/);
  assert.match(consumer, /ProcessRefund/);
  assert.match(forced, /RecordAndFreezeForcedRefund/);
  assert.match(service, /internal\/v1\/refunds\/forced/);
  assert.match(service, /admin\.GET\("\/refunds"/);
});

test('all shipped environments keep normal refund creation disabled by default', async () => {
  const [main, compose, remoteCompose, helmTemplate, helmValues] = await Promise.all([
    read('cmd/billing-service/main.go'),
    read('deploy/compose/compose.yaml'),
    read('deploy/compose/compose.remote.yaml'),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
    read('deploy/helm/account-platform/values.yaml'),
  ]);

  assert.match(main, /optionalBooleanEnvironment\("BILLING_NORMAL_REFUND_CREATION_ENABLED", false\)/);
  assert.match(compose, /BILLING_NORMAL_REFUND_CREATION_ENABLED: \$\{BILLING_NORMAL_REFUND_CREATION_ENABLED:-false}/);
  assert.match(remoteCompose, /BILLING_NORMAL_REFUND_CREATION_ENABLED: "false"/);
  assert.match(helmTemplate, /BILLING_NORMAL_REFUND_CREATION_ENABLED/);
  assert.equal(YAML.parse(helmValues).billing.normalRefundCreationEnabled, false);
});

test('capabilities and administration UI require both product and provider refund gates', async () => {
  const [contract, adminView, accountView] = await Promise.all([
    read('contracts/openapi/billing.yaml'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/AccountPlatformView.jsx'),
  ]);
  const schema = YAML.parse(contract).components.schemas.PaymentCapabilities;

  assert.ok(schema.required.includes('normal_refund_creation_enabled'));
  assert.equal(schema.properties.normal_refund_creation_enabled.type, 'boolean');
  assert.match(adminView, /normal_refund_creation_enabled === true\s*&&\s*paymentCapabilities\.refund_supported === true/);
  assert.doesNotMatch(accountView, /refundPaymentOrder|createRefund|refundWholePackage/);
});
