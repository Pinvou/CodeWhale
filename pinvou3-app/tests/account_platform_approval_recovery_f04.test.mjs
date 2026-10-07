import assert from 'node:assert/strict';
import test from 'node:test';
import { updateAndSubmitGovernedApproval, createAndSubmitFreshGovernedApproval, recoverGovernedApproval } from '../src/features/account-platform/admin-governed-approval-workflow.js';

const requestID = '11111111-1111-4111-8111-111111111111';
const targetID = '22222222-2222-4222-8222-222222222222';
const draft = { command_type: 'TOKEN_CREDENTIAL_REVOKE', target_resource_type: 'PROVIDER_CREDENTIAL', target_resource_id: targetID,
  idempotency_key: 'owned-logical-edit', reason: 'owned immutable editor intent', normalized_command: { token_account_id: targetID,
    expected_account_version: '9007199254740993', credential_id: targetID, credential_version: '3' },
  critical_resource_versions: {}, financial_snapshot: {}, redacted_evidence: [], expected_impact: {} };

// Controlled authority proves the JavaScript workflow and wire identity only.
// Real Account state-event receipt behavior is tested separately in PostgreSQL.
function authority({ lose = '', originalCreate = false } = {}) {
  let state = { approval_request_id: requestID, command_type: draft.command_type, target: { resource_type: draft.target_resource_type, resource_id: targetID },
    detail_availability: 'AVAILABLE', action_control_version: 3, version: 1, status: 'DRAFT', allowed_actions: ['EDIT', 'SUBMIT'] };
  const existing = structuredClone(state);
  const calls = { updates: [], submits: 0, creates: 0, lists: 0 };
  let saved = null;
  let lost = false;
  const client = {
    financeSessionEpoch: 1,
    governedApprovalRequests: async () => { calls.lists++; return { items: [structuredClone(state)], has_more: false }; },
    governedApprovalRequest: async () => structuredClone(state),
    updateGovernedApprovalDraft: async (id, current, input) => {
      assert.equal(id, requestID);
      assert.equal(current.action_control_version, 3);
      calls.updates.push({ expectedVersion: current.version, input: structuredClone(input) });
      const identity = JSON.stringify([current.version, input]);
      if (saved) { assert.equal(identity, saved, 'Retry must retain original CAS and full input'); return; }
      assert.equal(current.version, state.version);
      saved = identity; state = { ...state, version: state.version + 1 };
      if (lose === 'PUT' && !lost) { lost = true; throw new TypeError('controlled committed update lost acknowledgement'); }
    },
    submitGovernedApprovalRequest: async (id, current) => {
      assert.equal(id, requestID); assert.equal(current.version, state.version);
      calls.submits++; state = { ...state, version: state.version + 1, status: 'SUBMITTED', allowed_actions: [] };
      if (lose === 'SUBMIT' && !lost) { lost = true; throw new TypeError('controlled committed submit lost acknowledgement'); }
    },
    createGovernedApprovalDraft: async input => {
      calls.creates++; assert.equal(originalCreate, true, 'Recovery must not allocate a new draft for an edited existing request');
      assert.deepEqual(input, draft); return requestID;
    },
  };
  return { client, existing, calls, state: () => structuredClone(state) };
}

test('Normal edit updates once and submits the fresh version', async () => {
  const a = authority();
  const result = await updateAndSubmitGovernedApproval(a.client, a.existing, draft);
  assert.equal(result.status, 'SUBMITTED'); assert.equal(a.calls.updates.length, 1); assert.equal(a.calls.submits, 1);
});

for (const lose of ['PUT', 'SUBMIT']) {
  test(`Frozen existing edit recovers committed ${lose} lost acknowledgement with original CAS`, async () => {
    const a = authority({ lose });
    await assert.rejects(updateAndSubmitGovernedApproval(a.client, a.existing, draft), TypeError);
    const result = await updateAndSubmitGovernedApproval(a.client, a.existing, draft);
    assert.equal(result.status, 'SUBMITTED'); assert.equal(a.calls.submits, 1);
    assert.deepEqual(a.calls.updates.map(x => x.expectedVersion), lose === 'SUBMIT' ? [1, 1, 1] : [1, 1]); assert.equal(a.calls.creates, 0);
  });
  test(`Fresh approval entry retains its first edit CAS after committed ${lose} lost acknowledgement`, async () => {
    const a = authority({ lose });
    await assert.rejects(createAndSubmitFreshGovernedApproval(a.client, draft), TypeError);
    const result = await createAndSubmitFreshGovernedApproval(a.client, draft);
    assert.equal(result.status, 'SUBMITTED'); assert.equal(a.calls.submits, 1);
    assert.deepEqual(a.calls.updates.map(x => x.expectedVersion), lose === 'SUBMIT' ? [1, 1, 1] : [1, 1]); assert.equal(a.calls.lists, 1); assert.equal(a.calls.creates, 0);
  });
}

test('Fresh invocation rejects changed frozen facts without dispatching another update', async () => {
  const a = authority({ lose: 'PUT' });
  await assert.rejects(createAndSubmitFreshGovernedApproval(a.client, draft), TypeError);
  await assert.rejects(createAndSubmitFreshGovernedApproval(a.client, { ...draft, reason: 'another changed intent' }), { code: 'APPROVAL_FACTS_CHANGED' });
  assert.equal(a.calls.updates.length, 1); assert.equal(a.calls.submits, 0);
});

test('Reference recovery uses the retained edit receipt rather than creating another approval', async () => {
  const a = authority({ lose: 'PUT' });
  await assert.rejects(createAndSubmitFreshGovernedApproval(a.client, draft), TypeError);
  const result = await recoverGovernedApproval(a.client, requestID, draft);
  assert.equal(result.status, 'SUBMITTED'); assert.equal(a.calls.creates, 0); assert.equal(a.calls.submits, 1);
});

test('Lost first acknowledgement without reference replays the same create invocation', async () => {
  const a = authority({ originalCreate: true });
  // Authoritative history says the original same-key draft is already active.
  a.client.governedApprovalRequests = async () => ({ items: [{ ...a.existing, status: 'SUBMITTED' }] });
  a.client.governedApprovalRequest = async () => ({ ...a.existing, version: 2, status: 'SUBMITTED', allowed_actions: [] });
  const result = await recoverGovernedApproval(a.client, '', draft);
  assert.equal(result.status, 'SUBMITTED'); assert.equal(a.calls.creates, 2); assert.equal(a.calls.submits, 0);
});

test('Reference mismatch cannot edit or submit the other request', async () => {
  const a = authority();
  a.client.governedApprovalRequests = async () => ({ items: [{ ...a.existing, approval_request_id: targetID }] });
  await assert.rejects(recoverGovernedApproval(a.client, requestID, draft), { code: 'COMMAND_RESULT_UNKNOWN' });
  assert.equal(a.calls.updates.length, 0); assert.equal(a.calls.submits, 0); assert.equal(a.calls.creates, 0);
});

test('Terminal non-success recovery does not issue a write', async () => {
  const a = authority();
  a.client.governedApprovalRequest = async () => ({ ...a.existing, status: 'WITHDRAWN', allowed_actions: [] });
  await assert.rejects(recoverGovernedApproval(a.client, requestID, draft), { finalResult: true, code: 'APPROVAL_WITHDRAWN' });
  assert.equal(a.calls.updates.length, 0); assert.equal(a.calls.creates, 0);
});


test('An interleaved foreign edit followed by submit cannot be reported as this invocation', async () => {
  const a = authority();
  const originalUpdate = a.client.updateGovernedApprovalDraft;
  let edited = false;
  let authorityChecks = 0;
  a.client.updateGovernedApprovalDraft = async (...args) => {
    authorityChecks++;
    if (edited) throw Object.assign(new Error('current caller fingerprint differs'), { status: 409, code: 'APPROVAL_FACTS_CHANGED' });
    return originalUpdate(...args);
  };
  const originalRead = a.client.governedApprovalRequest;
  let reads = 0;
  a.client.governedApprovalRequest = async () => {
    reads++;
    if (reads === 2) {
      edited = true;
      return { ...a.existing, version: 4, status: 'SUBMITTED', allowed_actions: [], command: { ...draft.normalized_command, credential_version: '2' } };
    }
    return originalRead();
  };
  await assert.rejects(updateAndSubmitGovernedApproval(a.client, a.existing, draft), { code: 'APPROVAL_FACTS_CHANGED' });
  assert.equal(authorityChecks, 2); assert.equal(a.calls.submits, 0);
});


test('Create replay cannot adopt an interleaved foreign edited active intent', async () => {
  const a = authority({ originalCreate: true });
  let creates = 0;
  a.client.governedApprovalRequests = async () => ({ items: [{ ...a.existing, status: 'SUBMITTED' }] });
  a.client.createGovernedApprovalDraft = async () => {
    creates++;
    if (creates > 1) throw Object.assign(new Error('original create fingerprint changed after foreign edit'), { status: 409, code: 'APPROVAL_FACTS_CHANGED' });
    return requestID;
  };
  a.client.governedApprovalRequest = async () => ({ ...a.existing, version: 4, status: 'SUBMITTED', allowed_actions: [], command: { ...draft.normalized_command, credential_version: '2' } });
  await assert.rejects(createAndSubmitFreshGovernedApproval(a.client, draft), { code: 'APPROVAL_FACTS_CHANGED' });
  assert.equal(creates, 2); assert.equal(a.calls.submits, 0);
});
