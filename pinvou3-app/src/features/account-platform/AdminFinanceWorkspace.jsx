import React, { useEffect, useState } from 'react';
import { financeCopy, ledgerRepairCopy } from '../../shared/i18n.js';
import { AdminFinancialRepairDraftForm } from './AdminFinancialRepairDraftForm.jsx';
import { formatBalance } from './balance-view.js';
import { formatFen, integerMoney, requireFinanceCollection, requireFinanceID, wholeRefundTargets } from './finance-contract.js';
import { useFinanceOperation } from './useFinanceOperation.js';
import { walletAdjustmentApprovalDraft, buildPaymentControlApprovalDraft, currentApprovalEditDraft, adjustmentApprovalAction, latestApprovalByTarget, currentPendingAdjustment, walletReplacementEligible } from './admin-finance.js';
import { createAndSubmitFreshGovernedApproval, updateAndSubmitGovernedApproval } from './admin-governed-approval-workflow.js';
import { loadFinancePage } from './admin-finance-pagination.js';
import { secureRequestID } from './request-id.js';
import { approvalBusinessRoute, approvalRequester, approvalCommands } from './admin-approval-actions.js';
import { AdminPaymentReviewQueue } from './AdminPaymentReviewQueue.jsx';

function Feedback({ operation, copy }) {
  return <>{operation.busy && <p role="status">{copy.loading}</p>}{operation.error && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}</>;
}

function Payments({ client, copy, language, permissions, owner }) {
  const [page, setPage] = useState(null);
  const [query, setQuery] = useState('');
  const [cursor, setCursor] = useState('');
  const [previous, setPrevious] = useState([]);
  const [detail, setDetail] = useState(null);
  const [targets, setTargets] = useState([]);
  const [targetID, setTargetID] = useState('');
  const [reason, setReason] = useState('');
  const [refundEnabled, setRefundEnabled] = useState(false);
  const operation = useFinanceOperation(owner, copy, client, 'payments');
  const load = async (next = cursor) => {
    const result = await operation.read(async () => {
      const [orders, overview] = await Promise.all([loadFinancePage({ q: query, cursor: next }, filters => client.paymentOrders(filters), () => { setCursor(''); setPrevious([]); }), client.request('billing', '/admin/v1/overview')]);
      return { orders: orders.value, cursor: orders.query.cursor, enabled: overview?.payment_capabilities?.normal_refund_creation_enabled === true };
    });
    if (result) { setPage(result.orders); setCursor(result.cursor); setRefundEnabled(result.enabled); }
  };
  useEffect(() => { void load(''); return () => {}; }, [client, owner]); // eslint-disable-line react-hooks/exhaustive-deps
  const open = async id => {
    const result = await operation.read(async () => { const value = await client.paymentOrder(id); if (value?.order_id !== id) throw new Error('INVALID_FINANCE_ACK'); return { value, targets: wholeRefundTargets(value) }; });
    if (result) { setDetail(result.value); setTargets(result.targets); setTargetID(result.targets[0]?.package.package_id || ''); setReason(''); }
  };
  const refund = async () => {
    const target = targets.find(item => item.package.package_id === targetID);
    const facts = operation.pendingFacts || (detail && target && { action: 'refund', orderID: detail.order_id, transactionID: target.transaction.payment_transaction_id, packageID: targetID, amountFen: String(target.transaction.amount_fen), reason: reason.trim() });
    if (!facts || (!operation.unknown && (!reason.trim() || !refundEnabled || !permissions.includes('platform.billing.repairs.request') || !globalThis.confirm(copy.confirmRefund)))) return;
    const value = await operation.mutate(facts, async (current, key) => {
      if (current.action === 'repair') return client.repairPaymentOrder(current.orderID, current.reason);
      const receipt = await client.refundPaymentOrder(current.orderID, current.transactionID, current.packageID, current.reason, key);
      if (integerMoney(receipt.amount_fen) !== integerMoney(current.amountFen)) throw new Error('INVALID_FINANCE_ACK');
      return receipt;
    });
    if (value) { await open(facts.orderID); await load(); }
  };
  return <section><h3>{copy.orders}</h3><form onSubmit={event => { event.preventDefault(); setPrevious([]); void load(''); }}><label>{copy.search}<input value={query} onChange={event => setQuery(event.target.value)} /></label><button disabled={operation.busy || operation.unknown}>{copy.search}</button></form><Feedback operation={operation} copy={copy} />
    {operation.unknown && <button disabled={operation.busy} onClick={() => void refund()}>{copy.retry}</button>}
    {page && <table><thead><tr><th>{copy.orderID}</th><th>{copy.amount}</th><th>{copy.status}</th><th>{copy.actions}</th></tr></thead><tbody>{page.items.map(item => <tr key={item.order_id}><td>{item.order_id}</td><td>{formatFen(item.amount_fen, language)}</td><td>{item.status}</td><td><button disabled={operation.busy || operation.unknown} onClick={() => void open(item.order_id)}>{copy.details}</button></td></tr>)}</tbody></table>}
    <button disabled={operation.busy || operation.unknown || !previous.length} onClick={() => { const values = [...previous]; const value = values.pop(); setPrevious(values); void load(value); }}>{copy.previous}</button><button disabled={operation.busy || operation.unknown || !page?.next_cursor} onClick={() => { setPrevious(values => [...values, cursor]); void load(page.next_cursor); }}>{copy.next}</button>
    {detail && <article><h4>{copy.details}: {detail.order_id}</h4><p>{detail.status} / {detail.fulfillment_status} / {detail.attention_summary}</p>
      {permissions.includes('platform.billing.orders.sync') && <button disabled={operation.busy || operation.unknown} onClick={async () => { const value = await operation.read(() => client.syncOrder(detail.order_id)); if (value) await open(detail.order_id); }}>{copy.query}</button>}
      <label>{copy.reason}<textarea disabled={operation.busy || operation.unknown} maxLength={500} value={reason} onChange={event => setReason(event.target.value)} /></label>
      {detail.status === 'MANUAL_REVIEW' && permissions.includes('platform.billing.repairs.request') && <button disabled={operation.busy || operation.unknown || !reason.trim()} onClick={async () => { const value = await operation.mutate({ action: 'repair', orderID: detail.order_id, reason: reason.trim() }, facts => client.repairPaymentOrder(facts.orderID, facts.reason)); if (value) await open(detail.order_id); }}>{copy.orderRepair}</button>}
      <h4>{copy.refunds}</h4>{targets.length ? <label>{copy.transaction} / {copy.package}<select disabled={operation.busy || operation.unknown} value={targetID} onChange={event => setTargetID(event.target.value)}>{targets.map(item => <option key={item.package.package_id} value={item.package.package_id}>{item.transaction.payment_transaction_id} / {item.package.package_id} — {formatFen(item.transaction.amount_fen, language)}</option>)}</select></label> : <p>{copy.refundUnavailable}</p>}
      {!refundEnabled && <p>{copy.refundUnavailable}</p>}
      {permissions.includes('platform.billing.repairs.request') && targets.length > 0 && <button disabled={operation.busy || operation.unknown || !refundEnabled || !reason.trim()} onClick={() => void refund()}>{operation.unknown ? copy.retry : copy.refunds}</button>}
    </article>}
  </section>;
}

const defaultApprovalCommandTypes = approvalCommands;

export function Approvals({ client, copy, permissions, owner, commandTypes = defaultApprovalCommandTypes, initialRequestID = '', requestID = initialRequestID, query = null, pendingUserID = null, onStateChange, onNavigate, centerCopy }) {
  const [items, setItems] = useState([]);
  const [page, setPage] = useState(null);
  const [cursor, setCursor] = useState('');
  const [previous, setPrevious] = useState([]);
  const [detail, setDetail] = useState(null);
  const [reason, setReason] = useState('');
  const operation = useFinanceOperation(owner, copy, client, 'approvals');
  const load = async (next = cursor) => { const result = await operation.read(() => loadFinancePage({ ...query, cursor: next }, filters => client.governedApprovalRequests(filters), () => { setCursor(''); setPrevious([]); })); if (result) { const value = result.value; setPage(value); setCursor(result.query.cursor); setItems(requireFinanceCollection(value).filter(item => commandTypes.includes(item.command_type) && (!pendingUserID || approvalRequester(item) !== pendingUserID))); } };
  const queryKey = JSON.stringify(query);
  useEffect(() => { onStateChange?.(operation.busy || operation.unknown); }, [onStateChange, operation.busy, operation.unknown]);
  useEffect(() => { let active = true; void (async () => { setDetail(null); setPage(null); setItems([]); setPrevious([]); await load(''); if (active && requestID) await open(requestID); })(); return () => { active = false; }; }, [client, owner, requestID, queryKey, pendingUserID]); // eslint-disable-line react-hooks/exhaustive-deps
  const open = async id => { const value = await operation.read(() => client.governedApprovalRequest(id)); if (value) { if (value.approval_request_id !== id || !commandTypes.includes(value.command_type)) return; setDetail(value); } };
  const act = async action => {
    let facts = operation.pendingFacts;
    if (!facts) {
      const draft = action === 'EDIT' ? await operation.read(() => currentApprovalEditDraft(client, detail, reason)) : null;
      if (action === 'EDIT' && !draft) return;
      facts = { detail, action, reason, draft };
    }
    const value = await operation.mutate(facts, async (facts, invocationKey) => {
      if (facts.action === 'EDIT') await updateAndSubmitGovernedApproval(client, facts.detail, { ...facts.draft, idempotency_key: invocationKey });
      else if (facts.action === 'SUBMIT') await client.submitGovernedApprovalRequest(facts.detail.approval_request_id, facts.detail);
      else if (facts.action === 'WITHDRAW') await client.withdrawGovernedApprovalRequest(facts.detail.approval_request_id, facts.detail);
      else await client.decideGovernedApproval(facts.detail.approval_request_id, facts.detail, facts.action, facts.reason);
      return true;
    });
    if (value) { await open(facts.detail.approval_request_id); await load(); }
  };
  const businessRoute = approvalBusinessRoute(detail);
  return <section><h3>{copy.approvals}</h3><button disabled={operation.busy} onClick={() => void load()}>{copy.refresh}</button><Feedback operation={operation} copy={copy} />{operation.unknown && <button disabled={operation.busy} onClick={() => void act('')}>{copy.retry}</button>}<table><thead><tr><th>{copy.command}</th><th>{copy.status}</th>{centerCopy && <><th>{centerCopy.target}</th><th>{centerCopy.requester}</th><th>{centerCopy.updated}</th></>}<th>{copy.details}</th></tr></thead><tbody>{items.map(item => <tr key={item.approval_request_id}><td>{item.command_type}</td><td>{item.status}</td>{centerCopy && <><td>{item.target?.resource_id || item.target_resource_id}</td><td>{approvalRequester(item)}</td><td>{item.updated_at}</td></>}<td><button disabled={operation.busy || operation.unknown} onClick={() => void open(item.approval_request_id)}>{copy.details}</button></td></tr>)}</tbody></table>
    <button disabled={operation.busy || operation.unknown || !previous.length} onClick={() => { const values = [...previous]; const value = values.pop(); setPrevious(values); void load(value); }}>{copy.previous}</button><button disabled={operation.busy || operation.unknown || !page?.next_cursor} onClick={() => { setPrevious(values => [...values, cursor]); void load(page.next_cursor); }}>{copy.next}</button>
    {detail && <article><h4>{detail.approval_request_id}</h4><p>{detail.command_type} / {detail.status} / {copy.version}: {detail.version}</p><p>{detail.reason}</p>{businessRoute && onNavigate && <button disabled={operation.busy || operation.unknown} onClick={() => onNavigate(businessRoute)}>{centerCopy.openBusinessArea}</button>}<h5>{copy.command}</h5><pre>{JSON.stringify(detail.command || {}, null, 2)}</pre>{detail.execution && <><h5>{copy.effects}</h5><p>{detail.execution.attempt_status} / {detail.execution.error_class} / {detail.execution.next_retry_at} / {detail.execution.execution_deadline}</p></>}<label>{copy.reason}<textarea disabled={operation.busy || operation.unknown} value={reason} onChange={event => setReason(event.target.value)} maxLength={2000} /></label>{['EDIT', 'SUBMIT', 'WITHDRAW', 'APPROVE', 'REJECT'].filter(action => detail.allowed_actions?.includes(action)).map(action => {
      const approver = action === 'APPROVE' || action === 'REJECT';
      const allowed = approver ? permissions.includes({ LEDGER_REPAIR: 'platform.billing.repairs.approve', WALLET_ADJUSTMENT: 'platform.billing.adjustments.approve', PAYMENT_PROVIDER_RESUME: 'platform.billing.payment-controls.resume.approve', PAYMENT_PRODUCT_RESUME: 'platform.billing.payment-controls.resume.approve', TOKEN_MODEL_LIFECYCLE_CHANGE: 'platform.models.lifecycle.approve', TOKEN_MODEL_CATALOG_ROLLBACK: 'platform.models.catalog.rollback.approve', TOKEN_CREDENTIAL_REVOKE: 'platform.tokens.credentials.revoke.approve' }[detail.command_type]) : true;
      return allowed && <button key={action} disabled={operation.busy || operation.unknown || ((approver || action === 'EDIT') && reason.trim().length < 10)} onClick={() => void act(action)}>{copy[{ EDIT: 'editSubmit', SUBMIT: 'submit', WITHDRAW: 'withdraw', APPROVE: 'approve', REJECT: 'reject' }[action]]}</button>;
    })}</article>}
  </section>;
}

function Wallets({ client, copy, language, permissions, owner, requesterID, onViewApproval }) {
  const [page, setPage] = useState(null);
  const [cursor, setCursor] = useState('');
  const [previous, setPrevious] = useState([]);
  const [walletID, setWalletID] = useState('');
  const [delta, setDelta] = useState('');
  const [reason, setReason] = useState('');
  const [ledger, setLedger] = useState(null);
  const [adjustments, setAdjustments] = useState(null);
  const [adjustmentCursor, setAdjustmentCursor] = useState('');
  const [adjustmentPrevious, setAdjustmentPrevious] = useState([]);
  const [selected, setSelected] = useState(null);
  const [replacement, setReplacement] = useState(null);
  const operation = useFinanceOperation(owner, copy, client, 'wallets');
  const canRequest = permissions.includes('platform.billing.repairs.request');
  const loadAdjustments = async (next = adjustmentCursor) => {
    const result = await operation.read(() => loadFinancePage({ cursor: next, status: 'PENDING_APPROVAL' }, filters => client.adjustments(filters), () => { setAdjustmentCursor(''); setAdjustmentPrevious([]); }));
    if (result) { setAdjustments(result.value); setAdjustmentCursor(result.query.cursor); setSelected(null); }
  };
  const load = async (next = cursor) => {
    const result = await operation.read(() => loadFinancePage({ cursor: next }, filters => client.wallets(filters), () => { setCursor(''); setPrevious([]); }));
    if (result) { setPage(result.value); setCursor(result.query.cursor); }
  };
  useEffect(() => { void (async () => { await load(''); await loadAdjustments(''); })(); }, [client, owner]); // eslint-disable-line react-hooks/exhaustive-deps
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
    if (facts.action === 'create' && (!facts.walletID || facts.reason.length < 10)) return;
    const value = await operation.mutate(facts, async (facts, operationKey) => {
      if (facts.action === 'resume') {
        const fresh = await currentPendingAdjustment(client, facts.item, requesterID);
        // Fresh history is checked again by the workflow before its write.
        return createAndSubmitFreshGovernedApproval(client, walletAdjustmentApprovalDraft(fresh, fresh.reason, fresh.request_idempotency_key));
      }
      const key = facts.replacement?.key || operationKey;
      if (facts.replacement && (facts.walletID !== facts.replacement.walletID || !walletReplacementEligible(await client.governedApprovalRequest(facts.replacement.requestID), facts.replacement.adjustmentID, facts.walletID, requesterID))) {
        throw Object.assign(new Error('old adjustment approval changed'), { status: 409 });
      }
      const item = await client.createAdjustment({ wallet_id: requireFinanceID(facts.walletID), amount_microcredits: facts.delta, reason: facts.reason, idempotency_key: key });
      if (item.wallet_id !== facts.walletID || integerMoney(item.amount_microcredits) !== integerMoney(facts.delta) || item.requested_by !== requesterID || item.request_idempotency_key !== key || item.reason !== facts.reason || item.status !== 'PENDING_APPROVAL' || !Number.isSafeInteger(item.version) || item.version < 1) throw new Error('INVALID_FINANCE_ACK');
      requireFinanceID(item.adjustment_id);
      return createAndSubmitFreshGovernedApproval(client, walletAdjustmentApprovalDraft(item, facts.reason, key));
    });
    if (value) { setReplacement(null); setWalletID(''); setDelta(''); setReason(''); await load(); await loadAdjustments(); }
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
  return <section><h3>{copy.wallet}</h3><button disabled={operation.busy || operation.unknown} onClick={async () => { await load(); await loadAdjustments(); }}>{copy.refresh}</button><Feedback operation={operation} copy={copy} />{operation.unknown && <button disabled={operation.busy} onClick={() => void adjust()}>{copy.retry}</button>}{page && <table><thead><tr><th>{copy.wallet}</th><th>{copy.total}</th><th>{copy.remaining}</th><th>{copy.status}</th></tr></thead><tbody>{page.items.map(item => <tr key={item.wallet_id}><td><button disabled={operation.busy || operation.unknown || Boolean(replacement)} onClick={async () => { setWalletID(item.wallet_id); if (permissions.includes('platform.billing.ledger.read')) { const value = await operation.read(() => client.walletLedger(item.wallet_id)); if (value) setLedger(value); } }}>{item.wallet_id}</button></td><td>{formatBalance(item.posted_microcredits, language)}</td><td>{formatBalance(item.available_microcredits, language)}</td><td>{item.status} / {item.financial_status}</td></tr>)}</tbody></table>}
    <button disabled={operation.busy || operation.unknown || !previous.length} onClick={() => { const values = [...previous]; const value = values.pop(); setPrevious(values); void load(value); }}>{copy.previous}</button><button disabled={operation.busy || operation.unknown || !page?.next_cursor} onClick={() => { setPrevious(values => [...values, cursor]); void load(page.next_cursor); }}>{copy.next}</button>
    {ledger && <ul>{requireFinanceCollection(ledger).map(item => <li key={item.posting_id}>{item.journal_entry_id} / {item.created_at}</li>)}</ul>}
    <section data-finance-adjustments><h4>{copy.adjustment}</h4>{adjustments && <table><thead><tr><th>{copy.wallet}</th><th>{copy.delta}</th><th>{copy.status}</th><th>{copy.details}</th></tr></thead><tbody>{adjustments.items.map(item => <tr key={item.adjustment_id}><td>{item.wallet_id}</td><td>{formatBalance(item.amount_microcredits, language)}</td><td>{item.status}<br />{item.request_idempotency_key}</td><td><button disabled={operation.busy || operation.unknown} onClick={() => void inspect(item)}>{copy.details}</button></td></tr>)}</tbody></table>}
      <button disabled={operation.busy || operation.unknown || !adjustmentPrevious.length} onClick={() => { const values = [...adjustmentPrevious]; const value = values.pop(); setAdjustmentPrevious(values); void loadAdjustments(value); }}>{copy.previous}</button><button disabled={operation.busy || operation.unknown || !adjustments?.next_cursor} onClick={() => { setAdjustmentPrevious(values => [...values, adjustmentCursor]); void loadAdjustments(adjustments.next_cursor); }}>{copy.next}</button>
      {selected && <article><h4>{selected.item.adjustment_id}</h4><p>{selected.item.reason}</p>{selected.approval && <><p>{copy.approvals}: {selected.approval.approval_request_id} / {selected.approval.status}</p><button disabled={operation.busy || operation.unknown} onClick={() => onViewApproval(selected.approval.approval_request_id)}>{copy.details}</button></>}{resumeAvailable && <button disabled={operation.busy || operation.unknown} onClick={resume}>{copy.submit}</button>}{canRequest && walletReplacementEligible(selected.approval, selected.item.adjustment_id, selected.item.wallet_id, requesterID) && <button disabled={operation.busy || operation.unknown} onClick={beginReplacement}>{copy.newInvocation}</button>}</article>}
    </section>
    {replacement && <p>{copy.approvals}: {replacement.requestID} / {copy.wallet}: {replacement.walletID}</p>}
    {canRequest && <form onSubmit={event => { event.preventDefault(); void adjust(); }}><label>{copy.wallet}<input disabled={operation.busy || operation.unknown} readOnly={Boolean(replacement)} value={walletID} onChange={event => setWalletID(event.target.value)} /></label><label>{copy.delta}<input disabled={operation.busy || operation.unknown} value={delta} onChange={event => setDelta(event.target.value)} pattern="-?(0|[1-9][0-9]*)" /></label><label>{copy.reason}<textarea disabled={operation.busy || operation.unknown} maxLength={2000} value={reason} onChange={event => setReason(event.target.value)} /></label><button disabled={operation.busy || operation.unknown || !walletID || !delta || reason.trim().length < 10}>{operation.unknown ? copy.retry : copy.draft}</button></form>}
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

export function AdminFinanceWorkspace({ client, me, language, initialTab = 'orders' }) {
  const copy = financeCopy[language];
  const permissions = me.platform_permissions || [];
  const owner = `${me.user_id}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`;
  const canReadFinance = permissions.includes('platform.billing.wallets.read');
  const canRequestRepair = permissions.includes('platform.billing.repairs.request');
  const defaultTab = canReadFinance ? 'orders' : canRequestRepair ? 'ledger' : 'approvals';
  const [tab, setTab] = useState(canReadFinance || (initialTab === 'ledger' && canRequestRepair) ? initialTab : defaultTab);
  useEffect(() => {
    if (initialTab === 'ledger' && canRequestRepair) setTab('ledger');
    else if (canReadFinance && ['orders', 'wallets', 'controls'].includes(initialTab)) setTab(initialTab);
    else setTab(defaultTab);
  }, [initialTab, canReadFinance, canRequestRepair, defaultTab]);
  const [approvalID, setApprovalID] = useState('');
  const tabs = [...(canRequestRepair ? [['ledger', ledgerRepairCopy[language].title]] : []),
    ...(canReadFinance ? [['orders', copy.orders], ['wallets', copy.wallet], ['controls', copy.providerControls]] : []),
    ['approvals', copy.approvals], ...(canReadFinance ? [['late', copy.sla], ['anomalies', copy.resolve]] : [])];
  return <section className="adm-panel"><h2>{copy.finance}</h2><nav>{tabs.map(([key, label]) => <button key={key} aria-pressed={tab === key} onClick={() => setTab(key)}>{label}</button>)}</nav><div key={`${owner}:${tab}`}>{tab === 'ledger' && canRequestRepair && <AdminFinancialRepairDraftForm client={client} copy={ledgerRepairCopy[language]} onCreated={id => { setApprovalID(id); setTab('approvals'); }} />}{tab === 'late' && canReadFinance && <AdminPaymentReviewQueue {...{ client, copy, me }} />}{tab === 'anomalies' && canReadFinance && <AdminPaymentReviewQueue {...{ client, copy, me }} anomaly />}{tab === 'orders' && canReadFinance && <Payments {...{ client, copy, language, permissions, owner }} />}{tab === 'wallets' && canReadFinance && <Wallets {...{ client, copy, language, permissions, owner }} requesterID={me.user_id} onViewApproval={id => { setApprovalID(id); setTab('approvals'); }} />}{tab === 'controls' && canReadFinance && <ProviderControls {...{ client, copy, permissions, owner }} />}{tab === 'approvals' && <Approvals {...{ client, copy, permissions, owner }} initialRequestID={approvalID} />}</div></section>;
}
