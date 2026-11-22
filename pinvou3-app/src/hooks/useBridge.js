import { useEffect, useState } from 'react';
import { can, isWeb } from '../shared/platform.js';
import { isLocalModel, activeModelIsLocal } from '../shared/model-options.js';
export { shouldShowModelAccessGate } from '../shared/model-access-state.js';
import { CHAT_RECOVERY_COPY } from '../shared/i18n.js';

const bridge = window.TauriBridge || { available: false };

function mergeSlices(domains) {
  if (!bridge.available || !bridge.state) return null;
  return bridge.state.getMany(domains);
}

function useBridgeState(domains) {
  const domainKey = domains.join('|');
  const [bridgeState, setBridgeState] = useState(() => mergeSlices(domains));
  useEffect(() => {
    if (!bridge.available) return undefined;
    bridge.lifecycle.init({ uiDictionary: CHAT_RECOVERY_COPY }).catch(e => console.warn('[TauriBridge] init failed', e));
    return bridge.state.subscribeMany(domains, setBridgeState);
  }, [domainKey]);
  return bridgeState;
}

function usePlatformCapability(capability) {
  const [supported, setSupported] = useState(() => can(capability));
  useEffect(() => {
    const refresh = () => setSupported(can(capability));
    if (!isWeb) {
      if (!['desktopRemoteControl', 'failureDiagnostics'].includes(capability) || !bridge.available || !bridge.state) return undefined;
      return bridge.state.subscribe('platform', refresh);
    }
    window.addEventListener('pinvou:web-capabilities', refresh);
    window.addEventListener('pinvou:web-connection', refresh);
    refresh();
    return () => {
      window.removeEventListener('pinvou:web-capabilities', refresh);
      window.removeEventListener('pinvou:web-connection', refresh);
    };
  }, [capability]);
  return supported;
}

export {
  bridge,
  useBridgeState,
  usePlatformCapability,
  isLocalModel,
  activeModelIsLocal,
};
