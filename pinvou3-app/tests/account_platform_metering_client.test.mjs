import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { once } from 'node:events';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { adminPlatformCopy } from '../src/shared/admin-platform-i18n.js';

const ids = { protocol:'12000000-0000-4000-8000-000000000001', formula:'12000000-0000-4000-8000-000000000002', unit:'12000000-0000-4000-8000-000000000003', price:'12000000-0000-4000-8000-000000000004', order:'12000000-0000-4000-8000-000000000005', transaction:'12000000-0000-4000-8000-000000000006', package:'12000000-0000-4000-8000-000000000007', refund:'12000000-0000-4000-8000-000000000008' };

async function fixture(t, reply) {
  const requests = [];
  const server = http.createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += chunk;
    const item = { path:request.url, method:request.method, headers:request.headers, body:body ? JSON.parse(body) : null };
    requests.push(item);
    const result = reply(item, requests.length);
    response.writeHead(result.status ?? 200, { 'Content-Type':'application/json' });
    response.end(JSON.stringify(result.body));
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => new Promise(resolve => { server.closeAllConnections(); server.close(resolve); }));
  const client = new AdminPlatformClient(`http://127.0.0.1:${server.address().port}`);
  client.csrfToken = 'controlled-session-proof';
  return { client, requests };
}

test('protocol draft unknown response retains a UUID; acknowledgement releases it for a deliberate new version', async t => {
  const input = { protocol_code:'CHAT', request_schema:{ parameters:['request_count'] } };
  const { client, requests } = await fixture(t, (_, count) => ({ body:count === 1 ? {} : { ...input, protocol_definition_id:ids.protocol, status:'DRAFT' } }));
  await assert.rejects(client.createProtocol(input), { code:'REQUEST_FAILED' });
  await client.createProtocol(input);
  await client.createProtocol(input);
  const keys = requests.map(value => value.headers['idempotency-key']);
  assert.match(keys[0], /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  assert.equal(keys[0], keys[1]);
  assert.notEqual(keys[1], keys[2]);
  assert.deepEqual(requests[0].body, input);
});

test('formula draft rejects foreign facts and retries the same logical key', async t => {
  const input = { required_unit_versions:{ REQUEST:ids.unit }, expression:{ REQUEST:'request_count' } };
  const { client, requests } = await fixture(t, (_, count) => ({ body:{ ...input, formula_version_id:ids.formula, protocol_definition_id:count === 1 ? ids.price : ids.protocol, status:'DRAFT' } }));
  await assert.rejects(client.createFormula(ids.protocol, input), { code:'REQUEST_FAILED' });
  await client.createFormula(ids.protocol, input);
  assert.equal(requests[0].headers['idempotency-key'], requests[1].headers['idempotency-key']);
  assert.equal(requests[0].path, `/api/admin/token/admin/v1/protocols/${ids.protocol}/formulas`);
});

for (const [kind, invoke, field, id] of [
  ['protocol', client => client.publishProtocol(ids.protocol), 'protocol_definition_id', ids.protocol],
  ['formula', client => client.publishFormula(ids.formula), 'formula_version_id', ids.formula],
  ['price', client => client.publishPrice(ids.price, 7), 'price_version_id', ids.price],
]) test(`${kind} publish rejects unknown acknowledgement and preserves key across 503`, async t => {
  const { client, requests } = await fixture(t, (_, count) => count === 1 ? { status:503, body:{code:'SERVICE_UNAVAILABLE'} } : { body:{ [field]:id, status:'PUBLISHED', lifecycle_status:'PUBLISHED' } });
  await assert.rejects(invoke(client), { status:503 });
  await invoke(client);
  assert.equal(requests[0].headers['idempotency-key'], requests[1].headers['idempotency-key']);
  assert.equal(requests[1].headers['x-csrf-token'], 'controlled-session-proof');
});

test('changing immutable facts or authenticated session never borrows a pending logical key', async t => {
  const { client, requests } = await fixture(t, () => ({ body:{} }));
  await assert.rejects(client.createProtocol({ protocol_code:'CHAT', request_schema:{} }));
  await assert.rejects(client.createProtocol({ protocol_code:'IMAGE', request_schema:{} }));
  client.csrfToken = 'another-controlled-session';
  await assert.rejects(client.createProtocol({ protocol_code:'IMAGE', request_schema:{} }));
  assert.equal(new Set(requests.map(value => value.headers['idempotency-key'])).size, 3);
});

test('metering unit creation transmits safe integer metadata and retains key on missing facts', async t => {
  const input = { unit_code:'REQUEST', minimum_precision:1, price_denominator:1000000, rounding_rule:'CEILING' };
  const { client, requests } = await fixture(t, (_, count) => ({ body:count === 1 ? { unit_definition_id:ids.unit } : { ...input, unit_definition_id:ids.unit, status:'ACTIVE' } }));
  await assert.rejects(client.createMeteringUnit(input), { code:'REQUEST_FAILED' });
  await client.createMeteringUnit(input);
  assert.equal(requests[0].headers['idempotency-key'], requests[1].headers['idempotency-key']);
  assert.deepEqual(requests[1].body, input);
});

test('targeted refund sends transaction and package rather than a user-selected amount; foreign ACK retains key', async t => {
  const { client, requests } = await fixture(t, (_, count) => ({ body:{ refund_id:ids.refund, payment_order_id:ids.order, payment_transaction_id:ids.transaction, entitlement_package_id:count === 1 ? ids.unit : ids.package, reason:'controlled refund reason' } }));
  const key = client.refundRequestKey(ids.order, ids.transaction, ids.package, 'controlled refund reason');
  await assert.rejects(client.refundPaymentOrder(ids.order, ids.transaction, ids.package, 'controlled refund reason', key), { code:'REQUEST_FAILED' });
  assert.equal(client.refundRequestKey(ids.order, ids.transaction, ids.package, 'controlled refund reason'), key);
  await client.refundPaymentOrder(ids.order, ids.transaction, ids.package, 'controlled refund reason', key);
  assert.notEqual(client.refundRequestKey(ids.order, ids.transaction, ids.package, 'controlled refund reason'), key);
  assert.deepEqual(requests[0].body, { payment_transaction_id:ids.transaction, entitlement_package_id:ids.package, reason:'controlled refund reason' });
  assert.equal('amount_fen' in requests[0].body, false);
});

test('new lifecycle and financial labels have explicit three-language entries', () => {
  for (const copy of Object.values(adminPlatformCopy)) {
    for (const key of Object.keys(adminPlatformCopy.zh.pricing)) assert.equal(typeof copy.pricing[key], 'string');
    for (const key of ['metering','receivables','lateReviews','anomalies','refunds','providerSnapshot','holdEvidence']) assert.equal(typeof copy.billing[key], 'string');
  }
});

test('price draft rejects incomplete or changed pricing facts and numeric read rates without releasing its logical key', async t => {
  const input = { scope_type:'MODEL', scope_id:'controlled-model', protocol_code:'CHAT', formula_version_id:ids.formula, credit_unit:'MICROCREDIT', effective_from:'2030-01-01T00:00:00+08:00', effective_to:null, expected_etag:1, units:[{ unit_definition_id:ids.unit, config_type:'FIXED_PRICE', fixed_price_per_denominator:0 }] };
  const complete = { ...input, price_version_id:ids.price, lifecycle_status:'DRAFT', version:1, space_etag:1, effective_from:'2029-12-31T16:00:00Z', units:[{ ...input.units[0], fixed_price_per_denominator:'0' }] };
  const replies = [ { ...complete, units:[] }, { ...complete, units:input.units }, { ...complete, effective_from:'2030-01-02T00:00:00+08:00' }, { ...complete, price_version_id:'00000000-0000-0000-0000-000000000000' }, complete ];
  const { client, requests } = await fixture(t, (_, count) => ({ body:replies[count - 1] }));
  for (let index = 0; index < 4; index += 1) await assert.rejects(client.createPriceDraft(input), { code:'REQUEST_FAILED' });
  await client.createPriceDraft(input);
  assert.equal(new Set(requests.map(value => value.headers['idempotency-key'])).size, 1);
  assert.deepEqual(requests[0].body, input);
});

for (const update of [false, true]) test(`price draft ${update ? 'update' : 'create'} requires its exact namespace ETag acknowledgement`, async t => {
  const input = { scope_type:'MODEL', scope_id:'controlled-model', protocol_code:'CHAT', formula_version_id:ids.formula, credit_unit:'MICROCREDIT', effective_from:'2030-01-01T00:00:00Z', effective_to:null, expected_etag:7, units:[{ unit_definition_id:ids.unit, config_type:'INHERIT_GLOBAL' }] };
  const expected = update ? 8 : 7;
  const complete = { ...input, price_version_id:ids.price, lifecycle_status:'DRAFT', version:2, space_etag:expected };
  const { client, requests } = await fixture(t, (_, count) => ({ body:count < 3 ? { ...complete, space_etag:count === 1 ? expected + 1 : expected - 1 } : complete }));
  const invoke = () => update ? client.updatePriceDraft(ids.price, input) : client.createPriceDraft(input);
  await assert.rejects(invoke(), { code:'REQUEST_FAILED' });
  await assert.rejects(invoke(), { code:'REQUEST_FAILED' });
  assert.equal((await invoke()).space_etag, expected);
  // An immutable successful replay remains valid even if another actor later advances the namespace.
  assert.equal((await invoke()).space_etag, expected);
  assert.ok(requests.every(value => value.body.expected_etag === 7));
  if (!update) assert.equal(requests[0].headers['idempotency-key'], requests[2].headers['idempotency-key']);
});
