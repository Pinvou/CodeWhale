import { hasPermission } from './authorization-ui.js';

export const portalNavigationSections = Object.freeze(['creditsUsage', 'ordersInvoices', 'myOrganization', 'accountSettings']);

export function initialPortalSection(search = '') {
  const params = new URLSearchParams(search);
  if (params.get('account_closure') === 'confirm') return 'accountSettings';
  const requested = params.get('section');
  if (['credits', 'entitlements', 'usage', 'creditsUsage'].includes(requested)) return 'creditsUsage';
  if (['orders', 'invoice', 'ordersInvoices'].includes(requested)) return 'ordersInvoices';
  if (['organization', 'myOrganization'].includes(requested)) return 'myOrganization';
  if (['overview', 'profile', 'closure', 'accountSettings'].includes(requested)) return 'accountSettings';
  return 'creditsUsage';
}

export function personalPortalSections(me) {
  if (!me) return [];
  const organization = me.active_context?.context_type === 'ORGANIZATION' && Boolean(me.active_context.organization_id);
  const can = permission => hasPermission(me, permission);
  const credits = organization
    ? ['org.wallet.read', 'org.quota.read.self', 'org.usage.read.self'].some(can)
    : ['personal.billing.read', 'personal.usage.read'].some(can);
  const management = organization && ['org.profile.read', 'org.members.read', 'org.units.read', 'org.roles.read', 'org.models.read', 'org.wallet.read', 'org.quota.read.self', 'org.quota.read.all'].some(can);
  return portalNavigationSections.filter(section => section === 'accountSettings'
    || section === 'creditsUsage' && credits
    || section === 'ordersInvoices' && !organization && can('personal.billing.read')
    || section === 'myOrganization' && management);
}

export function availablePortalSection(search = '', available = portalNavigationSections) {
  const requested = initialPortalSection(search);
  return available.includes(requested) ? requested : available[0] || 'accountSettings';
}

export function portalSectionLocation(section, search = '', pathname = '/', preserveProfile = false) {
  const params = new URLSearchParams(search);
  params.set('section', section);
  if (!preserveProfile || section !== 'accountSettings') params.delete('profile');
  return `${pathname || '/'}?${params.toString()}`;
}

export function portalLoginLocation(pathname = '/', search = '') {
  const safePath = typeof pathname === 'string' && pathname.startsWith('/') && !pathname.startsWith('//') && !/[\r\n]/.test(pathname) ? pathname : '/';
  const safeSearch = typeof search === 'string' && search.startsWith('?') && !/[\r\n]/.test(search) ? search : '';
  return `/auth/login?return_to=${encodeURIComponent(safePath + safeSearch)}`;
}
