import { localizedProductDisplayName } from './localized-product-display-name.js';

function timestamp(value) {
  const parsed = new Date(value || '').getTime();
  return Number.isFinite(parsed) ? parsed : 0;
}

export function paymentOrderHistory(orders = []) {
  return [...orders]
    .sort((left, right) => timestamp(right?.created_at) - timestamp(left?.created_at) || String(right?.order_id || '').localeCompare(String(left?.order_id || '')));
}

export function orderHistoryTimestamp(item) {
  return timestamp(item?.paid_at) ? item.paid_at : item?.created_at;
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

/** @param {unknown} channels */
export function hasSelectablePaymentChannels(channels = []) {
  if (!Array.isArray(channels)) return false;
  const uniqueChannels = new Set(channels.map((channel) => String(channel || '').trim().toUpperCase()).filter(Boolean));
  return uniqueChannels.size > 1;
}

/**
 * @param {unknown} channels
 * @param {unknown} selectedChannel
 */
export function effectivePaymentChannel(channels = [], selectedChannel = '') {
  if (!Array.isArray(channels)) return '';
  const available = [...new Set(channels.map((channel) => String(channel || '').trim().toUpperCase()).filter(Boolean))];
  const selected = String(selectedChannel || '').trim().toUpperCase();
  return available.includes(selected) ? selected : available[0] || '';
}

/**
 * @param {{ scanToPay?: string, scanToPayWithApps?: (apps: string[]) => string, channels?: Record<string, string> }} copy
 * @param {unknown} scanApps
 */
export function paymentScanPrompt(copy, scanApps = []) {
  if (!Array.isArray(scanApps) || typeof copy?.scanToPayWithApps !== 'function') return copy?.scanToPay || '';
  const labels = [...new Set(scanApps.map((app) => String(app || '').trim().toUpperCase()).filter(Boolean))]
    .map((app) => copy.channels?.[app] || app);
  return labels.length ? copy.scanToPayWithApps(labels) : copy.scanToPay;
}

export function orderSummaryStatus(item) {
  if (['PAID', 'PAID_LATE'].includes(item?.status)) {
    if (item?.fulfillment_status !== 'CREDITED') return 'PROCESSING';
    return item.status === 'PAID_LATE' ? 'LATE_COMPLETED' : 'COMPLETED';
  }
  if (['CREATED', 'PENDING'].includes(item?.status)) {
    if (item?.close_status === 'CLOSE_REQUESTED') return 'CLOSING';
    if (item?.close_status === 'LOCALLY_CLOSED_EXTERNAL_PENDING' || item?.close_status === 'EXPIRED') return 'EXPIRED';
    if (item?.close_status === 'PROVIDER_CLOSED') return 'CLOSED';
  }
  return item?.status || 'UNKNOWN';
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

export function paymentOrderDisplayName(item, language) {
  return localizedProductDisplayName(item, language, 'product_id');
}
