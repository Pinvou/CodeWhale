const VERSION_KEYS = ['context_version', 'personal_authorization_version', 'platform_authorization_version', 'organization_authorization_version'];

export const adminSectionPermissions = Object.freeze({
  overview: ['platform.system.status.read'],
  organizations: ['platform.organizations.read'],
  users: ['platform.users.read'],
  billing: ['platform.billing.wallets.read', 'platform.pricing.read', 'platform.usage.read'],
  tokens: ['platform.tokens.read', 'platform.models.read', 'platform.usage.read'],
  system: ['platform.system.status.read'],
  audit: ['platform.audit.read'],
  identity: ['platform.account-closures.read'],
});

export const adminSectionMutationPermissions = Object.freeze({
  overview: [],
  organizations: ['platform.organizations.create', 'platform.organizations.status.manage', 'platform.organizations.delete', 'platform.organizations.owner.transfer'],
  users: ['platform.users.status.manage'],
  billing: ['platform.billing.orders.sync', 'platform.billing.repairs.request', 'platform.billing.adjustments.approve', 'platform.billing.holds.release', 'platform.pricing.manage', 'platform.usage.reconcile.retry'],
  tokens: ['platform.tokens.manage', 'platform.tokens.credentials.validate', 'platform.models.sync', 'platform.models.manage', 'platform.usage.reconcile.retry'],
  system: [],
  audit: ['platform.audit.export'],
  identity: ['platform.account-closures.repair'],
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

export function adminVisibleSections(snapshot) {
  return Object.keys(adminSectionPermissions).filter((section) => hasAnyPermission(snapshot, adminSectionPermissions[section]));
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
    billingRead: organizationContext ? active.has('org.wallet.read') || active.has('org.quota.read.self') || active.has('org.quota.read.all') : hasPermission(snapshot, 'personal.billing.read'),
    rechargeCreate: organizationContext ? active.has('org.entitlements.purchase') : hasPermission(snapshot, 'personal.recharge.create'),
    usageRead: organizationContext ? active.has('org.usage.read.self') || active.has('org.usage.read.all') : hasPermission(snapshot, 'personal.usage.read'),
    usageCancel: organizationContext ? false : hasPermission(snapshot, 'personal.usage.cancel'),
  };
}
