import React from 'react';
import { accountLayoutCopy, adminLayoutCopy } from '../../shared/i18n.js';

const known = (dictionary, value) => typeof value === 'string' && Object.prototype.hasOwnProperty.call(dictionary, value);
const stateValue = (dictionary, value, missing) => known(dictionary, value) ? dictionary[value] : typeof value === 'string' && value.trim() ? <>{missing} (<code>{value}</code>)</> : missing;
const hasPaymentTime = value => typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) && !value.startsWith('0001-') && Number.isFinite(Date.parse(value.replace(/\.\d+/, '')));
const lifecycle = ['CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'EXPIRED', 'MANUAL_REVIEW'];

export function AdminOrderStatusSummary({ detail, language }) {
  const words = adminLayoutCopy[language].orderStatusSummary, shared = accountLayoutCopy[language].statuses;
  const orderStates = Object.fromEntries(lifecycle.map(key => [key, shared[key]]));
  const paidCount = detail.payment_summary?.paid_count;
  // A tracked receipt is a historical financial fact. It does not decide
  // delivery, current net balance, provider closure, or refund eligibility.
  const sameOrder = Array.isArray(detail.transactions) ? detail.transactions.filter(transaction => transaction?.payment_order_id === detail.order_id) : [];
  const hasReceipt = (Number.isSafeInteger(paidCount) && paidCount > 0) || sameOrder.some(transaction => ['PAID', 'LATE_PAID'].includes(transaction.status));
  const reviewTransactions = sameOrder.filter(transaction => transaction.status === 'REVIEW_REQUIRED');
  // The original successful-payment merge records verified and paid times on a
  // same-order review transaction. Order-level alerts alone do not prove payment.
  const receiptUnderReview = reviewTransactions.some(transaction => hasPaymentTime(transaction.paid_at) && hasPaymentTime(transaction.verified_at));
  const payment = hasReceipt ? words.paymentRecorded : receiptUnderReview ? words.paymentUnderReview : paidCount === 0 && !reviewTransactions.length ? words.noRecordedPayment : words.paymentUnconfirmed;
  const expiredPending = detail.status === 'EXPIRED' && detail.fulfillment_status === 'PENDING';
  return <>
    <dl className="adm-user-details adm-order-status-summary">
      <dt>{words.order}</dt><dd>{stateValue(orderStates, detail.status, words.unknown)}</dd>
      <dt>{words.payment}</dt><dd>{payment}{hasReceipt && receiptUnderReview && <> · {words.paymentUnderReview}</>}</dd>
      <dt>{words.fulfillment}</dt><dd>{stateValue(words.fulfillmentStates, detail.fulfillment_status, words.unknown)}</dd>
      <dt>{words.attention}</dt><dd>{stateValue(words.attentionStates, detail.attention_summary, words.unknown)}</dd>
    </dl>
    {expiredPending && <p><small>{words.expiredPending}</small></p>}
  </>;
}
