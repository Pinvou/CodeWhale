import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { AdminHighRiskActionDialog } from '../../src/features/account-platform/AdminHighRiskActionDialog.jsx';
// These aliases exist only in this smoke's Vite transform. Product exports and
// handlers are unchanged; the test mounts the original private React components.
import { ImeProtocolDialog } from '../../src/features/account-platform/AdminProtocolFormulaWorkspace.jsx';
import { ImeOwnerDialog, ImeRecoveryDialog } from '../../src/features/account-platform/AdminTokenResourcesWorkspace.jsx';
import { tokenManagementCopy } from '../../src/shared/i18n.js';

const copy = tokenManagementCopy.en.modelToken;
const control = { closed: 0, queries: [], submissions: 0, formSubmits: [], keyEvents: [] };
window.adminIme = control;
// Observe native form default actions independently of execute/recovery calls.
document.addEventListener('submit', event => { control.formSubmits.push({ trusted: event.isTrusted, labelledBy: event.target.getAttribute('aria-labelledby') }); }, true);
const root = createRoot(document.getElementById('root'));
const histories = { current: {} };
const client = {
  tokenOwnerCandidates: async () => ({ items: [{ id: '22222222-2222-4222-8222-222222222222', type: 'USER', status: 'ACTIVE', display_label: 'Controlled owner' }] }),
  createTokenAccountRecoveryAction: () => pending(),
};
function pending() {
  control.submissions++;
  return new Promise((_resolve, reject) => { control.reject = () => reject(Object.assign(new Error('controlled lost acknowledgement'), { status: 503 })); });
}
function Fixture({ kind }) {
  const [open, setOpen] = useState(true);
  const [mode, setMode] = useState('ready');
  const [query, setQuery] = useState({ owner_type: 'USER', status: 'ACTIVE', q: '' });
  control.setMode = setMode;
  const onClose = () => { control.closed++; setOpen(false); };
  if (!open) return <p data-testid="closed">Closed</p>;
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
