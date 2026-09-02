const route = (tab, query = {}, exact = false) => ({ tab, query, exact });

export function financeOverviewDrilldown(surface, key) {
  if (surface === 'accounts') {
    return {
      wallet_count: route('wallets'),
      posted_microcredits: route('wallets'),
      reserved_microcredits: route('wallets'),
      available_microcredits: route('wallets'),
      receivable_microcredits: route('receivables'),
      pending_adjustment_count: route('adjustments', { status: 'PENDING_APPROVAL' }, true),
      at_risk_hold_count: route('holds'),
    }[key] || null;
  }
  if (surface === 'payments') {
    return {
      orders_created_count: route('orders'),
      payments_received_fen: route('orders'),
      late_review_open_count: route('lateReviews'),
      payment_anomaly_open_count: route('anomalies', { status: 'OPEN' }, true),
      reconciliation_open_count: route('reconciliations'),
      refund_attention_count: route('refunds'),
      provider_controlled_count: route('controls'),
      product_controlled_count: route('controls'),
    }[key] || null;
  }
  if (surface === 'pricing') return route('prices');
  if (surface === 'products') {
    return {
      product_draft_count: route('catalog', { product_lifecycle_status: 'DRAFT' }, true),
      product_on_sale_count: route('catalog', { product_lifecycle_status: 'ON_SALE' }, true),
      product_off_sale_count: route('catalog', { product_lifecycle_status: 'OFF_SALE' }, true),
      product_deleted_count: route('catalog', { product_lifecycle_status: 'DELETED', product_include_deleted: 'true' }, true),
      admission_pending_count: route('catalog'),
      admission_blocked_count: route('catalog'),
    }[key] || null;
  }
  return null;
}

export function financeOverviewMetricTimeScope(surface, key) {
  return surface === 'payments' && ['orders_created_count', 'payments_received_fen'].includes(key) ? 'window' : 'point';
}

export function formatFinanceOverviewValue(metric, { formatCount, formatPoints, formatMoney }) {
  if (metric?.unit === 'MICROCREDITS') return formatPoints(metric.value);
  if (metric?.unit === 'CNY_FEN') return formatMoney(metric.value);
  return formatCount(metric?.value);
}
