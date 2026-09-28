import { localizedProductDisplayName } from './localized-product-display-name.js';

const visibleStatuses = new Set(['ACTIVE', 'CONSUMED']);

function expiryTime(item) {
  const value = new Date(item?.valid_until || item?.expires_at || '').getTime();
  return Number.isFinite(value) ? value : Number.POSITIVE_INFINITY;
}

export function activeEntitlementPackages(items = [], now = Date.now()) {
  return items
    .filter((item) => {
      const expiresAt = expiryTime(item);
      return visibleStatuses.has(item?.status) && Number.isFinite(expiresAt) && expiresAt > now;
    })
    .sort((left, right) => expiryTime(left) - expiryTime(right) || String(left?.package_id || '').localeCompare(String(right?.package_id || '')));
}

export function allEntitlementPackages(items = []) {
  return [...items].sort((left, right) => expiryTime(left) - expiryTime(right) || String(left?.package_id || '').localeCompare(String(right?.package_id || '')));
}

export function entitlementProgress(item) {
  const total = Number(item?.points_total_microcredits);
  const used = Number(item?.points_used_microcredits);
  if (!Number.isFinite(total) || total <= 0 || !Number.isFinite(used)) return 0;
  return Math.min(100, Math.max(0, (used / total) * 100));
}

export function remainingEntitlementMicrocredits(items = []) {
  return items.reduce((sum, item) => {
    const value = Number(item?.points_remaining_microcredits);
    return sum + (Number.isFinite(value) && value > 0 ? value : 0);
  }, 0);
}

export function entitlementPackageDisplayName(item, language) {
  return localizedProductDisplayName(item, language, 'plan_id');
}
