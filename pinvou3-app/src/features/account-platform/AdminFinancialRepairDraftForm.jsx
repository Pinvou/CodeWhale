import { AdminUsageSettlementFinancialRepairForm } from './AdminUsageSettlementFinancialRepairForm.jsx';
import { AdminReconciliationFinancialRepairForm } from './AdminReconciliationFinancialRepairForm.jsx';
import React, { useEffect, useRef, useState } from 'react';
import {
  financialRepairInputs, financialRepairPrecheckBody,
  financialRepairProjectionRows, financialRepairDraftFromProjection,
} from './admin-financial-repair-form.js';

import { AdminChargebackFinancialRepairForm } from './AdminChargebackFinancialRepairForm.jsx';
import { AdminRefundExceptionRepairForm } from './AdminRefundExceptionRepairForm.jsx';
import { AdminPaymentFinancialRepairForm } from './AdminPaymentFinancialRepairForm.jsx';
import { AdminReceivableWriteOffForm } from './AdminReceivableWriteOffForm.jsx';
import { useClosureOperation } from './useClosureOperation.js';
import { ledgerIntentStorage, readLedgerIntent } from './ledger-repair-intent.js';
import { requireFinanceID } from './finance-contract.js';

// The repair page never asks the operator for amounts the owner already knows:
// the precheck returns the frozen projection and the draft copies it verbatim.
// The only operator inputs are the target references, the free-text reason and
// the request key.
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

function LedgerRepairDraftForm({ client, copy, owner, onCreated }) {
  const [commandType, setCommandType] = useState('LEDGER_REPAIR');
  const [values, setValues] = useState({});
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

  useEffect(() => {
    mounted.current = true;
    setFacts(null);
    setProjection(null);
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
  const label = (field) => (field.key === 'target_resource_id' ? copy.targets[commandType] : copy.fields[field.key]);
  const setValue = (key, value) => {
    factsRevision.current += 1;
    setBusy((current) => current === 'facts' ? '' : current);
    setValues((current) => ({ ...current, [key]: value }));
    setFacts(null);
    setProjection(null);
    setFailure('');
    setCreated('');
  };
  const changeCommand = (next) => {
    factsRevision.current += 1;
    setBusy((current) => current === 'facts' ? '' : current);
    setCommandType(next);
    setValues({});
    setFacts(null);
    setProjection(null);
    setFailure('');
    setCreated('');
  };

  const readFacts = async (event) => {
    event.preventDefault();
    if (locked) return;
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
    setFailure('');
    setCreated('');
    try {
      const answer = await client.approvalCommandPrecheck(prepared.body);
      if (!live()) return;
      const rendered = financialRepairProjectionRows(answer);
      if (!rendered) throw Object.assign(new Error('unusable projection'), { code: 'FINANCIAL_REPAIR_PRECHECK_INVALID' });
      setProjection(answer);
      setFacts(rendered);
    } catch (error) {
      if (!live()) return;
      setProjection(null);
      setFacts(null);
      setFailure(error?.code === 'APPROVAL_PRECHECK_UNAVAILABLE' ? copy.factsUnavailable : copy.factsFailed);
    } finally {
      if (live()) setBusy('');
    }
  };

  const createDraft = async (event) => {
    event.preventDefault();
    if (busy !== '' || operation.busy || operation.blocked) return;
    const frozen = pending || { projection, reason, requestKey: requestKey.trim() };
    const prepared = financialRepairDraftFromProjection(frozen.projection, frozen.reason, frozen.requestKey);
    if (prepared.error) { setFailure(copy.invalidDraft); return; }
    const revision = factsRevision.current;
    const live = () => mounted.current && factsRevision.current === revision && currentClient.current === client;
    setFailure('');
    const id = await operation.mutate(frozen, async (current, key) => {
      const draft = financialRepairDraftFromProjection(current.projection, current.reason, key);
      if (draft.error) throw Object.assign(new Error('invalid frozen draft'), { status: 400 });
      let id;
      try { id = requireFinanceID(await client.createGovernedApprovalDraft(draft.draft)); }
      catch (error) {
        if (live() && error.code === 'RECENT_AUTHENTICATION_REQUIRED') setNeedsAuthentication(true);
        throw error;
      }
      const saved = readLedgerIntent(owner.split(':')[0], 'ledger-repair-draft');
      if (!saved || saved.key !== key || JSON.stringify(saved.facts) !== JSON.stringify(current)) {
        throw Object.assign(new Error('invocation changed before acknowledgment'), { code: 'COMMAND_RESULT_UNKNOWN' });
      }
      return id;
    });
    if (!id || !live()) return;
    setCreated(id); setProjection(null); setFacts(null); setValues({}); setReason(''); setRequestKey('');
    onCreated?.(id);
  };

  return <section className="adm-financial-repair" aria-label={copy.title}>
    <h3>{copy.title}</h3>
    <p>{copy.lead}</p>
    <form className="adm-financial-repair-target" onSubmit={readFacts}>
      <label>{copy.command}
        <select disabled={locked} value={commandType} onChange={(event) => changeCommand(event.target.value)}>
          {['LEDGER_REPAIR'].map((command) => <option key={command} value={command}>{command}</option>)}
        </select>
      </label>
      {fields.map((field) => <label key={field.key}>{label(field)}
        <input disabled={locked} required value={values[field.key] || ''} inputMode={field.kind === 'integer' ? 'numeric' : 'text'}
          onChange={(event) => setValue(field.key, event.target.value)} />
      </label>)}
      <button type="submit" disabled={locked}>{busy === 'facts' ? copy.reading : copy.readFacts}</button>
    </form>
    {(failure || operation.error) && <p role="alert">{failure || operation.error}</p>}{operation.unknown && <p role="status">{copy.recoveryPending}</p>}
    {needsAuthentication && <><p>{copy.recentAuthenticationRequired}</p><button disabled={operation.busy} onClick={() => client.reauthenticate('/admin.html')}>{copy.reauthenticate}</button></>}
    {created && <p role="status">{copy.createdHint} <code>{created}</code></p>}
    {visibleFacts && <div className="adm-financial-repair-facts">
      <h4>{copy.frozenFacts}</h4>
      <dl>{visibleFacts.rows.map((row) => <div key={row.key}><dt>{copy.fields[row.key] || row.key}</dt><dd><code>{row.value}</code></dd></div>)}</dl>
      <details><summary>{copy.criticalFacts}</summary><pre>{visibleFacts.critical}</pre></details>
      <details><summary>{copy.financialFacts}</summary><pre>{visibleFacts.financial}</pre></details>
    </div>}
    {visibleProjection && <form className="adm-financial-repair-draft" onSubmit={createDraft}>
      <label>{copy.reason}<textarea disabled={locked} required minLength="10" maxLength="2000" value={pending?.reason || reason} onChange={(event) => setReason(event.target.value)} /></label>
      <small>{copy.reasonHint}</small>
      <label>{copy.requestKey}<input disabled={locked} required maxLength="111" value={pending?.requestKey || requestKey} onChange={(event) => setRequestKey(event.target.value)} /></label>
      <small>{copy.requestKeyHint}</small>
      <button type="submit" disabled={busy !== '' || operation.busy || operation.blocked}>{operation.busy ? copy.creating : operation.unknown ? copy.retry : copy.createDraft}</button>
    </form>}
  </section>;
}
