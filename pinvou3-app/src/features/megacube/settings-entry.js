/**
 * Entry visibility is a setup/recovery affordance, never device identity or
 * permission. Only confirmed unsupported capabilities may hide that entry.
 * @param {Array<PromiseSettledResult<Record<string, any>>>} results
 * @returns {'available'|'unsupported'|'unknown'}
 */
export function cmsSettingsEntryState(results) {
  if (results.some(result => result.status === 'fulfilled'
    && [result.value?.available, result.value?.detected, result.value?.supported].includes(true))) {
    return 'available';
  }
  /** @param {PromiseSettledResult<Record<string, any>>} result */
  const unsupported = result => result.status === 'fulfilled' && (
    result.value?.supported === false
    || (result.value?.available === false && result.value?.reason === 'unsupported_platform')
  );
  return results.length === 2 && results.every(unsupported) ? 'unsupported' : 'unknown';
}
