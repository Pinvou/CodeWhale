import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { Status } from '../../src/features/account-platform/AdminModelTokenPrimitives.jsx';
import { AdminReconciliationWorkspace } from '../../src/features/account-platform/AdminReconciliationWorkspace.jsx';
import { PlatformRoleBindings } from '../../src/features/account-platform/PlatformRoleBindings.jsx';
import { PlatformRoleBindingDetail } from '../../src/features/account-platform/PlatformRoleBindingDetail.jsx';
import { adminRolePresentationCopy, platformRoleCopy, tokenManagementCopy } from '../../src/shared/i18n.js';

const bindings = ['ACTIVE', 'FUTURE_REVIEW'].map((status, index) => ({
  binding_id: `00000000-0000-4000-8000-00000000000${index + 1}`,
  user_id: '00000000-0000-4000-8000-000000000003', role_code: 'PLATFORM_SUPPORT',
  status, starts_at: '2026-11-01T00:00:00Z', expires_at: null, grant_source: 'MANUAL',
}));
const state = window.statusLabelsProbe = { viewed: [], reconciliationCalls: [] };
const reconciliationClient = {
  reconciliationRuns: async query => { state.reconciliationCalls.push({ kind: 'runs', ...query }); return { items: [], next_cursor: null }; },
  reconciliationCases: async query => {
    state.reconciliationCalls.push({ kind: 'cases', ...query });
    return { items: [{ case_id: '00000000-0000-4000-8000-000000000004', merchant_order_no: 'controlled-order', case_type: 'REFUND_DATE_UNVERIFIED', status: 'IN_REVIEW' }], next_cursor: null };
  },
  reconciliationCaseCommand: async () => { throw new Error('read-only fixture must not write'); },
};
function Probe() {
  const [language, setLanguage] = useState(new URLSearchParams(location.search).get('language') || 'en');
  const [value, setValue] = useState('constructor');
  const [selected, setSelected] = useState(null);
  state.setValue = setValue;
  state.renderedValue = value;
  const words = adminRolePresentationCopy[language], copy = platformRoleCopy[language];
  if (new URLSearchParams(location.search).has('reconciliation')) return <>
    <select id="probe-language" value={language} onChange={event => setLanguage(event.target.value)}>{['zh', 'en', 'ja'].map(code => <option key={code} value={code}>{code}</option>)}</select>
    <AdminReconciliationWorkspace client={reconciliationClient} me={{ user_id: '00000000-0000-4000-8000-000000000003', platform_authorization_version: 1, platform_permissions: ['platform.billing.reconciliation.read'] }} language={language} />
  </>;
  return <>
    <select id="probe-language" value={language} onChange={event => setLanguage(event.target.value)}>{['zh', 'en', 'ja'].map(code => <option key={code} value={code}>{code}</option>)}</select>
    <div id="probe-status"><Status value={value} copy={tokenManagementCopy[language].modelToken} /></div>
    <PlatformRoleBindings {...{ words, copy, language, bindings }} target="" blocked={false} onApplyTarget={() => {}} canRevoke={() => false} onView={item => { state.viewed.push(item.status); setSelected(item); }} />
    {selected && <PlatformRoleBindingDetail {...{ words, copy, language }} binding={selected} onClose={() => setSelected(null)} />}
  </>;
}
createRoot(document.querySelector('#root')).render(<Probe />);
