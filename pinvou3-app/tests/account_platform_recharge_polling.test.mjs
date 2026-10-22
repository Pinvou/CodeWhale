import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import test from 'node:test';
import { OrganizationClient } from '../src/features/account-platform/organization-client.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { createRechargeOrderReader, rechargePollDelay, refreshPaidRechargeRead, startRechargeOrderPolling } from '../src/features/account-platform/recharge-order-polling.js';
const id = '11111111-1111-4111-8111-111111111111', other = '22222222-2222-4222-8222-222222222222';
const pending = () => ({ order_id: id, amount_fen: '9007199254740993', status: 'PENDING', close_status: 'OPEN', expires_at: new Date(Date.now() + 60000).toISOString() });
const wait = async predicate => { const end = Date.now() + 3000; while (!predicate()) { if (Date.now() > end) assert.fail('controlled operation did not converge'); await new Promise(resolve => setTimeout(resolve, 5)); } };
function clock() {
  const queue = new Map(); let sequence = 0;
  return { queue, setTimeout(fn, delay) { const key = ++sequence; queue.set(key, { fn, delay }); return key; }, clearTimeout(key) { queue.delete(key); },
    fire() { assert.equal(queue.size, 1); const [key, item] = [...queue][0]; queue.delete(key); item.fn(); return item.delay; } };
}
async function fixture(t, handler) {
  const calls = [], responses = new Set(); let active = 0, maximum = 0;
  const server = createServer((request, response) => {
    calls.push({ method: request.method, url: request.url }); active++; maximum = Math.max(maximum, active); responses.add(response);
    response.on('close', () => { active--; responses.delete(response); });
    handler(request, response);
  });
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  t.after(async () => { for (const response of responses) response.destroy(); server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); });
  const client = new AccountPlatformBffClient(`http://127.0.0.1:${server.address().port}`);
  return { client, calls, active: () => active, maximum: () => maximum };
}
const send = (response, value, status = 200) => { response.writeHead(status, { 'Content-Type': 'application/json' }); response.end(JSON.stringify(value)); };
function polling(client, order = pending(), current = () => true, extra = {}) {
  const timers = clock(), accepted = [], errors = [];
  const reader = createRechargeOrderReader(client, current);
  const state = { order };
  const stop = startRechargeOrderPolling({ reader, timers, random: () => 0.5, getOrder: () => state.order, isCurrent: current,
    accept: value => { accepted.push(value); state.order = value; }, onError: reason => errors.push(reason), ...extra });
  return { reader, timers, accepted, errors, state, dispose() { stop(); reader.dispose(); } };
}

test('recharge polling preserves the implemented cadence without a new retry policy', () => {
  assert.equal(rechargePollDelay(0, () => 0.5), 2000); assert.equal(rechargePollDelay(3, () => 0.5), 3500);
  assert.equal(rechargePollDelay(99, () => 0.5), 5000); assert.equal(rechargePollDelay(0, () => 0), 1800);
  assert.equal(rechargePollDelay(99, () => 1), 5500);
});

test('production recharge poller and BFF client use controlled actual HTTP without a second order request', async t => {
  await t.test('held GET coalesces manual query, exact DTO updates once and paid stops', async t => {
    let response; const f = await fixture(t, (_request, r) => { response = r; });
    const p = polling(f.client); t.after(() => p.dispose());
    assert.equal(p.timers.fire(), 2000); await wait(() => f.calls.length === 1);
    const manual = p.reader.read(id); void manual.catch(() => {}); await new Promise(resolve => setTimeout(resolve, 30)); assert.equal(f.calls.length, 1);
    send(response, { ...pending(), status: 'PAID', payment_url: '', qr_data: '' });
    const result = await manual; await wait(() => p.accepted.length === 1);
    assert.equal(result.amount_fen, '9007199254740993'); assert.equal(p.state.order.status, 'PAID');
    assert.equal(p.timers.queue.size, 0); assert.equal(f.maximum(), 1);
    assert.deepEqual(f.calls, [{ method: 'GET', url: `/api/token/v1/recharge/orders/${id}` }]);
  });
  await t.test('coalesced manual and auto PAID refresh once while a later paid read refreshes new facts', async t => {
    let response; const f = await fixture(t, (_request, r) => { response = r; });
    let callbacks = 0; const seen = new WeakSet();
    const refresh = () => { callbacks++; };
    const p = polling(f.client, pending(), () => true, { accept: (latest, request) => refreshPaidRechargeRead(latest, request, seen, refresh) }); t.after(() => p.dispose());
    p.timers.fire(); await wait(() => f.calls.length === 1); const manual = p.reader.read(id);
    send(response, { ...pending(), status: 'PAID', fulfillment_status: 'CREDITED' }); const paid = await manual;
    await refreshPaidRechargeRead(paid, manual, seen, refresh); await wait(() => callbacks === 1);
    assert.equal(f.calls.length, 1); assert.equal(callbacks, 1); assert.equal(p.timers.queue.size, 0);
    const later = p.reader.read(id); await wait(() => f.calls.length === 2);
    send(response, { ...paid, fulfillment_status: 'CREDITED', version: 2 });
    await refreshPaidRechargeRead(await later, later, seen, refresh);
    assert.equal(callbacks, 2, 'a later response of the same order must not be suppressed forever');
  });
  await t.test('retired current context rejects late ACK without consumer callback or new timer', async t => {
    let response, current = true; const f = await fixture(t, (_request, r) => { response = r; });
    const p = polling(f.client, pending(), () => current); t.after(() => p.dispose());
    p.timers.fire(); await wait(() => f.calls.length === 1); current = false;
    send(response, { ...pending(), status: 'PAID' }); await wait(() => f.active() === 0); await new Promise(resolve => setTimeout(resolve, 20));
    assert.equal(p.accepted.length, 0); assert.equal(p.errors.length, 0); assert.equal(p.timers.queue.size, 0);
  });
  await t.test('live client login epoch fences a held HTTP ACK without any consumer render', async t => {
    let response; const f = await fixture(t, (_request, r) => { response = r; });
    // Consumer isCurrent deliberately stays true: no React/owner rerender assists this guard.
    const p = polling(f.client, pending(), () => true); t.after(() => p.dispose());
    p.timers.fire(); await wait(() => f.calls.length === 1);
    const oldEpoch = f.client.financeSessionEpoch; f.client.financeSessionEpoch++;
    send(response, { ...pending(), status: 'PAID' }); await wait(() => f.active() === 0); await new Promise(resolve => setTimeout(resolve, 20));
    assert.equal(f.client.financeSessionEpoch, oldEpoch + 1); assert.equal(p.accepted.length, 0);
    assert.equal(p.errors.length, 0); assert.equal(p.timers.queue.size, 0);
    await assert.rejects(p.reader.read(id), reason => reason.name === 'AbortError'); assert.equal(f.calls.length, 1);
  });
  await t.test('view disposal aborts the owned actual HTTP request', async t => {
    const f = await fixture(t, () => {}), p = polling(f.client); t.after(() => p.dispose());
    p.timers.fire(); await wait(() => f.calls.length === 1);
    const manual = p.reader.read(id); const observed = manual.catch(reason => reason); p.dispose();
    assert.equal((await observed).name, 'AbortError'); await wait(() => f.active() === 0);
    assert.equal(p.accepted.length, 0); assert.equal(p.timers.queue.size, 0);
  });
  await t.test('selected order change cannot consume old order response', async t => {
    let response; const f = await fixture(t, (_request, r) => { response = r; }), p = polling(f.client); t.after(() => p.dispose());
    p.timers.fire(); await wait(() => f.calls.length === 1); p.state.order = { ...pending(), order_id: other };
    send(response, { ...pending(), status: 'PAID' }); await wait(() => f.active() === 0); await new Promise(resolve => setTimeout(resolve, 20));
    assert.equal(p.accepted.length, 0); assert.equal(p.timers.queue.size, 0);
  });
  await t.test('401 and 403 are terminal authorization failures for this timer', async t => {
    for (const status of [401, 403]) {
      const f = await fixture(t, (_request, response) => send(response, { code: 'CONTROLLED_DENIED' }, status)), p = polling(f.client); t.after(() => p.dispose());
      p.timers.fire(); await wait(() => p.errors.length === 1);
      assert.equal(p.errors[0].status, status); assert.equal(p.accepted.length, 0); assert.equal(p.timers.queue.size, 0); assert.equal(f.calls.length, 1);
    }
  });
  await t.test('transient error schedules original delay but local expiry prevents next HTTP', async t => {
    const f = await fixture(t, (_request, response) => send(response, { code: 'CONTROLLED_UNAVAILABLE' }, 503));
    let now = Date.now(); const order = pending(), p = polling(f.client, order, () => true, { now: () => now }); t.after(() => p.dispose());
    p.timers.fire(); await wait(() => p.errors.length === 1 && p.timers.queue.size === 1);
    assert.equal([...p.timers.queue.values()][0].delay, 2500); now = new Date(order.expires_at).getTime(); p.timers.fire();
    await new Promise(resolve => setTimeout(resolve, 20)); assert.equal(f.calls.length, 1); assert.equal(p.timers.queue.size, 0);
  });
  await t.test('query started before expiry may still consume owned paid proof after expiry', async t => {
    let response, now = Date.now(); const f = await fixture(t, (_request, r) => { response = r; });
    const order = pending(), p = polling(f.client, order, () => true, { now: () => now }); t.after(() => p.dispose());
    p.timers.fire(); await wait(() => f.calls.length === 1); now = new Date(order.expires_at).getTime() + 1;
    send(response, { ...order, status: 'PAID', payment_url: '', qr_data: '' }); await wait(() => p.accepted.length === 1);
    assert.equal(p.accepted[0].status, 'PAID'); assert.equal(p.timers.queue.size, 0);
  });
  await t.test('wrong-order ACK never reaches consumer', async t => {
    const f = await fixture(t, (_request, response) => send(response, { ...pending(), order_id: other })), p = polling(f.client); t.after(() => p.dispose());
    p.timers.fire(); await wait(() => p.errors.length === 1); assert.equal(p.accepted.length, 0); assert.match(p.errors[0].message, /INVALID_FINANCE/);
  });
  await t.test('nonpayable and already expired states never schedule or read', async () => {
    for (const order of [{ ...pending(), status: 'PAID' }, { ...pending(), close_status: 'CLOSE_REQUESTED' },
      { ...pending(), close_status: 'LOCALLY_CLOSED_EXTERNAL_PENDING' }, { ...pending(), expires_at: new Date(0).toISOString() }]) {
      let reads = 0; const p = polling({ rechargeOrder() { reads++; } }, order);
      assert.equal(p.timers.queue.size, 0); assert.equal(reads, 0); p.dispose();
    }
  });
});


test('organization polling retains explicit organization scope through the production client and actual HTTP', async t => {
  const org = '33333333-3333-4333-8333-333333333333', foreign = '44444444-4444-4444-8444-444444444444';
  const forOrganization = client => {
    const api = new OrganizationClient(client);
    return { api, get financeSessionEpoch() { return client.financeSessionEpoch; }, rechargeOrder(orderID, signal) { return api.order(org, orderID, signal); } };
  };
  await t.test('held organization GET and manual read share paid wallet/package refresh without a POST', async t => {
    let response; const f = await fixture(t, (request, r) => {
      if (request.url.endsWith('/' + id)) response = r;
      else if (request.url.endsWith('/balance')) send(r, { owner_type: 'ORGANIZATION', owner_id: org, wallet_id: other, posted_microcredits: '9007199254740993', reserved_microcredits: '0', available_microcredits: '9007199254740993' });
      else if (request.url.endsWith('/entitlement-packages')) send(r, { items: [] });
      else send(r, {}, 404);
    });
    const scoped = forOrganization(f.client), seen = new WeakSet(); let refreshes = 0;
    const consume = (latest, request) => refreshPaidRechargeRead(latest, request, seen, async () => {
      const [balance, packages] = await Promise.all([scoped.api.balance(org), scoped.api.packages(org)]);
      assert.equal(balance.owner_id, org); assert.equal(balance.available_microcredits, '9007199254740993'); assert.deepEqual(packages, []); refreshes++;
    });
    const p = polling(scoped, { ...pending(), beneficiary_organization_id: org }, () => true, { accept: consume }); t.after(() => p.dispose());
    p.timers.fire(); await wait(() => f.calls.length === 1); const manual = p.reader.read(id);
    send(response, { ...pending(), beneficiary_organization_id: org, status: 'PAID', fulfillment_status: 'CREDITED' });
    await consume(await manual, manual); await wait(() => refreshes === 1);
    assert.equal(p.timers.queue.size, 0); assert.equal(f.calls.length, 3); assert.ok(f.calls.every(call => call.method === 'GET'));
    assert.deepEqual(f.calls.map(call => call.url).sort(), [`/api/token/v1/organizations/${org}/recharge/orders/${id}`, `/api/token/v1/organizations/${org}/balance`, `/api/token/v1/organizations/${org}/entitlement-packages`].sort());
  });
  await t.test('foreign beneficiary is rejected even when order ID matches', async t => {
    const f = await fixture(t, (_request, response) => send(response, { ...pending(), beneficiary_organization_id: foreign, status: 'PAID' }));
    const p = polling(forOrganization(f.client)); t.after(() => p.dispose()); p.timers.fire(); await wait(() => p.errors.length === 1);
    assert.equal(p.accepted.length, 0); assert.match(p.errors[0].message, /INVALID_ORGANIZATION_ACK/);
  });
  await t.test('organization or purchase authority retirement drops the held receipt', async t => {
    for (const retirement of ['organization-context', 'purchase-permission', 'live-login-epoch']) {
      let response, current = true; const f = await fixture(t, (_request, r) => { response = r; });
      const p = polling(forOrganization(f.client), pending(), () => current); t.after(() => p.dispose());
      p.timers.fire(); await wait(() => f.calls.length === 1);
      if (retirement === 'live-login-epoch') f.client.financeSessionEpoch++; else current = false;
      send(response, { ...pending(), beneficiary_organization_id: org, status: 'PAID' });
      await wait(() => f.active() === 0); await new Promise(resolve => setTimeout(resolve, 20));
      assert.equal(p.accepted.length, 0, retirement); assert.equal(p.errors.length, 0, retirement); assert.equal(p.timers.queue.size, 0, retirement);
      await assert.rejects(p.reader.read(id), reason => reason.name === 'AbortError'); assert.equal(f.calls.length, 1);
    }
  });
});
