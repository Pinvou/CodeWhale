const VERSION_KEYS = ['context_version', 'personal_authorization_version', 'platform_authorization_version'];
export function mergeObservedVersions(current = {}, observed = {}) {
  const next = { ...current };
  for (const key of VERSION_KEYS) {
    const value = Number(observed[key] || 0);
    if (Number.isSafeInteger(value) && value > Number(next[key] || 0)) next[key] = value;
  }
  return next;
}

export function hasPermission(snapshot, permission) {
 const values = permission.startsWith('platform.') ? snapshot?.platform_permissions : snapshot?.personal_permissions;
 return Array.isArray(values) && values.includes(permission);
}
