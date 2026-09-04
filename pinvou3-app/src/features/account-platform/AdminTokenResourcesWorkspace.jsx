import React, { useEffect, useMemo, useRef, useState } from 'react';
import { previousAdminModelTokenCursor, readAdminModelTokenQuery, rememberAdminModelTokenNextCursor, synchronizeAdminModelTokenCursorHistory } from './admin-navigation.js';
import { executeAdminSourceRequest, initialAdminSourceState, reduceAdminSourceState, visibleAdminSourceState } from './admin-model-token-state.js';
import { ownerCandidateQuery, routeEditorVisible, serverRouteHealth, stableRouteChange, stableRouteChoices, tokenAccountCreateInput, tokenResourceAccess } from './admin-token-resource-state.js';
import { AdminHighRiskActionDialog } from './AdminHighRiskActionDialog.jsx';
import { governedTokenEnvelope, requireGovernedTokenCommandResult, validStableRouteWriteResult } from './admin-high-risk-action-state.js';

function useSource(load, query, enabled, refreshVersion = 0) {
  const [state, setState] = useState(initialAdminSourceState);
  const generation = useRef(0);
  const queryKey = JSON.stringify(query);
  useEffect(() => {
    if (!enabled) return undefined;
    const controller = new globalThis.AbortController();
    void executeAdminSourceRequest({ requestID: ++generation.current, queryKey, query, load, signal: controller.signal, transition: (event) => setState((previous) => reduceAdminSourceState(previous, event)) });
    return () => controller.abort();
  }, [enabled, load, queryKey, refreshVersion]);
  return visibleAdminSourceState(state, queryKey, enabled);
}

function focusWithin(event, root) {
  if (event.key !== 'Tab') return;
  const controls = [...(root?.querySelectorAll('button:not(:disabled),input:not(:disabled),select:not(:disabled),textarea:not(:disabled),[tabindex]:not([tabindex="-1"])') || [])];
  if (!controls.length) return;
  if (event.shiftKey && document.activeElement === controls[0]) { event.preventDefault(); controls.at(-1).focus(); }
  else if (!event.shiftKey && document.activeElement === controls.at(-1)) { event.preventDefault(); controls[0].focus(); }
}

function Status({ value, copy }) {
  const tone = ['ACTIVE', 'HEALTHY', 'PUBLISHED'].includes(value) ? 'ok' : ['PROBING', 'DEGRADED', 'STALE', 'PROVISIONING'].includes(value) ? 'warn' : '';
  return <span className={`adm-status ${tone}`}>{copy.enumLabels[value] || value}</span>;
}

function Feedback({ state, copy, onRetry }) {
  if (state.error) return <div className="adm-source-error" role="alert"><strong>{state.error.status === 410 ? copy.cursorExpired : copy.sourceUnavailable}</strong>{state.error.requestID && <span>{copy.requestID}: {state.error.requestID}</span>}<button type="button" onClick={onRetry}>{copy.retry}</button></div>;
  if (state.loading && !state.items.length) return <div className="adm-source-state" role="status">{copy.loading}</div>;
  if (!state.loading && !state.items.length) return <div className="adm-source-state">{copy.noResults}</div>;
  return null;
}

function Paging({ source, query, state, copy, histories, onChange }) {
  if (!state.page || state.error) return null;
  const history = synchronizeAdminModelTokenCursorHistory(histories.current[source], query);
  histories.current[source] = history;
  const previous = previousAdminModelTokenCursor(history);
  return <div className="adm-pagination adm-model-token-pagination"><span>{copy.pageCount(state.items.length, state.page.hasMore)}</span><div className="adm-row-actions">
    <button type="button" disabled={state.loading || !previous.available} onClick={() => { histories.current[source] = { ...history, currentCursor: previous.cursor }; onChange({ ...query, cursor: previous.cursor }); }}>{copy.previous}</button>
    <button type="button" disabled={state.loading || !state.page.nextCursor} onClick={() => { histories.current[source] = rememberAdminModelTokenNextCursor(history, state.page.nextCursor); onChange({ ...query, cursor: state.page.nextCursor }); }}>{copy.next}</button>
  </div></div>;
}

function OwnerDialog({ client, copy, query, histories, onQueryChange, onClose, onCreated }) {
  const root = useRef(null);
  const first = useRef(null);
  const previousFocus = useRef(document.activeElement);
  const ownerType = query.owner_type || 'USER';
  const organizationID = query.organization_id || '';
  const requestQuery = useMemo(() => ownerCandidateQuery(ownerType, query, organizationID), [ownerType, organizationID, JSON.stringify(query)]);
  const [refresh, setRefresh] = useState(0);
  const [selected, setSelected] = useState(null);
  const [selectedOrganization, setSelectedOrganization] = useState(organizationID ? { id: organizationID, type: 'ORGANIZATION', display_label: organizationID } : null);
  const [name, setName] = useState('');
  const [searchDraft, setSearchDraft] = useState(query.q || '');
  const [priority, setPriority] = useState(100);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const load = useMemo(() => (filters, signal) => client.tokenOwnerCandidates(filters, signal), [client]);
  const state = useSource(load, requestQuery, true, refresh);
  useEffect(() => { first.current?.focus(); return () => previousFocus.current?.focus?.(); }, []);
  useEffect(() => { setSelected(null); }, [JSON.stringify(requestQuery)]);
  useEffect(() => { setSearchDraft(query.q || ''); }, [query.q]);
  const update = (patch, mode = 'push') => onQueryChange('ownerCandidates', { ...query, ...patch }, mode);
  const retry = () => {
    if (state.error?.status === 410) {
      delete histories.current.ownerCandidates;
      update({ cursor: '' }, 'replace');
      return;
    }
    setRefresh((value) => value + 1);
  };
  const choose = (candidate) => {
    if (ownerType === 'UNIT' && !organizationID) {
      setSelectedOrganization(candidate);
      update({ organization_id: candidate.id, q: '', cursor: '', status: 'ACTIVE' });
      return;
    }
    setSelected(candidate.status === 'ACTIVE' ? candidate : null);
  };
  const submit = async (event) => {
    event.preventDefault();
    const input = tokenAccountCreateInput({ candidate: selected, organization: selectedOrganization, name, priority });
    if (!input) { setError({ code: 'INVALID_SELECTION' }); return; }
    setBusy(true); setError(null);
    try { await client.createTokenAccount(input); await onCreated(); onClose(); }
    catch (reason) { setError({ code: reason.code || 'REQUEST_FAILED', requestID: reason.requestID || '' }); }
    finally { setBusy(false); }
  };
  const selectingOrganization = ownerType === 'UNIT' && !organizationID;
  return <div className="adm-protocol-dialog-backdrop"><form ref={root} className="adm-protocol-dialog adm-token-dialog" role="dialog" aria-modal="true" aria-labelledby="token-owner-dialog-title" onSubmit={submit} onKeyDown={(event) => { focusWithin(event, root.current); if (event.key === 'Escape' && !busy) onClose(); }}>
    <header><div><h3 id="token-owner-dialog-title">{copy.createTokenAccount}</h3><p>{copy.ownerSelectionLead}</p></div><button ref={first} type="button" disabled={busy} onClick={onClose} aria-label={copy.close}>×</button></header>
    {error && <div className="adm-inline-message error" role="alert">{copy.ownerCreateFailed}{error.requestID && <small>{copy.requestID}: {error.requestID}</small>}</div>}
    <div className="adm-token-dialog-grid"><label>{copy.ownerType}<select value={ownerType} onChange={(event) => { setSelectedOrganization(null); update({ owner_type: event.target.value, organization_id: '', q: '', cursor: '', status: 'ACTIVE' }); }}><option value="USER">{copy.enumLabels.USER}</option><option value="ORGANIZATION">{copy.enumLabels.ORGANIZATION}</option><option value="UNIT">{copy.enumLabels.UNIT}</option></select></label><label>{copy.candidateStatus}<select value={query.status || 'ACTIVE'} onChange={(event) => update({ status: event.target.value, cursor: '' })}><option value="ACTIVE">{copy.enumLabels.ACTIVE}</option><option value="DISABLED">{copy.enumLabels.DISABLED}</option><option value="PROVISIONING">{copy.enumLabels.PROVISIONING}</option></select></label></div>
    {ownerType === 'UNIT' && organizationID && <div className="adm-token-selection"><span>{copy.organizationContext}</span><strong>{selectedOrganization?.display_label || organizationID}</strong><button type="button" onClick={() => { setSelectedOrganization(null); update({ organization_id: '', q: '', cursor: '' }); }}>{copy.changeOrganization}</button></div>}
    <div className="adm-token-candidate-search"><label><span>{selectingOrganization ? copy.findOrganization : copy.findOwner}</span><input type="search" value={searchDraft} placeholder={copy.ownerSearchPlaceholder} onChange={(event) => setSearchDraft(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); update({ q: searchDraft, cursor: '' }); } }} /></label><button type="button" onClick={() => update({ q: searchDraft, cursor: '' })}>{copy.search}</button></div>
    <Feedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-token-candidates" role="listbox" aria-label={selectingOrganization ? copy.findOrganization : copy.findOwner}>{state.items.map((candidate) => <button type="button" role="option" aria-selected={selected?.id === candidate.id} key={`${candidate.type}-${candidate.id}`} disabled={candidate.status !== 'ACTIVE'} onClick={() => choose(candidate)}><span><strong>{candidate.display_label}</strong><small>{copy.enumLabels[candidate.type] || candidate.type}</small></span><Status value={candidate.status} copy={copy} /></button>)}</div>}
    <Paging source="ownerCandidates" query={requestQuery} state={state} copy={copy} histories={histories} onChange={(next) => update({ cursor: next.cursor })} />
    {!selectingOrganization && <><div className="adm-token-dialog-grid"><label>{copy.accountName}<input required maxLength="120" value={name} onChange={(event) => setName(event.target.value)} /></label><label>{copy.priority}<input required type="number" min="0" max="10000" value={priority} onChange={(event) => setPriority(event.target.value)} /></label></div><div className="adm-token-selection"><span>{copy.selectedOwner}</span><strong>{selected?.display_label || copy.notSelected}</strong></div></>}
    <footer><button type="button" disabled={busy} onClick={onClose}>{copy.cancel}</button><button type="submit" disabled={busy || selectingOrganization || !selected}>{busy ? copy.submitting : copy.confirmCreate}</button></footer>
  </form></div>;
}

function RouteDialog({ client, copy, account, onClose, onSaved }) {
  const [query, setQuery] = useState({ q: '', lifecycle_status: 'PUBLISHED', cursor: '' });
  const [searchDraft, setSearchDraft] = useState('');
  const [refresh, setRefresh] = useState(0);
  const [selectedModelID, setSelectedModelID] = useState('');
  const [selectedProtocolVersionID, setSelectedProtocolVersionID] = useState('');
  const [priority, setPriority] = useState(100);
  const [scope, setScope] = useState('NONE');
  const histories = useRef({});
  const load = useMemo(() => (filters, signal) => client.catalogModels(filters, signal), [client]);
  const state = useSource(load, query, true, refresh);
  const choices = stableRouteChoices(state.items);
  const modelOptions = [...new Map(choices.map((item) => [item.modelID, item])).values()];
  const protocolOptions = choices.filter((item) => item.modelID === selectedModelID);
  const choice = protocolOptions.find((item) => item.protocolVersionID === selectedProtocolVersionID);
  const existing = account.routes?.find((route) => route.model_id === choice?.modelID && route.protocol_version_id === choice?.protocolVersionID);
  useEffect(() => { if (choice && existing) { setPriority(existing.priority); setScope(existing.default_scope); } }, [choice?.protocolVersionID, existing?.route_id]);
  const retry = () => {
    if (state.error?.status === 410) {
      histories.current.routeModels = undefined;
      setQuery((current) => ({ ...current, cursor: '' }));
      return;
    }
    setRefresh((value) => value + 1);
  };
  const impactLoader = useMemo(() => choice ? (signal) => client.stableTokenRouteImpact(account.token_account_id, { action: 'UPSERT', model_id: choice.modelID, protocol_version_id: choice.protocolVersionID }, signal) : null, [client, account.token_account_id, choice?.modelID, choice?.protocolVersionID]);
  return <AdminHighRiskActionDialog action={{ key: `route-upsert-${choice?.modelID || 'unselected'}-${choice?.protocolVersionID || ''}`, impactAction: 'TOKEN_ROUTE_UPSERT', title: copy.configureStableRoute, lead: copy.stableRouteLead }} copy={copy} target={{ label: account.name, version: `${copy.actionAccountVersion}: ${account.account_version}` }} policy="AUDIT_ONLY" recentAuth impactLoader={impactLoader} precheckEnabled={Boolean(choice)} payloadDetails={{ model_id: choice?.modelID || '', protocol_version_id: choice?.protocolVersionID || '', priority: Number(priority), default_scope: scope, expected_account_version: account.account_version, expected_route_version: existing?.route_version || 0 }} onClose={onClose} execute={async (payload, key, signal) => {
    const input = stableRouteChange({ choice, accountVersion: account.account_version, existingRoute: existing, priority, defaultScope: scope, reason: payload.reason });
    if (!input) throw Object.assign(new Error('invalid route'), { code: 'INVALID_ROUTE', status: 400 });
    const result = await client.upsertStableTokenRoute(account.token_account_id, input, key, signal);
    if (!validStableRouteWriteResult(result, { tokenAccountID: account.token_account_id, modelID: choice.modelID, protocolVersionID: choice.protocolVersionID, priority: Number(priority), defaultScope: scope, expectedAccountVersion: account.account_version })) throw Object.assign(new Error('invalid stable route response'), { code: 'INVALID_COMMAND_RESPONSE' });
    return result;
  }} onCompleted={onSaved}>
    <legend>{copy.routeConfiguration}</legend>
    <div className="adm-token-candidate-search"><label><span>{copy.findModel}</span><input type="search" value={searchDraft} placeholder={copy.modelSearchPlaceholder} onChange={(event) => setSearchDraft(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); setQuery((current) => ({ ...current, q: searchDraft, cursor: '' })); } }} /></label><button type="button" onClick={() => setQuery((current) => ({ ...current, q: searchDraft, cursor: '' }))}>{copy.search}</button></div>
    <Feedback state={state} copy={copy} onRetry={retry} />
    {choices.length > 0 && <div className="adm-token-dialog-grid"><label>{copy.model}<select required value={selectedModelID} onChange={(event) => { setSelectedModelID(event.target.value); setSelectedProtocolVersionID(''); }}><option value="">{copy.chooseModel}</option>{modelOptions.map((item) => <option key={item.modelID} value={item.modelID}>{item.displayName}</option>)}</select></label><label>{copy.protocol}<select required disabled={!selectedModelID} value={selectedProtocolVersionID} onChange={(event) => setSelectedProtocolVersionID(event.target.value)}><option value="">{copy.chooseProtocol}</option>{protocolOptions.map((item) => <option key={item.protocolVersionID} value={item.protocolVersionID}>{item.protocolCode} · v{item.protocolVersion}</option>)}</select></label></div>}
    <Paging source="routeModels" query={query} state={state} copy={copy} histories={histories} onChange={setQuery} />
    <div className="adm-token-dialog-grid"><label>{copy.priority}<input required type="number" min="0" max="10000" value={priority} onChange={(event) => setPriority(event.target.value)} /></label><label>{copy.defaultScope}<select value={scope} onChange={(event) => setScope(event.target.value)}><option value="NONE">{copy.enumLabels.NONE}</option><option value="UNIT">{copy.enumLabels.UNIT}</option><option value="ORG">{copy.enumLabels.ORG}</option></select></label></div>
    <p className="adm-safe-preview-note">{copy.routeServerOwned}</p>
  </AdminHighRiskActionDialog>;
}

function RouteHealth({ client, copy, accountID, access, onClose }) {
  const [state, setState] = useState({ item: null, loading: true, error: null, updatedAt: null });
  const [refresh, setRefresh] = useState(0);
  const [routeDialog, setRouteDialog] = useState(false);
  const [highRiskAction, setHighRiskAction] = useState(null);
  const showRouteDialog = routeEditorVisible(access, routeDialog);
  useEffect(() => {
    const controller = new globalThis.AbortController();
    setState({ item: null, loading: true, error: null, updatedAt: null });
    void client.tokenAccount(accountID, controller.signal).then((item) => setState({ item, loading: false, error: null, updatedAt: new Date().toISOString() })).catch((error) => { if (error.name !== 'AbortError') setState({ item: null, loading: false, error, updatedAt: null }); });
    return () => controller.abort();
  }, [client, accountID, refresh]);
  useEffect(() => { if (!access.manage) setHighRiskAction(null); if (!access.showRouteWrite) setRouteDialog(false); }, [access.manage, access.showRouteWrite]);
  const reload = async () => setRefresh((value) => value + 1);
  const statusTarget = highRiskAction?.type === 'ENABLE' ? 'ACTIVE' : 'DISABLED';
  const statusTitle = statusTarget === 'ACTIVE' ? copy.enableTokenAccount : copy.disableTokenAccount;
  return <section className="adm-card adm-source-card adm-token-health" aria-labelledby="token-health-title"><div className="adm-source-head"><div><button type="button" className="adm-back" onClick={onClose}>← {copy.backToAccounts}</button><h2 id="token-health-title">{state.item ? `${copy.routeHealth} · ${state.item.name}` : copy.routeHealth}</h2><p>{state.updatedAt ? `${copy.lastUpdated} ${new Date(state.updatedAt).toLocaleString()}` : copy.notUpdated}</p></div><div className="adm-row-actions">{access.manage && state.item?.status === 'ACTIVE' && <button type="button" className="danger" onClick={() => setHighRiskAction({ type: 'DISABLE' })}>{copy.disableTokenAccount}</button>}{access.manage && state.item?.status === 'DISABLED' && <button type="button" onClick={() => setHighRiskAction({ type: 'ENABLE' })}>{copy.enableTokenAccount}</button>}{access.showRouteWrite && state.item && <button type="button" onClick={() => setRouteDialog(true)}>{copy.configureStableRoute}</button>}<button type="button" disabled={state.loading} onClick={reload}>{state.loading ? copy.loading : copy.refreshHealth}</button></div></div>
    {access.explainRouteBlocked && <div className="adm-inline-message info" role="status">{copy.modelPermissionRequired}</div>}
    {state.error && <div className="adm-source-error" role="alert"><strong>{copy.healthUnavailable}</strong>{state.error.requestID && <span>{copy.requestID}: {state.error.requestID}</span>}<button type="button" onClick={reload}>{copy.retry}</button></div>}
    {state.loading && <div className="adm-source-state" role="status">{copy.loading}</div>}
    {state.item && (!state.item.routes?.length ? <div className="adm-source-state">{copy.noRoutes}</div> : <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.model}</th><th>{copy.protocol}</th><th>{copy.health}</th><th>{copy.healthEvidence}</th><th>{copy.routable}</th>{access.manage && <th>{copy.actions}</th>}</tr></thead><tbody>{state.item.routes.map((route) => { const health = serverRouteHealth(route, state.item.as_of); return <tr key={route.route_id}><td data-label={copy.model}><strong>{route.model_id ? copy.stableModel : copy.legacyUnresolved}</strong>{route.model_id && <small className="adm-secondary-id">{route.model_id}</small>}</td><td data-label={copy.protocol}>{route.protocol_code}{route.protocol_version_id && <small className="adm-secondary-id">{route.protocol_version_id}</small>}</td><td data-label={copy.health}><Status value={health.status} copy={copy} /></td><td data-label={copy.healthEvidence}><span>{health.observedAt ? new Date(health.observedAt).toLocaleString() : copy.notObserved}</span><small className="adm-secondary-id">{health.expiresAt ? `${copy.expiresAt} ${new Date(health.expiresAt).toLocaleString()}` : copy.noExpiry}{health.diagnostic ? ` · ${health.diagnostic}` : ''}</small></td><td data-label={copy.routable}>{health.routable ? copy.yes : copy.no}</td>{access.manage && <td data-label={copy.actions}>{route.model_id && route.protocol_version_id && <button type="button" className="danger" onClick={() => setHighRiskAction({ type: 'DELETE_ROUTE', route })}>{copy.deleteRoute}</button>}</td>}</tr>; })}</tbody></table></div>)}
    {showRouteDialog && state.item && <RouteDialog client={client} copy={copy} account={state.item} onClose={() => setRouteDialog(false)} onSaved={reload} />}
    {['DISABLE', 'ENABLE'].includes(highRiskAction?.type) && state.item && access.manage && <AdminHighRiskActionDialog action={{ key: `token-account-status-${statusTarget.toLowerCase()}`, impactAction: `TOKEN_ACCOUNT_STATUS_${statusTarget}`, title: statusTitle, lead: copy.disableTokenAccountLead }} copy={copy} target={{ label: state.item.name, version: `${copy.actionAccountVersion}: ${state.item.account_version}` }} policy="AUDIT_ONLY" recentAuth dangerous={statusTarget === 'DISABLED'} payloadDetails={{ token_account_id: accountID, expected_account_version: state.item.account_version, target_status: statusTarget }} impactLoader={(signal) => client.tokenAccountStatusImpact(accountID, statusTarget, signal)} onClose={() => setHighRiskAction(null)} execute={async (payload, key, signal) => {
      const result = await client.createGovernedTokenCommand(await governedTokenEnvelope({ commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID, reason: payload.reason, impact: payload.precheck, expectedVersions: { token_account_id: accountID, expected_account_version: state.item.account_version, target_status: statusTarget }, command: { target_status: statusTarget } }), key, signal);
      return requireGovernedTokenCommandResult(result, { commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID });
    }} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID })} onCompleted={reload} />}
    {highRiskAction?.type === 'DELETE_ROUTE' && state.item && access.manage && <AdminHighRiskActionDialog action={{ key: `route-delete-${highRiskAction.route.route_id}`, impactAction: 'TOKEN_ROUTE_DELETE', title: copy.deleteRoute, lead: copy.deleteRouteLead }} copy={copy} target={{ label: highRiskAction.route.model_id, version: `${copy.actionRouteVersion}: ${highRiskAction.route.route_version}` }} policy="AUDIT_ONLY" recentAuth dangerous payloadDetails={{ token_account_id: accountID, route_id: highRiskAction.route.route_id, expected_account_version: state.item.account_version, expected_route_version: highRiskAction.route.route_version }} impactLoader={(signal) => client.stableTokenRouteImpact(accountID, { action: 'DELETE', route_id: highRiskAction.route.route_id }, signal)} onClose={() => setHighRiskAction(null)} execute={async (payload, key, signal) => {
      const result = await client.createGovernedTokenCommand(await governedTokenEnvelope({ commandType: 'TOKEN_ROUTE_DELETE', targetResourceType: 'TOKEN_ROUTE', targetResourceID: highRiskAction.route.route_id, reason: payload.reason, impact: payload.precheck, expectedVersions: { token_account_id: accountID, route_id: highRiskAction.route.route_id, expected_account_version: state.item.account_version, expected_route_version: highRiskAction.route.route_version }, command: { route_id: highRiskAction.route.route_id } }), key, signal);
      return requireGovernedTokenCommandResult(result, { commandType: 'TOKEN_ROUTE_DELETE', targetResourceType: 'TOKEN_ROUTE', targetResourceID: highRiskAction.route.route_id });
    }} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_ROUTE_DELETE', targetResourceType: 'TOKEN_ROUTE', targetResourceID: highRiskAction.route.route_id })} onCompleted={reload} />}
  </section>;
}

export function AdminTokenResourcesWorkspace({ client, copy, permissions, formsEnabled, actionsEnabled, routeVersion, onQueryChange, onRouteChange, histories }) {
  const access = tokenResourceAccess(permissions, formsEnabled && actionsEnabled);
  const actionAccess = { ...access, manage: actionsEnabled && access.manage };
  const query = useMemo(() => readAdminModelTokenQuery('tokenAccounts'), [routeVersion]);
  const ownerQuery = useMemo(() => readAdminModelTokenQuery('ownerCandidates'), [routeVersion]);
  const selectedAccountID = useMemo(() => new URLSearchParams(globalThis.location?.search || '').get('token_account_id') || '', [routeVersion]);
  const [refresh, setRefresh] = useState(0);
  const load = useMemo(() => (filters, signal) => client.tokenAccounts(filters, signal), [client]);
  const state = useSource(load, query, true, refresh);
  const retry = () => {
    if (state.error?.status === 410) { delete histories.current.tokenAccounts; onQueryChange('tokenAccounts', { ...query, cursor: '' }, 'replace'); }
    else setRefresh((value) => value + 1);
  };
  const closeOwner = () => onQueryChange('ownerCandidates', { q: '', owner_type: '', status: '', organization_id: '', cursor: '' }, 'push');
  if (selectedAccountID) return <RouteHealth client={client} copy={copy} accountID={selectedAccountID} access={actionAccess} onClose={() => onRouteChange({ token_account_id: null, token_detail_tab: null }, 'push')} />;
  return <>
    {access.explainCreateBlocked && <div className="adm-inline-message info" role="status">{copy.ownerCandidatePermissionRequired}</div>}
    <section className="adm-card adm-source-card" aria-labelledby="token-accounts-title"><div className="adm-source-head"><div><h2 id="token-accounts-title">{copy.tokenAccounts}</h2><p>{state.updatedAt ? `${copy.lastUpdated} ${new Date(state.updatedAt).toLocaleString()}` : copy.notUpdated}</p></div><div className="adm-row-actions">{access.showCreate && <button type="button" onClick={() => onQueryChange('ownerCandidates', { q: '', owner_type: 'USER', status: 'ACTIVE', organization_id: '', cursor: '' }, 'push')}>{copy.createTokenAccount}</button>}<button type="button" disabled={state.loading} onClick={retry}>{state.loading ? copy.loading : copy.refreshSource}</button></div></div>
      <form key={JSON.stringify(query)} className="adm-model-token-filters" onSubmit={(event) => { event.preventDefault(); const values = new globalThis.FormData(event.currentTarget); onQueryChange('tokenAccounts', { ...query, q: values.get('q'), owner_type: values.get('owner_type'), status: values.get('status'), health_status: values.get('health_status'), cursor: '' }); }}><label><span>{copy.tokenSearch}</span><input name="q" type="search" defaultValue={query.q} placeholder={copy.tokenSearchPlaceholder} /></label><label><span>{copy.ownerType}</span><select name="owner_type" defaultValue={query.owner_type}><option value="">{copy.all}</option><option value="USER">{copy.enumLabels.USER}</option><option value="ORG">{copy.enumLabels.ORG}</option><option value="ORG_UNIT">{copy.enumLabels.ORG_UNIT}</option></select></label><label><span>{copy.status}</span><select name="status" defaultValue={query.status}><option value="">{copy.all}</option>{['ACTIVE', 'DISABLED', 'FROZEN'].map((value) => <option key={value} value={value}>{copy.enumLabels[value]}</option>)}</select></label><label><span>{copy.health}</span><select name="health_status" defaultValue={query.health_status}><option value="">{copy.all}</option>{['UNKNOWN', 'PROBING', 'HEALTHY', 'DEGRADED', 'UNHEALTHY', 'STALE'].map((value) => <option key={value} value={value}>{copy.enumLabels[value]}</option>)}</select></label><button type="submit">{copy.search}</button><button type="button" onClick={() => onQueryChange('tokenAccounts', { q: '', owner_type: '', owner_id: '', status: '', health_status: '', credential_status: '', cursor: '' })}>{copy.clear}</button></form>
      <Feedback state={state} copy={copy} onRetry={retry} />
      {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.tokenAccount}</th><th>{copy.owner}</th><th>{copy.status}</th><th>{copy.credentials}</th><th>{copy.routes}</th><th>{copy.actions}</th></tr></thead><tbody>{state.items.map((item) => <tr key={item.token_account_id}><td data-label={copy.tokenAccount}><strong>{item.name}</strong><small className="adm-secondary-id">{item.token_account_id}</small></td><td data-label={copy.owner}>{copy.enumLabels[item.owner_type] || item.owner_type}<small className="adm-secondary-id">{item.owner_id}</small></td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.credentials}>{item.active_credential_count}</td><td data-label={copy.routes}>{item.route_count}</td><td data-label={copy.actions}><button type="button" onClick={() => onRouteChange({ token_account_id: item.token_account_id, token_detail_tab: 'routes' }, 'push')}>{copy.viewRouteHealth}</button></td></tr>)}</tbody></table></div>}
      <Paging source="tokenAccounts" query={query} state={state} copy={copy} histories={histories} onChange={(next) => onQueryChange('tokenAccounts', next)} />
    </section>
    {access.requestOwnerCandidates && ownerQuery.owner_type && <OwnerDialog client={client} copy={copy} query={ownerQuery} histories={histories} onQueryChange={onQueryChange} onClose={closeOwner} onCreated={async () => setRefresh((value) => value + 1)} />}
  </>;
}
