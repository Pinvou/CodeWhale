import assert from 'node:assert/strict';
import test from 'node:test';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { createAndSubmitFreshGovernedApproval, createAndSubmitGovernedApproval, updateAndSubmitGovernedApproval } from '../src/features/account-platform/admin-governed-approval-workflow.js';

const id = '11111111-1111-4111-8111-111111111111';
const capability = (version = 2, epoch = 1) => ({ control: { version, epoch, authorization_relay_epoch: 1 }, session_approval_wire_version: 2 });
const json = (value, status = 200) => new Response(JSON.stringify(value), { status });
const draft = { command_type: 'WALLET_ADJUSTMENT', target_resource_type: 'MANUAL_ADJUSTMENT', target_resource_id: id, idempotency_key: 'controlled-original-key' };
const detail = { approval_request_id: id, command_type: draft.command_type, target: { resource_type: draft.target_resource_type, resource_id: id }, status: 'DRAFT', version: 1, action_control_version: 2, detail_availability: 'AVAILABLE', allowed_actions: ['EDIT', 'SUBMIT'] };
async function withFetch(fn, action) {
  const original = globalThis.fetch;
  globalThis.fetch = fn;
  try { await action(); } finally { globalThis.fetch = original; }
}
async function rejection(action) {
  try { await action(); } catch (error) { return error; }
  assert.fail('Expected a rejection');
}

for (const value of ['1.5', 'abc', '9223372036854775808']) test(`actual client rejects ${value} before dispatch`, async () => {
  let calls = 0;
  await withFetch(() => { calls++; throw new Error('Unexpected fetch'); }, async () => {
    const error = await rejection(() => new AdminPlatformClient('').createRechargeProduct({ base_points: value, bonus_points: '0', mapping: { amount_fen: '100' } }, 'controlled-key'));
    assert.equal(calls, 0);
    assert.equal(error.financeRequestState, 'NOT_DISPATCHED');
  });
});

test('valid int64 text is still sent as exact JSON digits', async () => {
  await withFetch(async (_url, options) => {
    assert.ok(options.body.includes('"base_points":9007199254740993'));
    assert.ok(options.body.includes('"amount_fen":9223372036854775807'));
    return json({ catalog_item_id: id, version: 1 }, 201);
  }, async () => { await new AdminPlatformClient('').createRechargeProduct({ base_points: '9007199254740993', bonus_points: '0', mapping: { amount_fen: '9223372036854775807' } }, 'controlled-key'); });
});

test('transport failure and invalid success ACK are not declared undispatched', async () => {
  const client = new AdminPlatformClient('');
  for (const response of [() => { throw new TypeError('controlled connection reset'); }, () => json({ catalog_item_id: 'bad', version: 1 }, 201)]) {
    await withFetch(response, async () => {
      const error = await rejection(() => client.createRechargeProduct({ base_points: '100' }, 'controlled-key'));
      assert.notEqual(error.financeRequestState, 'NOT_DISPATCHED');
    });
  }
});

test('optional usage codec rejects an invalid body before dispatch', async () => {
  let calls = 0;
  await withFetch(() => { calls++; throw new Error('Unexpected fetch'); }, async () => {
    const codec = { encode: () => { throw new Error('INVALID_USAGE_INTEGER'); } };
    const error = await rejection(() => new AdminPlatformClient('').mutate('token', '/v1/admin/usage', 'POST', {}, 'controlled-key', undefined, 200, codec));
    assert.equal(calls, 0);
    assert.equal(error.financeRequestState, 'NOT_DISPATCHED');
  });
});

test('optional usage codec keeps exact bytes and decodes the successful response', async () => {
  const encoded = '{"evidence_high_watermark":9007199254740993}';
  const decoded = { evidence_high_watermark: '9007199254740993' };
  const body = { evidence_high_watermark: '9007199254740993' };
  const codec = { encode: (value) => { assert.equal(value, body); return encoded; }, decode: (value) => { assert.equal(value, encoded); return decoded; } };
  await withFetch(async (_url, options) => {
    assert.equal(options.body, encoded);
    return new Response(encoded, { status: 200 });
  }, async () => {
    assert.equal(await new AdminPlatformClient('').mutate('token', '/v1/admin/usage', 'POST', body, 'controlled-key', undefined, 200, codec), decoded);
  });
});

test('optional usage codec response rejection is not declared undispatched', async () => {
  let calls = 0;
  const codec = { encode: () => '{}', decode: () => { throw new Error('INVALID_USAGE_ACK'); } };
  await withFetch(async () => { calls++; return json({}); }, async () => {
    const error = await rejection(() => new AdminPlatformClient('').mutate('token', '/v1/admin/usage', 'POST', {}, 'controlled-key', undefined, 200, codec));
    assert.equal(calls, 1);
    assert.notEqual(error.financeRequestState, 'NOT_DISPATCHED');
  });
});

test('decision control rejection before POST is distinct from post-POST ACK rejection', async () => {
  const client = new AdminPlatformClient(''); const calls = [];
  await withFetch(async (url) => { calls.push(url); return url.endsWith('/capability') ? json(capability()) : json({}, 200); }, async () => {
    const before = await rejection(() => client.decideGovernedApproval(id, { ...detail, action_control_version: 1 }, 'APPROVE', 'Controlled reason'));
    assert.equal(before.financeRequestState, 'NOT_DISPATCHED');
    assert.equal(calls.length, 1);
    const after = await rejection(() => client.decideGovernedApproval(id, detail, 'APPROVE', 'Controlled reason'));
    assert.notEqual(after.financeRequestState, 'NOT_DISPATCHED');
    assert.equal(calls.filter(url => url.endsWith('/actions/decide')).length, 1);
  });
});

test('failed capability GET before a decision remains a non-writing failure', async () => {
  await withFetch(() => { throw new TypeError('controlled GET failure'); }, async () => {
    const error = await rejection(() => new AdminPlatformClient('').decideGovernedApproval(id, detail, 'APPROVE', 'Controlled reason'));
    assert.equal(error.financeRequestState, 'NOT_DISPATCHED');
  });
});

test('current-lineage lower control version clears the cached tuple and aborts readers', async () => {
  const client = new AdminPlatformClient(''); client.approvalControl = capability(3);
  const reader = new AbortController(); client.approvalControllers.add(reader);
  await withFetch(() => json(capability(2)), async () => {
    await assert.rejects(client.governedApprovalCapability());
    assert.equal(client.approvalControl, null);
    assert.equal(reader.signal.aborted, true);
    assert.equal(client.approvalControllers.size, 0);
  });
});

test('current-lineage malformed control also clears stale authority', async () => {
  const client = new AdminPlatformClient(''); client.approvalControl = capability(3);
  const reader = new AbortController(); client.approvalControllers.add(reader);
  await withFetch(() => json({}), async () => {
    await assert.rejects(client.governedApprovalCapability());
    assert.equal(client.approvalControl, null);
    assert.equal(reader.signal.aborted, true);
  });
});

for (const retired of [capability(2), capability(3, 8), { code: 'APPROVAL_PROTOCOL_UNAVAILABLE' }]) test(`retired capability response ${JSON.stringify(retired)} cannot clear newer lineage`, async () => {
  const client = new AdminPlatformClient(''); client.approvalControl = capability(3);
  let complete;
  await withFetch(() => new Promise(resolve => { complete = resolve; }), async () => {
    const old = client.governedApprovalCapability();
    client.approvalControl = capability(4);
    const reader = new AbortController(); client.approvalControllers.add(reader);
    complete(json(retired, retired.code ? 503 : 200));
    await assert.rejects(old);
    assert.deepEqual(client.approvalControl, capability(4));
    assert.equal(reader.signal.aborted, false);
  });
});

test('a capability response from a retired session cannot install authority', async () => {
  const client = new AdminPlatformClient(''); let complete;
  await withFetch(() => new Promise(resolve => { complete = resolve; }), async () => {
    const old = client.governedApprovalCapability(); client.financeSessionEpoch++;
    complete(json(capability()));
    await assert.rejects(old);
    assert.equal(client.approvalControl, null);
  });
});

test('complete session replacement clears control and active read controllers', async () => {
  const client = new AdminPlatformClient(''); client.csrfToken = 'SYNTHETIC_OLD'; client.approvalControl = capability();
  const reader = new AbortController(); client.approvalControllers.add(reader);
  await withFetch(() => new Response('{}', { status: 401 }), async () => {
    await client.session();
    assert.equal(client.approvalControl, null);
    assert.equal(reader.signal.aborted, true);
  });
});

test('parallel capability reads can confirm the exact same live tuple', async () => {
  const client = new AdminPlatformClient(''); const completions = [];
  await withFetch(() => new Promise(resolve => { completions.push(resolve); }), async () => {
    const first = client.governedApprovalCapability(); const second = client.governedApprovalCapability();
    completions[0](json(capability())); const installed = await first;
    completions[1](json(capability())); assert.equal(await second, client.approvalControl);
    assert.equal(installed, client.approvalControl);
  });
});

test('late rejected decision cannot revoke a newer live control or its readers', async () => {
  const client = new AdminPlatformClient(''); let complete;
  await withFetch((url) => url.endsWith('/capability') ? json(capability()) : new Promise(resolve => { complete = resolve; }), async () => {
    const old = client.decideGovernedApproval(id, detail, 'APPROVE', 'Controlled reason');
    while (!complete) await new Promise(resolve => setTimeout(resolve, 0));
    client.approvalControl = capability(3);
    const reader = new AbortController(); client.approvalControllers.add(reader);
    complete(json({ code: 'APPROVAL_ACTION_CONTROL_CHANGED' }, 409));
    const error = await rejection(() => old);
    assert.notEqual(error.financeRequestState, 'NOT_DISPATCHED');
    assert.deepEqual(client.approvalControl, capability(3));
    assert.equal(reader.signal.aborted, false);
  });
});

test('late success ACK from a retired session is not a current decision receipt', async () => {
  const client = new AdminPlatformClient(''); let complete;
  await withFetch((url) => url.endsWith('/capability') ? json(capability()) : new Promise(resolve => { complete = resolve; }), async () => {
    const old = client.decideGovernedApproval(id, detail, 'APPROVE', 'Controlled reason');
    while (!complete) await new Promise(resolve => setTimeout(resolve, 0));
    client.financeSessionEpoch++;
    complete(new Response(null, { status: 204 }));
    const error = await rejection(() => old);
    assert.notEqual(error.financeRequestState, 'NOT_DISPATCHED');
  });
});

test('incomplete history before any write is explicitly non-writing', async () => {
  const client = new AdminPlatformClient(''); let posts = 0;
  await withFetch((url, options) => { if (options?.method === 'POST') posts++; return url.endsWith('/capability') ? json(capability()) : json({ items: [], has_more: true }); }, async () => {
    const error = await rejection(() => createAndSubmitFreshGovernedApproval(client, draft));
    assert.equal(error.code, 'APPROVAL_HISTORY_INCOMPLETE');
    assert.equal(error.financeRequestState, 'NOT_DISPATCHED');
    assert.equal(posts, 0);
  });
});

test('approval created ID and prior write survive subsequent readonly failure', async () => {
  const client = new AdminPlatformClient(''); let writes = 0;
  await withFetch((url, options) => {
    if (url.endsWith('/capability')) return json(capability());
    if (options?.method === 'POST') { writes++; return json({ approval_request_id: id }, 201); }
    throw new TypeError('controlled detail GET reset');
  }, async () => {
    const error = await rejection(() => createAndSubmitGovernedApproval(client, draft));
    assert.equal(error.commandReference, id);
    assert.equal(error.financeRequestState, 'WRITE_OBSERVED');
    assert.equal(writes, 1);
  });
});

test('successful update followed by 403 preserves original reference and write observation', async () => {
  const client = new AdminPlatformClient(''); let updated = false;
  await withFetch((url, options) => {
    if (url.endsWith('/capability')) return json(capability());
    if (options?.method === 'PUT') { updated = true; return new Response(null, { status: 204 }); }
    return updated ? json({ code: 'PERMISSION_DENIED' }, 403) : json(detail);
  }, async () => {
    const error = await rejection(() => updateAndSubmitGovernedApproval(client, detail, draft));
    assert.equal(error.status, 403);
    assert.equal(error.commandReference, id);
    assert.equal(error.financeRequestState, 'WRITE_OBSERVED');
  });
});
