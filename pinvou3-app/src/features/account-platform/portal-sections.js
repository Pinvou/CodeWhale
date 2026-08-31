export const portalNavigationSections = ['creditsUsage', 'ordersInvoices', 'accountSettings'];

export function initialPortalSection(search = '') {
  const requested = new URLSearchParams(search).get('section');
  if (['credits', 'entitlements', 'usage', 'creditsUsage'].includes(requested)) return 'creditsUsage';
  if (['orders', 'invoice', 'ordersInvoices'].includes(requested)) return 'ordersInvoices';
  if (['overview', 'profile', 'closure', 'accountSettings'].includes(requested)) return 'accountSettings';
  return 'creditsUsage';
}

export function availablePortalSection(search = '', availableSections = portalNavigationSections) {
  const requested = initialPortalSection(search);
  return availableSections.includes(requested) ? requested : availableSections[0] || 'accountSettings';
}

export function portalSectionLocation(section, search = '', pathname = '/', preserveProfile = false) {
  const params = new URLSearchParams(search);
  params.set('section', section);
  if (!preserveProfile || section !== 'accountSettings') params.delete('profile');
  const query = params.toString();
  return `${pathname || '/'}${query ? `?${query}` : ''}`;
}
