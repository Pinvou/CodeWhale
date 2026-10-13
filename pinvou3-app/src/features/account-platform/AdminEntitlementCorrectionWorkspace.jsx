import React, { useEffect, useRef, useState } from 'react';
import { entitlementCorrectionCopy } from '../../shared/i18n.js';
import { entitlementCorrectionPrecheck, entitlementCorrectionApprovalDraft, entitlementCorrectionInputValid } from './admin-entitlement-correction.js';
import { createAndSubmitFreshGovernedApproval } from './admin-governed-approval-workflow.js';
import { useClosureOperation } from './useClosureOperation.js';
import { entitlementIntentStorage, entitlementEditReferences } from './entitlement-correction-intent.js';

export function AdminEntitlementCorrectionWorkspace({ client, copy, language, permissions, owner }) {
  const text = entitlementCorrectionCopy[language];
  const [packageID, setPackageID] = useState(''), [orderID, setOrderID] = useState('');
  const [reasonCode, setReasonCode] = useState(''), [reason, setReason] = useState('');
  const [precheck, setPrecheck] = useState(null), [receipt, setReceipt] = useState(null);
  const [reading, setReading] = useState(false), [readError, setReadError] = useState('');
  const [needsAuthentication, setNeedsAuthentication] = useState(false);
  const generation = useRef(0), controller = useRef(null);
  const canRequest = permissions.includes('platform.billing.repairs.request');
  const operation = useClosureOperation(owner, { ...copy, recoveryUnavailable: text.recoveryUnavailable }, client, 'entitlement-correction', entitlementIntentStorage);
  useEffect(() => {
    generation.current++; controller.current?.abort(); setReading(false); setReadError('');
    setPrecheck(null); setReceipt(null); setNeedsAuthentication(false);
    return () => { generation.current++; controller.current?.abort(); };
  }, [client, client.financeSessionEpoch, owner, canRequest]);
  const pending = operation.pendingFacts;
  const projection = entitlementCorrectionPrecheck(pending?.precheck || precheck);
  const locked = reading || operation.busy || operation.unknown || operation.blocked;
  const change = (setter, value) => { generation.current++; controller.current?.abort(); setter(value); setPrecheck(null); setReceipt(null); setReadError(''); };
  const load = async () => {
    if (!canRequest || locked) return;
    if (!entitlementCorrectionInputValid(packageID, orderID, reasonCode.trim())) { setReadError(copy.inputInvalid); return; }
    const stamp = ++generation.current; controller.current?.abort(); controller.current = new AbortController();
    setReading(true); setPrecheck(null); setReceipt(null); setReadError('');
    try {
      const data = await client.entitlementCorrectionPrecheck(packageID, orderID, reasonCode.trim(), controller.current.signal);
      const result = entitlementCorrectionPrecheck(data);
      if (!result || result.packageID !== packageID || result.orderID !== orderID || result.reasonCode !== reasonCode.trim()) throw new Error('INVALID_ENTITLEMENT_PRECHECK');
      if (stamp === generation.current) setPrecheck(data);
    } catch (error) {
      if (stamp === generation.current) {
        setReadError(error.status === 403 ? copy.permissions : error.status === 400 && error.code === 'INVALID_APPROVAL_PRECHECK_REQUEST' ? copy.inputInvalid : text.unavailable);
        if (error.code === 'RECENT_AUTHENTICATION_REQUIRED') setNeedsAuthentication(true);
      }
    } finally { if (stamp === generation.current) setReading(false); }
  };
  const submit = async () => {
    if (!canRequest || operation.busy || operation.blocked) return;
    const facts = pending || { precheck, reasonCode: reasonCode.trim(), reason: reason.trim() };
    if (!operation.unknown && (!entitlementCorrectionPrecheck(facts.precheck) || [...facts.reason].length < 10)) return;
    const stamp = generation.current;
    const result = await operation.mutate(facts, async (current, key) => {
      setNeedsAuthentication(false);
      try { return await createAndSubmitFreshGovernedApproval(client, entitlementCorrectionApprovalDraft(current.precheck, current.reasonCode, current.reason, key), undefined, '', entitlementEditReferences(owner, key)); }
      catch (error) {
        if (stamp === generation.current && error.code === 'RECENT_AUTHENTICATION_REQUIRED') setNeedsAuthentication(true);
        // A later read/submit rejection cannot undo an earlier draft write.
        // The governed workflow attaches its authoritative request reference.
        if (error.commandReference) error.code = 'COMMAND_RESULT_UNKNOWN';
        throw error;
      }
    });
    if (result) { setReceipt(result); setPrecheck(null); }
  };
  if (!canRequest) return <p role="alert">{copy.permissions}</p>;
  return <section><h2>{text.title}</h2><p>{text.explanation}</p>
    {(reading || operation.busy) && <p role="status">{copy.loading}</p>}
    {(readError || operation.error) && <p role="alert">{readError || operation.error}</p>}
    {operation.message && <p role="status">{operation.message}</p>}
    {operation.unknown && <p>{text.recoveryPending}</p>}
    {needsAuthentication && <><p>{text.recentAuthenticationRequired}</p><button onClick={() => client.reauthenticate('/admin.html?section=entitlement-correction')}>{text.reauthenticate}</button></>}
    <form onSubmit={event => { event.preventDefault(); void load(); }}>
      <label>{text.packageID}<input disabled={locked} value={pending?.precheck.normalized_command.package_id || packageID} onChange={event => change(setPackageID, event.target.value)} /></label>
      <label>{text.orderID}<input disabled={locked} value={pending?.precheck.normalized_command.payment_order_id || orderID} onChange={event => change(setOrderID, event.target.value)} /></label>
      <label>{text.reasonCode}<input disabled={locked} value={pending?.reasonCode || reasonCode} onChange={event => change(setReasonCode, event.target.value)} /><small>{text.reasonCodeHelp}</small></label>
      <button disabled={locked || !packageID || !orderID || !reasonCode.trim()}>{text.precheck}</button>
    </form>
    {projection && <dl>
      <dt>{text.source}</dt><dd>{text.types[projection.critical.source_type]}</dd>
      <dt>{text.target}</dt><dd>{text.types[projection.critical.target_type]}</dd>
      <dt>{text.amount}</dt><dd>{String(projection.critical.amount_microcredits)}</dd>
      <dt>{text.total}</dt><dd>{String(projection.financial.package_total_microcredits)}</dd>
    </dl>}
    {(projection || operation.unknown) && <>
      <label>{text.operatorReason}<textarea disabled={locked} maxLength={2000} value={pending?.reason || reason} onChange={event => setReason(event.target.value)} /></label>
      <button disabled={reading || operation.busy || operation.blocked || !operation.unknown && [...reason.trim()].length < 10} onClick={() => void submit()}>{operation.unknown ? copy.retry : text.submit}</button>
    </>}
    {receipt && <article><h3>{text.approvalRecorded}</h3><p>{receipt.approval_request_id} · {text.approvalStatuses[receipt.status]}</p><p>{text.noFinancialAck}</p></article>}
  </section>;
}
