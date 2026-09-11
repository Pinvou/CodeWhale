import { modelOperationsWindow, modelTokenPageOverviewPermissionView } from './admin-model-operations-overview.js';

export function planAdminModelTokenPageOverview(section, permissions = [], enabled = false, windowValue = '24h') {
  if (!enabled) return null;
  const granted = new Set(permissions);
  if (section === 'models') return granted.has('platform.models.read') ? { section, permissionView: 'MODELS', window: '', load: (client, signal) => client.tokenModelCatalogOverview(signal) } : null;
  if (section === 'tokenResources') return granted.has('platform.tokens.read') ? { section, permissionView: 'TOKENS', window: '', load: (client, signal) => client.tokenResourcesPageOverview(signal) } : null;
  if (section !== 'modelOperations') return null;
  const permissionView = modelTokenPageOverviewPermissionView(permissions);
  if (!permissionView) return null;
  const usageAllowed = permissionView !== 'TOKEN_ONLY';
  const window = usageAllowed ? modelOperationsWindow(windowValue) : '';
  return { section, permissionView, window, load: (client, signal) => client.tokenModelOperationsPageOverview(window, signal) };
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
