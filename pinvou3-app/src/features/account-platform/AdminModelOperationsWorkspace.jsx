import React, { useEffect, useMemo, useRef, useState } from 'react';
import { adminModelTokenQueryUpdates, readAdminModelTokenQuery } from './admin-navigation.js';
import { FilterForm, Pagination, SourceFeedback, SourceHeader, Status, useAdminSource } from './AdminModelTokenPrimitives.jsx';
import { AdminHighRiskActionDialog } from './AdminHighRiskActionDialog.jsx';
import { governedTokenEnvelope } from './admin-high-risk-action-state.js';
import { requireUsageDetail, requireUsageReadiness, requireUsagePrecheck, requireUsageCommandResult } from './admin-usage-governance.js';

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

function UsageDetail({ id, client, copy, routeVersion, onBack, onQueryChange, histories, canReconcile }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [reconcileOpen, setReconcileOpen] = useState(false);
  const load = useMemo(() => (signal) => client.usageRequest(id, signal).then(item => requireUsageDetail(item, id)), [client, id]);
  const loadReadiness = useMemo(() => (signal) => client.usageReconciliationCommandReadiness(signal).then(requireUsageReadiness), [client]);
  const state = useDetailSource({ enabled: Boolean(id), load, refreshVersion });
  const readiness = useDetailSource({ enabled: Boolean(id) && canReconcile, load: loadReadiness, refreshVersion });
  const item = state.item;
	const reconciliationReady = canReconcile && readiness.item?.commands_enabled === true;
	const reconcileCommand = item ? { usage_request_id: id, expected_usage_version: item.updated_at, evidence_high_watermark: item.last_persisted_sequence, replay_model_request: false } : null;
	useEffect(() => {
		if (!reconciliationReady) setReconcileOpen(false);
	}, [reconciliationReady]);
  return <section className="adm-card adm-operation-detail" aria-labelledby="usage-detail-title">
    <header><button type="button" className="secondary" onClick={onBack}>← {copy.backToList}</button><div><h2 id="usage-detail-title">{copy.usageDetail}</h2><p>{copy.usageDetailLead}</p></div><div className="adm-row-actions">{reconciliationReady && ['SETTLEMENT_PENDING', 'REVIEW'].includes(item?.status) && <button type="button" onClick={() => setReconcileOpen(true)}>{copy.retryReconciliation}</button>}<button type="button" onClick={() => setRefreshVersion((value) => value + 1)}>{copy.refreshSource}</button></div></header>
    {detailFeedback(state, copy, () => setRefreshVersion((value) => value + 1))}
		{canReconcile && !readiness.loading && !reconciliationReady && (readiness.error || readiness.item) && <p className="adm-source-lead" role="status">{readiness.error ? copy.commandReadinessUnavailable : copy.commandReadinessReasons?.[readiness.item?.unavailable_reason] || copy.commandReadinessUnavailable}</p>}
    {item && <>
      <DetailFacts facts={[
        [copy.request, <span key="request" className="adm-mono">{item.usage_request_id}</span>],
        [copy.model, item.model],
        [copy.modelID, valueOrDash(item.model_id)],
        [copy.catalogVersion, valueOrDash(item.catalog_version)],
        [copy.capabilityVersion, valueOrDash(item.model_capability_version_id)],
        [copy.priceVersion, valueOrDash(item.price_version_id)],
        [copy.routeGeneration, valueOrDash(item.route_generation)],
        [copy.executionStatus, <Status key="executionStatus" value={item.execution_status} copy={copy} />],
        [copy.settlementStatus, <Status key="settlementStatus" value={item.settlement_status} copy={copy} />],
        [copy.deliveryStatus, <Status key="deliveryStatus" value={item.delivery_status} copy={copy} />],
        [copy.holdReference, valueOrDash(item.hold_id)],
        [copy.tokenAccountID, valueOrDash(item.token_account_id)],
        [copy.protocol, valueOrDash(item.protocol_definition_id)],
        [copy.adapterVersion, valueOrDash(item.adapter_version)],
        [copy.attemptStatus, valueOrDash(item.attempt_status)],
        [copy.usageAmount, `${item.actual_microcredits ?? '—'} / ${item.maximum_microcredits}`],
        [copy.tokenUsage, `${valueOrDash(item.input_tokens)} + ${valueOrDash(item.cached_input_tokens)} / ${valueOrDash(item.output_tokens)}`],
        [copy.reconciliationAttempts, item.reconciliation_attempts],
        [copy.reconciliationError, valueOrDash(item.reconciliation_last_error)],
        [copy.updatedAt, <DateValue key="updatedAt" value={item.updated_at} />],
      ]} />
      <Evidence client={client} usageID={id} copy={copy} routeVersion={routeVersion} onQueryChange={onQueryChange} histories={histories} />
    </>}
		{reconcileOpen && item && reconcileCommand && reconciliationReady && <AdminHighRiskActionDialog action={{ key: `usage-reconcile-${id}`, impactAction: 'TOKEN_USAGE_RECONCILIATION_RETRY', title: copy.retryReconciliation, lead: copy.retryReconciliationHelp }} copy={copy} target={{ label: id, version: `${copy.updatedAt}: ${new Date(item.updated_at).toLocaleString()}` }} policy="AUDIT_ONLY" reasonMin={10} reasonMax={2000} impactLoader={async (signal) => requireUsagePrecheck(await client.usageReconciliationPrecheck({ command_type: 'TOKEN_USAGE_RECONCILIATION_RETRY', target_resource_type: 'USAGE_REQUEST', target_resource_id: id, command: reconcileCommand }, signal), { commandType: 'TOKEN_USAGE_RECONCILIATION_RETRY', targetResourceType: 'USAGE_REQUEST', targetResourceID: id, command: reconcileCommand })} onClose={() => setReconcileOpen(false)} execute={async (payload, key, signal) => requireUsageCommandResult(await client.createUsageReconciliationCommand(await governedTokenEnvelope({ commandType: 'TOKEN_USAGE_RECONCILIATION_RETRY', targetResourceType: 'USAGE_REQUEST', targetResourceID: id, reason: payload.reason, impact: payload.precheck, command: reconcileCommand }), key, signal), id, payload.precheck.expected_versions.evidence_high_watermark)} recover={async (reference, signal, frozen) => requireUsageCommandResult(await client.usageReconciliationResult(reference, signal), id, frozen.precheck.expected_versions.evidence_high_watermark)} recoverByIdempotencyKey={async (key, signal, frozen) => requireUsageCommandResult(await client.usageReconciliationByIdempotencyKey(key, signal), id, frozen.precheck.expected_versions.evidence_high_watermark)} onCompleted={() => setRefreshVersion((value) => value + 1)} />}
  </section>;
}

export function AdminModelOperationsWorkspace({ client, copy, permissions, routeVersion, onRouteChange }) {
  const histories = useRef({});
  const canRead = permissions.includes('platform.usage.read');
  const canReconcile = permissions.includes('platform.usage.reconcile.retry');
  const tab = new URLSearchParams(globalThis.location?.search || '').get('operations_tab') === 'reconciliation' ? 'reconciliation' : 'usage';
  const id = readDetailID('usage_request_id');
  const usageLoad = useMemo(() => (filters, signal) => client.usageRequests(filters, signal), [client]);
  const reconciliationLoad = useMemo(() => (filters, signal) => client.usageReconciliationCases(filters, signal), [client]);
  const onQueryChange = (source, query, mode) => onRouteChange(adminModelTokenQueryUpdates(source, query), mode);
  const open = (usageID) => onRouteChange({ usage_request_id: usageID, ...adminModelTokenQueryUpdates('usageEvidence', {}) });
  const back = () => onRouteChange({ usage_request_id: null, ...adminModelTokenQueryUpdates('usageEvidence', {}) });
  if (!canRead) return null;
  if (id) return <UsageDetail key={`${client.financeSessionEpoch || 0}:${id}`} id={id} client={client} copy={copy} routeVersion={routeVersion} onBack={back} onQueryChange={onQueryChange} histories={histories} canReconcile={canReconcile} />;
  return <>
    <nav aria-label={copy.operationsViews}>{['usage', 'reconciliation'].map(value => <button key={value} type="button" aria-pressed={tab === value} onClick={() => onRouteChange({ operations_tab: value })}>{value === 'usage' ? copy.usageRequests : copy.reconciliationQueue}</button>)}</nav>
    {tab === 'usage' ? <UsageList load={usageLoad} copy={copy} routeVersion={routeVersion} onQueryChange={onQueryChange} onOpen={open} histories={histories} canOpen />
      : <ReconciliationList load={reconciliationLoad} copy={copy} routeVersion={routeVersion} onQueryChange={onQueryChange} onOpen={open} histories={histories} canOpen />}
  </>;
}
