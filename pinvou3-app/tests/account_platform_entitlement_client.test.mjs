import test from 'node:test';
import assert from 'node:assert/strict';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';

const packageID = '00000000-0000-4000-8000-000000000001';
const orderID = '00000000-0000-4000-8000-000000000002';

test('owner precheck uses the exact read-only Account route and preserves int64 facts', async () => {
  const original = globalThis.fetch;
  let call;
  globalThis.fetch = async (url, options) => {
    call = { url, options };
    return new Response('{"critical_resource_versions":{"amount_microcredits":9223372036854775807},"financial_snapshot":{"package_total_microcredits":9223372036854775807}}', {
      status: 200, headers: { 'Content-Type': 'application/json' },
    });
  };
  try {
    const client = new AdminPlatformClient('https://portal.example'); client.csrfToken = 'controlled-test-csrf';
    const result = await client.entitlementCorrectionPrecheck(packageID, orderID, 'CORRECTION');
    assert.equal(call.url, 'https://portal.example/api/admin/account/admin/v2/approval-command-prechecks');
    assert.equal(call.options.method, 'POST'); assert.equal(call.options.credentials, 'same-origin');
    assert.equal(call.options.headers['X-CSRF-Token'], 'controlled-test-csrf');
    assert.deepEqual(JSON.parse(call.options.body), { command_type: 'ENTITLEMENT_COMPONENT_ADJUSTMENT', target_resource_id: packageID, payment_order_id: orderID, reason_code: 'CORRECTION' });
    assert.equal(result.critical_resource_versions.amount_microcredits, '9223372036854775807');
    assert.equal(result.financial_snapshot.package_total_microcredits, '9223372036854775807');
    assert.equal(call.options.headers['Idempotency-Key'], undefined);
  } finally { globalThis.fetch = original; }
});

test('precheck cannot accept an asynchronous or empty success receipt', async () => {
  const original = globalThis.fetch;
  try {
    const client = new AdminPlatformClient();
    for (const status of [202, 204]) {
      globalThis.fetch = async () => new Response(status === 204 ? null : '{}', { status });
      await assert.rejects(client.entitlementCorrectionPrecheck(packageID, orderID, 'CORRECTION'), /INVALID_MUTATION_ACK/);
    }
  } finally { globalThis.fetch = original; }
});

test('invalid selected identity is rejected before transport', async () => {
  const original = globalThis.fetch; let calls = 0;
  globalThis.fetch = async () => { calls++; throw new Error('unexpected transport'); };
  try {
    const client = new AdminPlatformClient();
    assert.throws(() => client.entitlementCorrectionPrecheck('../foreign', orderID, 'CORRECTION'));
    assert.throws(() => client.entitlementCorrectionPrecheck(packageID, '', 'CORRECTION'));
    assert.equal(calls, 0);
  } finally { globalThis.fetch = original; }
});
