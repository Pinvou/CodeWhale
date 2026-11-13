import React, { useEffect, useRef, useState } from 'react';
import { reconciliationCopy, ledgerRepairCopy, financeCopy } from '../../shared/i18n.js';
import { AdminReconciliationFinancialRepairForm } from './AdminReconciliationFinancialRepairForm.jsx';
import { AdminReconciliationBillImport } from './AdminReconciliationBillImport.jsx';
import './admin-reconciliation-layout.css';
import { AdminReconciliationBatchPicker, ReconciliationBatchSummary } from './AdminReconciliationBatchPicker.jsx';
import { ReconciliationCaseAmounts, ReconciliationCaseOwner, ReconciliationCaseEvidence } from './AdminReconciliationCaseEvidence.jsx';
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
    <p>{copy.caseOwner}: <ReconciliationCaseOwner id={item.owner_id} copy={copy} /></p>
    <ReconciliationCaseEvidence item={item} copy={copy} />
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
  const [caseScope, setCaseScope] = useState('none');
  const [runContext, setRunContext] = useState(null);
  const chooseRun = (id, item) => {
    setRunContext(previous => item || (previous?.reconciliation_run_id === id ? previous : null));
    setRunID(id); setCaseScope('batch');
  };
  const clearRun = scope => { setRunContext(null); setRunID(''); setCaseScope(scope); };
  const [status, setStatus] = useState('');
  const filterKey = JSON.stringify(tab === 'runs' ? [tab, canReadCase, channel, date] : [tab, canReadCase, caseScope, runID, status]);
  const currentFilter = useRef({ key: filterKey });
  if (currentFilter.current.key !== filterKey) currentFilter.current = { key: filterKey };
  const readScope = currentFilter.current;
  const canQuery = canReadCase && (tab === 'runs' || caseScope !== 'none');
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
    if (!canQuery || currentFilter.current !== readScope) return;
    controller.current?.abort();
    const active = new AbortController(); controller.current = active;
    const stamp = ++readGeneration.current;
    const isCurrent = () => stamp === readGeneration.current && currentFilter.current === readScope && !active.signal.aborted;
    setReading({ readScope, stage: 'loading' });
    let requested = false, retryCursor = next;
    try {
      if (tab === 'cases' && caseScope === 'batch') requireFinanceID(runID);
      const range = tab === 'runs' ? reconciliationDayRange(date) : null;
      const query = tab === 'runs' ? { channel, ...range, cursor: next } : { run_id: caseScope === 'batch' ? runID : '', status, cursor: next };
      requested = true;
      const result = await loadFinancePage(query, filters => {
        if (!isCurrent()) throw Object.assign(new Error('stale reconciliation query'), { name: 'AbortError' });
        return tab === 'runs' ? client.reconciliationRuns(filters, active.signal) : client.reconciliationCases(filters, active.signal);
      }, () => { if (isCurrent()) { retryCursor = ''; setHistory([]); setCursor(''); } });
      if (isCurrent()) {
        const value = result.value;
        requireFinanceCollection(value);
        value.items.forEach(item => requireFinanceID(tab === 'runs' ? item.reconciliation_run_id : item.case_id));
        setPage({ ...value, tab, readScope }); setCursor(result.query.cursor);
        setReading({ readScope, stage: 'ready' });
      }
    } catch (error) {
      if (!isCurrent()) return;
      const localInput = !requested && ['INVALID_RECONCILIATION_INPUT', 'INVALID_FINANCE_ACK'].includes(error?.message);
      setReading({ readScope, stage: 'error', retryCursor, code: localInput ? 'invalid' : reconciliationReadError(error) });
    }
  };
  useEffect(() => {
    setPage(null); setSelected(null); setHistory([]); setCursor(''); void load('');
    return () => { readGeneration.current++; controller.current?.abort(); };
  }, [readScope]); // eslint-disable-line react-hooks/exhaustive-deps
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
      setHistory([]); void load('');
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
  const currentRead = reading?.readScope === readScope ? reading : null;
  const busy = currentRead?.stage === 'loading' || writing;
  const visiblePage = currentRead?.stage === 'ready' && page?.readScope === readScope ? page : null;
  const selectedHere = selected?.readScope === readScope ? selected : null;
  const caseEmpty = tab === 'cases' && visiblePage?.items.length === 0 ?
    (cursor || visiblePage.next_cursor ? copy.caseNoPageCases : status ? copy.caseNoStatusMatches : caseScope === 'all' ? copy.caseNoAllCases :
      runContext?.reconciliation_run_id === runID && runContext.status === 'COMPLETED' ? copy.caseNoBatchCases : copy.caseNoCasesUnconfirmed) : '';
  const refresh = () => { setHistory([]); void load(''); };
  const checkResult = () => {
    if (writeResult.channel !== channel || writeResult.business_date !== date) {
      setChannel(writeResult.channel); setDate(writeResult.business_date);
    } else refresh();
  };
  return <section className="adm-panel adm-reconciliation-workspace"><h2>{copy.title}</h2>
    <nav className="adm-tabs adm-reconciliation-navigation" aria-label={copy.reconciliationViews}>{['runs', 'cases'].map(value => <button type="button" key={value} disabled={writing} aria-pressed={tab === value} onClick={() => setTab(value)}>{copy[value]}</button>)}</nav>
    {tab === 'cases' && canReadCase && <>
      <div className="adm-case-scope" role="group" aria-label={copy.caseScope}>
        <button type="button" disabled={writing} aria-pressed={caseScope === 'all'} onClick={() => clearRun('all')}>{copy.caseScopeAll}</button>
        {caseScope !== 'none' && <button type="button" disabled={writing} onClick={() => clearRun('none')}>{copy.caseScopeClear}</button>}
      </div>
      {caseScope === 'batch' ? <ReconciliationBatchSummary id={runID} item={runContext} copy={copy} /> : caseScope === 'all' ? <p>{copy.caseScopeAllActive}</p> : null}
      <AdminReconciliationBatchPicker {...{client,copy}} initialChannel={runContext?.reconciliation_run_id === runID ? runContext.channel : channel} initialDate={runContext?.reconciliation_run_id === runID ? runContext.business_date : date} selectedID={runID} onChoose={chooseRun} disabled={writing} />
    </>}
    <div className="adm-reconciliation-toolbar" role="group" aria-label={copy.reconciliationFilters}>
      {tab === 'runs' ? <><label>{copy.channel}<select disabled={busy} value={channel} onChange={event => setChannel(event.target.value)}>{['WECHAT', 'ALIPAY', 'UNIONPAY'].map(value => <option key={value}>{value}</option>)}</select></label><label>{copy.businessDate}<input type="date" disabled={busy} value={date} onChange={event => setDate(event.target.value)} /></label></> :
      <><label>{copy.status}<select disabled={writing || !canQuery} value={status} onChange={event => setStatus(event.target.value)}><option value="">{copy.all}</option>{['OPEN', 'IN_REVIEW', 'RESOLVED', 'IGNORED'].map(value => <option key={value}>{value}</option>)}</select></label></>}
      <button type="button" disabled={busy || !canQuery} onClick={refresh}>{copy.refresh}</button>
    </div>
    {tab === 'runs' && canRun && <>
      <section className="adm-reconciliation-run"><h3>{copy.reconciliationRunTitle}</h3><p>{copy.batchRunHelp}</p>{Boolean(visiblePage?.items.length) && <p>{copy.batchExisting}</p>}<button type="button" disabled={busy} onClick={() => void write(false)}>{copy.run}</button></section>
      <AdminReconciliationBillImport {...{channel,date,copy}} disabled={busy} onImport={items => write(true, items)} />
    </>}
    {tab === 'runs' && writeResult && <div className="adm-inline-message" role={writeResult.kind === 'saved' ? 'status' : 'alert'}>
      <strong>{copy[writeResult.kind]}</strong><p>{writeResult.channel} · {writeResult.business_date}{writeResult.id && <> · <code>{writeResult.id}</code></>}</p>
      {writeResult.kind === 'runUnknown' && <button type="button" disabled={busy} onClick={checkResult}>{copy.batchCheck}</button>}
    </div>}
    {currentRead?.stage === 'error' && <div className="adm-case-query-state"><p role="alert">{copy[currentRead.code === 'permissions' ? 'permission' : currentRead.code] || copy.unavailable}</p><button type="button" disabled={writing} onClick={() => void load(currentRead.retryCursor)}>{copy.caseQueryRetry}</button></div>}
    <div className="adm-case-query-state">
      <p role="status">{writing ? copy.batchSubmitting : !canReadCase ? copy.permission : !canQuery ? copy.caseScopeNone : !currentRead ? copy.batchNotRead : currentRead.stage === 'loading' ? copy.loading : caseEmpty || (tab === 'runs' && visiblePage?.items.length === 0 ? copy.empty : '')}</p>
      {caseEmpty && status && <button type="button" onClick={() => setStatus('')}>{copy.caseFilterClear}</button>}
    </div>
    {visiblePage && (
      // biome-ignore lint/a11y/noNoninteractiveTabindex: The named overflow region must remain keyboard-scrollable.
      <div className={tab === 'cases' ? "adm-reconciliation-table adm-reconciliation-cases" : "adm-reconciliation-table"} role="region" aria-label={copy[tab]} tabIndex={0}><table><thead><tr><th>{tab === 'runs' ? copy.businessDate : copy.merchant}</th>{tab === 'runs' && <><th>{copy.channel}</th><th>{copy.batchID}</th></>}<th>{copy.status}</th><th>{tab === 'runs' ? copy.count : copy.caseType}</th>{tab === 'runs' && <><th>{copy.rows}</th><th>{copy.source}</th></>}{tab === 'cases' && <><th>{copy.caseMoney}</th><th>{copy.caseOwner}</th></>}<th>{copy.action}</th></tr></thead><tbody>{visiblePage.items.map(item => <tr key={tab === 'runs' ? item.reconciliation_run_id : item.case_id}><td>{tab === 'runs' ? item.business_date : item.merchant_order_no}</td>{tab === 'runs' && <><td>{item.channel}</td><td><code>{item.reconciliation_run_id}</code></td></>}<td>{tab === 'runs' ? copy.batchStatuses[item.status] || item.status : item.status}</td><td>{tab === 'runs' ? item.case_count : reconciliationCaseLabel(item.case_type, copy)}</td>{tab === 'runs' && <><td>{item.provider_row_count}</td><td>{item.source_mode}</td></>}{tab === 'cases' && <><td><ReconciliationCaseAmounts item={item} copy={copy} /></td><td><ReconciliationCaseOwner id={item.owner_id} copy={copy} /></td></>}<td><button type="button" disabled={busy} onClick={() => { if (tab === 'runs') { chooseRun(item.reconciliation_run_id, item); setTab('cases'); } else setSelected({ item, readScope }); }}>{tab === 'runs' ? copy.cases : copy.caseDetails}</button></td></tr>)}</tbody></table></div>)}
    <nav className="adm-reconciliation-pagination" aria-label={copy.reconciliationPages}><button type="button" disabled={busy || !history.length} onClick={() => { const values = [...history]; const next = values.pop(); setHistory(values); void load(next); }}>{copy.previous}</button><button type="button" disabled={busy || !visiblePage?.next_cursor} onClick={() => { setHistory(values => [...values, cursor]); void load(visiblePage.next_cursor); }}>{copy.next}</button></nav>
    {selectedHere && <CaseActions key={selectedHere.item.case_id} client={client} item={selectedHere.item} owner={owner} copy={copy} canWrite={canWrite} canRequestRepair={canRequestRepair} canReadCase={canReadCase} repairCopy={repairCopy} operationCopy={operationCopy} onViewApproval={onViewApproval} onChanged={item => {
      if (currentFilter.current !== selectedHere.readScope) return;
      setSelected(current => current === selectedHere ? { ...current, item } : current);
      setPage(current => current?.readScope === selectedHere.readScope ? { ...current, items: current.items.map(value => value.case_id === item.case_id ? item : value) } : current);
    }} />}
  </section>;
}

export function AdminReconciliationWorkspace({ client, me, language, onViewApproval }) {
  return <Workspace key={`${me.user_id}:${client.financeSessionEpoch || 0}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`} client={client} me={me} copy={reconciliationCopy[language]} repairCopy={ledgerRepairCopy[language]} operationCopy={financeCopy[language]} onViewApproval={onViewApproval} />;
}
