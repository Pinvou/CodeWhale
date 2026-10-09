import assert from 'node:assert/strict';
import http from 'node:http';
import test from 'node:test';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { entitlementCorrectionApprovalDraft } from '../src/features/account-platform/admin-entitlement-correction.js';

const command = 'ENTITLEMENT_COMPONENT_ADJUSTMENT';
const id = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const maximum = '9223372036854775807';
const large = '9007199254740993';
function precheckJSON(wallet = maximum) {
  return `{"command_type":"${command}","policy_version":2,"snapshot_schema_version":2,"target_resource_type":"ENTITLEMENT_PACKAGE","target_resource_id":"${id(1)}","normalized_command":{"package_id":"${id(1)}","payment_order_id":"${id(2)}","reason_code":"PRODUCT_SPLIT_CORRECTION"},"critical_resource_versions":{"package_id":"${id(1)}","wallet_id":"${id(3)}","wallet_version":${wallet},"payment_order_id":"${id(2)}","payment_transaction_id":"${id(4)}","source_component_id":"${id(5)}","source_lot_id":"${id(6)}","target_component_id":null,"target_lot_id":null,"source_type":"BASE","target_type":"BONUS","amount_microcredits":${large},"source_evidence_digest":"${'a'.repeat(64)}","facts_digest":"${'b'.repeat(64)}","policy_version":2},"financial_snapshot":{"package_total_microcredits":${maximum},"source_original_microcredits":${maximum},"target_original_microcredits":0,"effective_total_microcredits":${maximum}}}`;
}
async function transport(t) {
  const state = { calls: [], version: 1, status: 200, precheck: precheckJSON(), detail: '', list: '', writeError: null };
  const server = http.createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += chunk;
    state.calls.push({ url: request.url, method: request.method, headers: request.headers, body });
    response.setHeader('Content-Type', 'application/json');
    if (request.url === '/api/admin/approvals/capability') {
      response.end(JSON.stringify({ control: { version: state.version, epoch: 1, authorization_relay_epoch: 1 }, session_approval_wire_version: 2 }));
    } else if (request.url === '/api/admin/account/admin/v2/approval-command-prechecks') {
      response.writeHead(state.status); response.end(state.status === 204 ? undefined : state.precheck);
    } else if (request.method === 'GET') {
      response.end(request.url.includes('?') ? state.list : state.detail);
    } else if (state.writeError) {
      response.writeHead(state.writeError.status); response.end(JSON.stringify({ code: state.writeError.code }));
    } else if (request.method === 'POST') {
      response.writeHead(201); response.end(JSON.stringify({ approval_request_id: id(8) }));
    } else {
      response.writeHead(204); response.end();
    }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => { server.closeAllConnections(); return new Promise(resolve => server.close(resolve)); });
  const client = new AdminPlatformClient(`http://127.0.0.1:${server.address().port}`);
  client.csrfToken = 'controlled-test-csrf';
  return { state, client };
}
const detailJSON = (wallet = maximum) => `{"approval_request_id":"${id(8)}","version":4,"action_control_version":1,"allowed_actions":["UPDATE"],"critical_resource_versions":{"wallet_version":${wallet},"amount_microcredits":${large}},"command_type":"${command}","reason":"quotes \\\" and braces {}"}`;

test('real owner precheck to create/update emits original int64 tokens and current control/CSRF', async t => {
  const { client, state } = await transport(t);
  const value = await client.entitlementCorrectionPrecheck(id(1), id(2), 'PRODUCT_SPLIT_CORRECTION');
  assert.equal(value.critical_resource_versions.wallet_version, maximum);
  assert.equal(value.critical_resource_versions.amount_microcredits, large);
  const draft = entitlementCorrectionApprovalDraft(value, 'PRODUCT_SPLIT_CORRECTION', 'Verified original payment product split', 'transport-request-key');
  assert.equal(await client.createGovernedApprovalDraft(draft), id(8));
  state.detail = detailJSON();
  const detail = await client.governedApprovalRequest(id(8));
  assert.equal(await client.updateGovernedApprovalDraft(id(8), detail, draft), null);
  const writes = state.calls.filter(call => call.method !== 'GET');
  assert.deepEqual(writes.map(call => [call.method, call.url]), [['POST', '/api/admin/account/admin/v2/approval-command-prechecks'], ['POST', '/api/admin/approvals/requests'], ['PUT', `/api/admin/approvals/requests/${id(8)}`]]);
  assert.deepEqual(JSON.parse(writes[0].body), { command_type: command, target_resource_id: id(1), payment_order_id: id(2), reason_code: 'PRODUCT_SPLIT_CORRECTION' });
  for (const call of writes) { assert.equal(call.headers['x-csrf-token'], 'controlled-test-csrf'); assert.equal(call.headers['idempotency-key'], undefined); }
  for (const call of writes.slice(1)) {
    assert.match(call.body, new RegExp(`"wallet_version":${maximum}(?=[,}])`));
    assert.match(call.body, new RegExp(`"amount_microcredits":${large}(?=[,}])`));
    assert.match(call.body, new RegExp(`"package_total_microcredits":${maximum}(?=[,}])`));
    assert.equal(call.headers['x-pinvou-approval-action-control-version'], '1');
    assert.equal(JSON.parse(call.body).idempotency_key, 'approval:transport-request-key');
  }
  assert.equal(JSON.parse(writes[2].body).expected_version, 4);
});

test('mixed current and legacy reads preserve ENT wallet tokens without changing unrelated wallet values', async t => {
  const { client, state } = await transport(t);
  state.detail = detailJSON();
  state.list = `{"items":[${state.detail},{"command_type":"LEDGER_REPAIR_POSTING","critical_resource_versions":{"wallet_version":7,"amount_microcredits":7},"allowed_actions":["APPROVE"]},{"command_type":"OTHER","wallet_version":"unrelated"}]}`;
  const current = await client.governedApprovalRequests({ command_type: command, status: 'DRAFT' });
  const legacy = await client.approvalRequests({ status: 'DRAFT' });
  for (const value of [current, legacy]) {
    assert.equal(value.items[0].critical_resource_versions.wallet_version, maximum);
    assert.equal(value.items[0].critical_resource_versions.amount_microcredits, large);
    assert.equal(value.items[1].critical_resource_versions.wallet_version, 7);
    assert.equal(value.items[2].wallet_version, 'unrelated');
  }
  assert.deepEqual(current.items[0].allowed_actions, []);
  assert.equal(current.items[1].critical_resource_versions.amount_microcredits, '7');
  assert.equal(legacy.items[1].critical_resource_versions.amount_microcredits, 7);
  assert.equal((await client.approvalRequest(id(8))).critical_resource_versions.wallet_version, maximum);
  assert.equal((await client.governedApprovalRequest(id(8))).critical_resource_versions.wallet_version, maximum);
  assert.ok(state.calls.some(call => call.url.includes('command_type=ENTITLEMENT_COMPONENT_ADJUSTMENT')));
});

test('ENT create/update encode the frozen wallet version as an integer without a precheck shortcut', async t => {
  const { client, state } = await transport(t);
  const draft = { command_type: command, critical_resource_versions: { wallet_version: maximum, amount_microcredits: large } };
  await client.createGovernedApprovalDraft(draft);
  await client.updateGovernedApprovalDraft(id(8), { approval_request_id: id(8), version: 4, action_control_version: 1 }, draft);
  for (const call of state.calls.filter(call => call.method !== 'GET')) {
    assert.match(call.body, /"wallet_version":9223372036854775807[,}]/);
    assert.match(call.body, /"amount_microcredits":9007199254740993[,}]/);
  }
  const count = state.calls.filter(call => call.method !== 'GET').length;
  for (const wallet of [Number.MAX_SAFE_INTEGER + 1, '9223372036854775808', '0', '-1', 'invalid']) {
    await assert.rejects(client.createGovernedApprovalDraft({ ...draft, critical_resource_versions: { wallet_version: wallet } }));
  }
  assert.equal(state.calls.filter(call => call.method !== 'GET').length, count);
});

test('generic EDIT precheck selects ENT codec and malformed ENT wallet fails closed', async t => {
  const { client, state } = await transport(t);
  const input = { command_type: command, target_resource_id: id(1), payment_order_id: id(2), reason_code: 'PRODUCT_SPLIT_CORRECTION' };
  assert.equal((await client.approvalCommandPrecheck(input)).critical_resource_versions.wallet_version, maximum);
  for (const wallet of ['0', '-1', '9223372036854775808', '1.5', '"invalid"']) {
    state.precheck = precheckJSON(wallet);
    await assert.rejects(client.approvalCommandPrecheck(input));
    state.detail = detailJSON(wallet);
    await assert.rejects(client.governedApprovalRequest(id(8)));
  }
});

test('precheck HTTP errors/async ACK, invalid identity and aborted signal retain existing failure boundaries', async t => {
  const { client, state } = await transport(t);
  for (const status of [202, 204]) {
    state.status = status;
    await assert.rejects(client.entitlementCorrectionPrecheck(id(1), id(2), 'CORRECTION'), /INVALID_MUTATION_ACK/);
  }
  state.status = 422; state.precheck = '{"code":"PRECHECK_REJECTED","request_id":"controlled-request"}';
  await assert.rejects(client.entitlementCorrectionPrecheck(id(1), id(2), 'CORRECTION'), error => error.status === 422 && error.code === 'PRECHECK_REJECTED' && error.requestID === 'controlled-request');
  const count = state.calls.length;
  assert.throws(() => client.entitlementCorrectionPrecheck('../foreign', id(2), 'CORRECTION'));
  const controller = new AbortController(); controller.abort();
  await assert.rejects(client.entitlementCorrectionPrecheck(id(1), id(2), 'CORRECTION', controller.signal), error => error.name === 'AbortError');
  assert.equal(state.calls.length, count);
});

test('control refresh rejects stale EDIT and server control errors clear capability', async t => {
  const { client, state } = await transport(t);
  state.detail = detailJSON();
  const detail = await client.governedApprovalRequest(id(8));
  state.version = 2;
  await assert.rejects(client.updateGovernedApprovalDraft(id(8), detail, { command_type: command }), error => error.code === 'APPROVAL_ACTION_CONTROL_CHANGED');
  assert.equal(state.calls.filter(call => call.method === 'PUT').length, 0);
  state.writeError = { status: 409, code: 'APPROVAL_ACTION_CONTROL_CHANGED' };
  await assert.rejects(client.createGovernedApprovalDraft({ command_type: command }), error => error.status === 409 && error.code === 'APPROVAL_ACTION_CONTROL_CHANGED');
  assert.equal(client.approvalControl, null);
});

test('other-command create and precheck keep their established generic codec', async t => {
  const { client, state } = await transport(t);
  state.precheck = '{"command_type":"LEDGER_REPAIR_POSTING","wallet_version":7,"amount_microcredits":9007199254740993}';
  const value = await client.approvalCommandPrecheck({ command_type: 'LEDGER_REPAIR_POSTING' });
  assert.equal(value.wallet_version, 7); assert.equal(value.amount_microcredits, large);
  await client.createGovernedApprovalDraft({ command_type: 'LEDGER_REPAIR_POSTING', critical_resource_versions: { wallet_version: 7 }, amount_microcredits: large });
  const call = state.calls.at(-1);
  assert.match(call.body, /"wallet_version":7[,}]/); assert.match(call.body, /"amount_microcredits":9007199254740993[,}]/);
});
