const TEAM_RELAY_ACCESS_STORAGE_KEY = 'pinvou.team-relay.beta-access';
const TEAM_RELAY_ACCESS_GRANTED_VALUE = 'enabled-v1';

function resolveStorage(storage) {
  if (storage) return storage;
  try {
    return globalThis.localStorage || null;
  } catch {
    return null;
  }
}

export function loadTeamRelayAccess(storage) {
  const target = resolveStorage(storage);
  if (!target) return false;
  try {
    return target.getItem(TEAM_RELAY_ACCESS_STORAGE_KEY) === TEAM_RELAY_ACCESS_GRANTED_VALUE;
  } catch {
    return false;
  }
}

export async function unlockTeamRelayAccess(accessCode, verifyAccess, storage) {
  if (typeof verifyAccess !== 'function') throw new Error('team_relay_access_verifier_unavailable');
  if (!await verifyAccess(String(accessCode || '').trim())) return false;

  const target = resolveStorage(storage);
  if (!target) throw new Error('team_relay_access_storage_unavailable');
  target.setItem(TEAM_RELAY_ACCESS_STORAGE_KEY, TEAM_RELAY_ACCESS_GRANTED_VALUE);
  return true;
}

export function disableTeamRelayAccess(storage) {
  const target = resolveStorage(storage);
  if (!target) throw new Error('team_relay_access_storage_unavailable');
  target.removeItem(TEAM_RELAY_ACCESS_STORAGE_KEY);
}
