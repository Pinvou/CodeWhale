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

test('governed approval draft lifecycle uses one control generation and refetch-only results', async () => {
  const original = globalThis.fetch;
  const client = new AdminPlatformClient('https://portal.example');
  client.csrfToken = 'csrf-token';
  let version = 9;
  let wire = 2;
  let pending;
  const writes = [];
  globalThis.fetch = async (url, options = {}) => {
    const path = new URL(url).pathname;
    if (path.endsWith('/capability')) {
      return Response.json({ control: { schema_version: 1, mode: wire === 2 ? 'PROOF_REQUIRED' : 'SAFE_ROLLBACK',
        approval_wire_version: wire, proof_required: true, epoch: 3,
        authorization_relay_state: 'RUNNING', authorization_relay_epoch: version - 7,
        version, request_id: 'request' }, session_approval_wire_version: wire });
    }
    writes.push({ path, method: options.method, headers: options.headers, body: JSON.parse(options.body) });
    if (pending) return new Promise(resolve => { pending.resolve = resolve; });
    return path.endsWith('/requests')
      ? Response.json({ approval_request_id: 'request-id' }, { status: 201 })
      : new Response(null, { status: 204 });
  };
  try {
    const draft = { command_type: 'WALLET_ADJUSTMENT', idempotency_key: 'fresh-key' };
    assert.equal(await client.createGovernedApprovalDraft(draft), 'request-id');
    const detail = { approval_request_id: 'request-id', version: 1, action_control_version: 9 };
    assert.equal(await client.updateGovernedApprovalDraft('request-id', detail, draft), null);
    assert.equal(await client.submitGovernedApprovalRequest('request-id', detail), null);
    assert.equal(await client.withdrawGovernedApprovalRequest('request-id', detail), null);
    assert.deepEqual(writes.map(({ path, method }) => `${method} ${path}`), [
      'POST /api/admin/approvals/requests', 'PUT /api/admin/approvals/requests/request-id',
      'POST /api/admin/approvals/requests/request-id/actions/submit',
      'POST /api/admin/approvals/requests/request-id/actions/withdraw',
    ]);
    assert.ok(writes.every(({ headers }) => headers['X-Pinvou-Approval-Action-Control-Version'] === '9' && headers['X-CSRF-Token'] === 'csrf-token'));
    assert.equal(writes[1].body.expected_version, 1);
    assert.deepEqual(writes[2].body, { expected_version: 1 });
    pending = {};
    const late = client.submitGovernedApprovalRequest('request-id', detail);
    await new Promise(resolve => setTimeout(resolve, 0));
    version = 10;
    await client.governedApprovalCapability();
    pending.resolve(new Response(null, { status: 204 }));
    await assert.rejects(late, error => error.name === 'AbortError' || error.code === 'APPROVAL_ACTION_CONTROL_CHANGED');
    pending = null;
    await assert.rejects(client.withdrawGovernedApprovalRequest('request-id', detail), { code: 'APPROVAL_ACTION_CONTROL_CHANGED' });
    const legacyClient = new AdminPlatformClient('https://portal.example');
    legacyClient.csrfToken = 'csrf-token';
    wire = 1;
    assert.equal(await legacyClient.createGovernedApprovalDraft(draft), 'request-id');
    assert.equal(writes.at(-1).headers['X-Pinvou-Approval-Action-Control-Version'], '10');
  } finally {
    globalThis.fetch = original;
  }
});
