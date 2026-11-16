import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { Status } from '../../src/features/account-platform/AdminModelTokenPrimitives.jsx';
import { AdminReconciliationWorkspace } from '../../src/features/account-platform/AdminReconciliationWorkspace.jsx';
import { AdminModelOperationsWorkspace } from '../../src/features/account-platform/AdminModelOperationsWorkspace.jsx';
import { PlatformRoleBindings } from '../../src/features/account-platform/PlatformRoleBindings.jsx';
import { PlatformRoleBindingDetail } from '../../src/features/account-platform/PlatformRoleBindingDetail.jsx';
import { adminRolePresentationCopy, platformRoleCopy, tokenManagementCopy } from '../../src/shared/i18n.js';

const bindings = ['ACTIVE', 'FUTURE_REVIEW'].map((status, index) => ({
  binding_id: `00000000-0000-4000-8000-00000000000${index + 1}`,
  user_id: '00000000-0000-4000-8000-000000000003', role_code: 'PLATFORM_SUPPORT',
  status, starts_at: '2026-11-01T00:00:00Z', expires_at: null, grant_source: 'MANUAL',
}));
const bindingClient = new AdminPlatformClient();
const bindingActor = { user_id: '00000000-0000-4000-8000-000000000003', platform_authorization_version: 1, platform_permissions: [] };
const state = window.statusLabelsProbe = { viewed: [], reconciliationCalls: [], operationsCalls: [], delivery: 'NOT_STARTED', attempt: 'STARTED' };
const usageID = '11111111-1111-4111-8111-111111111111';
const observedAt = '2026-10-08T00:00:00.123456Z';
const collection = items => ({ items, next_cursor: null, has_more: false, limit: 50, as_of: observedAt });
// Read-only downstream client boundary. Enum values come from the published
// delivery/severity CHECKs and usage_queries.go, rather than invented statuses.
const operationsClient = {
  usageRequests: async () => collection([]),
  usageReconciliationCases: async query => {
    state.operationsCalls.push({ ...query });
    return collection(['P0', 'P1', 'P2'].filter(severity => !query.severity || query.severity === severity).map(severity => ({
      case_id: `00000000-0000-4000-8000-00000000001${severity.slice(1)}`, resource_id: usageID,
      case_type: 'USAGE_RECONCILIATION_REVIEW', severity, status: 'OPEN', resolution_due_at: observedAt,
    })));
  },
  usageRequest: async () => ({ usage_request_id: usageID, updated_at: observedAt, status: 'REVIEW',
    execution_status: 'REVIEW', settlement_status: 'REVIEW', delivery_status: state.delivery, attempt_status: state.attempt,
    model: 'CONTROLLED_MODEL', maximum_microcredits: '1', actual_microcredits: '1',
    last_persisted_sequence: '1', reconciliation_attempts: 1 }),
  usageEvidence: async () => collection(['CANCELLATION', 'CONVERGENCE'].map(source_kind => ({
    source_kind, source_id: usageID, observed_at: observedAt, evidence_type: 'CONTROLLED_EVIDENCE', evidence: {},
  }))),
};
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
  const [routeVersion, setRouteVersion] = useState(0);
  state.setValue = setValue;
  state.renderedValue = value;
  const words = adminRolePresentationCopy[language], copy = platformRoleCopy[language];
  state.operationsCopy = () => tokenManagementCopy[language].modelToken;
  if (new URLSearchParams(location.search).has('operations')) return <div id="operations-probe">
    <AdminModelOperationsWorkspace client={operationsClient} copy={state.operationsCopy()} language={language}
      permissions={['platform.usage.read']} routeVersion={routeVersion} onRouteChange={updates => {
        const url = new URL(location.href);
        for (const [key, value] of Object.entries(updates)) {
          if (value == null || value === '') url.searchParams.delete(key); else url.searchParams.set(key, value);
        }
        history.replaceState({}, '', url); setRouteVersion(value => value + 1);
      }} />
  </div>;
  if (new URLSearchParams(location.search).has('reconciliation')) return <>
    <select id="probe-language" value={language} onChange={event => setLanguage(event.target.value)}>{['zh', 'en', 'ja'].map(code => <option key={code} value={code}>{code}</option>)}</select>
    <AdminReconciliationWorkspace client={reconciliationClient} me={{ user_id: '00000000-0000-4000-8000-000000000003', platform_authorization_version: 1, platform_permissions: ['platform.billing.reconciliation.read'] }} language={language} />
  </>;
  return <>
    <select id="probe-language" value={language} onChange={event => setLanguage(event.target.value)}>{['zh', 'en', 'ja'].map(code => <option key={code} value={code}>{code}</option>)}</select>
    <div id="probe-status"><Status value={value} copy={tokenManagementCopy[language].modelToken} /></div>
    <PlatformRoleBindings client={bindingClient} me={bindingActor} acceptedSessionEpoch={0} routeScope="status-probe" {...{ words, copy, language, bindings }} target="" blocked={false} onApplyTarget={() => {}} canRevoke={() => false} onView={item => { state.viewed.push(item.status); setSelected(item); }} />
    {selected && <PlatformRoleBindingDetail client={bindingClient} me={bindingActor} acceptedSessionEpoch={0} routeScope="status-probe" {...{ words, copy, language }} binding={selected} onClose={() => setSelected(null)} />}
  </>;
}
createRoot(document.querySelector('#root')).render(<Probe />);
