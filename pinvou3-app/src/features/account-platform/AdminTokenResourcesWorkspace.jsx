import { TokenResourceFeedback, TokenResourcePaging } from './TokenResourcePrimitives.jsx';
import { useAdminSource } from './AdminModelTokenPrimitives.jsx';
import { TokenAccountGovernanceActions } from './TokenAccountGovernanceActions.jsx';
import { secureRequestID } from './request-id.js';
import React, { useEffect, useMemo, useRef, useState } from 'react';
import { readAdminModelTokenQuery } from './admin-navigation.js';
import { ownerCandidateQuery, serverRouteHealth, tokenAccountCreateInput, tokenResourceAccess } from './admin-token-resource-state.js';
import { AdminHighRiskActionDialog } from './AdminHighRiskActionDialog.jsx';
import { governedTokenEnvelope, requireGovernedTokenCommandResult, requireGovernedTokenPrecheck, validTokenAccountRecoveryActionResult } from './admin-high-risk-action-state.js';
import { classifyTokenAccountRecoveryFailure, recoverTokenCommandByKey } from './admin-token-command-recovery.js';


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

function TokenAccountRecoveryDialog({ client, copy, command, onClose, onCompleted }) {
  const root = useRef(null);
  const first = useRef(null);
  const controller = useRef(null);
  const attempt = useRef(null);
  const [action, setAction] = useState('REQUEUE');
  const [reason, setReason] = useState('');
  const [state, setState] = useState({ busy: false, error: null, result: null });
  useEffect(() => { first.current?.focus(); return () => controller.current?.abort(); }, []);
  const submit = async (event) => {
    event.preventDefault();
    if (state.busy || [...reason.trim()].length < 10 || [...reason.trim()].length > 500) return;
    const payload = { action, expected_recovery_epoch: command.recovery_epoch, reason: reason.trim() };
    const payloadKey = JSON.stringify(payload);
    if (attempt.current?.unknown && attempt.current.payloadKey !== payloadKey) {
      setState({ busy: false, result: null, error: { code: 'COMMAND_PAYLOAD_LOCKED' } });
      return;
    }
    if (!attempt.current || attempt.current.payloadKey !== payloadKey) attempt.current = { key: `token-account-recovery-${secureRequestID()}`, payloadKey, actionID: '', unknown: false };
    const recovering = Boolean(attempt.current.actionID || attempt.current.unknown);
    controller.current?.abort();
    controller.current = new globalThis.AbortController();
    setState({ busy: true, error: null, result: null });
    try {
      let result;
      if (attempt.current.actionID) {
        result = await client.tokenAccountRecoveryAction(command.command_reference, attempt.current.actionID, controller.current.signal);
      } else if (attempt.current.unknown) {
        result = await client.tokenAccountRecoveryActionByIdempotencyKey(command.command_reference, attempt.current.key, controller.current.signal);
      } else {
        result = await client.createTokenAccountRecoveryAction(command.command_reference, payload, attempt.current.key, controller.current.signal);
      }
      if (!validTokenAccountRecoveryActionResult(result, { commandReference: command.command_reference, action })) throw Object.assign(new Error('invalid recovery action response'), { code: 'INVALID_COMMAND_RESPONSE' });
      attempt.current.unknown = result.status === 'PENDING';
      attempt.current.actionID = result.action_id;
      setState({ busy: false, error: null, result });
      if (result.status === 'SUCCEEDED') void onCompleted?.(result);
    } catch (error) {
      if (error?.name === 'AbortError') return;
      const actionID = String(error?.details?.action_id || '');
      if (actionID) attempt.current.actionID = actionID;
      attempt.current.unknown = classifyTokenAccountRecoveryFailure(error, recovering).unknown;
      setState({ busy: false, result: null, error: { code: attempt.current.unknown ? 'COMMAND_RESULT_UNKNOWN' : error?.code || 'REQUEST_FAILED', requestID: error?.requestID || '' } });
    }
  };
  return <div className="adm-protocol-dialog-backdrop"><form ref={root} className="adm-protocol-dialog adm-high-risk-dialog" role="dialog" aria-modal="true" aria-labelledby="token-account-recovery-title" onSubmit={submit} onKeyDown={(event) => { focusWithin(event, root.current); if (event.key === 'Escape' && !state.busy && !attempt.current?.unknown) onClose(); }}>
    <header><div><h3 id="token-account-recovery-title">{copy.accountRecoveryTitle}</h3><p>{copy.accountRecoveryLead}</p></div><button ref={first} type="button" disabled={state.busy || attempt.current?.unknown} onClick={onClose} aria-label={copy.close}>×</button></header>
    <dl className="adm-command-target"><div><dt>{copy.commandReference}</dt><dd>{command.command_reference}</dd></div><div><dt>{copy.accountRecoveryEpoch}</dt><dd>{command.recovery_epoch}</dd></div></dl>
    <div className="adm-inline-message info" role="status">{copy.recentAuthRequired}</div>
    {state.error && <div className="adm-inline-message error" role="alert"><strong>{state.error.code === 'COMMAND_RESULT_UNKNOWN' ? copy.actionUnknown : copy.accountRecoveryFailed}</strong>{state.error.requestID && <small>{copy.requestID}: {state.error.requestID}</small>}</div>}
    {state.result && <div className="adm-inline-message success" role="status"><strong>{copy.accountRecoveryAccepted}</strong><small>{copy.actionAuditReference}: {state.result.audit_reference}</small></div>}
    <label>{copy.accountRecoveryAction}<select value={action} disabled={state.busy || attempt.current?.unknown || state.result?.status === 'SUCCEEDED'} onChange={(event) => { setAction(event.target.value); setState({ busy: false, error: null, result: null }); attempt.current = null; }}><option value="REQUEUE">{copy.accountRecoveryRequeue}</option><option value="CANCEL">{copy.accountRecoveryCancel}</option></select></label>
    <label>{copy.reason}<textarea required minLength="10" maxLength="500" rows="4" disabled={state.busy || attempt.current?.unknown || state.result?.status === 'SUCCEEDED'} value={reason} onChange={(event) => setReason(event.target.value)} /></label>
    <p className="adm-safe-preview-note">{copy.accountRecoveryNotice}</p>
    <footer><button type="button" disabled={state.busy || attempt.current?.unknown} onClick={onClose}>{copy.close}</button>{state.result?.status !== 'SUCCEEDED' && <button className={action === 'CANCEL' ? 'danger' : ''} type="submit" disabled={state.busy || [...reason.trim()].length < 10}>{state.busy ? copy.submitting : attempt.current?.unknown ? copy.actionRecoverSameKey : copy.confirm}</button>}</footer>
  </form></div>;
}

function OwnerDialog({ client, copy, query, histories, onQueryChange, onClose, onCreated }) {
  const root = useRef(null);
  const first = useRef(null);
  const previousFocus = useRef(document.activeElement);
  const ownerType = query.owner_type || 'USER';
  const organizationID = query.organization_id || '';
  const queryKey = JSON.stringify(query);
  const requestQuery = useMemo(() => ownerCandidateQuery(ownerType, query, organizationID), [ownerType, organizationID, queryKey]);
  const requestQueryKey = JSON.stringify(requestQuery);
  const [refresh, setRefresh] = useState(0);
  const [selected, setSelected] = useState(null);
  const [selectedOrganization, setSelectedOrganization] = useState(organizationID ? { id: organizationID, type: 'ORGANIZATION', display_label: organizationID } : null);
  const [name, setName] = useState('');
  const [searchDraft, setSearchDraft] = useState(query.q || '');
  const [priority, setPriority] = useState(100);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
	const [pendingInput, setPendingInput] = useState(null);
	const [manualReview, setManualReview] = useState(null);
  const load = useMemo(() => (filters, signal) => client.tokenOwnerCandidates(filters, signal), [client]);
  const state = useAdminSource({ load, query: requestQuery, enabled: true, refreshVersion: refresh });
  useEffect(() => { first.current?.focus(); return () => previousFocus.current?.focus?.(); }, []);
  useEffect(() => { setSelected(null); }, [requestQueryKey]);
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
		setError(null);
		setPendingInput(input);
  };
  const selectingOrganization = ownerType === 'UNIT' && !organizationID;
	return <>{!pendingInput && <div className="adm-protocol-dialog-backdrop"><form ref={root} className="adm-protocol-dialog adm-token-dialog" role="dialog" aria-modal="true" aria-labelledby="token-owner-dialog-title" onSubmit={submit} onKeyDown={(event) => { focusWithin(event, root.current); if (event.key === 'Escape' && !busy) onClose(); }}>
    <header><div><h3 id="token-owner-dialog-title">{copy.createTokenAccount}</h3><p>{copy.ownerSelectionLead}</p></div><button ref={first} type="button" disabled={busy} onClick={onClose} aria-label={copy.close}>×</button></header>
    {error && <div className="adm-inline-message error" role="alert">{copy.ownerCreateFailed}{error.requestID && <small>{copy.requestID}: {error.requestID}</small>}</div>}
    <div className="adm-token-dialog-grid"><label>{copy.ownerType}<select value={ownerType} onChange={(event) => { setSelectedOrganization(null); update({ owner_type: event.target.value, organization_id: '', q: '', cursor: '', status: 'ACTIVE' }); }}><option value="USER">{copy.enumLabels.USER}</option></select></label><label>{copy.candidateStatus}<select value={query.status || 'ACTIVE'} onChange={(event) => update({ status: event.target.value, cursor: '' })}><option value="ACTIVE">{copy.enumLabels.ACTIVE}</option><option value="DISABLED">{copy.enumLabels.DISABLED}</option><option value="PROVISIONING">{copy.enumLabels.PROVISIONING}</option></select></label></div>
    {ownerType === 'UNIT' && organizationID && <div className="adm-token-selection"><span>{copy.organizationContext}</span><strong>{selectedOrganization?.display_label || organizationID}</strong><button type="button" onClick={() => { setSelectedOrganization(null); update({ organization_id: '', q: '', cursor: '' }); }}>{copy.changeOrganization}</button></div>}
    <div className="adm-token-candidate-search"><label><span>{selectingOrganization ? copy.findOrganization : copy.findOwner}</span><input type="search" value={searchDraft} placeholder={copy.ownerSearchPlaceholder} onChange={(event) => setSearchDraft(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); update({ q: searchDraft, cursor: '' }); } }} /></label><button type="button" onClick={() => update({ q: searchDraft, cursor: '' })}>{copy.search}</button></div>
    <TokenResourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-token-candidates" role="listbox" aria-label={selectingOrganization ? copy.findOrganization : copy.findOwner}>{state.items.map((candidate) => <button type="button" role="option" aria-selected={selected?.id === candidate.id} key={`${candidate.type}-${candidate.id}`} disabled={candidate.status !== 'ACTIVE'} onClick={() => choose(candidate)}><span><strong>{candidate.display_label}</strong><small>{copy.enumLabels[candidate.type] || candidate.type}</small></span><Status value={candidate.status} copy={copy} /></button>)}</div>}
    <TokenResourcePaging source="ownerCandidates" query={requestQuery} state={state} copy={copy} histories={histories} onChange={(next) => update({ cursor: next.cursor })} />
    {!selectingOrganization && <><div className="adm-token-dialog-grid"><label>{copy.accountName}<input required maxLength="120" value={name} onChange={(event) => setName(event.target.value)} /></label><label>{copy.priority}<input required type="number" min="0" max="10000" value={priority} onChange={(event) => setPriority(event.target.value)} /></label></div><div className="adm-token-selection"><span>{copy.selectedOwner}</span><strong>{selected?.display_label || copy.notSelected}</strong></div></>}
    <footer><button type="button" disabled={busy} onClick={onClose}>{copy.cancel}</button><button type="submit" disabled={busy || selectingOrganization || !selected}>{busy ? copy.submitting : copy.confirmCreate}</button></footer>
	</form></div>}
	{pendingInput && <AdminHighRiskActionDialog action={{ key: `token-account-create-${pendingInput.owner_type}-${pendingInput.owner_id}`, impactAction: 'TOKEN_ACCOUNT_CREATE', title: copy.createTokenAccount, lead: copy.ownerSelectionLead }} copy={copy} target={{ label: name, version: selected?.display_label || pendingInput.owner_id }} policy="AUDIT_ONLY" recentAuth payloadDetails={pendingInput} impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_ACCOUNT_CREATE', target_resource_type: 'TOKEN_ACCOUNT', target_resource_id: pendingInput.owner_id, command: pendingInput }, signal), { commandType: 'TOKEN_ACCOUNT_CREATE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: pendingInput.owner_id })} onClose={() => setPendingInput(null)} onManualReview={(result) => { setPendingInput(null); setManualReview(result); }} execute={async (payload, key, signal) => {
		const expected = { commandType: 'TOKEN_ACCOUNT_CREATE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: pendingInput.owner_id };
		const envelope = await governedTokenEnvelope({ commandType: expected.commandType, targetResourceType: expected.targetResourceType, targetResourceID: expected.targetResourceID, reason: payload.reason, impact: payload.precheck, command: pendingInput });
		try { return requireGovernedTokenCommandResult(await client.createGovernedTokenCommand(envelope, key, signal), expected); }
		catch (error) {
			if (error?.commandReference || error?.finalResult || (Number(error?.status) > 0 && Number(error?.status) < 500)
				|| ['TOKEN_ACCOUNT_CREATE_NOT_ACTIVE', 'TOKEN_ACCOUNT_POLICY_UNAVAILABLE', 'TOKEN_ACCOUNT_POLICY_CONFIG_MISMATCH'].includes(error?.code)) throw error;
			return recoverTokenCommandByKey(client, expected, key, signal);
		}
	}} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_ACCOUNT_CREATE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: pendingInput.owner_id })} recoverByIdempotencyKey={async (key, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandByIdempotencyKey('TOKEN_ACCOUNT_CREATE', key, signal), { commandType: 'TOKEN_ACCOUNT_CREATE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: pendingInput.owner_id })} onCompleted={async () => { await onCreated(); onClose(); }} />}
	{manualReview && <TokenAccountRecoveryDialog client={client} copy={copy} command={manualReview} onClose={() => setManualReview(null)} onCompleted={async () => { await onCreated(); setManualReview(null); onClose(); }} />}
	</>;
}

function RouteHealth({ client, copy, accountID, access, onClose }) {
  const [state, setState] = useState({ item: null, loading: true, error: null, updatedAt: null });
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    const controller = new globalThis.AbortController();
    setState({ item: null, loading: true, error: null, updatedAt: null });
    void client.tokenAccount(accountID, controller.signal).then((item) => setState({ item, loading: false, error: null, updatedAt: new Date().toISOString() })).catch((error) => { if (error.name !== 'AbortError') setState({ item: null, loading: false, error, updatedAt: null }); });
    return () => controller.abort();
  }, [client, accountID, refresh]);
  const reload = async () => setRefresh((value) => value + 1);
  return <section className="adm-card adm-source-card adm-token-health" aria-labelledby="token-health-title"><div className="adm-source-head"><div><button type="button" className="adm-back" onClick={onClose}>← {copy.backToAccounts}</button><h2 id="token-health-title">{state.item ? `${copy.routeHealth} · ${state.item.name}` : copy.routeHealth}</h2><p>{state.updatedAt ? `${copy.lastUpdated} ${new Date(state.updatedAt).toLocaleString()}` : copy.notUpdated}</p></div><div className="adm-row-actions"><button type="button" disabled={state.loading} onClick={reload}>{state.loading ? copy.loading : copy.refreshHealth}</button></div></div>
    {state.error && <div className="adm-source-error" role="alert"><strong>{copy.healthUnavailable}</strong>{state.error.requestID && <span>{copy.requestID}: {state.error.requestID}</span>}<button type="button" onClick={reload}>{copy.retry}</button></div>}
    {state.loading && <div className="adm-source-state" role="status">{copy.loading}</div>}
    {state.item && <TokenAccountGovernanceActions client={client} copy={copy} account={state.item} access={access} onSaved={reload}>{(onDeleteRoute) => (    !state.item.routes?.length ? <div className="adm-source-state">{copy.noRoutes}</div> : <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.model}</th><th>{copy.protocol}</th><th>{copy.health}</th><th>{copy.healthEvidence}</th><th>{copy.routable}</th>{access.manage && <th>{copy.actions}</th>}</tr></thead><tbody>{state.item.routes.map((route) => { const health = serverRouteHealth(route, state.item.as_of); return <tr key={route.route_id}><td data-label={copy.model}><strong>{route.model_id ? copy.stableModel : copy.legacyUnresolved}</strong>{route.model_id && <small className="adm-secondary-id">{route.model_id}</small>}</td><td data-label={copy.protocol}>{route.protocol_code}{route.protocol_version_id && <small className="adm-secondary-id">{route.protocol_version_id}</small>}</td><td data-label={copy.health}><Status value={health.status} copy={copy} /></td><td data-label={copy.healthEvidence}><span>{health.observedAt ? new Date(health.observedAt).toLocaleString() : copy.notObserved}</span><small className="adm-secondary-id">{health.expiresAt ? `${copy.expiresAt} ${new Date(health.expiresAt).toLocaleString()}` : copy.noExpiry}{health.diagnostic ? ` · ${health.diagnostic}` : ''}</small></td><td data-label={copy.routable}>{health.routable ? copy.yes : copy.no}</td>{access.manage && <td data-label={copy.actions}>{route.model_id && route.protocol_version_id && <button type="button" className="danger" onClick={() => onDeleteRoute(route)}>{copy.deleteRoute}</button>}</td>}</tr>; })}</tbody></table></div>)}</TokenAccountGovernanceActions>}
  </section>;
}

function AccountRecoveryQueue({ client, copy, routeVersion, onQueryChange, histories, enabled, unavailableReason }) {
  const query = useMemo(() => readAdminModelTokenQuery('accountRecovery'), [routeVersion]);
  const [refresh, setRefresh] = useState(0);
  const [selected, setSelected] = useState(null);
  const load = useMemo(() => (filters, signal) => client.tokenAccountRecoveryQueue(filters, signal), [client]);
  const state = useAdminSource({ load, query, enabled, refreshVersion: refresh });
  useEffect(() => { if (!enabled) setSelected(null); }, [enabled]);
  const retry = () => {
    if (state.error?.status === 410) {
      delete histories.current.accountRecovery;
      onQueryChange('accountRecovery', { ...query, cursor: '' }, 'replace');
    } else setRefresh((value) => value + 1);
  };
  return <section className="adm-card adm-source-card" aria-labelledby="account-recovery-queue-title">
    <div className="adm-source-head"><div><h2 id="account-recovery-queue-title">{copy.accountRecoveryQueue}</h2><p>{copy.accountRecoveryQueueLead}</p></div><button type="button" disabled={!enabled || state.loading} onClick={retry}>{state.loading ? copy.loading : copy.refreshSource}</button></div>
    {!enabled && <div className="adm-inline-message info" role="status">{copy.tokenResourceUnavailableReasons?.[unavailableReason] || copy.sourceUnavailable}</div>}
    {enabled && <form key={query.q} className="adm-model-token-filters" onSubmit={(event) => { event.preventDefault(); const values = new globalThis.FormData(event.currentTarget); onQueryChange('accountRecovery', { q: values.get('q'), cursor: '' }); }}>
      <label><span>{copy.commandReference}</span><input type="search" name="q" defaultValue={query.q} maxLength="36" placeholder={copy.accountRecoverySearch} /></label><button type="submit">{copy.search}</button><button type="button" onClick={() => onQueryChange('accountRecovery', { q: '', cursor: '' })}>{copy.clear}</button>
    </form>}
    {enabled && <TokenResourceFeedback state={state} copy={copy} onRetry={retry} />}
    {enabled && state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.commandReference}</th><th>{copy.status}</th><th>{copy.accountRecoveryEpoch}</th><th>{copy.lastUpdated}</th><th>{copy.actions}</th></tr></thead><tbody>{state.items.map((item) => <tr key={item.command_reference}>
      <td data-label={copy.commandReference}><span className="adm-secondary-id">{item.command_reference}</span></td><td data-label={copy.status}>{copy.accountRecoveryRequired}</td><td data-label={copy.accountRecoveryEpoch}>{item.recovery_epoch}</td><td data-label={copy.lastUpdated}>{new Date(item.updated_at).toLocaleString()}</td><td data-label={copy.actions}><button type="button" onClick={() => setSelected(item)}>{copy.accountRecoveryHandle}</button></td>
    </tr>)}</tbody></table></div>}
    {enabled && <TokenResourcePaging source="accountRecovery" query={query} state={state} copy={copy} histories={histories} onChange={(next) => onQueryChange('accountRecovery', next)} />}
    {enabled && selected && <TokenAccountRecoveryDialog client={client} copy={copy} command={selected} onClose={() => setSelected(null)} onCompleted={() => { delete histories.current.accountRecovery; onQueryChange('accountRecovery', { ...query, cursor: '' }, 'replace'); setRefresh((value) => value + 1); }} />}
  </section>;
}

export function AdminTokenResourcesWorkspace({ client, copy, permissions, formsEnabled, actionsEnabled, detailsEnabled, routeVersion, onQueryChange, onRouteChange, histories }) {
  const access = tokenResourceAccess(permissions, formsEnabled && actionsEnabled);
  const actionAccess = { ...access, manage: actionsEnabled && access.manage };
  const query = useMemo(() => readAdminModelTokenQuery('tokenAccounts'), [routeVersion]);
  const ownerQuery = useMemo(() => readAdminModelTokenQuery('ownerCandidates'), [routeVersion]);
  const selectedAccountID = useMemo(() => new URLSearchParams(globalThis.location?.search || '').get('token_account_id') || '', [routeVersion]);
  const [readinessRefresh, setReadinessRefresh] = useState(0);
  const [refresh, setRefresh] = useState(0);
  const readinessLoad = useMemo(() => async (_query, signal) => ({ items: [await client.tokenResourceReadiness(signal)] }), [client]);
  const readinessState = useAdminSource({ load: readinessLoad, query: {}, enabled: true, refreshVersion: readinessRefresh });
  const readiness = readinessState.loading || readinessState.error ? null : readinessState.items[0] || null;
  const snapshotMode = readiness?.token_accounts_mode === 'SNAPSHOT';
  const accountsEnabled = Boolean(readiness) && readiness.token_accounts_mode !== 'BLOCKED';
  const load = useMemo(() => (filters, signal) => snapshotMode ? client.tokenAccounts(filters, signal) : client.tokenAccounts(signal), [client, snapshotMode]);
  const state = useAdminSource({ load, query, enabled: accountsEnabled, refreshVersion: refresh });
  const retry = () => {
    if (state.error?.status === 410) { delete histories.current.tokenAccounts; onQueryChange('tokenAccounts', { ...query, cursor: '' }, 'replace'); }
    else setReadinessRefresh((value) => value + 1);
  };
  const closeOwner = () => onQueryChange('ownerCandidates', { q: '', owner_type: '', status: '', organization_id: '', cursor: '' }, 'push');
	if (selectedAccountID && !detailsEnabled) return <RouteHealth client={client} copy={copy} accountID={selectedAccountID} access={actionAccess} onClose={() => onRouteChange({ token_account_id: null, token_detail_tab: null }, 'push')} />;
  return <>
    {access.explainCreateBlocked && <div className="adm-inline-message info" role="status">{copy.ownerCandidatePermissionRequired}</div>}
    <section className="adm-card adm-source-card" aria-labelledby="token-accounts-title"><div className="adm-source-head"><div><h2 id="token-accounts-title">{copy.tokenAccounts}</h2><p>{state.updatedAt ? `${copy.lastUpdated} ${new Date(state.updatedAt).toLocaleString()}` : copy.notUpdated}</p></div><div className="adm-row-actions">{access.showCreate && <button type="button" onClick={() => onQueryChange('ownerCandidates', { q: '', owner_type: 'USER', status: 'ACTIVE', organization_id: '', cursor: '' }, 'push')}>{copy.createTokenAccount}</button>}<button type="button" disabled={state.loading || readinessState.loading} onClick={retry}>{state.loading || readinessState.loading ? copy.loading : copy.refreshSource}</button></div></div>
      {readinessState.error && <TokenResourceFeedback state={readinessState} copy={copy} onRetry={retry} />}
      {readinessState.loading && !readiness && <div className="adm-source-state" role="status">{copy.loading}</div>}
      {readiness?.token_accounts_mode === 'BLOCKED' && <div className="adm-inline-message error" role="alert">{copy.tokenResourceUnavailableReasons?.[readiness.recovery_unavailable_reason] || copy.sourceUnavailable}</div>}
      {readiness?.token_accounts_mode === 'LEGACY_BOUNDED' && <div className="adm-inline-message info" role="status">{copy.tokenAccountsLegacyMode}</div>}
      {snapshotMode && <form key={JSON.stringify(query)} className="adm-model-token-filters" onSubmit={(event) => { event.preventDefault(); const values = new globalThis.FormData(event.currentTarget); onQueryChange('tokenAccounts', { ...query, q: values.get('q'), owner_type: values.get('owner_type'), status: values.get('status'), health_status: values.get('health_status'), cursor: '' }); }}><label><span>{copy.tokenSearch}</span><input name="q" type="search" defaultValue={query.q} placeholder={copy.tokenSearchPlaceholder} /></label><label><span>{copy.ownerType}</span><select name="owner_type" defaultValue={query.owner_type}><option value="">{copy.all}</option><option value="USER">{copy.enumLabels.USER}</option></select></label><label><span>{copy.status}</span><select name="status" defaultValue={query.status}><option value="">{copy.all}</option>{['ACTIVE', 'DISABLED', 'FROZEN'].map((value) => <option key={value} value={value}>{copy.enumLabels[value]}</option>)}</select></label><label><span>{copy.health}</span><select name="health_status" defaultValue={query.health_status}><option value="">{copy.all}</option>{['UNKNOWN', 'PROBING', 'HEALTHY', 'DEGRADED', 'UNHEALTHY', 'STALE'].map((value) => <option key={value} value={value}>{copy.enumLabels[value]}</option>)}</select></label><button type="submit">{copy.search}</button><button type="button" onClick={() => onQueryChange('tokenAccounts', { q: '', owner_type: '', owner_id: '', status: '', health_status: '', credential_status: '', cursor: '' })}>{copy.clear}</button></form>}
      {accountsEnabled && <TokenResourceFeedback state={state} copy={copy} onRetry={retry} />}
			{accountsEnabled && state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.tokenAccount}</th><th>{copy.owner}</th><th>{copy.status}</th><th>{copy.credentials}</th><th>{copy.routes}</th><th>{copy.actions}</th></tr></thead><tbody>{state.items.map((item) => <tr key={item.token_account_id}><td data-label={copy.tokenAccount}><strong>{item.name}</strong><small className="adm-secondary-id">{item.token_account_id}</small></td><td data-label={copy.owner}>{copy.enumLabels[item.owner_type] || item.owner_type}<small className="adm-secondary-id">{item.owner_id}</small></td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.credentials}>{item.active_credential_count}</td><td data-label={copy.routes}>{item.route_count}</td><td data-label={copy.actions}><button type="button" onClick={() => onRouteChange({ token_account_id: item.token_account_id, token_detail_tab: detailsEnabled ? 'overview' : 'routes' }, 'push')}>{detailsEnabled ? copy.viewDetail : copy.viewRouteHealth}</button></td></tr>)}</tbody></table></div>}
      {snapshotMode && <TokenResourcePaging source="tokenAccounts" query={query} state={state} copy={copy} histories={histories} onChange={(next) => onQueryChange('tokenAccounts', next)} />}
    </section>
    {actionsEnabled && access.manage && access.ownerCandidates && readiness && <AccountRecoveryQueue client={client} copy={copy} routeVersion={routeVersion} onQueryChange={onQueryChange} histories={histories} enabled={readiness.recovery_queue_available} unavailableReason={readiness.recovery_unavailable_reason} />}
    {access.requestOwnerCandidates && ownerQuery.owner_type && <OwnerDialog client={client} copy={copy} query={ownerQuery} histories={histories} onQueryChange={onQueryChange} onClose={closeOwner} onCreated={async () => setRefresh((value) => value + 1)} />}
  </>;
}
