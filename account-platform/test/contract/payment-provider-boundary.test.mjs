import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import YAML from 'yaml';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('billing depends only on the provider-neutral payment port', async () => {
  const billing = await read('internal/billing/payments.go');
  const port = await read('internal/platform/payment/provider.go');
  const h3c = await read('internal/platform/h3cpayment/provider.go');
  const simulator = await read('internal/platform/paymentsimulator/provider.go');
  assert.doesNotMatch(billing, /h3cmall|paymentsim/i);
  assert.match(billing, /internal\/platform\/payment/);
  assert.match(port, /type Provider interface/);
  assert.match(h3c, /payment\.Capabilities/);
  assert.match(simulator, /TestOnly:\s+true/);
});

test('provider capability contract is versioned and externally discoverable', async () => {
  const billing = YAML.parse(await read('contracts/openapi/billing.yaml'));
  const gateway = YAML.parse(await read('contracts/openapi/token-gateway.yaml'));
  const schema = billing.components.schemas.PaymentCapabilities;
  assert.ok(billing.paths['/payment-capabilities'].get);
  assert.ok(gateway.paths['/payment-capabilities'].get);
  for (const field of ['provider_id', 'version', 'channels', 'order_query_supported', 'order_history_retention_seconds', 'minimum_query_interval_seconds', 'maximum_payable_window_seconds', 'maximum_late_confirmation_seconds', 'authoritative_unpaid_terminal', 'refund_supported', 'refund_idempotency_supported', 'close_supported', 'notification_supported', 'bill_supported', 'bill_file_import_supported', 'reconciliation_mode', 'formal_collection_admitted']) {
    assert.ok(schema.required.includes(field), field);
  }
  assert.equal(billing.components.schemas.PaymentChannel.enum, undefined);
  assert.equal(billing.components.schemas.RechargeProduct.properties.sku_code, undefined);
});

test('product mapping and order snapshots are immutable provider facts', async () => {
  const migration = await read('migrations/billing/000023_payment_provider_catalog.up.sql');
  const payments = await read('internal/billing/payments.go');
  assert.match(migration, /CREATE TABLE payment_product_mappings/);
  assert.match(migration, /reject_payment_mapping_mutation/);
  assert.match(migration, /ADD COLUMN product_mapping_snapshot JSONB NOT NULL/);
  assert.match(migration, /ADD COLUMN provider_capability_snapshot JSONB NOT NULL/);
  assert.match(migration, /UPDATE recharge_products SET sku_code=''/);
  assert.match(payments, /product_mapping_id,product_mapping_snapshot,provider_capability_snapshot/);
});

test('development simulator is unavailable to production configuration', async () => {
  const main = await read('cmd/billing-service/main.go');
  const helm = await read('deploy/helm/account-platform/templates/app-deployments.yaml');
  const values = await read('deploy/helm/account-platform/values.yaml');
  const view = await read('../pinvou3-app/src/features/account-platform/AccountPlatformView.jsx');
  assert.match(main, /development payment provider is forbidden outside development or test/);
  assert.doesNotMatch(helm, /PAYMENT_SIMULATOR_URL|PAYMENT_SIM_HMAC/);
  assert.match(values, /paymentProvider: h3c_mall/);
  assert.match(values, /allowTentativePrices: false/);
  assert.doesNotMatch(view, /onPurchase\(productId, 'ALIPAY'\)/);
  assert.match(view, /availableChannels\.map/);
});
