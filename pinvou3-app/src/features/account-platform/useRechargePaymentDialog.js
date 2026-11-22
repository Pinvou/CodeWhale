import { useCallback, useEffect, useRef, useState } from 'react';
import { isPayablePaymentOrder, paymentOrderFacts } from './order-view.js';

// A presentation belongs to one explicit user action and one authorized view.
// Closing it retires only that presentation, never the order or its payment.
export function useRechargePaymentDialog({ client, owner, sessionEpoch, enabled, now, isCurrent }) {
  const facts = useRef(null), active = useRef(null), deadlines = useRef(new Map());
  const [snapshot, setSnapshot] = useState(null);
  facts.current = { client, owner, sessionEpoch, enabled, isCurrent };
  const owns = useCallback(ticket => {
    const current = facts.current;
    return ticket && current.enabled && current.isCurrent() && ticket.client === current.client && ticket.owner === current.owner
      && ticket.sessionEpoch === current.sessionEpoch && (current.client.financeSessionEpoch || 0) === ticket.sessionEpoch;
  }, []);
  const dismiss = useCallback(() => { if (active.current) active.current.order = paymentOrderFacts(active.current.order); active.current = null; setSnapshot(null); }, []);
  const begin = useCallback((orderID = null) => {
    const current = facts.current;
    const ticket = { client: current.client, owner: current.owner, sessionEpoch: current.sessionEpoch, orderID, order: null, trigger: globalThis.document?.activeElement };
    if (active.current) active.current.order = paymentOrderFacts(active.current.order);
    active.current = owns(ticket) ? ticket : null; setSnapshot(null);
    return active.current;
  }, [owns]);
  const boundOrder = useCallback((order, remember = false) => {
    const deadline = new Date(order.expires_at).getTime(), frozen = deadlines.current.get(order.order_id);
    const expires = frozen === undefined ? deadline : Math.min(frozen, deadline);
    if (remember && Number.isFinite(expires)) deadlines.current.set(order.order_id, expires);
    return { ...order, expires_at: Number.isFinite(expires) ? new Date(expires).toISOString() : order.expires_at };
  }, []);
  const accept = useCallback((ticket, order) => {
    if (!ticket || active.current !== ticket || !owns(ticket)) return;
    order = boundOrder(order, true);
    if (!isPayablePaymentOrder(order, Date.now())) { dismiss(); return; }
    ticket.orderID = order.order_id; ticket.order = order; setSnapshot({ ticket, order });
  }, [boundOrder, dismiss, owns]);
  const current = useCallback(() => owns(active.current) ? active.current : null, [owns]);
  useEffect(() => {
    if (active.current && (!owns(active.current) || active.current.order && !isPayablePaymentOrder(active.current.order, now))) dismiss();
  }, [client, owner, sessionEpoch, enabled, now, owns, dismiss]);
  useEffect(() => { deadlines.current.clear(); }, [client, owner, sessionEpoch]);
  useEffect(() => () => { if (active.current) active.current.order = paymentOrderFacts(active.current.order); active.current = null; deadlines.current.clear(); }, []);
  const order = snapshot && active.current === snapshot.ticket && owns(snapshot.ticket) && isPayablePaymentOrder(snapshot.order, now) ? snapshot.order : null;
  return { order, boundOrder, trigger: snapshot?.ticket.trigger, begin, accept, current, dismiss, canRestoreFocus: () => snapshot && owns(snapshot.ticket) };
}
