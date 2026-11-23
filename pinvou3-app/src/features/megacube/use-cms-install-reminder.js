import { useCallback, useEffect, useState, useSyncExternalStore } from 'react';
import { CMS_INSTALL_REMINDER_CHANGED, cmsInstallReminderKey, readCmsInstallReminder, reconcileCmsInstallReminder, setCmsInstallReminder } from './cms-install-reminder.js';

function storage() { try { return globalThis.localStorage; } catch { return undefined; } }
function notify(key) { globalThis.dispatchEvent?.(new CustomEvent(CMS_INSTALL_REMINDER_CHANGED, { detail: { key } })); }
function subscribe(listener) {
  globalThis.addEventListener?.('storage', listener);
  globalThis.addEventListener?.(CMS_INSTALL_REMINDER_CHANGED, listener);
  return () => {
    globalThis.removeEventListener?.('storage', listener);
    globalThis.removeEventListener?.(CMS_INSTALL_REMINDER_CHANGED, listener);
  };
}

/** Keep homepage and device management in sync, including other windows. */
export function useCmsInstallReminder(service) {
  const key = cmsInstallReminderKey(service);
  const [failedKey, setFailedKey] = useState(null);
  const read = useCallback(() => {
    const value = readCmsInstallReminder(storage(), service);
    return value.muted ? 'muted' : value.known ? 'shown' : 'unknown';
  }, [service]);
  const preference = useSyncExternalStore(subscribe, read, () => 'unknown');
  useEffect(() => {
    const before = readCmsInstallReminder(storage(), service).muted;
    const after = reconcileCmsInstallReminder(storage(), service);
    if (before !== after.muted) notify(key);
    if (after.installed) setFailedKey(null);
  }, [service]);
  const change = muted => {
    const saved = setCmsInstallReminder(storage(), service, muted);
    setFailedKey(saved ? null : key || 'unavailable');
    if (saved) notify(key);
    return saved;
  };
  return { muted: preference === 'muted', failed: failedKey === (key || 'unavailable'), change };
}
