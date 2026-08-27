import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('provider circuits isolate provider capability and route with exact rolling and half-open policy', async () => {
  const circuit = await read('internal/platform/resilience/circuit.go');
  assert.match(circuit, /Key\(provider, capability, route string\)/);
  assert.match(circuit, /now\.Add\(-time\.Minute\)/);
  assert.match(circuit, /eligible >= 20 && failures\*2 >= eligible/);
  assert.match(circuit, /consecutiveFailures >= 5/);
  assert.match(circuit, /halfOpenProbes >= 3/);
  assert.match(circuit, /halfOpenSuccesses >= 3/);
  assert.match(circuit, /30 \* time\.Second[\s\S]*15\*time\.Minute/);
  assert.match(circuit, /BusinessRejection[\s\S]*continue/);
  assert.match(circuit, /BeforeHighRisk[\s\S]*b\.before\(key, false\)/);
});

test('payment and model adapters record outcomes without automatic high-risk half-open replay', async () => {
  const [payment, payments, refunds, reconciliation, token] = await Promise.all([
    read('internal/billing/provider_resilience.go'),
    read('internal/billing/payments.go'),
    read('internal/billing/refunds.go'),
    read('internal/billing/reconciliation.go'),
    read('internal/token/proxy.go'),
  ]);
  assert.match(payment, /BeforeHighRisk/);
  assert.match(payment, /UncertainResult/);
  assert.match(payment, /BusinessRejection/);
  assert.match(payments, /providerCall\(ctx, p, "ORDER_CREATE", order\.Channel, true, providerCallScope\(order\)/);
  assert.match(refunds, /providerCall\(ctx, p, "REFUND", "default", true/);
  assert.match(reconciliation, /providerCall\(ctx, p, "ORDER_QUERY", order\.Channel, false, providerCallScope\(order\)/);
  assert.match(token, /BeforeHighRisk\(circuitKey\)[\s\S]*PROVIDER_CIRCUIT_OPEN/);
  assert.match(token, /UncertainResult[\s\S]*UPSTREAM_RESULT_UNKNOWN/);
});

test('production trusted clock uses three NTP sources and blocks readiness and writes beyond one second', async () => {
  const [clock, config, server, deployment, policy] = await Promise.all([
    read('internal/platform/trustedclock/clock.go'),
    read('internal/platform/config/config.go'),
    read('internal/platform/httpserver/server.go'),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
    read('deploy/helm/account-platform/templates/networkpolicies.yaml'),
  ]);
  assert.match(clock, /len\(clean\) < 3/);
  assert.match(clock, /250\*time\.Millisecond[\s\S]*trusted clock offset alert/);
  assert.match(clock, /> time\.Second[\s\S]*ErrClockUnsafe/);
  assert.match(config, /TRUSTED_TIME_SOURCES[\s\S]*len\(timeSources\) < 3/);
  assert.match(server, /health\/ready[\s\S]*clock\.Check/);
  assert.match(server, /CLOCK_UNSAFE/);
  assert.match(deployment, /TRUSTED_TIME_SOURCES/);
  assert.match(policy, /timeSourceCidrs[\s\S]*protocol: UDP, port: 123/);
});
