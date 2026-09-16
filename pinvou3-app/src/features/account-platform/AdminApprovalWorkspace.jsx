import React, { useEffect, useState } from 'react';
import './admin-approval-workspace.css';

const views = ['mine', 'pending', 'execution', 'directory'];
const executionStatuses = ['EXECUTION_FAILED', 'EXECUTING', 'APPROVED', 'EXECUTED'];

function readRoute() {
  const query = new URLSearchParams(globalThis.location?.search || '');
  const view = query.get('approval_view');
  return {
    view: views.includes(view) ? view : 'mine',
    commandType: query.get('approval_command_type') || '',
    target: query.get('approval_target') || '',
    status: query.get('approval_status') || '',
    cursor: query.get('approval_cursor') || '',
    requestID: query.get('approval_request_id') || '',
  };
}

function targetOf(item) {
  return item?.target || { resource_type: item?.target_resource_type, resource_id: item?.target_resource_id };
}

function requesterOf(item) {
  return item?.requester?.user_id || item?.requester_user_id || '';
}

function date(value, language) {
  if (!value) return '—';
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? '—' : parsed.toLocaleString(language === 'ja' ? 'ja-JP' : language === 'en' ? 'en-US' : 'zh-CN');
}

export function AdminApprovalWorkspace({ client, copy, language, currentUserID, routeVersion }) {
  const c = copy.approvalCenter;
  const [route, setRoute] = useState(readRoute);
  const [commandInput, setCommandInput] = useState(route.commandType);
  const [targetInput, setTargetInput] = useState(route.target);
  const [page, setPage] = useState(null);
  const [directory, setDirectory] = useState(null);
  const [detail, setDetail] = useState(null);
  const [listError, setListError] = useState(false);
  const [detailError, setDetailError] = useState(false);
  const [loading, setLoading] = useState(false);
  const [refresh, setRefresh] = useState(0);
  const [previous, setPrevious] = useState([]);

  useEffect(() => { setRoute(readRoute()); setPrevious([]); }, [routeVersion]);
  useEffect(() => { setCommandInput(route.commandType); setTargetInput(route.target); }, [route.commandType, route.target]);

  const changeRoute = (patch, { replace = false } = {}) => {
    const next = { ...route, ...patch };
    const query = new URLSearchParams(globalThis.location?.search || '');
    for (const [key, value] of Object.entries({ approval_view: next.view, approval_command_type: next.commandType,
      approval_target: next.target, approval_status: next.status, approval_cursor: next.cursor,
      approval_request_id: next.requestID })) {
      if (value) query.set(key, value);
      else query.delete(key);
    }
    globalThis.history?.[replace ? 'replaceState' : 'pushState']?.(null, '', `${globalThis.location?.pathname || ''}?${query}`);
    setRoute(next);
  };

  useEffect(() => {
    const controller = new AbortController();
    setPage(null);
    setDirectory(null);
    setListError(false);
    setLoading(true);
    const load = async () => {
      try {
        if (route.view === 'directory') {
          const result = await client.governedApprovalDirectory(controller.signal);
          if (!controller.signal.aborted) setDirectory(result);
        } else {
          if (!currentUserID) throw new Error('approval actor unavailable');
          const filters = { limit: 50 };
          if (route.commandType) filters.command_type = route.commandType;
          if (route.target) filters.target = route.target;
          if (route.cursor) filters.cursor = route.cursor;
          if (route.view === 'mine') filters.requester_user_id = currentUserID;
          if (route.view === 'pending') filters.status = 'SUBMITTED';
          if (route.view === 'execution') filters.status = executionStatuses.includes(route.status) ? route.status : 'EXECUTION_FAILED';
          if (route.view === 'mine' && route.status) filters.status = route.status;
          const result = await client.governedApprovalRequests(filters, controller.signal);
          if (!controller.signal.aborted) setPage(result);
        }
      } catch {
        if (!controller.signal.aborted) setListError(true);
      } finally {
        if (!controller.signal.aborted) setLoading(false);
      }
    };
    void load();
    return () => controller.abort();
  }, [client, currentUserID, route.view, route.commandType, route.target, route.status, route.cursor, refresh]);

  useEffect(() => {
    const controller = new AbortController();
    setDetail(null);
    setDetailError(false);
    if (route.view === 'directory' || !route.requestID) return () => controller.abort();
    void client.governedApprovalRequest(route.requestID, controller.signal).then((result) => {
      if (!controller.signal.aborted) setDetail(result);
    }).catch(() => { if (!controller.signal.aborted) setDetailError(true); });
    return () => controller.abort();
  }, [client, route.view, route.requestID, refresh]);

  const rows = (page?.items || []).filter((item) => route.view !== 'pending' || requesterOf(item) !== currentUserID);
  const nextCursor = page?.next_cursor || '';
  const selectView = (view) => { setPrevious([]); changeRoute({ view, cursor: '', requestID: '', status: '' }); };
  const selectFilter = (patch) => { setPrevious([]); changeRoute({ ...patch, cursor: '', requestID: '' }); };
  const nextPage = () => { setPrevious((items) => [...items, route.cursor]); changeRoute({ cursor: nextCursor, requestID: '' }); };
  const previousPage = () => {
    const cursor = previous.at(-1) || '';
    setPrevious((items) => items.slice(0, -1));
    changeRoute({ cursor, requestID: '' });
  };

  return <section className="adm-approval-workspace" aria-label={copy.nav.approvals}>
    <nav className="adm-approval-tabs" aria-label={c.views}>
      {views.map((view) => <button key={view} type="button" aria-current={route.view === view ? 'page' : undefined}
        onClick={() => selectView(view)}>{c[view]}</button>)}
    </nav>
    <form className="adm-approval-toolbar" onSubmit={(event) => { event.preventDefault(); selectFilter({ commandType: commandInput.trim(), target: targetInput.trim() }); }}>
      {route.view !== 'directory' && <>
        <label>{c.commandType}<input value={commandInput} onChange={(event) => setCommandInput(event.target.value)} /></label>
        <label>{c.target}<input value={targetInput} onChange={(event) => setTargetInput(event.target.value)} /></label>
        {(route.view === 'mine' || route.view === 'execution') && <label>{c.status}<select value={route.status || (route.view === 'execution' ? 'EXECUTION_FAILED' : '')}
          onChange={(event) => selectFilter({ status: event.target.value })}>
          {route.view === 'mine' && <option value="">{c.allStatuses}</option>}
          {(route.view === 'execution' ? executionStatuses : ['DRAFT', 'SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTION_FAILED', 'EXECUTED', 'REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED']).map((status) => <option key={status} value={status}>{status}</option>)}
        </select></label>}
        <button type="submit">{copy.searchAction}</button>
      </>}
      <button type="button" onClick={() => setRefresh((value) => value + 1)}>{c.refresh}</button>
    </form>
    {loading && <p role="status">{c.loading}</p>}
    {listError && <p role="alert">{c.listUnavailable}</p>}
    {route.view === 'directory' && directory && <div className="adm-approval-table"><table><thead><tr><th>{c.commandType}</th><th>{c.risk}</th><th>{c.requestable}</th></tr></thead><tbody>
      {directory.items.map((item) => <tr key={`${item.command_type}-${item.policy_version}`}><td>{item.command_type} · v{item.policy_version}</td><td>{item.risk_class}</td><td>{item.requestable ? c.yes : c.no}</td></tr>)}
    </tbody></table>{directory.items.length === 0 && <p>{c.empty}</p>}</div>}
    {route.view !== 'directory' && page && <>
      <div className="adm-approval-table"><table><thead><tr><th>{c.commandType}</th><th>{c.status}</th><th>{c.target}</th><th>{c.requester}</th><th>{c.updated}</th><th>{c.detail}</th></tr></thead><tbody>
        {rows.map((item) => <tr key={item.approval_request_id}><td>{item.command_type}</td><td>{item.status}</td><td>{targetOf(item).resource_type} · {targetOf(item).resource_id}</td><td>{requesterOf(item)}</td><td>{date(item.updated_at, language)}</td><td><button type="button" onClick={() => changeRoute({ requestID: item.approval_request_id })}>{c.open}</button></td></tr>)}
      </tbody></table>{rows.length === 0 && <p>{c.empty}</p>}</div>
      <div className="adm-approval-pagination"><button type="button" disabled={!previous.length} onClick={previousPage}>{c.previous}</button><button type="button" disabled={!nextCursor} onClick={nextPage}>{c.next}</button></div>
    </>}
    {route.requestID && route.view !== 'directory' && <aside className="adm-approval-detail" aria-label={c.detail}>
      <div><h3>{c.detail}</h3><button type="button" onClick={() => changeRoute({ requestID: '' })}>{c.close}</button></div>
      {detailError && <p role="alert">{c.detailUnavailable}</p>}
      {!detail && !detailError && <p role="status">{c.loading}</p>}
      {detail && <dl>
        <div><dt>{c.requestID}</dt><dd><code>{detail.approval_request_id}</code></dd></div>
        <div><dt>{c.status}</dt><dd>{detail.status}</dd></div>
        <div><dt>{c.commandType}</dt><dd>{detail.command_type || '—'}</dd></div>
        <div><dt>{c.target}</dt><dd>{targetOf(detail).resource_type || '—'} · {targetOf(detail).resource_id || '—'}</dd></div>
        <div><dt>{c.requester}</dt><dd>{requesterOf(detail) || '—'}</dd></div>
        <div><dt>{c.updated}</dt><dd>{date(detail.updated_at, language)}</dd></div>
      </dl>}
      {detail?.detail_availability === 'DETAIL_PRESENTATION_UNAVAILABLE' && <p>{c.historicalMinimum}</p>}
    </aside>}
  </section>;
}
