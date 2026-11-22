import { isLocalGatewayUrl } from '../model-store/pinvou-model-link.js';
import { modelGuidancePresentation } from './model-guidance-connection.js';

const REMINDER_PHASES = new Set(['recover', 'unknown', 'connectionUnavailable', 'connectionUnknown', 'unlinked', 'ready']);

export function guidanceReminderCanMute(phase) { return REMINDER_PHASES.has(phase); }

/** Profile/socket namespace plus saved model identity. No address or credential is persisted. */
export function guidanceMuteKey(scope, model) {
  if (!/^pinvou\.megacube\.guidance\.v1\.[a-f0-9]{64}$/.test(scope || '')) return null;
  try {
    const target = model && isLocalGatewayUrl(model.base_url) && typeof model.id === 'string' && model.id.length > 0 && model.id.length <= 512
      ? `model.${encodeURIComponent(model.id)}` : 'local';
    return `${scope}.mutedUntilAvailable.${target}`;
  } catch { return null; }
}

export function readGuidanceMute(storage, key) {
  if (!key) return { known: false, muted: false };
  try {
    const value = storage.getItem(key);
    if (value == null) return { known: true, muted: false };
    return /^\d{1,16}$/.test(value) && Number(value) > 0
      ? { known: true, muted: true } : { known: false, muted: false };
  } catch { return { known: false, muted: false }; }
}

export function muteGuidanceUntilAvailable(storage, key, now = Date.now()) {
  if (!key || !Number.isSafeInteger(now) || now <= 0) return false;
  try {
    if (!readGuidanceMute(storage, key).muted) storage.setItem(key, String(now));
    return readGuidanceMute(storage, key).muted;
  } catch { return false; }
}

export function clearGuidanceMute(storage, key) {
  if (!key) return false;
  try {
    if (storage.getItem(key) !== null) storage.removeItem(key);
    return true;
  } catch { return false; }
}

/** Reset only on confirmed usable chat configuration, not an unrelated running service. */
export function reconcileGuidanceMute(storage, scope, model, snapshot, savedModels = []) {
  const key = guidanceMuteKey(scope, model);
  const available = modelGuidancePresentation(snapshot, model, savedModels).phase === 'available';
  if (available) {
    for (const target of new Set([key, guidanceMuteKey(scope, null)])) clearGuidanceMute(storage, target);
  }
  return { key, ...readGuidanceMute(storage, key), available };
}
