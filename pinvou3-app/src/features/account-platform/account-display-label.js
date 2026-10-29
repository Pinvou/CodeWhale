/**
 * @param {{ username?: unknown, display_name?: unknown } | null | undefined} account
 * @param {string} [fallback]
 */
export function accountDisplayLabel(account, fallback = '') {
  const username = String(account?.username || '').trim();
  if (username) return username;
  const displayName = String(account?.display_name || '').trim();
  return displayName || fallback;
}

export function maskedAccountMobile(value) {
  const text = String(value || '').trim();
  if (!text) return '—';
  if (text.length <= 7) return '••••';
  return `${text.slice(0, -8)}••••${text.slice(-4)}`;
}
