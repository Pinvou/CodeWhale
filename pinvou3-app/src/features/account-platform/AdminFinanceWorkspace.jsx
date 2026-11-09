import React, { useEffect, useState } from 'react';
import { reconciliationCopy, adminLayoutCopy, financeCopy, ledgerRepairCopy, entitlementCorrectionCopy } from '../../shared/i18n.js';
import { AdminEntitlementCorrectionWorkspace } from './AdminEntitlementCorrectionWorkspace.jsx';
import { AdminFinancialRepairDraftForm } from './AdminFinancialRepairDraftForm.jsx';
import { formatBalance } from './balance-view.js';
import { formatFen, integerMoney, requireFinanceCollection, requireFinanceID, wholeRefundTargets } from './finance-contract.js';
import { useFinanceOperation } from './useFinanceOperation.js';
import { walletAdjustmentApprovalDraft, buildPaymentControlApprovalDraft, currentApprovalEditDraft, adjustmentApprovalAction, latestApprovalByTarget, currentPendingAdjustment, walletReplacementEligible } from './admin-finance.js';
import { createAndSubmitFreshGovernedApproval, updateAndSubmitGovernedApproval } from './admin-governed-approval-workflow.js';
import { loadFinancePage } from './admin-finance-pagination.js';
import { secureRequestID } from './request-id.js';
import { financeNotDispatched, financeWriteObserved } from './finance-operation-state.js';
import { approvalBusinessRoute, approvalRequester, approvalCommands } from './admin-approval-actions.js';
import { AdminPaymentReviewQueue } from './AdminPaymentReviewQueue.jsx';
import { ActiveWorkspace, RetainedWorkspace } from './RetainedWorkspace.jsx';
import { AdminReconciliationWorkspace } from './AdminReconciliationWorkspace.jsx';
import { AdminFinanceFilters, useFinanceReadRoute, useFinanceRouteLoader } from './AdminFinanceFilters.jsx';
import { AdminFinanceOverview } from './AdminFinanceOverview.jsx';
import { AdminWalletLedger } from './AdminWalletLedger.jsx';
import { AdminIdentifier } from './AdminIdentifier.jsx';
import { usePortalDialogScope } from './portal-dialog-scope.jsx';
import { AdminFinanceReadWorkspace } from './AdminFinanceReadWorkspace.jsx';
import { approvalLabel } from './admin-approval-presentation.js';
import { AdminOrderStatusSummary } from './AdminOrderStatusSummary.jsx';
import { AdminOrderResults } from './AdminOrderResults.jsx';

function Feedback({ operation, copy }) {
  return <>{operation.busy && <p role="status">{copy.loading}</p>}{operation.error && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}</>;
}

function Payments({ client, copy, language, permissions, owner, route }) {
  const filters = useFinanceReadRoute('orders', route);
  const attemptedDetail = React.useRef(null);
  const visible = usePortalDialogScope(), frameHeading = React.useId();
  const listHeading = React.useRef(null), detailHeading = React.useRef(null);
  const returnSpot = React.useRef(null), previousSelection = React.useRef(null);
  const [listFailure, setListFailure] = useState('');
  const [page, setPage] = useState(null);
  const [query, setQuery] = useState('');
  const [cursor, setCursor] = useState('');
  const [previous, setPrevious] = useState([]);
  useEffect(() => { setPrevious([]); }, [filters.filterSignature]);
  const [detailSnapshot, setDetailSnapshot] = useState(null);
  const detail = detailSnapshot?.value || null;
  const [detailFailure, setDetailFailure] = useState(null);
  const detailEpoch = client.financeSessionEpoch || 0;
  const selectedOrder = route ? filters.selectedID : null;
  const [detailVisit, setDetailVisit] = useState(() => ({ id: selectedOrder, client, owner, epoch: detailEpoch }));
  if (detailVisit.id !== selectedOrder || detailVisit.client !== client || detailVisit.owner !== owner || detailVisit.epoch !== detailEpoch) {
    // Retire the previous visit before any detail feedback is rendered for this route.
    setDetailVisit({ id: selectedOrder, client, owner, epoch: detailEpoch });
  }
  const currentDetailError = route && detailFailure?.visit === detailVisit ? copy[detailFailure.key] : '';
  React.useLayoutEffect(() => {
    if (!route || !visible) return;
    const previous = previousSelection.current; previousSelection.current = selectedOrder;
    if (selectedOrder && selectedOrder !== previous) { detailHeading.current?.focus(); globalThis.scrollTo?.(0, 0); }
    else if (!selectedOrder && previous) {
      const saved = returnSpot.current;
      if (saved?.id === previous && saved.signature === filters.signature && saved.client === client && saved.owner === owner && saved.trigger?.isConnected && !saved.trigger.closest('[hidden]')) {
        saved.trigger.focus(); globalThis.scrollTo?.(0, saved.scroll);
      } else { listHeading.current?.focus(); globalThis.scrollTo?.(0, 0); }
    }
  }, [Boolean(route), visible, selectedOrder, filters.signature, client, owner]);

  const [targets, setTargets] = useState([]);
  const [targetID, setTargetID] = useState('');
  const [reason, setReason] = useState('');
  const [refundReason, setRefundReason] = useState('');
  const [refundEnabled, setRefundEnabled] = useState(null);
  const [paymentCapabilities, setPaymentCapabilities] = useState(null);
  const operation = useFinanceOperation(owner, copy, client, 'payments');
  const list = useFinanceRouteLoader({ route, signature: filters.signature, client, owner, operation, load: () => load(filters.query.cursor), reset: () => { setPage(null); setListFailure(''); } });
  const load = async (next = cursor) => {
    const result = await list.read(async () => {
      setListFailure('');
      const [orders, overview] = await Promise.all([loadFinancePage({ ...(route ? filters.query : { q: query }), cursor: next }, filters => client.paymentOrders(filters), () => { setCursor(''); setPrevious([]); }), client.request('billing', '/admin/v1/overview')]);
      return { orders: orders.value, cursor: orders.query.cursor, capabilities: overview?.payment_capabilities, enabled: typeof overview?.payment_capabilities?.normal_refund_creation_enabled === 'boolean' ? overview.payment_capabilities.normal_refund_creation_enabled : null };
    }, (error, isCurrent) => {
      const key = error?.status === 403 ? 'permissions' : 'unavailable';
      if (route && isCurrent) setListFailure(key);
      return route ? '' : key;
    });
    if (result) { setPage(result.orders); setCursor(result.cursor); setRefundEnabled(result.enabled); setPaymentCapabilities(result.capabilities); }
  };
  useEffect(() => { if (!route) void load(''); }, [client, owner]); // eslint-disable-line react-hooks/exhaustive-deps
  const open = async (id, visit = detailVisit) => {
    let started = false;
    const result = await operation.read(async () => { started = true; const value = await client.paymentOrder(id); if (value?.order_id !== id) throw new Error('INVALID_FINANCE_ACK'); return { value, targets: wholeRefundTargets(value) }; }, error => {
      const key = error?.status === 403 ? 'permissions' : 'unavailable';
      if (route && (client.financeSessionEpoch || 0) === detailEpoch) {
        // A list reload must not clear feedback from a failed detail read.
        if (visit.id === id) setDetailFailure({ visit, key });
        if (error?.status === 401 || error?.status === 403) setDetailSnapshot(current => current?.value.order_id === id ? null : current);
      }
      // Routed detail errors belong to the selected order, not the shared
      // list/write feedback slot that can be visible on a different order.
      return route ? '' : key;
    });
    if (result) { setDetailFailure(current => current?.visit === visit && visit.id === id ? null : current); setDetailSnapshot({ visit, value: result.value }); setTargets(result.targets); setTargetID(result.targets[0]?.package.package_id || ''); setReason(''); setRefundReason(''); }
    return started;
  };
  useEffect(() => {
    const id = detailVisit.id;
    if (id && !operation.busy && !operation.unknown && attemptedDetail.current !== detailVisit) {
      attemptedDetail.current = detailVisit;
      void open(id).then(started => { if (!started && attemptedDetail.current === detailVisit) attemptedDetail.current = null; });
    }
    if (!id) { attemptedDetail.current = null; if (route && !operation.unknown) setDetailSnapshot(null); }
  }, [detailVisit, operation.busy, operation.unknown, operation.error, page]);
  const selectDetail = (id, trigger) => {
    if (!route) { void open(id); return; }
    const visit = detailVisit.id === id ? detailVisit : { ...detailVisit, id };
    if (visit !== detailVisit) setDetailVisit(visit);
    attemptedDetail.current = visit;
    returnSpot.current = { id, trigger, client, owner, signature: filters.signature, scroll: globalThis.scrollY || 0 };
    route.navigate(route.section, { order_id: id });
    void open(id, visit);
  };
  const repair = async () => {
    const value = await operation.mutate({ action: 'repair', orderID: detail.order_id, reason: reason.trim(), commandProtocol: 1 }, (facts, key) => client.repairPaymentOrder(facts.orderID, facts.reason, key, facts.commandProtocol)); if (value) await open(detail.order_id);
  };
  const refund = async () => {
    const target = targets.find(item => item.package.package_id === targetID);
    const reasonForRefund = (route ? refundReason : reason).trim();
    const facts = operation.pendingFacts || (detail && target && { action: 'refund', orderID: detail.order_id, transactionID: target.transaction.payment_transaction_id, packageID: targetID, amountFen: String(target.transaction.amount_fen), reason: reasonForRefund });
    if (!facts || (!operation.unknown && (!reasonForRefund || !refundEnabled || !permissions.includes('platform.billing.repairs.request') || !globalThis.confirm(copy.confirmRefund)))) return;
    const value = await operation.mutate(facts, async (current, key) => {
      if (current.action === 'repair') return client.repairPaymentOrder(current.orderID, current.reason, key, current.commandProtocol);
      const receipt = await client.refundPaymentOrder(current.orderID, current.transactionID, current.packageID, current.reason, key);
      if (integerMoney(receipt.amount_fen) !== integerMoney(current.amountFen)) throw new Error('INVALID_FINANCE_ACK');
      return receipt;
    });
    if (value) { await open(facts.orderID); await load(); }
  };
  const orderActions = adminLayoutCopy[language].orderActionSections;
  const canSyncOrder = permissions.includes('platform.billing.orders.sync');
  const canRequestOrderAction = permissions.includes('platform.billing.repairs.request');
  const syncOrder = async () => { const value = await operation.read(() => client.syncOrder(detail.order_id)); if (value) await open(detail.order_id); };
  const refundUnavailable = !canRequestOrderAction ? orderActions.noPermission : refundEnabled === null ? orderActions.capabilityUnknown : !refundEnabled ? orderActions.capabilityOff : !targets.length ? orderActions.noTarget : '';
  const usableCodes = values => [...new Set(values.filter(x => typeof x === 'string' && x.trim()))];
  const visibleOrders = page?.items || [];
  const orderFilterOptions = { provider_id: usableCodes([paymentCapabilities?.provider_id, ...visibleOrders.map(item => item.provider_id)]), channel: usableCodes([...(Array.isArray(paymentCapabilities?.channels) ? paymentCapabilities.channels : []), ...visibleOrders.map(item => item.channel)]) };
  const listContext = adminLayoutCopy[language].orderListContext;
  const detailPage = adminLayoutCopy[language].orderDetailPage;
  const visibleDetail = detail && (!route || (detailSnapshot.visit === detailVisit && detail.order_id === selectedOrder)) ? detail : null;
  return <section><div hidden={Boolean(selectedOrder)}><h3 ref={listHeading} tabIndex={route ? -1 : undefined}>{copy.orders}</h3>{route && <p><small>{listContext.window}</small></p>}{route ? <AdminFinanceFilters kind="orders" orderFilterOptions={orderFilterOptions} language={language} value={filters.draft} onChange={filters.setDraft} disabled={operation.busy || operation.unknown} onSubmit={() => { setPrevious([]); filters.move({ ...filters.draft, cursor: '' }, null); }} /> : <form onSubmit={event => { event.preventDefault(); setPrevious([]); void load(''); }}><label>{copy.search}<input value={query} onChange={event => setQuery(event.target.value)} /></label><button disabled={operation.busy || operation.unknown}>{copy.search}</button></form>}<Feedback operation={operation} copy={copy} />
    {route && listFailure && <p role="alert">{copy[listFailure]}<button type="button" disabled={operation.busy || operation.unknown} onClick={() => void load(filters.query.cursor)}>{copy.retry}</button></p>}
    {page && (route ? <AdminOrderResults page={page} language={language} disabled={operation.busy || operation.unknown} onOpen={selectDetail} /> : <table><thead><tr><th>{copy.orderID}</th><th>{copy.amount}</th><th>{copy.status}</th><th>{copy.actions}</th></tr></thead><tbody>{page.items.map(item => <tr key={item.order_id}><td>{item.order_id}</td><td>{formatFen(item.amount_fen, language)}</td><td>{item.status}</td><td><button disabled={operation.busy || operation.unknown} onClick={() => selectDetail(item.order_id)}>{copy.details}</button></td></tr>)}</tbody></table>)}
    <button disabled={operation.busy || operation.unknown || !previous.length} onClick={() => { const values = [...previous]; const value = values.pop(); setPrevious(values); if (route) filters.move({ ...filters.query, cursor: value }, null); else void load(value); }}>{copy.previous}</button><button disabled={operation.busy || operation.unknown || !page?.next_cursor} onClick={() => { setPrevious(values => [...values, cursor]); if (route) filters.move({ ...filters.query, cursor: page.next_cursor }, null); else void load(page.next_cursor); }}>{copy.next}</button>
    </div>
    {operation.unknown && <div data-retained-finance-command role="alert"><p>{operation.error}</p><code>{operation.pendingFacts?.orderID} / {operation.pendingKey}</code><button disabled={operation.busy} onClick={() => void refund()}>{copy.retry}</button></div>}
    {selectedOrder && <header className="adm-order-detail-header">
      <button type="button" disabled={operation.busy || operation.unknown} onClick={() => route.navigate(route.section, { order_id: null }, 'replace')}>{detailPage.back}</button>
      <h3 id={frameHeading} tabIndex={-1} ref={detailHeading}>{detailPage.title}</h3>
      <p>{copy.orderID}: <code>{selectedOrder}</code></p>
      {visibleDetail && <p>{listContext.amount}: {formatFen(visibleDetail.amount_fen, language)}</p>}
      {currentDetailError && <p role="alert">{currentDetailError}<button type="button" disabled={operation.busy || operation.unknown} onClick={() => void open(selectedOrder)}>{copy.retry}</button></p>}
      {!visibleDetail && !currentDetailError && !operation.unknown && <p role="status">{copy.loading}</p>}
      {visibleDetail && !operation.unknown && <Feedback operation={operation} copy={copy} />}
    </header>}
    {visibleDetail && <article aria-labelledby={route ? frameHeading : undefined}>{!route && <h4>{copy.details}: {detail.order_id}</h4>}{route ? <AdminOrderStatusSummary detail={detail} language={language} /> : <p>{detail.status} / {detail.fulfillment_status} / {detail.attention_summary}</p>}
      {route ? <>
        {canSyncOrder && <section className="adm-order-action-group"><h5>{orderActions.queryTitle}</h5><p>{orderActions.queryHelp}</p><button type="button" disabled={operation.busy || operation.unknown} onClick={() => void syncOrder()}>{copy.query}</button></section>}
        {detail.status === 'MANUAL_REVIEW' && canRequestOrderAction && <section className="adm-order-action-group">
          <h5>{copy.orderRepair}</h5><label>{orderActions.repairReason}<textarea disabled={operation.busy || operation.unknown} maxLength={500} value={reason} onChange={event => setReason(event.target.value)} /></label>
          <button type="button" disabled={operation.busy || operation.unknown || !reason.trim()} onClick={() => void repair()}>{copy.orderRepair}</button>
        </section>}
        <section className="adm-order-action-group">
          <h5>{copy.refunds}</h5>
          {refundUnavailable ? <p>{refundUnavailable}</p> : <>
            <label>{copy.transaction} / {copy.package}<select disabled={operation.busy || operation.unknown} value={targetID} onChange={event => setTargetID(event.target.value)}>{targets.map(item => <option key={item.package.package_id} value={item.package.package_id}>{item.transaction.payment_transaction_id} / {item.package.package_id} — {formatFen(item.transaction.amount_fen, language)}</option>)}</select></label>
            <label>{orderActions.refundReason}<textarea disabled={operation.busy || operation.unknown} maxLength={500} value={refundReason} onChange={event => setRefundReason(event.target.value)} /></label>
            <button type="button" disabled={operation.busy || operation.unknown || !refundReason.trim()} onClick={() => void refund()}>{copy.refunds}</button>
          </>}
        </section>
      </> : <>
      {canSyncOrder && <button disabled={operation.busy || operation.unknown} onClick={() => void syncOrder()}>{copy.query}</button>}
      <label>{copy.reason}<textarea disabled={operation.busy || operation.unknown} maxLength={500} value={reason} onChange={event => setReason(event.target.value)} /></label>
      {detail.status === 'MANUAL_REVIEW' && permissions.includes('platform.billing.repairs.request') && <button disabled={operation.busy || operation.unknown || !reason.trim()} onClick={() => void repair()}>{copy.orderRepair}</button>}
      <h4>{copy.refunds}</h4>{targets.length ? <label>{copy.transaction} / {copy.package}<select disabled={operation.busy || operation.unknown} value={targetID} onChange={event => setTargetID(event.target.value)}>{targets.map(item => <option key={item.package.package_id} value={item.package.package_id}>{item.transaction.payment_transaction_id} / {item.package.package_id} — {formatFen(item.transaction.amount_fen, language)}</option>)}</select></label> : <p>{copy.refundUnavailable}</p>}
      {!refundEnabled && <p>{copy.refundUnavailable}</p>}
      {permissions.includes('platform.billing.repairs.request') && targets.length > 0 && <button disabled={operation.busy || operation.unknown || !refundEnabled || !reason.trim()} onClick={() => void refund()}>{operation.unknown ? copy.retry : copy.refunds}</button>}
      </>}
    </article>}
  </section>;
}

const defaultApprovalCommandTypes = approvalCommands;

export function Approvals({ client, copy, permissions, owner, commandTypes = defaultApprovalCommandTypes, initialRequestID = '', requestID = initialRequestID, query = null, pendingUserID = null, onStateChange, onNavigate, centerCopy, mineReadCopy, pendingReadCopy, mineApplyGeneration }) {
  const [items, setItems] = useState([]);
  const [page, setPage] = useState(null);
  const [cursor, setCursor] = useState('');
  const [previous, setPrevious] = useState([]);
  const [detail, setDetail] = useState(null);
  const [reason, setReason] = useState('');
  const [mineResult, setMineResult] = useState(null);
  const [mineDetailScope, setMineDetailScope] = useState(null);
  const mineRequest = React.useRef(null);
  const lastMineApply = React.useRef(null);
  const operation = useFinanceOperation(owner, { ...copy, unavailable: copy.approvalReadUnavailable || copy.unavailable }, client, 'approvals');
  const loadLegacy = async (next = cursor) => { const result = await operation.read(() => loadFinancePage({ ...query, cursor: next }, filters => client.governedApprovalRequests(filters), () => { setCursor(''); setPrevious([]); })); if (result) { const value = result.value; setPage(value); setCursor(result.query.cursor); setItems(requireFinanceCollection(value).filter(item => commandTypes.includes(item.command_type) && (!pendingUserID || approvalRequester(item) !== pendingUserID))); } };
  const queryKey = JSON.stringify(query);
  const mine = Boolean(mineReadCopy), pending = Boolean(pendingReadCopy && pendingUserID);
  const scopedRead = mine || pending, readCopy = mine ? mineReadCopy : pendingReadCopy;
  const mineEpoch = client.financeSessionEpoch || 0;
  const mineScope = React.useMemo(() => ({ client, owner, queryKey, requestID, mine, pending, pendingUserID, epoch: mineEpoch }), [client, owner, queryKey, requestID, mine, pending, pendingUserID, mineEpoch]);
  const liveMineScope = React.useRef(mineScope);
  liveMineScope.current = mineScope;
  const readState = scopedRead && mineResult?.scope === mineScope ? mineResult : null;
  const scopedLoading = scopedRead && (!readState || readState.phase === 'loading');
  const busy = operation.busy || scopedLoading;
  const load = async (next = cursor) => {
    if (!scopedRead) return loadLegacy(next);
    mineRequest.current?.controller.abort();
    const controller = new AbortController(), request = { scope: mineScope, controller };
    mineRequest.current = request;
    const live = () => mineRequest.current === request && liveMineScope.current === mineScope && !controller.signal.aborted;
    let attemptedCursor = next;
    setMineResult(previous => ({ scope: mineScope, phase: 'loading', hasResult: previous?.scope === mineScope && previous.hasResult,
      acceptedTime: previous?.scope === mineScope ? previous.acceptedTime : null }));
    try {
      const result = await loadFinancePage({ ...query, cursor: next },
        filters => client.governedApprovalRequests(filters, controller.signal, { requireCollection: true }),
        () => { attemptedCursor = ''; if (live()) { setCursor(''); setPrevious([]); } });
      if (!live()) return;
      const list = requireFinanceCollection(result.value);
      if (mine && (!query?.requester_user_id || list.some(item => approvalRequester(item) !== query.requester_user_id))) throw new Error('INVALID_APPROVAL_COLLECTION');
      setPage(result.value); setCursor(result.query.cursor);
      setItems(list.filter(item => commandTypes.includes(item.command_type) && (!pending || approvalRequester(item) !== pendingUserID)));
      const rawSnapshot = result.value.as_of;
      const snapshot = typeof rawSnapshot === 'string' && /^\d{4}-\d{2}-\d{2}T/.test(rawSnapshot) && /(?:Z|[+-]\d{2}:\d{2})$/.test(rawSnapshot) ? Date.parse(rawSnapshot) : NaN;
      setMineResult({ scope: mineScope, phase: 'ready', hasResult: true, acceptedTime: Number.isFinite(snapshot)
        ? { source: 'snapshotTime', iso: new Date(snapshot).toISOString() } : { source: 'receivedTime', iso: new Date().toISOString() } });
      return 'ready';
    } catch (failure) {
      if (!live()) return;
      const phase = failure?.status === 403 ? 'denied' : failure?.status === 401 ? 'signedOut' : 'failed';
      if (phase !== 'failed') { setPage(null); setItems([]); setDetail(null); }
      setMineResult(previous => ({ scope: mineScope, phase, requestedCursor: attemptedCursor,
        hasResult: phase === 'failed' && previous?.scope === mineScope && previous.hasResult,
        acceptedTime: phase === 'failed' && previous?.scope === mineScope ? previous.acceptedTime : null }));
      return phase;
    }
  };
  useEffect(() => {
    onStateChange?.(busy || operation.unknown);
    // This single mounted child owns its parent's navigation lock. Release it
    // on retirement; the next child's effect installs its own current lock.
    return () => onStateChange?.(false);
  }, [onStateChange, busy, operation.unknown]);
  useEffect(() => { let active = true; void (async () => { setDetail(null); setPage(null); setItems([]); setPrevious([]); const phase = await load(''); if (active && requestID && (!scopedRead || phase === 'ready' || phase === 'failed')) await open(requestID); })(); return () => { active = false; if (mineRequest.current?.scope === mineScope) mineRequest.current.controller.abort(); }; }, [client, owner, requestID, queryKey, pendingUserID, mineScope]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    const previousApply = lastMineApply.current;
    lastMineApply.current = { generation: mineApplyGeneration, scope: mineScope };
    if (!mine || previousApply?.scope !== mineScope || previousApply.generation === mineApplyGeneration) return;
    setPrevious([]); void load('');
  }, [mine, mineApplyGeneration, mineScope]); // eslint-disable-line react-hooks/exhaustive-deps
  const open = async id => { const value = await operation.read(async () => { const value = await client.governedApprovalRequest(id); if (value?.approval_request_id !== id || !commandTypes.includes(value.command_type)) throw new Error('INVALID_FINANCE_ACK'); return value; }); if (value && (!scopedRead || liveMineScope.current === mineScope)) { if (scopedRead) setMineDetailScope(mineScope); setDetail(value); } };
  const act = async action => {
    let facts = operation.pendingFacts;
    if (!facts) {
      const draft = action === 'EDIT' ? await operation.read(() => currentApprovalEditDraft(client, detail, reason)) : null;
      if (action === 'EDIT' && !draft) return;
      facts = { detail, action, reason, draft, ...(['WITHDRAW', 'APPROVE', 'REJECT'].includes(action) ? { commandProtocol: 1 } : {}) };
    }
    const value = await operation.mutate(facts, async (facts, invocationKey) => {
      if (facts.action === 'EDIT') await updateAndSubmitGovernedApproval(client, facts.detail, { ...facts.draft, idempotency_key: invocationKey });
      else if (facts.action === 'SUBMIT') await client.submitGovernedApprovalRequest(facts.detail.approval_request_id, facts.detail);
      else if (facts.action === 'WITHDRAW') await client.withdrawGovernedApprovalRequest(facts.detail.approval_request_id, facts.detail, undefined, facts.commandProtocol);
      else await client.decideGovernedApproval(facts.detail.approval_request_id, facts.detail, facts.action, facts.reason, undefined, facts.commandProtocol);
      return true;
    });
    if (value) { await open(facts.detail.approval_request_id); await load(); }
  };
  const businessRoute = approvalBusinessRoute(detail);
  const factTime = value => {
    const epoch = typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T/.test(value) && /(?:Z|[+-]\d{2}:\d{2})$/.test(value) ? Date.parse(value) : NaN;
    if (!Number.isFinite(epoch) || value.startsWith('0001-01-01T')) return <span>{pendingReadCopy.missingFact}</span>;
    const iso = new Date(epoch).toISOString();
    return <time dateTime={iso}>{iso.slice(0, 19).replace('T', ' ')} UTC</time>;
  };
  const showScopedRows = readState?.hasResult && items.length > 0;
  const scopedFeedback = scopedRead && <div className={mine ? "adm-approval-mine-read" : "adm-approval-mine-read adm-approval-pending-read"}>
    {scopedLoading && <p role="status">{readCopy.loading}</p>}
    {readState && ['denied', 'signedOut', 'failed'].includes(readState.phase) && <p role="alert">{readCopy[readState.phase]}</p>}
    {readState?.hasResult && readState.phase !== 'ready' && <p className="adm-approval-previous">{readCopy.previous}</p>}
    {readState?.phase === 'ready' && !items.length && <p role="status">{pending && page?.next_cursor ? readCopy.emptyWithNext : pending && cursor ? readCopy.emptyPage : readCopy.empty}</p>}
    {pending && readState?.phase === 'ready' && <small className="adm-approval-current-page-count">{readCopy.pageCount.replace('{count}', items.length)}</small>}
    <div className="adm-approval-mine-tools">
      {readState?.acceptedTime && <small>{readCopy[readState.acceptedTime.source]} <time dateTime={readState.acceptedTime.iso}>{readState.acceptedTime.iso.slice(0, 19).replace('T', ' ')} UTC</time></small>}
      {(readState?.phase === 'ready' || readState?.phase === 'failed') && <button type="button" disabled={busy || operation.unknown}
        onClick={() => void load(readState.phase === 'failed' ? readState.requestedCursor : cursor)}>{readState.phase === 'failed' ? readCopy.retryRead : readCopy.refreshRead}</button>}
    </div>
  </div>;
  return <section><h3>{copy.approvals}</h3>{!scopedRead && <button disabled={busy} onClick={() => void load()}>{copy.refresh}</button>}<Feedback operation={operation} copy={copy} />{operation.unknown && <button disabled={busy} onClick={() => void act('')}>{copy.retry}</button>}{scopedFeedback}{(!scopedRead || showScopedRows) && <table><thead><tr><th>{copy.command}</th><th>{copy.status}</th>{centerCopy && <><th>{centerCopy.target}</th><th>{pending ? pendingReadCopy.requesterID : centerCopy.requester}</th>{pending ? <><th>{pendingReadCopy.submittedAt}</th><th>{pendingReadCopy.expiresAt}</th></> : <th>{centerCopy.updated}</th>}</>}<th>{copy.details}</th></tr></thead><tbody>{items.map(item => <tr key={item.approval_request_id}><td>{mine ? <><strong>{approvalLabel(centerCopy?.policyCommandLabels, item.command_type)}</strong>{approvalLabel(centerCopy?.policyCommandLabels, item.command_type) !== item.command_type && <code className="adm-approval-status-code">{item.command_type}</code>}</> : item.command_type}</td><td>{mine ? <span className={'adm-status adm-approval-mine-status' + (['APPROVED', 'EXECUTED'].includes(item.status) ? ' ok' : ['EXECUTING', 'EXECUTION_FAILED', 'REJECTED', 'EXPIRED', 'INVALIDATED'].includes(item.status) ? ' warn' : '')}>{approvalLabel(centerCopy?.statusLabels, item.status)}</span> : approvalLabel(centerCopy?.statusLabels, item.status)}{approvalLabel(centerCopy?.statusLabels, item.status) !== item.status && <small className="adm-approval-status-code">{item.status}</small>}</td>{centerCopy && <><td>{item.target?.resource_id || item.target_resource_id}</td><td>{approvalRequester(item) || (pending ? pendingReadCopy.missingFact : '')}</td>{pending ? <><td>{factTime(item.submitted_at)}</td><td>{factTime(item.expires_at)}</td></> : <td>{item.updated_at}</td>}</>}<td><button disabled={busy || operation.unknown} onClick={() => void open(item.approval_request_id)}>{copy.details}</button></td></tr>)}</tbody></table>}
    <button disabled={busy || operation.unknown || (scopedRead && readState?.phase !== 'ready') || !previous.length} onClick={() => { const values = [...previous]; const value = values.pop(); setPrevious(values); void load(value); }}>{copy.previous}</button><button disabled={busy || operation.unknown || (scopedRead && readState?.phase !== 'ready') || !page?.next_cursor} onClick={() => { setPrevious(values => [...values, cursor]); void load(page.next_cursor); }}>{copy.next}</button>
    {detail && (!scopedRead || mineDetailScope === mineScope && !['denied', 'signedOut'].includes(readState?.phase)) && <article><h4>{detail.approval_request_id}</h4><p>{mine ? approvalLabel(centerCopy?.policyCommandLabels, detail.command_type) : detail.command_type} / {approvalLabel(centerCopy?.statusLabels, detail.status)} / {copy.version}: {detail.version}</p>{approvalLabel(centerCopy?.statusLabels, detail.status) !== detail.status && <code className="adm-approval-status-code">{detail.status}</code>}<p>{detail.reason}</p>{businessRoute && onNavigate && <button disabled={busy || operation.unknown} onClick={() => onNavigate(businessRoute)}>{centerCopy.openBusinessArea}</button>}<h5>{copy.command}</h5><pre>{JSON.stringify(detail.command || {}, null, 2)}</pre>{detail.command_type === 'TOKEN_MODEL_LIFECYCLE_CHANGE' && <><p>{copy.forceCancelInflight}: {detail.command?.force_cancel_inflight === true ? copy.forceCancelEnabled : copy.forceCancelDisabled}</p><p>{copy.forceCancelInflightHelp}</p></>}{detail.execution && <><h5>{copy.effects}</h5><p>{detail.execution.attempt_status} / {detail.execution.error_class} / {detail.execution.next_retry_at} / {detail.execution.execution_deadline}</p></>}<label>{copy.reason}<textarea disabled={busy || operation.unknown} value={reason} onChange={event => setReason(event.target.value)} maxLength={2000} /></label>{['EDIT', 'SUBMIT', 'WITHDRAW', 'APPROVE', 'REJECT'].filter(action => detail.allowed_actions?.includes(action)).map(action => {
      const approver = action === 'APPROVE' || action === 'REJECT';
      const allowed = approver ? permissions.includes({ USAGE_SETTLEMENT_REPAIR: 'platform.billing.repairs.approve', RECONCILIATION_FINANCIAL_REPAIR: 'platform.billing.repairs.approve', CHARGEBACK_FINANCIAL_REPAIR: 'platform.billing.repairs.approve', REFUND_EXCEPTION_REPAIR: 'platform.billing.repairs.approve', PAYMENT_FINANCIAL_REPAIR: 'platform.billing.repairs.approve', RECEIVABLE_WRITE_OFF: 'platform.billing.repairs.approve', ENTITLEMENT_COMPONENT_ADJUSTMENT: 'platform.billing.repairs.approve', LEDGER_REPAIR: 'platform.billing.repairs.approve', WALLET_ADJUSTMENT: 'platform.billing.adjustments.approve', PAYMENT_PROVIDER_RESUME: 'platform.billing.payment-controls.resume.approve', PAYMENT_PRODUCT_RESUME: 'platform.billing.payment-controls.resume.approve', TOKEN_MODEL_LIFECYCLE_CHANGE: 'platform.models.lifecycle.approve', TOKEN_MODEL_CATALOG_ROLLBACK: 'platform.models.catalog.rollback.approve', TOKEN_CREDENTIAL_REVOKE: 'platform.tokens.credentials.revoke.approve' }[detail.command_type]) : true;
      return allowed && <button key={action} disabled={busy || operation.unknown || ((approver || action === 'EDIT') && reason.trim().length < 10)} onClick={() => void act(action)}>{copy[{ EDIT: 'editSubmit', SUBMIT: 'submit', WITHDRAW: 'withdraw', APPROVE: 'approve', REJECT: 'reject' }[action]]}</button>;
    })}</article>}
  </section>;
}

function walletFact(labels, value, unavailable) {
  if (typeof value !== 'string' || !value.trim()) return unavailable;
  return Object.prototype.hasOwnProperty.call(labels, value) ? labels[value] : value;
}

function adjustmentAmountView(value, language, words) {
  if (value === '' || value == null) return null;
  try {
    const amount = integerMoney(value);
    if (amount === 0n || amount === -(1n << 63n)) return { valid: false, text: words.invalidAmount };
    const absolute = amount < 0n ? -amount : amount;
    return { valid: true, text: words.change(amount > 0n ? words.increase : words.decrease, formatBalance(absolute.toString(), language)) };
  } catch {
    return { valid: false, text: words.invalidAmount };
  }
}

function Wallets({ client, copy, language, permissions, owner, requesterID, onViewApproval, route, acceptedSessionEpoch, acceptedPlatformAuthorizationVersion }) {
  const walletDirectoryVisible = usePortalDialogScope();
  const [walletReadDenied, setWalletReadDenied] = useState(false);
  const walletCopy = adminLayoutCopy[language].walletDirectory;
  const walletIDLabel = adminLayoutCopy[language].financeLists.wallet_id;
  const adjustmentCopy = adminLayoutCopy[language].walletAdjustmentForm;
  const formID = React.useId();
  const filters = useFinanceReadRoute('wallets', route);
  const [page, setPage] = useState(null);
  const [cursor, setCursor] = useState('');
  const [previous, setPrevious] = useState([]);
  useEffect(() => { setPrevious([]); }, [filters.filterSignature]);
  const [walletID, setWalletID] = useState('');
  const [delta, setDelta] = useState('');
  const [reason, setReason] = useState('');
  const [ledger, setLedger] = useState(null);
  const [adjustments, setAdjustments] = useState(null);
  const [adjustmentCursor, setAdjustmentCursor] = useState('');
  const [adjustmentPrevious, setAdjustmentPrevious] = useState([]);
  const [selected, setSelected] = useState(null);
  const [replacement, setReplacement] = useState(null);
  const operation = useFinanceOperation(owner, { ...copy, saved: adjustmentCopy.submissionConfirmed }, client, 'wallets');
  const list = useFinanceRouteLoader({ route, signature: filters.signature, client, owner, operation, load: async () => { if (await load(filters.query.cursor)) await loadAdjustments(''); }, reset: () => setPage(null) });
  const canRequest = permissions.includes('platform.billing.repairs.request');
  const loadAdjustments = async (next = adjustmentCursor) => {
    const result = await operation.read(() => loadFinancePage({ cursor: next, status: 'PENDING_APPROVAL' }, filters => client.adjustments(filters), () => { setAdjustmentCursor(''); setAdjustmentPrevious([]); }));
    if (result) { setAdjustments(result.value); setAdjustmentCursor(result.query.cursor); setSelected(null); }
  };
  const load = async (next = cursor) => {
    const result = await list.read(() => loadFinancePage({ ...(route ? filters.query : {}), cursor: next }, filters => client.wallets(filters), () => { setCursor(''); setPrevious([]); }), (reason, current) => {
      if (current && [401, 403].includes(reason?.status)) setWalletReadDenied(true);
      return reason.status === 403 ? "permissions" : "unavailable";
    });
    if (result) setWalletReadDenied(false);
    if (result) { setPage(result.value); setCursor(result.query.cursor); return true; }
  };
  useEffect(() => { if (!route) void (async () => { await load(''); await loadAdjustments(''); })();  }, [client, owner]); // eslint-disable-line react-hooks/exhaustive-deps
  const inspect = async item => {
    const result = await operation.read(async () => {
      const history = await client.governedApprovalRequests({ command_type: 'WALLET_ADJUSTMENT', target: item.adjustment_id, limit: 200 });
      if (history?.has_more || history?.next_cursor || !Array.isArray(history?.items) || history.items.length >= 200) throw new Error('APPROVAL_HISTORY_INCOMPLETE');
      const summary = latestApprovalByTarget(requireFinanceCollection(history)).get(item.adjustment_id);
      const approval = summary ? await client.governedApprovalRequest(summary.approval_request_id) : null;
      if (approval && (approval.approval_request_id !== summary.approval_request_id || approval.command_type !== 'WALLET_ADJUSTMENT' || approval.target?.resource_type !== 'MANUAL_ADJUSTMENT' || approval.target?.resource_id !== item.adjustment_id || approval.command?.adjustment_id !== item.adjustment_id || approval.command?.wallet_id !== item.wallet_id)) throw new Error('INVALID_FINANCE_ACK');
      return { item, approval };
    });
    if (result) setSelected(result);
  };
  const adjust = async () => {
    const facts = operation.pendingFacts || { action: 'create', walletID, delta, reason: reason.trim(), replacement };
    if (!operation.pendingFacts && !canSubmitAdjustment) return;
    const value = await operation.mutate(facts, async (facts, operationKey) => {
      if (facts.action === 'resume') {
        const fresh = await currentPendingAdjustment(client, facts.item, requesterID);
        // Fresh history is checked again by the workflow before its write.
        return createAndSubmitFreshGovernedApproval(client, walletAdjustmentApprovalDraft(fresh, fresh.reason, fresh.request_idempotency_key));
      }
      const key = facts.replacement?.key || operationKey;
      try { requireFinanceID(facts.walletID); integerMoney(facts.delta); }
      catch (error) { throw financeNotDispatched(error, true); }
      if (facts.createdAdjustment) {
        try {
          const fresh = await currentPendingAdjustment(client, facts.createdAdjustment, requesterID);
          return await createAndSubmitFreshGovernedApproval(client, walletAdjustmentApprovalDraft(fresh, facts.reason, key));
        } catch (error) { throw financeWriteObserved(error); }
      }
      if (facts.replacement && (facts.walletID !== facts.replacement.walletID || !walletReplacementEligible(await client.governedApprovalRequest(facts.replacement.requestID), facts.replacement.adjustmentID, facts.walletID, requesterID))) {
        throw Object.assign(new Error('old adjustment approval changed'), { status: 409 });
      }
      const item = await client.createAdjustment({ wallet_id: requireFinanceID(facts.walletID), amount_microcredits: facts.delta, reason: facts.reason, idempotency_key: key });
      if (item.wallet_id !== facts.walletID || integerMoney(item.amount_microcredits) !== integerMoney(facts.delta) || item.requested_by !== requesterID || item.request_idempotency_key !== key || item.reason !== facts.reason || item.status !== 'PENDING_APPROVAL' || !Number.isSafeInteger(item.version) || item.version < 1) throw new Error('INVALID_FINANCE_ACK');
      requireFinanceID(item.adjustment_id);
      // Only a fully validated receipt can become a recovery checkpoint. Keep
      // the original input/key and re-read this exact adjustment on retry.
      facts.createdAdjustment = item;
      try { return await createAndSubmitFreshGovernedApproval(client, walletAdjustmentApprovalDraft(item, facts.reason, key)); }
      catch (error) { throw financeWriteObserved(error); }
    });
    if (value) {
      if (facts.action === 'create') { setReplacement(null); setWalletID(''); setDelta(''); setReason(''); }
      await load(); await loadAdjustments();
    }
  };
  const resume = () => {
    if (!selected || adjustmentApprovalAction(selected.item, selected.approval, { canRequest: canRequest && selected.item.requested_by === requesterID }) !== 'resume') return;
    void operation.mutate({ action: 'resume', item: selected.item }, async facts => {
      const fresh = await currentPendingAdjustment(client, facts.item, requesterID);
      return createAndSubmitFreshGovernedApproval(client, walletAdjustmentApprovalDraft(fresh, fresh.reason, fresh.request_idempotency_key));
    }).then(async value => { if (value) await loadAdjustments(); });
  };
  const beginReplacement = () => {
    if (!selected || !canRequest || !walletReplacementEligible(selected.approval, selected.item.adjustment_id, selected.item.wallet_id, requesterID)) return;
    setReplacement({ requestID: selected.approval.approval_request_id, adjustmentID: selected.item.adjustment_id, walletID: selected.item.wallet_id, key: `replacement:${selected.approval.approval_request_id}:${secureRequestID()}` });
    setWalletID(selected.item.wallet_id); setDelta(''); setReason('');
  };
  const resumeAvailable = selected && adjustmentApprovalAction(selected.item, selected.approval, { canRequest: canRequest && selected.item.requested_by === requesterID }) === 'resume';
  const pendingAdjustment = operation.busy || operation.unknown ? operation.pendingFacts : null;
  const pendingRecord = pendingAdjustment?.action === 'resume' ? pendingAdjustment.item : null;
  const formReplacement = pendingAdjustment ? pendingAdjustment.replacement : replacement;
  const originalAdjustmentWalletID = pendingRecord?.wallet_id || pendingAdjustment?.walletID;
  const formWalletID = pendingAdjustment ? originalAdjustmentWalletID || '' : walletID;
  const formDelta = pendingAdjustment ? pendingRecord?.amount_microcredits ?? pendingAdjustment.delta ?? '' : delta;
  const formReason = pendingAdjustment ? pendingRecord?.reason ?? pendingAdjustment.reason ?? '' : reason;
  const adjustmentWallet = canRequest && !walletReadDenied && !pendingAdjustment && !operation.unknown ? page?.items.find(item => item.wallet_id === walletID) : null;
  const adjustmentAmount = adjustmentAmountView(formDelta, language, adjustmentCopy);
  const canSubmitAdjustment = canRequest && !operation.busy && !operation.unknown && Boolean(walletID && adjustmentAmount?.valid && reason.trim().length >= 10);
  const fieldHelp = suffix => `${formID}-${suffix}`;
  const balanceSnapshot = field => {
    const value = adjustmentWallet ? formatBalance(adjustmentWallet[field], language) : '—';
    return value === '—' ? adjustmentCopy.unavailable : adjustmentCopy.points(value);
  };
  return <section><h3>{copy.wallet}</h3>{route && <AdminFinanceFilters kind="wallets" language={language} value={filters.draft} onChange={filters.setDraft} disabled={operation.busy || operation.unknown} onSubmit={() => { setPrevious([]); filters.move({ ...filters.draft, cursor: '' }, null); }} />}<button disabled={operation.busy || operation.unknown} onClick={async () => { await load(); await loadAdjustments(); }}>{copy.refresh}</button><Feedback operation={operation} copy={copy} />{operation.unknown && <button disabled={operation.busy} onClick={() => void adjust()}>{copy.retry}</button>}{page && <div className="adm-table-wrap adm-wallet-directory-scroll" role="region" aria-label={copy.wallet} tabIndex={0}>
    <table className="adm-table adm-wallet-directory" role="table">
      <thead role="rowgroup"><tr role="row"><th role="columnheader" scope="col">{copy.wallet}</th><th role="columnheader" scope="col">{copy.total}</th><th role="columnheader" scope="col">{copy.remaining}</th><th role="columnheader" scope="col">{copy.status}</th></tr></thead>
      <tbody role="rowgroup">{page.items.map(item => {
        const walletType = walletFact(walletCopy.types, item.owner_type, walletCopy.typeUnavailable);
        return <tr role="row" key={item.wallet_id}>
          <td role="cell"><button type="button" className="adm-wallet-directory-open" aria-label={walletCopy.select(walletType, item.wallet_id)} disabled={operation.busy || operation.unknown || Boolean(replacement)} onClick={async () => { setWalletID(item.wallet_id); if (route) filters.move(filters.query, item.wallet_id); else if (permissions.includes('platform.billing.ledger.read')) { const value = await operation.read(() => client.walletLedger(item.wallet_id)); if (value) setLedger(value); } }}>{walletType}</button>
            <div className="adm-wallet-directory-id"><small>{walletIDLabel}</small>{walletDirectoryVisible && (walletReadDenied ? <code>{item.wallet_id}</code> : <AdminIdentifier client={client} owner={owner} routeScope={route?.search || 'wallets'} value={item.wallet_id} label={walletIDLabel} targetLabel={item.wallet_id} language={language} sessionEpoch={acceptedSessionEpoch} platformAuthorizationVersion={acceptedPlatformAuthorizationVersion} alwaysExpanded />)}</div>
          </td>
          <td role="cell"><span className="adm-wallet-directory-mobile-label" aria-hidden="true">{copy.total}</span>{formatBalance(item.posted_microcredits, language)}</td><td role="cell"><span className="adm-wallet-directory-mobile-label" aria-hidden="true">{copy.remaining}</span>{formatBalance(item.available_microcredits, language)}</td>
          <td role="cell"><dl className="adm-wallet-directory-facts"><div><dt>{walletCopy.accountStatus}</dt><dd>{walletFact(walletCopy.account, item.status, walletCopy.unavailable)}</dd></div><div><dt>{walletCopy.riskStatus}</dt><dd>{walletFact(walletCopy.risk, item.financial_status, walletCopy.unavailable)}</dd></div></dl></td>
        </tr>;
      })}</tbody>
    </table>
  </div>}
    <button disabled={operation.busy || operation.unknown || !previous.length} onClick={() => { const values = [...previous]; const value = values.pop(); setPrevious(values); if (route) filters.move({ ...filters.query, cursor: value }, null); else void load(value); }}>{copy.previous}</button><button disabled={operation.busy || operation.unknown || !page?.next_cursor} onClick={() => { setPrevious(values => [...values, cursor]); if (route) filters.move({ ...filters.query, cursor: page.next_cursor }, null); else void load(page.next_cursor); }}>{copy.next}</button>
    {route && filters.selectedID && permissions.includes('platform.billing.ledger.read') && <AdminWalletLedger {...{ client, language, route }} walletID={filters.selectedID} />}
    {ledger && <ul>{requireFinanceCollection(ledger).map(item => <li key={item.posting_id}>{item.journal_entry_id} / {item.created_at}</li>)}</ul>}
    <section data-finance-adjustments><h4>{copy.adjustment}</h4>{adjustments && <table><thead><tr><th>{copy.wallet}</th><th>{copy.delta}</th><th>{copy.status}</th><th>{copy.details}</th></tr></thead><tbody>{adjustments.items.map(item => <tr key={item.adjustment_id}><td>{item.wallet_id}</td><td>{formatBalance(item.amount_microcredits, language)}</td><td>{item.status}<br />{item.request_idempotency_key}</td><td><button disabled={operation.busy || operation.unknown} onClick={() => void inspect(item)}>{copy.details}</button></td></tr>)}</tbody></table>}
      <button disabled={operation.busy || operation.unknown || !adjustmentPrevious.length} onClick={() => { const values = [...adjustmentPrevious]; const value = values.pop(); setAdjustmentPrevious(values); void loadAdjustments(value); }}>{copy.previous}</button><button disabled={operation.busy || operation.unknown || !adjustments?.next_cursor} onClick={() => { setAdjustmentPrevious(values => [...values, adjustmentCursor]); void loadAdjustments(adjustments.next_cursor); }}>{copy.next}</button>
      {selected && <article><h4>{selected.item.adjustment_id}</h4><p>{selected.item.reason}</p>{selected.approval && <><p>{copy.approvals}: {selected.approval.approval_request_id} / {selected.approval.status}</p><button disabled={operation.busy || operation.unknown} onClick={() => onViewApproval(selected.approval.approval_request_id)}>{copy.details}</button></>}{resumeAvailable && <button disabled={operation.busy || operation.unknown} onClick={resume}>{copy.submit}</button>}{canRequest && walletReplacementEligible(selected.approval, selected.item.adjustment_id, selected.item.wallet_id, requesterID) && <button disabled={operation.busy || operation.unknown} onClick={beginReplacement}>{copy.newInvocation}</button>}</article>}
    </section>
    {formReplacement && <p>{copy.approvals}: {formReplacement.requestID} / {copy.wallet}: {formReplacement.walletID}</p>}
    {canRequest && <form className="adm-wallet-adjustment-form" aria-labelledby={fieldHelp('title')} onSubmit={event => { event.preventDefault(); if (canSubmitAdjustment) void adjust(); }}>
      <header><h4 id={fieldHelp('title')}>{adjustmentCopy.title}</h4><p>{adjustmentCopy.approval}</p></header>
      <div className="adm-wallet-adjustment-field">
        <label htmlFor={fieldHelp('wallet')}>{walletIDLabel}</label>
        <input id={fieldHelp('wallet')} aria-describedby={fieldHelp('wallet-help')} disabled={operation.busy || operation.unknown} readOnly={Boolean(formReplacement)} value={formWalletID} onChange={event => setWalletID(event.target.value)} />
        <small id={fieldHelp('wallet-help')}>{adjustmentCopy.walletHelp}</small>
      </div>
      {operation.unknown && originalAdjustmentWalletID && <p className="adm-wallet-adjustment-direction">{adjustmentCopy.recoveryWallet} <code>{originalAdjustmentWalletID}</code></p>}
      {!pendingAdjustment && !operation.unknown && <div className="adm-wallet-adjustment-snapshot">
        {adjustmentWallet ? <><p>{adjustmentCopy.snapshot}</p><dl><div><dt>{adjustmentCopy.posted}</dt><dd>{balanceSnapshot('posted_microcredits')}</dd></div><div><dt>{adjustmentCopy.available}</dt><dd>{balanceSnapshot('available_microcredits')}</dd></div></dl></> : <p>{adjustmentCopy.noSnapshot}</p>}
      </div>}
      <div className="adm-wallet-adjustment-field">
        <label htmlFor={fieldHelp('delta')}>{adjustmentCopy.amount}</label>
        <input id={fieldHelp('delta')} aria-describedby={fieldHelp('delta-help') + (adjustmentAmount ? ' ' + fieldHelp('direction') : '')} aria-invalid={adjustmentAmount?.valid === false || undefined} disabled={operation.busy || operation.unknown} value={formDelta} onChange={event => setDelta(event.target.value)} pattern="-?(0|[1-9][0-9]*)" />
        <small id={fieldHelp('delta-help')}>{adjustmentCopy.amountHelp}</small>
        {adjustmentAmount && <p id={fieldHelp('direction')} className="adm-wallet-adjustment-direction">{adjustmentAmount.text}</p>}
      </div>
      <div className="adm-wallet-adjustment-field">
        <label htmlFor={fieldHelp('reason')}>{copy.reason}</label>
        <textarea id={fieldHelp('reason')} aria-describedby={fieldHelp('reason-help')} disabled={operation.busy || operation.unknown} maxLength={2000} value={formReason} onChange={event => setReason(event.target.value)} />
        <small id={fieldHelp('reason-help')}>{adjustmentCopy.reasonHelp}</small>
      </div>
      <footer><button type="submit" disabled={!canSubmitAdjustment}>{adjustmentCopy.send}</button></footer>
    </form>}
  </section>;
}

function ProviderControls({ client, copy, permissions, owner }) {
  const [control, setControl] = useState(null);
  const [reason, setReason] = useState('');
  const operation = useFinanceOperation(owner, copy, client, 'provider-controls');
  const load = async () => { const value = await operation.read(() => client.paymentProviderControls()); if (value) setControl(value); };
  useEffect(() => { void load(); }, [client, owner]); // eslint-disable-line react-hooks/exhaustive-deps
  const resume = async product => {
    const result = await operation.mutate(operation.pendingFacts || { control, product, reason: reason.trim() }, (facts, key) => {
      const draft = buildPaymentControlApprovalDraft(facts.control, facts.product, facts.reason, key);
      if (!draft) throw Object.assign(new Error('unavailable draft'), { status: 409 });
      return createAndSubmitFreshGovernedApproval(client, draft);
    });
    if (result) await load();
  };
  return <section><h3>{copy.providerControls}</h3><button disabled={operation.busy} onClick={() => void load()}>{copy.refresh}</button><Feedback operation={operation} copy={copy} />{operation.unknown && <button disabled={operation.busy} onClick={() => void resume(null)}>{copy.retry}</button>}<label>{copy.reason}<textarea disabled={operation.busy || operation.unknown} value={reason} onChange={event => setReason(event.target.value)} /></label>
    {control && <><p>{control.provider_id} / {control.reason_code}</p>{control.resume_draft && permissions.includes('platform.billing.payment-controls.resume.request') && <button disabled={operation.busy || operation.unknown || reason.trim().length < 10} onClick={() => void resume(null)}>{operation.unknown ? copy.retry : copy.resume}</button>}<ul>{(control.products || []).map(item => <li key={item.product_id}>{item.product_id} / {item.reason_code} {item.resume_draft && permissions.includes('platform.billing.payment-controls.resume.request') && <button disabled={operation.busy || operation.unknown || reason.trim().length < 10} onClick={() => void resume(item)}>{operation.unknown ? copy.retry : copy.resume}</button>}</li>)}</ul></>}
  </section>;
}

function initialFinanceTab(initialTab, canReadFinance, canRequestRepair, defaultTab) {
  if (['ledger', 'correction', 'writeoff', 'paymentrepair', 'refundrepair', 'chargebackrepair', 'reconciliationrepair', 'usagesettlementrepair'].includes(initialTab) && canRequestRepair) return initialTab;
  if (canReadFinance && ['orders', 'wallets', 'controls', 'late', 'anomalies', 'receivables', 'refunds', 'adjustments'].includes(initialTab)) return initialTab;
  return defaultTab;
}

const billingTaskGroups = [
  ['accounts', ['wallets', 'receivables', 'adjustments']],
  ['ledger', ['ledger', 'writeoff', 'correction']],
  ['exceptions', ['paymentrepair', 'refundrepair', 'chargebackrepair', 'reconciliationrepair', 'usagesettlementrepair']],
];

function BillingTaskNavigation({ tabs, active, choose, words }) {
  const navID = React.useId();
  if (!tabs.length) return null;
  const remaining = new Map(tabs);
  const groups = billingTaskGroups.map(([key, keys]) => [key, keys.flatMap(tab => {
    if (!remaining.has(tab)) return [];
    const label = remaining.get(tab);
    remaining.delete(tab);
    return [[tab, label]];
  })]);
  if (remaining.size) groups.push(['other', Array.from(remaining)]);
  return <nav className="adm-billing-tasks" aria-label={words.title}>
    {groups.filter(([, items]) => items.length).map(([key, items]) => {
      const titleID = navID + '-' + key;
      return <div className="adm-billing-task-group" role="group" aria-labelledby={titleID} key={key}>
        <h4 id={titleID}>{words[key]}</h4>
        <div className="adm-tabs">{items.map(([tab, label]) => <button type="button" key={tab} aria-pressed={active === tab} onClick={() => choose(tab)}>{label}</button>)}</div>
      </div>;
    })}
  </nav>;
}

export function AdminFinanceWorkspace({ client, me, language, initialTab = 'orders', surface, route, onTabChange, onViewApproval, acceptedSessionEpoch, acceptedPlatformAuthorizationVersion }) {
  const copy = financeCopy[language];
  const permissions = me.platform_permissions || [];
  const owner = `${me.user_id}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`;
  const canReadFinance = permissions.includes('platform.billing.wallets.read');
  const canRequestRepair = permissions.includes('platform.billing.repairs.request');
  const defaultTab = canReadFinance ? 'orders' : canRequestRepair ? 'ledger' : 'approvals';
  const [tab, setTab] = useState(() => initialFinanceTab(initialTab, canReadFinance, canRequestRepair, defaultTab));
  useEffect(() => {
    setTab(initialFinanceTab(initialTab, canReadFinance, canRequestRepair, defaultTab));
  }, [initialTab, canReadFinance, canRequestRepair, defaultTab]);
  const [approvalID, setApprovalID] = useState('');
  const tabs = [...(canRequestRepair ? [['ledger', ledgerRepairCopy[language].title], ['writeoff', ledgerRepairCopy[language].writeOffTitle], ['paymentrepair', ledgerRepairCopy[language].paymentRepairTitle], ['refundrepair', ledgerRepairCopy[language].refundRepairTitle], ['chargebackrepair', ledgerRepairCopy[language].chargebackRepairTitle], ['reconciliationrepair', ledgerRepairCopy[language].reconciliationRepairTitle], ['usagesettlementrepair', ledgerRepairCopy[language].usageSettlementRepairTitle], ['correction', entitlementCorrectionCopy[language].title]] : []),
    ...(canReadFinance ? [['orders', copy.orders], ['wallets', copy.wallet], ...(surface ? ['receivables', 'refunds', 'adjustments'].map(key => [key, adminLayoutCopy[language].financeLists[key]]) : []), ['controls', copy.providerControls]] : []),
    ...(!onViewApproval ? [['approvals', copy.approvals]] : []), ...(canReadFinance ? [['late', copy.sla], ['anomalies', copy.resolve]] : []), ...(surface && permissions.includes('platform.billing.reconciliation.read') ? [['reconciliations', reconciliationCopy[language].title]] : [])];
  const displayedTabs = tabs.filter(([key]) => !surface || (surface === 'billing' ? !['orders', 'controls', 'late', 'anomalies', 'refunds', 'reconciliations'].includes(key) : ['orders', 'controls', 'late', 'anomalies', 'refunds', 'reconciliations'].includes(key)));
  const deniedTab = surface && initialTab && ['orders', 'wallets', 'controls', 'late', 'anomalies', 'receivables', 'refunds', 'adjustments', 'ledger', 'correction', 'writeoff', 'paymentrepair', 'refundrepair', 'chargebackrepair', 'reconciliationrepair', 'usagesettlementrepair', 'reconciliations'].includes(initialTab) && !displayedTabs.some(([key]) => key === initialTab);
  const active = deniedTab ? null : displayedTabs.some(([key]) => key === tab) ? tab : displayedTabs[0]?.[0];
  const choose = key => { setTab(key); onTabChange?.(key); };
  const approval = id => { if (onViewApproval) onViewApproval(id); else { setApprovalID(id); setTab('approvals'); } };
  useEffect(() => { if (initialTab === 'reconciliations' && permissions.includes('platform.billing.reconciliation.read')) setTab(initialTab); }, [initialTab, permissions.includes('platform.billing.reconciliation.read')]);
  const Views = surface ? RetainedWorkspace : ActiveWorkspace;
  const compactOverview = surface === 'billing' && active === 'ledger';
  const overview = surface && canReadFinance && route ? (surface === 'billing'
    ? <details key="overview" className={compactOverview ? 'adm-billing-overview-disclosure' : undefined} open={!compactOverview}>
      <summary hidden={!compactOverview}>{adminLayoutCopy[language].billingTasks.showOverview}</summary>
      <AdminFinanceOverview client={client} language={language} surface="accounts" route={route} permissions={permissions} />
    </details>
    : <AdminFinanceOverview key="overview" client={client} language={language} surface="payments" route={route} permissions={permissions} />) : null;
  const navigation = surface === 'billing'
    ? <BillingTaskNavigation key="navigation" tabs={displayedTabs} active={active} choose={choose} words={adminLayoutCopy[language].billingTasks} />
    : <nav key="navigation" className="adm-tabs">{displayedTabs.map(([key, label]) => <button key={key} aria-pressed={active === key} onClick={() => choose(key)}>{label}</button>)}</nav>;
  const denied = deniedTab ? <p key="denied" role="alert">{copy.permissions}</p> : null;
  const workspaces = <Views key="workspace" active={active} available={tabs.map(([key]) => key)}>{view => <>{['receivables', 'refunds', 'adjustments'].includes(view) && canReadFinance && route && <AdminFinanceReadWorkspace kind={view} {...{ client, language, route, onViewApproval, permissions }} />}{view === 'correction' && canRequestRepair && <AdminEntitlementCorrectionWorkspace {...{ client, copy, language, permissions, owner }} />}{view === 'ledger' && canRequestRepair && <AdminFinancialRepairDraftForm owner={owner} client={client} copy={ledgerRepairCopy[language]} commandTypes={['LEDGER_REPAIR']} onCreated={approval} />}{view === 'writeoff' && canRequestRepair && <AdminFinancialRepairDraftForm client={client} owner={owner} operationCopy={copy} copy={{ ...ledgerRepairCopy[language], title: ledgerRepairCopy[language].writeOffTitle }} commandTypes={['RECEIVABLE_WRITE_OFF']} onCreated={approval} />}{view === 'paymentrepair' && canRequestRepair && <AdminFinancialRepairDraftForm client={client} owner={owner} operationCopy={copy} copy={{ ...ledgerRepairCopy[language], title: ledgerRepairCopy[language].paymentRepairTitle }} commandTypes={['PAYMENT_FINANCIAL_REPAIR']} onCreated={approval} />}{view === 'usagesettlementrepair' && canRequestRepair && <AdminFinancialRepairDraftForm client={client} owner={owner} operationCopy={copy} copy={{ ...ledgerRepairCopy[language], title: ledgerRepairCopy[language].usageSettlementRepairTitle }} commandTypes={['USAGE_SETTLEMENT_REPAIR']} onCreated={approval} />}{view === 'reconciliationrepair' && canRequestRepair && <AdminFinancialRepairDraftForm client={client} owner={owner} operationCopy={copy} copy={{ ...ledgerRepairCopy[language], title: ledgerRepairCopy[language].reconciliationRepairTitle }} canReadCase={permissions.includes('platform.billing.reconciliation.read')} commandTypes={['RECONCILIATION_FINANCIAL_REPAIR']} onCreated={approval} />}{view === 'chargebackrepair' && canRequestRepair && <AdminFinancialRepairDraftForm client={client} owner={owner} operationCopy={copy} copy={{ ...ledgerRepairCopy[language], title: ledgerRepairCopy[language].chargebackRepairTitle }} commandTypes={['CHARGEBACK_FINANCIAL_REPAIR']} onCreated={approval} />}{view === 'refundrepair' && canRequestRepair && <AdminFinancialRepairDraftForm client={client} owner={owner} operationCopy={copy} copy={{ ...ledgerRepairCopy[language], title: ledgerRepairCopy[language].refundRepairTitle }} commandTypes={['REFUND_EXCEPTION_REPAIR']} onCreated={approval} />}{view === 'late' && canReadFinance && <AdminPaymentReviewQueue {...{ client, copy, me, route, language }} />}{view === 'anomalies' && canReadFinance && <AdminPaymentReviewQueue {...{ client, copy, me, route, language }} anomaly />}{view === 'orders' && canReadFinance && <Payments {...{ client, copy, language, permissions, owner, route }} />}{view === 'wallets' && canReadFinance && <Wallets {...{ client, copy, language, permissions, owner, route, acceptedSessionEpoch, acceptedPlatformAuthorizationVersion }} requesterID={me.user_id} onViewApproval={approval} />}{view === 'controls' && canReadFinance && <ProviderControls {...{ client, copy, permissions, owner }} />}{view === 'approvals' && <Approvals {...{ client, copy, permissions, owner }} initialRequestID={approvalID} />}{view === 'reconciliations' && permissions.includes('platform.billing.reconciliation.read') && <AdminReconciliationWorkspace client={client} me={me} language={language} onViewApproval={approval} />}</>}</Views>;
  return <section className="adm-panel">{!surface && <h2>{copy.finance}</h2>}{compactOverview
    ? [navigation, denied, workspaces, overview]
    : [overview, navigation, denied, workspaces]}</section>;
}
