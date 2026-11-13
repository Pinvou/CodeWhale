import React, { useEffect, useRef, useState } from 'react';
import { requireFinanceID } from './finance-contract.js';
import { loadFinancePage } from './admin-finance-pagination.js';
import { reconciliationDayRange, reconciliationReadError } from './reconciliation-contract.js';

const CHANNELS = ['WECHAT', 'ALIPAY', 'UNIONPAY'];

export function ReconciliationBatchSummary({ id, item, copy }) {
  if (!id) return <p>{copy.caseBatchNotSelected}</p>;
  const known = item?.reconciliation_run_id === id ? item : null;
  const fields = known ? [
    [copy.channel, known.channel],
    [copy.businessDate, known.business_date],
    [copy.status, copy.batchStatuses[known.status] || known.status],
    [copy.count, known.case_count],
    [copy.rows, known.provider_row_count],
  ] : [];
  return <div className="adm-case-batch-summary" role="status">
    <strong>{copy.caseBatchSelected}</strong>
    <code dir="ltr">{id}</code>
    {known ? <dl>{fields.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value ?? copy.caseBatchNotProvided}</dd></div>)}</dl> : <span>{copy.caseBatchFactsUnavailable}</span>}
  </div>;
}

export function AdminReconciliationBatchPicker({ client, copy, initialChannel, initialDate, selectedID, onChoose, disabled }) {
  const [channel, setChannel] = useState(initialChannel);
  const [date, setDate] = useState(initialDate);
  const [knownID, setKnownID] = useState('');
  const [idInvalid, setIDInvalid] = useState(false);
  const [result, setResult] = useState(null);
  const request = useRef(null);

  useEffect(() => () => { request.current?.abort(); request.current = null; }, [client]);

  const load = async ({ scope, cursor = '', history = [] }) => {
    if (disabled) return;
    request.current?.abort();
    const active = new AbortController();
    request.current = active;
    const isCurrent = () => request.current === active && !active.signal.aborted;
    setResult({ kind: 'loading', scope, cursor, history });
    try {
      const query = { channel: scope.channel, ...reconciliationDayRange(scope.date), cursor };
      const response = await loadFinancePage(query, filters => {
        // A stale 410 must not start another request after scope change.
        if (!isCurrent()) throw Object.assign(new Error('stale batch search'), { name: 'AbortError' });
        return client.reconciliationRuns(filters, active.signal);
      }, () => { if (isCurrent()) history = []; });
      if (!isCurrent()) return;
      response.value.items.forEach(item => requireFinanceID(item.reconciliation_run_id));
      setResult({ kind: 'ready', scope, cursor: response.query.cursor, history, page: response.value });
    } catch (error) {
      if (!isCurrent()) return;
      const code = error?.message === 'INVALID_RECONCILIATION_INPUT' ? 'invalid' : reconciliationReadError(error);
      setResult({ kind: 'error', scope, cursor: history.length ? cursor : '', history, code });
    }
  };

  const page = result?.page;
  const scopeLabel = result?.scope?.date ? [result.scope.channel, result.scope.date].join(' · ') : '';
  const chooseKnown = event => {
    event.preventDefault();
    if (disabled) return;
    let id;
    try { id = requireFinanceID(knownID.trim()); }
    catch { setIDInvalid(true); return; }
    setIDInvalid(false);
    onChoose(id, null);
  };

  return <section className="adm-case-batch-picker" aria-label={copy.caseBatchFind}>
    <h3>{copy.caseBatchFind}</h3>
    <form className="adm-reconciliation-toolbar" onSubmit={event => { event.preventDefault(); void load({ scope: { channel, date } }); }}>
      <label>{copy.channel}<select disabled={disabled} value={channel} onChange={event => setChannel(event.target.value)}>{CHANNELS.map(value => <option key={value}>{value}</option>)}</select></label>
      <label>{copy.caseBatchShanghaiDate}<input type="date" disabled={disabled} value={date} onChange={event => setDate(event.target.value)} /></label>
      <button type="submit" disabled={disabled}>{copy.caseBatchFind}</button>
    </form>
    <div role="status">{scopeLabel && <strong>{scopeLabel} · </strong>}{!result ? copy.caseBatchSearchPrompt : result.kind === 'loading' ? copy.loading : result.kind === 'ready' ? page.items.length ? copy.caseBatchCandidates : result.cursor ? copy.caseBatchPageEmpty : copy.caseBatchCandidatesEmpty : ''}</div>
    {result?.kind === 'error' && <div role="alert"><p>{copy[result.code === 'permissions' ? 'permission' : result.code] || copy.unavailable}</p><button type="button" disabled={disabled} onClick={() => void load(result)}>{copy.refresh}</button></div>}
    {page?.items.length > 0 && <ul className="adm-case-batch-candidates">{page.items.map(item => <li key={item.reconciliation_run_id}>
      <button type="button" disabled={disabled} aria-pressed={selectedID === item.reconciliation_run_id} onClick={() => onChoose(item.reconciliation_run_id, item)}>
        <span>{[item.channel, item.business_date, copy.batchStatuses[item.status] || item.status].filter(Boolean).join(' · ')}</span>
        <code dir="ltr">{item.reconciliation_run_id}</code>
        <small>{copy.count}: {item.case_count ?? copy.caseBatchNotProvided} · {copy.rows}: {item.provider_row_count ?? copy.caseBatchNotProvided}</small>
      </button>
    </li>)}</ul>}
    {page && (result.history.length > 0 || page.next_cursor) && <nav className="adm-reconciliation-pagination" aria-label={copy.caseBatchCandidatePages}>
      <button type="button" disabled={disabled || !result.history.length} onClick={() => void load({ scope: result.scope, cursor: result.history[result.history.length - 1], history: result.history.slice(0, -1) })}>{copy.previous}</button>
      <button type="button" disabled={disabled || !page.next_cursor} onClick={() => void load({ scope: result.scope, cursor: page.next_cursor, history: [...result.history, result.cursor] })}>{copy.next}</button>
    </nav>}
    <details><summary>{copy.caseBatchKnownID}</summary><form className="adm-reconciliation-toolbar" onSubmit={chooseKnown}>
      <label>{copy.batchID}<input dir="ltr" disabled={disabled} value={knownID} aria-invalid={idInvalid || undefined} onChange={event => { setKnownID(event.target.value); setIDInvalid(false); }} /></label>
      <button type="submit" disabled={disabled || !knownID.trim()}>{copy.caseBatchApplyID}</button>
      {idInvalid && <small role="alert">{copy.invalid}</small>}
    </form></details>
  </section>;
}
