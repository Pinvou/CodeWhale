import { cmsSettingsEntryState } from './settings-entry.js';

export const INITIAL_DEVICE_MANAGEMENT = Object.freeze({ isCube: false, managementAvailable: false });

/** Device identity comes from hardware/device evidence, never a running cmsd. */
export function deviceManagementState(previous, device, capabilities, entryAllowed) {
  let isCube = previous.isCube;
  if (device.status === 'fulfilled') {
    const value = device.value;
    if (value?.supported === false || value?.hardware?.state === 'incompatible') isCube = false;
    else if (value?.supported === true && (value.detected === true || value.hardware?.state === 'compatible')) isCube = true;
  }
  return { isCube, managementAvailable: entryAllowed
    && (isCube || cmsSettingsEntryState(capabilities) !== 'unsupported') };
}

export function deviceManagementTitle(device, copy) {
  return device.isCube ? copy.deviceManagement : copy.monitor;
}
