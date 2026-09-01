export const roleScopes = Object.freeze(['PERSONAL', 'PLATFORM', 'ORGANIZATION', 'UNIT']);
export const roleCatalogScopes = Object.freeze(['PERSONAL', 'PLATFORM', 'ORGANIZATION']);

const roleCatalogSortOrder = Object.freeze({ ORGANIZATION: 0, PERSONAL: 1, PLATFORM: 2 });

const protectedRoleCodes = new Set(['PERSONAL_USER', 'MEMBER', 'OWNER', 'PLATFORM_SUPER_ADMIN']);

function normalized(value) {
  return String(value || '').trim().toLowerCase();
}

export function roleName(copy, code) {
  return copy.roles.names?.[code] || code;
}

export function roleDescription(copy, role) {
  return copy.roles.descriptions?.[role?.code] || role?.description || '';
}

export function roleScopeLabel(copy, scope) {
  return copy.roles.scopes?.[scope] || scope;
}

export function roleCatalogScope(scope) {
  // Catalog grouping is presentation-only. Grants and authorization decisions keep UNIT as their real scope.
  return scope === 'UNIT' ? 'ORGANIZATION' : scope;
}

export function roleGrantMode(copy, role) {
  if (role?.code === 'OWNER') return copy.roles.ownershipFlow;
  if (role?.code === 'PLATFORM_SUPER_ADMIN' || role?.code === 'PLATFORM_AUDIT_PII_EXPORT') return copy.roles.secureFlow;
  if (!role?.assignable || role.scope === 'PERSONAL' || role.code === 'MEMBER') return copy.roles.lifecycleManaged;
  return copy.roles.directGrant;
}

export function canGrantRole(role) {
  return Boolean(role?.assignable) && role.scope !== 'PERSONAL' && !protectedRoleCodes.has(role.code);
}

export function canRevokeBinding(binding, roleByCode) {
  const role = roleByCode.get(binding?.role_code);
  return ['ACTIVE', 'SCHEDULED'].includes(binding?.status) && canGrantRole(role);
}

function permissionDomain(code) {
  const value = normalized(code);
  if (/billing|payment|pricing|product|wallet|usage|quota/.test(value)) return 'finance';
  if (/token|model|credential|route|provision/.test(value)) return 'models';
  if (/audit|system|event|approval|security/.test(value)) return 'governance';
  return 'identity';
}

export function permissionLabel(copy, code) {
  const parts = String(code || '').split('.').filter(Boolean);
  const action = parts.at(-1) || '';
  const resource = parts.slice(1, -1).map((part) => copy.roles.permissionTokens?.[part] || part.replaceAll('-', ' ')).join(' / ');
  const actionLabel = copy.roles.permissionActions?.[action] || action;
  return resource ? `${actionLabel} · ${resource}` : actionLabel || code;
}

export function permissionGroups(copy, role) {
  const groups = new Map();
  for (const code of role?.permissions || []) {
    const domain = permissionDomain(code);
    const items = groups.get(domain) || [];
    items.push({ code, label: permissionLabel(copy, code) });
    groups.set(domain, items);
  }
  return ['identity', 'finance', 'models', 'governance']
    .filter((domain) => groups.has(domain))
    .map((domain) => ({ domain, label: copy.navGroups[domain], items: groups.get(domain).sort((left, right) => left.code.localeCompare(right.code)) }));
}

export function filterRoles(roles, filters, copy) {
  const query = normalized(filters?.query);
  const matched = (roles || []).filter((role) => {
    if (filters?.scope && roleCatalogScope(role.scope) !== filters.scope) return false;
    const direct = roleGrantMode(copy, role) === copy.roles.directGrant;
    if (filters?.grantable === 'direct' && !direct) return false;
    if (filters?.grantable === 'managed' && direct) return false;
    if (!query) return true;
    return [role.code, role.description, roleDescription(copy, role), roleName(copy, role.code), ...(role.permissions || [])].some((value) => normalized(value).includes(query));
  });
  return [...matched].sort((left, right) => {
    const scopeOrder = roleCatalogSortOrder[roleCatalogScope(left.scope)] - roleCatalogSortOrder[roleCatalogScope(right.scope)];
    return scopeOrder || left.code.localeCompare(right.code);
  });
}

export function filterBindings(bindings, filters, roleByCode, userByID, organizationByID, copy) {
  const query = normalized(filters?.query);
  return (bindings || []).filter((binding) => {
    if (filters?.scope && binding.scope !== filters.scope) return false;
    if (filters?.status && binding.status !== filters.status) return false;
    const role = roleByCode.get(binding.role_code);
    const user = userByID.get(binding.user_id);
    const organization = organizationByID.get(binding.organization_id);
    if (!query) return true;
    return [
      binding.user_id,
      binding.role_code,
      roleName(copy, binding.role_code),
      user?.username,
      user?.display_name,
      user?.user_code,
      organization?.name,
    ].some((value) => normalized(value).includes(query));
  });
}

export function summarizeRoleScopes(roles) {
  const counts = Object.fromEntries(roleCatalogScopes.map((scope) => [scope, 0]));
  for (const role of roles || []) {
    const scope = roleCatalogScope(role.scope);
    if (scope in counts) counts[scope] += 1;
  }
  return counts;
}
