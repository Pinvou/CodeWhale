import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const accountPlatformRoot = path.resolve(import.meta.dirname, '../..');
const repositoryRoot = path.resolve(accountPlatformRoot, '..');
const read = (relative) => readFile(path.join(accountPlatformRoot, relative), 'utf8');
const readRepository = (relative) => readFile(path.join(repositoryRoot, relative), 'utf8');

function historyItemValidator(contract) {
  const document = JSON.parse(JSON.stringify({
    $schema: 'https://json-schema.org/draft/2020-12/schema',
    $ref: '#/$defs/RechargeOrderHistoryItem',
    $defs: contract.components.schemas,
  }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: true });
  addFormats(ajv);
  return ajv.compile(document);
}

const historyItem = {
  order_id: '11111111-1111-4111-8111-111111111111',
  pinvou_order_id: '11111111-1111-4111-8111-111111111111',
  provider_order_reference: 'provider-order-reference',
  product_id: 'PLAN_STANDARD',
  plan_name: 'purchase snapshot',
  sku_code: 'STANDARD',
  order_type: 'PLAN',
  amount_fen: 100,
  display_points: 100,
  channel: 'ALIPAY',
  status: 'PAID',
  fulfillment_status: 'CREDITED',
  close_status: 'OPEN',
  invoice_status: 'NOT_SUPPORTED',
  expires_at: '2026-09-28T01:00:00Z',
  created_at: '2026-09-28T00:00:00Z',
};

test('recharge order history keeps its snapshot and optionally exposes current localized names', async () => {
  const contract = YAML.parse(await read('contracts/openapi/billing.yaml'));
  const validate = historyItemValidator(contract);
  const localized = {
    ...historyItem,
    current_plan_name_zh_cn: '中文',
    current_plan_name_en: 'English',
    current_plan_name_ja: '日本語',
  };
  assert.equal(validate(localized), true, JSON.stringify(validate.errors));
  assert.equal(validate(historyItem), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...localized, unexpected: true }), false);
  assert.equal(contract.components.schemas.RechargeOrderHistoryItem.required.includes('plan_name'), true);
  for (const field of ['current_plan_name_zh_cn', 'current_plan_name_en', 'current_plan_name_ja']) {
    assert.equal(contract.components.schemas.RechargeOrderHistoryItem.required.includes(field), false);
  }
});

test('Billing projects current catalog names without overwriting the order purchase snapshot', async () => {
  const [model, query] = await Promise.all([
    read('internal/billing/payments.go'),
    read('internal/billing/refunds.go'),
  ]);
  for (const field of ['CurrentPlanNameZhCN', 'CurrentPlanNameEN', 'CurrentPlanNameJA']) assert.match(model, new RegExp(field));
  assert.match(query, /product_snapshot->>'plan_name'.*catalog\.display_name_zh_cn.*catalog\.display_name_en.*catalog\.display_name_ja/s);
  assert.match(query, /LEFT JOIN recharge_product_catalog_items catalog ON catalog\.product_id=po\.product_id/);
});

test('Web order list, pending payment, responsive card, and details re-evaluate the active language', async () => {
  const view = await readRepository('pinvou3-app/src/features/account-platform/AccountPlatformView.jsx');
  const orderView = await readRepository('pinvou3-app/src/features/account-platform/order-view.js');
  assert.match(view, /PendingPaymentOrders copy=\{copy\} language=\{language\}/);
  assert.ok((view.match(/paymentOrderDisplayName\(item, language\)/g) || []).length >= 5);
  assert.match(orderView, /current_plan_name_zh_cn/);
  assert.match(orderView, /current_plan_name_en/);
  assert.match(orderView, /current_plan_name_ja/);
  assert.match(orderView, /currentNames\[languageKey\], item\?\.plan_name, item\?\.product_id/);
});
