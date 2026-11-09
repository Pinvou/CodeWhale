import React, { useEffect, useRef, useState } from 'react';
import { reconciliationCopy, ledgerRepairCopy, financeCopy } from '../../shared/i18n.js';
import { AdminReconciliationFinancialRepairForm } from './AdminReconciliationFinancialRepairForm.jsx';
import { AdminReconciliationBillImport } from './AdminReconciliationBillImport.jsx';
import './admin-reconciliation-layout.css';
import { AdminReconciliationBatchPicker, ReconciliationBatchSummary } from './AdminReconciliationBatchPicker.jsx';
import { requireFinanceCollection, requireFinanceID } from './finance-contract.js';
import { RECONCILIATION_ACTIONS, reconciliationCommand, reconciliationDayRange, reconciliationCaseLabel, reconciliationReadError, reconciliationWriteError, requireReconciliationDate } from './reconciliation-contract.js';
import { loadFinancePage } from './admin-finance-pagination.js';
import { useFinanceOperation } from './useFinanceOperation.js';

function today() {
  const parts = new Intl.DateTimeFormat('en', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(new Date());
  const part = type => parts.find(value => value.type === type)?.value;
  return `${part('year')}-${part('month')}-${part('day')}`;
}

function CaseActions({ client, item, owner, copy, canWrite, onChanged, canRequestRepair, canReadCase, repairCopy, operationCopy, onViewApproval }) {
  const [repairOpen, setRepairOpen] = useState(false);
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
    {canWrite && <><label>{copy.action}<select disabled={disabled} value={action} onChange={event => setAction(event.target.value)}>{RECONCILIATION_ACTIONS.map((value, index) => <option key={value} value={value} disabled={item.case_type === 'LEDGER_REPAIR_PROOF_UNAVAILABLE' && value === 'IGNORE'}>{copy[labels[index]]}</option>)}</select></label>
      {action === 'ASSIGN' && <label>{copy.owner}<input disabled={disabled} value={fields.owner} onChange={update('owner')} /></label>}
      {action === 'SET_REPAIR_ACTION' && <label>{copy.repair}<textarea disabled={disabled} value={fields.repair} onChange={update('repair')} /></label>}
      {['RESOLVE', 'IGNORE'].includes(action) && <label>{copy.review}<textarea disabled={disabled} value={fields.review} onChange={update('review')} /></label>}
      <label>{copy.reason}<textarea disabled={disabled} value={fields.reason} onChange={update('reason')} /></label>
      <button disabled={op.busy} onClick={() => void submit()}>{op.unknown ? copy.retry : copy.submit}</button></>}
    {canRequestRepair && <>
      <button type="button" disabled={op.busy || op.unknown} onClick={() => setRepairOpen(true)}>{repairCopy.caseRepairStart}</button>
      {repairOpen && <AdminReconciliationFinancialRepairForm client={client} owner={owner} copy={{ ...repairCopy, title: repairCopy.reconciliationRepairTitle }} operationCopy={operationCopy} sourceCaseID={item.case_id} canReadCase={canReadCase} onCreated={onViewApproval} />}
    </>}
    {(invalid || op.error) && <p role="alert">{invalid ? copy.invalid : op.error}</p>}{op.message && <p role="status">{op.message}</p>}
  </section>;
}

function Workspace({ client, me, copy, repairCopy, operationCopy, onViewApproval }) {
  const owner = `${me.user_id}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`;
  const permissions = me.platform_permissions || [];
  const canRun = permissions.includes('platform.billing.reconciliation.run');
  const canWrite = permissions.includes('platform.billing.reconciliation.manage');
  const canRequestRepair = permissions.includes('platform.billing.repairs.request');
  const canReadCase = permissions.includes('platform.billing.reconciliation.read');
  const [tab, setTab] = useState('runs');
  const [channel, setChannel] = useState('WECHAT');
  const [date, setDate] = useState(today);
  const [runID, setRunID] = useState('');
  const [runContext, setRunContext] = useState(null);
  const chooseRun = (id, item) => { setRunContext(previous => item || (previous?.reconciliation_run_id === id ? previous : null)); setRunID(id); };
  const [status, setStatus] = useState('');
  const filterKey = JSON.stringify([tab, channel, date, runID, status]);
  const currentFilter = useRef(filterKey);
  currentFilter.current = filterKey;
  const [page, setPage] = useState(null);
  const [reading, setReading] = useState(null);
  const [cursor, setCursor] = useState('');
  const [history, setHistory] = useState([]);
  const [selected, setSelected] = useState(null);
  const [writing, setWriting] = useState(false);
  const [writeResult, setWriteResult] = useState(null);
  // Manual runs have no idempotency-key contract. An unknown result requires
  // reading current runs; another click is an explicit new run.
  const generation = useRef(0);
  const readGeneration = useRef(0), controller = useRef(null), inWrite = useRef(false);
  useEffect(() => () => { generation.current++; controller.current?.abort(); }, []);
  const load = async (next = '') => {
    controller.current?.abort();
    const active = new AbortController(); controller.current = active;
    const stamp = ++readGeneration.current;
    const isCurrent = () => stamp === readGeneration.current && currentFilter.current === filterKey && !active.signal.aborted;
    setReading({ filterKey, stage: 'loading' });
    let requested = false;
    try {
      if (tab === 'cases' && runID) requireFinanceID(runID);
      const range = tab === 'runs' ? reconciliationDayRange(date) : null;
      const query = tab === 'runs' ? { channel, ...range, cursor: next } : { run_id: runID, status, cursor: next };
      requested = true;
      const result = await loadFinancePage(query, filters => tab === 'runs' ? client.reconciliationRuns(filters, active.signal) : client.reconciliationCases(filters, active.signal), () => { if (isCurrent()) { setHistory([]); setCursor(''); } });
      if (isCurrent()) {
        const value = result.value;
        requireFinanceCollection(value);
        value.items.forEach(item => requireFinanceID(tab === 'runs' ? item.reconciliation_run_id : item.case_id));
        setPage({ ...value, tab, filterKey }); setCursor(result.query.cursor);
        setReading({ filterKey, stage: 'ready' });
      }
    } catch (error) {
      if (!isCurrent()) return;
      const localInput = !requested && ['INVALID_RECONCILIATION_INPUT', 'INVALID_FINANCE_ACK'].includes(error?.message);
      setReading({ filterKey, stage: 'error', code: localInput ? 'invalid' : reconciliationReadError(error) });
    }
  };
  useEffect(() => {
    setPage(null); setSelected(null); setHistory([]); setCursor(''); void load('');
    return () => { readGeneration.current++; controller.current?.abort(); };
  }, [tab, channel, date, runID, status]); // eslint-disable-line react-hooks/exhaustive-deps
  const write = async (imported, items) => {
    if (!canRun || inWrite.current) return;
    inWrite.current = true;
    const epoch = generation.current;
    setWriteResult(null); setWriting(true);
    const scope = { channel, business_date: date };
    try {
      const facts = { channel, business_date: requireReconciliationDate(date) };
      const result = imported ? await client.importReconciliation({ ...facts, items }) : await client.runReconciliation(facts);
      if (epoch !== generation.current) return;
      setWriteResult({ ...scope, kind: 'saved', id: result.reconciliation_run_id });
      if (result.reconciliation_run_id === runID) { setHistory([]); void load(''); }
      else setRunID(result.reconciliation_run_id);
    } catch (error) {
      if (epoch !== generation.current) return;
      const kind = reconciliationWriteError(error);
      let id = '';
      if (kind === 'runUnknown') {
        try { id = requireFinanceID(error.details?.reconciliation_run_id); } catch { /* An unknown result may have no usable ID. */ }
      }
      setWriteResult({ ...scope, kind, id });
    } finally {
      if (epoch === generation.current) { inWrite.current = false; setWriting(false); }
    }
  };
  const currentRead = reading?.filterKey === filterKey ? reading : null;
  const busy = currentRead?.stage === 'loading' || writing;
  const visiblePage = currentRead?.stage === 'ready' && page?.filterKey === filterKey ? page : null;
  const refresh = () => { setHistory([]); void load(''); };
  const checkResult = () => {
    if (writeResult.channel !== channel || writeResult.business_date !== date) {
      setChannel(writeResult.channel); setDate(writeResult.business_date);
    } else refresh();
  };
  return <section className="adm-panel adm-reconciliation-workspace"><h2>{copy.title}</h2>
    <nav className="adm-tabs adm-reconciliation-navigation" aria-label={copy.reconciliationViews}>{['runs', 'cases'].map(value => <button type="button" key={value} disabled={busy} aria-pressed={tab === value} onClick={() => setTab(value)}>{copy[value]}</button>)}</nav>
    {tab === 'cases' && canReadCase && <>
      <ReconciliationBatchSummary id={runID} item={runContext} copy={copy} />
      <AdminReconciliationBatchPicker {...{client,copy}} initialChannel={runContext?.reconciliation_run_id === runID ? runContext.channel : channel} initialDate={runContext?.reconciliation_run_id === runID ? runContext.business_date : date} selectedID={runID} onChoose={chooseRun} disabled={writing} />
    </>}
    <div className="adm-reconciliation-toolbar" role="group" aria-label={copy.reconciliationFilters}>
      {tab === 'runs' ? <><label>{copy.channel}<select disabled={busy} value={channel} onChange={event => setChannel(event.target.value)}>{['WECHAT', 'ALIPAY', 'UNIONPAY'].map(value => <option key={value}>{value}</option>)}</select></label><label>{copy.businessDate}<input type="date" disabled={busy} value={date} onChange={event => setDate(event.target.value)} /></label></> :
      <><label>{copy.status}<select disabled={busy} value={status} onChange={event => setStatus(event.target.value)}><option value="">{copy.all}</option>{['OPEN', 'IN_REVIEW', 'RESOLVED', 'IGNORED'].map(value => <option key={value}>{value}</option>)}</select></label></>}
      <button type="button" disabled={busy} onClick={refresh}>{copy.refresh}</button>
    </div>
    {tab === 'runs' && canRun && <>
      <section className="adm-reconciliation-run"><h3>{copy.reconciliationRunTitle}</h3><p>{copy.batchRunHelp}</p>{Boolean(visiblePage?.items.length) && <p>{copy.batchExisting}</p>}<button type="button" disabled={busy} onClick={() => void write(false)}>{copy.run}</button></section>
      <AdminReconciliationBillImport {...{channel,date,copy}} disabled={busy} onImport={items => write(true, items)} />
    </>}
    {tab === 'runs' && writeResult && <div className="adm-inline-message" role={writeResult.kind === 'saved' ? 'status' : 'alert'}>
      <strong>{copy[writeResult.kind]}</strong><p>{writeResult.channel} · {writeResult.business_date}{writeResult.id && <> · <code>{writeResult.id}</code></>}</p>
      {writeResult.kind === 'runUnknown' && <button type="button" disabled={busy} onClick={checkResult}>{copy.batchCheck}</button>}
    </div>}
    {currentRead?.stage === 'error' && <p role="alert">{copy[currentRead.code === 'permissions' ? 'permission' : currentRead.code] || copy.unavailable}</p>}
    <p role="status">{writing ? copy.batchSubmitting : !currentRead ? copy.batchNotRead : currentRead.stage === 'loading' ? copy.loading : visiblePage?.items.length === 0 ? copy.empty : ''}</p>
    {visiblePage && (
      // biome-ignore lint/a11y/noNoninteractiveTabindex: The named overflow region must remain keyboard-scrollable.
      <div className="adm-reconciliation-table" role="region" aria-label={copy[tab]} tabIndex={0}><table><thead><tr><th>{tab === 'runs' ? copy.businessDate : copy.merchant}</th>{tab === 'runs' && <><th>{copy.channel}</th><th>{copy.batchID}</th></>}<th>{copy.status}</th><th>{tab === 'runs' ? copy.count : copy.caseType}</th>{tab === 'runs' && <><th>{copy.rows}</th><th>{copy.source}</th></>}<th>{copy.action}</th></tr></thead><tbody>{visiblePage.items.map(item => <tr key={tab === 'runs' ? item.reconciliation_run_id : item.case_id}><td>{tab === 'runs' ? item.business_date : item.merchant_order_no}</td>{tab === 'runs' && <><td>{item.channel}</td><td><code>{item.reconciliation_run_id}</code></td></>}<td>{tab === 'runs' ? copy.batchStatuses[item.status] || item.status : item.status}</td><td>{tab === 'runs' ? item.case_count : reconciliationCaseLabel(item.case_type, copy)}</td>{tab === 'runs' && <><td>{item.provider_row_count}</td><td>{item.source_mode}</td></>}<td><button type="button" disabled={busy} onClick={() => { if (tab === 'runs') { chooseRun(item.reconciliation_run_id, item); setTab('cases'); } else setSelected(item); }}>{copy.cases}</button></td></tr>)}</tbody></table></div>)}
    <nav className="adm-reconciliation-pagination" aria-label={copy.reconciliationPages}><button type="button" disabled={busy || !history.length} onClick={() => { const values = [...history]; const next = values.pop(); setHistory(values); void load(next); }}>{copy.previous}</button><button type="button" disabled={busy || !visiblePage?.next_cursor} onClick={() => { setHistory(values => [...values, cursor]); void load(visiblePage.next_cursor); }}>{copy.next}</button></nav>
    {selected && <CaseActions key={selected.case_id} client={client} item={selected} owner={owner} copy={copy} canWrite={canWrite} canRequestRepair={canRequestRepair} canReadCase={canReadCase} repairCopy={repairCopy} operationCopy={operationCopy} onViewApproval={onViewApproval} onChanged={item => { setSelected(item); setPage(current => current && ({ ...current, items: current.items.map(value => value.case_id === item.case_id ? item : value) })); }} />}
  </section>;
}

export function AdminReconciliationWorkspace({ client, me, language, onViewApproval }) {
  return <Workspace key={`${me.user_id}:${client.financeSessionEpoch || 0}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`} client={client} me={me} copy={reconciliationCopy[language]} repairCopy={ledgerRepairCopy[language]} operationCopy={financeCopy[language]} onViewApproval={onViewApproval} />;
}
