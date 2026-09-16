import assert from 'node:assert/strict';
import test from 'node:test';
import { approvalActionsForDetail, approvalBusinessRoute, approvalDecisionForAction } from '../../../pinvou3-app/src/features/account-platform/admin-approval-actions.js';

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

test('only closed matching v2 commands link back to their owning business area', () => {
  const id = '0199a300-0001-7000-8000-000000000017';
  const detail = (commandType, type, value, command) => ({ detail_availability: 'AVAILABLE', command_type: commandType,
    target: { resource_type: type, resource_id: value }, command });
  const wallet = detail('WALLET_ADJUSTMENT', 'MANUAL_ADJUSTMENT', id, { adjustment_id: id });
  assert.deepEqual(approvalBusinessRoute(wallet), { section: 'billing', query: { finance_tab: 'adjustments' } });
  assert.deepEqual(approvalBusinessRoute(detail('PAYMENT_PROVIDER_RESUME', 'PAYMENT_PROVIDER_CONTROL', 'provider', { provider_id: 'provider' })),
    { section: 'payments', query: { finance_tab: 'controls' } });
  assert.deepEqual(approvalBusinessRoute(detail('PAYMENT_PRODUCT_RESUME', 'PAYMENT_PRODUCT_CONTROL', 'product', { product_id: 'product' })),
    { section: 'payments', query: { finance_tab: 'controls' } });
  const model = detail('TOKEN_MODEL_LIFECYCLE_CHANGE', 'MODEL', id, { model_id: id });
  assert.deepEqual(approvalBusinessRoute(model, { modelDetailsEnabled: true }), { section: 'models', query: { model_id: id } });
  assert.deepEqual(approvalBusinessRoute(model), { section: 'models', query: {} });
  assert.deepEqual(approvalBusinessRoute(detail('TOKEN_MODEL_CATALOG_ROLLBACK', 'MODEL_CATALOG', '12', { source_catalog_version: '12' })),
    { section: 'models', query: {} });
  const credential = detail('TOKEN_CREDENTIAL_REVOKE', 'PROVIDER_CREDENTIAL', id, { credential_id: id, token_account_id: id });
  assert.deepEqual(approvalBusinessRoute(credential, { tokenDetailsEnabled: true }), { section: 'tokenResources', query: { token_account_id: id } });
  assert.equal(approvalBusinessRoute({ ...wallet, detail_availability: 'DETAIL_PRESENTATION_UNAVAILABLE' }), null);
  assert.equal(approvalBusinessRoute({ ...wallet, command: { adjustment_id: 'other' } }), null);
  assert.equal(approvalBusinessRoute({ ...credential, command: { credential_id: id, token_account_id: 'unsafe' } }), null);
});
