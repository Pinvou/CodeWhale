import { usePortalDialogScope } from './portal-dialog-scope.jsx';
import React, { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { formatFen, safePaymentQR, safePaymentURL } from './finance-contract.js';
import { isPayablePaymentOrder, paymentOrderRemainingSeconds, paymentScanPrompt } from './order-view.js';

function PaymentCode({ source, copy, children, onRetryFocus }) {
  const [status, setStatus] = useState('loading'), [attempt, setAttempt] = useState(0), [slow, setSlow] = useState(false);
  useEffect(() => {
    setSlow(false);
    if (status !== 'loading') return undefined;
    const timer = setTimeout(() => setSlow(true), 15_000);
    return () => clearTimeout(timer);
  }, [status, attempt]);
  return <div className="ap-payment-code" data-qr-state={source ? status : 'preparing'} aria-busy={!source || status === 'loading'}>
    {source && <img key={attempt} src={source} alt={copy.providerQR} width="240" height="240" hidden={status !== 'loaded'} onLoad={() => setStatus('loaded')} onError={() => setStatus('error')} />}
    {(!source || status === 'loading') && <div className="ap-payment-code-placeholder" role="status"><span className="ap-payment-spinner" aria-hidden="true" /><p>{source ? copy.qrLoading : copy.paymentPreparing}</p>{slow && <p>{copy.paymentSlow}</p>}</div>}
    {source && status === 'error' && <div className="ap-payment-code-placeholder" role="alert"><p>{copy.qrLoadFailed}</p><button type="button" onClick={() => { onRetryFocus(); setStatus('loading'); setAttempt(value => value + 1); }}>{copy.qrRetry}</button></div>}
    {source && status === 'loaded' && children}
  </div>;
}

export function RechargePaymentDialog({ order, copy, language, now, busy, onDismiss, onQuery, canRestoreFocus, returnFocus, active = true, error = '', notice = '' }) {
  const scopeActive = usePortalDialogScope();
  const visible = scopeActive && active;
  const titleID = useId(), dialog = useRef(null), closeButton = useRef(null), restore = useRef(canRestoreFocus);
  restore.current = canRestoreFocus;
  useLayoutEffect(() => {
    if (!visible) return undefined;
    const previous = returnFocus || document.activeElement, element = dialog.current;
    closeButton.current?.focus();
    return () => {
      if (previous?.isConnected && element?.contains(document.activeElement) && !previous.closest('[hidden]') && restore.current()) {
        const target = previous.disabled ? previous.closest('section[tabindex]') : previous; target?.focus();
      }
    };
  }, [visible]);
  const keyDown = event => {
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); onDismiss(); return; }
    if (event.key !== 'Tab') return;
    const controls = [...dialog.current.querySelectorAll('button:not(:disabled), a[href]')];
    const first = controls[0], last = controls[controls.length - 1];
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
  };
  const qr = safePaymentQR(order.qr_data), url = safePaymentURL(order.payment_url);
  if (!visible) return null;
  return createPortal(<div className="ap-order-close-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) onDismiss(); }}>
    <div className="ap-order-close-dialog ap-payment-dialog" ref={dialog} role="dialog" aria-modal="true" aria-labelledby={titleID} onKeyDown={keyDown}>
      <h3 id={titleID}>{copy.paymentDialog}</h3>{error && <p role="alert">{error}</p>}{notice && <p role="status">{notice === copy.saved && isPayablePaymentOrder(order, now) ? copy.paymentOrderCreated : notice}</p>}
      <dl><dt>{copy.orderID}</dt><dd>{order.order_id}</dd><dt>{copy.amount}</dt><dd>{formatFen(order.amount_fen, language)}</dd><dt>{copy.channel}</dt><dd>{copy.channels?.[order.channel] || order.channel || '—'}</dd></dl>
      <p>{copy.countdown(paymentOrderRemainingSeconds(order, now))}</p>
      {(qr || !url && isPayablePaymentOrder(order, now)) && <PaymentCode key={order.order_id + ':' + qr} source={qr} copy={copy} onRetryFocus={() => closeButton.current?.focus()}><p>{paymentScanPrompt(copy, order.qr_scan_apps)}</p></PaymentCode>}
      {url && <p><a href={url} target="_blank" rel="noopener noreferrer">{copy.pay}</a></p>}
      <footer><button ref={closeButton} onClick={onDismiss}>{copy.dismissPayment}</button><button disabled={busy} onClick={onQuery}>{copy.query}</button></footer>
    </div>
  </div>, document.body);
}
