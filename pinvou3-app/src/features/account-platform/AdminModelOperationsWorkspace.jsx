import React, { useEffect, useMemo, useRef, useState } from 'react';
import { adminModelTokenQueryUpdates, readAdminModelTokenQuery } from './admin-navigation.js';
import { FilterForm, Pagination, SourceFeedback, SourceHeader, Status, useAdminSource } from './AdminModelTokenPrimitives.jsx';
import { AdminHighRiskActionDialog } from './AdminHighRiskActionDialog.jsx';
import { governedTokenEnvelope, requireGovernedTokenCommandResult, requireGovernedTokenPrecheck } from './admin-high-risk-action-state.js';

function readDetailID(key) {
  return String(new URLSearchParams(globalThis.location?.search || '').get(key) || '').trim();
}

function useDetailSource({ enabled, load, refreshVersion }) {
  const [state, setState] = useState({ item: null, loading: false, error: null, updatedAt: null });
  const generation = useRef(0);
  useEffect(() => {
    if (!enabled) {
      setState({ item: null, loading: false, error: null, updatedAt: null });
      return undefined;
    }
    const controller = new globalThis.AbortController();
    const requestID = ++generation.current;
    setState({ item: null, loading: true, error: null, updatedAt: null });
    void load(controller.signal).then((item) => {
      if (!controller.signal.aborted && generation.current === requestID) {
        setState({ item, loading: false, error: null, updatedAt: new Date().toISOString() });
      }
    }).catch((error) => {
      if (!controller.signal.aborted && error?.name !== 'AbortError' && generation.current === requestID) {
        setState({ item: null, loading: false, error, updatedAt: null });
      }
    });
    return () => controller.abort();
  }, [enabled, load, refreshVersion]);
  return state;
}

function detailFeedback(state, copy, retry) {
  if (state.loading) return <div className="adm-source-state" role="status">{copy.loading}</div>;
  if (!state.error) return null;
  return <div className="adm-source-error" role="alert"><strong>{copy.detailUnavailable}</strong><span>{state.error.requestID ? `${copy.requestID}: ${state.error.requestID}` : copy.retryHelp}</span><button type="button" onClick={retry}>{copy.retry}</button></div>;
}

function valueOrDash(value) {
  return value === null || value === undefined || value === '' ? '—' : String(value);
}

function DateValue({ value }) {
  return <>{value ? new Date(value).toLocaleString() : '—'}</>;
}

function DetailFacts({ facts }) {
  return <dl className="adm-operation-facts">{facts.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>;
}

function sourceRetry({ state, source, query, histories, onQueryChange, setRefreshVersion }) {
  if (state.error?.status === 410) {
    delete histories.current[source];
    onQueryChange(source, { ...query, cursor: '' }, 'replace');
  } else setRefreshVersion((value) => value + 1);
}

function ProvisioningList({ load, copy, routeVersion, onQueryChange, onOpen, histories, canOpen }) {
  const query = useMemo(() => readAdminModelTokenQuery('provisionings'), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion });
  const retry = () => sourceRetry({ state, source: 'provisionings', query, histories, onQueryChange, setRefreshVersion });
  return <section className="adm-card adm-source-card" aria-labelledby="provisionings-title">
    <SourceHeader title={copy.provisionings} titleID="provisionings-title" state={state} copy={copy} onRetry={retry} />
    <FilterForm source="provisionings" query={query} copy={copy} onQueryChange={onQueryChange} fields={[
      { key: 'q', label: copy.provisioningSearch, placeholder: copy.identifierPlaceholder },
      { key: 'status', label: copy.status, options: ['PENDING', 'READY', 'FAILED', 'REPAIR_REQUIRED'] },
      { key: 'model_id', label: copy.modelID },
      { key: 'token_account_id', label: copy.tokenAccountID },
      { key: 'scope_type', label: copy.scope, options: ['PERSONAL', 'ORGANIZATION'] },
      { key: 'scope_id', label: copy.scopeID },
      { key: 'sla_overdue', label: copy.slaOverdue, options: ['true', 'false'] },
    ]} />
    <SourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.provisioning}</th><th>{copy.modelID}</th><th>{copy.status}</th><th>{copy.stage}</th><th>{copy.generation}</th><th>{copy.updatedAt}</th>{canOpen && <th>{copy.actions}</th>}</tr></thead><tbody>{state.items.map((item) => <tr key={item.provisioning_id}><td data-label={copy.provisioning}><span className="adm-mono">{item.provisioning_id}</span></td><td data-label={copy.modelID}><span className="adm-mono">{item.model_id}</span></td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.stage}>{copy.enumLabels?.[item.stage] || item.stage}</td><td data-label={copy.generation}>{item.actual_generation || 0} / {item.desired_generation}</td><td data-label={copy.updatedAt}><DateValue value={item.updated_at} /></td>{canOpen && <td data-label={copy.actions}><button type="button" onClick={() => onOpen(item.provisioning_id)}>{copy.viewDetail}</button></td>}</tr>)}</tbody></table></div>}
    <Pagination source="provisionings" query={query} state={state} copy={copy} cursorHistory={histories} onQueryChange={onQueryChange} />
  </section>;
}

function UsageList({ load, copy, routeVersion, onQueryChange, onOpen, histories, canOpen }) {
  const query = useMemo(() => readAdminModelTokenQuery('usage'), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion });
  const retry = () => sourceRetry({ state, source: 'usage', query, histories, onQueryChange, setRefreshVersion });
  return <section className="adm-card adm-source-card" aria-labelledby="usage-requests-title">
    <SourceHeader title={copy.usageRequests} titleID="usage-requests-title" state={state} copy={copy} onRetry={retry} />
    <FilterForm source="usage" query={query} copy={copy} onQueryChange={onQueryChange} fields={[
      { key: 'q', label: copy.usageSearch, placeholder: copy.identifierPlaceholder },
      { key: 'execution_status', label: copy.executionStatus, options: ['RECEIVED', 'HOLD_RESERVING', 'HOLD_RESERVED', 'DISPATCH_INTENT_PERSISTED', 'UPSTREAM_DISPATCHING', 'STREAMING', 'SETTLEMENT_PENDING', 'SUCCEEDED', 'FAILED', 'CANCELLED_BEFORE_DISPATCH', 'REVIEW'] },
      { key: 'settlement_status', label: copy.settlementStatus, options: ['HOLD_PENDING', 'HOLD_ACTIVE', 'USAGE_PENDING', 'SETTLED', 'RELEASED', 'REVIEW'] },
      { key: 'model', label: copy.model },
      { key: 'token_account_id', label: copy.tokenAccountID },
      { key: 'organization_id', label: copy.organizationID },
      { key: 'started_from', label: copy.startedFrom, type: 'datetime-local' },
      { key: 'started_to', label: copy.startedTo, type: 'datetime-local' },
    ]} />
    <SourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.request}</th><th>{copy.model}</th><th>{copy.executionStatus}</th><th>{copy.settlementStatus}</th><th>{copy.startedAt}</th>{canOpen && <th>{copy.actions}</th>}</tr></thead><tbody>{state.items.map((item) => <tr key={item.usage_request_id}><td data-label={copy.request}><span className="adm-mono">{item.usage_request_id}</span></td><td data-label={copy.model}>{item.model}</td><td data-label={copy.executionStatus}><Status value={item.execution_status} copy={copy} /></td><td data-label={copy.settlementStatus}><Status value={item.settlement_status} copy={copy} /></td><td data-label={copy.startedAt}><DateValue value={item.started_at} /></td>{canOpen && <td data-label={copy.actions}><button type="button" onClick={() => onOpen(item.usage_request_id)}>{copy.viewDetail}</button></td>}</tr>)}</tbody></table></div>}
    <Pagination source="usage" query={query} state={state} copy={copy} cursorHistory={histories} onQueryChange={onQueryChange} />
  </section>;
}

function ReconciliationList({ load, copy, routeVersion, onQueryChange, onOpen, histories, canOpen }) {
  const query = useMemo(() => readAdminModelTokenQuery('reconciliation'), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion });
  const retry = () => sourceRetry({ state, source: 'reconciliation', query, histories, onQueryChange, setRefreshVersion });
  return <section className="adm-card adm-source-card" aria-labelledby="reconciliation-title">
    <SourceHeader title={copy.reconciliationQueue} titleID="reconciliation-title" state={state} copy={copy} onRetry={retry} />
    <p className="adm-source-lead">{copy.reconciliationQueueLead}</p>
    <FilterForm source="reconciliation" query={query} copy={copy} onQueryChange={onQueryChange} fields={[
      { key: 'q', label: copy.caseSearch, placeholder: copy.identifierPlaceholder },
      { key: 'status', label: copy.status, options: ['OPEN', 'RESOLVED', 'LONG_TERM_PLAN'] },
      { key: 'severity', label: copy.severity, options: ['P0', 'P1', 'P2'] },
      { key: 'case_type', label: copy.caseType, options: ['USAGE_RECONCILIATION_REVIEW'] },
      { key: 'owner_id', label: copy.ownerID },
      { key: 'sla_overdue', label: copy.slaOverdue, options: ['true', 'false'] },
    ]} />
    <SourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.caseID}</th><th>{copy.caseType}</th><th>{copy.severity}</th><th>{copy.status}</th><th>{copy.resolutionDue}</th>{canOpen && <th>{copy.actions}</th>}</tr></thead><tbody>{state.items.map((item) => <tr key={item.case_id}><td data-label={copy.caseID}><span className="adm-mono">{item.case_id}</span></td><td data-label={copy.caseType}>{copy.enumLabels?.[item.case_type] || item.case_type}</td><td data-label={copy.severity}><Status value={item.severity} copy={copy} /></td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.resolutionDue}><DateValue value={item.resolution_due_at} /></td>{canOpen && <td data-label={copy.actions}><button type="button" onClick={() => onOpen(item.resource_id)}>{copy.reviewEvidence}</button></td>}</tr>)}</tbody></table></div>}
    <Pagination source="reconciliation" query={query} state={state} copy={copy} cursorHistory={histories} onQueryChange={onQueryChange} />
  </section>;
}

function Attempts({ client, provisioningID, copy, routeVersion, onQueryChange, histories }) {
  const query = useMemo(() => readAdminModelTokenQuery('provisioningAttempts'), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const load = useMemo(() => (filters, signal) => client.provisioningAttempts(provisioningID, filters, signal), [client, provisioningID]);
  const state = useAdminSource({ enabled: Boolean(provisioningID), load, query, refreshVersion });
  const retry = () => sourceRetry({ state, source: 'provisioningAttempts', query, histories, onQueryChange, setRefreshVersion });
  return <section className="adm-operation-subsection" aria-labelledby="attempts-title">
    <SourceHeader title={copy.attemptHistory} titleID="attempts-title" state={state} copy={copy} onRetry={retry} />
    <FilterForm source="provisioningAttempts" query={query} copy={copy} onQueryChange={onQueryChange} fields={[{ key: 'status', label: copy.status, options: ['RUNNING', 'SUCCEEDED', 'FAILED', 'TIMED_OUT'] }]} />
    <SourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.roundAttempt}</th><th>{copy.status}</th><th>{copy.stage}</th><th>{copy.startedAt}</th><th>{copy.endedAt}</th><th>{copy.diagnostics}</th></tr></thead><tbody>{state.items.map((item) => <tr key={item.attempt_id}><td>{item.repair_round} / {item.attempt_number}</td><td><Status value={item.status} copy={copy} /></td><td>{item.stage}</td><td><DateValue value={item.started_at} /></td><td><DateValue value={item.ended_at} /></td><td>{valueOrDash(item.error_code)}</td></tr>)}</tbody></table></div>}
    <Pagination source="provisioningAttempts" query={query} state={state} copy={copy} cursorHistory={histories} onQueryChange={onQueryChange} />
  </section>;
}

function Evidence({ client, usageID, copy, routeVersion, onQueryChange, histories }) {
  const query = useMemo(() => readAdminModelTokenQuery('usageEvidence'), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const load = useMemo(() => (filters, signal) => client.usageEvidence(usageID, filters, signal), [client, usageID]);
  const state = useAdminSource({ enabled: Boolean(usageID), load, query, refreshVersion });
  const retry = () => sourceRetry({ state, source: 'usageEvidence', query, histories, onQueryChange, setRefreshVersion });
  return <section className="adm-operation-subsection" aria-labelledby="evidence-title">
    <SourceHeader title={copy.evidenceHistory} titleID="evidence-title" state={state} copy={copy} onRetry={retry} />
    <FilterForm source="usageEvidence" query={query} copy={copy} onQueryChange={onQueryChange} fields={[{ key: 'evidence_type', label: copy.evidenceType }]} />
    <SourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-evidence-list">{state.items.map((item) => <article key={`${item.source_kind}:${item.source_id}`}><div><strong>{item.evidence_type}</strong><Status value={item.source_kind} copy={copy} /></div><time>{new Date(item.observed_at).toLocaleString()}</time><pre>{JSON.stringify(item.evidence || {}, null, 2)}</pre></article>)}</div>}
    <Pagination source="usageEvidence" query={query} state={state} copy={copy} cursorHistory={histories} onQueryChange={onQueryChange} />
  </section>;
}

function ProvisioningDetail({ id, client, copy, routeVersion, onBack, onQueryChange, histories, canRepair }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [repairOpen, setRepairOpen] = useState(false);
  const load = useMemo(() => (signal) => client.provisioning(id, signal), [client, id]);
  const state = useDetailSource({ enabled: Boolean(id), load, refreshVersion });
  const item = state.item;
	const repairCommand = item ? { provisioning_id: id, expected_provisioning_version: item.updated_at, target_generation: item.desired_generation, replay_model_request: false } : null;
	useEffect(() => {
		if (!canRepair) setRepairOpen(false);
	}, [canRepair]);
  return <section className="adm-card adm-operation-detail" aria-labelledby="provisioning-detail-title">
    <header><button type="button" className="secondary" onClick={onBack}>← {copy.backToList}</button><div><h2 id="provisioning-detail-title">{copy.provisioningDetail}</h2><p>{copy.provisioningDetailLead}</p></div><div className="adm-row-actions">{canRepair && ['FAILED', 'REPAIR_REQUIRED'].includes(item?.status) && <button type="button" onClick={() => setRepairOpen(true)}>{copy.repairProvisioning}</button>}<button type="button" onClick={() => setRefreshVersion((value) => value + 1)}>{copy.refreshSource}</button></div></header>
    {detailFeedback(state, copy, () => setRefreshVersion((value) => value + 1))}
    {item && <>
      <DetailFacts facts={[
        [copy.provisioning, <span className="adm-mono">{item.provisioning_id}</span>],
        [copy.status, <Status value={item.status} copy={copy} />],
        [copy.modelID, <span className="adm-mono">{item.model_id}</span>],
        [copy.protocol, `${item.protocol_code} · ${valueOrDash(item.protocol_version_id)}`],
        [copy.scope, `${item.scope_type} · ${item.scope_id}`],
        [copy.generation, `${item.actual_generation || 0} / ${item.desired_generation}`],
        [copy.stage, item.stage],
        [copy.slaDeadline, <DateValue value={item.sla_deadline_at} />],
        [copy.lastError, valueOrDash(item.last_error_code)],
        [copy.updatedAt, <DateValue value={item.updated_at} />],
      ]} />
      <Attempts client={client} provisioningID={id} copy={copy} routeVersion={routeVersion} onQueryChange={onQueryChange} histories={histories} />
    </>}
		{repairOpen && item && repairCommand && canRepair && <AdminHighRiskActionDialog action={{ key: `provisioning-repair-${id}`, impactAction: 'TOKEN_ROUTE_PROVISIONING_REPAIR', title: copy.repairProvisioning, lead: copy.repairProvisioningHelp }} copy={copy} target={{ label: id, version: `${copy.generation}: ${item.desired_generation}` }} policy="AUDIT_ONLY" recentAuth reasonMin={1} reasonMax={500} impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_ROUTE_PROVISIONING_REPAIR', target_resource_type: 'PROVISIONING', target_resource_id: id, command: repairCommand }, signal), { commandType: 'TOKEN_ROUTE_PROVISIONING_REPAIR', targetResourceType: 'PROVISIONING', targetResourceID: id })} onClose={() => setRepairOpen(false)} execute={async (payload, key, signal) => requireGovernedTokenCommandResult(await client.createGovernedTokenCommand(await governedTokenEnvelope({ commandType: 'TOKEN_ROUTE_PROVISIONING_REPAIR', targetResourceType: 'PROVISIONING', targetResourceID: id, reason: payload.reason, impact: payload.precheck, command: repairCommand }), key, signal), { commandType: 'TOKEN_ROUTE_PROVISIONING_REPAIR', targetResourceType: 'PROVISIONING', targetResourceID: id })} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_ROUTE_PROVISIONING_REPAIR', targetResourceType: 'PROVISIONING', targetResourceID: id })} onCompleted={() => setRefreshVersion((value) => value + 1)} />}
  </section>;
}

function UsageDetail({ id, client, copy, permissions, routeVersion, onBack, onQueryChange, histories, onNavigate, canReconcile }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [reconcileOpen, setReconcileOpen] = useState(false);
  const load = useMemo(() => (signal) => client.usageRequest(id, signal), [client, id]);
  const state = useDetailSource({ enabled: Boolean(id), load, refreshVersion });
  const item = state.item;
	const reconcileCommand = item ? { usage_request_id: id, expected_usage_version: item.updated_at, evidence_high_watermark: item.last_persisted_sequence, replay_model_request: false } : null;
	useEffect(() => {
		if (!canReconcile) setReconcileOpen(false);
	}, [canReconcile]);
  return <section className="adm-card adm-operation-detail" aria-labelledby="usage-detail-title">
    <header><button type="button" className="secondary" onClick={onBack}>← {copy.backToList}</button><div><h2 id="usage-detail-title">{copy.usageDetail}</h2><p>{copy.usageDetailLead}</p></div><div className="adm-row-actions">{canReconcile && ['SETTLEMENT_PENDING', 'REVIEW'].includes(item?.status) && <button type="button" onClick={() => setReconcileOpen(true)}>{copy.retryReconciliation}</button>}<button type="button" onClick={() => setRefreshVersion((value) => value + 1)}>{copy.refreshSource}</button></div></header>
    {detailFeedback(state, copy, () => setRefreshVersion((value) => value + 1))}
    {item && <>
      <DetailFacts facts={[
        [copy.request, <span className="adm-mono">{item.usage_request_id}</span>],
        [copy.model, item.model],
        [copy.modelID, valueOrDash(item.model_id)],
        [copy.catalogVersion, valueOrDash(item.catalog_version)],
        [copy.capabilityVersion, valueOrDash(item.model_capability_version_id)],
        [copy.priceVersion, valueOrDash(item.price_version_id)],
        [copy.routeGeneration, valueOrDash(item.route_generation)],
        [copy.executionStatus, <Status value={item.execution_status} copy={copy} />],
        [copy.settlementStatus, <Status value={item.settlement_status} copy={copy} />],
        [copy.deliveryStatus, <Status value={item.delivery_status} copy={copy} />],
        [copy.holdReference, valueOrDash(item.hold_id)],
        [copy.tokenAccountID, valueOrDash(item.token_account_id)],
        [copy.protocol, valueOrDash(item.protocol_definition_id)],
        [copy.adapterVersion, valueOrDash(item.adapter_version)],
        [copy.attemptStatus, valueOrDash(item.attempt_status)],
        [copy.usageAmount, `${item.actual_microcredits ?? '—'} / ${item.maximum_microcredits}`],
        [copy.tokenUsage, `${item.input_tokens || 0} + ${item.cached_input_tokens || 0} / ${item.output_tokens || 0}`],
        [copy.reconciliationAttempts, item.reconciliation_attempts],
        [copy.reconciliationError, valueOrDash(item.reconciliation_last_error)],
        [copy.updatedAt, <DateValue value={item.updated_at} />],
      ]} />
      {permissions.includes('platform.audit.read') && <div className="adm-row-actions adm-operation-audit"><button type="button" onClick={() => onNavigate?.('audit', { audit_resource: id })}>{copy.viewAudit}</button></div>}
      <Evidence client={client} usageID={id} copy={copy} routeVersion={routeVersion} onQueryChange={onQueryChange} histories={histories} />
    </>}
		{reconcileOpen && item && reconcileCommand && canReconcile && <AdminHighRiskActionDialog action={{ key: `usage-reconcile-${id}`, impactAction: 'TOKEN_USAGE_RECONCILIATION_RETRY', title: copy.retryReconciliation, lead: copy.retryReconciliationHelp }} copy={copy} target={{ label: id, version: `${copy.updatedAt}: ${new Date(item.updated_at).toLocaleString()}` }} policy="AUDIT_ONLY" reasonMin={1} reasonMax={500} impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_USAGE_RECONCILIATION_RETRY', target_resource_type: 'USAGE_REQUEST', target_resource_id: id, command: reconcileCommand }, signal), { commandType: 'TOKEN_USAGE_RECONCILIATION_RETRY', targetResourceType: 'USAGE_REQUEST', targetResourceID: id })} onClose={() => setReconcileOpen(false)} execute={async (payload, key, signal) => requireGovernedTokenCommandResult(await client.createGovernedTokenCommand(await governedTokenEnvelope({ commandType: 'TOKEN_USAGE_RECONCILIATION_RETRY', targetResourceType: 'USAGE_REQUEST', targetResourceID: id, reason: payload.reason, impact: payload.precheck, command: reconcileCommand }), key, signal), { commandType: 'TOKEN_USAGE_RECONCILIATION_RETRY', targetResourceType: 'USAGE_REQUEST', targetResourceID: id })} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_USAGE_RECONCILIATION_RETRY', targetResourceType: 'USAGE_REQUEST', targetResourceID: id })} onCompleted={() => setRefreshVersion((value) => value + 1)} />}
  </section>;
}

export function AdminModelOperationsWorkspace({ client, copy, permissions, detailsEnabled, actionsEnabled, routeVersion, operations, requests, onRouteChange, onQueryChange, onNavigate, histories }) {
  const provisioningID = detailsEnabled && operations.tab === 'provisionings' ? readDetailID('provisioning_id') : '';
  const usageID = detailsEnabled && ['usage', 'reconciliation'].includes(operations.tab) ? readDetailID('usage_request_id') : '';
  const provisioningRequest = requests.find((request) => request.source === 'provisionings');
  const usageRequest = requests.find((request) => request.source === 'usage');
  const reconciliationRequest = requests.find((request) => request.source === 'reconciliation');
  const clearProvisioning = () => onRouteChange({ provisioning_id: null, ...adminModelTokenQueryUpdates('provisioningAttempts', {}) }, 'push');
  const clearUsage = () => onRouteChange({ usage_request_id: null, ...adminModelTokenQueryUpdates('usageEvidence', {}) }, 'push');
  if (provisioningID) return <ProvisioningDetail id={provisioningID} client={client} copy={copy} routeVersion={routeVersion} onBack={clearProvisioning} onQueryChange={onQueryChange} histories={histories} canRepair={actionsEnabled && permissions.includes('platform.tokens.manage')} />;
  if (usageID) return <UsageDetail id={usageID} client={client} copy={copy} permissions={permissions} routeVersion={routeVersion} onBack={clearUsage} onQueryChange={onQueryChange} onNavigate={onNavigate} histories={histories} canReconcile={actionsEnabled && permissions.includes('platform.usage.reconcile.retry')} />;
  return <>
    {provisioningRequest && <div role="tabpanel" hidden={operations.tab !== 'provisionings'} id="model-operations-panel-provisionings" aria-labelledby="model-operations-tab-provisionings"><ProvisioningList load={provisioningRequest.load} copy={copy} routeVersion={routeVersion} onQueryChange={onQueryChange} histories={histories} canOpen={detailsEnabled} onOpen={(id) => onRouteChange({ provisioning_id: id }, 'push')} /></div>}
    {usageRequest && <div role="tabpanel" hidden={operations.tab !== 'usage'} id="model-operations-panel-usage" aria-labelledby="model-operations-tab-usage"><UsageList load={usageRequest.load} copy={copy} routeVersion={routeVersion} onQueryChange={onQueryChange} histories={histories} canOpen={detailsEnabled} onOpen={(id) => onRouteChange({ usage_request_id: id }, 'push')} /></div>}
    {reconciliationRequest && <div role="tabpanel" hidden={operations.tab !== 'reconciliation'} id="model-operations-panel-reconciliation" aria-labelledby="model-operations-tab-reconciliation"><ReconciliationList load={reconciliationRequest.load} copy={copy} routeVersion={routeVersion} onQueryChange={onQueryChange} histories={histories} canOpen={detailsEnabled} onOpen={(id) => onRouteChange({ usage_request_id: id }, 'push')} /></div>}
  </>;
}
