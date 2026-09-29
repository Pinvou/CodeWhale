import { secureRequestID } from './request-id.js';
import React, { useEffect, useMemo, useRef, useState } from 'react';
import { previousAdminModelTokenCursor, readAdminModelTokenQuery, rememberAdminModelTokenNextCursor, synchronizeAdminModelTokenCursorHistory } from './admin-navigation.js';
import { executeAdminSourceRequest, initialAdminSourceState, reduceAdminSourceState, visibleAdminSourceState } from './admin-model-token-state.js';
import { ownerCandidateQuery, routeEditorVisible, serverRouteHealth, stableRouteChoices, tokenAccountCreateInput, tokenResourceAccess } from './admin-token-resource-state.js';
import { AdminHighRiskActionDialog } from './AdminHighRiskActionDialog.jsx';
import { governedTokenEnvelope, requireGovernedRouteCommandResult, requireGovernedTokenCommandResult, requireGovernedTokenPrecheck, validTokenAccountRecoveryActionResult } from './admin-high-risk-action-state.js';

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
  if (state.error) return <div className="adm-source-error" role="alert"><strong>{state.error.status === 410 ? copy.cursorExpired : copy.tokenResourceSourceErrors?.[state.error.code] || copy.sourceUnavailable}</strong>{state.error.requestID && <span>{copy.requestID}: {state.error.requestID}</span>}<button type="button" onClick={onRetry}>{copy.retry}</button></div>;
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
    controller.current?.abort();
    controller.current = new globalThis.AbortController();
    setState({ busy: true, error: null, result: null });
    try {
      let result;
      if (attempt.current.actionID) {
        result = await client.tokenAccountRecoveryAction(command.command_reference, attempt.current.actionID, controller.current.signal);
      } else if (attempt.current.unknown) {
        try {
          result = await client.tokenAccountRecoveryActionByIdempotencyKey(command.command_reference, attempt.current.key, controller.current.signal);
        } catch (lookupError) {
          if (lookupError?.status !== 404 || lookupError?.code !== 'NOT_FOUND') throw lookupError;
          attempt.current.unknown = false;
          result = await client.createTokenAccountRecoveryAction(command.command_reference, payload, attempt.current.key, controller.current.signal);
        }
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
      attempt.current.unknown = Number(error?.status) === 0 || Number(error?.status) >= 500;
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
  const requestQuery = useMemo(() => ownerCandidateQuery(ownerType, query, organizationID), [ownerType, organizationID, JSON.stringify(query)]);
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
		setError(null);
		setPendingInput(input);
  };
  const selectingOrganization = ownerType === 'UNIT' && !organizationID;
	return <>{!pendingInput && <div className="adm-protocol-dialog-backdrop"><form ref={root} className="adm-protocol-dialog adm-token-dialog" role="dialog" aria-modal="true" aria-labelledby="token-owner-dialog-title" onSubmit={submit} onKeyDown={(event) => { focusWithin(event, root.current); if (event.key === 'Escape' && !busy) onClose(); }}>
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
	</form></div>}
	{pendingInput && <AdminHighRiskActionDialog action={{ key: `token-account-create-${pendingInput.owner_type}-${pendingInput.owner_id}`, impactAction: 'TOKEN_ACCOUNT_CREATE', title: copy.createTokenAccount, lead: copy.ownerSelectionLead }} copy={copy} target={{ label: name, version: selected?.display_label || pendingInput.owner_id }} policy="AUDIT_ONLY" recentAuth payloadDetails={pendingInput} impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_ACCOUNT_CREATE', target_resource_type: 'TOKEN_ACCOUNT', target_resource_id: pendingInput.owner_id, command: pendingInput }, signal), { commandType: 'TOKEN_ACCOUNT_CREATE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: pendingInput.owner_id })} onClose={() => setPendingInput(null)} onManualReview={(result) => { setPendingInput(null); setManualReview(result); }} execute={async (payload, key, signal) => {
		const expected = { commandType: 'TOKEN_ACCOUNT_CREATE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: pendingInput.owner_id };
		const envelope = await governedTokenEnvelope({ commandType: expected.commandType, targetResourceType: expected.targetResourceType, targetResourceID: expected.targetResourceID, reason: payload.reason, impact: payload.precheck, command: pendingInput });
		try { return requireGovernedTokenCommandResult(await client.createGovernedTokenCommand(envelope, key, signal), expected); }
		catch (error) {
			if (error?.commandReference || error?.finalResult || (Number(error?.status) > 0 && Number(error?.status) < 500)
				|| ['TOKEN_ACCOUNT_CREATE_NOT_ACTIVE', 'TOKEN_ACCOUNT_POLICY_UNAVAILABLE', 'TOKEN_ACCOUNT_POLICY_CONFIG_MISMATCH'].includes(error?.code)) throw error;
			return requireGovernedTokenCommandResult(await client.governedTokenCommandByIdempotencyKey(expected.commandType, key, signal), expected);
		}
	}} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_ACCOUNT_CREATE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: pendingInput.owner_id })} recoverByIdempotencyKey={async (key, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandByIdempotencyKey('TOKEN_ACCOUNT_CREATE', key, signal), { commandType: 'TOKEN_ACCOUNT_CREATE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: pendingInput.owner_id })} onCompleted={async () => { await onCreated(); onClose(); }} />}
	{manualReview && <TokenAccountRecoveryDialog client={client} copy={copy} command={manualReview} onClose={() => setManualReview(null)} onCompleted={async () => { await onCreated(); setManualReview(null); onClose(); }} />}
	</>;
}

function RouteDialog({ client, copy, account, onClose, onSaved }) {
  const submittedExpected = useRef(null);
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
  const command = useMemo(() => ({ token_account_id: account.token_account_id, model_id: choice?.modelID || '', protocol_version_id: choice?.protocolVersionID || '', priority: Number(priority), default_scope: scope }), [account.token_account_id, choice?.modelID, choice?.protocolVersionID, priority, scope]);
  const expected = { commandType: 'TOKEN_ROUTE_UPSERT', targetResourceType: 'TOKEN_ROUTE', targetResourceID: account.token_account_id, tokenAccountID: account.token_account_id, modelID: choice?.modelID, protocolVersionID: choice?.protocolVersionID, priority: Number(priority), defaultScope: scope, expectedAccountVersion: account.account_version, expectedRouteVersion: existing?.route_version || 0 };
  const impactLoader = useMemo(() => choice ? async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_ROUTE_UPSERT', target_resource_type: 'TOKEN_ROUTE', target_resource_id: account.token_account_id, command }, signal), { commandType: 'TOKEN_ROUTE_UPSERT', targetResourceType: 'TOKEN_ROUTE', targetResourceID: account.token_account_id }) : null, [client, account.token_account_id, choice?.modelID, choice?.protocolVersionID, command]);
  return <AdminHighRiskActionDialog action={{ key: `route-upsert-${choice?.modelID || 'unselected'}-${choice?.protocolVersionID || ''}-${Number(priority)}-${scope}`, impactAction: 'TOKEN_ROUTE_UPSERT', title: copy.configureStableRoute, lead: copy.stableRouteLead }} copy={copy} target={{ label: account.name, version: `${copy.actionAccountVersion}: ${account.account_version}` }} policy="AUDIT_ONLY" recentAuth impactLoader={impactLoader} precheckEnabled={Boolean(choice)} payloadDetails={{ model_id: choice?.modelID || '', protocol_version_id: choice?.protocolVersionID || '', priority: Number(priority), default_scope: scope, expected_account_version: account.account_version, expected_route_version: existing?.route_version || 0 }} onClose={onClose} execute={async (payload, key, signal) => {
    submittedExpected.current = { ...expected, expectedAccountVersion: payload.precheck.expected_versions.expected_account_version, expectedRouteVersion: payload.precheck.expected_versions.expected_route_version };
    const result = await client.createGovernedTokenCommand(await governedTokenEnvelope({ ...expected, reason: payload.reason, impact: payload.precheck, command }), key, signal);
    return requireGovernedRouteCommandResult(result, submittedExpected.current);
  }} recover={async (reference, signal) => requireGovernedRouteCommandResult(await client.governedTokenCommandResult(reference, signal), submittedExpected.current)} recoverByIdempotencyKey={async (key, signal) => requireGovernedRouteCommandResult(await client.governedTokenCommandByIdempotencyKey('TOKEN_ROUTE_UPSERT', key, signal), submittedExpected.current)} onCompleted={onSaved}>
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
  const submittedDeleteExpected = useRef(null);
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
  const deleteExpected = { commandType: 'TOKEN_ROUTE_DELETE', targetResourceType: 'TOKEN_ROUTE', targetResourceID: highRiskAction?.route?.route_id, tokenAccountID: accountID, modelID: highRiskAction?.route?.model_id, protocolVersionID: highRiskAction?.route?.protocol_version_id, expectedRouteVersion: highRiskAction?.route?.route_version, expectedAccountVersion: state.item?.account_version };
  const deleteCommand = { token_account_id: accountID, route_id: highRiskAction?.route?.route_id };
  return <section className="adm-card adm-source-card adm-token-health" aria-labelledby="token-health-title"><div className="adm-source-head"><div><button type="button" className="adm-back" onClick={onClose}>← {copy.backToAccounts}</button><h2 id="token-health-title">{state.item ? `${copy.routeHealth} · ${state.item.name}` : copy.routeHealth}</h2><p>{state.updatedAt ? `${copy.lastUpdated} ${new Date(state.updatedAt).toLocaleString()}` : copy.notUpdated}</p></div><div className="adm-row-actions">{access.manage && state.item?.status === 'ACTIVE' && <button type="button" className="danger" onClick={() => setHighRiskAction({ type: 'DISABLE' })}>{copy.disableTokenAccount}</button>}{access.manage && state.item?.status === 'DISABLED' && <button type="button" onClick={() => setHighRiskAction({ type: 'ENABLE' })}>{copy.enableTokenAccount}</button>}{access.showRouteWrite && state.item && <button type="button" onClick={() => setRouteDialog(true)}>{copy.configureStableRoute}</button>}<button type="button" disabled={state.loading} onClick={reload}>{state.loading ? copy.loading : copy.refreshHealth}</button></div></div>
    {access.explainRouteBlocked && <div className="adm-inline-message info" role="status">{copy.modelPermissionRequired}</div>}
    {state.error && <div className="adm-source-error" role="alert"><strong>{copy.healthUnavailable}</strong>{state.error.requestID && <span>{copy.requestID}: {state.error.requestID}</span>}<button type="button" onClick={reload}>{copy.retry}</button></div>}
    {state.loading && <div className="adm-source-state" role="status">{copy.loading}</div>}
    {state.item && (!state.item.routes?.length ? <div className="adm-source-state">{copy.noRoutes}</div> : <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.model}</th><th>{copy.protocol}</th><th>{copy.health}</th><th>{copy.healthEvidence}</th><th>{copy.routable}</th>{access.manage && <th>{copy.actions}</th>}</tr></thead><tbody>{state.item.routes.map((route) => { const health = serverRouteHealth(route, state.item.as_of); return <tr key={route.route_id}><td data-label={copy.model}><strong>{route.model_id ? copy.stableModel : copy.legacyUnresolved}</strong>{route.model_id && <small className="adm-secondary-id">{route.model_id}</small>}</td><td data-label={copy.protocol}>{route.protocol_code}{route.protocol_version_id && <small className="adm-secondary-id">{route.protocol_version_id}</small>}</td><td data-label={copy.health}><Status value={health.status} copy={copy} /></td><td data-label={copy.healthEvidence}><span>{health.observedAt ? new Date(health.observedAt).toLocaleString() : copy.notObserved}</span><small className="adm-secondary-id">{health.expiresAt ? `${copy.expiresAt} ${new Date(health.expiresAt).toLocaleString()}` : copy.noExpiry}{health.diagnostic ? ` · ${health.diagnostic}` : ''}</small></td><td data-label={copy.routable}>{health.routable ? copy.yes : copy.no}</td>{access.manage && <td data-label={copy.actions}>{route.model_id && route.protocol_version_id && <button type="button" className="danger" onClick={() => setHighRiskAction({ type: 'DELETE_ROUTE', route })}>{copy.deleteRoute}</button>}</td>}</tr>; })}</tbody></table></div>)}
    {showRouteDialog && state.item && <RouteDialog client={client} copy={copy} account={state.item} onClose={() => setRouteDialog(false)} onSaved={reload} />}
    {['DISABLE', 'ENABLE'].includes(highRiskAction?.type) && state.item && access.manage && <AdminHighRiskActionDialog action={{ key: `token-account-status-${statusTarget.toLowerCase()}`, impactAction: 'TOKEN_ACCOUNT_STATUS_CHANGE', title: statusTitle, lead: copy.disableTokenAccountLead }} copy={copy} target={{ label: state.item.name, version: `${copy.actionAccountVersion}: ${state.item.account_version}` }} policy="AUDIT_ONLY" recentAuth dangerous={statusTarget === 'DISABLED'} payloadDetails={{ token_account_id: accountID, expected_account_version: state.item.account_version, target_status: statusTarget }} impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_ACCOUNT_STATUS_CHANGE', target_resource_type: 'TOKEN_ACCOUNT', target_resource_id: accountID, command: { target_status: statusTarget } }, signal), { commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID })} onClose={() => setHighRiskAction(null)} execute={async (payload, key, signal) => {
      const result = await client.createGovernedTokenCommand(await governedTokenEnvelope({ commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID, reason: payload.reason, impact: payload.precheck, expectedVersions: { token_account_id: accountID, expected_account_version: state.item.account_version, target_status: statusTarget }, command: { target_status: statusTarget } }), key, signal);
      return requireGovernedTokenCommandResult(result, { commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID });
    }} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID })} onCompleted={reload} />}
    {highRiskAction?.type === 'DELETE_ROUTE' && state.item && access.manage && <AdminHighRiskActionDialog action={{ key: `route-delete-${highRiskAction.route.route_id}`, impactAction: 'TOKEN_ROUTE_DELETE', title: copy.deleteRoute, lead: copy.deleteRouteLead }} copy={copy} target={{ label: highRiskAction.route.model_id, version: `${copy.actionRouteVersion}: ${highRiskAction.route.route_version}` }} policy="AUDIT_ONLY" recentAuth dangerous payloadDetails={{ ...deleteCommand, expected_account_version: state.item.account_version, expected_route_version: highRiskAction.route.route_version }} impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_ROUTE_DELETE', target_resource_type: 'TOKEN_ROUTE', target_resource_id: highRiskAction.route.route_id, command: deleteCommand }, signal), deleteExpected)} onClose={() => setHighRiskAction(null)} execute={async (payload, key, signal) => {
      submittedDeleteExpected.current = { ...deleteExpected, expectedAccountVersion: payload.precheck.expected_versions.expected_account_version, expectedRouteVersion: payload.precheck.expected_versions.expected_route_version };
      const result = await client.createGovernedTokenCommand(await governedTokenEnvelope({ ...deleteExpected, reason: payload.reason, impact: payload.precheck, command: deleteCommand }), key, signal);
      return requireGovernedRouteCommandResult(result, submittedDeleteExpected.current);
    }} recover={async (reference, signal) => requireGovernedRouteCommandResult(await client.governedTokenCommandResult(reference, signal), submittedDeleteExpected.current)} recoverByIdempotencyKey={async (key, signal) => requireGovernedRouteCommandResult(await client.governedTokenCommandByIdempotencyKey('TOKEN_ROUTE_DELETE', key, signal), submittedDeleteExpected.current)} onCompleted={reload} />}
  </section>;
}

function AccountRecoveryQueue({ client, copy, routeVersion, onQueryChange, histories, enabled, unavailableReason }) {
  const query = useMemo(() => readAdminModelTokenQuery('accountRecovery'), [routeVersion]);
  const [refresh, setRefresh] = useState(0);
  const [selected, setSelected] = useState(null);
  const load = useMemo(() => (filters, signal) => client.tokenAccountRecoveryQueue(filters, signal), [client]);
  const state = useSource(load, query, enabled, refresh);
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
    {enabled && <Feedback state={state} copy={copy} onRetry={retry} />}
    {enabled && state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.commandReference}</th><th>{copy.status}</th><th>{copy.accountRecoveryEpoch}</th><th>{copy.lastUpdated}</th><th>{copy.actions}</th></tr></thead><tbody>{state.items.map((item) => <tr key={item.command_reference}>
      <td data-label={copy.commandReference}><span className="adm-secondary-id">{item.command_reference}</span></td><td data-label={copy.status}>{copy.accountRecoveryRequired}</td><td data-label={copy.accountRecoveryEpoch}>{item.recovery_epoch}</td><td data-label={copy.lastUpdated}>{new Date(item.updated_at).toLocaleString()}</td><td data-label={copy.actions}><button type="button" onClick={() => setSelected(item)}>{copy.accountRecoveryHandle}</button></td>
    </tr>)}</tbody></table></div>}
    {enabled && <Paging source="accountRecovery" query={query} state={state} copy={copy} histories={histories} onChange={(next) => onQueryChange('accountRecovery', next)} />}
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
  const readinessState = useSource(readinessLoad, {}, true, readinessRefresh);
  const readiness = readinessState.loading || readinessState.error ? null : readinessState.items[0] || null;
  const snapshotMode = readiness?.token_accounts_mode === 'SNAPSHOT';
  const accountsEnabled = Boolean(readiness) && readiness.token_accounts_mode !== 'BLOCKED';
  const load = useMemo(() => (filters, signal) => snapshotMode ? client.tokenAccounts(filters, signal) : client.tokenAccounts(signal), [client, snapshotMode]);
  const state = useSource(load, query, accountsEnabled, refresh);
  const retry = () => {
    if (state.error?.status === 410) { delete histories.current.tokenAccounts; onQueryChange('tokenAccounts', { ...query, cursor: '' }, 'replace'); }
    else setReadinessRefresh((value) => value + 1);
  };
  const closeOwner = () => onQueryChange('ownerCandidates', { q: '', owner_type: '', status: '', organization_id: '', cursor: '' }, 'push');
	if (selectedAccountID && !detailsEnabled) return <RouteHealth client={client} copy={copy} accountID={selectedAccountID} access={actionAccess} onClose={() => onRouteChange({ token_account_id: null, token_detail_tab: null }, 'push')} />;
  return <>
    {access.explainCreateBlocked && <div className="adm-inline-message info" role="status">{copy.ownerCandidatePermissionRequired}</div>}
    <section className="adm-card adm-source-card" aria-labelledby="token-accounts-title"><div className="adm-source-head"><div><h2 id="token-accounts-title">{copy.tokenAccounts}</h2><p>{state.updatedAt ? `${copy.lastUpdated} ${new Date(state.updatedAt).toLocaleString()}` : copy.notUpdated}</p></div><div className="adm-row-actions">{access.showCreate && <button type="button" onClick={() => onQueryChange('ownerCandidates', { q: '', owner_type: 'USER', status: 'ACTIVE', organization_id: '', cursor: '' }, 'push')}>{copy.createTokenAccount}</button>}<button type="button" disabled={state.loading || readinessState.loading} onClick={retry}>{state.loading || readinessState.loading ? copy.loading : copy.refreshSource}</button></div></div>
      {readinessState.error && <Feedback state={readinessState} copy={copy} onRetry={retry} />}
      {readinessState.loading && !readiness && <div className="adm-source-state" role="status">{copy.loading}</div>}
      {readiness?.token_accounts_mode === 'BLOCKED' && <div className="adm-inline-message error" role="alert">{copy.tokenResourceUnavailableReasons?.[readiness.recovery_unavailable_reason] || copy.sourceUnavailable}</div>}
      {readiness?.token_accounts_mode === 'LEGACY_BOUNDED' && <div className="adm-inline-message info" role="status">{copy.tokenAccountsLegacyMode}</div>}
      {snapshotMode && <form key={JSON.stringify(query)} className="adm-model-token-filters" onSubmit={(event) => { event.preventDefault(); const values = new globalThis.FormData(event.currentTarget); onQueryChange('tokenAccounts', { ...query, q: values.get('q'), owner_type: values.get('owner_type'), status: values.get('status'), health_status: values.get('health_status'), cursor: '' }); }}><label><span>{copy.tokenSearch}</span><input name="q" type="search" defaultValue={query.q} placeholder={copy.tokenSearchPlaceholder} /></label><label><span>{copy.ownerType}</span><select name="owner_type" defaultValue={query.owner_type}><option value="">{copy.all}</option><option value="USER">{copy.enumLabels.USER}</option><option value="ORG">{copy.enumLabels.ORG}</option><option value="ORG_UNIT">{copy.enumLabels.ORG_UNIT}</option></select></label><label><span>{copy.status}</span><select name="status" defaultValue={query.status}><option value="">{copy.all}</option>{['ACTIVE', 'DISABLED', 'FROZEN'].map((value) => <option key={value} value={value}>{copy.enumLabels[value]}</option>)}</select></label><label><span>{copy.health}</span><select name="health_status" defaultValue={query.health_status}><option value="">{copy.all}</option>{['UNKNOWN', 'PROBING', 'HEALTHY', 'DEGRADED', 'UNHEALTHY', 'STALE'].map((value) => <option key={value} value={value}>{copy.enumLabels[value]}</option>)}</select></label><button type="submit">{copy.search}</button><button type="button" onClick={() => onQueryChange('tokenAccounts', { q: '', owner_type: '', owner_id: '', status: '', health_status: '', credential_status: '', cursor: '' })}>{copy.clear}</button></form>}
      {accountsEnabled && <Feedback state={state} copy={copy} onRetry={retry} />}
			{accountsEnabled && state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.tokenAccount}</th><th>{copy.owner}</th><th>{copy.status}</th><th>{copy.credentials}</th><th>{copy.routes}</th><th>{copy.actions}</th></tr></thead><tbody>{state.items.map((item) => <tr key={item.token_account_id}><td data-label={copy.tokenAccount}><strong>{item.name}</strong><small className="adm-secondary-id">{item.token_account_id}</small></td><td data-label={copy.owner}>{copy.enumLabels[item.owner_type] || item.owner_type}<small className="adm-secondary-id">{item.owner_id}</small></td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.credentials}>{item.active_credential_count}</td><td data-label={copy.routes}>{item.route_count}</td><td data-label={copy.actions}><button type="button" onClick={() => onRouteChange({ token_account_id: item.token_account_id, token_detail_tab: detailsEnabled ? 'overview' : 'routes' }, 'push')}>{detailsEnabled ? copy.viewDetail : copy.viewRouteHealth}</button></td></tr>)}</tbody></table></div>}
      {snapshotMode && <Paging source="tokenAccounts" query={query} state={state} copy={copy} histories={histories} onChange={(next) => onQueryChange('tokenAccounts', next)} />}
    </section>
    {actionsEnabled && access.manage && access.ownerCandidates && readiness && <AccountRecoveryQueue client={client} copy={copy} routeVersion={routeVersion} onQueryChange={onQueryChange} histories={histories} enabled={readiness.recovery_queue_available} unavailableReason={readiness.recovery_unavailable_reason} />}
    {access.requestOwnerCandidates && ownerQuery.owner_type && <OwnerDialog client={client} copy={copy} query={ownerQuery} histories={histories} onQueryChange={onQueryChange} onClose={closeOwner} onCreated={async () => setRefresh((value) => value + 1)} />}
  </>;
}
