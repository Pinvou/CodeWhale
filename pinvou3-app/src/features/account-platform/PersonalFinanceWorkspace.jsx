import React, { useCallback, useEffect, useRef, useState } from 'react';
import { financeCopy } from '../../shared/i18n.js';
import { hasPermission } from './authorization-ui.js';
import { formatBalance } from './balance-view.js';
import { formatFen, requireFinanceCollection, requireFinanceID, requirePaymentOrder, integerMoney } from './finance-contract.js';
import { paymentOrderHistory, effectivePaymentChannel, isPayablePaymentOrder, orderSummaryStatus, orderHistoryTimestamp, paymentOrderFacts, upsertPaymentOrderFacts, orderDurationDays } from './order-view.js';
import { localizedProductDisplayName } from './localized-product-display-name.js';
import { secureRequestID } from './request-id.js';
import { personalRechargeOwner, registerPersonalRechargeReauthentication, recoverPersonalRechargeReauthentication, settlePersonalRechargeReauthentication } from './personal-recharge-reauthentication.js';
import { PersonalOrderCloseConfirmation } from './PersonalOrderCloseConfirmation.jsx';
import { createRechargeOrderReader, refreshPaidRechargeRead, startRechargeOrderPolling, startRechargeOrderListPolling } from './recharge-order-polling.js';

import { useRechargePaymentDialog } from './useRechargePaymentDialog.js';
import { RechargePaymentDialog } from './RechargePaymentDialog.jsx';
import { PersonalFinancePortalSections } from './PersonalFinancePortalSections.jsx';

export function PersonalFinanceWorkspace({ client, me, language, onPaid, presentationTargets }) {
  const copy = financeCopy[language];
  const [data, setData] = useState(null);
  const [productID, setProductID] = useState('');
  const [channel, setChannel] = useState('');
  const [order, setOrder] = useState(null);
  const [statement, setStatement] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState(false);
  const [unknown, setUnknown] = useState(false);
  const [recoveryBlocked, setRecoveryBlocked] = useState(false);
  const [now, setNow] = useState(Date.now());
  const [closeConfirmation, setCloseConfirmation] = useState(null);
  const closeIntent = useRef(null);
  const mounted = useRef(false);
  const operationBusy = useRef(false);
  const currentView = useRef(null);
  const generation = useRef(0);
  const orderReader = useRef(null);
  const paidReads = useRef(new WeakSet());
  const readVersions = useRef(new WeakMap());
  const knownOrders = useRef([]), withdrawnOrders = useRef(new Map());
  const orderRevision = useRef(0), readDenied = useRef(false);
  const intent = useRef(null);
  const canCreate = hasPermission(me, 'personal.recharge.create');
  const canRead = hasPermission(me, 'personal.billing.read');
  const canManage = hasPermission(me, 'personal.recharge.manage');
  const owner = personalRechargeOwner(me);
  const sessionEpoch = client.financeSessionEpoch || 0;
  // Render-time facts fence an old dialog even before identity cleanup effects.
  currentView.current = { client, owner, sessionEpoch, order, canRead, canManage, busy, unknown, onPaid, orders: data?.orders || [] };
  const cancelClose = useCallback(expected => {
    if (expected && expected !== closeIntent.current) return;
    closeIntent.current = null; setCloseConfirmation(null);
  }, []);
  const closeIsCurrent = useCallback(pending => {
    const current = currentView.current;
    return pending && mounted.current && !readDenied.current && (current.client.financeSessionEpoch || 0) === current.sessionEpoch && !operationBusy.current && !current.busy && !current.unknown &&
      current.canRead && current.canManage && pending.client === current.client && pending.owner === current.owner &&
      pending.generation === generation.current && pending.order === current.order &&
      pending.orderID === current.order?.order_id && isPayablePaymentOrder(current.order, Date.now());
  }, []);
  useEffect(() => {
    if (closeIntent.current && !closeIsCurrent(closeIntent.current)) cancelClose();
  }, [client, owner, order, canRead, canManage, busy, unknown, now, closeIsCurrent, cancelClose]);
  const ownsView = stamp => {
    const current = currentView.current;
    return mounted.current && stamp === generation.current && current.client === client && current.owner === owner
      && current.sessionEpoch === sessionEpoch && (client.financeSessionEpoch || 0) === sessionEpoch && current.canRead && !readDenied.current;
  };
  const payment = useRechargePaymentDialog({ client, owner, sessionEpoch, enabled: canRead && !readDenied.current, now,
    isCurrent: () => ownsView(generation.current) });
  const showFailure = reason => setError(reason.message?.startsWith('INVALID_FINANCE') ? 'invalid' : reason.status === 403 ? 'permissions' : 'unavailable');
  const readFailure = reason => {
    if (reason?.status === 401 || reason?.status === 403) {
      readDenied.current = true; payment.dismiss(); setData(null); setOrder(null);
    }
    showFailure(reason);
  };
  const rememberOrder = latest => {
    latest = payment.boundOrder(latest);
    const previous = withdrawnOrders.current.get(latest.order_id);
    // These public projections have no summary_version. Once this view has
    // learned a nonpayable state/close fact, an older PENDING cannot undo it.
    if (previous && ['CREATED', 'PENDING'].includes(latest.status)) return { ...latest, ...previous };
    if (!['CREATED', 'PENDING'].includes(latest.status) || latest.close_status !== 'OPEN') withdrawnOrders.current.set(latest.order_id, paymentOrderFacts(latest));
    return latest;
  };
  const acceptCollection = incoming => {
    const orders = incoming.map(rememberOrder);
    knownOrders.current = orders.map(paymentOrderFacts);
    const selectedOrder = currentView.current.order;
    const selectedLatest = selectedOrder && orders.find(item => item.order_id === selectedOrder.order_id);
    if (selectedLatest) setOrder(paymentOrderFacts(selectedLatest));
    const ticket = payment.current();
    if (ticket?.orderID) {
      const latest = orders.find(item => item.order_id === ticket.orderID);
      if (!latest || !isPayablePaymentOrder(latest, Date.now())) payment.dismiss();
      else if (ticket.order) payment.accept(ticket, { ...ticket.order, ...latest, payment_url: ticket.order.payment_url, qr_data: ticket.order.qr_data });
    }
    return orders.map(paymentOrderFacts);
  };
  const load = async (stamp = generation.current, signal) => {
    const revision = orderRevision.current;
    const [products, capabilities, orders, packages] = await Promise.all([
      client.products(signal), client.paymentCapabilities(signal), orderReader.current.readList().catch(reason => { if (ownsView(stamp)) readFailure(reason); throw reason; }), client.entitlementPackages(signal),
    ]);
    const result = { products: requireFinanceCollection(products), capabilities, orders: requireFinanceCollection(orders), packages: requireFinanceCollection(packages) };
    for (const item of result.products) { integerMoney(item.amount_fen); if (!item.product_id || !Array.isArray(item.channels)) throw new Error('INVALID_FINANCE_ACK'); }
    for (const item of result.orders) requirePaymentOrder(item);
    for (const item of result.packages) { requireFinanceID(item.package_id); integerMoney(item.points_remaining_microcredits); integerMoney(item.points_used_microcredits); }
    if (ownsView(stamp)) {
      if (revision === orderRevision.current) result.orders = acceptCollection(result.orders);
      else result.orders = knownOrders.current;
      setData(result);
    }
    return result;
  };
  useEffect(() => {
    const controller = new AbortController();
    const stamp = ++generation.current;
    const reader = createRechargeOrderReader(client, () => ownsView(stamp));
    orderReader.current = reader;
    const retire = () => {
      reader.dispose(); if (orderReader.current === reader) orderReader.current = null;
      mounted.current = false; generation.current++; knownOrders.current = []; withdrawnOrders.current.clear(); closeIntent.current = null; controller.abort();
    };
    readDenied.current = false; orderRevision.current++; knownOrders.current = []; withdrawnOrders.current.clear(); payment.dismiss();
    mounted.current = true; operationBusy.current = true; closeIntent.current = null; setCloseConfirmation(null);
    const recovered = recoverPersonalRechargeReauthentication(client, owner);
    intent.current = recovered.intent; setRecoveryBlocked(recovered.blocked);
    setData(null); setOrder(null); setStatement(''); setUnknown(Boolean(recovered.intent)); setError(recovered.blocked ? 'reauthenticationRecoveryUnavailable' : recovered.intent ? 'unknown' : ''); setNotice(''); setBusy(true);
    if (!canRead) { operationBusy.current = false; setBusy(false); return retire; }
    void load(stamp, controller.signal).catch(reason => { if (stamp === generation.current && reason.name !== 'AbortError') showFailure(reason); }).finally(() => { if (stamp === generation.current) { operationBusy.current = false; setBusy(false); } });
    return retire;
    // Identity/version changes invalidate all financial state and pending intent.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, owner, sessionEpoch, canRead]);
  useEffect(() => registerPersonalRechargeReauthentication(client, { owner, epoch: client.financeSessionEpoch || 0, getIntent: () => intent.current }), [client, owner, client.financeSessionEpoch]);
  useEffect(() => { const timer = globalThis.setInterval(() => setNow(Date.now()), 1000); return () => globalThis.clearInterval(timer); }, []);
  const selected = data?.products.find(item => item.product_id === productID) || data?.products[0];
  const selectedChannel = effectivePaymentChannel(selected?.channels, channel);
  const run = async (action, { reload = true } = {}) => {
    if (busy || operationBusy.current || !mounted.current) return;
    operationBusy.current = true; cancelClose();
    const stamp = generation.current;
    setBusy(true); setError(''); setNotice('');
    try {
      await action(stamp);
      if (reload && ownsView(stamp)) await load(stamp);
    } catch (reason) {
      if (stamp === generation.current && reason.name !== 'AbortError') showFailure(reason);
    } finally { if (stamp === generation.current) { operationBusy.current = false; setBusy(false); } }
  };
  const refreshRecovery = () => {
    const recovered = recoverPersonalRechargeReauthentication(client, owner);
    if (recovered.blocked || recovered.intent && intent.current &&
        (recovered.intent.key !== intent.current.key || recovered.intent.productID !== intent.current.productID || recovered.intent.channel !== intent.current.channel)) {
      setRecoveryBlocked(true); setError('reauthenticationRecoveryUnavailable');
      return;
    }
    setRecoveryBlocked(false);
    if (recovered.intent) { intent.current = recovered.intent; setUnknown(true); setError('unknown'); }
    else if (intent.current) setError('unknown');
    // No retained record can mean that an authoritative old-view receipt
    // settled it. Do not clear an independent current invocation on this read.
  };
  const create = () => run(async stamp => {
    if (!canCreate || recoveryBlocked || (!intent.current && (!selected || !selectedChannel))) return;
    const presentation = payment.begin();
    const hadUnresolved = Boolean(intent.current);
    const current = intent.current || { productID: selected.product_id, channel: selectedChannel, key: `recharge-${secureRequestID()}` };
    intent.current = current;
    try {
      const result = await client.createRecharge(current.productID, current.channel, current.key);
      settlePersonalRechargeReauthentication(client, owner, current);
      if (!ownsView(stamp)) return;
      const displayed = rememberOrder(payment.boundOrder(result, true));
      knownOrders.current = upsertPaymentOrderFacts(knownOrders.current, displayed);
      intent.current = null; setUnknown(false); orderRevision.current++; setOrder(paymentOrderFacts(displayed)); setData(current => current ? { ...current, orders: knownOrders.current } : current); payment.accept(presentation, displayed); setNotice('saved');
    } catch (reason) {
      if (stamp !== generation.current) return;
      // A successful status with invalid/missing ACK is also an unknown outcome.
      if (hadUnresolved || !Number.isInteger(reason.status) || reason.status >= 500) { setUnknown(true); setError('unknown'); }
      else { intent.current = null; setUnknown(false); throw reason; }
    }
  });
  const readOrder = orderID => {
    const request = orderReader.current.read(orderID);
    if (!readVersions.current.has(request)) {
      const stamp = generation.current; readVersions.current.set(request, orderRevision.current);
      void request.catch(reason => { if (ownsView(stamp) && reason.name !== 'AbortError') readFailure(reason); });
    }
    return request;
  };
  const acceptOrderRead = async (latest, request, stamp, refreshList = false) => {
    if (!ownsView(stamp) || (readVersions.current.get(request) !== orderRevision.current && !['PAID', 'PAID_LATE'].includes(latest.status))) return false;
    latest = rememberOrder(payment.boundOrder(latest, true));
    knownOrders.current = upsertPaymentOrderFacts(knownOrders.current, latest);
    orderRevision.current++; setOrder(paymentOrderFacts(latest));
    const ticket = payment.current();
    if (ticket?.order?.order_id === latest.order_id) payment.accept(ticket, latest);
    setData(current => ownsView(stamp) && current ? { ...current,
      orders: knownOrders.current } : current);
    if (['PAID', 'PAID_LATE'].includes(latest.status)) {
      // Manual and automatic consumers share this exact GET Promise. A later
      // GET of the same order may contain new fulfillment facts and refresh again.
      await refreshPaidRechargeRead(latest, request, paidReads.current, async () => {
        await load(stamp);
        if (ownsView(stamp)) await currentView.current.onPaid?.();
      });
    } else if (refreshList) await load(stamp);
    return true;
  };
  const selectOrder = (orderID, continuePayment = false) => run(async stamp => {
    const presentation = continuePayment ? payment.begin(orderID) : null;
    if (!continuePayment) payment.dismiss();
    const request = readOrder(orderID);
    const latest = await request;
    const accepted = await acceptOrderRead(latest, request, stamp);
    if (accepted && ownsView(stamp) && continuePayment) payment.accept(presentation, latest);
    if (ownsView(stamp) && !['PAID', 'PAID_LATE'].includes(latest.status)) await load(stamp);
  }, { reload: false });
  const query = () => order && selectOrder(order.order_id);
  const queryPayment = () => run(async stamp => {
    const ticket = payment.current(); if (!ticket?.order) return;
    const request = readOrder(ticket.order.order_id);
    await acceptOrderRead(await request, request, stamp);
  }, { reload: false });
  const close = () => {
    if (closeIsCurrent(closeIntent.current)) return;
    const current = currentView.current;
    const pending = { client: current.client, owner: current.owner, order: current.order, orderID: current.order?.order_id, generation: generation.current };
    if (!closeIsCurrent(pending)) { cancelClose(); return; }
    closeIntent.current = pending; setCloseConfirmation(pending);
  };
  const confirmClose = pending => {
    if (pending !== closeIntent.current) return;
    if (!closeIsCurrent(pending)) { cancelClose(); return; }
    // Consume synchronously: duplicate clicks cannot reuse this confirmation.
    cancelClose();
    return run(async stamp => { const result = await pending.client.closeRechargeOrder(pending.orderID); if (ownsView(stamp)) { const displayed = rememberOrder(payment.boundOrder(result, true)); knownOrders.current = upsertPaymentOrderFacts(knownOrders.current, displayed); orderRevision.current++; setOrder(paymentOrderFacts(displayed)); const ticket = payment.current(); if (ticket?.order?.order_id === result.order_id) payment.accept(ticket, result); setNotice('saved'); } });
  };
  const claim = () => run(async stamp => {
    if (!order || !statement.trim()) return;
    await client.addPaymentClaim(order.order_id, statement.trim());
    if (stamp === generation.current) { setStatement(''); setNotice('saved'); }
  });
  useEffect(() => {
    if (!canRead || busy || unknown || !isPayablePaymentOrder(payment.order, Date.now()) || !orderReader.current) return undefined;
    const stamp = generation.current, reader = orderReader.current, orderID = payment.order.order_id;
    const isCurrent = () => {
      const current = currentView.current;
      return mounted.current && stamp === generation.current && orderReader.current === reader && current.client === client
        && current.owner === owner && current.sessionEpoch === sessionEpoch && (client.financeSessionEpoch || 0) === sessionEpoch && current.canRead
        && !current.busy && !current.unknown && !readDenied.current && payment.current()?.order?.order_id === orderID;
    };
    const ownsCompletedRead = () => ownsView(stamp) && orderReader.current === reader && currentView.current.canRead
      && !currentView.current.busy && !currentView.current.unknown && currentView.current.order?.order_id === orderID;
    return startRechargeOrderPolling({ reader: { read: readOrder }, isCurrent, getOrder: () => payment.current()?.order,
      allowLatePaid: ownsCompletedRead,
      accept: async (latest, request) => {
        if (isCurrent() || ownsCompletedRead()) await acceptOrderRead(latest, request, stamp);
      },
      onError: reason => { if (isCurrent()) readFailure(reason); },
    });
    // Only identity/status/window changes restart the original query cadence.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, owner, sessionEpoch, canRead, busy, unknown, payment.order?.order_id, payment.order?.status, payment.order?.close_status, payment.order?.expires_at]);
  const hasPending = data?.orders.some(item => isPayablePaymentOrder(item, now)) || false;
  useEffect(() => {
    if (!canRead || !hasPending || readDenied.current || !orderReader.current) return undefined;
    const stamp = generation.current, reader = orderReader.current;
    const isCurrent = () => ownsView(stamp) && orderReader.current === reader;
    return startRechargeOrderListPolling({ isCurrent,
      hasPayableOrders: () => knownOrders.current.some(item => isPayablePaymentOrder(item, Date.now())),
      read: async () => { const revision = orderRevision.current, request = reader.readList(); return { revision, request, value: await request }; },
      accept: async ({ revision, request, value }) => {
        const orders = requireFinanceCollection(value); for (const item of orders) requirePaymentOrder(item);
        if (isCurrent() && revision === orderRevision.current) {
          const previous = knownOrders.current;
          const changed = orders.filter(item => !isPayablePaymentOrder(item, Date.now()) && previous.some(old => old.order_id === item.order_id && isPayablePaymentOrder(old, Date.now())));
          if (changed.length) orderRevision.current++;
          const facts = acceptCollection(orders);
          setData(current => current ? { ...current, orders: facts } : current);
          const paid = changed.find(item => ['PAID', 'PAID_LATE'].includes(item.status));
          if (paid) {
            for (const item of changed) { const concurrent = reader.pending(item.order_id); if (concurrent) paidReads.current.add(concurrent); }
            await refreshPaidRechargeRead(paid, request, paidReads.current, async () => {
              await load(stamp); if (ownsView(stamp)) await currentView.current.onPaid?.();
            });
          }
        }
      },
      onError: reason => { if (isCurrent()) readFailure(reason); },
    });
    // Collection refresh never selects a detail or restarts its presentation.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, owner, sessionEpoch, canRead, hasPending]);
  const payable = isPayablePaymentOrder(order, now);
  const duration = orderDurationDays(order);
  if (!canRead) return null;
  if (presentationTargets) return <>
    {payment.order && <RechargePaymentDialog active={presentationTargets.section === 'creditsUsage'} error={error ? copy[error] : ''} notice={notice ? copy[notice] : ''} order={payment.order} copy={copy} language={language} now={now} busy={busy} onDismiss={payment.dismiss} onQuery={() => void queryPayment()} canRestoreFocus={payment.canRestoreFocus} returnFocus={payment.trigger} />}
    {closeConfirmation && closeIsCurrent(closeConfirmation) && <PersonalOrderCloseConfirmation active={presentationTargets.section === 'ordersInvoices'} copy={copy} orderID={closeConfirmation.orderID} amount={formatFen(closeConfirmation.order.amount_fen, language)} canRestoreFocus={() => closeIsCurrent(closeConfirmation)} onCancel={() => cancelClose(closeConfirmation)} onConfirm={() => void confirmClose(closeConfirmation)} />}
    <PersonalFinancePortalSections language={language} targets={presentationTargets} view={{ copy, data, order, busy, error, notice, unknown, recoveryBlocked, canCreate, canManage, selected, selectedChannel, hasPending, now, payable, statement, paymentOpen: Boolean(payment.order),
      onCreate: () => void create(), onRefresh: () => void run(async () => { refreshRecovery(); }),
      onProduct: id => { setProductID(id); setChannel(''); }, onChannel: setChannel,
      onSelectOrder: (id, showPayment) => void selectOrder(requireFinanceID(id), showPayment),
      onQuery: () => void query(), onCloseOrder: close, onClaim: () => void claim(), onStatement: setStatement,
    }} />
  </>;
  return <section tabIndex={-1} className="ap-finance" aria-label={copy.title}>
    {payment.order && <RechargePaymentDialog error={error ? copy[error] : ''} notice={notice ? copy[notice] : ''} order={payment.order} copy={copy} language={language} now={now} busy={busy} onDismiss={payment.dismiss} onQuery={() => void queryPayment()} canRestoreFocus={payment.canRestoreFocus} returnFocus={payment.trigger} />}
    {closeConfirmation && closeIsCurrent(closeConfirmation) && <PersonalOrderCloseConfirmation copy={copy} orderID={closeConfirmation.orderID} amount={formatFen(closeConfirmation.order.amount_fen, language)} canRestoreFocus={() => closeIsCurrent(closeConfirmation)} onCancel={() => cancelClose(closeConfirmation)} onConfirm={() => void confirmClose(closeConfirmation)} />}
    <header><h2>{copy.title}</h2><button disabled={busy} onClick={() => void run(async () => { refreshRecovery(); })}>{copy.refresh}</button></header>
    {busy && <p role="status">{copy.loading}</p>}
    {error && <p role="alert">{copy[error]}</p>}
    {notice && <p role="status">{copy[notice]}</p>}
    {unknown && <button disabled={busy || recoveryBlocked || !canCreate} onClick={() => void create()}>{copy.retry}</button>}
    {data && <>
      <h3>{copy.products}</h3>
      {data.products.length === 0 ? <p>{copy.empty}</p> : <form onSubmit={event => { event.preventDefault(); void create(); }}>
        <label>{copy.product}<select disabled={busy || unknown || recoveryBlocked} value={selected?.product_id || ''} onChange={event => { setProductID(event.target.value); setChannel(''); }}>
          {data.products.map(item => <option key={item.product_id} value={item.product_id}>{localizedProductDisplayName(item, language, 'product_id')} — {formatFen(item.amount_fen, language)}</option>)}
        </select></label>
        <label>{copy.channel}<select disabled={busy || unknown || recoveryBlocked} value={selectedChannel} onChange={event => setChannel(event.target.value)}>{(selected?.channels || []).map(value => <option key={value} value={value}>{value}</option>)}</select></label>
        {!selectedChannel && <p>{copy.noChannel}</p>}
        <button disabled={busy || unknown || recoveryBlocked || !canCreate || !selectedChannel}>{copy.create}</button>
      </form>}
      <section aria-label={copy.pendingOrders}><h3>{copy.pendingOrders}</h3>
        {!hasPending && <p>{copy.empty}</p>}
        <ul className="ap-pending-payments">{paymentOrderHistory(data.orders).filter(item => isPayablePaymentOrder(item, now)).map(item => <li key={item.order_id}>
          <span>{localizedProductDisplayName(item, language, 'product_id')} · {formatFen(item.amount_fen, language)} · {orderSummaryStatus(item)} · {copy.countdown(Math.max(0, Math.ceil((new Date(item.expires_at).getTime() - now) / 1000)))}</span>
          <button disabled={busy || unknown || recoveryBlocked} onClick={() => void selectOrder(requireFinanceID(item.order_id), true)}>{copy.continuePayment}</button>
        </li>)}</ul>
      </section>
      <h3>{copy.orders}</h3>
      {data.orders.length === 0 && <p>{copy.empty}</p>}
      <table><thead><tr><th>{copy.product}</th><th>{copy.amount}</th><th>{copy.status}</th><th>{copy.paidAt}</th><th>{copy.history}</th></tr></thead><tbody>{paymentOrderHistory(data.orders).map(item => <tr key={item.order_id}>
        <td>{localizedProductDisplayName(item, language, 'product_id')}</td><td>{formatFen(item.amount_fen, language)}</td><td>{orderSummaryStatus(item)}</td><td>{orderHistoryTimestamp(item) || '—'}</td><td><button disabled={busy} onClick={() => void selectOrder(requireFinanceID(item.order_id))}>{copy.details}</button></td>
      </tr>)}</tbody></table>
      {order && <article><h3>{copy.history}</h3><dl><dt>{copy.orderID}</dt><dd>{order.order_id}</dd><dt>{copy.amount}</dt><dd>{formatFen(order.amount_fen, language)}</dd><dt>{copy.status}</dt><dd>{orderSummaryStatus(order)}</dd><dt>{copy.expires}</dt><dd>{order.expires_at}</dd>{duration !== null && <><dt>{copy.validityDays}</dt><dd>{copy.durationDays(duration)}</dd></>}</dl>
        <button disabled={busy} onClick={() => void query()}>{copy.query}</button>
        {payable && canManage && <button disabled={busy || unknown || recoveryBlocked} onClick={close}>{copy.close}</button>}
        {['MANUAL_REVIEW', 'CLOSED', 'FAILED'].includes(order.status) && <form onSubmit={event => { event.preventDefault(); void claim(); }}><label>{copy.statement}<textarea value={statement} maxLength={2000} onChange={event => setStatement(event.target.value)} /></label><button disabled={busy || !statement.trim()}>{copy.claim}</button></form>}
      </article>}
      <h3>{copy.packages}</h3>
      {data.packages.length === 0 && <p>{copy.empty}</p>}
      <table><thead><tr><th>{copy.name}</th><th>{copy.type}</th><th>{copy.remaining}</th><th>{copy.used}</th><th>{copy.validUntil}</th><th>{copy.status}</th></tr></thead><tbody>{data.packages.map(item => <tr key={item.package_id}><td>{localizedProductDisplayName(item, language, 'plan_id')}</td><td>{item.source_type}</td><td>{formatBalance(item.points_remaining_microcredits, language)}</td><td>{formatBalance(item.points_used_microcredits, language)}</td><td>{item.valid_until || item.expires_at}</td><td>{item.status}</td></tr>)}</tbody></table>
    </>}
  </section>;
}
