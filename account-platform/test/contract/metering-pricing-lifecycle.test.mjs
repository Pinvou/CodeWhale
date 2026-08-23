import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('versioned metering and pricing persist deterministic immutable facts', async () => {
  const [billingMigration, tokenMigration, resolution, lifecycle, metering] = await Promise.all([
    read('migrations/billing/000022_versioned_metering_pricing.up.sql'),
    read('migrations/token/000010_protocol_formula_versions.up.sql'),
    read('internal/billing/pricing_resolution.go'),
    read('internal/billing/pricing_admin_v2.go'),
    read('internal/token/metering.go'),
  ]);

  assert.match(billingMigration, /CREATE TABLE metering_unit_definitions/);
  assert.match(billingMigration, /FIXED_PRICE','INHERIT_GLOBAL/);
  assert.match(billingMigration, /CREATE TABLE pricing_lifecycle_commands/);
  assert.match(billingMigration, /ADD COLUMN pricing_snapshot JSONB/);
  assert.match(billingMigration, /ADD COLUMN request_snapshot JSONB/);
  assert.match(tokenMigration, /CREATE TABLE protocol_formula_versions/);
  assert.match(tokenMigration, /PUBLISHED','DEPRECATED/);
  assert.match(resolution, /ErrPriceAmbiguous/);
  assert.match(resolution, /configuration\.configType == "INHERIT_GLOBAL"/);
  assert.match(lifecycle, /lockTransactionKey\(ctx, tx, "pricing\.space"/);
  assert.match(lifecycle, /FALLBACK_GLOBAL/);
  assert.match(metering, /versioned, deterministic byte rule/);
});

test('gateway renews and extends the original hold before further delivery', async () => {
  const [proxy, billingClient, reconciliation] = await Promise.all([
    read('internal/token/proxy.go'),
    read('internal/token/billing.go'),
    read('internal/token/reconciliation.go'),
  ]);

  assert.match(proxy, /renewHold\(/);
  assert.match(proxy, /additional_preauthorization_failed/);
  assert.match(proxy, /client\.Extend\(/);
  assert.match(billingClient, /"additional_microcredits": additional/);
  assert.match(reconciliation, /PricingSnapshot/);
  assert.match(reconciliation, /PromptTokens - cached/);
});

test('admin contract exposes units, price lifecycle, protocols, and formulas', async () => {
  const [source, page, script] = await Promise.all([
    read('contracts/openapi/admin.yaml'),
    read('internal/portal/static/admin.html'),
    read('internal/portal/static/assets/pricing-admin.js'),
  ]);
  const contract = YAML.parse(source);

  for (const route of [
    '/billing/admin/v1/metering-units',
    '/billing/admin/v1/pricing',
    '/billing/admin/v1/pricing/{price_version_id}/publish',
    '/billing/admin/v1/pricing/{price_version_id}/transition',
    '/token/admin/v1/protocols',
    '/token/admin/v1/protocols/{protocol_definition_id}/formulas',
    '/token/admin/v1/formulas/{formula_version_id}/preview',
  ]) assert.ok(contract.paths[route], `missing admin route ${route}`);

  assert.match(page, /pricing-admin\.js/);
  for (const copy of ['计量、协议与价格生命周期', 'Metering, protocols, and pricing lifecycle', '計量・プロトコル・価格ライフサイクル']) assert.match(script, new RegExp(copy));
});
