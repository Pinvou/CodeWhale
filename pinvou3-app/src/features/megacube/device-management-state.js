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
  const managementAvailable = entryAllowed
    && (isCube || cmsSettingsEntryState(capabilities) !== 'unsupported');
  // r14 R4：两字段未变化时返回 previous 的对象身份，让 setState 走 React 的
  // bail-out——否则 30s 探测的每 tick 都换新字面量，根组件级联全 App 重渲染。
  if (previous.isCube === isCube && previous.managementAvailable === managementAvailable) return previous;
  return { isCube, managementAvailable };
}

export function deviceManagementTitle(device, copy) {
  return device.isCube ? copy.deviceManagement : copy.monitor;
}
