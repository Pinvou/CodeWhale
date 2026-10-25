import React, { useId, useLayoutEffect, useRef } from 'react';
import { createPortal } from 'react-dom';
import { formatFen, safePaymentQR, safePaymentURL } from './finance-contract.js';
import { paymentOrderRemainingSeconds, paymentScanPrompt } from './order-view.js';

export function RechargePaymentDialog({ order, copy, language, now, busy, onDismiss, onQuery, canRestoreFocus, returnFocus }) {
  const titleID = useId(), dialog = useRef(null), closeButton = useRef(null), restore = useRef(canRestoreFocus);
  restore.current = canRestoreFocus;
  useLayoutEffect(() => {
    const previous = returnFocus || document.activeElement, element = dialog.current;
    closeButton.current?.focus();
    return () => {
      if (previous?.isConnected && element.contains(document.activeElement) && restore.current()) {
        const target = previous.disabled ? previous.closest('section[tabindex]') : previous; target?.focus();
      }
    };
  }, []);
  const keyDown = event => {
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); onDismiss(); return; }
    if (event.key !== 'Tab') return;
    const controls = [...dialog.current.querySelectorAll('button:not(:disabled), a[href]')];
    const first = controls[0], last = controls[controls.length - 1];
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
  };
  const qr = safePaymentQR(order.qr_data), url = safePaymentURL(order.payment_url);
  return createPortal(<div className="ap-order-close-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) onDismiss(); }}>
    <div className="ap-order-close-dialog ap-payment-dialog" ref={dialog} role="dialog" aria-modal="true" aria-labelledby={titleID} onKeyDown={keyDown}>
      <h3 id={titleID}>{copy.paymentDialog}</h3>
      <dl><dt>{copy.orderID}</dt><dd>{order.order_id}</dd><dt>{copy.amount}</dt><dd>{formatFen(order.amount_fen, language)}</dd><dt>{copy.channel}</dt><dd>{copy.channels?.[order.channel] || order.channel || '—'}</dd></dl>
      <p>{copy.countdown(paymentOrderRemainingSeconds(order, now))}</p>
      {qr && <><img src={qr} alt={copy.providerQR} width="240" height="240" /><p>{paymentScanPrompt(copy, order.qr_scan_apps)}</p></>}
      {url && <p><a href={url} target="_blank" rel="noopener noreferrer">{copy.pay}</a></p>}
      <footer><button ref={closeButton} onClick={onDismiss}>{copy.dismissPayment}</button><button disabled={busy} onClick={onQuery}>{copy.query}</button></footer>
    </div>
  </div>, document.body);
}
