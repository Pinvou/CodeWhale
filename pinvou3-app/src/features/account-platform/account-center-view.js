import { integerMoney, formatFen } from './finance-contract.js';

// Keep user-facing points exact across the int64 transport boundary.
export function pointsToMicrocredits(value) {
  if (typeof value !== 'string' || !/^(?:0|[1-9]\d*)(?:\.\d{1,4})?$/.test(value)) throw new Error('INVALID_ORGANIZATION_INPUT');
  const [whole, fraction = ''] = value.split('.');
  const amount = integerMoney((BigInt(whole) * 10000n + BigInt(fraction.padEnd(4, '0'))).toString());
  if (amount <= 0n) throw new Error('INVALID_ORGANIZATION_INPUT');
  return amount.toString();
}

export function productPointFacts(product) {
  try {
    const base = integerMoney(product.base_points), bonus = integerMoney(product.bonus_points ?? 0);
    if (base < 0n || bonus < 0n) return null;
    return { base, bonus, total: base + bonus };
  } catch {
    try { const total = integerMoney(product.display_points); return total >= 0n ? { total } : null; }
    catch { return null; }
  }
}

export function formatRMB(value, language) {
  return `¥${formatFen(value, language)}`;
}

export function packageAllocatedMicrocredits(item) {
  if (!Array.isArray(item.components)) return null;
  try {
    return item.components.reduce((sum, component) => sum + integerMoney(component.active_grant_microcredits) + integerMoney(component.suspended_grant_microcredits), 0n).toString();
  } catch { return null; }
}

export function quotaSources(packages, now = Date.now()) {
  const validExpiry = value => !value || Number.isFinite(Date.parse(value)) && Date.parse(value) > now;
  const started = value => !value || Number.isFinite(Date.parse(value)) && Date.parse(value) <= now;
  return packages.flatMap(item => item.status !== 'ACTIVE' || !started(item.valid_from) || !validExpiry(item.valid_until || item.expires_at) ? [] :
    (Array.isArray(item.components) ? item.components : []).filter(component => {
      try { return component.status === 'ACTIVE' && ['BASE', 'BONUS'].includes(component.component_type)
        && component.component_id && started(component.valid_from) && validExpiry(component.valid_until || component.expires_at) && integerMoney(component.available_microcredits) > 0n; }
      catch { return false; }
    })
      .map(component => ({ package: item, component })));
}
