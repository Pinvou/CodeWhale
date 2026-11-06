import React, { useEffect, useId, useRef, useState } from 'react';
import {
  financialRepairInputs, financialRepairPrecheckBody, financialRepairDateCanUseCalendar,
  financialRepairProjectionRows, financialRepairDraftFromProjection,
} from './admin-financial-repair-form.js';
import { useFinanceOperation } from './useFinanceOperation.js';
import { requireFinanceID } from './finance-contract.js';
import { reconciliationRepairHost } from './reconciliation-repair-context.js';
import { createAndSubmitFreshGovernedApproval } from './admin-governed-approval-workflow.js';

// The repair page never asks the operator for amounts the owner already knows:
// the precheck returns the frozen projection and the draft copies it verbatim.
// The only operator inputs are the target references, the free-text reason and
// the request key.
const reconciliationRepairCommandTypes = ['RECONCILIATION_FINANCIAL_REPAIR'];

export function AdminReconciliationFinancialRepairForm({ client, copy, onCreated, owner = '', operationCopy = copy, commandTypes = reconciliationRepairCommandTypes, sourceCaseID = '', canReadCase = false }) {
  const host = reconciliationRepairHost(sourceCaseID);
  const [commandType, setCommandType] = useState(commandTypes[0]);
  const [values, setValues] = useState(() => host.initialValues);
  const [caseContext, setCaseContext] = useState(null);
  const [contextError, setContextError] = useState('');
  const [manualBusinessDate, setManualBusinessDate] = useState(false);
  const dateHintID = useId();
  const [facts, setFacts] = useState(null);
  const [projection, setProjection] = useState(null);
  const [reason, setReason] = useState('');
  const [requestKey, setRequestKey] = useState('');
  const [busy, setBusy] = useState('');
  const [failure, setFailure] = useState('');
  const [created, setCreated] = useState('');
  const [needsAuthentication, setNeedsAuthentication] = useState(false);
  const factsRevision = useRef(0);
  const mounted = useRef(false);
  const currentClient = useRef(client);
  currentClient.current = client;
  const operation = useFinanceOperation(owner, operationCopy, client, host.scope);
  const reconciliationRepair = commandType === 'RECONCILIATION_FINANCIAL_REPAIR';
  const governed = reconciliationRepair;
  const locked = busy !== '' || operation.busy || operation.unknown;
  const pending = operation.pendingFacts;
  const displayedFacts = pending ? financialRepairProjectionRows(pending.projection) : facts;
  const displayedBusinessDate = pending ? String(pending.draft.normalized_command.business_date) : values.business_date || '';
  const sourceIdentity = JSON.stringify([owner, client.financeSessionEpoch || 0, canReadCase, values.target_resource_id || '']);
  const currentSource = useRef(sourceIdentity);
  currentSource.current = sourceIdentity;
  const visibleContext = !pending && canReadCase && caseContext?.client === client && caseContext.identity === sourceIdentity ? caseContext.value : null;
  const activeProjection = pending?.projection || projection;
  const matchingFacts = displayedFacts && activeProjection?.command_type === 'RECONCILIATION_FINANCIAL_REPAIR' ? [
    ['reconciliation_case_id', activeProjection.target_resource_id],
    ['run_id', activeProjection.critical_resource_versions.run_id],
    ['payment_evidence_id', activeProjection.normalized_command.payment_evidence_id],
    ['business_date', activeProjection.normalized_command.business_date],
    ['amount_fen', String(activeProjection.financial_snapshot.amount_fen)],
    ['currency', activeProjection.financial_snapshot.currency],
    ['provider_transaction_id', activeProjection.financial_snapshot.provider_transaction_id],
  ] : null;
  const calendarAvailable = financialRepairDateCanUseCalendar(displayedBusinessDate);
  const manualDateMode = manualBusinessDate || !calendarAvailable;

  useEffect(() => {
    mounted.current = true;
    setNeedsAuthentication(false);
    setFacts(null);
    setProjection(null);
    setCaseContext(null);
    setContextError('');
    setBusy((current) => current === 'facts' || current === 'context' ? '' : current);
    return () => {
      mounted.current = false;
      factsRevision.current += 1;
    };
  }, [client, owner, client.financeSessionEpoch, canReadCase]);

  const fields = financialRepairInputs[commandType];
  const label = (field) => (field.key === 'target_resource_id' ? copy.targets[commandType] : copy.fields[field.key]);
  const setValue = (key, value) => {
    factsRevision.current += 1;
    setBusy((current) => current === 'facts' || current === 'context' ? '' : current);
    setValues((current) => ({ ...current, [key]: value }));
    if (key === 'target_resource_id') { setCaseContext(null); setContextError(''); }
    setFacts(null);
    setProjection(null);
    setFailure('');
    setCreated('');
  };
  const changeCommand = (next) => {
    factsRevision.current += 1;
    setBusy((current) => current === 'facts' || current === 'context' ? '' : current);
    setCommandType(next);
    setValues(host.initialValues);
    setCaseContext(null);
    setContextError('');
    setFacts(null);
    setProjection(null);
    setFailure('');
    setCreated('');
  };

  const readCaseContext = async () => {
    if (!canReadCase || locked || pending) return;
    let caseID;
    try { caseID = requireFinanceID(String(values.target_resource_id || '').toLowerCase()); }
    catch { setContextError(copy.invalidInput); return; }
    const identity = sourceIdentity;
    const revision = ++factsRevision.current;
    const live = () => mounted.current && currentClient.current === client && currentSource.current === identity && factsRevision.current === revision;
    setBusy('context'); setContextError(''); setCaseContext(null);
    setFacts(null); setProjection(null); setFailure(''); setCreated('');
    try {
      const value = await client.reconciliationRepairContext(caseID);
      if (live()) setCaseContext({ identity, client, value });
    } catch (error) {
      if (live()) setContextError(error?.status === 403 ? copy.caseContextDenied : copy.caseContextUnavailable);
    } finally { if (live()) setBusy(''); }
  };

  const readFacts = async (event) => {
    event.preventDefault();
    const prepared = financialRepairPrecheckBody(commandType, values);
    if (prepared.error) {
      setProjection(null);
      setFacts(null);
      setFailure(copy.invalidInput);
      return;
    }
    const revision = ++factsRevision.current;
    const live = () => mounted.current && factsRevision.current === revision && currentClient.current === client;
    setBusy('facts');
    setNeedsAuthentication(false);
    setFailure('');
    setCreated('');
    try {
      const answer = await client.approvalCommandPrecheck(prepared.body);
      if (!live()) return;
      const rendered = financialRepairProjectionRows(answer);
      if (!rendered) throw Object.assign(new Error('unusable projection'), { code: 'FINANCIAL_REPAIR_PRECHECK_INVALID' });
      if (reconciliationRepair && (answer.command_type !== commandType || answer.target_resource_id !== prepared.body.target_resource_id ||
        answer.normalized_command.reconciliation_case_id !== prepared.body.target_resource_id ||
        answer.normalized_command.payment_evidence_id !== prepared.body.payment_evidence_id ||
        answer.normalized_command.business_date !== prepared.body.business_date)) {
        throw Object.assign(new Error('owner projection changed the reconciliation evidence'), { code: 'FINANCIAL_REPAIR_PRECHECK_INVALID' });
      }
      setProjection(answer);
      setFacts(rendered);
    } catch (error) {
      if (!live()) return;
      setProjection(null);
      setFacts(null);
      if (error?.code === 'RECENT_AUTHENTICATION_REQUIRED' || error?.status === 401) setNeedsAuthentication(true);
      setFailure(error?.code === 'APPROVAL_PRECHECK_UNAVAILABLE' ? copy.factsUnavailable : copy.factsFailed);
    } finally {
      if (live()) setBusy('');
    }
  };

  const createDraft = async (event) => {
    event.preventDefault();
    if (governed) {
      const prepared = pending || { ...financialRepairDraftFromProjection(projection, reason, requestKey), projection };
      if (!prepared.draft) { setFailure(copy.invalidDraft); return; }
      const recovering = operation.unknown;
      const revision = factsRevision.current;
      const live = () => mounted.current && factsRevision.current === revision && currentClient.current === client;
      setNeedsAuthentication(false);
      const result = await operation.mutate(prepared, async current => {
        try { return await createAndSubmitFreshGovernedApproval(client, current.draft); }
        catch (error) {
          // Inspect the original denial before preserving a previous unknown
          // write with COMMAND_RESULT_UNKNOWN; the facts/key stay unchanged.
          if (live() && (error?.code === 'RECENT_AUTHENTICATION_REQUIRED' || error?.status === 401)) setNeedsAuthentication(true);
          // A denied replay cannot prove that an earlier unknown write did not
          // commit. Keep the original facts/key until authority confirms it.
          if (recovering && !error.finalResult) throw Object.assign(error, { code: 'COMMAND_RESULT_UNKNOWN' });
          throw error;
        }
      });
      if (result) {
        setCreated(result.approval_request_id); setProjection(null); setFacts(null);
        setValues(host.initialValues); setCaseContext(null); setContextError('');
        setReason(''); setRequestKey(''); onCreated?.(result.approval_request_id);
      }
      return;
    }

  };

  return <section className="adm-financial-repair" aria-label={copy.title}>
    <h3>{copy.title}</h3>
    <p>{copy.reconciliationLead}</p>
    <p>{copy.ledgerEffectNone}</p>
    {reconciliationRepair && <small className="adm-financial-repair-code">{copy.fields.ledger_effect}: <code>NONE</code></small>}
    {governed && operation.error && <p role="alert">{operation.error}</p>}
    {needsAuthentication && <div className="adm-reconciliation-repair-reauthentication"><p>{copy.writeOffRecentAuthenticationRequired}</p><button type="button" disabled={busy !== '' || operation.busy} onClick={() => client.reauthenticate('/admin.html')}>{copy.reauthenticate}</button></div>}
    {visibleContext && <section className="adm-financial-repair-facts" aria-label={copy.caseContextTitle}>
      <h4>{copy.caseContextTitle}</h4><p>{copy.caseContextHelp}</p>
      <dl>{['case_id', 'reconciliation_run_id', 'case_type', 'status', 'case_version', 'provider_id', 'provider_version', 'channel', 'business_date', 'merchant_order_no'].map(key => <div key={key}><dt>{copy.caseContextLabels[key]}</dt><dd><code>{visibleContext[key]}</code></dd></div>)}
        <div><dt>{copy.caseContextLabels.amount_fen}</dt><dd>{visibleContext.snapshot_available ? <code>{visibleContext.amount_fen}</code> : copy.caseContextMissing}</dd></div>
        <div><dt>{copy.caseContextLabels.currency}</dt><dd><code>{visibleContext.currency || '—'}</code></dd></div>
        <div><dt>{copy.caseContextLabels.provider_transaction_id}</dt><dd><code>{visibleContext.provider_transaction_id || '—'}</code></dd></div>
      </dl>
    </section>}
    {contextError && <p role="alert">{contextError}</p>}
    {canReadCase && sourceCaseID && !pending && <>
      {!visibleContext && <p>{copy.caseContextHelp}</p>}
      <button type="button" disabled={locked} onClick={() => void readCaseContext()}>{busy === 'context' ? copy.caseContextReading : copy.caseContextRead}</button>
    </>}
    <form className="adm-financial-repair-target" onSubmit={readFacts}>
      <label>{copy.command}
        <select disabled={governed && locked} value={commandType} onChange={(event) => changeCommand(event.target.value)}>
          {commandTypes.map((command) => <option key={command} value={command}>{command === 'RECONCILIATION_FINANCIAL_REPAIR' ? copy.reconciliationRepairTitle : command}</option>)}
        </select>
        {reconciliationRepair && <small className="adm-financial-repair-code"><code>{commandType}</code></small>}
      </label>
      {fields.map((field) => <label key={field.key}>{label(field)}
        <input disabled={governed && locked} readOnly={host.targetLocked && field.key === 'target_resource_id'} required value={pending ? String(field.key === 'target_resource_id' ? pending.draft.target_resource_id : pending.draft.normalized_command[field.key]) : values[field.key] || ''} inputMode={field.kind === 'integer' ? 'numeric' : 'text'}
          type={field.kind === 'date' ? manualDateMode ? 'text' : 'date' : undefined}
          aria-describedby={field.kind === 'date' ? dateHintID : host.targetLocked && field.key === 'target_resource_id' ? `${dateHintID}-case` : undefined}
          onChange={(event) => setValue(field.key, event.target.value)} />
        {field.kind === 'date' && <small id={dateHintID}>{copy.businessDateHint}</small>}
        {host.targetLocked && field.key === 'target_resource_id' && <small id={`${dateHintID}-case`}>{copy.caseTargetLocked}</small>}
      </label>)}
      <button type="submit" disabled={locked}>{busy === 'facts' ? copy.reading : copy.readFacts}</button>
      {canReadCase && !sourceCaseID && <button type="button" disabled={locked || Boolean(pending)} onClick={() => void readCaseContext()}>{busy === 'context' ? copy.caseContextReading : copy.caseContextRead}</button>}
      {reconciliationRepair && <div className="adm-reconciliation-date-mode">
        <button type="button" disabled={locked || Boolean(pending) || manualDateMode && !calendarAvailable}
          aria-describedby={manualDateMode && !calendarAvailable ? `${dateHintID}-mode` : undefined}
          onClick={() => setManualBusinessDate(!manualDateMode)}>{manualDateMode ? copy.dateUseCalendar : copy.dateManualEntry}</button>
        {manualDateMode && !calendarAvailable && <small id={`${dateHintID}-mode`}>{copy.dateCalendarUnavailableHelp}</small>}
      </div>}
    </form>
    {failure && <p role="alert">{failure}</p>}
    {created && <p role="status">{copy.createdHint} <code>{created}</code></p>}
    {matchingFacts && <section className="adm-financial-repair-facts" aria-label={copy.evidenceMatchTitle}>
      <h4>{copy.evidenceMatchTitle}</h4><p>{copy.evidenceMatchHelp}</p>
      <dl>{matchingFacts.map(([key, value]) => <div key={key}><dt>{copy.caseContextLabels[key] || copy.fields[key]}</dt><dd><code>{value}</code></dd></div>)}</dl>
    </section>}
    {displayedFacts && <div className="adm-financial-repair-facts">
      <h4>{copy.frozenFacts}</h4>
      <dl>{displayedFacts.rows.map((row) => <div key={row.key}><dt>{copy.fields[row.key] || row.key}</dt><dd><code>{row.value}</code></dd></div>)}</dl>
      <details><summary>{copy.criticalFacts}</summary><pre>{displayedFacts.critical}</pre></details>
      <details><summary>{copy.financialFacts}</summary><pre>{displayedFacts.financial}</pre></details>
    </div>}
    {(projection || governed && operation.unknown) && <form className="adm-financial-repair-draft" onSubmit={createDraft}>
      <label>{copy.reason}<textarea disabled={governed && locked} required minLength="10" maxLength="2000" value={pending?.draft.reason ?? reason} onChange={(event) => setReason(event.target.value)} /></label>
      <small>{copy.reasonHint}</small>
      <label>{copy.requestKey}<input disabled={governed && locked} required maxLength="111" value={pending?.draft.idempotency_key?.slice('approval:'.length) ?? requestKey} onChange={(event) => setRequestKey(event.target.value)} /></label>
      <small>{copy.requestKeyHint}</small>
      <button type="submit" disabled={busy !== '' || operation.busy}>{governed && operation.unknown ? operationCopy.retry : busy === 'draft' || operation.busy ? copy.creating : copy.createDraft}</button>
    </form>}
  </section>;
}
