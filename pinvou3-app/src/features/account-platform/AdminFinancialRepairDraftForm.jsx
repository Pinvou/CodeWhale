import React, { useState } from 'react';
import {
  financialRepairCommandTypes, financialRepairInputs, financialRepairPrecheckBody,
  financialRepairProjectionRows, financialRepairDraftFromProjection,
} from './admin-financial-repair-form.js';

// The repair page never asks the operator for amounts the owner already knows:
// the precheck returns the frozen projection and the draft copies it verbatim.
// The only operator inputs are the target references, the free-text reason and
// the request key.
export function AdminFinancialRepairDraftForm({ client, copy, onCreated }) {
  const [commandType, setCommandType] = useState(financialRepairCommandTypes[0]);
  const [values, setValues] = useState({});
  const [facts, setFacts] = useState(null);
  const [projection, setProjection] = useState(null);
  const [reason, setReason] = useState('');
  const [requestKey, setRequestKey] = useState('');
  const [busy, setBusy] = useState('');
  const [failure, setFailure] = useState('');
  const [created, setCreated] = useState('');

  const fields = financialRepairInputs[commandType];
  const label = (field) => (field.key === 'target_resource_id' ? copy.targets[commandType] : copy.fields[field.key]);
  const setValue = (key, value) => {
    setValues((current) => ({ ...current, [key]: value }));
    setFacts(null);
    setProjection(null);
    setFailure('');
    setCreated('');
  };
  const changeCommand = (next) => {
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
    setBusy('facts');
    setFailure('');
    setCreated('');
    try {
      const answer = await client.approvalCommandPrecheck(prepared.body);
      const rendered = financialRepairProjectionRows(answer);
      if (!rendered) throw Object.assign(new Error('unusable projection'), { code: 'FINANCIAL_REPAIR_PRECHECK_INVALID' });
      setProjection(answer);
      setFacts(rendered);
    } catch (error) {
      setProjection(null);
      setFacts(null);
      setFailure(error?.code === 'APPROVAL_PRECHECK_UNAVAILABLE' ? copy.factsUnavailable : copy.factsFailed);
    } finally {
      setBusy('');
    }
  };

  const createDraft = async (event) => {
    event.preventDefault();
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
    <form className="adm-financial-repair-target" onSubmit={readFacts}>
      <label>{copy.command}
        <select value={commandType} onChange={(event) => changeCommand(event.target.value)}>
          {financialRepairCommandTypes.map((command) => <option key={command} value={command}>{command}</option>)}
        </select>
      </label>
      {fields.map((field) => <label key={field.key}>{label(field)}
        <input required value={values[field.key] || ''} inputMode={field.kind === 'integer' ? 'numeric' : 'text'}
          onChange={(event) => setValue(field.key, event.target.value)} />
      </label>)}
      <button type="submit" disabled={busy !== ''}>{busy === 'facts' ? copy.reading : copy.readFacts}</button>
    </form>
    {failure && <p role="alert">{failure}</p>}
    {created && <p role="status">{copy.createdHint} <code>{created}</code></p>}
    {facts && <div className="adm-financial-repair-facts">
      <h4>{copy.frozenFacts}</h4>
      <dl>{facts.rows.map((row) => <div key={row.key}><dt>{copy.fields[row.key] || row.key}</dt><dd><code>{row.value}</code></dd></div>)}</dl>
      <details><summary>{copy.criticalFacts}</summary><pre>{facts.critical}</pre></details>
      <details><summary>{copy.financialFacts}</summary><pre>{facts.financial}</pre></details>
    </div>}
    {projection && <form className="adm-financial-repair-draft" onSubmit={createDraft}>
      <label>{copy.reason}<textarea required minLength="10" maxLength="2000" value={reason} onChange={(event) => setReason(event.target.value)} /></label>
      <small>{copy.reasonHint}</small>
      <label>{copy.requestKey}<input required maxLength="200" value={requestKey} onChange={(event) => setRequestKey(event.target.value)} /></label>
      <small>{copy.requestKeyHint}</small>
      <button type="submit" disabled={busy !== ''}>{busy === 'draft' ? copy.creating : copy.createDraft}</button>
    </form>}
  </section>;
}
