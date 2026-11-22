import { guidanceStorageKey } from './model-guidance-state.js';
import { clearGuidanceMute, readGuidanceMute } from './model-guidance-mute.js';

export const CMS_INSTALL_REMINDER_CHANGED = 'pinvou:cms-install-reminder';

/** This preference belongs to a device connection, never to a chat model. */
export function cmsInstallReminderKey(service) {
  const scope = guidanceStorageKey(service?.recoveryScope);
  return scope ? `${scope}.cmsInstallDismissed` : null;
}

export function readCmsInstallReminder(storage, service) {
  return readGuidanceMute(storage, cmsInstallReminderKey(service));
}

export function setCmsInstallReminder(storage, service, muted, now = Date.now()) {
  const key = cmsInstallReminderKey(service);
  if (!key) return false;
  try {
    if (muted) {
      if (!Number.isSafeInteger(now) || now <= 0) return false;
      if (!readGuidanceMute(storage, key).muted) storage.setItem(key, String(now));
    } else if (!clearGuidanceMute(storage, key)) return false;
    return readGuidanceMute(storage, key).muted === muted && readGuidanceMute(storage, key).known;
  } catch { return false; }
}

/** Offline, unverified bindings and healthy model endpoints are not installation evidence. */
export function reconcileCmsInstallReminder(storage, service) {
  const installed = service?.supported === true && service?.scope === 'user' && service?.installed === true;
  if (installed) clearGuidanceMute(storage, cmsInstallReminderKey(service));
  return { ...readCmsInstallReminder(storage, service), installed };
}
