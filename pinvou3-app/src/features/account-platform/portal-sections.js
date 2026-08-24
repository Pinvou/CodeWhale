export const portalNavigationSections = ['creditsUsage', 'ordersInvoices', 'accountSettings'];

const successfulPaymentStates = new Set(['PAID', 'PAID_LATE']);

export function successfulPaymentOrders(orders = []) {
  return orders.filter(order => successfulPaymentStates.has(order?.status));
}

export function initialPortalSection(search = '') {
  const requested = new URLSearchParams(search).get('section');
  if (['credits', 'entitlements', 'usage', 'creditsUsage'].includes(requested)) return 'creditsUsage';
  if (['orders', 'invoice', 'ordersInvoices'].includes(requested)) return 'ordersInvoices';
  return 'accountSettings';
}
