export const adminNavigationGroups = Object.freeze([
  { key: 'workbench', sections: ['overview'] },
  { key: 'identity', sections: ['users', 'roles', 'organizations', 'myorg'] },
  { key: 'finance', sections: ['billing', 'payments', 'products'] },
  { key: 'models', sections: ['tokens'] },
  { key: 'governance', sections: ['approvals', 'audit', 'system', 'settings'] },
]);

export function readAdminSection(locationLike = globalThis.location) {
  if (!locationLike) return '';
  return new URLSearchParams(String(locationLike.search || '')).get('section') || '';
}

export function adminSectionURL(section, locationLike = globalThis.location, queryUpdates = {}) {
  const pathname = String(locationLike?.pathname || '/admin.html');
  const search = new URLSearchParams(String(locationLike?.search || ''));
  search.set('section', section);
  for (const [key, value] of Object.entries(queryUpdates || {})) {
    if (value === null || value === undefined || value === '') search.delete(key);
    else search.set(key, String(value));
  }
  const query = search.toString();
  return `${pathname}${query ? `?${query}` : ''}${String(locationLike?.hash || '')}`;
}

export function writeAdminSection(section, mode = 'push', historyLike = globalThis.history, locationLike = globalThis.location, queryUpdates = {}) {
  if (!historyLike || !section) return;
  const url = adminSectionURL(section, locationLike, queryUpdates);
  if (mode === 'replace') historyLike.replaceState({ section }, '', url);
  else historyLike.pushState({ section }, '', url);
}
