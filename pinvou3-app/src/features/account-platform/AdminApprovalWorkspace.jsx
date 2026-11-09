import React, { useEffect, useMemo, useRef, useState } from 'react';
import { financeCopy } from '../../shared/i18n.js';
import { approvalReadCopy, pendingApprovalReadCopy } from '../../shared/admin-approval-read-i18n.js';
import { Approvals } from './AdminFinanceWorkspace.jsx';
import { approvalCommands, approvalViews, approvalStatuses, executionStatuses, approvalQuery, readApprovalRoute } from './admin-approval-actions.js';
import { approvalLabel } from './admin-approval-presentation.js';
import './admin-approval-workspace.css';

export function AdminApprovalWorkspace({ client, copy, language, me, onNavigate, routeSearch, onRouteChange }) {
  const c = copy.approvalCenter;
  const [route, setRoute] = useState(() => readApprovalRoute(globalThis.location?.search));
  const [command, setCommand] = useState(route.commandType);
  const [target, setTarget] = useState(route.target);
  const [mineStatus, setMineStatus] = useState(route.status);
  const [mineApplyGeneration, setMineApplyGeneration] = useState(0);
  const mine = route.view === 'mine', mineWords = approvalReadCopy[language], mineTargetID = React.useId();
  const [blocked, setBlocked] = useState(false);
  const [directory, setDirectory] = useState(null);
  const [directoryState, setDirectoryState] = useState('loading');
  const [refresh, setRefresh] = useState(0);
  const owner = `${me.user_id}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`;
  const localRoute = useRef(null);
  const externalRoute = useRef({ search: routeSearch, applied: true });
  useEffect(() => {
    if (routeSearch === undefined) return;
    if (externalRoute.current.search !== routeSearch) externalRoute.current = { search: routeSearch, applied: false };
    if (!blocked && !externalRoute.current.applied) {
      externalRoute.current.applied = true;
      const next = readApprovalRoute(routeSearch);
      const echo = localRoute.current && ['view', 'commandType', 'target', 'status', 'requestID'].every(key => next[key] === localRoute.current[key]);
      localRoute.current = null;
      setRoute(next);
      // Parent URL acknowledgement must not erase filters the user has typed
      // but not submitted; an actual external route restores all its fields.
      if (!echo) { setCommand(next.commandType); setTarget(next.target); setMineStatus(next.status); }
    }
  }, [routeSearch, blocked]);
  const query = useMemo(() => approvalQuery(route, me.user_id), [route, me.user_id]);
  const changeRoute = patch => {
    if (blocked) return;
    const next = { ...route, ...patch };
    localRoute.current = next;
    const params = new URLSearchParams(globalThis.location?.search || '');
    for (const [key, value] of Object.entries({ approval_view: next.view, approval_command_type: next.commandType,
      approval_target: next.target, approval_status: next.status, approval_request_id: next.requestID })) {
      if (value) params.set(key, value); else params.delete(key);
    }
    params.delete('approval_cursor');
    if (onRouteChange) onRouteChange({ approval_view: next.view || null, approval_command_type: next.commandType || null, approval_target: next.target || null, approval_status: next.status || null, approval_request_id: next.requestID || null, approval_cursor: null });
    else globalThis.history?.replaceState(null, '', `${globalThis.location?.pathname || ''}?${params}${globalThis.location?.hash || ''}`);
    setRoute(next);
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
    setDirectory(null); setDirectoryState('loading');
    void client.governedApprovalDirectory(controller.signal).then(value => {
      if (controller.signal.aborted) return;
      if (!Array.isArray(value?.items)) throw new Error('INVALID_APPROVAL_DIRECTORY');
      setDirectory({ ...value, items: value.items.filter(item => item && approvalCommands.includes(item.command_type)) });
      setDirectoryState('ready');
    }).catch(() => { if (!controller.signal.aborted) setDirectoryState('error'); });
    return () => controller.abort();
  }, [client, owner, route.view, refresh]);
  return <section className="adm-approval-workspace" aria-label={copy.nav.approvals}>
    <nav className="adm-tabs adm-approval-tabs" aria-label={c.views}>{approvalViews.map(view => <button key={view} className={route.view === view ? 'active' : undefined} disabled={blocked} aria-current={route.view === view ? 'page' : undefined} onClick={() => { if (view !== route.view) setMineStatus(''); changeRoute({ view, status: '', requestID: '' }); }}>{c[view]}</button>)}</nav>
    {route.view !== 'directory' && <form className="adm-approval-toolbar" onSubmit={event => { event.preventDefault(); if (mine) applyMine(); else changeRoute({ commandType: command, target: target.trim(), requestID: '' }); }}>
      <label>{c.commandType}<select disabled={blocked} value={command} onChange={event => setCommand(event.target.value)}><option value="">{c.allCommands}</option>{approvalCommands.map(type => <option key={type} value={type}>{mine ? approvalLabel(c.policyCommandLabels, type) : type}</option>)}</select></label>
      <label>{mine ? <span id={mineTargetID + '-label'}>{mineWords.targetID}</span> : c.target}<input disabled={blocked} value={target} aria-labelledby={mine ? mineTargetID + '-label' : undefined} aria-describedby={mine ? mineTargetID + '-help' : undefined} onChange={event => setTarget(event.target.value)} />{mine && <small className="adm-approval-target-help" id={mineTargetID + '-help'}>{mineWords.targetHelp}</small>}</label>
      {route.view !== 'pending' && <label>{c.status}<select disabled={blocked} value={route.view === 'execution' ? query.status : mineStatus} onChange={event => mine ? setMineStatus(event.target.value) : changeRoute({ status: event.target.value, requestID: '' })}>
        {route.view === 'mine' && <option value="">{c.allStatuses}</option>}{(route.view === 'execution' ? executionStatuses : approvalStatuses).map(status => <option key={status} value={status}>{approvalLabel(c.statusLabels, status)}</option>)}
      </select></label>}{mine ? <div className="adm-approval-query-actions"><button type="submit" className="adm-approval-query" disabled={blocked}>{mineWords.query}</button><button type="button" disabled={blocked} onClick={() => applyMine(true)}>{mineWords.reset}</button></div> : <button disabled={blocked}>{copy.refresh}</button>}
    </form>}
    {route.view === 'execution' && <p className="adm-approval-execution-scope">{c.executionScopeLabel}: <strong>{approvalLabel(c.statusLabels, query.status)}</strong><small>{c.executionDefaultHelp}</small></p>}
    {route.view === 'directory' ? <><button onClick={() => setRefresh(value => value + 1)}>{c.refresh}</button>
      {directoryState === 'loading' && <p role="status">{c.loading}</p>}{directoryState === 'error' && <p role="alert">{c.listUnavailable}</p>}
      {directory && <div className="adm-table-wrap adm-approval-policy-wrap"><table className="adm-approval-policy-table"><thead><tr><th>{c.commandType}</th><th>{c.risk}</th><th>{c.requestable}</th></tr></thead><tbody>{directory.items.map(item => {
        const commandLabel = approvalLabel(c.policyCommandLabels, item.command_type), riskLabel = approvalLabel(c.policyRiskLabels, item.risk_class);
        return <tr key={`${item.command_type}:${item.policy_version}`}>
          <td><strong>{commandLabel}</strong>{commandLabel !== item.command_type && <code>{item.command_type}</code>}<small>{financeCopy[language].version}: {item.policy_version}</small></td>
          <td><span className="adm-approval-policy-risk">{riskLabel}</span>{riskLabel !== item.risk_class && <code>{item.risk_class}</code>}</td>
          <td>{item.requestable ? c.yes : c.no}</td>
        </tr>;
      })}</tbody></table></div>}
    </> : <Approvals client={client} copy={financeCopy[language]} permissions={me.platform_permissions || []} owner={owner} requestID={route.requestID}
      query={query} pendingUserID={route.view === 'pending' ? me.user_id : null} onStateChange={setBlocked} onNavigate={onNavigate} centerCopy={c} mineReadCopy={mine ? mineWords : undefined} pendingReadCopy={route.view === 'pending' ? pendingApprovalReadCopy[language] : undefined} mineApplyGeneration={mine ? mineApplyGeneration : undefined} />}
  </section>;
}
