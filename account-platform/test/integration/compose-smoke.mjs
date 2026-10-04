import assert from 'node:assert/strict';

const services = [
  ['account-service', 'http://127.0.0.1:18101'],
  ['billing-service', 'http://127.0.0.1:18102'],
  ['token-gateway', 'http://127.0.0.1:18103'],
  ['payment-simulator', 'http://127.0.0.1:18104'],
  ['token-portal', 'http://127.0.0.1:18105'],
];

async function get(url, options = {}) {
  const response = await fetch(url, { redirect: 'manual', signal: AbortSignal.timeout(5000), ...options });
  return response;
}

for (const [name, origin] of services) {
  const response = await get(`${origin}/health/ready`);
  assert.equal(response.status, 200, `${name} readiness returned ${response.status}`);
  assert.equal(response.headers.get('cache-control'), 'no-store', `${name} readiness must not be cached`);
  assert.match(response.headers.get('x-request-id') || '', /^[0-9a-f]{32}$/, `${name} request ID`);
  assert.match(response.headers.get('x-trace-id') || '', /^[0-9a-f]{32}$/, `${name} trace ID`);
}

const portal = await get('http://127.0.0.1:18105/');
assert.equal(portal.status, 200);
assert.equal(portal.headers.get('x-frame-options'), 'DENY');
assert.match(portal.headers.get('content-security-policy') || '', /frame-ancestors 'none'/);
assert.equal(portal.headers.get('cross-origin-resource-policy'), 'same-origin');

const metrics = await get('http://127.0.0.1:18105/metrics');
assert.equal(metrics.status, 404, 'metrics must not be exposed on the public application listener');

console.log(`compose smoke passed for ${services.length} services`);
