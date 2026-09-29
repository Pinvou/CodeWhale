const VERSION_KEYS = ['context_version', 'personal_authorization_version', 'platform_authorization_version', 'organization_authorization_version'];

export const organizationPortalReadPermissions = Object.freeze([
  'org.profile.read',
  'org.wallet.read',
  'org.quota.read.self',
  'org.quota.read.all',
  'org.units.read',
  'org.members.read',
  'org.models.read',
]);

export const adminSectionPermissions = Object.freeze({
  overview: ['platform.users.read', 'platform.organizations.read', 'platform.billing.wallets.read', 'platform.tokens.read', 'platform.usage.read', 'platform.system.status.read'],
  organizations: ['platform.organizations.read'],
  users: ['platform.users.read', 'platform.registrations.repair', 'platform.profile-changes.repair', 'platform.account-closures.read'],
  roles: ['platform.roles.read'],
  billing: ['platform.billing.wallets.read', 'platform.usage.read'],
  payments: ['platform.billing.wallets.read', 'platform.usage.read'],
  products: ['platform.products.read', 'platform.pricing.read'],
  tokens: ['platform.tokens.read', 'platform.models.read', 'platform.usage.read'],
  models: ['platform.models.read', 'platform.pricing.read'],
  tokenResources: ['platform.tokens.read'],
  modelOperations: ['platform.tokens.read', 'platform.usage.read'],
  approvals: ['platform.billing.wallets.read', 'platform.billing.repairs.request', 'platform.billing.repairs.approve', 'platform.billing.payment-controls.resume.request', 'platform.billing.payment-controls.resume.approve',
    'platform.models.lifecycle.request', 'platform.models.lifecycle.approve', 'platform.models.catalog.rollback.request', 'platform.models.catalog.rollback.approve',
    'platform.tokens.credentials.revoke.request', 'platform.tokens.credentials.revoke.approve'],
  system: ['platform.system.status.read', 'platform.users.identity.repair'],
  audit: ['platform.audit.read'],
  operations: ['platform.registration-bonus.read'],
  settings: ['platform.sms-config.read'],
});

export const adminSectionMutationPermissions = Object.freeze({
  overview: [],
  organizations: ['platform.organizations.create', 'platform.organizations.profile.manage', 'platform.organizations.status.manage', 'platform.organizations.members.manage', 'platform.organizations.units.manage', 'platform.organizations.delete', 'platform.organizations.owner.transfer'],
  users: ['platform.users.status.manage', 'platform.registrations.repair', 'platform.profile-changes.repair', 'platform.account-closures.repair'],
  roles: ['platform.roles.assign'],
  billing: ['platform.billing.repairs.request', 'platform.billing.holds.release'],
  payments: ['platform.billing.orders.sync', 'platform.billing.repairs.request', 'platform.billing.payment-controls.resume.request', 'platform.usage.reconcile.retry'],
  products: ['platform.products.create', 'platform.products.publish', 'platform.products.unpublish', 'platform.products.delete', 'platform.pricing.manage'],
  tokens: ['platform.tokens.manage', 'platform.tokens.credentials.validate', 'platform.models.sync', 'platform.models.manage', 'platform.usage.reconcile.retry'],
  models: ['platform.models.sync', 'platform.models.manage', 'platform.pricing.manage'],
  tokenResources: ['platform.tokens.manage', 'platform.tokens.credentials.validate'],
  modelOperations: ['platform.usage.reconcile.retry'],
  approvals: ['platform.billing.repairs.request', 'platform.billing.repairs.approve', 'platform.billing.payment-controls.resume.request', 'platform.billing.payment-controls.resume.approve',
    'platform.models.lifecycle.request', 'platform.models.lifecycle.approve', 'platform.models.catalog.rollback.request', 'platform.models.catalog.rollback.approve',
    'platform.tokens.credentials.revoke.request', 'platform.tokens.credentials.revoke.approve'],
  system: ['platform.users.identity.repair'],
  audit: ['platform.audit.export'],
  operations: ['platform.registration-bonus.manage'],
  settings: ['platform.sms-config.manage'],
});

export function permissionSet(snapshot, scope = 'active') {
  if (scope === 'personal') {
    const personal = snapshot?.personal_permissions;
    if (Array.isArray(personal)) return new Set(personal);
    if (!snapshot?.active_context?.organization_id) return new Set(snapshot?.active_context?.permissions || []);
    return new Set();
  }
  if (scope === 'platform') return new Set(snapshot?.platform_permissions || []);
  return new Set(snapshot?.active_context?.permissions || []);
}

export function hasPermission(snapshot, permission) {
  const scope = permission.startsWith('personal.') ? 'personal' : permission.startsWith('platform.') ? 'platform' : 'active';
  return permissionSet(snapshot, scope).has(permission);
}

export function hasAnyPermission(snapshot, permissions = []) {
  return permissions.some((permission) => hasPermission(snapshot, permission));
}

export function canAccessOrganizationPortal(snapshot) {
  return Boolean(snapshot?.active_context?.organization_id) && hasAnyPermission(snapshot, organizationPortalReadPermissions);
}

export function authorizationVersions(snapshot) {
  const active = snapshot?.active_context || {};
  return {
    context_version: Number(active.context_version || 0),
    personal_authorization_version: Number(active.personal_authorization_version || 0),
    platform_authorization_version: Number(active.platform_authorization_version || 0),
    organization_authorization_version: Number(active.organization_authorization_version || 0),
    organization_id: String(active.organization_id || ''),
  };
}

export function authorizationVersionChanged(current, next) {
  if (!current || !next) return true;
  if (String(current.organization_id || '') !== String(next.organization_id || '')) return true;
  return VERSION_KEYS.some((key) => Number(current[key] || 0) !== Number(next[key] || 0));
}

export function mergeObservedVersions(current = {}, observed = {}) {
  const next = { ...current };
  for (const key of VERSION_KEYS) {
    const value = Number(observed[key] || 0);
    if (Number.isSafeInteger(value) && value > Number(next[key] || 0)) next[key] = value;
  }
  return next;
}

export function adminVisibleSections(snapshot, { modelTokenIAEnabled = false } = {}) {
  const visible = Object.keys(adminSectionPermissions).filter((section) => hasAnyPermission(snapshot, adminSectionPermissions[section]));
  const hidden = modelTokenIAEnabled ? new Set(['tokens']) : new Set(['models', 'tokenResources', 'modelOperations']);
  const filtered = visible.filter((section) => !hidden.has(section));
  if (filtered.length > 0 && !filtered.includes('settings')) filtered.push('settings');
  return filtered;
}

export function adminSectionReadOnly(snapshot, section) {
  return !hasAnyPermission(snapshot, adminSectionMutationPermissions[section] || []);
}

export function personalSurfaceAccess(snapshot) {
  const organizationContext = Boolean(snapshot?.active_context?.organization_id);
  const active = permissionSet(snapshot);
  return {
    profileRead: hasPermission(snapshot, 'personal.profile.read'),
    profileUpdate: hasPermission(snapshot, 'personal.profile.update'),
    contextSwitch: hasPermission(snapshot, 'personal.context.switch'),
    closureRead: hasPermission(snapshot, 'personal.account.close.read'),
    closureRequest: hasPermission(snapshot, 'personal.account.close.request'),
    invitationsRead: hasPermission(snapshot, 'personal.invitations.read'),
    invitationsAccept: hasPermission(snapshot, 'personal.invitations.accept'),
    billingRead: organizationContext ? active.has('org.wallet.read') : hasPermission(snapshot, 'personal.billing.read'),
    rechargeCreate: organizationContext ? active.has('org.entitlements.purchase') : hasPermission(snapshot, 'personal.recharge.create'),
    usageRead: organizationContext ? active.has('org.usage.read.self') || active.has('org.usage.read.all') : hasPermission(snapshot, 'personal.usage.read'),
    usageCancel: organizationContext ? false : hasPermission(snapshot, 'personal.usage.cancel'),
  };
}
