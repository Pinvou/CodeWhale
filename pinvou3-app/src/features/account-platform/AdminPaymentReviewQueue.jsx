import { statusLabel } from './status-label.js';
import React, { useEffect, useState } from 'react';
import { adminLayoutCopy, accountStatusCopy } from '../../shared/i18n.js';
import { requireFinanceCollection } from './finance-contract.js';
import { isFinanceSnapshotExpired, loadFinancePage } from './admin-finance-pagination.js';
import { useFinanceOperation } from './useFinanceOperation.js';
import { paymentAnomalyResolutions } from './payment-anomaly-resolutions.js';
import { freshLatePaymentQuery, latePaymentQueryAuthority } from './late-payment-query-contract.js';
import { useLatePaymentQuery } from './useLatePaymentQuery.js';
import { AdminFinanceFilters, useFinanceReadRoute, useFinanceRouteLoader } from './AdminFinanceFilters.jsx';
import { LatePaymentQueryConfirmation } from './LatePaymentQueryConfirmation.jsx';
import { AdminLateReviewFilters } from './AdminLateReviewFilters.jsx';
import { AdminLateReviewDeadlineTools, AdminLateReviewDeadlines } from './AdminLateReviewDeadlines.jsx';
import { normalizeAnomalyStatus, anomalyStatusText } from './anomaly-queue-status.js';
import { AdminPaymentAnomalyTable } from './AdminPaymentAnomalyTable.jsx';

export function AdminPaymentReviewQueue({ client, copy, me, anomaly = false, route, language = 'zh' }) {
  const filters = useFinanceReadRoute(anomaly ? 'anomalies' : 'late', route);
  const attemptedDetail = React.useRef('');
  const [page, setPage] = useState(null);
  const queueRoute = Boolean(route);
  const lateRoute = queueRoute && !anomaly;
  const queueWords = adminLayoutCopy[language][anomaly ? 'anomalyQueueResults' : 'lateReviewResults'];
  const anomalyStatusCopy = anomaly ? adminLayoutCopy[language].anomalyQueueStatus : null;
  const [queueList, setQueueList] = useState({ kind: 'loading' });
  const recoveredQueuePage = React.useRef(null);
  const [cursor, setCursor] = useState('');
  const [previous, setPrevious] = useState([]);
  useEffect(() => { setPrevious([]); }, [filters.filterSignature]);
  const [status, setStatus] = useState('');
  const [detail, setDetail] = useState(null);
  const [reason, setReason] = useState('');
  const [nextReview, setNextReview] = useState('');
  const [resolution, setResolution] = useState('');
  const [completionProof, setCompletionProof] = useState('PAYMENT_REPAIR');
  const [completionReference, setCompletionReference] = useState('');
  const [queryConfirmation, setQueryConfirmation] = useState(null);
  const permissions = me.platform_permissions || [];
  const owner = `${me.user_id}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`;
  const operation = useFinanceOperation(owner, copy, client, anomaly ? 'anomaly-review' : 'late-review');
  const list = useFinanceRouteLoader({ route, signature: filters.signature, client, owner, operation, load: () => load(filters.query.cursor), reset: () => { setPage(null); if (queueRoute) setQueueList({ kind: 'loading' }); } });
  const queryAuthority = latePaymentQueryAuthority(me);
  const query = useLatePaymentQuery(me.user_id, copy, client, !anomaly, queryAuthority);
  const busy = operation.busy || query.busy;
  const blocked = operation.unknown || query.unknown;
  const legacyQuery = operation.unknown && operation.pendingFacts?.action === 'query';
  const canQuery = permissions.includes('platform.billing.orders.sync');
  useEffect(() => { setQueryConfirmation(null); }, [client, owner, anomaly, query.retired]);
  const canRepair = permissions.includes('platform.billing.repairs.request');
  const load = async (next = cursor) => {
    const readQuery = { ...(route ? filters.query : { status }), cursor: next };
    if (!queueRoute) {
      const result = await list.read(() => loadFinancePage(readQuery, filters => anomaly ? client.paymentAnomalies(filters) : client.latePaymentReviews(filters), () => { setCursor(''); setPrevious([]); }));
      if (result) { setPage(result.value); setCursor(result.query.cursor); }
      return;
    }
    const epoch = client.financeSessionEpoch || 0, queryKey = JSON.stringify(readQuery);
    const result = await list.read(async () => {
      const recovered = recoveredQueuePage.current;
      recoveredQueuePage.current = null;
      if (recovered?.client === client && recovered.owner === owner && recovered.epoch === epoch && recovered.anomaly === anomaly && recovered.queryKey === queryKey) return { kind: 'ready', ...recovered.result, recovered: true };
      setQueueList({ kind: 'loading' }); setPage(null);
      let restarted = false;
      try {
        const current = await loadFinancePage(readQuery, query => anomaly ? client.paymentAnomalies(query) : client.latePaymentReviews(query), () => { restarted = true; });
        return { kind: 'ready', ...current, restarted };
      } catch (reason) {
        const kind = reason?.status === 403 ? 'denied' : isFinanceSnapshotExpired(reason) ? 'cursorExpired' : reason?.code === 'PAGINATION_CURSOR_INVALID' ? 'cursorInvalid' :
          reason?.code === 'INVALID_FILTER' ? 'filterInvalid' : 'unavailable';
        return { kind, restarted };
      }
    });
    if (!result) return;
    if (result.restarted) { setCursor(''); setPrevious([]); }
    if (result.kind !== 'ready') { setQueueList(result); return; }
    if (result.restarted) {
      // This response was already read for the first batch of the same query.
      // Replace the expired address without fetching the same batch again.
      recoveredQueuePage.current = { client, owner, epoch, anomaly, queryKey: JSON.stringify(result.query), result: { value: result.value, query: result.query } };
      filters.move(result.query, filters.selectedID || null, 'replace');
      return;
    }
    setPage(result.value); setCursor(result.query.cursor);
    setQueueList({ kind: 'ready', recovered: Boolean(result.recovered) });
  };
  useEffect(() => { if (!route) void load(''); }, [client, owner, anomaly]); // eslint-disable-line react-hooks/exhaustive-deps
  const open = async id => {
    let started = false;
    const value = await operation.read(async () => {
      started = true;
      const result = await (anomaly ? client.paymentAnomaly(id) : client.latePaymentReview(id));
      if (result?.case?.case_id !== id) throw new Error('INVALID_FINANCE_ACK');
      return result;
    });
    if (value?.case?.case_id === id) { setDetail(value); setReason(''); setResolution(''); setCompletionProof(value.financial_completion_proofs?.[0]?.proof_kind || 'NO_FINANCIAL_EFFECT'); setCompletionReference(value.financial_completion_proofs?.[0]?.reference_id || ''); }
    return started;
  };
  useEffect(() => {
    const id = route && filters.selectedID;
    if (id && !busy && !blocked && attemptedDetail.current !== id) { attemptedDetail.current = id; void open(id).then(started => { if (!started && attemptedDetail.current === id) attemptedDetail.current = ''; }); }
    if (!id) { attemptedDetail.current = ''; if (route && !blocked) setDetail(null); }
  }, [client, owner, filters.selectedID, busy, blocked, operation.error, page]);
  const execute = (facts, key) => {
    switch (facts.action) {
      case 'claim': return anomaly ? client.claimPaymentAnomaly(facts.id, key) : client.claimLatePaymentReview(facts.id);
      case 'query': return client.queryLatePaymentReview(facts.id, facts, key);
      case 'schedule': return client.scheduleLatePaymentReview(facts.id, facts.nextReview, facts.reason, key, facts.commandProtocol);
      case 'archive': return client.archiveLatePaymentReview(facts.id, key, facts.commandProtocol);
      case 'provider-blind': return client.escalateProviderBlind(facts.id, facts.reason, key, facts.commandProtocol);
      case 'resolve': return client.resolvePaymentAnomaly(facts.id, facts.resolution, facts.reason, key);
      case 'propose-completion': return client.proposePaymentFinancialCompletion(facts.id, facts.proofKind, facts.referenceID, facts.reason, key);
      case 'confirm-completion': return client.confirmPaymentFinancialCompletion(facts.id, facts.proposalID, facts.reason, key);
      default: throw new Error('INVALID_FINANCE_ACK');
    }
  };
  const act = async action => {
    if (query.unknown || query.busy) return;
    // Capture the user's local wall time only for a new invocation. Its UTC
    // instant is immutable with the key even if the system time zone changes.
    const scheduleDate = !operation.pendingFacts && action === 'schedule' ? new Date(nextReview) : null;
    const facts = operation.pendingFacts || { action, id: detail?.case?.case_id, reason: reason.trim(), resolution, ...(action === 'propose-completion' ? { proofKind: completionProof, referenceID: completionProof === 'NO_FINANCIAL_EFFECT' ? '' : completionReference.trim() } : {}), ...(action === 'confirm-completion' ? { proposalID: detail?.financial_completion_proposal?.proposal_id } : {}), nextReview: scheduleDate && Number.isFinite(scheduleDate.getTime()) ? scheduleDate.toISOString() : nextReview, commandProtocol: action === 'schedule' || action === 'provider-blind' || action === 'archive' ? 1 : undefined };
    if (!facts.id || (!operation.pendingFacts && action === 'resolve' && !paymentAnomalyResolutions(detail?.case?.anomaly_type).includes(resolution)) || (!operation.unknown && !globalThis.confirm(copy.confirmAction))) return;
    const value = await operation.mutate(facts, async (current, key) => {
      const receipt = await execute(current, key);
      if ((receipt?.case_id || receipt?.case?.case_id) !== current.id) throw new Error('INVALID_FINANCE_ACK');
      return receipt;
    });
    if (value) { await open(facts.id); await load(); }
  };
  const item = !route || blocked || detail?.case?.case_id === filters.selectedID ? detail?.case : null;
  const reviewStatus = value => anomaly ? value.status : value.review_status;
  const reviewLabel = value => anomaly && ['OPEN', 'RESOLVED'].includes(normalizeAnomalyStatus(value)) ? anomalyStatusText(anomalyStatusCopy, value) : statusLabel(value, language, anomaly ? accountStatusCopy[language].anomalies : accountStatusCopy[language].latePayments, true);
  const statuses = anomaly ? ['OPEN', 'RESOLVED'] : ['REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND', 'ARCHIVED_CONFIRMED_UNPAID', 'RESOLVED_PAID'];
  const active = item && (anomaly ? item.status === 'OPEN' : ['REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND'].includes(item.review_status));
  const owned = item?.owner_id === me.user_id;
  const completionEligible = anomaly && item?.status === 'RESOLVED' && item.resolution_code === 'ESCALATE_FINANCIAL_REPAIR' && item.financial_action_status === 'PENDING' && ['DUPLICATE_PAYMENT', 'LATE_PAYMENT_CONFLICT', 'PAYMENT_FACT_CONFLICT', 'ILLEGAL_TRANSITION'].includes(item.anomaly_type);
  const completionProposal = detail?.financial_completion_proposal;
  const resolutions = paymentAnomalyResolutions(item?.anomaly_type);
  const confirmQuery = () => {
    if (busy || query.retired || (operation.unknown && !legacyQuery) || anomaly || !canQuery || !active || !owned) return;
    setQueryConfirmation({ facts: freshLatePaymentQuery(item, me.user_id, queryAuthority), owner, epoch: client.financeSessionEpoch || 0 });
  };
  const performQuery = async () => {
    const original = queryConfirmation;
    setQueryConfirmation(null);
    if (!original || busy || query.retired || (operation.unknown && !legacyQuery) || anomaly || !canQuery || !active || !owned || original.owner !== owner ||
      original.epoch !== (client.financeSessionEpoch || 0) || original.facts.id !== item?.case_id) return;
    const value = await query.begin(original.facts);
    if (value) { await open(original.facts.id); await load(); }
  };
  const retryQuery = async key => {
    const value = await query.retry(key);
    if (value) { await open(value.case_id); await load(); }
  };

  const hasQueueFilters = Boolean((!anomaly && filters.query.overdue === 'true') ||
    ['q', 'status', 'severity', 'owner_id', 'provider_id', 'created_from', 'created_to', ...(anomaly ? ['anomaly_type'] : ['entered_from', 'entered_to', 'next_review_before'])]
      .some(key => filters.query[key]?.trim().replace(/^\p{White_Space}+|\p{White_Space}+$/gu, '')));
  const anomalyConditions = anomaly ? ['q','anomaly_type','severity','owner_id','provider_id','created_from','created_to']
    .filter(key => filters.query[key]?.trim()).map(key => ({ key, label: adminLayoutCopy[language].financeLists[key], value: filters.query[key] })) : [];
  const clearQueueFilters = () => { setPrevious([]); filters.move({ limit: filters.query.limit, cursor: '' }, null); };
  const emptyMessage = cursor ? queueWords.emptyBatch : hasQueueFilters ? queueWords.emptyFiltered : queueWords.empty;
  const clearWhenFiltered = !cursor && hasQueueFilters && <div className="adm-row-actions"><button type="button" onClick={clearQueueFilters} disabled={busy || blocked}>{queueWords.clear}</button></div>;
  const retryQueueRead = () => {
    const first = queueList.restarted || ['cursorInvalid', 'cursorExpired'].includes(queueList.kind);
    if (first) setPrevious([]);
    filters.move({ ...filters.query, ...(first ? { cursor: '' } : {}) }, filters.selectedID || null, 'replace');
  };
  const queueFailure = queueRoute && !['loading', 'ready'].includes(queueList.kind);
  return <section><h3>{anomaly ? copy.resolve : copy.sla}</h3>{route && !anomaly ? <AdminLateReviewFilters language={language} draft={filters.draft} applied={filters.query} setDraft={filters.setDraft} disabled={busy || blocked} onApply={value => { setPrevious([]); filters.move({ ...value, cursor: '' }, null); }} /> : route ? <AdminFinanceFilters kind="anomalies" language={language} value={filters.draft} onChange={filters.setDraft} disabled={busy || blocked} onSubmit={() => { setPrevious([]); filters.move({ ...filters.draft, cursor: '' }, null); }} /> : <form onSubmit={event => { event.preventDefault(); setPrevious([]); void load(''); }}><label>{copy.status}<select value={status} onChange={event => setStatus(event.target.value)}><option value="">{copy.more}</option>{statuses.map(value => <option key={value} value={value}>{reviewLabel(value)}</option>)}</select></label><button disabled={busy}>{copy.refresh}</button></form>}
    {route && anomaly && <div className="adm-inline-message">
      <p><strong>{anomalyStatusCopy.applied}:</strong> {anomalyStatusText(anomalyStatusCopy, filters.query.status, true)}. {anomalyStatusCopy.snapshotNote}</p>
      {normalizeAnomalyStatus(filters.query.status) && <div className="adm-row-actions"><button type="button" disabled={busy || blocked} onClick={() => { setPrevious([]); filters.move({ ...filters.query, status: '', cursor: '' }, null); }}>{anomalyStatusCopy.showAll}</button></div>}
    </div>}
    {route && anomaly && (anomalyConditions.length > 0 || hasQueueFilters) && <div className="adm-inline-message adm-anomaly-query-summary">
      {anomalyConditions.length > 0 && <><p><strong>{queueWords.applied}:</strong></p><ul>{anomalyConditions.map(({ key, label, value }) => <li key={key}>{label} <bdi>{value}</bdi></li>)}</ul></>}
      {hasQueueFilters && (!page || page.items.length > 0 || cursor) && <div className="adm-row-actions"><button type="button" disabled={busy || blocked} onClick={clearQueueFilters}>{queueWords.clear}</button></div>}
    </div>}
    {route && !anomaly && <AdminLateReviewDeadlineTools language={language} query={filters.query} snapshot={page?.snapshot_as_of} disabled={busy || blocked} onChange={value => { setPrevious([]); filters.move({ ...value, cursor: '' }, null); }} />}
    {queueRoute && queueList.kind === 'loading' && <p className="adm-inline-message" role="status">{queueWords.loading}</p>}
    {queueFailure && <div className="adm-inline-message error">
      <p role="alert">{queueWords[queueList.kind]}</p>
      {queueList.kind !== 'denied' && <div className="adm-row-actions"><button type="button" onClick={retryQueueRead} disabled={busy || blocked}>{queueList.restarted || ['cursorInvalid', 'cursorExpired'].includes(queueList.kind) ? queueWords.firstBatch : copy.retry}</button></div>}
    </div>}
    {queueRoute && queueList.recovered && <p className="adm-inline-message" role="status">{queueWords.recovered}</p>}
    {busy && (!queueRoute || queueList.kind !== 'loading') && <p role="status">{copy.loading}</p>}{operation.error && !(legacyQuery && operation.error === copy.unknown) && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}
    {operation.unknown && <div data-retained-finance-command><code>{operation.pendingFacts?.id} / {operation.pendingKey}</code><button disabled={busy || query.unknown} onClick={() => void act('')}>{copy.retry}</button></div>}
    {legacyQuery && <p role="alert">{copy.queryUnknown} <code>{operation.pendingKey}</code></p>}
    {query.message && <p role="status">{query.message}</p>}
    {query.unknownQueries.map(value => <div key={value.key} role="alert"><p>{copy.queryUnknown}</p><code>{value.facts.id} / {value.key}</code><button disabled={busy || query.retired} onClick={() => void retryQuery(value.key)}>{copy.retry}</button></div>)}
    {page && anomaly && <AdminPaymentAnomalyTable items={page.items} emptyState={queueRoute && !page.items.length ? <><strong>{emptyMessage}</strong>{clearWhenFiltered}</> : null} language={language} detailsLabel={copy.details} disabled={busy || (operation.unknown && !legacyQuery)} onView={id => { if (route) { attemptedDetail.current = id; filters.move(filters.query, id); } void open(id); }} />}
    {page && !anomaly && <table className="adm-table"><thead><tr><th>{copy.orderID}</th><th>{copy.status}</th><th>{copy.severity}</th><th>{copy.sla}</th><th>{copy.actions}</th></tr></thead><tbody>{lateRoute && !page.items.length && <tr><td colSpan={5} className="adm-late-empty-cell">
      <strong>{emptyMessage}</strong>{clearWhenFiltered}
    </td></tr>}{page.items.map(value => <tr key={value.case_id}><td data-label={anomaly ? undefined : copy.orderID}>{value.payment_order_id || value.case_id}</td><td data-label={anomaly ? undefined : copy.status}>{reviewLabel(reviewStatus(value))}</td><td data-label={anomaly ? undefined : copy.severity}>{value.severity}</td><td data-label={anomaly ? undefined : copy.sla}>{anomaly ? <>{value.claim_due_at}<br />{value.resolution_due_at || value.next_review_at}</> : <AdminLateReviewDeadlines value={value} language={language} snapshot={page.snapshot_as_of} />}</td><td data-label={anomaly ? undefined : copy.actions}><button disabled={busy || (operation.unknown && !legacyQuery)} onClick={() => { if (route) { attemptedDetail.current = value.case_id; filters.move(filters.query, value.case_id); } void open(value.case_id); }}>{copy.details}</button></td></tr>)}</tbody></table>}
    <button disabled={busy || !previous.length} onClick={() => { const values = [...previous]; const value = values.pop(); setPrevious(values); if (route) filters.move({ ...filters.query, cursor: value }, null); else void load(value); }}>{copy.previous}</button><button disabled={busy || !page?.next_cursor} onClick={() => { setPrevious(values => [...values, cursor]); if (route) filters.move({ ...filters.query, cursor: page.next_cursor }, null); else void load(page.next_cursor); }}>{copy.next}</button>
    {route && filters.selectedID && <nav><button disabled={busy || blocked} onClick={() => filters.move(filters.query, null)}>{adminLayoutCopy[language].backToDirectory}</button><code>{filters.selectedID}</code>{operation.error && !blocked && <button disabled={busy} onClick={() => void open(filters.selectedID)}>{copy.retry}</button>}</nav>}
    {item && <article><h4>{item.case_id}</h4><p>{reviewLabel(reviewStatus(item))} / {item.severity} / {copy.owner}: {item.owner_id || copy.empty}</p>{anomaly ? <p>{item.claim_due_at}<br />{item.first_response_due_at}<br />{item.mitigation_due_at}<br />{item.resolution_due_at}</p> : <AdminLateReviewDeadlines value={item} language={language} detailed />}
      <label>{copy.reason}<textarea disabled={busy || blocked} maxLength={2000} value={reason} onChange={event => setReason(event.target.value)} /></label>
      {canQuery && active && !owned && <button disabled={busy || blocked} onClick={() => void act('claim')}>{copy.claimCase}</button>}
      {owned && active && !anomaly && canQuery && <><button disabled={busy || blocked || query.retired} onClick={confirmQuery}>{copy.query}</button>{(query.unknown || legacyQuery) && <button disabled={busy || query.retired || (operation.unknown && !legacyQuery)} onClick={confirmQuery}>{copy.newQuery}</button>}<label>{copy.nextReview}<input type="datetime-local" value={nextReview} onChange={event => setNextReview(event.target.value)} /></label><button disabled={busy || blocked || !nextReview || !reason.trim()} onClick={() => void act('schedule')}>{copy.schedule}</button><button disabled={busy || blocked || item.review_status !== 'REVIEW_REQUIRED_LATE_PAYMENT'} onClick={() => void act('archive')}>{copy.archive}</button><button disabled={busy || blocked || reason.trim().length < 10} onClick={() => void act('provider-blind')}>{copy.escalate}</button></>}
      {owned && active && anomaly && canRepair && <><label>{copy.resolution}<select value={resolution} onChange={event => setResolution(event.target.value)}><option value="">{copy.resolution}</option>{resolutions.map(value => <option key={value} value={value}>{copy.resolutions[value]}</option>)}</select></label>{resolution === 'RECONCILE_REFUND' && <p>{copy.refundReconcileHelp}</p>}<button disabled={busy || blocked || !resolutions.includes(resolution) || reason.trim().length < 10} onClick={() => void act('resolve')}>{copy.resolve}</button></>}
      {completionEligible && <section aria-label={copy.financialCompletion}>
        <h5>{copy.financialCompletion}</h5><p>{copy.financialCompletionHelp}</p>
        {completionProposal && <div><p>{copy.completionProposer}: <code>{completionProposal.proposer_id}</code></p><p>{copy.completionProofs[completionProposal.facts.proof_kind]} / <code>{completionProposal.proposal_id}</code></p>
          <dl><dt>{copy.orderID}</dt><dd>{completionProposal.facts.payment_order_id}</dd><dt>{copy.transaction}</dt><dd>{completionProposal.facts.provider_transaction_id || copy.empty}</dd><dt>{copy.amountFen}</dt><dd>{completionProposal.facts.amount_fen} {completionProposal.facts.currency}</dd><dt>{copy.reason}</dt><dd>{completionProposal.reason}</dd></dl>
          <dl><dt>{copy.completionReference}</dt><dd>{completionProposal.facts.reference_id}</dd><dt>{copy.provider}</dt><dd>{completionProposal.facts.provider_id} / {completionProposal.facts.provider_version}</dd><dt>{copy.package}</dt><dd>{completionProposal.facts.receipt.package_id || copy.empty}</dd><dt>{copy.completionJournal}</dt><dd>{completionProposal.facts.receipt.journal_id || copy.empty}</dd><dt>{copy.completionDigest}</dt><dd><code>{completionProposal.facts.facts_digest}</code></dd></dl>
          {canRepair && <button disabled={busy || blocked || completionProposal.proposer_id === me.user_id || completionProposal.case_version !== item.version || reason.trim().length < 10} onClick={() => void act('confirm-completion')}>{copy.confirmFinancialCompletion}</button>}
        </div>}
        {canRepair && <><label>{copy.completionEvidence}<select disabled={busy || blocked} value={completionProof} onChange={event => { setCompletionProof(event.target.value); setCompletionReference(detail.financial_completion_proofs?.find(value => value.proof_kind === event.target.value)?.reference_id || ''); }}>{Object.entries(copy.completionProofs).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label>
          {completionProof !== 'NO_FINANCIAL_EFFECT' && <label>{copy.completionReference}<select disabled={busy || blocked} value={completionReference} onChange={event => setCompletionReference(event.target.value)}><option value="">{copy.empty}</option>{(detail.financial_completion_proofs || []).filter(value => value.proof_kind === completionProof).map(value => <option key={value.reference_id} value={value.reference_id}>{value.reference_id} / {value.amount_fen} {value.currency}</option>)}</select></label>}
          <button disabled={busy || blocked || reason.trim().length < 10 || (completionProof !== 'NO_FINANCIAL_EFFECT' && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(completionReference.trim()))} onClick={() => void act('propose-completion')}>{copy.proposeFinancialCompletion}</button>
        </>}
      </section>}
      <ul>{(detail.events || []).map(value => <li key={value.event_id || value.id}>{value.event_type} / {value.created_at}</li>)}</ul>
    </article>}
    {queryConfirmation && <LatePaymentQueryConfirmation copy={copy} caseID={queryConfirmation.facts.id}
      newReview={query.unknown || legacyQuery} onCancel={() => setQueryConfirmation(null)} onConfirm={() => void performQuery()} />}
  </section>;
}
