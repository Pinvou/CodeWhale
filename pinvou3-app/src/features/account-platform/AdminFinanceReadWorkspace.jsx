import React, { useEffect, useState } from 'react';
import { accountLayoutCopy, adminLayoutCopy, financeCopy } from '../../shared/i18n.js';
import { formatBalance } from './balance-view.js';
import { formatFen, requireFinanceCollection, requireFinanceID } from './finance-contract.js';
import { localFinanceDate } from './AdminFinanceFilters.jsx';
import { financeReadDefinition, financeReadIdentity, financeListRouteUpdates, readFinanceListRoute } from './admin-finance-read-route.js';

const definitions = {
  receivables: { id: 'receivable_id', columns: ['receivable_id', 'wallet_id', 'original_microcredits', 'settled_microcredits', 'outstanding_microcredits', 'status'] },
  refunds: { id: 'operation_id', columns: ['operation_id', 'operation_type', 'payment_order_id', 'amount_fen', 'status', 'created_at'] },
  adjustments: { id: 'adjustment_id', columns: ['adjustment_id', 'wallet_id', 'amount_microcredits', 'status', 'requested_by', 'created_at'] },
};
const statuses = {
  receivables: ['OPEN', 'PARTIALLY_SETTLED', 'SETTLED', 'WRITTEN_OFF'],
  refunds: ['REQUESTED', 'SUCCEEDED', 'FAILED', 'MANUAL_REVIEW', 'REVIEW_REQUIRED', 'RESOLVED'],
  adjustments: ['PENDING_APPROVAL', 'APPROVED', 'REJECTED', 'POSTED'],
};
// A read-only view uses the installed typed clients. Financial commands remain
// in their existing owners and are never dispatched by URL or refresh effects.
export function AdminFinanceReadWorkspace({ kind, client, language, route, onViewApproval }) {
  const copy = financeCopy[language], words = adminLayoutCopy[language].financeLists, config = definitions[kind], fields = financeReadDefinition(kind).fields;
  const current = readFinanceListRoute(kind, route.search), signature = JSON.stringify(current);
  const [draft, setDraft] = useState(current.query), [state, setState] = useState({ owner: null, signature: '', data: null, error: null }), [revision, setRevision] = useState(0), [previous, setPrevious] = useState([]);
  const filterSignature = JSON.stringify(Object.fromEntries(Object.entries(current.query).filter(([key]) => key !== 'cursor')));
  useEffect(() => { setDraft(Object.fromEntries(Object.entries(current.query).map(([key, value]) => [key, key.startsWith('created_') ? localFinanceDate(value) : value]))); }, [signature]);
  useEffect(() => { setPrevious([]); }, [filterSignature]);
  useEffect(() => {
    const controller = new AbortController();
    setState({ owner: client, signature, data: null, error: null });
    void (async () => {
      const query = { ...current.query };
      for (const key of ['wallet_id', 'requested_by']) if (query[key]) requireFinanceID(query[key]);
      for (const key of ['created_from', 'created_to']) if (query[key]) { const date = new Date(query[key]); if (!Number.isFinite(date.getTime())) throw new Error('INVALID_FINANCE_FILTER'); if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(query[key])) query[key] = date.toISOString(); }
      if (current.selectedID) {
        if (kind === 'refunds') { if (!/^(NORMAL|FORCED):/.test(current.selectedID)) throw new Error('INVALID_FINANCE_ACK'); requireFinanceID(current.selectedID.split(':')[1]); }
        else requireFinanceID(current.selectedID);
      }
      const page = await client[kind](query, controller.signal), items = requireFinanceCollection(page);
      for (const item of items) { requireFinanceID(item[config.id]); financeReadIdentity(kind, item); }
      let selected = items.find(item => financeReadIdentity(kind, item) === current.selectedID);
      if (current.selectedID && !selected && kind === 'refunds') {
        const matching = await client[kind]({ q: current.selectedID, limit: 200 }, controller.signal);
        selected = requireFinanceCollection(matching).find(item => financeReadIdentity(kind, item) === current.selectedID);
      }
      const allocations = current.selectedID && kind === 'receivables' ? requireFinanceCollection(await client.receivableAllocations(current.selectedID, controller.signal)) : null;
      if (!controller.signal.aborted) setState({ owner: client, signature, data: { page, items, selected, allocations }, error: null });
    })().catch(error => { if (!controller.signal.aborted) setState({ owner: client, signature, data: null, error }); });
    return () => controller.abort();
  }, [client, kind, signature, revision]);
  const visible = state.owner === client && state.signature === signature ? state : { data: null, error: null };
  const labelStatus = value => words[value] || accountLayoutCopy[language].statuses[value] || value;
  const cell = (key, value) => value == null ? '—' : key.endsWith('_microcredits') ? formatBalance(value, language) : key === 'amount_fen' ? formatFen(value, language) : key.endsWith('_at') ? Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language]) : '—' : ['status', 'operation_type'].includes(key) ? labelStatus(value) : String(value);
  const move = (query, selectedID = null, mode = 'push') => { setRevision(value => value + 1); route.navigate(route.section, financeListRouteUpdates(kind, query, selectedID, current.query), mode); };
  const refresh = () => { if (visible.error?.status === 410 || visible.error?.status === 400 && current.query.cursor) move({ ...current.query, cursor: '' }, current.selectedID || null, 'replace'); else setRevision(value => value + 1); };
  const table = (items, columns, identify) => <div className="adm-table-wrap"><table className="adm-table"><thead><tr>{columns.map(key => <th key={key}>{words[key] || copy[key]}</th>)}{identify && <th>{copy.details}</th>}</tr></thead><tbody>{items.map((item, index) => <tr key={identify ? financeReadIdentity(kind, item) : index}>{columns.map(key => <td key={key} data-label={words[key] || copy[key]}>{cell(key, item[key])}</td>)}{identify && <td><button onClick={() => move(current.query, financeReadIdentity(kind, item))}>{copy.details}</button></td>}</tr>)}</tbody></table></div>;
  return <section className="adm-finance-read-view" aria-label={words[kind]}><header><h3>{words[kind]}</h3><button onClick={refresh}>{copy.refresh}</button></header>
    {visible.error && <p role="alert">{visible.error.status === 403 ? copy.permissions : visible.error.status === 410 ? words.cursorExpired : copy.unavailable}<button onClick={refresh}>{copy.retry}</button></p>}
    {!visible.data && !visible.error && <p role="status">{copy.loading}</p>}
    {current.selectedID ? <><button onClick={() => move(current.query)}>{words.back}</button><h4>{current.selectedID}</h4>{visible.data && <>
      {visible.data.selected ? <dl className="adm-user-details">{config.columns.map(key => <React.Fragment key={key}><dt>{words[key] || copy[key]}</dt><dd>{cell(key, visible.data.selected[key])}</dd></React.Fragment>)}</dl> : <p>{words.detailUnavailable}</p>}
      {visible.data.allocations && table(visible.data.allocations, ['amount_microcredits', 'created_at'])}
      {kind === 'adjustments' && onViewApproval && route.available.includes('approvals') && <button onClick={() => onViewApproval(null, { approval_command_type: 'WALLET_ADJUSTMENT', approval_target: current.selectedID })}>{copy.approvals}</button>}
    </>}</> : <><form className="adm-filter-row" onSubmit={event => { event.preventDefault(); move({ ...draft, cursor: '' }); }}>
      {fields.map(key => <label key={key}>{words[key]}{key === 'status' ? <select value={draft[key] || ''} onChange={event => setDraft(value => ({ ...value, [key]: event.target.value }))}><option value="">{words.allStatuses}</option>{statuses[kind].map(value => <option key={value} value={value}>{labelStatus(value)}</option>)}</select> : <input maxLength={200} step={key.startsWith('created_') ? '0.001' : undefined} type={key.startsWith('created_') ? 'datetime-local' : 'text'} value={draft[key] || ''} onChange={event => setDraft(value => ({ ...value, [key]: event.target.value }))} />}</label>)}
      <label>{words.limit}<select value={draft.limit} onChange={event => setDraft(value => ({ ...value, limit: event.target.value }))}>{['25', '50', '100', '200'].map(value => <option key={value}>{value}</option>)}</select></label><button>{words.search}</button>
    </form>{visible.data && <><p>{words.pageCount(visible.data.items.length)}</p>{table(visible.data.items, config.columns, config.id)}{visible.data.items.length === 0 && <p>{copy.empty}</p>}<nav><button disabled={!previous.length} onClick={() => { const values = [...previous], cursor = values.pop(); setPrevious(values); move({ ...current.query, cursor }); }}>{copy.previous}</button><button disabled={!visible.data.page.next_cursor} onClick={() => { setPrevious(value => [...value, current.query.cursor]); move({ ...current.query, cursor: visible.data.page.next_cursor }); }}>{copy.next}</button></nav></>}</>}
  </section>;
}
