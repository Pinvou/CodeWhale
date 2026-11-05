export function planAdminModelTokenPageOverview(section, permissions = [], enabled = false) {
  if (!enabled) return null;
  const granted = new Set(permissions);
  if (section === 'models') return granted.has('platform.models.read') ? { section, permissionView: 'MODELS', window: '', load: (client, signal) => client.tokenModelCatalogOverview(signal) } : null;
  if (section === 'tokenResources') return granted.has('platform.tokens.read') ? { section, permissionView: 'TOKENS', window: '', load: (client, signal) => client.tokenResourcesPageOverview(signal) } : null;
  if (section !== 'modelOperations' || !granted.has('platform.tokens.read')) return null;
  const window = granted.has('platform.usage.read') ? '24h' : '';
  return { section, permissionView: window ? 'TOKEN_AND_USAGE' : 'TOKEN_ONLY', window, load: (client, signal) => client.tokenProvisioningPageOverview(signal, window) };
}
export function modelTokenPageOverviewCacheKey({ userID = '', authorizationVersion = 0, section = '', permissionView = '', window = '' } = {}) {
  const version = Number(authorizationVersion);
  if (!String(userID).trim() || !Number.isSafeInteger(version) || version < 1 || !section || !permissionView) return '';
  return JSON.stringify([String(userID), version, section, permissionView, String(window || '')]);
}

export function createModelTokenPageOverviewRequestTracker() {
  const generations = new Map();
  return {
    begin(key) {
      const generation = (generations.get(key) || 0) + 1;
      generations.set(key, generation);
      return generation;
    },
    isCurrent(key, generation) {
      return generations.get(key) === generation;
    },
    invalidate(key) {
      generations.set(key, (generations.get(key) || 0) + 1);
    },
    invalidateAll() {
      for (const key of generations.keys()) this.invalidate(key);
    },
  };
}
