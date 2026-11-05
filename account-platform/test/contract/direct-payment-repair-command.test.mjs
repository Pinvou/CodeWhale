import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { AdminPlatformClient } from '../../../pinvou3-app/src/features/account-platform/admin-client.js';
import { integerMoney } from '../../../pinvou3-app/src/features/account-platform/finance-contract.js';
const source = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AdminFinanceWorkspace.jsx', import.meta.url), 'utf8');
const repairStart = source.indexOf('  const repair = async () => {');
const refundStart = source.indexOf('  const refund = async () => {', repairStart);
const end = source.indexOf('  return <section>', refundStart);
assert.ok(repairStart > 0 && refundStart > repairStart && end > refundStart);
function callbacks(operation, client, detail, reason) {
  return new Function('operation', 'client', 'detail', 'reason', 'targets', 'targetID', 'refundEnabled', 'permissions', 'copy', 'integerMoney', 'open', 'load', `${source.slice(repairStart, end)}\nreturn {repair, refund};`)(operation, client, detail, reason, [], '', false, [], {}, integerMoney, async () => {}, async () => {});
}
test('the actual initial repair and UNKNOWN retry callbacks carry one explicit protocol/key/facts', async () => {
  const client = new AdminPlatformClient(''), key = 'finance-original-direct-repair-1111', calls = [];
  let original;
  const operation = { unknown: false, pendingFacts: null, async mutate(facts, invoke) { original ||= facts; return invoke(facts, key); } };
  const prior = globalThis.fetch;
  globalThis.fetch = async (url, options) => {
    calls.push({ url, key: options.headers['Idempotency-Key'], body: options.body });
    if (calls.length === 1) throw new TypeError('controlled committed repair ACK lost');
    return new Response(JSON.stringify({ order_id: 'original-order' }), { status: 200 });
  };
  try {
    await assert.rejects(callbacks(operation, client, { order_id: 'original-order' }, ' original reason ').repair(), TypeError);
    assert.equal(original.action, 'repair'); assert.equal(original.commandProtocol, 1);
    operation.unknown = true; operation.pendingFacts = Object.freeze(original);
    await callbacks(operation, client, { order_id: 'different-order' }, 'different form reason').refund();
    assert.equal(calls.length, 2); assert.deepEqual(calls.map(x => x.key), [key, key]); assert.equal(calls[0].body, calls[1].body);
    assert.deepEqual(JSON.parse(calls[0].body), { reason: 'original reason', command_protocol: 1 });
    assert.match(calls[1].url, /original-order\/repair$/);
  } finally { globalThis.fetch = prior; }
});
test('an unversioned old pending repair is never upgraded by the real retry callback', async () => {
  const old = Object.freeze({ action: 'repair', orderID: 'old-order', reason: 'old original reason' });
  const operation = { unknown: true, pendingFacts: old, async mutate(facts, invoke) { assert.equal(facts, old); return invoke(facts, 'old-original-repair-key'); } };
  const prior = globalThis.fetch; let calls = 0; globalThis.fetch = async () => { calls++; throw new Error('unexpected dispatch'); };
  try { await assert.rejects(callbacks(operation, new AdminPlatformClient(''), { order_id: 'new-order' }, 'new reason').refund(), /FINANCE_COMMAND_PROTOCOL_REQUIRED/); assert.equal(calls, 0); assert.equal(old.commandProtocol, undefined); } finally { globalThis.fetch = prior; }
});
test('refund facts and synchronization retain their existing protocol without repair provenance', async () => {
  const line = source.slice(source.indexOf('    const facts = operation.pendingFacts || (detail && target &&'), source.indexOf('    if (!facts ||', refundStart));
  const facts = new Function('operation', 'detail', 'target', 'targetID', 'reason', `${line}return facts;`)({}, { order_id: 'order' }, { transaction: { payment_transaction_id: 'transaction', amount_fen: '100' } }, 'package', 'reason');
  assert.equal(facts.action, 'refund'); assert.equal(facts.commandProtocol, undefined);
  const prior = globalThis.fetch; let body; globalThis.fetch = async (_url, options) => { body = JSON.parse(options.body); return new Response('{}', { status: 200 }); };
  try { await new AdminPlatformClient('').syncPaymentOrder('order'); assert.deepEqual(body, {}); } finally { globalThis.fetch = prior; }
});
