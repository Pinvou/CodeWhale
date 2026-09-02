import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

const endpoints = {
  accounts: ['platform.billing.wallets.read', 'FinanceAccountsOverview'],
  payments: ['platform.billing.wallets.read', 'FinancePaymentsOverview'],
  pricing: ['platform.pricing.read', 'FinancePricingOverview'],
  products: ['platform.products.read', 'FinanceProductsOverview'],
};

test('finance overviews expose independent permissions and atomic response contracts', async () => {
  const [service, portal, contractSource] = await Promise.all([
    read('internal/billing/service.go'),
    read('internal/portal/service.go'),
    read('contracts/openapi/admin.yaml'),
  ]);
  const contract = YAML.parse(contractSource);
  for (const [resource, [permission, response]] of Object.entries(endpoints)) {
    const route = `/billing/admin/v1/finance-overviews/${resource}`;
    const operation = contract.paths[route]?.get;
    assert.ok(operation, `${route} must publish GET`);
    assert.equal(operation.responses['200'].$ref, `#/components/responses/${response}`);
    for (const status of ['400', '403', '503']) assert.ok(operation.responses[status], `${route} must publish ${status}`);
    assert.match(service, new RegExp(`admin\\.GET\\("/finance-overviews/${resource}", s\\.authorizer\\.Require\\("${permission.replaceAll('.', '\\.')}"\\)`));
    assert.match(portal, new RegExp(`"/admin/v1/finance-overviews/${resource}"`));
  }
  assert.equal(contract.components.responses.FinanceOverviewUnavailable.content['application/json'].schema.properties.code.const, 'FINANCE_OVERVIEW_UNAVAILABLE');
});

test('finance overview schemas fix metric keys, units, status sets, and time windows', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const expectations = {
    FinanceAccountsMetric: {
      wallet_count: ['COUNT', ['ACTIVE', 'FROZEN', 'CLOSED']],
      posted_microcredits: ['MICROCREDITS', ['ACTIVE', 'FROZEN', 'CLOSED']],
      reserved_microcredits: ['MICROCREDITS', ['ACTIVE', 'FROZEN', 'CLOSED']],
      available_microcredits: ['MICROCREDITS', ['ACTIVE', 'FROZEN', 'CLOSED']],
      receivable_microcredits: ['MICROCREDITS', ['ACTIVE', 'FROZEN', 'CLOSED']],
      pending_adjustment_count: ['COUNT', ['PENDING_APPROVAL']],
      at_risk_hold_count: ['COUNT', ['ACTIVE', 'REVIEW']],
    },
    FinancePaymentsMetric: {
      orders_created_count: ['COUNT', ['CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'EXPIRED', 'MANUAL_REVIEW']],
      payments_received_fen: ['CNY_FEN', ['PAID', 'LATE_PAID']],
      late_review_open_count: ['COUNT', ['REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND']],
      payment_anomaly_open_count: ['COUNT', ['OPEN']],
      reconciliation_open_count: ['COUNT', ['OPEN', 'IN_REVIEW']],
      refund_attention_count: ['COUNT', ['FAILED', 'MANUAL_REVIEW']],
      provider_controlled_count: ['COUNT', ['NEW_ORDERS_DISABLED']],
      product_controlled_count: ['COUNT', ['NEW_ORDERS_DISABLED']],
    },
    FinancePricingMetric: {
      price_draft_count: ['COUNT', ['DRAFT']],
      price_future_count: ['COUNT', ['PUBLISHED']],
      price_active_count: ['COUNT', ['ACTIVE']],
      price_ended_or_cancelled_count: ['COUNT', ['ENDED', 'CANCELLED']],
      metering_active_count: ['COUNT', ['ACTIVE']],
      metering_inactive_count: ['COUNT', ['INACTIVE']],
    },
    FinanceProductsMetric: {
      product_draft_count: ['COUNT', ['DRAFT']],
      product_on_sale_count: ['COUNT', ['ON_SALE']],
      product_off_sale_count: ['COUNT', ['OFF_SALE']],
      product_deleted_count: ['COUNT', ['DELETED']],
      admission_pending_count: ['COUNT', ['PENDING']],
      admission_blocked_count: ['COUNT', ['MISSING', 'FAILED', 'EXPIRED', 'REVOKED']],
    },
  };
  for (const [schemaName, metrics] of Object.entries(expectations)) {
    const variants = contract.components.schemas[schemaName].allOf[1].oneOf;
    assert.deepEqual(Object.fromEntries(variants.map(variant => [
      variant.properties.key.const,
      [variant.properties.unit.const, variant.properties.included_statuses.const],
    ])), metrics);
  }
  assert.equal(contract.components.schemas.FinanceAccountsOverview.properties.window.type, 'null');
  assert.equal(contract.components.schemas.FinancePricingOverview.properties.window.type, 'null');
  assert.equal(contract.components.schemas.FinanceProductsOverview.properties.window.type, 'null');
  assert.equal(contract.components.schemas.FinanceOverviewWindow.properties.timezone.const, 'UTC');
  const paymentParameters = contract.paths['/billing/admin/v1/finance-overviews/payments'].get.parameters.map(parameter => parameter.name);
  assert.deepEqual(paymentParameters, ['from', 'to']);
});

test('finance overview queries preserve boundary, deduplication, tombstone, and admission semantics', async () => {
  const implementation = await read('internal/billing/finance_overviews.go');
  assert.match(implementation, /created_at>=\$1 AND created_at<\$2/);
  assert.match(implementation, /status='REVIEW' OR \(status='ACTIVE' AND expires_at<=\$1\)/);
  assert.match(implementation, /COALESCE\(provider_paid_at,verified_at\)>=\$1 AND COALESCE\(provider_paid_at,verified_at\)<\$2/);
  assert.doesNotMatch(implementation, /JOIN payment_evidence/);
  assert.match(implementation, /count\(\*\) FILTER \(WHERE lifecycle_status='DELETED'\)/);
  assert.match(implementation, /lifecycle_status<>'DELETED' AND latest_status='PENDING'/);
  assert.match(implementation, /COALESCE\(latest_status,''\)<>'PENDING'/);
  assert.match(implementation, /latest_status IS NULL OR latest_status='FAILED'/);
  assert.match(implementation, /lifecycle_status='ON_SALE' AND current_evidence_id IS NULL/);
  assert.match(implementation, /latest_expires_at<=\$1 OR latest_revoked/);
  assert.match(implementation, /ORDER BY requested_at DESC,verification_id DESC/);
  assert.match(implementation, /FINANCE_OVERVIEW_UNAVAILABLE/);
});
