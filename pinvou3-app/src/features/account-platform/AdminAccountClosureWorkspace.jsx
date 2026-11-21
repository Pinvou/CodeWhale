import { statusLabel } from './status-label.js';
import React, { useEffect, useRef, useState } from 'react';
import { accountClosureCopy, financeCopy } from '../../shared/i18n.js';
import { closureQueueCopy } from '../../shared/admin-closure-queue-i18n.js';
import { requireAccountClosure } from './account-closure-contract.js';
import { useClosureOperation } from './useClosureOperation.js';
import { closureRepairIntents } from './closure-intent.js';
import './admin-closure-queue.css';

const initialFilters = () => ({ status: '', stage: '' });
const firstPage = (filters = initialFilters()) => ({ filters, cursor: '' });
const statuses = ['IN_PROGRESS', 'BLOCKED', 'RETRY', 'REPAIR_REQUIRED'];
const stages = ['CLOSURE_REQUESTED', 'QUIESCING', 'FINANCIAL_CLEARANCE', 'IDENTITY_CLEANUP', 'ANONYMIZED'];
const empty = { items: null, pending: [], nextCursor: '', loading: true, error: null };
const recoveryRows = actor => closureRepairIntents(actor).map(value => ({ closure_request_id: value.facts.id }));
const sameReader = (one, two) => one?.client === two.client && one.userID === two.userID && one.epoch === two.epoch && one.authorization === two.authorization;

function ClosureRepair({ client, owner, item, isLocal, pending, active, copy, closureCopy, queue, language, canRepair, onStarted, onFinished, onChanged }) {
  const [reason, setReason] = useState('');
  const [needsAuthentication, setNeedsAuthentication] = useState(false);
  const id = item.closure_request_id;
  const operation = useClosureOperation(owner, { ...copy, recoveryUnavailable: closureCopy.recoveryUnavailable }, client, `account-closure-retry:${id}`);
  const retry = async () => {
    if (!canRepair || active || operation.busy || operation.blocked || (!operation.unknown && !reason.trim())) return;
    onStarted(item);
    try {
      const result = await operation.mutate({ id, reason: reason.trim() }, async (facts, key) => {
        setNeedsAuthentication(false);
        let raw;
        try { raw = await client.retryAccountClosure(facts.id, facts.reason, key); }
        catch (error) { if (error.code === 'RECENT_AUTHENTICATION_REQUIRED') setNeedsAuthentication(true); throw error; }
        const receipt = requireAccountClosure(raw);
        if (receipt.closure_request_id !== facts.id) { const error = new Error('COMMAND_RESULT_UNKNOWN'); error.code = 'COMMAND_RESULT_UNKNOWN'; throw error; }
        return receipt;
      });
      if (result) onChanged();
    } finally { onFinished(id); }
  };
  const validTime = typeof item.updated_at === 'string' && Number.isFinite(Date.parse(item.updated_at));
  const locale = { zh: 'zh-CN', ja: 'ja-JP', en: 'en-US' }[language];
  return <tr data-closure-id={id} data-closure-source={isLocal ? 'local' : 'server'}>
    <th scope="row"><code>{id}</code></th>
    <td>{isLocal ? (pending ? queue.pending : queue.sending) : statusLabel(item.status, language, closureCopy.statuses)}</td>
    <td>{isLocal ? '—' : statusLabel(item.stage, language, closureCopy.stages)}</td>
    <td>{!isLocal && validTime ? <time dateTime={item.updated_at}>{new Date(item.updated_at).toLocaleString(locale)}</time> : '—'}</td>
    <td className="adm-closure-action-cell">
      {pending && <p>{closureCopy.recoveryPending}</p>}
      {operation.error && <p role="alert">{needsAuthentication && !operation.unknown ? closureCopy.recentAuthenticationRequired : operation.error}</p>}
      {operation.message && <p role="status">{operation.message}</p>}
      {needsAuthentication && canRepair && <><p>{closureCopy.recentAuthenticationRequired}</p><button type="button" disabled={active} onClick={() => client.reauthenticate('/admin.html?section=closures')}>{closureCopy.reauthenticate}</button></>}
      {canRepair && <><label>{copy.reason}<textarea value={operation.pendingFacts?.reason || reason} disabled={active || operation.busy || operation.unknown || operation.blocked} onChange={event => setReason(event.target.value)} /></label><button type="button" disabled={active || operation.busy || operation.blocked || (!operation.unknown && !reason.trim())} onClick={() => void retry()}>{copy.retry}</button></>}
    </td>
  </tr>;
}

export function AdminAccountClosureWorkspace({ client, me, language }) {
  const copy = financeCopy[language], closureCopy = accountClosureCopy[language], queue = closureQueueCopy[language];
  const [state, setState] = useState(empty);
  const [draft, setDraft] = useState(initialFilters);
  const [pinned, setPinned] = useState({ owner: null, items: [] });
  const generation = useRef(0), controller = useRef(null), currentQuery = useRef(firstPage());
  const permissions = me.platform_permissions || [];
  const canRead = permissions.includes('platform.account-closures.read');
  const canRepair = permissions.includes('platform.account-closures.repair');
  const epoch = client.financeSessionEpoch || 0;
  const authorization = me.platform_authorization_version || me.active_context?.platform_authorization_version || 0;
  const reader = { client, userID: me.user_id, epoch, authorization, canRead };
  const latest = useRef(reader);
  latest.current = reader;
  const load = async (query) => {
    if (!sameReader(reader, latest.current) || !latest.current.canRead) return;
    const stamp = ++generation.current;
    controller.current?.abort();
    const task = new AbortController();
    controller.current = task;
    currentQuery.current = query;
    const live = () => !task.signal.aborted && stamp === generation.current && sameReader(reader, latest.current) && latest.current.canRead;
    let pending;
    try { pending = recoveryRows(reader.userID); }
    catch {
      setState({ ...empty, reader, query, loading: false, error: { code: 'CLOSURE_RECOVERY_UNAVAILABLE' } });
      return;
    }
    setState({ ...empty, reader, query, pending });
    try {
      const result = await client.accountClosures(task.signal, query.cursor, query.filters);
      if (!live()) return;
      if (!Array.isArray(result?.items)) throw new Error('INVALID_CLOSURE_COLLECTION');
      result.items.forEach(requireAccountClosure);
      setState(previous => ({ ...previous, items: result.items, nextCursor: typeof result.next_cursor === 'string' ? result.next_cursor : '', loading: false }));
    } catch (error) {
      if (live()) setState(previous => ({ ...previous, loading: false, error }));
    }
  };
  useEffect(() => {
    const query = firstPage();
    setDraft(query.filters);
    if (canRead) void load(query); else setState(empty);
    return () => { generation.current++; controller.current?.abort(); };
  }, [client, me.user_id, epoch, authorization, canRead]); // eslint-disable-line react-hooks/exhaustive-deps
  const onStarted = (item) => {
    if (sameReader(reader, latest.current) && latest.current.canRead) setPinned(previous => {
      const prior = sameReader(previous.owner, reader) ? previous.items : [];
      return { owner: reader, items: [...prior.filter(row => row.closure_request_id !== item.closure_request_id), item] };
    });
  };
  const onFinished = (id) => {
    if (!sameReader(reader, latest.current)) return;
    if (latest.current.canRead) {
      try {
        const pending = recoveryRows(reader.userID);
        setState(previous => sameReader(previous.reader, reader) ? { ...previous, pending } : previous);
      } catch { /* A subsequent read preserves the original storage failure guard. */ }
    }
    setPinned(previous => sameReader(previous.owner, reader) ? { ...previous, items: previous.items.filter(item => item.closure_request_id !== id) } : previous);
  };
  const onChanged = () => { if (sameReader(reader, latest.current) && latest.current.canRead) void load(currentQuery.current); };
  if (!canRead) return <p role="alert">{closureCopy.listPermissionDenied}</p>;
  const visible = sameReader(state.reader, reader) ? state : { ...empty, query: firstPage() };
  const denied = [401, 403].includes(visible.error?.status) || visible.error?.code === 'PERMISSION_DENIED';
  const invalidCursor = Boolean(visible.query.cursor && visible.error?.code === 'INVALID_CLOSURE_FILTER');
  const problem = denied ? closureCopy.listPermissionDenied : visible.error?.code === 'CLOSURE_RECOVERY_UNAVAILABLE' ? closureCopy.recoveryUnavailable : invalidCursor ? queue.invalidCursor : closureCopy.listUnavailable;
  const visiblePinned = sameReader(pinned.owner, reader) ? pinned.items : [];
  const activeIDs = new Set(visiblePinned.map(item => item.closure_request_id));
  const server = visible.items || [];
  const pendingIDs = new Set(visible.pending.map(item => item.closure_request_id));
  const serverIDs = new Set(server.map(item => item.closure_request_id));
  const local = [...new Map([...visiblePinned, ...visible.pending].map(item => [item.closure_request_id, item])).values()].filter(item => !serverIDs.has(item.closure_request_id));
  const resultReady = !visible.loading && !visible.error && visible.items !== null;
  const filtered = Boolean(visible.query.filters.status || visible.query.filters.stage);
  const emptyText = !visible.query.cursor && filtered ? queue.noMatches : local.length ? queue.noServerRows : closureCopy.listEmpty;
  const columns = [queue.requestID, queue.status, queue.stage, queue.updated, queue.actions];
  const select = (key, label, all, values, words) => <label>{label}<select value={draft[key]} onChange={event => setDraft({ ...draft, [key]: event.target.value })}><option value="">{all}</option>{values.map(value => <option key={value} value={value}>{words[value]}</option>)}</select></label>;
  const rows = [
    ...(resultReady && !server.length ? [{ kind: 'empty' }] : []),
    ...server.map(item => ({ kind: 'record', item, isLocal: false })),
    ...(local.length ? [{ kind: 'local-header' }] : []),
    ...local.map(item => ({ kind: 'record', item, isLocal: true })),
  ];
  return <section className="adm-closure-directory" aria-busy={visible.loading}>
    <h2>{closureCopy.listTitle}</h2><p>{closureCopy.listLead}</p><p className="adm-closure-range">{queue.activeOnly}</p>
    <button type="button" disabled={visible.loading} onClick={() => void load(firstPage(currentQuery.current.filters))}>{copy.refresh}</button>
    {!denied && <form className="adm-closure-filters" onSubmit={event => { event.preventDefault(); void load(firstPage(draft)); }}>
      {select('status', queue.status, queue.allStatuses, statuses, closureCopy.statuses)}
      {select('stage', queue.stage, queue.allStages, stages, closureCopy.stages)}
      <button type="submit" className="adm-primary-button">{queue.search}</button>
      <button type="button" onClick={() => { const next = firstPage(); setDraft(next.filters); void load(next); }}>{queue.clear}</button>
    </form>}
    {!denied && filtered && <p className="adm-closure-range">{queue.applied}: {[closureCopy.statuses[visible.query.filters.status], closureCopy.stages[visible.query.filters.stage]].filter(Boolean).join(' · ')}</p>}
    {visible.loading && <p role="status">{closureCopy.listLoading}</p>}
    {visible.error && <div className="adm-panel" role="alert"><p>{problem}</p><button type="button" onClick={() => void load(invalidCursor ? firstPage(visible.query.filters) : visible.query)}>{invalidCursor ? queue.firstPage : copy.retry}</button></div>}
    {/* biome-ignore lint/a11y/noNoninteractiveTabindex: This labelled region allows keyboard scrolling on narrow screens. */}
    {!denied && <div className="adm-closure-table-wrap" tabIndex="0" role="region" aria-label={closureCopy.listTitle}><table className="adm-closure-table">
      <thead><tr>{columns.map(column => <th scope="col" key={column}>{column}</th>)}</tr></thead>
      <tbody>{rows.map(entry => {
        if (entry.kind === 'empty') return <tr key="service-empty" data-closure-empty><td colSpan={columns.length}><span role="status">{emptyText}</span></td></tr>;
        if (entry.kind === 'local-header') return <tr key="local-header" className="adm-closure-local-heading"><th colSpan={columns.length}>{queue.localRecovery}</th></tr>;
        return <ClosureRepair key={`${me.user_id}:${epoch}:${authorization}:${entry.item.closure_request_id}`} client={client} owner={me.user_id} item={entry.item} isLocal={entry.isLocal} pending={pendingIDs.has(entry.item.closure_request_id)} active={activeIDs.has(entry.item.closure_request_id)} copy={copy} closureCopy={closureCopy} queue={queue} language={language} canRepair={canRepair} onStarted={onStarted} onFinished={onFinished} onChanged={onChanged} />;
      })}</tbody>
    </table></div>}
    {resultReady && visible.query.cursor && <button type="button" onClick={() => void load(firstPage(visible.query.filters))}>{queue.firstPage}</button>}
    {resultReady && visible.nextCursor && <button type="button" onClick={() => void load({ filters: visible.query.filters, cursor: visible.nextCursor })}>{copy.next}</button>}
  </section>;
}
