import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { AdminFinanceWorkspace, Approvals } from '../../src/features/account-platform/AdminFinanceWorkspace.jsx';
import { useFinanceOperation } from '../../src/features/account-platform/useFinanceOperation.js';
import { financeCopy } from '../../src/shared/i18n.js';

const id = '11111111-1111-4111-8111-111111111111';
const state = { id, calls: [], language: 'en', kind: 'hook', historyIncomplete: true, unknown: false };
const client = new AdminPlatformClient('');
const root = createRoot(document.getElementById('root'));
const json = (value, status = 200) => new Response(JSON.stringify(value), { status });
globalThis.fetch = async (url, options = {}) => {
  state.calls.push({ url, method: options.method || 'GET', body: options.body, key: options.headers?.['Idempotency-Key'] });
  if (url.endsWith('/capability')) return json({ control: { version: 2, epoch: 1, authorization_relay_epoch: 1 }, session_approval_wire_version: 2 });
  if (url.includes('/wallets?')) return state.readStatus ? json({ code: state.readStatus === 403 ? 'PERMISSION_DENIED' : 'UPSTREAM_UNAVAILABLE' }, state.readStatus) : json({ items: [] });
  if (url.includes('/adjustments?')) return json({ items: state.created ? [state.created] : [] });
  if (url.endsWith('/adjustments')) {
    const input = JSON.parse(options.body);
    state.created = { ...input, amount_microcredits: String(input.amount_microcredits), adjustment_id: id, requested_by: id, request_idempotency_key: input.idempotency_key, version: 1, status: 'PENDING_APPROVAL' };
    return json(state.created, 201);
  }
  if (url.includes('/requests?')) return state.kind === 'approvals' ? json({ items: [id, '22222222-2222-4222-8222-222222222222'].map(approval_request_id => ({ approval_request_id, command_type: 'WALLET_ADJUSTMENT', status: 'DRAFT' })) }) : json({ items: [], has_more: state.historyIncomplete });
  if (url.endsWith('/api/admin/approvals/requests')) return json({ approval_request_id: id }, 201);
  if (url.endsWith('/actions/submit')) { state.submitted = true; return new Response(null, { status: 204 }); }
  if (url.endsWith(`/requests/${id}`)) return json({ approval_request_id: id, command_type: 'WALLET_ADJUSTMENT', target: { resource_type: 'MANUAL_ADJUSTMENT', resource_id: id }, status: state.submitted ? 'SUBMITTED' : 'DRAFT', version: state.submitted ? 2 : 1, action_control_version: 2, detail_availability: 'AVAILABLE', allowed_actions: state.submitted ? [] : ['SUBMIT'] });
  if (url.endsWith('/requests/22222222-2222-4222-8222-222222222222')) return json({ approval_request_id: '33333333-3333-4333-8333-333333333333', command_type: 'WALLET_ADJUSTMENT', status: 'DRAFT', version: 1, action_control_version: 2, allowed_actions: [] });
  if (url.endsWith('/actions/decide')) {
    if (state.transportFailure) throw new TypeError('Controlled POST transport reset');
    return new Response(null, { status: state.badAck ? 200 : 204 });
  }
  if (url.endsWith('/recharge-products')) {
    if (state.transportFailure) throw new TypeError('Controlled POST transport reset');
    return json({ catalog_item_id: id }, state.badAck ? 200 : 201);
  }
  throw new Error(`Unexpected controlled route: ${url}`);
};

function Hook() {
  const operation = useFinanceOperation(id, financeCopy[state.language], client, 'controlled-dispatch');
  state.operation = operation;
  return <><p data-unknown>{String(operation.unknown)}</p><p role="alert">{operation.error}</p></>;
}
function render() {
  const me = { user_id: id, platform_authorization_version: 1, platform_permissions: ['platform.billing.wallets.read', 'platform.billing.repairs.request'] };
  root.render(state.kind === 'wallet' ? <AdminFinanceWorkspace {...{ client, me }} language={state.language} initialTab="wallets" /> : state.kind === 'approvals' ? <Approvals client={client} copy={financeCopy[state.language]} permissions={[]} owner={id} /> : <Hook />);
}
state.client = client;
state.copy = () => financeCopy[state.language];
state.render = (kind, language = 'en') => { state.kind = kind; state.language = language; render(); };
window.f02Dispatch = state;
render();
