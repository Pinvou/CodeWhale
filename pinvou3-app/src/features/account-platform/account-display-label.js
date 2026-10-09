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
