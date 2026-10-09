import React, { useEffect, useRef, useState } from 'react';
import {
  financialRepairCommandTypes, financialRepairInputs, financialRepairPrecheckBody,
  financialRepairProjectionRows, financialRepairDraftFromProjection,
} from './admin-financial-repair-form.js';
import { integerMoney } from './finance-contract.js';
import { useFinanceOperation } from './useFinanceOperation.js';
import { createAndSubmitFreshGovernedApproval } from './admin-governed-approval-workflow.js';

// The repair page never asks the operator for amounts the owner already knows:
// the precheck returns the frozen projection and the draft copies it verbatim.
// The only operator inputs are the target references, the free-text reason and
// the request key.
export function AdminFinancialRepairDraftForm({ client, copy, onCreated, owner = '', operationCopy = copy, commandTypes = financialRepairCommandTypes }) {
  const [commandType, setCommandType] = useState(commandTypes[0]);
  const [values, setValues] = useState({});
  const [facts, setFacts] = useState(null);
  const [projection, setProjection] = useState(null);
  const [reason, setReason] = useState('');
  const [requestKey, setRequestKey] = useState('');
  const [busy, setBusy] = useState('');
  const [failure, setFailure] = useState('');
  const [created, setCreated] = useState('');
  const factsRevision = useRef(0);
  const mounted = useRef(false);
  const currentClient = useRef(client);
  currentClient.current = client;
  const operation = useFinanceOperation(owner, operationCopy, client, `financial-repair:${commandTypes.join(':')}`);
  const writeOff = commandType === 'RECEIVABLE_WRITE_OFF';
  const paymentRepair = commandType === 'PAYMENT_FINANCIAL_REPAIR';
  const governed = writeOff || paymentRepair;
  const locked = busy !== '' || operation.busy || operation.unknown;
  const pending = operation.pendingFacts;
  const displayedFacts = pending ? financialRepairProjectionRows(pending.projection) : facts;

  useEffect(() => {
    mounted.current = true;
    setFacts(null);
    setProjection(null);
    setBusy((current) => current === 'facts' ? '' : current);
    return () => {
      mounted.current = false;
      factsRevision.current += 1;
    };
  }, [client]);

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
      if (writeOff && (answer.command_type !== commandType || answer.target_resource_id !== prepared.body.target_resource_id ||
        integerMoney(answer.normalized_command.write_off_microcredits) !== integerMoney(prepared.body.write_off_microcredits) ||
        integerMoney(answer.normalized_command.expected_receivable_version) !== integerMoney(prepared.body.expected_version))) {
        throw Object.assign(new Error('owner projection changed the requested intent'), { code: 'FINANCIAL_REPAIR_PRECHECK_INVALID' });
      }
      if (paymentRepair && (answer.command_type !== commandType || answer.target_resource_id !== prepared.body.target_resource_id ||
        answer.normalized_command.payment_order_id !== prepared.body.target_resource_id ||
        answer.normalized_command.payment_evidence_id !== prepared.body.payment_evidence_id)) {
        throw Object.assign(new Error('owner projection changed the payment evidence'), { code: 'FINANCIAL_REPAIR_PRECHECK_INVALID' });
      }
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
    if (governed) {
      const prepared = pending || { ...financialRepairDraftFromProjection(projection, reason, requestKey), projection };
      if (!prepared.draft) { setFailure(copy.invalidDraft); return; }
      const recovering = operation.unknown;
      const result = await operation.mutate(prepared, async current => {
        try { return await createAndSubmitFreshGovernedApproval(client, current.draft); }
        catch (error) {
          // A denied replay cannot prove that an earlier unknown write did not
          // commit. Keep the original facts/key until authority confirms it.
          if (recovering && !error.finalResult) throw Object.assign(error, { code: 'COMMAND_RESULT_UNKNOWN' });
          throw error;
        }
      });
      if (result) {
        setCreated(result.approval_request_id); setProjection(null); setFacts(null);
        setValues({}); setReason(''); setRequestKey(''); onCreated?.(result.approval_request_id);
      }
      return;
    }
    const prepared = financialRepairDraftFromProjection(projection, reason, requestKey);
    if (prepared.error) {
      setFailure(copy.invalidDraft);
      return;
    }
    setBusy('draft');
    setFailure('');
    try {
      const id = await client.createGovernedApprovalDraft(prepared.draft);
      setCreated(id);
      setProjection(null);
      setFacts(null);
      setValues({});
      setReason('');
      setRequestKey('');
      onCreated?.(id);
    } catch (error) {
      setFailure(error?.code === 'APPROVAL_ACTION_CONTROL_CHANGED' ? copy.controlChanged : copy.draftFailed);
    } finally {
      setBusy('');
    }
  };

  return <section className="adm-financial-repair" aria-label={copy.title}>
    <h3>{copy.title}</h3>
    <p>{copy.lead}</p>
    {governed && operation.error && <p role="alert">{operation.error}</p>}
    <form className="adm-financial-repair-target" onSubmit={readFacts}>
      <label>{copy.command}
        <select disabled={governed && locked} value={commandType} onChange={(event) => changeCommand(event.target.value)}>
          {commandTypes.map((command) => <option key={command} value={command}>{command}</option>)}
        </select>
      </label>
      {fields.map((field) => <label key={field.key}>{label(field)}
        <input disabled={governed && locked} required value={pending ? String(field.key === 'target_resource_id' ? pending.draft.target_resource_id : field.key === 'expected_version' ? pending.draft.normalized_command.expected_receivable_version : pending.draft.normalized_command[field.key]) : values[field.key] || ''} inputMode={field.kind === 'integer' ? 'numeric' : 'text'}
          onChange={(event) => setValue(field.key, event.target.value)} />
      </label>)}
      <button type="submit" disabled={locked}>{busy === 'facts' ? copy.reading : copy.readFacts}</button>
    </form>
    {failure && <p role="alert">{failure}</p>}
    {created && <p role="status">{copy.createdHint} <code>{created}</code></p>}
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
