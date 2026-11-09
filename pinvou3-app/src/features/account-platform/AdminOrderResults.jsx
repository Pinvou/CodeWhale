import React from 'react';
import { accountLayoutCopy, adminLayoutCopy, financeCopy } from '../../shared/i18n.js';
import { formatFen } from './finance-contract.js';

const dateLabel = (value, locale) => {
  if (!value || !Number.isFinite(Date.parse(value))) return '—';
  const fmt = new Intl.DateTimeFormat(locale, { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' });
  const fields = Object.fromEntries(fmt.formatToParts(new Date(value)).map(part => [part.type, part.value]));
  return [fields.year, fields.month, fields.day].join('-') + ' ' + [fields.hour, fields.minute, fields.second].join(':');
};
const identifier = value => typeof value === 'string' && value ? <bdi><code>{value}</code></bdi> : '—';

export function AdminOrderResults({ page, language, disabled, onOpen }) {
  const words = adminLayoutCopy[language].orderListContext, copy = financeCopy[language];
  const locale = { zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language];
  const statuses = accountLayoutCopy[language].statuses;
  if (!page.items.length) return <p role="status">{words.emptyPage}</p>;
  const columns = [copy.orderID, words.participants, words.product, words.paymentChannel, words.created, words.amount, copy.status, copy.actions];
  return <div className="adm-table-wrap"><table className="adm-table adm-order-list"><thead><tr>{columns.map(label => <th scope="col" key={label}>{label}</th>)}</tr></thead>
    <tbody>{page.items.map(item => <tr key={item.order_id}>
      <td data-label={columns[0]}>{identifier(item.order_id)}{item.merchant_order_no && <small>{words.merchant}: {identifier(item.merchant_order_no)}</small>}</td>
      <td data-label={columns[1]}><small>{words.payer}</small>{identifier(item.payer_user_id)}<small>{words.requester}</small>{identifier(item.requester_id)}</td>
      <td data-label={columns[2]}>{identifier(item.product_id)}{item.provider_sku && <small>SKU: {identifier(item.provider_sku)}</small>}</td>
      <td data-label={columns[3]}><small>{words.provider}</small>{identifier(item.provider_id)}<small>{words.channel}</small>{identifier(item.channel)}</td>
      <td data-label={columns[4]}>{dateLabel(item.created_at, locale)}</td>
      <td data-label={columns[5]}>{formatFen(item.amount_fen, language)}</td>
      <td data-label={columns[6]}>{Object.prototype.hasOwnProperty.call(statuses, item.status) ? statuses[item.status] : identifier(item.status)}</td>
      <td data-label={columns[7]}><button type="button" disabled={disabled} aria-label={copy.details + ': ' + item.order_id} onClick={event => onOpen(item.order_id, event.currentTarget)}>{copy.details}</button></td>
    </tr>)}</tbody>
  </table></div>;
}
