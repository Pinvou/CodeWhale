import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminPlatformView } from '../../src/features/account-platform/AdminPlatformView.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { encodeFinanceJSON, parseFinanceJSON } from '../../src/features/account-platform/finance-contract.js';
import { financeCopy, ledgerRepairCopy, adminPlatformCopy } from '../../src/shared/i18n.js';
const id = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const params = new URLSearchParams(location.search), language = params.get('language') || 'zh';
const projection = digest => ({ command_type: 'RECEIVABLE_WRITE_OFF', policy_version: 1, snapshot_schema_version: 1,
  target_resource_type: 'RECEIVABLE', target_resource_id: id(1), normalized_command: { receivable_id: id(1), write_off_microcredits: '9007199254740993', expected_receivable_version: '9223372036854775806', reason_code: 'APPROVED_RECEIVABLE_WRITE_OFF' },
  critical_resource_versions: { receivable_version: '9223372036854775806', facts_digest: digest.repeat(64), policy_version: 1 },
  financial_snapshot: { write_off_microcredits: '9007199254740993', outstanding_microcredits: '9223372036854775807', settled_microcredits: '0', original_microcredits: '9223372036854775807', source_type: 'USAGE_OVERAGE', source_reference: 'usage:controlled', wallet_receivable_microcredits: '9223372036854775807' } });
const state = window.f11 = { calls: [], id, language, financeCopy, ledgerRepairCopy, adminPlatformCopy, created: params.has('existing'),
  status: 'DRAFT', version: 1, prechecks: 0, lose: params.has('lost'), loseEdit: params.has('edit-lost'), denyReplay: params.has('lost'), reason: 'Controlled write-off justification', key: 'approval:controlled-write-off', command: projection('a').normalized_command };
const client = new AdminPlatformClient(); client.csrfToken = 'controlled-csrf';
const root = createRoot(document.getElementById('root'));
state.replaceIdentity = () => { const next = new AdminPlatformClient(); next.csrfToken = 'controlled-next'; root.render(<AdminPlatformView client={next} language={language} copy={adminPlatformCopy[language]} onLanguageChange={() => {}} />); };
const reply = (body, status = 200, headers = {}) => new Response(status === 204 ? null : encodeFinanceJSON(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });
const original = window.fetch.bind(window);
window.fetch = async (url, options = {}) => {
  if (!String(url).startsWith('/api/')) return original(url, options);
  state.calls.push({ url: String(url), method: options.method || 'GET', body: options.body, headers: options.headers, cache: options.cache, credentials: options.credentials });
  if (url === '/api/session') return reply({ authenticated: true, csrf_token: 'controlled-csrf' });
  if (url === '/api/account/v1/account/bootstrap') return reply({ user_id: params.has('approve') ? id(7) : id(8), status: 'ACTIVE', personal_permissions: [],
    platform_permissions: params.has('deny') ? [] : params.has('approve') ? ['platform.billing.repairs.approve'] : ['platform.billing.repairs.request'], platform_authorization_version: 1 });
  if (url === '/api/admin/approvals/capability') return reply({ session_approval_wire_version: 2, control: { version: 1, epoch: 1, authorization_relay_epoch: 1 } });
  if (url === '/api/admin/approvals/directory') return reply({ schema_version: 1, platform_authorization_version: 1, items: [
    { command_type: 'LEDGER_REPAIR', policy_version: 1, risk_class: 'FINANCIAL_FACT_CHANGE', requestable: true },
    { command_type: 'ENTITLEMENT_COMPONENT_ADJUSTMENT', policy_version: 2, risk_class: 'FINANCIAL_FACT_CHANGE', requestable: true },
    { command_type: 'RECEIVABLE_WRITE_OFF', policy_version: 1, risk_class: 'FINANCIAL_FACT_CHANGE', requestable: true },
    { command_type: 'RECONCILIATION_FINANCIAL_REPAIR', policy_version: 1, risk_class: 'FINANCIAL_FACT_CHANGE', requestable: true },
  ] }, 200, { 'X-Platform-Authorization-Version': '1' });
  if (url === '/api/admin/account/admin/v2/approval-command-prechecks') {
    state.prechecks++;
    if (params.has('unavailable')) return reply({ code: 'APPROVAL_PRECHECK_UNAVAILABLE' }, 503);
    const facts = projection(state.prechecks > 1 ? 'b' : 'a');
    if (params.has('mismatch')) facts.target_resource_id = id(2);
    if (params.has('late')) return new Promise(resolve => { state.releasePrecheck = () => resolve(reply(facts)); });
    return reply(facts);
  }
  if (url === '/api/admin/approvals/requests' && options.method === 'POST') {
    if (state.created && state.denyReplay && !state.lose) return reply({ code: 'FORBIDDEN' }, 403);
    if (params.has('initial-deny')) return reply({ code: 'FORBIDDEN' }, 403);
    state.created = true; const draft = parseFinanceJSON(options.body); state.reason = draft.reason; state.key = draft.idempotency_key; state.command = draft.normalized_command;
    if (state.lose) { state.lose = false; throw new TypeError('controlled committed POST lost ACK'); }
    return reply({ approval_request_id: id(9) }, 201);
  }
  if (String(url).startsWith('/api/admin/approvals/requests?')) return reply({ items: state.created ? [{ approval_request_id: id(9), command_type: 'RECEIVABLE_WRITE_OFF', requester: { user_id: id(8) }, status: state.status, version: state.version, target: { resource_type: 'RECEIVABLE', resource_id: id(1) } }] : [], has_more: false });
  if (url === `/api/admin/approvals/requests/${id(9)}` && options.method === 'PUT') {
    if (state.denyReplay) return reply({ code: 'FORBIDDEN' }, 403);
    const draft = parseFinanceJSON(options.body); if (draft.expected_version === state.version) state.version++;
    state.reason = draft.reason; state.command = draft.normalized_command;
    if (state.loseEdit) { state.loseEdit = false; throw new TypeError('controlled committed EDIT lost ACK'); }
    return reply({}, 204);
  }
  if (url === `/api/admin/approvals/requests/${id(9)}/actions/submit`) { state.version++; state.status = 'SUBMITTED'; return reply({}, 204); }
  if (url === `/api/admin/approvals/requests/${id(9)}/actions/decide`) { state.status = 'EXECUTED'; return reply({}, 204); }
  if (url === `/api/admin/approvals/requests/${id(9)}`) return reply({ approval_request_id: id(9), command_type: 'RECEIVABLE_WRITE_OFF', status: state.status, version: state.version,
    requester: { user_id: id(8) }, action_control_version: 1, detail_availability: 'AVAILABLE', allowed_actions: state.status === 'DRAFT' ? ['EDIT', 'SUBMIT', 'WITHDRAW'] : ['APPROVE', 'REJECT'],
    target: { resource_type: 'RECEIVABLE', resource_id: id(1) }, command: state.command, reason: state.reason, critical_resource_versions: projection('a').critical_resource_versions,
    financial_snapshot: projection('a').financial_snapshot, execution: state.status === 'EXECUTED' ? { attempt_status: 'EXECUTED' } : undefined });
  throw new Error(`Uncontrolled production request: ${url}`);
};
root.render(<AdminPlatformView client={client} language={language} copy={adminPlatformCopy[language]} onLanguageChange={() => {}} />);
