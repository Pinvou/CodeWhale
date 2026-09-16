import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { createAndSubmitFreshGovernedApproval, createAndSubmitGovernedApproval, recoverGovernedApproval, submitExistingGovernedApproval, supersedesEligibleApproval, updateAndSubmitGovernedApproval } from '../../../pinvou3-app/src/features/account-platform/admin-governed-approval-workflow.js';

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
    assert.match(source, /createAndSubmitFreshGovernedApproval\(client, draft, signal\)/, name);
    assert.match(source, /recoverGovernedApproval\(client, reference, draft, signal\)/, name);
    assert.doesNotMatch(source, /client\.createApprovalDraft\(|client\.submitApprovalRequest\(/, name);
  }
});

test('fresh Token approval refuses incomplete or active history and links an eligible terminal request', async () => {
  const oldID = '0199a300-0001-7000-8000-000000000018';
  const old = (status) => ({ approval_request_id: oldID, command_type: draft.command_type,
    target: { resource_id: draft.target_resource_id }, status });
  for (const status of ['DRAFT', 'SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTION_FAILED']) {
    assert.throws(() => supersedesEligibleApproval({ items: [old(status)] }, draft.command_type, draft.target_resource_id),
      { code: 'APPROVAL_ALREADY_ACTIVE' });
  }
  assert.equal(supersedesEligibleApproval({ items: [old('REJECTED')] }, draft.command_type, draft.target_resource_id), oldID);
  assert.equal(supersedesEligibleApproval({ items: [old('EXECUTED')] }, draft.command_type, draft.target_resource_id), '');
  assert.throws(() => supersedesEligibleApproval({ items: [old('REJECTED')], has_more: true }, draft.command_type, draft.target_resource_id),
    { code: 'APPROVAL_HISTORY_INCOMPLETE' });
  assert.throws(() => supersedesEligibleApproval({ items: [old('REJECTED')], next_cursor: 'more' }, draft.command_type, draft.target_resource_id),
    { code: 'APPROVAL_HISTORY_INCOMPLETE' });
  let created;
  const client = { governedApprovalRequests: async () => ({ items: [old('WITHDRAWN')] }),
    createGovernedApprovalDraft: async (value) => { created = value; return requestID; },
    governedApprovalRequest: async () => detail('SUBMITTED', 2) };
  await createAndSubmitFreshGovernedApproval(client, draft);
  assert.equal(created.supersedes_request_id, oldID);
  client.governedApprovalRequests = async () => ({ items: [old('SUBMITTED')] });
  client.createGovernedApprovalDraft = async () => assert.fail('must not create while prior approval is active');
  await assert.rejects(createAndSubmitFreshGovernedApproval(client, draft), { code: 'APPROVAL_ALREADY_ACTIVE' });
});

test('existing business draft is re-read and never submitted from a stale list version', async () => {
  const existing = { approval_request_id: requestID, command_type: draft.command_type,
    target_resource_type: draft.target_resource_type, target_resource_id: draft.target_resource_id, status: 'DRAFT', version: 1 };
  let reads = 0;
  let submits = 0;
  const client = { governedApprovalRequest: async () => ++reads === 1 ? detail('DRAFT', 1) : detail('SUBMITTED', 2),
    submitGovernedApprovalRequest: async (id, current) => { assert.equal(id, requestID); assert.equal(current.version, submits + 1); submits++; } };
  assert.equal((await submitExistingGovernedApproval(client, existing)).status, 'SUBMITTED');
  assert.equal(reads, 2);
  assert.equal(submits, 1);
  client.governedApprovalRequest = async () => ({ ...detail('DRAFT', 2), allowed_actions: [] });
  await assert.rejects(submitExistingGovernedApproval(client, existing), { code: 'COMMAND_RESULT_UNKNOWN', commandReference: requestID });
  assert.equal(submits, 1);
  client.governedApprovalRequest = async () => ++reads === 3 ? detail('DRAFT', 2) : detail('SUBMITTED', 3);
  assert.equal((await submitExistingGovernedApproval(client, existing)).version, 3);
  assert.equal(submits, 2);
});

test('business edit CAS-updates the same draft and submits only after a fresh version read', async () => {
  const existing = { approval_request_id: requestID, command_type: draft.command_type,
    target: { resource_type: draft.target_resource_type, resource_id: draft.target_resource_id } };
  const calls = [];
  const client = {
    governedApprovalRequest: async () => {
      calls.push('read');
      return calls.length === 1 ? { ...detail('DRAFT'), allowed_actions: ['EDIT'] }
        : calls.length === 3 ? detail('DRAFT', 2) : detail('SUBMITTED', 3);
    },
    updateGovernedApprovalDraft: async (id, value, edited) => {
      assert.equal(id, requestID); assert.equal(value.version, 1); assert.equal(edited, draft); calls.push('update');
    },
    submitGovernedApprovalRequest: async (id, value) => {
      assert.equal(id, requestID); assert.equal(value.version, 2); calls.push('submit');
    },
  };
  assert.equal((await updateAndSubmitGovernedApproval(client, existing, draft)).status, 'SUBMITTED');
  assert.deepEqual(calls, ['read', 'update', 'read', 'submit', 'read']);
});

test('business edit never retries an uncertain update or submits an unconfirmed draft', async () => {
  const existing = { approval_request_id: requestID, command_type: draft.command_type,
    target: { resource_type: draft.target_resource_type, resource_id: draft.target_resource_id } };
  let updates = 0;
  const client = {
    governedApprovalRequest: async () => ({ ...detail('DRAFT'), allowed_actions: ['EDIT'] }),
    updateGovernedApprovalDraft: async () => { updates++; throw new Error('network lost'); },
    submitGovernedApprovalRequest: async () => assert.fail('must not submit after unknown update'),
  };
  await assert.rejects(updateAndSubmitGovernedApproval(client, existing, draft), { commandReference: requestID });
  assert.equal(updates, 1);
  client.updateGovernedApprovalDraft = async () => { updates++; };
  await assert.rejects(updateAndSubmitGovernedApproval(client, existing, draft), { code: 'COMMAND_RESULT_UNKNOWN', commandReference: requestID });
  assert.equal(updates, 2);
});

test('wallet adjustment approval keeps its existing Billing creation but uses governed Account writes', () => {
  const source = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  assert.match(source, /const ensureAdjustmentApproval = async[\s\S]*?return createAndSubmitGovernedApproval\(client, draft\)/);
  assert.doesNotMatch(source, /client\.createApprovalDraft\(|client\.submitApprovalRequest\(/);
  assert.match(source, /created = await client\.createAdjustment\(/);
});
