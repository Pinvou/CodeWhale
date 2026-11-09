import React, { useEffect, useId, useRef, useState } from 'react';
import { accountLayoutCopy, adminLayoutCopy, financeCopy } from '../../shared/i18n.js';
import { formatBalance } from './balance-view.js';
import { formatFen, requireFinanceCollection, requireFinanceID } from './finance-contract.js';
import { localFinanceDate } from './AdminFinanceFilters.jsx';
import { AdminAdjustmentLookup } from './AdminAdjustmentLookup.jsx';
import { financeReadDefinition, financeReadIdentity, financeListRouteUpdates, readFinanceListRoute } from './admin-finance-read-route.js';

const receivableSources = ['USAGE_OVERAGE', 'CHARGEBACK', 'FORCED_REFUND'];
const billingListFilterTrim = raw => raw.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, '');
function billingListEnumDisplayKey(raw) {
  // The Billing filter trims Unicode White_Space (not BOM) and uppercases
  // each rune. This is for labels only: the route and GET keep the raw value.
  return Array.from(billingListFilterTrim(raw), rune => {
    const upper = rune.toUpperCase();
    return upper.length === 1 ? upper : rune;
  }).join('');
}
const adjustmentZone = 'Asia/Shanghai';
const adjustmentDate = (value, language) => {
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) || !Number.isFinite(Date.parse(value))) return '';
  const date = new Intl.DateTimeFormat({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language], { timeZone: adjustmentZone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' });
  const parts = Object.fromEntries(date.formatToParts(new Date(value)).map(({ type, value: part }) => [type, part]));
  return `${parts.year}-${parts.month}-${parts.day} ${parts.hour}:${parts.minute}:${parts.second}`;
};

const definitions = {
  receivables: { id: 'receivable_id', columns: ['receivable_id', 'wallet_id', 'original_microcredits', 'settled_microcredits', 'outstanding_microcredits', 'status'] },
  refunds: { id: 'operation_id', columns: ['operation_id', 'operation_type', 'payment_order_id', 'amount_fen', 'status', 'created_at'] },
  adjustments: { id: 'adjustment_id', columns: ['adjustment_id', 'wallet_id', 'amount_microcredits', 'status', 'requested_by', 'created_at', 'effective_at'] },
};
const statuses = {
  receivables: ['OPEN', 'PARTIALLY_SETTLED', 'SETTLED', 'WRITTEN_OFF'],
  refunds: ['REQUESTED', 'SUCCEEDED', 'FAILED', 'MANUAL_REVIEW', 'REVIEW_REQUIRED', 'RESOLVED'],
  adjustments: ['PENDING_APPROVAL', 'APPROVED', 'REJECTED', 'POSTED'],
};
// A read-only view uses the installed typed clients. Financial commands remain
// in their existing owners and are never dispatched by URL or refresh effects.
export function AdminFinanceReadWorkspace({ kind, client, language, route, onViewApproval, permissions = [] }) {
  const copy = financeCopy[language], words = adminLayoutCopy[language].financeLists, config = definitions[kind], fields = financeReadDefinition(kind).fields;
  const current = readFinanceListRoute(kind, route.search), signature = JSON.stringify(current);
  const walletHelpID = useId();
  const receivableFilters = kind === 'receivables' ? words.receivableFilter : null;
  const timeWords = kind === 'receivables' ? words.receivableTime : null;
  const resultWords = kind === 'receivables' ? words.receivableResults : null;
  const timeHelpID = useId();
  const [draft, setDraft] = useState(current.query), [state, setState] = useState({ owner: null, signature: '', data: null, error: null }), [revision, setRevision] = useState(0), [previous, setPrevious] = useState([]);
  const sourceDisplayKey = receivableFilters ? billingListEnumDisplayKey(draft.source_type || '') : '';
  const unsupportedSource = Boolean(sourceDisplayKey) && !receivableSources.includes(sourceDisplayKey);
  const filterSignature = JSON.stringify(Object.fromEntries(Object.entries(current.query).filter(([key]) => key !== 'cursor')));
  const adjustmentFirstBatch = kind === 'adjustments' && !billingListFilterTrim(current.query.cursor || '');
  useEffect(() => { setDraft(Object.fromEntries(Object.entries(current.query).map(([key, value]) => [key, key.startsWith('created_') ? localFinanceDate(value) : value]))); }, [signature]);
  useEffect(() => { setPrevious([]); }, [filterSignature]);
  useEffect(() => { if (adjustmentFirstBatch) setPrevious([]); }, [adjustmentFirstBatch]);
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
      if (!controller.signal.aborted) setState({ owner: client, signature, data: { page, items, selected, allocations, query }, error: null });
    })().catch(error => { if (!controller.signal.aborted) setState({ owner: client, signature, data: null, error }); });
    return () => controller.abort();
  }, [client, kind, signature, revision]);
  const visible = state.owner === client && state.signature === signature ? state : { data: null, error: null };
  const labelStatus = value => words[value] || accountLayoutCopy[language].statuses[value] || value;
  const cell = (key, value) => value == null ? '—' : key.endsWith('_microcredits') ? formatBalance(value, language) : key === 'amount_fen' ? formatFen(value, language) : key.endsWith('_at') ? Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language]) : '—' : ['status', 'operation_type'].includes(key) ? labelStatus(value) : String(value);
  const adjustmentFacts = kind === 'adjustments' ? adminLayoutCopy[language].adjustmentFacts : null;
  const adjustmentLookup = kind === 'adjustments' ? adminLayoutCopy[language].adjustmentLookup : null;
  const canReadRequester = permissions.includes('platform.users.read') && route.available.includes('users');
  const canReadWalletLedger = permissions.includes('platform.billing.ledger.read');
  const [requesterDirectoryDenial, setRequesterDirectoryDenial] = useState(null);
  const directoryEpoch = client.financeSessionEpoch || 0;
  const currentRequesterOwner = useRef(null);
  currentRequesterOwner.current = { client, epoch: directoryEpoch, allowed: canReadRequester };
  const requesterDirectoryDenied = requesterDirectoryDenial?.client === client && requesterDirectoryDenial.epoch === directoryEpoch;
  const onRequesterDirectoryAccess = allowed => {
    const owner = currentRequesterOwner.current;
    if (owner.client !== client || owner.epoch !== directoryEpoch || !owner.allowed || (client.financeSessionEpoch || 0) !== directoryEpoch) return;
    setRequesterDirectoryDenial(allowed ? null : { client, epoch: directoryEpoch });
  };
  const openRequester = canReadRequester && !requesterDirectoryDenied ? id => route.navigate('users', { user_id: id }) : null;
  const openWallet = canReadWalletLedger ? id => route.navigate('billing', { finance_tab: 'wallets', ...financeListRouteUpdates('wallets', { q: id, limit: '25' }, id), ...financeListRouteUpdates('ledger', { limit: '50' }) }) : null;
  const related = (key, value) => {
    let id;
    try { id = requireFinanceID(value); } catch { return cell(key, value); }
    const open = key === 'requested_by' ? openRequester : openWallet;
    const label = key === 'requested_by' ? adjustmentLookup.openRequester : adjustmentLookup.openWallet;
    return open ? <button type="button" className="adm-name-link" aria-label={label + ': ' + id} onClick={() => open(id)}><code>{id}</code></button> : <code>{id}</code>;
  };
  const column = key => adjustmentFacts && key === 'amount_microcredits' ? adjustmentFacts.amount : adjustmentFacts && key === 'created_at' ? adjustmentFacts.requestTime + ' (' + adjustmentZone + ')' : adjustmentFacts && key === 'effective_at' ? adjustmentFacts.effectiveTime + ' (' + adjustmentZone + ')' : words[key] || copy[key];
  const valueFor = (key, item) => {
    if (adjustmentLookup && ['wallet_id', 'requested_by'].includes(key)) return related(key, item[key]);
    if (adjustmentFacts && key === 'amount_microcredits') {
      const amount = formatBalance(item.amount_microcredits, language);
      return amount === '—' ? adjustmentFacts.unavailable : (amount === '0' ? amount : amount.startsWith('-') ? '−' + amount.slice(1) : '+' + amount) + ' ' + adjustmentFacts.points;
    }
    if (adjustmentFacts && key === 'created_at') return adjustmentDate(item.created_at, language) || adjustmentFacts.unavailable;
    if (adjustmentFacts && key === 'effective_at') {
      if (item.status !== 'POSTED') return ['PENDING_APPROVAL', 'APPROVED', 'REJECTED'].includes(item.status) ? adjustmentFacts.notEffective : adjustmentFacts.unavailable;
      // Billing records the journal entry and decided_at in the same POSTED transaction.
      try { requireFinanceID(item.journal_entry_id); } catch { return adjustmentFacts.unavailable; }
      return adjustmentDate(item.decided_at, language) || adjustmentFacts.unavailable;
    }
    return cell(key, item[key]);
  };
  const move = (query, selectedID = null, mode = 'push') => { setRevision(value => value + 1); route.navigate(route.section, financeListRouteUpdates(kind, query, selectedID, current.query), mode); };
  const dateInputValue = value => value?.endsWith('Z') ? localFinanceDate(value) : value || '';
  const fillDay = offset => {
    const now = new Date();
    const day = shift => new Date(now.getFullYear(), now.getMonth(), now.getDate() + offset + shift);
    // An explicit preset replaces the old instant even if both display the same local time.
    setDraft(value => ({ ...value, created_from: day(0).toISOString(), created_to: day(1).toISOString() }));
  };
  const timeRange = timeWords && <fieldset className="adm-receivable-time-range" aria-describedby={timeHelpID}>
    <legend>{timeWords.legend}</legend>
    <small id={timeHelpID}>{timeWords.zone.replace('{zone}', () => Intl.DateTimeFormat().resolvedOptions().timeZone || timeWords.local)}</small>
    <div className="adm-receivable-time-fields">
      {['created_from', 'created_to'].map(key => <label key={key}>{key === 'created_from' ? timeWords.from : timeWords.to}
        <input maxLength={200} step="0.001" type="datetime-local" value={dateInputValue(draft[key])} onChange={event => setDraft(value => ({ ...value, [key]: event.target.value }))} />
      </label>)}
    </div>
    <div className="adm-receivable-time-presets">
      <button type="button" onClick={() => fillDay(0)}>{timeWords.today}</button>
      <button type="button" onClick={() => fillDay(-1)}>{timeWords.yesterday}</button>
    </div>
  </fieldset>;
  const refresh = () => { if (visible.error?.status === 410 || visible.error?.status === 400 && current.query.cursor) move({ ...current.query, cursor: '' }, current.selectedID || null, 'replace'); else setRevision(value => value + 1); };
  const adjustmentResults = kind === 'adjustments' ? adminLayoutCopy[language].adjustmentList : null;
  const adjustmentQuery = adjustmentResults ? visible.data?.query : null;
  const adjustmentStatus = adjustmentResults ? billingListEnumDisplayKey(current.query.status || '') : '';
  const showAdjustmentView = adjustmentResults && (!adjustmentStatus || statuses.adjustments.includes(adjustmentStatus));
  const adjustmentDraftStatus = adjustmentResults ? billingListEnumDisplayKey(draft.status || '') : '';
  const unsupportedAdjustmentDraft = Boolean(adjustmentDraftStatus) && !statuses.adjustments.includes(adjustmentDraftStatus);
  const statusControlValue = unsupportedAdjustmentDraft ? '__invalid_adjustment_status__' : adjustmentResults ? adjustmentDraftStatus : draft.status || '';
  const appliedAdjustment = adjustmentQuery ? fields.filter(key => key === 'status' ? adjustmentStatus : adjustmentQuery[key]) : [];
  const adjustmentCursor = billingListFilterTrim(adjustmentQuery?.cursor || '');
  const pendingAdjustment = adjustmentStatus === 'PENDING_APPROVAL';
  const emptyAdjustments = adjustmentCursor ? adjustmentResults?.emptyPage : pendingAdjustment && appliedAdjustment.length === 1 ? adjustmentResults?.pendingEmpty : appliedAdjustment.length ? adjustmentResults?.noMatches : adjustmentResults?.noRecords;
  // Describe the successful GET: its date values may have been resolved from a legacy local URL.
  const appliedQuery = resultWords ? visible.data?.query : null;
  const appliedSourceDisplayKey = billingListEnumDisplayKey(appliedQuery?.source_type || '');
  const appliedStatusDisplayKey = billingListEnumDisplayKey(appliedQuery?.status || '');
  const appliedCursor = billingListFilterTrim(appliedQuery?.cursor || '');
  const applied = appliedQuery ? fields.filter(key => key === 'source_type' ? appliedSourceDisplayKey : key === 'status' ? appliedStatusDisplayKey : appliedQuery[key]) : [];
  const appliedValue = key => key === 'status' ? labelStatus(appliedStatusDisplayKey) :
    key === 'source_type' ? receivableSources.includes(appliedSourceDisplayKey) ? receivableFilters.types[appliedSourceDisplayKey] : receivableFilters.unsupported : appliedQuery[key];
  const appliedLabel = key => key === 'created_from' ? timeWords.from : key === 'created_to' ? timeWords.to : words[key];
  const emptyReceivables = appliedCursor ? resultWords?.emptyPage : applied.length ? resultWords?.noMatches : resultWords?.noRecords;
  const table = (items, columns, identify) => <div className="adm-table-wrap"><table className="adm-table"><thead><tr>{columns.map(key => <th key={key} data-adjustment-points={adjustmentFacts && key === 'amount_microcredits' ? true : undefined}>{column(key)}</th>)}{identify && <th>{copy.details}</th>}</tr></thead><tbody>{items.map((item, index) => <tr key={identify ? financeReadIdentity(kind, item) : index}>{columns.map(key => <td key={key} data-label={column(key)} data-adjustment-points={adjustmentFacts && key === 'amount_microcredits' ? true : undefined}>{valueFor(key, item)}</td>)}{identify && <td><button onClick={() => move(current.query, financeReadIdentity(kind, item))}>{copy.details}</button></td>}</tr>)}</tbody></table></div>;
  return <section className="adm-finance-read-view" aria-label={words[kind]}><header><h3>{words[kind]}</h3>{showAdjustmentView && !current.selectedID && <span>{adjustmentResults.view}: {adjustmentStatus ? labelStatus(adjustmentStatus) : words.allStatuses}</span>}<button onClick={refresh}>{copy.refresh}</button></header>
    {visible.error && <p role="alert">{visible.error.status === 403 ? copy.permissions : visible.error.status === 410 ? words.cursorExpired : copy.unavailable}<button onClick={refresh}>{copy.retry}</button></p>}
    {!visible.data && !visible.error && <p role="status">{copy.loading}</p>}
    {adjustmentLookup && <p><small>{adjustmentLookup.relatedHelp}</small></p>}
    {current.selectedID ? <><button onClick={() => move(current.query)}>{words.back}</button><h4>{current.selectedID}</h4>{visible.data && <>
      {visible.data.selected ? <dl className="adm-user-details">{config.columns.map(key => <React.Fragment key={key}><dt>{column(key)}</dt><dd>{valueFor(key, visible.data.selected)}</dd></React.Fragment>)}</dl> : <p>{words.detailUnavailable}</p>}
      {visible.data.allocations && table(visible.data.allocations, ['amount_microcredits', 'created_at'])}
      {kind === 'adjustments' && onViewApproval && route.available.includes('approvals') && <button onClick={() => onViewApproval(null, { approval_command_type: 'WALLET_ADJUSTMENT', approval_target: current.selectedID })}>{copy.approvals}</button>}
    </>}</> : <><form className="adm-filter-row" onSubmit={event => { event.preventDefault(); move({ ...draft, cursor: '' }); }}>
      {fields.map(key => adjustmentLookup && ['wallet_id', 'requested_by'].includes(key) ? <AdminAdjustmentLookup key={key} routeSignature={signature} appliedValue={current.query[key]} client={client} kind={key === 'wallet_id' ? 'wallet' : 'requester'} label={words[key]} words={adjustmentLookup} available={key === 'wallet_id' || canReadRequester} onOpenRelated={key === 'wallet_id' ? openWallet : openRequester} onDirectoryAccess={key === 'requested_by' ? onRequesterDirectoryAccess : undefined} value={draft[key] || ''} onChange={id => setDraft(value => ({ ...value, [key]: id }))} /> : timeWords && key === 'created_to' ? null : timeWords && key === 'created_from' ? <React.Fragment key={key}>{timeRange}</React.Fragment> : <label key={key}>{receivableFilters && key === 'wallet_id' ? <span id={walletHelpID + '-label'}>{words[key]}</span> : words[key]}{adjustmentFacts && key.startsWith('created_') && <small>{adjustmentFacts.inputZone} ({Intl.DateTimeFormat().resolvedOptions().timeZone})</small>}{receivableFilters && key === 'wallet_id' ?
        <><input maxLength={200} value={draft[key] || ''} aria-labelledby={walletHelpID + '-label'} aria-describedby={walletHelpID} onChange={event => setDraft(value => ({ ...value, [key]: event.target.value }))} /><small id={walletHelpID}>{receivableFilters.help}</small></>
        : receivableFilters && key === 'source_type' ? <select value={unsupportedSource ? draft[key] : sourceDisplayKey} onChange={event => setDraft(value => ({ ...value, [key]: event.target.value }))}>
          <option value="">{receivableFilters.all}</option>
          {unsupportedSource && <option disabled value={draft[key]}>{receivableFilters.unsupported}</option>}
          {receivableSources.map(source => <option key={source} value={source}>{receivableFilters.types[source]}</option>)}
        </select> : key === 'status' ? <select value={statusControlValue} aria-invalid={unsupportedAdjustmentDraft || undefined} onChange={event => setDraft(value => ({ ...value, [key]: event.target.value }))}><option value="">{words.allStatuses}</option>{unsupportedAdjustmentDraft && <option value={statusControlValue} disabled>{adjustmentResults.invalidStatus}</option>}{statuses[kind].map(value => <option key={value} value={value}>{labelStatus(value)}</option>)}</select> : <input maxLength={200} step={key.startsWith('created_') ? '0.001' : undefined} type={key.startsWith('created_') ? 'datetime-local' : 'text'} value={draft[key] || ''} onChange={event => setDraft(value => ({ ...value, [key]: event.target.value }))} />}</label>)}
      <label>{words.limit}<select value={draft.limit} onChange={event => setDraft(value => ({ ...value, limit: event.target.value }))}>{['25', '50', '100', '200'].map(value => <option key={value}>{value}</option>)}</select></label><button>{words.search}</button>
    </form>{adjustmentResults && appliedAdjustment.length > 0 && <section className="adm-receivable-applied" aria-label={adjustmentResults.applied}>
      <h4>{adjustmentResults.applied}</h4>
      <dl>{appliedAdjustment.map(key => <div key={key}><dt>{words[key]}</dt><dd>{key === 'status' ? labelStatus(adjustmentStatus) : <code>{adjustmentQuery[key]}</code>}</dd></div>)}</dl>
      <button type="button" onClick={() => move({ limit: current.query.limit, cursor: '' })}>{adjustmentResults.all}</button>
    </section>}{visible.data && <>
      {resultWords && applied.length > 0 && <section className="adm-receivable-applied" aria-label={resultWords.applied}>
        <h4>{resultWords.applied}</h4>
        <dl>{applied.map(key => <div key={key}><dt>{appliedLabel(key)}</dt><dd>{key.startsWith('created_') || key === 'wallet_id' ? <code>{appliedValue(key)}</code> : appliedValue(key)}</dd></div>)}</dl>
        <button type="button" onClick={() => move({ limit: current.query.limit, cursor: '' })}>{resultWords.clear}</button>
      </section>}
      {adjustmentResults && visible.data.items.length === 0 ? <div className="adm-receivable-empty">
        <p role="status">{emptyAdjustments}</p>
        {adjustmentCursor && <button type="button" onClick={() => { setPrevious([]); move({ ...current.query, cursor: '' }); }}>{adjustmentResults.first}</button>}
      </div> : resultWords && visible.data.items.length === 0 ? <div className="adm-receivable-empty">
        <p role="status">{emptyReceivables}</p>
        {appliedCursor && <button type="button" onClick={() => { setPrevious([]); move({ ...current.query, cursor: '' }); }}>{resultWords.first}</button>}
      </div> : <><p>{words.pageCount(visible.data.items.length)}</p>{table(visible.data.items, config.columns, config.id)}{visible.data.items.length === 0 && <p>{copy.empty}</p>}</>}
      <nav><button disabled={adjustmentFirstBatch || !previous.length} onClick={() => { const values = [...previous], cursor = values.pop(); setPrevious(values); move({ ...current.query, cursor }); }}>{copy.previous}</button><button disabled={!visible.data.page.next_cursor} onClick={() => { setPrevious(value => [...value, current.query.cursor]); move({ ...current.query, cursor: visible.data.page.next_cursor }); }}>{copy.next}</button></nav></>}</>}
  </section>;
}
