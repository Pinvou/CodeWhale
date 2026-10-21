import { useEffect, useState } from 'react';
import { canInvoke, onPlatformConnectionChange } from '../../shared/platform.js';
import { invokeTauri, isTauriAvailable } from '../../platform/tauri/client.js';
import { fetchCmsDeviceStatus } from './cms-api.js';
import { INITIAL_DEVICE_MANAGEMENT, deviceManagementState } from './device-management-state.js';

/** Shared by a window's navigation and page, including when onboarding is dismissed. */
export function useDeviceManagement({ enabled = true } = {}) {
  const [device, setDevice] = useState(INITIAL_DEVICE_MANAGEMENT);
  useEffect(() => {
    if (!enabled) return undefined;
    let alive = true;
    let generation = 0;
    const probe = async () => {
      const ticket = ++generation;
      const commands = ['cms_status', 'cms_service_status'];
      const allowed = isTauriAvailable() && commands.some(canInvoke);
      const query = command => canInvoke(command)
        ? invokeTauri(command) : Promise.resolve({ supported: false });
      const [device, ...capabilities] = await Promise.allSettled([
        isTauriAvailable() && canInvoke('cms_device_status')
          ? fetchCmsDeviceStatus(true) : Promise.resolve({ supported: false }),
        ...commands.map(query),
      ]);
      if (alive && ticket === generation) {
        setDevice(previous => deviceManagementState(previous, device, capabilities, allowed));
      }
    };
    void probe();
    const timer = setInterval(() => { void probe(); }, 30000);
    const disconnect = onPlatformConnectionChange(() => {
      // A browser connection may now refer to another host. Do not carry its identity over.
      setDevice(INITIAL_DEVICE_MANAGEMENT);
      void probe();
    });
    return () => { alive = false; ++generation; clearInterval(timer); disconnect(); };
  }, [enabled]);
  return device;
}
