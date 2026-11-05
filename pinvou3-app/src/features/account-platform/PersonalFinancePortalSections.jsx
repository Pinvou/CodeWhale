import { PortalDialogScope } from './portal-dialog-scope.jsx';
import React, { useState } from 'react';
import { createPortal } from 'react-dom';
import { accountLayoutCopy } from '../../shared/i18n.js';
import { PortalDialog } from './PortalDialog.jsx';
import { PortalBenefits } from './PortalBenefits.jsx';
import { formatFen } from './finance-contract.js';
import { localizedProductDisplayName } from './localized-product-display-name.js';
import { isPayablePaymentOrder, orderHistoryTimestamp, orderSummaryStatus, paymentOrderHistory, orderDurationDays } from './order-view.js';

function date(value, language) {
  const parsed = new Date(value);
  return value && Number.isFinite(parsed.getTime()) ? parsed.toLocaleString({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language]) : '—';
}

// Presentation is split into stable DOM targets; the parent remains the one
// owner of payment polling, in-flight keys, retries and reauthentication.
export function PersonalFinancePortalSections({ view, targets, language }) {
  const words = accountLayoutCopy[language], { copy, data, order } = view;
  const [dialog, setDialog] = useState(null);
  const status = value => words.statuses[value] || value;
  const feedback = <div className="ap-finance-feedback">
    {view.busy && <p role="status">{copy.loading}</p>}
    {view.error && <p className="ap-alert error" role="alert">{copy[view.error]}</p>}
    {view.notice && <p className="ap-alert" role="status">{copy[view.notice]}</p>}
    {view.unknown && <div className="ap-alert" role="status"><p>{words.recoveryPending}</p><button disabled={view.busy || view.recoveryBlocked || !view.canCreate} onClick={view.onCreate}>{copy.retry}</button></div>}
  </div>;
  const credits = <section className="ap-portal-finance" aria-label={copy.title}>
    <div className="ap-section-heading"><h3>{copy.products}</h3><button type="button" disabled={view.busy} onClick={view.onRefresh}>{copy.refresh}</button></div>
    {data && <>
      {!data.products.length ? <p>{copy.empty}</p> : <form className="ap-recharge-form" onSubmit={event => { event.preventDefault(); view.onCreate(); }}>
        <div className="ap-product-picker">{data.products.map(item => <label key={item.product_id} className={item.product_id === view.selected?.product_id ? 'selected' : ''}><input type="radio" name="portal-product" disabled={view.busy || view.unknown || view.recoveryBlocked} value={item.product_id} checked={item.product_id === view.selected?.product_id} onChange={() => view.onProduct(item.product_id)} /><strong>{localizedProductDisplayName(item, language, 'product_id')}</strong><span>{formatFen(item.amount_fen, language)}</span></label>)}</div>
        <div className="ap-recharge-controls"><label>{copy.channel}<select disabled={view.busy || view.unknown || view.recoveryBlocked} value={view.selectedChannel} onChange={event => view.onChannel(event.target.value)}>{(view.selected?.channels || []).map(value => <option key={value} value={value}>{copy.channels?.[value] || value}</option>)}</select></label><button disabled={view.busy || view.unknown || view.recoveryBlocked || !view.canCreate || !view.selectedChannel}>{copy.create}</button></div>
        {!view.selectedChannel && <p>{copy.noChannel}</p>}
      </form>}
      {view.hasPending && <section aria-label={copy.pendingOrders}><h3>{copy.pendingOrders}</h3><ul className="ap-pending-payments">{paymentOrderHistory(data.orders).filter(item => isPayablePaymentOrder(item, view.now)).map(item => <li key={item.order_id}><span>{localizedProductDisplayName(item, language, 'product_id')} · {formatFen(item.amount_fen, language)} · {copy.countdown(Math.max(0, Math.ceil((new Date(item.expires_at).getTime() - view.now) / 1000)))}</span><button disabled={view.busy || view.unknown || view.recoveryBlocked} onClick={() => view.onSelectOrder(item.order_id, true)}>{copy.continuePayment}</button></li>)}</ul></section>}
      <PortalBenefits items={data.packages} language={language} now={view.now} />
    </>}
  </section>;
  const orders = <section className="ap-order-history" aria-label={copy.orders}>
    <div className="ap-section-heading"><h3>{copy.orders}</h3><button type="button" disabled={view.busy} onClick={view.onRefresh}>{copy.refresh}</button></div>
    {data && !data.orders.length && <p>{copy.empty}</p>}
    {data && <div className="ap-order-table-wrap"><table><thead><tr><th>{copy.product}</th><th>{copy.amount}</th><th>{copy.paidAt}</th><th>{copy.status}</th><th>{copy.orderID}</th><th>{copy.details}</th></tr></thead><tbody>{paymentOrderHistory(data.orders).map(item => <tr key={item.order_id}>
      <td data-label={copy.product}>{localizedProductDisplayName(item, language, 'product_id')}</td><td data-label={copy.amount}>{formatFen(item.amount_fen, language)}</td><td data-label={copy.paidAt}>{date(orderHistoryTimestamp(item), language)}</td><td data-label={copy.status}><span className="ap-status">{status(orderSummaryStatus(item))}</span></td><td data-label={copy.orderID}><code>{item.order_id}</code></td><td data-label={copy.details}><button type="button" disabled={view.busy} onClick={() => { setDialog({ kind: 'order', id: item.order_id }); view.onSelectOrder(item.order_id); }}>{copy.details}</button></td>
    </tr>)}</tbody></table></div>}
    <section className="ap-invoice-notice"><h3>{words.invoices}</h3><p>{words.invoicesUnavailable}</p></section>
  </section>;
  return <>
    {targets.feedback && createPortal(feedback, targets.feedback)}
    {targets.credits && createPortal(<PortalDialogScope active={targets.section === 'creditsUsage'}>{credits}</PortalDialogScope>, targets.credits)}
    {targets.orders && createPortal(<PortalDialogScope active={targets.section === 'ordersInvoices'}>{orders}</PortalDialogScope>, targets.orders)}
    {dialog?.kind === 'order' && order?.order_id === dialog.id && !view.paymentOpen && <PortalDialog active={targets.section === 'ordersInvoices'} title={copy.history} closeLabel={words.closeDialog} onClose={() => setDialog(null)} busy={view.busy}>
      {feedback}<dl><dt>{copy.orderID}</dt><dd>{order.order_id}</dd><dt>{copy.amount}</dt><dd>{formatFen(order.amount_fen, language)}</dd><dt>{copy.status}</dt><dd>{status(orderSummaryStatus(order))}</dd><dt>{copy.expires}</dt><dd>{date(order.expires_at, language)}</dd>{orderDurationDays(order) !== null && <><dt>{copy.validityDays}</dt><dd>{copy.durationDays(orderDurationDays(order))}</dd></>}</dl>
      <div className="ap-dialog-actions"><button disabled={view.busy} onClick={view.onQuery}>{copy.query}</button>{view.payable && view.canManage && <button disabled={view.busy || view.unknown || view.recoveryBlocked} onClick={view.onCloseOrder}>{copy.close}</button>}</div>
      {['MANUAL_REVIEW', 'CLOSED', 'FAILED'].includes(order.status) && <form onSubmit={event => { event.preventDefault(); view.onClaim(); }}><label>{copy.statement}<textarea value={view.statement} maxLength={2000} onChange={event => view.onStatement(event.target.value)} /></label><button disabled={view.busy || !view.statement.trim()}>{copy.claim}</button></form>}
    </PortalDialog>}
  </>;
}
