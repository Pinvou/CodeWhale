import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import path from 'node:path';

const billingOrigin = 'http://127.0.0.1:18102';
const serviceToken = process.env.INTERNAL_SERVICE_TOKEN;
const composeFile = process.env.PINVOU_TEST_COMPOSE_FILE;
const composeEnvFile = process.env.PINVOU_TEST_COMPOSE_ENV_FILE;
const project = process.env.COMPOSE_PROJECT_NAME;

assert.ok(serviceToken, 'the fault runner requires its generated service token');
assert.match(project || '', /^pinvou-account-platform-test-\d+$/, 'faults may only target an isolated test project');
assert.ok(composeFile && composeEnvFile, 'the fault runner requires explicit Compose paths');
const expectedCompose = path.resolve(import.meta.dirname, '../../deploy/compose/compose.yaml');
assert.equal(path.resolve(composeFile), expectedCompose, 'faults may only use the repository test Compose file');
assert.equal(path.resolve(composeEnvFile), `${expectedCompose.slice(0, -'compose.yaml'.length)}.env.example`);

function compose(...args) {
  const result = spawnSync('docker', ['compose', '--env-file', composeEnvFile, '-f', composeFile, ...args], {
    env: process.env,
    stdio: 'inherit',
    shell: false,
  });
  if (result.error) throw result.error;
  assert.equal(result.status, 0, `docker compose ${args.join(' ')} failed`);
}

async function requestJSON(path, { method = 'GET', body, headers = {}, statuses = [200] } = {}) {
  const response = await fetch(`${billingOrigin}${path}`, {
    method,
    headers: {
      authorization: `Bearer ${serviceToken}`,
      ...(body === undefined ? {} : { 'content-type': 'application/json' }),
      ...headers,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    redirect: 'manual',
    signal: AbortSignal.timeout(10_000),
  });
  const text = await response.text();
  assert.ok(statuses.includes(response.status), `fixture Billing request returned ${response.status}`);
  return text ? JSON.parse(text) : null;
}

async function eventually(label, operation, predicate, timeoutMs = 30_000) {
  const deadline = Date.now() + timeoutMs;
  let latest;
  while (Date.now() < deadline) {
    try {
      latest = await operation();
      if (predicate(latest)) return latest;
    } catch {
      latest = null;
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  assert.fail(`${label} did not converge`);
}

async function waitReady() {
  await eventually(
    'billing readiness',
    async () => {
      const response = await fetch(`${billingOrigin}/health/ready`, { signal: AbortSignal.timeout(2000) });
      return { status: response.status };
    },
    (state) => state.status === 200,
  );
}

const requesterID = randomUUID();
const wallet = await requestJSON('/internal/v1/wallets/ensure', {
  method: 'POST',
  body: { owner_type: 'PERSONAL', owner_id: requesterID },
  statuses: [200, 201],
});
const catalog = await requestJSON('/v1/recharge/products?currency=CNY&channel=UNIONPAY');
const product = catalog.items.toSorted((left, right) => left.amount_fen - right.amount_fen)[0];
const order = await requestJSON('/internal/v1/recharge/orders', {
  method: 'POST',
  body: { requester_id: requesterID, wallet_id: wallet.wallet_id, product_id: product.product_id, channel: 'UNIONPAY' },
  headers: { 'idempotency-key': `fault-payment-${randomUUID()}` },
  statuses: [200, 201],
});

compose('stop', 'billing-service');
try {
  const pageURL = new URL(order.payment_url);
  assert.equal(pageURL.origin, 'http://127.0.0.1:18104', 'fault payment must remain on the owned test simulator');
  const completeURL = new URL(`${pageURL.pathname}/complete${pageURL.search}`, pageURL.origin);
  const payment = await fetch(completeURL, { method: 'POST', redirect: 'manual', signal: AbortSignal.timeout(10_000) });
  assert.equal(payment.status, 200, 'the provider must accept payment while Billing is unavailable');
  await payment.arrayBuffer();
  await new Promise((resolve) => setTimeout(resolve, 1500));
} finally {
  compose('start', 'billing-service');
}
await waitReady();

const credited = await eventually(
  'payment callback must converge after Billing restarts',
  () => requestJSON(`/internal/v1/recharge/orders/${order.order_id}?requester_id=${requesterID}`),
  (candidate) => candidate.fulfillment_status === 'CREDITED',
);
assert.ok(['PAID', 'PAID_LATE'].includes(credited.status));

compose('stop', 'nats');
try {
  const usageRequestID = randomUUID();
  const hold = await requestJSON('/internal/v1/holds/reserve', {
    method: 'POST',
    body: {
      usage_request_id: usageRequestID,
      wallet_id: wallet.wallet_id,
      maximum_microcredits: Math.min(1000, credited.credit_microcredits),
      lease_seconds: 300,
    },
  });
  const released = await requestJSON('/internal/v1/holds/release', {
    method: 'POST',
    body: { usage_request_id: usageRequestID, hold_id: hold.hold_id, reason: 'fault-test NATS outage' },
  });
  assert.equal(released.status, 'RELEASED', 'local Billing transactions must complete while NATS is unavailable');
} finally {
compose('start', 'nats');
}

const balance = await requestJSON(`/internal/v1/wallets/${wallet.wallet_id}/balance`);
assert.equal(balance.posted_microcredits, credited.credit_microcredits);
assert.equal(balance.reserved_microcredits, 0);

console.log('compose fault acceptance passed: callback retry after Billing restart and local settlement during NATS outage');
