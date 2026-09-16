import assert from 'node:assert/strict';
import test from 'node:test';
import { AdminPlatformClient } from '../../../pinvou3-app/src/features/account-platform/admin-client.js';

test('governed approval client locks detail actions to capability and drops late replies', async () => {
  const original = globalThis.fetch;
  const client = new AdminPlatformClient('https://portal.example');
  client.csrfToken = 'csrf-token';
  let version = 9;
  let relay = 'RUNNING';
  let pendingDetail;
  const routes = [];
  globalThis.fetch = async (url, options = {}) => {
    const path = new URL(url).pathname;
    routes.push(path);
    if (path.endsWith('/capability')) {
      return Response.json({ control: { schema_version: 1, mode: 'PROOF_REQUIRED', approval_wire_version: 2,
        proof_required: true, epoch: 3, authorization_relay_state: relay,
        authorization_relay_epoch: version - 7, version, request_id: 'request' }, session_approval_wire_version: 2 });
    }
    if (path.endsWith('/actions/decide')) {
      assert.equal(options.headers['X-Pinvou-Approval-Action-Control-Version'], String(version));
      assert.deepEqual(JSON.parse(options.body), { expected_version: 3, decision: 'APPROVED', reason: 'independent approval', action_control_version: version });
      return new Response(null, { status: 204 });
    }
    if (pendingDetail) return await new Promise(resolve => { pendingDetail.resolve = resolve; });
    return Response.json({ approval_request_id: 'request-id', status: 'SUBMITTED', version: 3,
      allowed_actions: ['APPROVE'], action_control_version: version });
  };
  try {
    const detail = await client.governedApprovalRequest('request-id');
    assert.equal(detail.action_control_version, 9);
    assert.equal(await client.decideGovernedApproval('request-id', detail, 'APPROVED', 'independent approval'), null);
    assert.ok(routes.includes('/api/admin/approvals/requests/request-id/actions/decide'));

    pendingDetail = {};
    const late = client.governedApprovalRequest('request-id');
    await new Promise(resolve => setTimeout(resolve, 0));
    version = 10;
    relay = 'FENCED';
    await client.governedApprovalCapability();
    pendingDetail.resolve(Response.json({ approval_request_id: 'request-id', status: 'SUBMITTED', version: 3,
      allowed_actions: ['APPROVE'], action_control_version: 9 }));
    await assert.rejects(late, error => error.name === 'AbortError' || error.code === 'APPROVAL_ACTION_CONTROL_CHANGED');
    pendingDetail = null;
    relay = 'RUNNING';
    version = 11;
    const refreshed = await client.governedApprovalRequest('request-id');
    assert.equal(refreshed.action_control_version, 11);
    await assert.rejects(client.decideGovernedApproval('request-id', detail, 'APPROVED', 'stale independent approval'),
      { code: 'APPROVAL_ACTION_CONTROL_CHANGED' });
  } finally {
    globalThis.fetch = original;
  }
});
