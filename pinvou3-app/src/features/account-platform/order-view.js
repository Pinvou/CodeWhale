import { successfulPaymentOrders } from './portal-sections.js';

function timestamp(value) {
  const parsed = new Date(value || '').getTime();
  return Number.isFinite(parsed) ? parsed : 0;
}

export function summarizedPaymentOrders(orders = []) {
  return successfulPaymentOrders(orders)
    .sort((left, right) => timestamp(right?.paid_at) - timestamp(left?.paid_at) || String(right?.order_id || '').localeCompare(String(left?.order_id || '')));
}

export function isPayablePaymentOrder(item, now = Date.now()) {
  return ['CREATED', 'PENDING'].includes(item?.status) && item?.close_status === 'OPEN' && timestamp(item?.expires_at) > now;
}

export function payablePaymentOrders(orders = [], now = Date.now()) {
  return orders
    .filter((item) => isPayablePaymentOrder(item, now))
    .sort((left, right) => timestamp(right?.created_at) - timestamp(left?.created_at) || String(right?.order_id || '').localeCompare(String(left?.order_id || '')));
}

export function paymentOrderRemainingSeconds(item, now = Date.now()) {
  return Math.max(0, Math.ceil((timestamp(item?.expires_at) - now) / 1000));
}

export function orderSummaryStatus(item) {
  if (item?.fulfillment_status !== 'CREDITED') return 'PROCESSING';
  return item?.status === 'PAID_LATE' ? 'LATE_COMPLETED' : 'COMPLETED';
}

export function orderDurationDays(item) {
  const creditedAt = timestamp(item?.credited_at);
  const validUntil = timestamp(item?.valid_until);
  if (!creditedAt || !validUntil || validUntil <= creditedAt) return null;
  return Math.max(1, Math.round((validUntil - creditedAt) / 86_400_000));
}

export function compactOrderID(value) {
  const text = String(value || '');
  return text.length > 14 ? `${text.slice(0, 8)}…` : text || '—';
}
