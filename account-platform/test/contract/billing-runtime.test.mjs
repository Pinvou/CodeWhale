import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');

test('Billing and Token contracts match production JSON and HTTP validation responses', async () => {
  const result = spawnSync(process.env.PINVOU_ACCOUNT_GO_EXECUTABLE || (process.platform === 'win32' ? 'go.exe' : 'go'),
    ['run', '-mod=readonly', './test/contract/billing-fixtures'],
    { cwd: root, encoding: 'utf8', timeout: 120000 });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  const fixtures = JSON.parse(result.stdout);
  const document = YAML.parse(await readFile(path.join(root, 'contracts/openapi/billing.yaml'), 'utf8'));
  const ajv = new Ajv2020({ strict: true, allErrors: true });
  addFormats(ajv);
  ajv.addFormat('int64', { type: 'number', validate: Number.isInteger });
  const schemas = JSON.parse(JSON.stringify(document.components.schemas).replaceAll('#/components/schemas/', '#/$defs/'));
  ajv.addSchema({ $id: 'urn:pinvou:billing-runtime', $defs: schemas });
  for (const [name, schema] of [['product', 'RechargeProduct'], ['paid', 'RechargeOrder'], ['created', 'RechargeOrder'], ['wallet', 'WalletBalance'], ['recent_created', 'RecentRechargeOrder'], ['recent_empty', 'RecentRechargeOrder']]) {
    const validate = ajv.compile({ $ref: `urn:pinvou:billing-runtime#/$defs/${schema}` });
    assert.ok(validate(fixtures[name]), `${name}: ${JSON.stringify(validate.errors)}`);
    assert.equal(validate({ ...fixtures[name], unexpected: true }), false, 'unknown response fields remain forbidden');
  }
  for (const [operation, template] of [['post', '/recharge/orders'], ['get', '/recharge/orders/{order_id}'], ['get', '/recharge/orders/recent']]) {
    const item = document.paths[template];
    const base = (item.servers ?? document.servers)[0].url;
    const route = `${operation.toUpperCase()} ${base}${template.replace('{order_id}', fixtures.paid.order_id)}`;
    assert.equal(fixtures.routes[route], 400, `${route} must reach production request validation`);
  }
  assert.ok(document.paths['/recharge/orders'].post.responses['200']);
  assert.ok(document.paths['/recharge/orders/{order_id}'].get.parameters.some((p) => p.name === 'requester_id' && p.in === 'query' && p.required));
  const request = ajv.compile({ $ref: 'urn:pinvou:billing-runtime#/$defs/CreateRechargeOrderRequest' });
  const valid = { wallet_id: fixtures.wallet.wallet_id, requester_id: fixtures.paid.requester_id, product_id: 'CNY_1', channel: 'ALIPAY' };
  assert.ok(request(valid), JSON.stringify(request.errors));
  assert.equal(request({ ...valid, requester_id: undefined, requester_user_id: valid.requester_id }), false);

  const token = YAML.parse(await readFile(path.join(root, 'contracts/openapi/token-gateway.yaml'), 'utf8'));
  const common = YAML.parse(await readFile(path.join(root, 'contracts/openapi/common.yaml'), 'utf8'));
  const tokenSchemas = JSON.parse(JSON.stringify(token.components.schemas).replaceAll('#/components/schemas/', '#/$defs/'));
  ajv.addSchema({ $id: 'urn:pinvou:token-runtime', $defs: tokenSchemas });
  const eligibleSchema = JSON.parse(JSON.stringify(token.paths['/token-accounts/eligible'].get.responses['200'].content['application/json'].schema)
    .replaceAll('#/components/schemas/', 'urn:pinvou:token-runtime#/$defs/'));
  const eligible = ajv.compile(eligibleSchema);
  assert.ok(eligible(fixtures.eligible), JSON.stringify(eligible.errors));
  assert.equal(fixtures.eligible.membership_version, 7);
  assert.equal(fixtures.eligible.items.length, 1);
  assert.equal(eligible({ ...fixtures.eligible, membership_version: undefined }), false);
  const error = ajv.compile(common.components.schemas.Error);
  assert.equal(fixtures.rate_limited.code, 'RATE_LIMITED');
  assert.ok(error(fixtures.rate_limited), JSON.stringify(error.errors));
  for (const [code, response] of Object.entries(fixtures.token_errors)) {
    assert.equal(response.code, code);
    assert.ok(error(response), `${code}: ${JSON.stringify(error.errors)}`);
  }
  const admin = YAML.parse(await readFile(path.join(root, 'contracts/openapi/admin.yaml'), 'utf8'));
  const adminSchemas = JSON.parse(JSON.stringify(admin.components.schemas).replaceAll('#/components/schemas/', '#/$defs/'));
  ajv.addSchema({ $id: 'urn:pinvou:admin-runtime', $defs: adminSchemas });
  const financialSchemas = [
    ['billing/admin/v1/overview', 'BillingOverview', ['posted_microcredits', 'reserved_microcredits']],
    ['billing/admin/v1/wallets', 'WalletCollection', ['posted_microcredits', 'reserved_microcredits', 'available_microcredits']],
    ['billing/admin/v1/payment-orders', 'PaymentOrderCollection', ['amount_fen', 'credit_microcredits']],
    ['billing/admin/v1/adjustments', 'AdjustmentCollection', ['amount_microcredits']],
    ['billing/admin/v1/usage-holds', 'HoldCollection', ['max_microcredits', 'actual_microcredits']],
    ['token/admin/v1/usage-requests', 'UsageCollection', ['maximum_microcredits', 'actual_microcredits']],
  ];
  for (const [route, schema, fields] of financialSchemas) {
    const validate = ajv.compile({ $ref: `urn:pinvou:admin-runtime#/$defs/${schema}` });
    const response = fixtures.financial_reads[route];
    assert.ok(validate(response), `${route}: ${JSON.stringify(validate.errors)}`);
    const item = response.items?.[0] ?? response;
    for (const field of fields) {
      assert.equal(typeof item[field], 'string', `${route} ${field} must remain exact`);
      const malformed = structuredClone(response);
      (malformed.items?.[0] ?? malformed)[field] = Number(item[field]);
      assert.equal(validate(malformed), false, `${route} must reject rounded money numbers`);
    }
  }
  const history = ajv.compile({ $ref: 'urn:pinvou:token-runtime#/$defs/UserUsageRecord' });
  const historyItem = fixtures.financial_reads['token/v1/me/usage-requests'].items[0];
  assert.ok(history(historyItem), JSON.stringify(history.errors));
  assert.equal(historyItem.actual_microcredits, '9007199254740993');
  assert.equal(history({ ...historyItem, actual_microcredits: 9007199254740993 }), false);
  assert.ok(history({ ...historyItem, actual_microcredits: null }), JSON.stringify(history.errors));
  assert.equal(error({ ...Object.values(fixtures.token_errors)[0], code: 'UNDECLARED_ERROR' }), false);
});
