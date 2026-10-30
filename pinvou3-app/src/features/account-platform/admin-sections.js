import { approvalCenterVisible } from './admin-approval-actions.js';
import { personalPortalSections } from './portal-sections.js';

export const adminSectionGroups = Object.freeze([
  { key: 'workbench', sections: ['overview'] },
  { key: 'identity', sections: ['users', 'roles', 'organizations', 'myorg'] },
  { key: 'finance', sections: ['billing', 'payments', 'products'] },
  { key: 'models', sections: ['tokens', 'models', 'tokenResources', 'modelOperations'] },
  { key: 'governance', sections: ['operations', 'approvals', 'audit', 'system', 'settings'] },
]);
export function visibleAdminSections(me, flags = {}) {
  const granted = new Set(me?.platform_permissions || []), can = (...values) => values.some(value => granted.has(value));
  const finance = can('platform.billing.wallets.read', 'platform.billing.repairs.request');
  const models = can('platform.models.read', 'platform.pricing.read');
  const tokens = can('platform.tokens.read');
  const operations = can('platform.usage.read', 'platform.tokens.read');
  const visible = {
    overview: can('platform.users.read', 'platform.organizations.read', 'platform.billing.wallets.read', 'platform.models.read', 'platform.tokens.read', 'platform.usage.read', 'platform.system.status.read'),
    users: can('platform.users.read'), roles: can('platform.roles.read'), organizations: can('platform.organizations.read'),
    myorg: personalPortalSections(me).includes('myOrganization'),
    billing: finance, payments: can('platform.billing.wallets.read', 'platform.billing.reconciliation.read'),
    products: can('platform.products.read', 'platform.pricing.read'),
    tokens: flags.admin_model_token_ia !== true && (models || tokens || operations || can('platform.models.lifecycle.request', 'platform.models.lifecycle.approve', 'platform.models.catalog.rollback.request', 'platform.models.catalog.rollback.approve', 'platform.tokens.credentials.revoke.request', 'platform.tokens.credentials.revoke.approve')),
    models: flags.admin_model_token_ia === true && models,
    tokenResources: flags.admin_model_token_ia === true && tokens,
    modelOperations: flags.admin_model_token_ia === true && operations,
    operations: can('platform.registration-bonus.read', 'platform.account-closures.read', 'platform.registrations.repair', 'platform.profile-changes.repair'),
    approvals: approvalCenterVisible([...granted]), audit: can('platform.audit.read', 'platform.audit.export'),
    system: can('platform.system.status.read'), settings: can('platform.sms-config.read'),
  };
  return adminSectionGroups.flatMap(group => group.sections).filter(key => visible[key]);
}
export function resolveAdminRoute(search, flags = {}, available) {
  const params = new URLSearchParams(search), requested = params.get('section') || 'overview';
  let section = requested, tab = params.get('finance_tab') || '';
  if (params.has('approval_view')) section = 'approvals';
  else if (requested === 'finance') section = tab === 'approvals' ? 'approvals' : ['orders', 'controls', 'late', 'lateReviews', 'anomalies'].includes(tab) ? 'payments' : 'billing';
  else if (requested === 'reconciliation') { section = 'payments'; tab = 'reconciliations'; }
  else if (requested === 'entitlement-correction') { section = 'billing'; tab = 'correction'; }
  else if (['registration', 'profile', 'closures'].includes(requested)) section = 'operations';
  else if (requested === 'management' || requested === 'tokens' && flags.admin_model_token_ia === true) {
    section = flags.admin_model_token_ia === true ? params.has('usage_request_id') || params.has('operations_tab') ? 'modelOperations' : params.has('token_account_id') ? 'tokenResources' : 'models' : 'tokens';
    if (params.has('approval_request_id')) section = 'approvals';
  }
  if (tab === 'lateReviews') tab = 'late';
  if (flags.admin_model_token_ia === true && ['tokens', 'management'].includes(requested) && !params.has('model_id') && !params.has('token_account_id') && !params.has('usage_request_id') && !params.has('operations_tab') && !params.has('approval_request_id') && available && !available.includes(section)) {
    section = ['models', 'tokenResources', 'modelOperations'].find(key => available.includes(key)) || section;
  }
  return { section, tab, operationsTab: ['registration', 'profile', 'closures'].includes(requested) ? requested : params.get('operations_page') || 'policy' };
}
export function adminRouteLocation(search, section, updates = {}, pathname = '/admin.html') {
  const params = new URLSearchParams(search);
  const previousSection = params.get('section');
  params.set('section', section);
  // Canonicalizing an existing operations alias must keep its selected tab.
  if (section === 'operations' && ['registration', 'profile', 'closures'].includes(previousSection) && !Object.prototype.hasOwnProperty.call(updates, 'operations_page')) params.set('operations_page', previousSection);
  if (section !== previousSection && ['billing', 'payments', 'products'].includes(section) && !Object.prototype.hasOwnProperty.call(updates, 'finance_tab')) params.set('finance_tab', { billing: 'wallets', payments: 'orders', products: 'catalog' }[section]);
  // Detail filters remain in the URL, but a stale approval selector must not win
  // over an explicit navigation to another page.
  if (section !== 'approvals') params.delete('approval_view');
  for (const [key, value] of Object.entries(updates)) {
    if (value === null || value === undefined || value === '') params.delete(key);
    else params.set(key, String(value));
  }
  return `${pathname}?${params}`;
}
