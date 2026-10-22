const VERSION_KEYS = ['context_version', 'personal_authorization_version', 'platform_authorization_version', 'organization_authorization_version'];
export function mergeObservedVersions(current = {}, observed = {}) {
  const next = { ...current };
  const context = Number(observed.context_version || 0);
  const previousContext = Number(current.context_version || 0);
  const scopedContext = Number.isSafeInteger(context) && context > 0;
  if (scopedContext) {
    // Organization counters belong to one active context, not to the user.
    // A late response cannot combine its old organization with the new one.
    if (context < previousContext) return next;
    if (context > previousContext) delete next.organization_authorization_version;
  }
  for (const key of VERSION_KEYS) {
    if (key === 'organization_authorization_version' && !scopedContext) continue;
    const value = Number(observed[key] || 0);
    if (Number.isSafeInteger(value) && value > Number(next[key] || 0)) next[key] = value;
  }
  return next;
}

export function hasPermission(snapshot, permission) {
 const values = permission.startsWith('platform.') ? snapshot?.platform_permissions : permission.startsWith('org.') ? snapshot?.active_context?.permissions || snapshot?.permissions : snapshot?.personal_permissions;
 return Array.isArray(values) && values.includes(permission);
}
