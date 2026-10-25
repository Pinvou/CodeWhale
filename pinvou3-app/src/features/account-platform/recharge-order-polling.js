import { isPayablePaymentOrder } from './order-view.js';

// Preserve the implemented O/F browser query cadence, including its jitter.
export function rechargePollDelay(attempt, random = Math.random) {
  const base = Math.min(5000, 2000 + Math.max(0, attempt) * 500);
  return Math.round(base * (0.9 + random() * 0.2));
}
const retired = () => new DOMException('Recharge view retired', 'AbortError');

// One reader belongs to one mounted, authorized client/owner/session generation.
// Manual queries and the timer share a GET; retiring the view aborts them all.
export function createRechargeOrderReader(client, isCurrent) {
  const controller = new AbortController(), pending = new Map();
  let closed = false;
  const epoch = client.financeSessionEpoch || 0;
  const current = () => !closed && isCurrent() && (client.financeSessionEpoch || 0) === epoch;
  return {
    read(orderID) {
      if (!current()) return Promise.reject(retired());
      if (pending.has(orderID)) return pending.get(orderID);
      const result = Promise.resolve().then(() => {
        if (!current()) throw retired();
        return client.rechargeOrder(orderID, controller.signal);
      }).then(value => {
        if (!current()) throw retired();
        return value;
      }).finally(() => { if (pending.get(orderID) === result) pending.delete(orderID); });
      pending.set(orderID, result);
      return result;
    },
    pending(orderID) { return pending.get(orderID); },
    readList() {
      if (!current()) return Promise.reject(retired());
      if (pending.has('list')) return pending.get('list');
      const result = Promise.resolve().then(() => {
        if (!current()) throw retired();
        return client.rechargeOrders(controller.signal);
      }).then(value => { if (!current()) throw retired(); return value; })
        .finally(() => { if (pending.get('list') === result) pending.delete('list'); });
      pending.set('list', result); return result;
    },
    dispose() { closed = true; controller.abort(); pending.clear(); },
  };
}

export function startRechargeOrderPolling({ reader, getOrder, isCurrent, accept, onError, allowLatePaid,
  timers = globalThis, now = Date.now, random = Math.random }) {
  const orderID = getOrder()?.order_id;
  let stopped = false, timer, attempt = 0;
  const current = () => !stopped && isCurrent() && getOrder()?.order_id === orderID;
  const payable = () => current() && isPayablePaymentOrder(getOrder(), now());
  const stop = () => { stopped = true; timers.clearTimeout(timer); };
  const schedule = () => { if (payable()) timer = timers.setTimeout(() => { void poll(); }, rechargePollDelay(attempt++, random)); };
  const poll = async () => {
    if (!payable()) return;
    let latest;
    try {
      const request = reader.read(orderID);
      latest = await request;
      if (!current()) {
        // A dismissed/expired window stops scheduling, but an already-started
        // owned paid read may still refresh facts without restoring instructions.
        if (allowLatePaid?.() && ['PAID', 'PAID_LATE'].includes(latest.status)) await accept(latest, request);
        return;
      }
      await accept(latest, request);
      if (current() && isPayablePaymentOrder(latest, now())) schedule();
    } catch (reason) {
      if (!current() || reason?.name === 'AbortError') return;
      onError?.(reason);
      if (reason?.status === 401 || reason?.status === 403) { stop(); return; }
      if (!latest || isPayablePaymentOrder(latest, now())) schedule();
    }
  };
  schedule();
  return stop;
}

// Both surfaces may consume the same read through a timer and a manual action.
// Deduplicate its view refresh, not subsequent reads of the same paid order.
export async function refreshPaidRechargeRead(order, request, seen, refresh) {
  if (!['PAID', 'PAID_LATE'].includes(order.status) || seen.has(request)) return;
  seen.add(request);
  await refresh();
}

// A closed payment window still receives callback changes through the collection.
// This reader never selects an order and never fetches its QR/detail endpoint.
export function startRechargeOrderListPolling({ read, hasPayableOrders, isCurrent, accept, onError,
  timers = globalThis, random = Math.random }) {
  let stopped = false, timer, attempt = 0;
  const current = () => !stopped && isCurrent();
  const schedule = () => { if (current() && hasPayableOrders()) timer = timers.setTimeout(() => { void poll(); }, rechargePollDelay(attempt++, random)); };
  const poll = async () => {
    if (!current() || !hasPayableOrders()) return;
    try { const result = await read(); if (!current()) return; await accept(result); }
    catch (reason) {
      if (!current() || reason?.name === 'AbortError') return;
      onError?.(reason);
      if (reason?.status === 401 || reason?.status === 403) { stopped = true; return; }
    }
    schedule();
  };
  schedule();
  return () => { stopped = true; timers.clearTimeout(timer); };
}
