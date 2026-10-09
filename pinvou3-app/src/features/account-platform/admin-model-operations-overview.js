

export function validateCatalogDifference(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const keys = ['baseline_catalog_version', 'target_catalog_version', 'target_observed_version', 'added_count', 'modified_count', 'removed_count', 'total_count', 'truncated', 'items'];
  if (Object.keys(value).length !== keys.length || keys.some((key) => !Object.prototype.hasOwnProperty.call(value, key))) return null;
  const providerObservation = value.target_observed_version !== null;
  if (!Number.isSafeInteger(value.baseline_catalog_version) || value.baseline_catalog_version < 0
    || providerObservation === (value.target_catalog_version !== null)
    || (value.target_catalog_version !== null && (!Number.isSafeInteger(value.target_catalog_version) || value.target_catalog_version < 1))
    || (providerObservation && !/^[0-9a-f]{64}$/.test(value.target_observed_version))) return null;
  if (![value.added_count, value.modified_count, value.removed_count, value.total_count].every((item) => Number.isSafeInteger(item) && item >= 0) || value.total_count !== value.added_count + value.modified_count + value.removed_count || typeof value.truncated !== 'boolean' || !Array.isArray(value.items) || value.items.length > 20 || value.truncated !== (value.total_count > value.items.length)) return null;
  const visibleCounts = { ADDED: 0, MODIFIED: 0, REMOVED: 0 };
  const seenIDs = new Set();
  for (const item of value.items) {
    if (!item || Object.keys(item).sort().join(',') !== 'change_type,display_name,model_id' || !['ADDED', 'MODIFIED', 'REMOVED'].includes(item.change_type) || typeof item.display_name !== 'string' || item.display_name.length > 160 || (item.model_id !== null && !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(item.model_id))) return null;
    if (providerObservation ? item.change_type !== 'ADDED' && item.model_id === null : item.model_id === null) return null;
    if (item.model_id !== null && seenIDs.has(item.model_id.toLowerCase())) return null;
    if (item.model_id !== null) seenIDs.add(item.model_id.toLowerCase());
    visibleCounts[item.change_type] += 1;
  }
  if (visibleCounts.ADDED > value.added_count || visibleCounts.MODIFIED > value.modified_count || visibleCounts.REMOVED > value.removed_count) return null;
  if (!value.truncated && (visibleCounts.ADDED !== value.added_count || visibleCounts.MODIFIED !== value.modified_count || visibleCounts.REMOVED !== value.removed_count)) return null;
  return value;
}
