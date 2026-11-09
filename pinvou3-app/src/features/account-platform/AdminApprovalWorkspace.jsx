import React, { useEffect, useMemo, useRef, useState } from 'react';
import { financeCopy } from '../../shared/i18n.js';
import { approvalReadCopy, pendingApprovalReadCopy, executionApprovalReadCopy, approvalDirectoryCopy, approvalDirectoryToolbarCopy } from '../../shared/admin-approval-read-i18n.js';
import { Approvals } from './AdminFinanceWorkspace.jsx';
import { approvalCommands, approvalViews, approvalStatuses, executionStatuses, approvalQuery, approvalExpiryDate, readApprovalRoute } from './admin-approval-actions.js';
import { approvalLabel } from './admin-approval-presentation.js';
import './admin-approval-workspace.css';

// These existing Billing tabs accept an unbound visit with repairs.request.
// A directory entry has no resource ID or authorization for a specific request.
const directoryRepairTabs = new Map([
  ['LEDGER_REPAIR', 'ledger'], ['ENTITLEMENT_COMPONENT_ADJUSTMENT', 'correction'], ['RECEIVABLE_WRITE_OFF', 'writeoff'],
  ['PAYMENT_FINANCIAL_REPAIR', 'paymentrepair'], ['REFUND_EXCEPTION_REPAIR', 'refundrepair'], ['CHARGEBACK_FINANCIAL_REPAIR', 'chargebackrepair'],
  ['RECONCILIATION_FINANCIAL_REPAIR', 'reconciliationrepair'], ['USAGE_SETTLEMENT_REPAIR', 'usagesettlementrepair'],
]);

const billingCommands = new Set(['WALLET_ADJUSTMENT', 'PAYMENT_PROVIDER_RESUME', 'PAYMENT_PRODUCT_RESUME', ...directoryRepairTabs.keys()]);
const tokenCommands = new Set(['TOKEN_MODEL_LIFECYCLE_CHANGE', 'TOKEN_MODEL_CATALOG_ROLLBACK', 'TOKEN_CREDENTIAL_REVOKE']);
const directoryRisks = ['FINANCIAL_FACT_CHANGE', 'GOVERNANCE_OR_NORMAL_STATE_MACHINE'];
const emptyDirectoryFilters = { query: '', domain: '', risk: '' };

export function AdminApprovalWorkspace({ client, copy, language, me, onNavigate, routeSearch, onRouteChange }) {
  const c = copy.approvalCenter;
  const [route, setRoute] = useState(() => readApprovalRoute(globalThis.location?.search));
  const [command, setCommand] = useState(route.commandType);
  const [target, setTarget] = useState(route.target);
  const [mineStatus, setMineStatus] = useState(route.status);
  const [expiryDraft, setExpiryDraft] = useState({ from: route.expiresFrom || '', to: route.expiresTo || '' });
  const [invalidExpiry, setInvalidExpiry] = useState(false);
  const expiryHelpID = React.useId();
  const [mineApplyGeneration, setMineApplyGeneration] = useState(0);
  const mine = route.view === 'mine', mineWords = approvalReadCopy[language], mineTargetID = React.useId();
  const [blocked, setBlocked] = useState(false);
  const [directory, setDirectory] = useState(null);
  const [directoryState, setDirectoryState] = useState(null);
  const [directoryFilters, setDirectoryFilters] = useState(emptyDirectoryFilters);
  const [refresh, setRefresh] = useState(0);
  const owner = `${me.user_id}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`;
  const localRoute = useRef(null);
  const directoryEpoch = client.financeSessionEpoch || 0;
  const directoryScope = useMemo(() => ({ client, owner, view: route.view, epoch: directoryEpoch }), [client, owner, route.view, directoryEpoch]);
  const directoryWords = approvalDirectoryCopy[language];
  const currentDirectory = directory?.scope === directoryScope && directory.refresh === refresh ? directory.value : null;
  const currentDirectoryState = directoryState?.scope === directoryScope && directoryState.refresh === refresh ? directoryState.phase : 'loading';
  const directoryTools = approvalDirectoryToolbarCopy[language];
  const queryTerm = directoryFilters.query.trim().toLocaleLowerCase(language);
  const visiblePolicies = currentDirectory?.items.filter(item => {
    const label = approvalLabel(c.policyCommandLabels, item.command_type).toLocaleLowerCase(language);
    const domain = billingCommands.has(item.command_type) ? 'billing' : tokenCommands.has(item.command_type) ? 'token' : '';
    const risk = directoryRisks.includes(item.risk_class) ? item.risk_class : 'other';
    return (!queryTerm || label.includes(queryTerm) || item.command_type.toLocaleLowerCase(language).includes(queryTerm)) &&
      (!directoryFilters.domain || directoryFilters.domain === domain) && (!directoryFilters.risk || directoryFilters.risk === risk);
  });
  const canVisitRepairs = (me.platform_permissions || []).includes('platform.billing.repairs.request') && !!onNavigate;
  const externalRoute = useRef({ search: routeSearch, applied: true });
  useEffect(() => {
    if (routeSearch === undefined) return;
    if (externalRoute.current.search !== routeSearch) externalRoute.current = { search: routeSearch, applied: false };
    if (!blocked && !externalRoute.current.applied) {
      externalRoute.current.applied = true;
      const next = readApprovalRoute(routeSearch);
      const echo = localRoute.current && ['view', 'commandType', 'target', 'status', 'requestID', 'expiresFrom', 'expiresTo'].every(key => next[key] === localRoute.current[key]);
      localRoute.current = null;
      setRoute(next);
      // Parent URL acknowledgement must not erase filters the user has typed
      // but not submitted; an actual external route restores all its fields.
      if (!echo) { setCommand(next.commandType); setTarget(next.target); setMineStatus(next.status); setExpiryDraft({ from: next.expiresFrom || '', to: next.expiresTo || '' }); setInvalidExpiry(false); }
    }
  }, [routeSearch, blocked]);
  const pending = route.view === 'pending', pendingWords = pendingApprovalReadCopy[language];
  const listWords = mine ? mineWords : pendingWords;
  const query = useMemo(() => approvalQuery(route, me.user_id), [route, me.user_id]);
  const changeRoute = patch => {
    if (blocked) return;
    const next = { ...route, ...patch };
    localRoute.current = next;
    const params = new URLSearchParams(globalThis.location?.search || '');
    for (const [key, value] of Object.entries({ approval_view: next.view, approval_command_type: next.commandType,
      approval_target: next.target, approval_status: next.status, approval_request_id: next.requestID, approval_expires_from: next.expiresFrom, approval_expires_to: next.expiresTo })) {
      if (value) params.set(key, value); else params.delete(key);
    }
    params.delete('approval_cursor');
    if (onRouteChange) onRouteChange({ approval_view: next.view || null, approval_command_type: next.commandType || null, approval_target: next.target || null, approval_status: next.status || null, approval_request_id: next.requestID || null, approval_expires_from: next.expiresFrom || null, approval_expires_to: next.expiresTo || null, approval_cursor: null });
    else globalThis.history?.replaceState(null, '', `${globalThis.location?.pathname || ''}?${params}${globalThis.location?.hash || ''}`);
    setRoute(next);
  };
  const applyPendingDeadline = (clear = false) => {
    if (blocked) return;
    const { from, to } = clear ? { from: '', to: '' } : expiryDraft;
    if ((from && !approvalExpiryDate(from)) || (to && !approvalExpiryDate(to)) || (from && to && from >= to)) { setInvalidExpiry(true); return; }
    setInvalidExpiry(false);
    const patch = { commandType: clear ? '' : command, target: clear ? '' : target.trim(), expiresFrom: from, expiresTo: to };
    if (clear) { setCommand(''); setTarget(''); setExpiryDraft({ from: '', to: '' }); }
    if (!route.requestID && Object.keys(patch).every(key => patch[key] === route[key])) setMineApplyGeneration(value => value + 1);
    else changeRoute({ ...patch, requestID: '' });
  };
  const applyMine = (clear = false) => {
    if (blocked) return;
    const patch = clear ? { commandType: '', target: '', status: '' } : { commandType: command, target: target.trim(), status: mineStatus };
    if (clear) { setCommand(''); setTarget(''); setMineStatus(''); }
    if (!route.requestID && ['commandType', 'target', 'status'].every(key => route[key] === patch[key])) setMineApplyGeneration(value => value + 1);
    else changeRoute({ ...patch, requestID: '' });
  };
  useEffect(() => {
    if (route.view !== 'directory') return undefined;
    const controller = new AbortController();
    const setPhase = phase => setDirectoryState({ scope: directoryScope, refresh, phase });
    setDirectory(null); setPhase('loading');
    void client.governedApprovalDirectory(controller.signal).then(value => {
      if (controller.signal.aborted) return;
      if (!Array.isArray(value?.items)) throw new Error('INVALID_APPROVAL_DIRECTORY');
      setDirectory({ scope: directoryScope, refresh, value: { ...value, items: value.items.filter(item => item?.visible === true && approvalCommands.includes(item.command_type)) } });
      setPhase('ready');
    }).catch(error => { if (!controller.signal.aborted) setPhase(error?.status === 403 ? 'denied' : 'error'); });
    return () => controller.abort();
  }, [client, directoryScope, route.view, refresh]);
  useEffect(() => { setDirectoryFilters(emptyDirectoryFilters); }, [directoryScope]);
  return <section className="adm-approval-workspace" aria-label={copy.nav.approvals}>
    <nav className="adm-tabs adm-approval-tabs" aria-label={c.views}>{approvalViews.map(view => <button key={view} className={route.view === view ? 'active' : undefined} disabled={blocked} aria-current={route.view === view ? 'page' : undefined} onClick={() => { if (view !== route.view) { setMineStatus(''); setExpiryDraft({ from: '', to: '' }); setInvalidExpiry(false); } changeRoute({ view, status: '', requestID: '', ...(view !== route.view ? { expiresFrom: '', expiresTo: '' } : {}) }); }}>{c[view]}</button>)}</nav>
    {route.view !== 'directory' && <form className={'adm-approval-toolbar' + (pending ? ' adm-approval-pending-toolbar' : '')} onSubmit={event => { event.preventDefault(); if (mine) applyMine(); else if (route.view === 'pending') applyPendingDeadline(); else changeRoute({ commandType: command, target: target.trim(), requestID: '' }); }}>
      <label>{c.commandType}<select disabled={blocked} value={command} onChange={event => setCommand(event.target.value)}><option value="">{c.allCommands}</option>{approvalCommands.map(type => <option key={type} value={type}>{mine || pending ? approvalLabel(c.policyCommandLabels, type) : type}</option>)}</select></label>
      <label>{mine || pending ? <span id={mineTargetID + '-label'}>{listWords.targetID}</span> : c.target}<input disabled={blocked} value={target} aria-labelledby={mine || pending ? mineTargetID + '-label' : undefined} aria-describedby={mine || pending ? mineTargetID + '-help' : undefined} onChange={event => setTarget(event.target.value)} />{(mine || pending) && <small className="adm-approval-target-help" id={mineTargetID + '-help'}>{listWords.targetHelp}</small>}</label>
      {route.view === 'pending' && <>
        <label>{pendingWords.expiryFrom}<input type="date" value={expiryDraft.from} disabled={blocked} aria-invalid={invalidExpiry || undefined} aria-describedby={expiryHelpID} onChange={event => { setInvalidExpiry(false); setExpiryDraft({ ...expiryDraft, from: event.target.value }); }} /></label>
        <label>{pendingWords.expiryTo}<input type="date" value={expiryDraft.to} disabled={blocked} aria-invalid={invalidExpiry || undefined} aria-describedby={expiryHelpID} onChange={event => { setInvalidExpiry(false); setExpiryDraft({ ...expiryDraft, to: event.target.value }); }} /></label>
        <div className="adm-approval-expiry-help" id={expiryHelpID}>{pendingWords.expiryHelp}{invalidExpiry && <p role="alert">{pendingWords.invalidExpiry}</p>}</div>
      </>}
      {route.view !== 'pending' && <label>{c.status}<select disabled={blocked} value={route.view === 'execution' ? query.status : mineStatus} onChange={event => mine ? setMineStatus(event.target.value) : changeRoute({ status: event.target.value, requestID: '' })}>
        {route.view === 'mine' && <option value="">{c.allStatuses}</option>}{(route.view === 'execution' ? executionStatuses : approvalStatuses).map(status => <option key={status} value={status}>{approvalLabel(c.statusLabels, status)}</option>)}
      </select></label>}{mine || pending ? <div className="adm-approval-query-actions"><button type="submit" className="adm-approval-query" disabled={blocked}>{listWords.query}</button><button type="button" disabled={blocked} onClick={() => mine ? applyMine(true) : applyPendingDeadline(true)}>{listWords.reset}</button></div> : <button disabled={blocked}>{copy.refresh}</button>}
    </form>}
    {route.view === 'execution' && <p className="adm-approval-execution-scope">{c.executionScopeLabel}: <strong>{approvalLabel(c.statusLabels, query.status)}</strong><small>{c.executionDefaultHelp}</small></p>}
    {route.view === 'directory' ? <>
      <div className="adm-approval-toolbar adm-approval-directory-toolbar" role="group" aria-label={directoryTools.filters}>
        <label>{directoryTools.search}<input type="search" value={directoryFilters.query} onChange={event => setDirectoryFilters(value => ({ ...value, query: event.target.value }))} /></label>
        <label>{directoryTools.domain}<select value={directoryFilters.domain} onChange={event => setDirectoryFilters(value => ({ ...value, domain: event.target.value }))}>
          <option value="">{directoryTools.all}</option><option value="billing">{directoryTools.billing}</option><option value="token">{directoryTools.token}</option>
        </select></label>
        <label>{c.risk}<select value={directoryFilters.risk} onChange={event => setDirectoryFilters(value => ({ ...value, risk: event.target.value }))}>
          <option value="">{directoryTools.all}</option>{directoryRisks.map(risk => <option key={risk} value={risk}>{approvalLabel(c.policyRiskLabels, risk)}</option>)}<option value="other">{directoryTools.otherRisk}</option>
        </select></label>
        <div className="adm-approval-query-actions"><button type="button" onClick={() => setDirectoryFilters(emptyDirectoryFilters)}>{directoryTools.clear}</button>
          <button type="button" disabled={currentDirectoryState === 'loading'} onClick={() => setRefresh(value => value + 1)}>{currentDirectoryState === 'error' ? directoryTools.retry : c.refresh}</button></div>
      </div>
      {currentDirectoryState === 'loading' && <p role="status" className="adm-approval-directory-state">{directoryTools.loading}</p>}
      {['error','denied'].includes(currentDirectoryState) && <p role="alert" className="adm-approval-directory-state">{currentDirectoryState === 'denied' ? directoryTools.denied : directoryTools.error}</p>}
      {currentDirectory && !visiblePolicies.length && <p role="status" className="adm-approval-directory-state">{currentDirectory.items.length ? directoryTools.filteredEmpty : directoryTools.empty}</p>}
      {!!visiblePolicies?.length && <div className="adm-table-wrap adm-approval-policy-wrap"><table className="adm-approval-policy-table"><thead><tr><th>{c.commandType}</th><th>{c.risk}</th><th>{c.requestable}</th></tr></thead><tbody>{visiblePolicies.map(item => {
        const commandLabel = approvalLabel(c.policyCommandLabels, item.command_type), riskLabel = approvalLabel(c.policyRiskLabels, item.risk_class);
        const requestable = item.requestable === true, unavailable = item.requestable === false, tab = requestable && canVisitRepairs ? directoryRepairTabs.get(item.command_type) : undefined;
        return <tr key={`${item.command_type}:${item.policy_version}`}>
          <td><strong>{commandLabel}</strong>{commandLabel !== item.command_type && <code>{item.command_type}</code>}<small>{financeCopy[language].version}: {item.policy_version}</small></td>
          <td><span className="adm-approval-policy-risk">{riskLabel}</span>{riskLabel !== item.risk_class && <code>{item.risk_class}</code>}</td>
          <td className="adm-approval-requestability"><span className={'adm-status' + (requestable ? ' ok' : '')}>{requestable ? directoryWords.requestable : unavailable ? directoryWords.unavailable : directoryWords.unknown}</span>
            <small>{requestable ? directoryWords.eligibleHelp : unavailable ? directoryWords.unavailableHelp : directoryWords.unknownHelp}</small>
            {tab && <button type="button" onClick={() => onNavigate({ section: 'finance', tab })}>{c.openBusinessArea}</button>}
          </td>
        </tr>;
      })}</tbody></table></div>}
    </> : <Approvals client={client} copy={financeCopy[language]} permissions={me.platform_permissions || []} owner={owner} requestID={route.requestID}
      query={query} pendingUserID={route.view === 'pending' ? me.user_id : null} onStateChange={setBlocked} onNavigate={onNavigate} centerCopy={c} mineReadCopy={mine ? mineWords : undefined} pendingReadCopy={pending ? pendingWords : undefined} executionReadCopy={route.view === 'execution' ? executionApprovalReadCopy[language] : undefined} mineApplyGeneration={mine || pending ? mineApplyGeneration : undefined} />}
  </section>;
}
