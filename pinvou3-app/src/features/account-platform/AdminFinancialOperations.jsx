import React, { useCallback, useEffect, useState } from 'react';
import { useAdminOperation } from './use-admin-operation.js';
import { formatMoney, formatPoints } from './credit-units.js';
import { requireAdminItems, requireAdminRecord } from './admin-acknowledgements.js';

const date = value => value ? new Date(value).toLocaleString() : '—';
const snapshot = value => JSON.stringify(value ?? null, null, 2);

function Records({ items, columns, rowKey, copy }) {
  if (!items.length) return <div className="adm-empty">{copy.noData}</div>;
  return <div className="adm-table-wrap"><table className="adm-table"><thead><tr>{columns.map(column => <th key={column.key}>{column.label}</th>)}</tr></thead><tbody>{items.map(item => <tr key={item[rowKey]}>{columns.map(column => <td key={column.key}>{column.render ? column.render(item) : item[column.key] ?? '—'}</td>)}</tr>)}</tbody></table></div>;
}

function Evidence({ title, value }) {
  return <article className="adm-panel"><h3>{title}</h3><pre className="adm-json-detail">{snapshot(value)}</pre></article>;
}

export function AdminFinancialOperations({ tab, client, copy, language, onAuthorizationError }) {
  const ops = useAdminOperation(onAuthorizationError, 'billing');
  const [items, setItems] = useState([]);
  const [filters, setFilters] = useState({ status:'', provider_id:'', owner_id:'', entered_from:'', entered_to:'', next_review_before:'', overdue:false, anomaly_type:'', severity:'' });
  const [detail, setDetail] = useState(null);
  const [loaded, setLoaded] = useState(false);
  const load = useCallback(async ({ signal, isCurrent }, submitted = filters) => {
    const value = tab === 'receivables' ? await client.receivables(signal)
      : tab === 'refunds' ? await client.refunds(signal)
        : tab === 'lateReviews' ? await client.latePaymentReviews(submitted, signal)
          : await client.paymentAnomalies(submitted, signal);
    const records = requireAdminItems(value);
    if (isCurrent()) { setItems(records); setLoaded(true); }
  }, [client, tab, filters]);
  useEffect(() => {
    setDetail(null); setItems([]); setLoaded(false);
    void ops.run(context => load(context, {}));
    // The initial read belongs to this mounted tab. Filter changes are submitted explicitly.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, tab]);
  const show = item => ops.run(async ({ signal, isCurrent }) => {
    const value = tab === 'receivables' ? await client.receivableAllocations(item.receivable_id, signal)
      : tab === 'lateReviews' ? await client.latePaymentReview(item.case_id, signal)
        : await client.paymentAnomaly(item.case_id, signal);
    if (tab === 'receivables') requireAdminItems(value);
    else requireAdminRecord(value.case, 'case_id', item.case_id);
    if (isCurrent()) setDetail({ item, value });
  });
  const mutate = action => ops.run(async context => {
    const value = await action();
    requireAdminRecord(value, 'case_id', detail.item.case_id);
    if (!context.isCurrent()) return;
    const current = tab === 'lateReviews' ? await client.latePaymentReview(detail.item.case_id, context.signal)
      : await client.paymentAnomaly(detail.item.case_id, context.signal);
    requireAdminRecord(current.case, 'case_id', detail.item.case_id);
    if (context.isCurrent()) setDetail({ item:current.case, value:current });
    await load(context);
    return 'completed';
  }, true);
  const reason = label => String(window.prompt(label) || '').trim();
  const fields = tab === 'lateReviews' ? ['status','provider_id','owner_id','entered_from','entered_to','next_review_before'] : ['anomaly_type','status','severity','owner_id'];
  const labels = { status:copy.accounts.status, provider_id:copy.billing.provider, owner_id:copy.billing.ownerID, entered_from:copy.billing.enteredFrom, entered_to:copy.billing.enteredTo, next_review_before:copy.billing.nextReviewBefore, anomaly_type:copy.billing.anomalyType, severity:copy.billing.severity };
  const detailID = tab === 'receivables' ? 'receivable_id' : 'case_id';
  const common = [{ key:'status', label:copy.accounts.status }, { key:'created_at', label:copy.accounts.created, render:item => date(item.created_at) }];
  const columns = tab === 'receivables' ? [
    {key:'receivable_id',label:copy.billing.receivable}, {key:'source_type',label:copy.billing.source},
    ...['original','settled','outstanding'].map(key => ({ key:`${key}_microcredits`, label:copy.billing[`${key}Receivable`], render:item => formatPoints(item[`${key}_microcredits`], language) })), ...common,
    {key:'actions',label:copy.accounts.actions,render:item => <button disabled={ops.busy} onClick={() => show(item)}>{copy.billing.viewAllocations}</button>},
  ] : tab === 'refunds' ? [
    {key:'operation_id',label:copy.billing.reference},{key:'operation_type',label:copy.billing.refundType},{key:'payment_order_id',label:copy.billing.order},
    {key:'payment_transaction_id',label:copy.billing.transactions},{key:'entitlement_package_id',label:copy.billing.package},
    {key:'amount_fen',label:copy.billing.refundCash,render:item => formatMoney(item.amount_fen, language)},...common,
  ] : [
    {key:'case_id',label:copy.billing.reference}, {key:tab === 'lateReviews' ? 'review_status' : 'anomaly_type',label:tab === 'lateReviews' ? copy.accounts.status : copy.billing.anomalyType},
    {key:'severity',label:copy.billing.severity},{key:'owner_id',label:copy.billing.owner},
    {key:tab === 'lateReviews' ? 'next_review_at' : 'resolution_due_at',label:tab === 'lateReviews' ? copy.billing.nextReview : copy.billing.resolutionDue,render:item => date(tab === 'lateReviews' ? item.next_review_at : item.resolution_due_at)},
    {key:'actions',label:copy.accounts.actions,render:item => <button disabled={ops.busy} onClick={() => show(item)}>{copy.billing.reviewDetail}</button>},
  ];
  const current = detail?.value?.case;
  return <section>
    {ops.failed && <p role="alert">{copy.billing.actionFailed}</p>}
    {ops.noticeKey && <p role="status">{copy.billing.completed}</p>}
    {detail ? <>
      <button disabled={ops.busy} onClick={() => { ops.invalidate(); setDetail(null); }}>{copy.billing.back}</button>
      <h3>{copy.billing[tab === 'lateReviews' ? 'reviewDetail' : tab === 'anomalies' ? 'anomalyDetail' : 'allocations']}</h3>
      {tab === 'receivables' ? <Evidence title={copy.billing.allocations} value={detail.value.items} /> : <>
        <p><code>{current.case_id}</code> · {current.review_status || current.status} · {current.owner_id || '—'}</p>
        {tab === 'anomalies' && <p>{copy.billing.financialAction}: {current.financial_action_status || '—'} · {copy.billing.reviewResult}: {current.resolution_code || '—'}</p>}
        {tab === 'lateReviews' && <p>{copy.billing.lastQuery}: {date(current.last_query_at)} · {current.last_query_result || '—'}</p>}
        <div className="adm-row-actions">
          <button disabled={ops.busy} onClick={() => mutate(() => tab === 'lateReviews' ? client.claimLatePaymentReview(current.case_id) : client.claimPaymentAnomaly(current.case_id))}>{copy.billing.claimReview}</button>
          {tab === 'lateReviews' ? <>
            <button disabled={ops.busy} onClick={() => mutate(() => client.queryLatePaymentReview(current.case_id))}>{copy.billing.queryProvider}</button>
            <button disabled={ops.busy} onClick={() => { const at = window.prompt(copy.billing.nextReviewPrompt); if (!at || !Number.isFinite(Date.parse(at))) return; const why = reason(copy.billing.reviewReason); if (why.length >= 10) void mutate(() => client.scheduleLatePaymentReview(current.case_id, at, why)); }}>{copy.billing.scheduleReview}</button>
            <button disabled={ops.busy} onClick={() => mutate(() => client.archiveLatePaymentReview(current.case_id))}>{copy.billing.archiveUnpaid}</button>
            <button disabled={ops.busy} onClick={() => { const why = reason(copy.billing.providerBlindReason); if (why.length >= 10) void mutate(() => client.escalateProviderBlind(current.case_id, why)); }}>{copy.billing.providerBlind}</button>
          </> : <button disabled={ops.busy} onClick={() => { const resolution = reason(copy.billing.anomalyResolutionPrompt); if (!['CONFIRM_CHARGEBACK','CONFIRM_FORCED_REFUND','CONFIRM_PAYMENT','DISMISS','ESCALATE_FINANCIAL_REPAIR'].includes(resolution)) return; const why = reason(copy.billing.anomalyResolutionReason); if (why.length >= 10) void mutate(() => client.resolvePaymentAnomaly(current.case_id, resolution, why)); }}>{copy.billing.resolveCase}</button>}
        </div>
        {tab === 'lateReviews' ? <><Evidence title={copy.billing.providerSnapshot} value={current.provider_capability_snapshot} /><Evidence title={copy.billing.policySnapshot} value={current.policy_snapshot} /><Evidence title={copy.billing.evidenceTimeline} value={detail.value.evidence} /></> : <Evidence title={copy.billing.resourceSnapshot} value={current.resource_snapshot} />}
        <Evidence title={copy.billing.operatorTimeline} value={detail.value.events} />
      </>}
    </> : <>
      {['lateReviews','anomalies'].includes(tab) && <form className="adm-ops-form" onSubmit={event => { event.preventDefault(); void ops.run(context => load(context)); }}>
        {fields.map(key => <label key={key}>{labels[key]}<input type={key.endsWith('_from') || key.endsWith('_to') || key.endsWith('_before') ? 'datetime-local' : 'text'} value={filters[key]} onChange={event => setFilters(current => ({ ...current, [key]:event.target.value }))} /></label>)}
        {tab === 'lateReviews' && <label><input type="checkbox" checked={filters.overdue} onChange={event => setFilters(current => ({ ...current, overdue:event.target.checked }))} />{copy.billing.overdueOnly}</label>}
        <button disabled={ops.busy}>{copy.searchAction}</button>
      </form>}
      {!loaded && ops.busy ? <p>{copy.loading}</p> : <Records items={items} columns={columns} rowKey={tab === 'refunds' ? 'operation_id' : detailID} copy={copy} />}
    </>}
  </section>;
}

export function AdminFinancialEvidence({ title, value }) {
  return <Evidence title={title} value={value} />;
}

export function AdminPaymentOrderDetail({ order, copy, language, busy, refundSupported, onBack, onRefund }) {
  const [packageID, setPackageID] = useState('');
  const [reason, setReason] = useState('');
  const packages = order.entitlement_packages || [];
  const selected = packages.find(item => item.package_id === packageID);
  return <section className="adm-panel">
    <button disabled={busy} onClick={onBack}>{copy.billing.back}</button><h3>{copy.billing.orderDetail}</h3>
    <p><code>{order.order_id}</code> · {order.provider_id} · {order.status} · {order.fulfillment_status}</p>
    <p>{copy.billing.payer}: <code>{order.payer_user_id}</code> · {copy.billing.beneficiary}: <code>{order.beneficiary_organization_id || copy.billing.personalBeneficiary}</code></p>
    <p>{copy.billing.paymentReference}: <code>{order.primary_provider_transaction_id || '—'}</code></p>
    <Evidence title={copy.billing.paymentSummary} value={order.payment_summary} /><Evidence title={copy.billing.refundSummary} value={order.refund_summary} /><Evidence title={copy.billing.chargebackSummary} value={order.chargeback_summary} /><p>{copy.billing.attentionSummary}: {order.attention_summary || '—'}</p>
    <Records copy={copy} items={order.transactions || []} rowKey="payment_transaction_id" columns={[
      {key:'payment_transaction_id',label:copy.billing.transactions},{key:'provider_transaction_id',label:copy.billing.paymentReference},
      {key:'amount_fen',label:copy.billing.paymentAmount,render:item => formatMoney(item.amount_fen, language)}, {key:'status',label:copy.accounts.status}, {key:'fulfillment_status',label:copy.billing.fulfillment},
    ]} />
    <h3>{copy.billing.entitlementPackages}</h3><Records copy={copy} items={packages} rowKey="package_id" columns={[
      {key:'package_id',label:copy.billing.package},{key:'source_payment_transaction_id',label:copy.billing.transactions},{key:'provider_order_reference',label:copy.billing.reference},
      {key:'points_remaining_microcredits',label:copy.billing.available,render:item => formatPoints(item.points_remaining_microcredits, language)},{key:'status',label:copy.accounts.status},
    ]} />
    <form className="adm-ops-form" onSubmit={event => { event.preventDefault(); if (selected?.source_payment_transaction_id && reason.trim()) void onRefund(selected.source_payment_transaction_id, selected.package_id, reason.trim()); }}>
      <label>{copy.billing.package}<select required disabled={busy} value={packageID} onChange={event => setPackageID(event.target.value)}><option value="">—</option>{packages.filter(item => item.source_payment_transaction_id).map(item => <option key={item.package_id} value={item.package_id}>{item.plan_name || item.product_id} · {item.package_id}</option>)}</select></label>
      <label>{copy.billing.reason}<input required disabled={busy} value={reason} onChange={event => setReason(event.target.value)} /></label><button disabled={busy || !refundSupported || !selected?.source_payment_transaction_id}>{copy.billing.refundWholePackage}</button>
    </form><p>{copy.refundCompositionNotice || copy.billing.refundReason}</p>
    <Evidence title={copy.billing.evidence} value={order.evidence} />
  </section>;
}

export function AdminUsageHoldDetail({ value, copy, onBack, busy }) {
  return <section className="adm-panel"><button disabled={busy} onClick={onBack}>{copy.billing.back}</button><h3>{copy.billing.holdEvidence}</h3>
    <Evidence title={copy.billing.holds} value={value.hold} /><Evidence title={copy.billing.componentAllocations} value={value.component_allocations} />
    <Evidence title={copy.billing.allocations} value={value.member_allocations} /><Evidence title={copy.billing.leaseHistory} value={value.lease_events} /><Evidence title={copy.billing.adminActions} value={value.admin_actions} />
  </section>;
}
