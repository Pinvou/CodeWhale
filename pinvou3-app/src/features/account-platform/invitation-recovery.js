const storagePrefix = 'pinvou.account.invitation-accept-retry.v1:';
const maximumRecoveryInvitations = 20;
const canonicalUUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function invitationIDs(items) {
  return [...new Set((items || []).map((item) => String(item?.invitation_id ?? item ?? '').trim()).filter((value) => canonicalUUID.test(value)))].slice(0, maximumRecoveryInvitations);
}

export function invitationRecoveryScope(account) {
  const userId = String(account?.user_id || '').trim();
  return userId ? `${storagePrefix}${userId}` : '';
}

export function readInvitationAcceptanceRetries(account, storage) {
  const key = invitationRecoveryScope(account);
  try {
    const target = storage ?? globalThis.sessionStorage;
    if (!key || !target) return [];
    const values = JSON.parse(target.getItem(key) || '[]');
    if (!Array.isArray(values)) return [];
    return invitationIDs(values).map((invitationId) => ({ invitation_id: invitationId }));
  } catch {
    return [];
  }
}

export function writeInvitationAcceptanceRetries(account, items, storage) {
  const key = invitationRecoveryScope(account);
  const invitationIds = invitationIDs(items);
  try {
    const target = storage ?? globalThis.sessionStorage;
    if (!key || !target) return;
    if (invitationIds.length) target.setItem(key, JSON.stringify(invitationIds));
    else target.removeItem(key);
  } catch {
    // Hardened browser contexts may deny storage; the in-memory retry remains available.
  }
}

export function clearInvitationAcceptanceRetries(account, storage) {
  const key = invitationRecoveryScope(account);
  try {
    const target = storage ?? globalThis.sessionStorage;
    if (!key || !target) return;
    target.removeItem(key);
  } catch {
    // Logout still clears the in-memory reference when storage is unavailable.
  }
}

export function invitationAcceptanceMayBeCommitted(error) {
  return error?.code === 'ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE' || !Number.isFinite(error?.status) || error.status === 0 || error.status >= 500;
}
