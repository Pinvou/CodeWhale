import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';

const billingOrigin = process.env.PINVOU_TEST_BILLING_ORIGIN || 'http://127.0.0.1:18102';
const paymentOrigin = process.env.PINVOU_TEST_PAYMENT_ORIGIN || 'http://127.0.0.1:18104';
for (const origin of [billingOrigin, paymentOrigin, process.env.PINVOU_TEST_PAYMENT_REQUEST_ORIGIN || paymentOrigin]) {
  const parsed = new URL(origin);
  assert.ok(parsed.protocol === 'http:' && ['127.0.0.1', '[::1]', 'localhost'].includes(parsed.hostname) && !parsed.username && !parsed.password && parsed.pathname === '/' && !parsed.search && !parsed.hash, 'acceptance origins must be plain local test HTTP origins');
}
const serviceToken = process.env.INTERNAL_SERVICE_TOKEN;
assert.ok(serviceToken, 'the E2E runner must inject a generated internal service token');

const serviceHeaders = { authorization: `Bearer ${serviceToken}` };

async function requestJSON(path, { method = 'GET', body, headers = {}, statuses = [200] } = {}) {
  for (let attempt = 0; attempt < 5; attempt += 1) {
    const response = await fetch(`${billingOrigin}${path}`, {
      method,
      headers: {
        ...serviceHeaders,
        ...(body === undefined ? {} : { 'content-type': 'application/json' }),
        ...headers,
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      redirect: 'manual',
      signal: AbortSignal.timeout(30_000),
    });
    const text = await response.text();
    if (response.status === 429 && attempt < 4) {
      await new Promise((resolve) => setTimeout(resolve, 1000));
      continue;
    }
    assert.ok(statuses.includes(response.status), `fixture Billing request returned ${response.status}`);
    return text ? JSON.parse(text) : null;
  }
  assert.fail('fixture Billing request exhausted its rate-limit retries');
}

async function eventually(label, operation, predicate, timeoutMs = 15_000) {
  const deadline = Date.now() + timeoutMs;
  let latest;
  while (Date.now() < deadline) {
    latest = await operation();
    if (predicate(latest)) return latest;
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  assert.fail(`${label} did not converge`);
}

async function products(channel, walletID) {
  const endpoint = walletID ? `/internal/v1/recharge/products?wallet_id=${walletID}&currency=CNY&channel=${channel}` : `/v1/recharge/products?currency=CNY&channel=${channel}`;
  const catalog = await requestJSON(endpoint);
  assert.ok(Array.isArray(catalog.items) && catalog.items.length >= 4, `${channel} recharge catalog must expose four products`);
  return catalog.items;
}

async function createOrder(requesterID, walletID, productID, channel, idempotencyKey) {
  return requestJSON('/internal/v1/recharge/orders', {
    method: 'POST',
    body: { requester_id: requesterID, wallet_id: walletID, product_id: productID, channel },
    headers: { 'idempotency-key': idempotencyKey },
    statuses: [200, 201],
  });
}

async function getOrder(orderID, requesterID) {
  return requestJSON(`/internal/v1/recharge/orders/${orderID}?requester_id=${requesterID}`);
}

async function completePayment(paymentURL) {
  const publicURL = new URL(paymentURL);
  assert.equal(publicURL.origin, paymentOrigin, 'payment URL must address the owned local simulator');
  const requestURL = new URL(`${publicURL.pathname}/complete${publicURL.search}`, process.env.PINVOU_TEST_PAYMENT_REQUEST_ORIGIN || publicURL.origin);
  const response = await fetch(requestURL, { method: 'POST', redirect: 'manual', signal: AbortSignal.timeout(10_000) });
  assert.equal(response.status, 200, `simulated payment completion returned ${response.status}`);
  await response.arrayBuffer();
}

const requesterID = randomUUID();
const wallet = await requestJSON('/internal/v1/wallets/ensure', {
  method: 'POST',
  body: { owner_type: 'ORGANIZATION', owner_id: randomUUID() },
  statuses: [200, 201],
});
assert.equal(wallet.status, 'ACTIVE');

const paidOrders = [];
for (const channel of ['UNIONPAY', 'ALIPAY', 'WECHAT']) {
  const catalog = await products(channel, wallet.wallet_id);
  assert.ok(catalog.every((product) => ['CNY_1', 'CNY_10', 'CNY_100', 'CNY_200'].includes(product.product_id) && product.validity_days === 0 && product.sku_code === ''), 'organization refund journey must use the four ordinary legacy products');
  const product = catalog.toSorted((left, right) => left.amount_fen - right.amount_fen)[0];
  const idempotencyKey = `e2e-${channel.toLowerCase()}-${randomUUID()}`;
  const attempts = channel === 'UNIONPAY' ? 100 : 1;
  const orders = await Promise.all(
    Array.from({ length: attempts }, () => createOrder(requesterID, wallet.wallet_id, product.product_id, channel, idempotencyKey)),
  );
  assert.equal(new Set(orders.map((order) => order.order_id)).size, 1, `${channel} idempotent creates must return one order`);
  const order = orders[0];
  assert.equal(order.channel, channel);
  const paymentURL = new URL(order.payment_url);
  assert.equal(paymentURL.origin, paymentOrigin, 'payment URL must use the configured public origin');
  assert.match(paymentURL.pathname, /^\/pay\//);

  const completions = channel === 'UNIONPAY' ? 100 : 1;
  await Promise.all(Array.from({ length: completions }, () => completePayment(order.payment_url)));
  const credited = await eventually(
    `${channel} payment callback`,
    () => getOrder(order.order_id, requesterID),
    (candidate) => ['PAID', 'PAID_LATE'].includes(candidate.status) && candidate.fulfillment_status === 'CREDITED',
  );
  paidOrders.push(credited);
}

const funded = await requestJSON(`/internal/v1/wallets/${wallet.wallet_id}/balance`);
const creditedTotal = paidOrders.reduce((total, order) => total + order.credit_microcredits, 0);
assert.equal(funded.posted_microcredits, creditedTotal, 'each successful channel payment must credit exactly once');
assert.equal(funded.reserved_microcredits, 0);

const usageRequestID = randomUUID();
const reserveAmount = Math.min(10_000, Math.floor(funded.available_microcredits / 2));
assert.ok(reserveAmount > 1, 'the funded wallet must support a reservation');
const reservations = await Promise.all(
  Array.from({ length: 100 }, () =>
    requestJSON('/internal/v1/holds/reserve', {
      method: 'POST',
      body: {
        usage_request_id: usageRequestID,
        wallet_id: wallet.wallet_id,
        maximum_microcredits: reserveAmount,
        lease_seconds: 300,
      },
    }),
  ),
);
assert.equal(new Set(reservations.map((hold) => hold.hold_id)).size, 1, 'concurrent reserves must create one hold');

const hold = reservations[0];
const actual = Math.floor(reserveAmount / 2);
const commits = await Promise.all(
  Array.from({ length: 100 }, () =>
    requestJSON('/internal/v1/holds/commit', {
      method: 'POST',
      body: {
        usage_request_id: usageRequestID,
        hold_id: hold.hold_id,
        actual_microcredits: actual,
        usage_snapshot: { model: 'mock-chat', input_tokens: 10, output_tokens: 5 },
      },
    }),
  ),
);
assert.ok(commits.every((candidate) => candidate.status === 'COMMITTED'));

const settled = await requestJSON(`/internal/v1/wallets/${wallet.wallet_id}/balance`);
assert.equal(settled.posted_microcredits, creditedTotal - actual, 'concurrent commits must debit once');
assert.equal(settled.reserved_microcredits, 0, 'terminal holds must release the entire reservation');
assert.ok(settled.available_microcredits >= 0, 'wallet balance must never be negative');

const refundableOrder = paidOrders.at(-1);
const refund = await requestJSON(`/internal/v1/recharge/orders/${refundableOrder.order_id}/refunds`, {
  method: 'POST',
  body: { requester_id: requesterID, amount_fen: 1, reason: 'automated partial-refund acceptance' },
  headers: { 'idempotency-key': `e2e-refund-${randomUUID()}` },
  statuses: [200, 201],
});
assert.equal(refund.status, 'SUCCEEDED', 'an unconsumed portion of a recharge lot must be refundable');

// Personal PLAN credit has a separate, non-refundable entitlement lifecycle.
const personalRequester = randomUUID();
const personalWallet = await requestJSON('/internal/v1/wallets/ensure', {
  method: 'POST', body: { owner_type: 'PERSONAL', owner_id: personalRequester }, statuses: [200, 201],
});
const personalProduct = (await products('ALIPAY')).find((product) => product.product_id === 'PLAN_TRIAL');
assert.ok(personalProduct, 'development simulator must expose its personal PLAN fixture');
const personalKey = `e2e-personal-plan-${randomUUID()}`;
const personalOrder = await createOrder(personalRequester, personalWallet.wallet_id, personalProduct.product_id, 'ALIPAY', personalKey);
await completePayment(personalOrder.payment_url);
const personalCredited = await eventually('personal PLAN credit', () => getOrder(personalOrder.order_id, personalRequester), (candidate) => candidate.fulfillment_status === 'CREDITED');
const personalReplay = await createOrder(personalRequester, personalWallet.wallet_id, personalProduct.product_id, 'ALIPAY', personalKey);
assert.equal(personalReplay.order_id, personalOrder.order_id);
const packages = await requestJSON(`/internal/v1/entitlement-packages?user_id=${personalRequester}&wallet_id=${personalWallet.wallet_id}`);
assert.equal(packages.items.length, 1);
assert.equal(packages.items[0].pinvou_order_id, personalOrder.order_id);
assert.equal(packages.items[0].plan_id, personalProduct.product_id);
assert.equal(BigInt(packages.summary.available_microcredits), BigInt(personalCredited.credit_microcredits));
const personalBalance = await requestJSON(`/internal/v1/wallets/${personalWallet.wallet_id}/balance`);
assert.equal(personalBalance.posted_microcredits, personalCredited.credit_microcredits);
const deniedRefund = await requestJSON(`/internal/v1/recharge/orders/${personalOrder.order_id}/refunds`, {
  method: 'POST', body: { requester_id: personalRequester, amount_fen: 1, reason: 'automated PLAN refund rejection acceptance' },
  headers: { 'idempotency-key': `e2e-plan-refund-${randomUUID()}` }, statuses: [402],
});
assert.equal(deniedRefund.code, 'WALLET_INSUFFICIENT', 'personal PLAN has no refundable source lot');
const personalAfterRefund = await requestJSON(`/internal/v1/wallets/${personalWallet.wallet_id}/balance`);
assert.equal(personalAfterRefund.posted_microcredits, personalBalance.posted_microcredits);
assert.equal(personalAfterRefund.reserved_microcredits, 0);

const businessDate = new Date().toISOString().slice(0, 10);
for (const channel of ['UNIONPAY', 'ALIPAY', 'WECHAT']) {
  const reconciliation = await requestJSON('/internal/v1/reconciliation/runs', {
    method: 'POST',
    body: { channel, business_date: businessDate },
  });
  assert.equal(reconciliation.channel, channel);
  assert.equal(reconciliation.case_count, 0, `${channel} reconciliation must close without mismatches`);
}

console.log('compose acceptance passed: three channels, idempotent recharge, settlement, partial refund and reconciliation');
