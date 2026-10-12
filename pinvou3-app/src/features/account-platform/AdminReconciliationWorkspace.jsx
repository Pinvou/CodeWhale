import React, { useEffect, useRef, useState } from 'react';
import { reconciliationCopy } from '../../shared/i18n.js';
import { requireFinanceCollection, requireFinanceID } from './finance-contract.js';
import { RECONCILIATION_ACTIONS, reconciliationCommand, reconciliationDayRange, reconciliationImportItems, reconciliationCaseLabel, reconciliationReadError, reconciliationWriteError, requireReconciliationDate } from './reconciliation-contract.js';
import { loadFinancePage } from './admin-finance-pagination.js';
import { useFinanceOperation } from './useFinanceOperation.js';

function today() {
  const parts = new Intl.DateTimeFormat('en', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(new Date());
  const part = type => parts.find(value => value.type === type)?.value;
  return `${part('year')}-${part('month')}-${part('day')}`;
}

function CaseActions({ client, item, owner, copy, canWrite, onChanged }) {
  const op = useFinanceOperation(owner, { ...copy, permissions: copy.permission, inputInvalid: copy.invalid }, client, `reconciliation-case:${item.case_id}`);
  const [action, setAction] = useState('CLAIM');
  const [fields, setFields] = useState({ reason: '', owner: '', repair: '', review: '' });
  const [invalid, setInvalid] = useState(false);
  const submit = async () => {
    setInvalid(false);
    try {
      const facts = op.pendingFacts || reconciliationCommand(action, fields);
      const result = await op.mutate(facts, async (frozen, key) => {
        try { return await client.reconciliationCaseCommand(item.case_id, frozen, key); }
        catch (error) {
          // The shared hook retains unknown writes even when a retry gets an input error.
          if (error.status === 400 && reconciliationReadError(error) === 'invalid') error.financeInputInvalid = true;
          throw error;
        }
      });
      if (result) onChanged(result);
    } catch { setInvalid(true); }
  };
  const update = name => event => setFields(value => ({ ...value, [name]: event.target.value }));
  const disabled = !canWrite || op.busy || op.unknown;
  const labels = ['claim', 'assign', 'setRepair', 'resolve', 'ignore', 'reopen'];
  return <section className="adm-panel">
    <h3>{item.merchant_order_no} · {reconciliationCaseLabel(item.case_type, copy)}</h3><p>{copy.status}: {item.status}</p>
    <details><summary>{copy.snapshots}</summary>{['provider_snapshot', 'internal_snapshot', 'refund_snapshot', 'ledger_snapshot'].map(key => <pre key={key}>{JSON.stringify(item[key] || {}, null, 2)}</pre>)}</details>
    {canWrite && <><label>{copy.action}<select disabled={disabled} value={action} onChange={event => setAction(event.target.value)}>{RECONCILIATION_ACTIONS.map((value, index) => <option key={value} value={value}>{copy[labels[index]]}</option>)}</select></label>
      {action === 'ASSIGN' && <label>{copy.owner}<input disabled={disabled} value={fields.owner} onChange={update('owner')} /></label>}
      {action === 'SET_REPAIR_ACTION' && <label>{copy.repair}<textarea disabled={disabled} value={fields.repair} onChange={update('repair')} /></label>}
      {['RESOLVE', 'IGNORE'].includes(action) && <label>{copy.review}<textarea disabled={disabled} value={fields.review} onChange={update('review')} /></label>}
      <label>{copy.reason}<textarea disabled={disabled} value={fields.reason} onChange={update('reason')} /></label>
      <button disabled={op.busy} onClick={() => void submit()}>{op.unknown ? copy.retry : copy.submit}</button></>}
    {(invalid || op.error) && <p role="alert">{invalid ? copy.invalid : op.error}</p>}{op.message && <p role="status">{op.message}</p>}
  </section>;
}

function Workspace({ client, me, copy }) {
  const owner = `${me.user_id}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`;
  const permissions = me.platform_permissions || [];
  const canRun = permissions.includes('platform.billing.reconciliation.run');
  const canWrite = permissions.includes('platform.billing.reconciliation.manage');
  const [tab, setTab] = useState('runs');
  const [channel, setChannel] = useState('WECHAT');
  const [date, setDate] = useState(today);
  const [bill, setBill] = useState('[]');
  const [runID, setRunID] = useState('');
  const [status, setStatus] = useState('');
  const filterKey = JSON.stringify([tab, channel, date, runID, status]);
  const op = useFinanceOperation(owner, { ...copy, permissions: copy.permission }, client, `reconciliation-list:${filterKey}`);
  const currentFilter = useRef(filterKey);
  currentFilter.current = filterKey;
  const [page, setPage] = useState(null);
  const [cursor, setCursor] = useState('');
  const [history, setHistory] = useState([]);
  const [selected, setSelected] = useState(null);
  const [invalid, setInvalid] = useState(false);
  const [writing, setWriting] = useState(false);
  const [writeError, setWriteError] = useState('');
  const [writeNotice, setWriteNotice] = useState('');
  // Manual runs have no idempotency-key contract. An unknown result requires
  // reading current runs; another click is an explicit new run.
  const generation = useRef(0);
  useEffect(() => () => { generation.current++; }, []);
  const load = async (next = '') => {
    setInvalid(false);
    try {
      if (tab === 'cases' && runID) requireFinanceID(runID);
      const range = tab === 'runs' ? reconciliationDayRange(date) : null;
      const query = tab === 'runs' ? { channel, ...range, cursor: next } : { run_id: runID, status, cursor: next };
      const result = await op.read(() => loadFinancePage(query, filters => tab === 'runs' ? client.reconciliationRuns(filters) : client.reconciliationCases(filters), () => { if (currentFilter.current === filterKey) { setHistory([]); setCursor(''); } }), reconciliationReadError);
      if (result && currentFilter.current === filterKey) {
        const value = result.value;
        requireFinanceCollection(value);
        value.items.forEach(item => requireFinanceID(tab === 'runs' ? item.reconciliation_run_id : item.case_id));
        setPage({ ...value, tab, filterKey }); setCursor(result.query.cursor);
      }
    } catch { setInvalid(true); }
  };
  useEffect(() => { setPage(null); setSelected(null); setHistory([]); setCursor(''); void load(''); }, [tab, channel, date, runID, status]); // eslint-disable-line react-hooks/exhaustive-deps
  const write = async imported => {
    const epoch = generation.current;
    setInvalid(false); setWriteError(''); setWriteNotice(''); setWriting(true);
    try {
      const facts = { channel, business_date: requireReconciliationDate(date) };
      const result = imported ? await client.importReconciliation({ ...facts, items: reconciliationImportItems(bill) }) : await client.runReconciliation(facts);
      if (epoch !== generation.current) return;
      setRunID(result.reconciliation_run_id); setWriteNotice(copy.saved); await load('');
    } catch (error) {
      if (epoch !== generation.current) return;
      setWriteError(copy[reconciliationWriteError(error)]);
    } finally { if (epoch === generation.current) setWriting(false); }
  };
  const busy = op.busy || writing;
  const visiblePage = page?.filterKey === filterKey ? page : null;
  return <section className="adm-panel"><h2>{copy.title}</h2>
    <nav>{['runs', 'cases'].map(value => <button key={value} disabled={busy} aria-pressed={tab === value} onClick={() => setTab(value)}>{copy[value]}</button>)}</nav>
    {tab === 'runs' ? <><label>{copy.channel}<select disabled={busy} value={channel} onChange={event => setChannel(event.target.value)}>{['WECHAT', 'ALIPAY', 'UNIONPAY'].map(value => <option key={value}>{value}</option>)}</select></label><label>{copy.businessDate}<input type="date" disabled={busy} value={date} onChange={event => setDate(event.target.value)} /></label>
      {canRun && <><button disabled={busy} onClick={() => void write(false)}>{copy.run}</button><label>{copy.importedItems}<textarea disabled={busy} value={bill} onChange={event => setBill(event.target.value)} /></label><button disabled={busy} onClick={() => void write(true)}>{copy.import}</button></>}
    </> : <><label>{copy.runs}<input disabled={busy} value={runID} onChange={event => setRunID(event.target.value)} /></label><label>{copy.status}<select disabled={busy} value={status} onChange={event => setStatus(event.target.value)}><option value="">{copy.all}</option>{['OPEN', 'IN_REVIEW', 'RESOLVED', 'IGNORED'].map(value => <option key={value}>{value}</option>)}</select></label></>}
    <button disabled={busy} onClick={() => { setHistory([]); void load(''); }}>{copy.refresh}</button>
    {(invalid || op.error || writeError) && <p role="alert">{invalid ? copy.invalid : op.error || writeError}</p>}{writeNotice && <p role="status">{writeNotice}</p>}{busy && <p role="status">{copy.loading}</p>}
    {visiblePage && <table><thead><tr><th>{tab === 'runs' ? copy.businessDate : copy.merchant}</th><th>{copy.status}</th><th>{tab === 'runs' ? copy.count : copy.caseType}</th>{tab === 'runs' && <><th>{copy.rows}</th><th>{copy.source}</th></>}<th>{copy.action}</th></tr></thead><tbody>{visiblePage.items.map(item => <tr key={tab === 'runs' ? item.reconciliation_run_id : item.case_id}><td>{tab === 'runs' ? item.business_date : item.merchant_order_no}</td><td>{item.status}</td><td>{tab === 'runs' ? item.case_count : reconciliationCaseLabel(item.case_type, copy)}</td>{tab === 'runs' && <><td>{item.provider_row_count}</td><td>{item.source_mode}</td></>}<td><button disabled={busy} onClick={() => { if (tab === 'runs') { setRunID(item.reconciliation_run_id); setTab('cases'); } else setSelected(item); }}>{copy.cases}</button></td></tr>)}</tbody></table>}
    {visiblePage?.items.length === 0 && <p>{copy.empty}</p>}
    <nav><button disabled={busy || !history.length} onClick={() => { const values = [...history]; const next = values.pop(); setHistory(values); void load(next); }}>{copy.previous}</button><button disabled={busy || !visiblePage?.next_cursor} onClick={() => { setHistory(values => [...values, cursor]); void load(visiblePage.next_cursor); }}>{copy.next}</button></nav>
    {selected && <CaseActions key={selected.case_id} client={client} item={selected} owner={owner} copy={copy} canWrite={canWrite} onChanged={item => { setSelected(item); setPage(current => current && ({ ...current, items: current.items.map(value => value.case_id === item.case_id ? item : value) })); }} />}
  </section>;
}

export function AdminReconciliationWorkspace({ client, me, language }) {
  return <Workspace key={`${me.user_id}:${client.financeSessionEpoch || 0}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`} client={client} me={me} copy={reconciliationCopy[language]} />;
}
