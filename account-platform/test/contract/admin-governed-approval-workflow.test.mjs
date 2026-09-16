import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { createAndSubmitGovernedApproval, recoverGovernedApproval } from '../../../pinvou3-app/src/features/account-platform/admin-governed-approval-workflow.js';

const requestID = '0199a300-0001-7000-8000-000000000017';
const draft = { command_type: 'TOKEN_MODEL_CATALOG_ROLLBACK', target_resource_type: 'MODEL_CATALOG', target_resource_id: '12' };
const detail = (status, version = 1) => ({ approval_request_id: requestID, command_type: draft.command_type,
  target: { resource_type: draft.target_resource_type, resource_id: draft.target_resource_id },
  status, version, action_control_version: 9, detail_availability: 'AVAILABLE', allowed_actions: status === 'DRAFT' ? ['SUBMIT'] : [] });

test('governed business workflow reads fresh detail, submits once, and reads outcome', async () => {
  const calls = [];
  const client = {
    createGovernedApprovalDraft: async (value) => { assert.equal(value, draft); calls.push('create'); return requestID; },
    governedApprovalRequest: async (id) => { assert.equal(id, requestID); calls.push('read'); return detail(calls.length < 4 ? 'DRAFT' : 'SUBMITTED', calls.length < 4 ? 1 : 2); },
    submitGovernedApprovalRequest: async (id, value) => { assert.equal(id, requestID); assert.equal(value.status, 'DRAFT'); calls.push('submit'); },
  };
  assert.equal((await createAndSubmitGovernedApproval(client, draft)).status, 'SUBMITTED');
  assert.deepEqual(calls, ['create', 'read', 'submit', 'read']);
});

test('an already submitted idempotent request is never submitted again', async () => {
  const client = { createGovernedApprovalDraft: async () => requestID,
    governedApprovalRequest: async () => detail('SUBMITTED', 2),
    submitGovernedApprovalRequest: async () => assert.fail('must not submit twice') };
  assert.equal((await createAndSubmitGovernedApproval(client, draft)).status, 'SUBMITTED');
});

test('edited or mismatched drafts fail closed with original request reference', async () => {
  for (const value of [detail('DRAFT', 2), { ...detail('DRAFT'), target: { resource_type: 'MODEL_CATALOG', resource_id: '13' } }]) {
    const client = { createGovernedApprovalDraft: async () => requestID,
      governedApprovalRequest: async () => value,
      submitGovernedApprovalRequest: async () => assert.fail('must not submit stale or mismatched draft') };
    await assert.rejects(createAndSubmitGovernedApproval(client, draft), { commandReference: requestID });
  }
});

test('unknown submit result never retries and recovery reads only the same request', async () => {
  let submitted = 0;
  const client = { createGovernedApprovalDraft: async () => requestID,
    governedApprovalRequest: async () => detail('DRAFT'),
    submitGovernedApprovalRequest: async () => { submitted++; throw Object.assign(new Error('network lost'), { status: 0 }); } };
  await assert.rejects(createAndSubmitGovernedApproval(client, draft), { commandReference: requestID });
  assert.equal(submitted, 1);
  await assert.rejects(recoverGovernedApproval(client, requestID, draft), { code: 'COMMAND_RESULT_UNKNOWN', commandReference: requestID });
  assert.equal(submitted, 1);
  client.governedApprovalRequest = async () => detail('EXECUTED', 3);
  assert.equal((await recoverGovernedApproval(client, requestID, draft)).status, 'EXECUTED');
  client.governedApprovalRequest = async () => detail('EXECUTION_FAILED', 3);
  assert.equal((await recoverGovernedApproval(client, requestID, draft)).status, 'EXECUTION_FAILED');
});

test('malformed action projection is not accepted as a fresh governed draft', async () => {
  const client = { createGovernedApprovalDraft: async () => requestID,
    governedApprovalRequest: async () => ({ ...detail('DRAFT'), allowed_actions: 'SUBMIT' }),
    submitGovernedApprovalRequest: async () => assert.fail('must not submit malformed detail') };
  await assert.rejects(createAndSubmitGovernedApproval(client, draft),
    { code: 'INVALID_APPROVAL_RESPONSE', commandReference: requestID });
});

test('legacy wire still requires the complete original draft contract', async () => {
  const client = { createGovernedApprovalDraft: async () => requestID,
    governedApprovalRequest: async () => { const value = detail('DRAFT'); delete value.detail_availability; return value; },
    submitGovernedApprovalRequest: async () => assert.fail('must not submit incomplete legacy detail') };
  await assert.rejects(createAndSubmitGovernedApproval(client, draft),
    { code: 'INVALID_APPROVAL_RESPONSE', commandReference: requestID });
});

test('all Token business approval entry points use the session-governed workflow', () => {
  for (const name of ['AdminModelTokenWorkspace.jsx', 'AdminModelDetailWorkspace.jsx', 'AdminTokenAccountDetailWorkspace.jsx']) {
    const source = readFileSync(new URL(`../../../pinvou3-app/src/features/account-platform/${name}`, import.meta.url), 'utf8');
    assert.match(source, /createAndSubmitGovernedApproval\(client, draft, signal\)/, name);
    assert.match(source, /recoverGovernedApproval\(client, reference, draft, signal\)/, name);
    assert.doesNotMatch(source, /client\.createApprovalDraft\(|client\.submitApprovalRequest\(/, name);
  }
});
