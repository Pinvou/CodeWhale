import assert from 'node:assert/strict';
import test from 'node:test';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';

const id = '11111111-1111-4111-8111-111111111111';
const detail = { approval_request_id: id, version: 4, action_control_version: 2, critical_resource_versions: { wallet: 3 } };
const reason = '  Controlled decision reason \u2003';
const cases = [['APPROVE', 'APPROVED'], ['REJECT', 'REJECTED'], ['APPROVED', 'APPROVED'], ['REJECTED', 'REJECTED']];

for (const wire of [1, 2]) {
  for (const [action, decision] of cases) {
    test(`actual approval client sends canonical ${decision} for ${action} on wire ${wire}`, async t => {
      const original = globalThis.fetch;
      t.after(() => { globalThis.fetch = original; });
      const calls = [];
      const client = new AdminPlatformClient('');
      client.csrfToken = 'PUBLIC_SYNTHETIC_CSRF';
      globalThis.fetch = async (url, options) => {
        calls.push({ url, options });
        if (url.endsWith('/capability')) return new Response(JSON.stringify({
          control: { version: 2, epoch: 1, authorization_relay_epoch: 1 }, session_approval_wire_version: wire,
        }), { status: 200 });
        return new Response(null, { status: 204 });
      };
      assert.equal(await client.decideGovernedApproval(id, detail, action, reason), null);
      assert.equal(calls.length, 2);
      const { url, options } = calls[1];
      assert.equal(url, `/api/admin/approvals/requests/${id}/actions/decide`);
      assert.equal(options.method, 'POST');
      assert.equal(options.credentials, 'same-origin');
      assert.equal(options.cache, 'no-store');
      assert.equal(options.headers['X-CSRF-Token'], client.csrfToken);
      assert.equal(options.headers['X-Pinvou-Approval-Action-Control-Version'], '2');
      assert.deepEqual(JSON.parse(options.body), {
        expected_version: 4, decision, reason,
        ...(wire === 2 ? { action_control_version: 2 } : { observed_versions: detail.critical_resource_versions }),
      });
    });
  }
}

test('invalid approval actions reject before any capability or mutation fetch', async t => {
  const original = globalThis.fetch;
  t.after(() => { globalThis.fetch = original; });
  let calls = 0;
  globalThis.fetch = async () => { calls++; throw new Error('unexpected fetch'); };
  for (const action of [undefined, null, '', 'approve', 'reject', ' APPROVE', 'REJECTED ', 'WITHDRAW', 'toString', '__proto__', 'constructor', {}, ['APPROVE'], new String('APPROVE')]) {
    await assert.rejects(new AdminPlatformClient('').decideGovernedApproval(id, detail, action, reason), error => {
      assert.equal(error.code, 'INVALID_APPROVAL_DECISION');
      assert.equal(error.financeRequestState, 'NOT_DISPATCHED');
      assert.equal(error.financeInputInvalid, true);
      return true;
    });
  }
  assert.equal(calls, 0);
});
