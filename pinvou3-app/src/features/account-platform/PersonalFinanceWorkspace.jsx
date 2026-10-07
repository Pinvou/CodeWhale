import React, { useEffect, useRef, useState } from 'react';
import { financeCopy } from '../../shared/i18n.js';
import { hasPermission } from './authorization-ui.js';
import { formatBalance } from './balance-view.js';
import { formatFen, requireFinanceCollection, requireFinanceID, requirePaymentOrder, integerMoney, safePaymentURL, safePaymentQR } from './finance-contract.js';
import { paymentOrderHistory, effectivePaymentChannel, isPayablePaymentOrder, orderSummaryStatus } from './order-view.js';
import { localizedProductDisplayName } from './localized-product-display-name.js';
import { secureRequestID } from './request-id.js';

export function PersonalFinanceWorkspace({ client, me, language }) {
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
  const [now, setNow] = useState(Date.now());
  const generation = useRef(0);
  const intent = useRef(null);
  const canCreate = hasPermission(me, 'personal.recharge.create');
  const owner = `${me.user_id}:${me.active_context?.context_version || 0}:${me.active_context?.personal_authorization_version || 0}`;
  const load = async (stamp = generation.current, signal) => {
    const [products, capabilities, orders, packages] = await Promise.all([
      client.products(signal), client.paymentCapabilities(signal), client.rechargeOrders(signal), client.entitlementPackages(signal),
    ]);
    const result = { products: requireFinanceCollection(products), capabilities, orders: requireFinanceCollection(orders), packages: requireFinanceCollection(packages) };
    for (const item of result.products) { integerMoney(item.amount_fen); if (!item.product_id || !Array.isArray(item.channels)) throw new Error('INVALID_FINANCE_ACK'); }
    for (const item of result.orders) requirePaymentOrder(item);
    for (const item of result.packages) { requireFinanceID(item.package_id); integerMoney(item.points_remaining_microcredits); integerMoney(item.points_used_microcredits); }
    if (stamp === generation.current) setData(result);
    return result;
  };
  useEffect(() => {
    const controller = new AbortController();
    const stamp = ++generation.current;
    intent.current = null; setData(null); setOrder(null); setStatement(''); setUnknown(false); setError(''); setNotice(''); setBusy(true);
    if (!hasPermission(me, 'personal.billing.read')) { setBusy(false); return () => { generation.current++; controller.abort(); }; }
    void load(stamp, controller.signal).catch(reason => { if (stamp === generation.current && reason.name !== 'AbortError') setError('unavailable'); }).finally(() => { if (stamp === generation.current) setBusy(false); });
    return () => { generation.current++; controller.abort(); };
    // Identity/version changes invalidate all financial state and pending intent.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, owner]);
  useEffect(() => { const timer = globalThis.setInterval(() => setNow(Date.now()), 1000); return () => globalThis.clearInterval(timer); }, []);
  const selected = data?.products.find(item => item.product_id === productID) || data?.products[0];
  const selectedChannel = effectivePaymentChannel(selected?.channels, channel);
  const run = async action => {
    if (busy) return;
    const stamp = generation.current;
    setBusy(true); setError(''); setNotice('');
    try {
      await action(stamp);
      if (stamp === generation.current) await load(stamp);
    } catch (reason) {
      if (stamp === generation.current) setError(reason.message?.startsWith('INVALID_FINANCE') ? 'invalid' : 'unavailable');
    } finally { if (stamp === generation.current) setBusy(false); }
  };
  const create = () => run(async stamp => {
    if (!canCreate || (!intent.current && (!selected || !selectedChannel))) return;
    const current = intent.current || { productID: selected.product_id, channel: selectedChannel, key: `recharge-${secureRequestID()}` };
    intent.current = current;
    try {
      const result = await client.createRecharge(current.productID, current.channel, current.key);
      if (stamp !== generation.current) return;
      intent.current = null; setUnknown(false); setOrder(result); setNotice('saved');
    } catch (reason) {
      if (stamp !== generation.current) return;
      // A successful status with invalid/missing ACK is also an unknown outcome.
      if (!Number.isInteger(reason.status) || reason.status >= 500) { setUnknown(true); setError('unknown'); }
      else { intent.current = null; setUnknown(false); throw reason; }
    }
  });
  const selectOrder = orderID => run(async stamp => { const result = await client.rechargeOrder(orderID); if (stamp === generation.current) setOrder(result); });
  const query = () => order && selectOrder(order.order_id);
  const close = () => {
    if (!order || !globalThis.confirm(copy.confirmClose)) return;
    return run(async stamp => { const result = await client.closeRechargeOrder(order.order_id); if (stamp === generation.current) { setOrder(result); setNotice('saved'); } });
  };
  const claim = () => run(async stamp => {
    if (!order || !statement.trim()) return;
    await client.addPaymentClaim(order.order_id, statement.trim());
    if (stamp === generation.current) { setStatement(''); setNotice('saved'); }
  });
  const payable = isPayablePaymentOrder(order, now);
  const paymentURL = payable ? safePaymentURL(order?.payment_url) : '';
  const paymentQR = payable ? safePaymentQR(order?.qr_data) : '';
  if (!hasPermission(me, 'personal.billing.read')) return null;
  return <section className="ap-finance" aria-label={copy.title}>
    <header><h2>{copy.title}</h2><button disabled={busy} onClick={() => void run(async () => {})}>{copy.refresh}</button></header>
    {busy && <p role="status">{copy.loading}</p>}
    {error && <p role="alert">{copy[error]}</p>}
    {notice && <p role="status">{copy[notice]}</p>}
    {unknown && <button disabled={busy || !canCreate} onClick={() => void create()}>{copy.retry}</button>}
    {data && <>
      <h3>{copy.products}</h3>
      {data.products.length === 0 ? <p>{copy.empty}</p> : <form onSubmit={event => { event.preventDefault(); void create(); }}>
        <label>{copy.product}<select disabled={busy || unknown} value={selected?.product_id || ''} onChange={event => { setProductID(event.target.value); setChannel(''); }}>
          {data.products.map(item => <option key={item.product_id} value={item.product_id}>{localizedProductDisplayName(item, language, 'product_id')} — {formatFen(item.amount_fen, language)}</option>)}
        </select></label>
        <label>{copy.channel}<select disabled={busy || unknown} value={selectedChannel} onChange={event => setChannel(event.target.value)}>{(selected?.channels || []).map(value => <option key={value} value={value}>{value}</option>)}</select></label>
        {!selectedChannel && <p>{copy.noChannel}</p>}
        <button disabled={busy || unknown || !canCreate || !selectedChannel}>{copy.create}</button>
      </form>}
      <h3>{copy.orders}</h3>
      {data.orders.length === 0 && <p>{copy.empty}</p>}
      <table><thead><tr><th>{copy.product}</th><th>{copy.amount}</th><th>{copy.status}</th><th>{copy.history}</th></tr></thead><tbody>{paymentOrderHistory(data.orders).map(item => <tr key={item.order_id}>
        <td>{localizedProductDisplayName(item, language, 'product_id')}</td><td>{formatFen(item.amount_fen, language)}</td><td>{orderSummaryStatus(item)}</td><td><button disabled={busy} onClick={() => void selectOrder(requireFinanceID(item.order_id))}>{copy.details}</button></td>
      </tr>)}</tbody></table>
      {order && <article><h3>{copy.history}</h3><dl><dt>{copy.orderID}</dt><dd>{order.order_id}</dd><dt>{copy.amount}</dt><dd>{formatFen(order.amount_fen, language)}</dd><dt>{copy.status}</dt><dd>{orderSummaryStatus(order)}</dd><dt>{copy.expires}</dt><dd>{order.expires_at}</dd></dl>
        {paymentURL && <a href={paymentURL} target="_blank" rel="noopener noreferrer">{copy.pay}</a>}
        {paymentQR && <img src={paymentQR} alt={copy.providerQR} width="240" height="240" />}
        <button disabled={busy} onClick={() => void query()}>{copy.query}</button>
        {payable && <button disabled={busy} onClick={() => void close()}>{copy.close}</button>}
        {['MANUAL_REVIEW', 'CLOSED', 'FAILED'].includes(order.status) && <form onSubmit={event => { event.preventDefault(); void claim(); }}><label>{copy.statement}<textarea value={statement} maxLength={2000} onChange={event => setStatement(event.target.value)} /></label><button disabled={busy || !statement.trim()}>{copy.claim}</button></form>}
      </article>}
      <h3>{copy.packages}</h3>
      {data.packages.length === 0 && <p>{copy.empty}</p>}
      <table><thead><tr><th>{copy.name}</th><th>{copy.type}</th><th>{copy.remaining}</th><th>{copy.used}</th><th>{copy.validUntil}</th><th>{copy.status}</th></tr></thead><tbody>{data.packages.map(item => <tr key={item.package_id}><td>{localizedProductDisplayName(item, language, 'plan_id')}</td><td>{item.source_type}</td><td>{formatBalance(item.points_remaining_microcredits, language)}</td><td>{formatBalance(item.points_used_microcredits, language)}</td><td>{item.valid_until || item.expires_at}</td><td>{item.status}</td></tr>)}</tbody></table>
    </>}
  </section>;
}
