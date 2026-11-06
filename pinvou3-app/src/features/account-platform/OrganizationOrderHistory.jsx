import React, { useEffect, useRef, useState } from 'react';
import { accountCenterCopy, financeCopy, accountLayoutCopy } from '../../shared/i18n.js';
import { CopyIdentifier } from './CopyIdentifier.jsx';
import { formatAccountDate, formatRMB } from './account-center-view.js';
import { localizedProductDisplayName } from './localized-product-display-name.js';
import { orderSummaryStatus } from './order-view.js';

// One read owner per organization/session. Pagination cannot carry a previous
// identity's orders into a new wallet, or turn a failed page into an empty one.
export function OrganizationOrderHistory({ api, org, owner, language, version, enabled, onSelect, busy, latestOrder }) {
  const words = accountCenterCopy[language], copy = financeCopy[language], layout = accountLayoutCopy[language];
  const epoch = api.client.financeSessionEpoch || 0;
  const [stored, setPage] = useState({ owner, api, epoch, items: [], cursor: null }), [loading, setLoading] = useState(false), [error, setError] = useState(false);
  const generation = useRef(0), pending = useRef(null), current = useRef(null);
  const page = stored.owner === owner && stored.api === api && stored.epoch === epoch ? stored : { items: [], cursor: null };
  const rows = page.items.map(item => latestOrder?.order_id === item.order_id ? { ...item, ...latestOrder } : item);
  current.current = { owner, api, enabled, epoch };
  const load = async (cursor = '') => {
    if (!enabled || pending.current || current.current.owner !== owner || current.current.api !== api || !current.current.enabled || (api.client.financeSessionEpoch || 0) !== epoch) return;
    const controller = new AbortController(), stamp = ++generation.current;
    pending.current = controller; setLoading(true); setError(false);
    const owns = () => generation.current === stamp && current.current.api === api && current.current.owner === owner && current.current.enabled && (api.client.financeSessionEpoch || 0) === epoch;
    try {
      const result = await api.orders(org, cursor, controller.signal);
      if (owns()) setPage(previous => ({ owner, api, epoch, items: cursor ? [...new Map([...previous.items, ...result.items].map(item => [item.order_id, item])).values()] : result.items, cursor: result.next_cursor }));
    } catch (reason) {
      if (owns() && reason.name !== 'AbortError') { setError(true); if (reason.status === 401 || reason.status === 403) setPage({ owner, api, epoch, items: [], cursor: null }); }
    } finally {
      if (pending.current === controller) pending.current = null;
      if (owns()) setLoading(false);
    }
  };
  useEffect(() => {
    pending.current?.abort(); pending.current = null; generation.current++;
    setPage({ owner, api, epoch, items: [], cursor: null }); setError(false); setLoading(false);
    if (enabled) void load();
    return () => { generation.current++; pending.current?.abort(); pending.current = null; };
    // All asynchronous reads belong to this exact API, actor/context and epoch.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api, org, owner, epoch, enabled, version]);
  if (!enabled) return null;
  const status = item => words.orderStatuses[orderSummaryStatus(item)] || layout.statuses[orderSummaryStatus(item)] || orderSummaryStatus(item);
  return <section className="ap-organization-orders">
    <div className="ap-section-heading"><h4>{words.myOrganizationOrders}</h4><button type="button" disabled={loading} onClick={() => void load()}>{copy.refresh}</button></div>
    <p className="ap-muted">{words.ordersScope}</p>
    {loading && <p role="status">{copy.loading}</p>}{error && <p role="alert">{copy.unavailable}</p>}
    {!loading && !error && !page.items.length && <p className="ap-empty">{words.emptyOrders}</p>}
    {page.items.length > 0 && <div className="ap-order-table-wrap"><table><thead><tr><th>{copy.product}</th><th>{copy.amount}</th><th>{words.createdAt}</th><th>{words.paidAt}</th><th>{copy.status}</th><th>{copy.orderID}</th><th>{copy.details}</th></tr></thead><tbody>
      {rows.map(item => <tr key={item.order_id}><td data-label={copy.product}>{localizedProductDisplayName(item, language, 'product_id')}</td><td data-label={copy.amount}>{formatRMB(item.amount_fen, language)}</td><td data-label={words.createdAt}>{formatAccountDate(item.created_at, language)}</td><td data-label={words.paidAt}>{formatAccountDate(item.paid_at, language)}</td><td data-label={copy.status}><span className="ap-status" data-status={orderSummaryStatus(item)}>{status(item)}</span></td><td data-label={copy.orderID}><CopyIdentifier value={item.order_id} language={language} compact /></td><td data-label={copy.details}><button type="button" disabled={busy} onClick={() => onSelect(item.order_id, item)}>{copy.details}</button></td></tr>)}
    </tbody></table></div>}
    {page.cursor && <div className="ap-action-group"><button type="button" disabled={loading} onClick={() => void load(page.cursor)}>{error ? copy.retry : words.moreOrders}</button></div>}
  </section>;
}
