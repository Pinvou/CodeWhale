import { AdminUsageSettlementFinancialRepairForm } from './AdminUsageSettlementFinancialRepairForm.jsx';
import { AdminReconciliationFinancialRepairForm } from './AdminReconciliationFinancialRepairForm.jsx';
import React, { useEffect, useId, useRef, useState } from 'react';
import {
  financialRepairInputs, financialRepairPrecheckBody,
  financialRepairProjectionRows, financialRepairDraftFromProjection,
  ledgerRepairProjectionMatchesInput,
} from './admin-financial-repair-form.js';

import { AdminChargebackFinancialRepairForm } from './AdminChargebackFinancialRepairForm.jsx';
import { AdminRefundExceptionRepairForm } from './AdminRefundExceptionRepairForm.jsx';
import { AdminPaymentFinancialRepairForm } from './AdminPaymentFinancialRepairForm.jsx';
import { AdminReceivableWriteOffForm } from './AdminReceivableWriteOffForm.jsx';
import { useClosureOperation } from './useClosureOperation.js';
import { ledgerIntentStorage, readLedgerIntent } from './ledger-repair-intent.js';
import { requireFinanceID } from './finance-contract.js';
import { ledgerPointsToMicrocredits, ledgerMicrocreditsToPoints } from './ledger-amount-input.js';

// Each command retains its closed operator-input contract. The owner precheck
// returns the frozen projection and the draft copies it verbatim.
export function AdminFinancialRepairDraftForm(props) {
  if (props.commandTypes?.length === 1 && props.commandTypes[0] === 'USAGE_SETTLEMENT_REPAIR') {
    return <AdminUsageSettlementFinancialRepairForm {...props} />;
  }
  if (props.commandTypes?.length === 1 && props.commandTypes[0] === 'RECONCILIATION_FINANCIAL_REPAIR') {
    return <AdminReconciliationFinancialRepairForm {...props} />;
  }
  if (props.commandTypes?.length === 1 && props.commandTypes[0] === 'CHARGEBACK_FINANCIAL_REPAIR') {
    return <AdminChargebackFinancialRepairForm {...props} />;
  }
  if (props.commandTypes?.length === 1 && props.commandTypes[0] === 'REFUND_EXCEPTION_REPAIR') {
    return <AdminRefundExceptionRepairForm {...props} />;
  }
  if (props.commandTypes?.length === 1 && props.commandTypes[0] === 'PAYMENT_FINANCIAL_REPAIR') {
    return <AdminPaymentFinancialRepairForm {...props} />;
  }
  if (props.commandTypes?.length === 1 && props.commandTypes[0] === 'RECEIVABLE_WRITE_OFF') {
    return <AdminReceivableWriteOffForm {...props} />;
  }
  if (!props.commandTypes || (props.commandTypes.length === 1 && props.commandTypes[0] === 'LEDGER_REPAIR')) {
    return <LedgerRepairDraftForm {...props} />;
  }
  return <section role="alert">{props.copy.invalidInput}</section>;
}

function LedgerRepairImpact({ projection, words, titleID }) {
  const normalized = projection.normalized_command;
  const financial = projection.financial_snapshot;
  const roles = [
    [words.original, normalized.wrong_ledger_account_id, financial.wrong_ledger_account_id, financial.wrong_account_type],
    [words.intended, normalized.correct_ledger_account_id, financial.correct_ledger_account_id, financial.correct_account_type],
  ];
  const points = ledgerMicrocreditsToPoints(normalized.amount_microcredits);
  return <section className="adm-ledger-impact" aria-labelledby={titleID}>
    <h5 id={titleID}>{words.title}</h5>
    <div className="adm-ledger-impact-accounts">{roles.map(([role, id, snapshotID, type]) => {
      const matching = id === snapshotID && typeof type === 'string' && type.length > 0;
      const accountType = matching ? (Object.prototype.hasOwnProperty.call(words.types, type) ? words.types[type] : type) : words.notProvided;
      return <dl key={role}><div><dt>{role}</dt><dd><code>{id}</code></dd></div><div><dt>{words.accountType}</dt><dd>{accountType}</dd></div></dl>;
    })}</div>
    <p className="adm-ledger-impact-amount">{words.requested}: <strong>{points ? words.points(points) : words.notProvided}</strong></p>
    <p className="adm-ledger-impact-limit">{words.limit}</p>
  </section>;
}

function LedgerRepairDraftForm({ client, copy, owner, onCreated }) {
  const commandType = 'LEDGER_REPAIR';
  const entry = copy.ledgerTargetEntry;
  const targetEntryID = useId();
  const [values, setValues] = useState({});
  const [amountPoints, setAmountPoints] = useState('');
  const [amountTouched, setAmountTouched] = useState(false);
  const amountHintID = useId(), amountErrorID = useId();
  const [facts, setFacts] = useState(null);
  const [projection, setProjection] = useState(null);
  const [reason, setReason] = useState('');
  const [requestKey, setRequestKey] = useState('');
  const [busy, setBusy] = useState('');
  const [failure, setFailure] = useState('');
  const [created, setCreated] = useState('');
  const [needsAuthentication, setNeedsAuthentication] = useState(false);
  const factsRevision = useRef(0);
  const acceptedRevision = useRef(null);
  const mounted = useRef(false);
  const currentClient = useRef(client);
  currentClient.current = client;

  useEffect(() => {
    mounted.current = true;
    setFacts(null);
    setProjection(null);
    acceptedRevision.current = null;
    setNeedsAuthentication(false);
    setBusy((current) => current === 'facts' ? '' : current);
    return () => {
      mounted.current = false;
      factsRevision.current += 1;
    };
  }, [client]);

  const operation = useClosureOperation(owner, copy, client, 'ledger-repair-draft', ledgerIntentStorage);
  const pending = operation.pendingFacts;
  const visibleProjection = pending?.projection || projection;
  const visibleFacts = pending ? financialRepairProjectionRows(pending.projection) : facts;
  const locked = busy !== '' || operation.busy || operation.unknown || operation.blocked;
  const fields = financialRepairInputs[commandType];
  const frozenAmount = pending?.projection?.normalized_command?.amount_microcredits;
  const refreshingPreview = busy === 'facts' && Boolean(visibleFacts);
  const displayedPoints = pending ? ledgerMicrocreditsToPoints(frozenAmount) : amountPoints;
  const displayedMicrocredits = pending ? String(frozenAmount) : values.amount_microcredits || '';
  const amountInvalid = !pending && amountPoints !== '' && !displayedMicrocredits;
  const label = (field) => field.key === 'amount_microcredits' ? copy.amountPointsLabel : entry.fields[field.key].label;
  const fieldValue = (field) => {
    if (field.key === 'amount_microcredits') return displayedPoints;
    if (pending && visibleFacts) return field.key === 'target_resource_id'
      ? pending.projection.target_resource_id
      : pending.projection.normalized_command[field.key] || '';
    return values[field.key] || '';
  };
  const setValue = (key, value) => {
    factsRevision.current += 1;
    acceptedRevision.current = null;
    setBusy((current) => current === 'facts' ? '' : current);
    setValues((current) => ({ ...current, [key]: value }));
    setFacts(null);
    setProjection(null);
    setFailure('');
    setCreated('');
  };
  const setAmount = (value) => {
    let microcredits = '';
    try { microcredits = ledgerPointsToMicrocredits(value); } catch { /* Invalid current text must never retain the prior wire value. */ }
    setAmountPoints(value);
    setValue('amount_microcredits', microcredits);
  };
  const readFacts = async (event) => {
    event.preventDefault();
    if (locked) return;
    const prepared = financialRepairPrecheckBody(commandType, values);
    if (prepared.error) {
      acceptedRevision.current = null;
      setProjection(null);
      setFacts(null);
      setFailure(prepared.field === 'amount_microcredits' ? copy.amountPointsInvalid : copy.invalidInput);
      if (prepared.field === 'amount_microcredits') setAmountTouched(true);
      return;
    }
    const revision = ++factsRevision.current;
    acceptedRevision.current = null;
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
      if (!ledgerRepairProjectionMatchesInput(answer, prepared.body)) {
        throw Object.assign(new Error('owner projection changed the ledger intent'), { code: 'FINANCIAL_REPAIR_PRECHECK_INVALID' });
      }
      acceptedRevision.current = revision;
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
    if (busy !== '' || operation.busy || operation.blocked) return;
    if (!mounted.current || currentClient.current !== client) return;
    if (!pending) {
      const current = financialRepairPrecheckBody(commandType, values);
      if (acceptedRevision.current !== factsRevision.current || current.error || !ledgerRepairProjectionMatchesInput(projection, current.body)) {
        setFailure(copy.invalidDraft); return;
      }
    }
    const frozen = pending || { projection, reason, requestKey: requestKey.trim() };
    const prepared = financialRepairDraftFromProjection(frozen.projection, frozen.reason, frozen.requestKey);
    if (prepared.error) { setFailure(copy.invalidDraft); return; }
    const revision = factsRevision.current;
    const live = () => mounted.current && factsRevision.current === revision && currentClient.current === client;
    setFailure('');
    setNeedsAuthentication(false);
    const id = await operation.mutate(frozen, async (current, key) => {
      const draft = financialRepairDraftFromProjection(current.projection, current.reason, key);
      if (draft.error) throw Object.assign(new Error('invalid frozen draft'), { status: 400 });
      let id;
      try { id = requireFinanceID(await client.createGovernedApprovalDraft(draft.draft)); }
      catch (error) {
        if (live() && (error?.code === 'RECENT_AUTHENTICATION_REQUIRED' || error?.status === 401)) setNeedsAuthentication(true);
        throw error;
      }
      const saved = readLedgerIntent(owner.split(':')[0], 'ledger-repair-draft');
      if (!saved || saved.key !== key || JSON.stringify(saved.facts) !== JSON.stringify(current)) {
        throw Object.assign(new Error('invocation changed before acknowledgment'), { code: 'COMMAND_RESULT_UNKNOWN' });
      }
      return id;
    });
    if (!id || !live()) return;
    acceptedRevision.current = null;
    setCreated(id); setProjection(null); setFacts(null); setValues({}); setAmountPoints(''); setAmountTouched(false); setReason(''); setRequestKey('');
    onCreated?.(id);
  };

  const workflow = copy.ledgerWorkflow;
  const readingSnapshot = busy === 'facts';
  const stage = operation.busy || operation.unknown || created ? 2 : readingSnapshot || visibleProjection ? 1 : 0;
  const phase = readingSnapshot ? workflow.phaseReading : workflow.phaseReview;
  return <section className="adm-financial-repair" aria-label={copy.title}>
    <h3>{copy.title}</h3>
    <ol className="adm-ledger-workflow" role="list" aria-label={workflow.title}>{workflow.steps.map((label, i) => <li key={label} aria-current={i === stage ? 'step' : undefined}><span>{label}</span>{i === 1 && stage === 1 && <small className="adm-ledger-workflow-phase">{phase}</small>}{i === 2 && <small className="adm-ledger-workflow-phase">{workflow.approvalNext}</small>}</li>)}</ol>
    <form className="adm-financial-repair-target adm-ledger-target-form" onSubmit={readFacts}>
      <div className="adm-ledger-target-intro"><strong>{entry.task}</strong><p>{entry.help}</p></div>
      {fields.map((field) => {
        const amountField = field.key === 'amount_microcredits';
        const inputID = targetEntryID + '-' + field.key;
        const input = <input id={inputID} name={field.key} disabled={locked} required value={fieldValue(field)}
          inputMode={amountField ? 'decimal' : field.kind === 'integer' ? 'numeric' : 'text'}
          data-ledger-amount={amountField ? '' : undefined}
          aria-describedby={amountField ? amountHintID + (amountTouched && amountInvalid ? ' ' + amountErrorID : '') : inputID + '-help'}
          aria-invalid={amountField && amountTouched && amountInvalid ? true : undefined}
          onBlur={amountField ? () => setAmountTouched(true) : undefined}
          onChange={(event) => amountField ? setAmount(event.target.value) : setValue(field.key, event.target.value)} />;
        if (amountField) return <label key={field.key}>{label(field)}{input}</label>;
        return <div className="adm-ledger-target-field" key={field.key}>
          <label htmlFor={inputID}>{label(field)}</label>
          {input}
          <small id={inputID + '-help'}>{entry.fields[field.key].help}</small>
        </div>;
      })}
      <small id={amountHintID}>{copy.amountPointsHint}{displayedMicrocredits && <> {copy.amountPointsEquivalent.replace('{amount}', displayedMicrocredits)}</>}</small>
      {amountTouched && amountInvalid && <small id={amountErrorID} role="alert">{copy.amountPointsInvalid}</small>}
      <button type="submit" disabled={locked || amountInvalid}>{busy === 'facts' ? workflow.loading : workflow.load}</button>
    </form>
    {(failure || operation.error) && <p role="alert">{failure || operation.error}</p>}{operation.unknown && <p role="status">{copy.recoveryPending}</p>}
    {needsAuthentication && <><p>{copy.recentAuthenticationRequired}</p><button disabled={operation.busy} onClick={() => client.reauthenticate('/admin.html?section=finance&finance_tab=ledger')}>{copy.reauthenticate}</button></>}
    {created && <p role="status">{copy.createdHint} <code>{created}</code></p>}
    {refreshingPreview && <p className="adm-ledger-impact-refresh" role="status">{copy.ledgerImpact.refreshing}</p>}
    {visibleFacts && !refreshingPreview && <div className="adm-financial-repair-facts">
      <h4>{copy.frozenFacts}</h4>
      {visibleProjection && <LedgerRepairImpact projection={visibleProjection} words={copy.ledgerImpact} titleID={targetEntryID + '-impact'} />}
      <details><summary>{copy.ledgerImpact.identifiers}</summary>
        <dl>{visibleFacts.rows.map((row) => <div key={row.key}><dt>{copy.fields[row.key] || row.key}</dt><dd><code>{row.value}</code></dd></div>)}</dl>
      </details>
      <details><summary>{copy.criticalFacts}</summary><pre>{visibleFacts.critical}</pre></details>
      <details><summary>{copy.financialFacts}</summary><pre>{visibleFacts.financial}</pre></details>
    </div>}
    {visibleProjection && !refreshingPreview && <form className="adm-financial-repair-draft" onSubmit={createDraft}>
      <label>{copy.reason}<textarea disabled={locked} required minLength="10" maxLength="2000" value={pending?.reason || reason} onChange={(event) => setReason(event.target.value)} /></label>
      <small>{copy.reasonHint}</small>
      <label>{copy.requestKey}<input disabled={locked} required maxLength="111" value={pending?.requestKey || requestKey} onChange={(event) => setRequestKey(event.target.value)} /></label>
      <small>{copy.requestKeyHint}</small>
      <button type="submit" disabled={busy !== '' || operation.busy || operation.blocked}>{operation.busy ? copy.creating : operation.unknown ? copy.retry : copy.createDraft}</button>
    </form>}
  </section>;
}
