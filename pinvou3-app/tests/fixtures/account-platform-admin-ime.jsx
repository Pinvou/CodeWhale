import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { AdminHighRiskActionDialog } from '../../src/features/account-platform/AdminHighRiskActionDialog.jsx';
// These aliases exist only in this smoke's Vite transform. Product exports and
// handlers are unchanged; the test mounts the original private React components.
import { ImeProtocolDialog } from '../../src/features/account-platform/AdminProtocolFormulaWorkspace.jsx';
import { ImeOwnerDialog, ImeRecoveryDialog } from '../../src/features/account-platform/AdminTokenResourcesWorkspace.jsx';
import { ImeCredentialLaunchDialog } from '../../src/features/account-platform/AdminTokenAccountDetailWorkspace.jsx';
import { ImeRouteDialog } from '../../src/features/account-platform/TokenAccountGovernanceActions.jsx';
import { AccountClosureConfirmation } from '../../src/features/account-platform/AccountClosureConfirmation.jsx';
import { tokenManagementCopy } from '../../src/shared/i18n.js';

const copy = tokenManagementCopy.en.modelToken;
const control = { closed: 0, queries: [], submissions: 0, formSubmits: [], keyEvents: [], confirms: [], revokes: [], resultQueries: [] };
const account = { token_account_id: '11111111-1111-4111-8111-111111111111', account_version: 1, name: 'Controlled account' };
const launch = { launch_id: '22222222-2222-4222-8222-222222222222', result_ref: '33333333-3333-4333-8333-333333333333', token_account_id: account.token_account_id, action: 'ENROLL', status: 'CREATED', expires_at: '2099-01-01T00:00:00Z', audit_request_id: 'controlled-audit' };
window.adminIme = control;
// Observe native form default actions independently of execute/recovery calls.
document.addEventListener('submit', event => { control.formSubmits.push({ trusted: event.isTrusted, labelledBy: event.target.getAttribute('aria-labelledby') }); }, true);
const root = createRoot(document.getElementById('root'));
const histories = { current: {} };
const client = {
  catalogModels: async query => { control.queries.push(query); return { items: [], has_more: false, next_cursor: '' }; },
  createCredentialLaunch: () => { control.submissions++; return new Promise(resolve => { control.resolveLaunch = () => resolve(launch); }); },
  revokeCredentialWindow: async (reference, reason) => { control.revokes.push({ reference, reason }); throw new Error('controlled lost revoke acknowledgement'); },
  credentialIsolationResult: async reference => { control.resultQueries.push(reference); return { result_ref: launch.result_ref, token_account_id: account.token_account_id, action: 'ENROLL', status: control.terminal ? 'REVOKED' : 'PENDING', audit_request_id: 'controlled-audit', window_confirmed: true, updated_at: '2026-10-28T00:00:00Z', ...(control.terminal ? { error_code: 'CREDENTIAL_LAUNCH_CANCELLED' } : {}) }; },
  tokenOwnerCandidates: async () => ({ items: [{ id: '22222222-2222-4222-8222-222222222222', type: 'USER', status: 'ACTIVE', display_label: 'Controlled owner' }] }),
  createTokenAccountRecoveryAction: () => pending(),
};
function pending() {
  control.submissions++;
  return new Promise((_resolve, reject) => { control.reject = () => reject(Object.assign(new Error('controlled lost acknowledgement'), { status: 503 })); });
}
function ClosureFixture() {
  const [open, setOpen] = useState(false);
  return <><button data-testid="closure-opener" onClick={() => setOpen(true)}>Open controlled consent</button>{open && <AccountClosureConfirmation copy={{ title: 'Controlled consent', confirmPrompt: 'Type CLOSE_MY_ACCOUNT', confirmInput: 'Confirmation', cancelConfirmation: 'Cancel', confirmAction: 'Confirm' }} onCancel={() => { control.closed++; setOpen(false); }} onConfirm={text => control.confirms.push(text)} canRestoreFocus={() => true} />}</>;
}
function Fixture({ kind }) {
  const [open, setOpen] = useState(true);
  const [mode, setMode] = useState('ready');
  const [query, setQuery] = useState({ owner_type: 'USER', status: 'ACTIVE', q: '' });
  control.setMode = setMode;
  const onClose = () => { control.closed++; setOpen(false); };
  if (kind === 'closure') return <ClosureFixture />;
  if (!open) return <p data-testid="closed">Closed</p>;
  if (kind === 'credential') return <ImeCredentialLaunchDialog client={client} copy={copy} account={account} action="ENROLL" enabled onClose={onClose} onCompleted={() => {}} />;
  if (kind === 'route') return <ImeRouteDialog client={client} copy={copy} account={account} onClose={onClose} onSaved={() => {}} />;
  if (kind === 'high-risk') return <AdminHighRiskActionDialog action={{ key: 'ime-action', title: 'Controlled action' }} copy={copy} target={{ label: 'Controlled target' }} impactRequired={false} execute={pending} onClose={onClose} />;
  if (kind === 'protocol') return <ImeProtocolDialog action={{ type: 'PROTOCOL_DRAFT', ...(mode === 'unknown' ? { failure: { unknown: true, code: 'COMMAND_RESULT_UNKNOWN' } } : {}) }} copy={copy} units={[]} busy={mode === 'busy'} onClose={onClose} onSubmit={pending} />;
  if (kind === 'recovery') return <ImeRecoveryDialog client={client} copy={copy} command={{ command_reference: '11111111-1111-4111-8111-111111111111', recovery_epoch: 1 }} onClose={onClose} />;
  return <ImeOwnerDialog client={client} copy={copy} query={query} histories={histories} onQueryChange={(_source, next) => { control.queries.push(next); setQuery(next); }} onClose={onClose} onCreated={() => {}} />;
}
control.mount = kind => {
  control.closed = 0; control.queries = []; control.submissions = 0; control.formSubmits = []; control.keyEvents = [];
  root.render(<Fixture key={kind} kind={kind} />);
};
control.mount(new URLSearchParams(location.search).get('kind') || 'high-risk');
