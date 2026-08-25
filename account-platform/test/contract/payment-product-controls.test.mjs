import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const repoRoot = path.resolve(root, '..');
const read = relative => readFile(path.join(root, relative), 'utf8');
const readRepo = relative => readFile(path.join(repoRoot, relative), 'utf8');

test('payment product safety controls are separate from catalog publication', async () => {
  const migration = await read('migrations/billing/000039_payment_product_controls.up.sql');
  const reviews = await read('internal/billing/late_payment_reviews.go');
  const payments = await read('internal/billing/payments.go');
  const repository = await read('internal/billing/postgres.go');

  assert.match(migration, /CREATE TABLE payment_product_controls/);
  assert.match(migration, /new_orders_enabled BOOLEAN NOT NULL DEFAULT TRUE/);
  assert.match(migration, /AUTOMATIC_STOP_LOSS/);
  assert.doesNotMatch(reviews, /UPDATE recharge_products[\s\S]{0,160}enabled = FALSE/);
  assert.match(reviews, /INSERT INTO payment_product_controls/);
  assert.match(payments, /payment_product_controls/);
  assert.match(payments, /if !productOrdersEnabled \{[\s\S]{0,100}ErrConflict/);
  assert.match(repository, /PROVIDER_PAUSED/);
  assert.match(repository, /PRODUCT_PAUSED/);
});

test('paused products remain visible but cannot be purchased', async () => {
  const openapi = await read('contracts/openapi/billing.yaml');
  const portal = await readRepo('pinvou3-app/src/features/account-platform/AccountPlatformView.jsx');
  const popover = await readRepo('pinvou3-app/src/features/account-platform/AccountPlatformPopover.jsx');
  const i18n = await readRepo('pinvou3-app/src/shared/account-platform-i18n.js');

  assert.match(openapi, /availability_status:[\s\S]*PROVIDER_PAUSED[\s\S]*PRODUCT_PAUSED/);
  assert.match(portal, /availability_status === 'AVAILABLE'/);
  assert.match(portal, /paymentChannelMaintenance/);
  assert.match(popover, /disabled=\{product\.availability_status !== 'AVAILABLE'\}/);
  assert.match(popover, /!selectedAvailable/);
  assert.match(i18n, /paymentChannelMaintenance:'支付渠道维护中，请稍后重试。'/);
});
