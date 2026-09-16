import assert from 'node:assert/strict';
import test from 'node:test';
import { approvalActionsForDetail, approvalDecisionForAction } from '../../../pinvou3-app/src/features/account-platform/admin-approval-actions.js';

test('approval center offers only detail-authorized actions and never self-approves', () => {
  const submitted = { requester: { user_id: 'owner' }, status: 'SUBMITTED', version: 2,
    allowed_actions: ['APPROVE', 'REJECT', 'WITHDRAW', 'APPROVE'] };
  assert.deepEqual(approvalActionsForDetail(submitted, 'owner'), ['WITHDRAW']);
  assert.deepEqual(approvalActionsForDetail(submitted, 'approver'), ['APPROVE', 'REJECT']);
  assert.deepEqual(approvalActionsForDetail({ ...submitted, detail_availability: 'DETAIL_PRESENTATION_UNAVAILABLE' }, 'approver'), []);
  assert.deepEqual(approvalActionsForDetail({ ...submitted, allowed_actions: undefined }, 'approver'), []);
  const draft = { requester_user_id: 'owner', status: 'DRAFT', version: 1, allowed_actions: ['SUBMIT', 'EDIT'] };
  assert.deepEqual(approvalActionsForDetail(draft, 'owner'), ['SUBMIT']);
  assert.deepEqual(approvalActionsForDetail({ ...draft, version: 2 }, 'owner'), []);
});

test('detail action names map to decision wire values without entering submit', () => {
  assert.equal(approvalDecisionForAction('APPROVE'), 'APPROVED');
  assert.equal(approvalDecisionForAction('REJECT'), 'REJECTED');
  for (const action of ['SUBMIT', 'WITHDRAW', 'APPROVED', '', undefined]) {
    assert.equal(approvalDecisionForAction(action), null);
  }
});
