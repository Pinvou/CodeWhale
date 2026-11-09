import React, { useEffect, useId, useRef, useState } from 'react';
import { entitlementCorrectionCopy } from '../../shared/i18n.js';
import { entitlementCorrectionPrecheck, entitlementCorrectionApprovalDraft, entitlementCorrectionInputValid } from './admin-entitlement-correction.js';
import { createAndSubmitFreshGovernedApproval } from './admin-governed-approval-workflow.js';
import { useClosureOperation } from './useClosureOperation.js';
import { formatBalance } from './balance-view.js';
import { entitlementIntentStorage, entitlementEditReferences } from './entitlement-correction-intent.js';

export function AdminEntitlementCorrectionWorkspace({ client, copy, language, permissions, owner }) {
  const text = entitlementCorrectionCopy[language];
  const actor = owner.split(':')[0];
  const [packageID, setPackageID] = useState(''), [orderID, setOrderID] = useState('');
  const [reasonCode, setReasonCode] = useState(''), [reason, setReason] = useState('');
  const [precheck, setPrecheck] = useState(null), [receipt, setReceipt] = useState(null);
  const [displayScope, setDisplayScope] = useState(null);
  const scopeEpoch = client.financeSessionEpoch;
  const [reading, setReading] = useState(false), [readError, setReadError] = useState('');
  const [needsAuthentication, setNeedsAuthentication] = useState(false);
  const generation = useRef(0), controller = useRef(null);
  const canRequest = permissions.includes('platform.billing.repairs.request');
  const operation = useClosureOperation(owner, { ...copy, recoveryUnavailable: text.recoveryUnavailable }, client, 'entitlement-correction', entitlementIntentStorage);
  useEffect(() => {
    generation.current++; controller.current?.abort(); setReading(false); setReadError('');
    setPrecheck(null); setReceipt(null); setNeedsAuthentication(false);
    setDisplayScope({ client, owner, epoch: scopeEpoch, canRequest });
    return () => { generation.current++; controller.current?.abort(); };
  }, [client, client.financeSessionEpoch, owner, canRequest]);
  const reasonCodeID = useId();
  const pending = operation.pendingFacts;
  const displayedReasonCode = pending?.reasonCode || reasonCode;
  const reasonCodeBytes = new TextEncoder().encode(displayedReasonCode.trim()).length;
  const reasonCodeOver = reasonCodeBytes > 80;
  const projection = entitlementCorrectionPrecheck(pending?.precheck || precheck);
  const visibleFacts = displayScope?.client === client && displayScope.owner === owner && displayScope.epoch === scopeEpoch && displayScope.canRequest && canRequest && !reading ? projection : null;
  const referenceID = useId();
  const composition = visibleFacts && (() => {
    // These are package category allocations, not a current wallet balance.
    // The existing proof validates both original totals and the amount that
    // the executor subtracts from the source and credits to the target type.
    const { critical, financial } = visibleFacts;
    const transfer = BigInt(critical.amount_microcredits);
    const source = BigInt(financial.source_original_microcredits);
    const target = BigInt(financial.target_original_microcredits);
    return {
      transfer, total: financial.package_total_microcredits,
      rows: [[critical.source_type, source, source - transfer], [critical.target_type, target, target + transfer]],
    };
  })();
  const showCredits = value => formatBalance(value, language) + ' ' + text.composition.points;
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
      try { return await createAndSubmitFreshGovernedApproval(client, entitlementCorrectionApprovalDraft(current.precheck, current.reasonCode, current.reason, key), undefined, '', entitlementEditReferences(actor, key)); }
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
      <label>{text.packageID}<input disabled={locked} value={pending?.precheck.normalized_command.package_id || packageID} aria-describedby={referenceID + '-package'} onChange={event => change(setPackageID, event.target.value)} /><small id={referenceID + '-package'}>{text.reference.packageHelp}</small></label>
      <label>{text.orderID}<input disabled={locked} value={pending?.precheck.normalized_command.payment_order_id || orderID} aria-describedby={referenceID + '-order'} onChange={event => change(setOrderID, event.target.value)} /><small id={referenceID + '-order'}>{text.reference.orderHelp}</small></label>
      <label>{text.reasonCode}<input disabled={locked} value={displayedReasonCode} aria-required="true" aria-invalid={reasonCodeOver || undefined} aria-describedby={`${reasonCodeID}-help ${reasonCodeID}-count${reasonCodeOver ? ` ${reasonCodeID}-over` : ''}`} onChange={event => change(setReasonCode, event.target.value)} /><small id={`${reasonCodeID}-help`}>{text.reasonCodeHelp}</small><small id={`${reasonCodeID}-count`}>{text.reasonCodeCount(reasonCodeBytes)}</small>{reasonCodeOver && <small id={`${reasonCodeID}-over`} role="alert">{text.reasonCodeOver}</small>}</label>
      <button disabled={locked || !packageID || !orderID || !reasonCode.trim() || reasonCodeOver}>{text.precheck}</button>
    </form>
    {visibleFacts && <section className="adm-financial-repair-facts" aria-label={text.reference.title}>
      <h3>{text.reference.title}</h3>
      <dl>
        <div><dt>{text.packageID}</dt><dd><code>{visibleFacts.packageID}</code></dd></div>
        <div><dt>{text.orderID}</dt><dd><code>{visibleFacts.orderID}</code></dd></div>
        <div><dt>{text.reference.wallet}</dt><dd><code>{visibleFacts.critical.wallet_id}</code></dd></div>
        <div><dt>{text.reference.transaction}</dt><dd><code>{visibleFacts.critical.payment_transaction_id}</code></dd></div>
      </dl>
    </section>}
    {composition && <section className="adm-financial-repair-facts" aria-label={text.composition.title}>
      <h3>{text.composition.title}</h3><p>{text.composition.scope}</p>
      <div className="adm-table-wrap"><table className="adm-table">
        <thead><tr><th scope="col">{text.composition.type}</th><th scope="col">{text.composition.before}</th><th scope="col">{text.composition.after}</th></tr></thead>
        <tbody>{composition.rows.map(([kind, before, after]) => <tr key={kind}>
          <th scope="row">{text.types[kind]}</th><td data-label={text.composition.before}>{showCredits(before)}</td><td data-label={text.composition.after}>{showCredits(after)}</td>
        </tr>)}</tbody>
      </table></div>
      <dl>
        <div><dt>{text.composition.amount}</dt><dd>{showCredits(composition.transfer)}</dd></div>
        <div><dt>{text.composition.total}</dt><dd>{showCredits(composition.total)}</dd></div>
      </dl>
    </section>}
    {(projection || operation.unknown) && <>
      <label>{text.operatorReason}<textarea disabled={locked} maxLength={2000} value={pending?.reason || reason} onChange={event => setReason(event.target.value)} /></label>
      <button disabled={reading || operation.busy || operation.blocked || !operation.unknown && [...reason.trim()].length < 10} onClick={() => void submit()}>{operation.unknown ? copy.retry : text.submit}</button>
    </>}
    {receipt && <article><h3>{text.approvalRecorded}</h3><p>{receipt.approval_request_id} · {text.approvalStatuses[receipt.status]}</p><p>{text.noFinancialAck}</p></article>}
  </section>;
}
