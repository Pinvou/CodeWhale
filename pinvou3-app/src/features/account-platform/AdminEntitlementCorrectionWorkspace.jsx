import React, { useState } from 'react';
import { entitlementCorrectionCopy } from '../../shared/i18n.js';
import { entitlementCorrectionPrecheck, entitlementCorrectionApprovalDraft } from './admin-entitlement-correction.js';
import { createAndSubmitFreshGovernedApproval } from './admin-governed-approval-workflow.js';
import { useFinanceOperation } from './useFinanceOperation.js';

// The owner precheck is a read, not an approval or a reservation. After the
// first write, retain that exact projection and invocation across unknown ACKs.
export function AdminEntitlementCorrectionWorkspace({ client, copy, language, permissions, owner }) {
  const text = entitlementCorrectionCopy[language];
  const [packageID, setPackageID] = useState('');
  const [orderID, setOrderID] = useState('');
  const [reasonCode, setReasonCode] = useState('');
  const [reason, setReason] = useState('');
  const [precheck, setPrecheck] = useState(null);
  const [receipt, setReceipt] = useState(null);
  const operation = useFinanceOperation(owner, copy, client, 'entitlement-correction');
  const projection = entitlementCorrectionPrecheck(precheck);
  const locked = operation.busy || operation.unknown;
  const change = (setter, value) => { setter(value); setPrecheck(null); setReceipt(null); };
  const load = async () => {
    setPrecheck(null); setReceipt(null);
    const value = await operation.read(async () => {
      const data = await client.entitlementCorrectionPrecheck(packageID, orderID, reasonCode.trim());
      const result = entitlementCorrectionPrecheck(data);
      if (!result || result.packageID !== packageID || result.orderID !== orderID || result.reasonCode !== reasonCode.trim()) {
        throw new Error('INVALID_ENTITLEMENT_PRECHECK');
      }
      return data;
    });
    if (value) setPrecheck(value);
  };
  const submit = async () => {
    const facts = operation.pendingFacts || { precheck, reasonCode: reasonCode.trim(), reason: reason.trim() };
    if (!operation.unknown && (!entitlementCorrectionPrecheck(facts.precheck) || [...facts.reason].length < 10)) return;
    const result = await operation.mutate(facts, (current, key) => {
      const draft = entitlementCorrectionApprovalDraft(current.precheck, current.reasonCode, current.reason, key);
      return createAndSubmitFreshGovernedApproval(client, draft);
    });
    if (result) { setReceipt(result); setPrecheck(null); }
  };
  if (!permissions.includes('platform.billing.repairs.request')) return <p role="alert">{copy.permissions}</p>;
  return <section>
    <h3>{text.title}</h3><p>{text.explanation}</p>
    {operation.busy && <p role="status">{copy.loading}</p>}
    {operation.error && <p role="alert">{operation.error}</p>}
    {operation.message && <p role="status">{operation.message}</p>}
    <form onSubmit={event => { event.preventDefault(); void load(); }}>
      <label>{text.packageID}<input disabled={locked} value={packageID} onChange={event => change(setPackageID, event.target.value)} /></label>
      <label>{text.orderID}<input disabled={locked} value={orderID} onChange={event => change(setOrderID, event.target.value)} /></label>
      <label>{text.reasonCode}<input disabled={locked} value={reasonCode} onChange={event => change(setReasonCode, event.target.value)} /></label>
      <button disabled={locked || !packageID || !orderID || !reasonCode.trim()}>{text.precheck}</button>
    </form>
    {projection && <dl>
      <dt>{text.source}</dt><dd>{projection.critical.source_type}</dd>
      <dt>{text.target}</dt><dd>{projection.critical.target_type}</dd>
      <dt>{text.amount}</dt><dd>{String(projection.critical.amount_microcredits)}</dd>
      <dt>{text.total}</dt><dd>{String(projection.financial.package_total_microcredits)}</dd>
    </dl>}
    {(projection || operation.unknown) && <>
      <label>{text.operatorReason}<textarea disabled={locked} maxLength={2000} value={reason} onChange={event => setReason(event.target.value)} /></label>
      <button disabled={operation.busy || !operation.unknown && [...reason.trim()].length < 10} onClick={() => void submit()}>{operation.unknown ? copy.retry : copy.draft}</button>
    </>}
    {receipt && <article><h4>{copy.approvals}</h4><p>{receipt.approval_request_id} / {receipt.status}</p></article>}
  </section>;
}
