import React, { useEffect, useMemo, useState } from 'react';
import { financeCopy } from '../../shared/i18n.js';
import { Approvals } from './AdminFinanceWorkspace.jsx';
import { approvalCommands, approvalViews, approvalStatuses, executionStatuses, approvalQuery, readApprovalRoute } from './admin-approval-actions.js';
import './admin-approval-workspace.css';

export function AdminApprovalWorkspace({ client, copy, language, me, onNavigate }) {
  const c = copy.approvalCenter;
  const [route, setRoute] = useState(() => readApprovalRoute(globalThis.location?.search));
  const [command, setCommand] = useState(route.commandType);
  const [target, setTarget] = useState(route.target);
  const [blocked, setBlocked] = useState(false);
  const [directory, setDirectory] = useState(null);
  const [directoryState, setDirectoryState] = useState('loading');
  const [refresh, setRefresh] = useState(0);
  const owner = `${me.user_id}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`;
  const query = useMemo(() => approvalQuery(route, me.user_id), [route, me.user_id]);
  const changeRoute = patch => {
    if (blocked) return;
    const next = { ...route, ...patch };
    const params = new URLSearchParams(globalThis.location?.search || '');
    for (const [key, value] of Object.entries({ approval_view: next.view, approval_command_type: next.commandType,
      approval_target: next.target, approval_status: next.status, approval_request_id: next.requestID })) {
      if (value) params.set(key, value); else params.delete(key);
    }
    params.delete('approval_cursor');
    globalThis.history?.replaceState(null, '', `${globalThis.location?.pathname || ''}?${params}${globalThis.location?.hash || ''}`);
    setRoute(next);
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
    <nav className="adm-approval-tabs" aria-label={c.views}>{approvalViews.map(view => <button key={view} disabled={blocked} aria-current={route.view === view ? 'page' : undefined} onClick={() => changeRoute({ view, status: '', requestID: '' })}>{c[view]}</button>)}</nav>
    {route.view !== 'directory' && <form className="adm-approval-toolbar" onSubmit={event => { event.preventDefault(); changeRoute({ commandType: command, target: target.trim(), requestID: '' }); }}>
      <label>{c.commandType}<select disabled={blocked} value={command} onChange={event => setCommand(event.target.value)}><option value="">{c.allCommands}</option>{approvalCommands.map(type => <option key={type}>{type}</option>)}</select></label>
      <label>{c.target}<input disabled={blocked} value={target} onChange={event => setTarget(event.target.value)} /></label>
      {route.view !== 'pending' && <label>{c.status}<select disabled={blocked} value={route.status || (route.view === 'execution' ? 'EXECUTION_FAILED' : '')} onChange={event => changeRoute({ status: event.target.value, requestID: '' })}>
        {route.view === 'mine' && <option value="">{c.allStatuses}</option>}{(route.view === 'execution' ? executionStatuses : approvalStatuses).map(status => <option key={status}>{status}</option>)}
      </select></label>}<button disabled={blocked}>{copy.refresh}</button>
    </form>}
    {route.view === 'directory' ? <><button onClick={() => setRefresh(value => value + 1)}>{c.refresh}</button>
      {directoryState === 'loading' && <p role="status">{c.loading}</p>}{directoryState === 'error' && <p role="alert">{c.listUnavailable}</p>}
      {directory && <table><thead><tr><th>{c.commandType}</th><th>{c.risk}</th><th>{c.requestable}</th></tr></thead><tbody>{directory.items.map(item => <tr key={`${item.command_type}:${item.policy_version}`}><td>{item.command_type} · v{item.policy_version}</td><td>{item.risk_class}</td><td>{item.requestable ? c.yes : c.no}</td></tr>)}</tbody></table>}
    </> : <Approvals client={client} copy={financeCopy[language]} permissions={me.platform_permissions || []} owner={owner} requestID={route.requestID}
      query={query} pendingUserID={route.view === 'pending' ? me.user_id : null} onStateChange={setBlocked} onNavigate={onNavigate} centerCopy={c} />}
  </section>;
}
