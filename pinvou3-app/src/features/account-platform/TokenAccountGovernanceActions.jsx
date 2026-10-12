import { TokenResourceFeedback, TokenResourcePaging } from './TokenResourcePrimitives.jsx';
import React, { useEffect, useMemo, useRef, useState } from 'react';
import { stableRouteChoices, routeEditorVisible } from './admin-token-resource-state.js';
import { AdminHighRiskActionDialog } from './AdminHighRiskActionDialog.jsx';
import { governedTokenEnvelope, requireGovernedRouteCommandResult, requireGovernedTokenCommandResult, requireGovernedTokenPrecheck } from './admin-high-risk-action-state.js';

import { useAdminSource } from './AdminModelTokenPrimitives.jsx';

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
  const state = useAdminSource({ load, query, enabled: true, refreshVersion: refresh });
  const choices = stableRouteChoices(state.items);
  const modelOptions = [...new Map(choices.map((item) => [item.modelID, item])).values()];
  const protocolOptions = choices.filter((item) => item.modelID === selectedModelID);
  const choice = protocolOptions.find((item) => item.protocolVersionID === selectedProtocolVersionID);
  const [routeLookup, setRouteLookup] = useState({ key: '', ready: false, route: null, error: null });
  const [routeLookupRefresh, setRouteLookupRefresh] = useState(0);
  const choiceModelID = choice?.modelID;
  const choiceProtocolID = choice?.protocolVersionID;
  const selectionKey = choice ? `${account.token_account_id}/${choiceModelID}/${choiceProtocolID}` : '';
  const lookupReady = Boolean(selectionKey && routeLookup.key === selectionKey && routeLookup.ready);
  const existing = lookupReady ? routeLookup.route : null;
  useEffect(() => {
    const controller = new globalThis.AbortController();
    setRouteLookup({ key: selectionKey, ready: false, route: null, error: null });
    setPriority(100); setScope('NONE');
    if (!choiceModelID || !choiceProtocolID) return () => controller.abort();
    const timeout = globalThis.setTimeout(() => {
      controller.abort();
      setRouteLookup({ key: selectionKey, ready: false, route: null, error: { code: 'QUERY_TIMEOUT' } });
    }, 15000);
    // The detail DTO and current filtered page cannot prove route absence.
    // Walk the independent unfiltered snapshot; never treat a failed/partial
    // read as a new route. Limit lookup work and reject cyclic cursors.
    void (async () => {
      const cursors = new Set(); let cursor = ''; let asOf = null;
      for (let pageNumber = 0; pageNumber < 1000; pageNumber++) {
        if (controller.signal.aborted) return;
        const page = await client.tokenAccountRoutes(account.token_account_id, { cursor }, controller.signal);
        if (controller.signal.aborted) return;
        if (!Array.isArray(page?.items) || typeof page.has_more !== 'boolean' || typeof page.as_of !== 'string' || !Number.isFinite(Date.parse(page.as_of)) || (asOf !== null && page.as_of !== asOf)) throw Object.assign(new Error('invalid route snapshot'), { code: 'INVALID_REQUEST' });
        asOf = page.as_of;
        if (page.items.some((item) => item?.token_account_id !== account.token_account_id)) throw Object.assign(new Error('foreign route snapshot'), { code: 'INVALID_REQUEST' });
        const matches = page.items.filter((item) => item.model_id === choiceModelID && item.protocol_version_id === choiceProtocolID);
        if (matches.length > 1) throw Object.assign(new Error('ambiguous route'), { code: 'INVALID_REQUEST' });
        const route = matches[0] || null;
        if (route && (!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(route.route_id) || !Number.isInteger(route.priority) || route.priority < 0 || route.priority > 10000 || !['NONE', 'UNIT', 'ORG'].includes(route.default_scope) || !Number.isSafeInteger(route.route_version) || route.route_version < 1)) throw Object.assign(new Error('invalid route facts'), { code: 'INVALID_REQUEST' });
        if (route || !page.has_more) {
          setPriority(route?.priority ?? 100); setScope(route?.default_scope ?? 'NONE');
          setRouteLookup({ key: selectionKey, ready: true, route, error: null }); return;
        }
        if (typeof page.next_cursor !== 'string' || !page.next_cursor || cursors.has(page.next_cursor)) throw Object.assign(new Error('invalid route cursor'), { code: 'INVALID_REQUEST' });
        cursor = page.next_cursor; cursors.add(cursor);
      }
      throw Object.assign(new Error('route lookup budget exceeded'), { code: 'QUERY_TIMEOUT' });
    })().catch((error) => { if (!controller.signal.aborted) setRouteLookup({ key: selectionKey, ready: false, route: null, error }); }).finally(() => globalThis.clearTimeout(timeout));
    return () => { globalThis.clearTimeout(timeout); controller.abort(); };
  }, [client, account.token_account_id, selectionKey, choiceModelID, choiceProtocolID, routeLookupRefresh]);
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
  return <AdminHighRiskActionDialog action={{ key: `route-upsert-${choice?.modelID || 'unselected'}-${choice?.protocolVersionID || ''}-${Number(priority)}-${scope}`, impactAction: 'TOKEN_ROUTE_UPSERT', title: copy.configureStableRoute, lead: copy.stableRouteLead }} copy={copy} target={{ label: account.name, version: `${copy.actionAccountVersion}: ${account.account_version}` }} policy="AUDIT_ONLY" recentAuth impactLoader={impactLoader} precheckEnabled={lookupReady} payloadDetails={{ model_id: choice?.modelID || '', protocol_version_id: choice?.protocolVersionID || '', priority: Number(priority), default_scope: scope, expected_account_version: account.account_version, expected_route_version: existing?.route_version || 0 }} onClose={onClose} execute={async (payload, key, signal) => {
    submittedExpected.current = { ...expected, expectedAccountVersion: payload.precheck.expected_versions.expected_account_version, expectedRouteVersion: payload.precheck.expected_versions.expected_route_version };
    const result = await client.createGovernedTokenCommand(await governedTokenEnvelope({ ...expected, reason: payload.reason, impact: payload.precheck, command }), key, signal);
    return requireGovernedRouteCommandResult(result, submittedExpected.current);
  }} recover={async (reference, signal) => requireGovernedRouteCommandResult(await client.governedTokenCommandResult(reference, signal), submittedExpected.current)} recoverByIdempotencyKey={async (key, signal) => requireGovernedRouteCommandResult(await client.governedTokenCommandByIdempotencyKey('TOKEN_ROUTE_UPSERT', key, signal), submittedExpected.current)} onCompleted={onSaved}>
    <legend>{copy.routeConfiguration}</legend>
    {choice && <TokenResourceFeedback state={{ loading: !lookupReady && !routeLookup.error, items: lookupReady ? [existing || {}] : [], error: routeLookup.key === selectionKey ? routeLookup.error : null }} copy={copy} onRetry={() => setRouteLookupRefresh((v) => v + 1)} />}
    <div className="adm-token-candidate-search"><label><span>{copy.findModel}</span><input type="search" value={searchDraft} placeholder={copy.modelSearchPlaceholder} onChange={(event) => setSearchDraft(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); setQuery((current) => ({ ...current, q: searchDraft, cursor: '' })); } }} /></label><button type="button" onClick={() => setQuery((current) => ({ ...current, q: searchDraft, cursor: '' }))}>{copy.search}</button></div>
    <TokenResourceFeedback state={state} copy={copy} onRetry={retry} />
    {choices.length > 0 && <div className="adm-token-dialog-grid"><label>{copy.model}<select required value={selectedModelID} onChange={(event) => { setSelectedModelID(event.target.value); setSelectedProtocolVersionID(''); }}><option value="">{copy.chooseModel}</option>{modelOptions.map((item) => <option key={item.modelID} value={item.modelID}>{item.displayName}</option>)}</select></label><label>{copy.protocol}<select required disabled={!selectedModelID} value={selectedProtocolVersionID} onChange={(event) => setSelectedProtocolVersionID(event.target.value)}><option value="">{copy.chooseProtocol}</option>{protocolOptions.map((item) => <option key={item.protocolVersionID} value={item.protocolVersionID}>{item.protocolCode} · v{item.protocolVersion}</option>)}</select></label></div>}
    <TokenResourcePaging source="routeModels" query={query} state={state} copy={copy} histories={histories} onChange={setQuery} />
    <div className="adm-token-dialog-grid"><label>{copy.priority}<input disabled={!lookupReady} required type="number" min="0" max="10000" value={priority} onChange={(event) => setPriority(event.target.value)} /></label><label>{copy.defaultScope}<select disabled={!lookupReady} value={scope} onChange={(event) => setScope(event.target.value)}><option value="NONE">{copy.enumLabels.NONE}</option><option value="UNIT">{copy.enumLabels.UNIT}</option><option value="ORG">{copy.enumLabels.ORG}</option></select></label></div>
    <p className="adm-safe-preview-note">{copy.routeServerOwned}</p>
  </AdminHighRiskActionDialog>;
}

export function TokenAccountGovernanceActions({ client, copy, account, access, onSaved, children }) {
  const accountID = account.token_account_id;
  const submittedDeleteExpected = useRef(null);
  const [routeDialog, setRouteDialog] = useState(false);
  const [highRiskAction, setHighRiskAction] = useState(null);
  const showRouteDialog = routeEditorVisible(access, routeDialog);
  useEffect(() => { if (!access.manage) setHighRiskAction(null); if (!access.showRouteWrite) setRouteDialog(false); }, [access.manage, access.showRouteWrite]);
  const reload = async () => { setRouteDialog(false); setHighRiskAction(null); await onSaved(); };
  const statusTarget = highRiskAction?.type === 'ENABLE' ? 'ACTIVE' : 'DISABLED';
  const statusTitle = statusTarget === 'ACTIVE' ? copy.enableTokenAccount : copy.disableTokenAccount;
  const deleteExpected = { commandType: 'TOKEN_ROUTE_DELETE', targetResourceType: 'TOKEN_ROUTE', targetResourceID: highRiskAction?.route?.route_id, tokenAccountID: accountID, modelID: highRiskAction?.route?.model_id, protocolVersionID: highRiskAction?.route?.protocol_version_id, expectedRouteVersion: highRiskAction?.route?.route_version, expectedAccountVersion: account?.account_version };
  const deleteCommand = { token_account_id: accountID, route_id: highRiskAction?.route?.route_id };
  return <>
    <div className="adm-row-actions">
      {access.manage && account.status === 'ACTIVE' && <button type="button" className="danger" onClick={() => setHighRiskAction({ type: 'DISABLE' })}>{copy.disableTokenAccount}</button>}
      {access.manage && account.status === 'DISABLED' && <button type="button" onClick={() => setHighRiskAction({ type: 'ENABLE' })}>{copy.enableTokenAccount}</button>}
      {access.showRouteWrite && <button type="button" onClick={() => setRouteDialog(true)}>{copy.configureStableRoute}</button>}
    </div>
    {access.explainRouteBlocked && <div className="adm-inline-message info" role="status">{copy.modelPermissionRequired}</div>}
    {children?.((route) => { if (access.manage) setHighRiskAction({ type: 'DELETE_ROUTE', route }); })}
    {showRouteDialog && account && <RouteDialog client={client} copy={copy} account={account} onClose={() => setRouteDialog(false)} onSaved={reload} />}
    {['DISABLE', 'ENABLE'].includes(highRiskAction?.type) && account && access.manage && <AdminHighRiskActionDialog action={{ key: `token-account-status-${statusTarget.toLowerCase()}`, impactAction: 'TOKEN_ACCOUNT_STATUS_CHANGE', title: statusTitle, lead: copy.disableTokenAccountLead }} copy={copy} target={{ label: account.name, version: `${copy.actionAccountVersion}: ${account.account_version}` }} policy="AUDIT_ONLY" recentAuth dangerous={statusTarget === 'DISABLED'} payloadDetails={{ token_account_id: accountID, expected_account_version: account.account_version, target_status: statusTarget }} impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_ACCOUNT_STATUS_CHANGE', target_resource_type: 'TOKEN_ACCOUNT', target_resource_id: accountID, command: { target_status: statusTarget } }, signal), { commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID })} onClose={() => setHighRiskAction(null)} execute={async (payload, key, signal) => {
      const result = await client.createGovernedTokenCommand(await governedTokenEnvelope({ commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID, reason: payload.reason, impact: payload.precheck, expectedVersions: { token_account_id: accountID, expected_account_version: account.account_version, target_status: statusTarget }, command: { target_status: statusTarget } }), key, signal);
      return requireGovernedTokenCommandResult(result, { commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID });
    }} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID })} recoverByIdempotencyKey={async (key, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandByIdempotencyKey('TOKEN_ACCOUNT_STATUS_CHANGE', key, signal), { commandType: 'TOKEN_ACCOUNT_STATUS_CHANGE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: accountID })} onCompleted={reload} />}
    {highRiskAction?.type === 'DELETE_ROUTE' && account && access.manage && <AdminHighRiskActionDialog action={{ key: `route-delete-${highRiskAction.route.route_id}`, impactAction: 'TOKEN_ROUTE_DELETE', title: copy.deleteRoute, lead: copy.deleteRouteLead }} copy={copy} target={{ label: highRiskAction.route.model_id, version: `${copy.actionRouteVersion}: ${highRiskAction.route.route_version}` }} policy="AUDIT_ONLY" recentAuth dangerous payloadDetails={{ ...deleteCommand, expected_account_version: account.account_version, expected_route_version: highRiskAction.route.route_version }} impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_ROUTE_DELETE', target_resource_type: 'TOKEN_ROUTE', target_resource_id: highRiskAction.route.route_id, command: deleteCommand }, signal), deleteExpected)} onClose={() => setHighRiskAction(null)} execute={async (payload, key, signal) => {
      submittedDeleteExpected.current = { ...deleteExpected, expectedAccountVersion: payload.precheck.expected_versions.expected_account_version, expectedRouteVersion: payload.precheck.expected_versions.expected_route_version };
      const result = await client.createGovernedTokenCommand(await governedTokenEnvelope({ ...deleteExpected, reason: payload.reason, impact: payload.precheck, command: deleteCommand }), key, signal);
      return requireGovernedRouteCommandResult(result, submittedDeleteExpected.current);
    }} recover={async (reference, signal) => requireGovernedRouteCommandResult(await client.governedTokenCommandResult(reference, signal), submittedDeleteExpected.current)} recoverByIdempotencyKey={async (key, signal) => requireGovernedRouteCommandResult(await client.governedTokenCommandByIdempotencyKey('TOKEN_ROUTE_DELETE', key, signal), submittedDeleteExpected.current)} onCompleted={reload} />}
  </>;
}
