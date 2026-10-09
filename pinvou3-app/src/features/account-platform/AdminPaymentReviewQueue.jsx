import React, { useEffect, useState } from 'react';
import { requireFinanceCollection } from './finance-contract.js';
import { loadFinancePage } from './admin-finance-pagination.js';
import { useFinanceOperation } from './useFinanceOperation.js';
import { paymentAnomalyResolutions } from './payment-anomaly-resolutions.js';

export function AdminPaymentReviewQueue({ client, copy, me, anomaly = false }) {
  const [page, setPage] = useState(null);
  const [cursor, setCursor] = useState('');
  const [previous, setPrevious] = useState([]);
  const [status, setStatus] = useState('');
  const [detail, setDetail] = useState(null);
  const [reason, setReason] = useState('');
  const [nextReview, setNextReview] = useState('');
  const [resolution, setResolution] = useState('');
  const permissions = me.platform_permissions || [];
  const owner = `${me.user_id}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`;
  const operation = useFinanceOperation(owner, copy, client, anomaly ? 'anomaly-review' : 'late-review');
  const canQuery = permissions.includes('platform.billing.orders.sync');
  const canRepair = permissions.includes('platform.billing.repairs.request');
  const load = async (next = cursor) => {
    const result = await operation.read(() => loadFinancePage({ status, cursor: next }, filters => anomaly ? client.paymentAnomalies(filters) : client.latePaymentReviews(filters), () => { setCursor(''); setPrevious([]); }));
    if (result) { setPage(result.value); setCursor(result.query.cursor); }
  };
  useEffect(() => { void load(''); }, [client, owner, anomaly]); // eslint-disable-line react-hooks/exhaustive-deps
  const open = async id => {
    const value = await operation.read(() => anomaly ? client.paymentAnomaly(id) : client.latePaymentReview(id));
    if (value?.case?.case_id === id) { setDetail(value); setReason(''); setResolution(''); }
  };
  const execute = (facts, key) => {
    switch (facts.action) {
      case 'claim': return anomaly ? client.claimPaymentAnomaly(facts.id, key) : client.claimLatePaymentReview(facts.id);
      case 'query': return client.queryLatePaymentReview(facts.id);
      case 'schedule': return client.scheduleLatePaymentReview(facts.id, facts.nextReview, facts.reason);
      case 'archive': return client.archiveLatePaymentReview(facts.id);
      case 'provider-blind': return client.escalateProviderBlind(facts.id, facts.reason);
      case 'resolve': return client.resolvePaymentAnomaly(facts.id, facts.resolution, facts.reason, key);
      default: throw new Error('INVALID_FINANCE_ACK');
    }
  };
  const act = async action => {
    const facts = operation.pendingFacts || { action, id: detail?.case?.case_id, reason: reason.trim(), resolution, nextReview };
    if (!facts.id || (!operation.pendingFacts && action === 'resolve' && !paymentAnomalyResolutions(detail?.case?.anomaly_type).includes(resolution)) || (!operation.unknown && !globalThis.confirm(copy.confirmAction))) return;
    const value = await operation.mutate(facts, async (current, key) => {
      const receipt = await execute(current, key);
      if ((receipt?.case_id || receipt?.case?.case_id) !== current.id) throw new Error('INVALID_FINANCE_ACK');
      return receipt;
    });
    if (value) { await open(facts.id); await load(); }
  };
  const item = detail?.case;
  const reviewStatus = value => anomaly ? value.status : value.review_status;
  const statuses = anomaly ? ['OPEN', 'RESOLVED'] : ['REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND', 'ARCHIVED_CONFIRMED_UNPAID', 'RESOLVED_PAID'];
  const active = item && (anomaly ? item.status === 'OPEN' : ['REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND'].includes(item.review_status));
  const owned = item?.owner_id === me.user_id;
  const resolutions = paymentAnomalyResolutions(item?.anomaly_type);
  return <section><h3>{anomaly ? copy.resolve : copy.sla}</h3><form onSubmit={event => { event.preventDefault(); setPrevious([]); void load(''); }}><label>{copy.status}<select value={status} onChange={event => setStatus(event.target.value)}><option value="">{copy.more}</option>{statuses.map(value => <option key={value} value={value}>{value}</option>)}</select></label><button disabled={operation.busy}>{copy.refresh}</button></form>
    {operation.busy && <p role="status">{copy.loading}</p>}{operation.error && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}
    {operation.unknown && <button disabled={operation.busy} onClick={() => void act('')}>{copy.retry}</button>}
    {page && <table><thead><tr><th>{copy.orderID}</th><th>{copy.status}</th><th>{copy.severity}</th><th>{copy.sla}</th><th>{copy.actions}</th></tr></thead><tbody>{page.items.map(value => <tr key={value.case_id}><td>{value.payment_order_id || value.case_id}</td><td>{reviewStatus(value)}</td><td>{value.severity}</td><td>{value.claim_due_at}<br />{value.resolution_due_at || value.next_review_at}</td><td><button disabled={operation.busy || operation.unknown} onClick={() => void open(value.case_id)}>{copy.details}</button></td></tr>)}</tbody></table>}
    <button disabled={operation.busy || !previous.length} onClick={() => { const values = [...previous]; const value = values.pop(); setPrevious(values); void load(value); }}>{copy.previous}</button><button disabled={operation.busy || !page?.next_cursor} onClick={() => { setPrevious(values => [...values, cursor]); void load(page.next_cursor); }}>{copy.next}</button>
    {item && <article><h4>{item.case_id}</h4><p>{reviewStatus(item)} / {item.severity} / {copy.owner}: {item.owner_id || copy.empty}</p><p>{item.claim_due_at}<br />{item.first_response_due_at}<br />{item.mitigation_due_at}<br />{item.resolution_due_at}</p>
      <label>{copy.reason}<textarea disabled={operation.busy || operation.unknown} maxLength={2000} value={reason} onChange={event => setReason(event.target.value)} /></label>
      {canQuery && active && !owned && <button disabled={operation.busy || operation.unknown} onClick={() => void act('claim')}>{copy.claimCase}</button>}
      {owned && active && !anomaly && canQuery && <><button disabled={operation.busy || operation.unknown} onClick={() => void act('query')}>{copy.query}</button><label>{copy.nextReview}<input type="datetime-local" value={nextReview} onChange={event => setNextReview(event.target.value)} /></label><button disabled={operation.busy || operation.unknown || !nextReview || !reason.trim()} onClick={() => void act('schedule')}>{copy.schedule}</button><button disabled={operation.busy || operation.unknown || item.review_status !== 'REVIEW_REQUIRED_LATE_PAYMENT'} onClick={() => void act('archive')}>{copy.archive}</button><button disabled={operation.busy || operation.unknown || reason.trim().length < 10} onClick={() => void act('provider-blind')}>{copy.escalate}</button></>}
      {owned && active && anomaly && canRepair && <><label>{copy.resolution}<select value={resolution} onChange={event => setResolution(event.target.value)}><option value="">{copy.resolution}</option>{resolutions.map(value => <option key={value} value={value}>{copy.resolutions[value]}</option>)}</select></label>{resolution === 'RECONCILE_REFUND' && <p>{copy.refundReconcileHelp}</p>}<button disabled={operation.busy || operation.unknown || !resolutions.includes(resolution) || reason.trim().length < 10} onClick={() => void act('resolve')}>{copy.resolve}</button></>}
      <ul>{(detail.events || []).map(value => <li key={value.event_id || value.id}>{value.event_type} / {value.created_at}</li>)}</ul>
    </article>}
  </section>;
}
