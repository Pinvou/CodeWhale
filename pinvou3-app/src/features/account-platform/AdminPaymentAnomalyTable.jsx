import React from 'react';
import { adminLayoutCopy } from '../../shared/i18n.js';
import { anomalyStatusText } from './anomaly-queue-status.js';
import './admin-payment-anomaly-queue.css';

const association = ['payment_order_id','payment_transaction_id','payment_evidence_id','refund_id','forced_refund_case_id','chargeback_case_id'];
const deadlines = ['claim_due_at','first_response_due_at','mitigation_due_at','resolution_due_at'];
const locale = { zh:'zh-CN', en:'en-GB', ja:'ja-JP' };
const valid = value => typeof value === 'string' && value.trim();
const timestamp = value => typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T/.test(value) && !value.startsWith('0001-01-01') && Number.isFinite(Date.parse(value));
const valueAt = (value, language, words) => timestamp(value) ? <time dir="ltr" dateTime={value}>{new Intl.DateTimeFormat(locale[language], { timeZone:'Asia/Shanghai',year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit',hourCycle:'h23' }).format(new Date(value))}</time> : words.missing;

export function AdminPaymentAnomalyTable({ items, emptyState, language, detailsLabel, disabled, onView }) {
  const copy = adminLayoutCopy[language].anomalyQueueFields;
  const status = adminLayoutCopy[language].anomalyQueueStatus;
  const headings = [copy.case,copy.type,copy.status,copy.severity,copy.owner,copy.range,copy.deadlines,copy.actions];
  return <div className="adm-anomaly-queue">
    <p>{copy.ordering} {copy.zone}</p>
    {(
      // biome-ignore lint/a11y/noNoninteractiveTabindex: The named overflow region must remain keyboard-scrollable.
      <div className="adm-table-wrap" role="region" aria-label={copy.case} tabIndex={0}><table className="adm-table adm-anomaly-table">
      <thead><tr>{headings.map(label => <th key={label} scope="col">{label}</th>)}</tr></thead>
      <tbody>{emptyState && <tr><td colSpan={headings.length} className="adm-anomaly-empty-cell">{emptyState}</td></tr>}{items.map(value => {
        const links = association.filter(key => valid(value[key]));
        const type = Object.prototype.hasOwnProperty.call(copy.types, value.anomaly_type) ? copy.types[value.anomaly_type] : null;
        return <tr key={value.case_id}>
          <td data-label={copy.case}><code dir="ltr">{value.case_id}</code></td>
          <td data-label={copy.type}>{type && <strong>{type}</strong>}{valid(value.anomaly_type) ? <code dir="ltr">{value.anomaly_type}</code> : copy.missing}</td>
          <td data-label={copy.status}>{anomalyStatusText(status, value.status)}</td>
          <td data-label={copy.severity}>{valid(value.severity) ? value.severity : copy.missing}</td>
          <td data-label={copy.owner}>{valid(value.owner_id) ? <><code dir="ltr">{value.owner_id}</code>{timestamp(value.claimed_at) && <small>{copy.claimed} {valueAt(value.claimed_at, language, copy)}</small>}</> : copy.unassigned}</td>
          <td data-label={copy.range}>{links.length ? <dl>{links.map(key => <div key={key}><dt>{copy[key]}</dt><dd><code dir="ltr">{value[key]}</code></dd></div>)}</dl> : copy.missing}</td>
          <td data-label={copy.deadlines}><dl>{deadlines.map(key => <div key={key}><dt>{copy[key]}</dt><dd>{valueAt(value[key], language, copy)}</dd></div>)}</dl></td>
          <td data-label={copy.actions}><button type="button" disabled={disabled} onClick={() => onView(value.case_id)}>{detailsLabel}</button></td>
        </tr>;
      })}</tbody>
    </table></div>)}
  </div>;
}
