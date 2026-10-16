import { useEffect, useMemo, useRef, useState } from 'react';
import { Monitor, Smartphone } from '../../components/icons.jsx';
import ControlledDevicePanel from './ControlledDevicePanel.jsx';
import RemoteControllerPanel from './RemoteControllerPanel.jsx';
import { CONTROLLED_BOUND_STATUSES, countdownSeconds } from './controlled-device-panel-state.mjs';
import { controlledDeviceUiState, controllerConnectionUiState, remoteControlTabTone } from './remote-control-logic.mjs';

function dotClass(state) {
  const tone = remoteControlTabTone(state);
  if (tone === 'success') return 'bg-[#34C759]';
  if (tone === 'waiting') return 'bg-[#FF9F0A]';
  if (tone === 'error') return 'bg-[#FF3B30]';
  return 'bg-[#C7C7CC] dark:bg-[#636366]';
}

const CONTROLLED_STATE_KEYS = {
  off: 'stateOff', stopped: 'stateStopped', registering: 'stateRegistering',
  waiting_for_pairing: 'stateWaiting', expiring: 'stateExpiring', expired: 'stateExpired',
  pairing_reserved: 'statePairingReserved', handshake_pending: 'stateHandshakePending',
  connected: 'stateConnected', reconnecting: 'stateReconnecting',
  resume_provisional: 'stateResumeProvisional',
  resume_activated_pending_liveness: 'stateResumeActivatedPendingLiveness',
  retiring_generation: 'stateRetiringGeneration', stopping: 'stateStopping', error: 'stateError',
};

const CONTROLLER_STATE_KEYS = {
  empty: 'stateEmpty', typing: 'stateTyping', ready: 'stateReady', format_invalid: 'stateFormatInvalid',
  invalid: 'stateInvalid', connecting: 'stateConnecting', safety_handshake: 'stateSafetyHandshake',
  connected: 'stateConnected', reconnecting: 'stateReconnecting', disconnected: 'stateDisconnected',
  busy: 'stateBusy', offline: 'stateOffline', version: 'stateVersion', protocol: 'stateProtocol',
  unsupported: 'stateUnsupported', local_busy: 'stateLocalBusy', failed: 'stateFailed',
};

export default function RemoteControlSettingsPage({
  t,
  bs,
  supported,
  conversationOpen,
  onConversationOpen,
  onConnected,
  onConversationClose,
}) {
  const copy = (t && t.controllerConnection) || {};
  const controlledCopy = (t && t.controlledDevice) || {};
  const [role, setRole] = useState('controlled');
  const [controllerFormValue, setControllerFormValue] = useState('');
  const [controllerFormState, setControllerFormState] = useState('empty');
  const [controlledNow, setControlledNow] = useState(() => Date.now());
  const controlledDevice = bs?.controlledDevice || {};
  const countdownObservation = useRef({ generation: null, remainingMs: null, observedAtMs: controlledNow });
  if (countdownObservation.current.generation !== controlledDevice.generation
    || countdownObservation.current.remainingMs !== controlledDevice.pairingRemainingMs) {
    countdownObservation.current = {
      generation: controlledDevice.generation,
      remainingMs: controlledDevice.pairingRemainingMs,
      observedAtMs: Date.now(),
    };
  }
  const controlledSeconds = countdownSeconds(
    countdownObservation.current.remainingMs,
    countdownObservation.current.observedAtMs,
    controlledNow,
  );

  useEffect(() => {
    if (!Number.isSafeInteger(controlledDevice.pairingRemainingMs)
      && !CONTROLLED_BOUND_STATUSES.has(controlledDevice.status)) return undefined;
    const timer = setInterval(() => setControlledNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [controlledDevice.pairingRemainingMs, controlledDevice.status]);

  const controlledState = useMemo(() => {
    return controlledDeviceUiState(controlledDevice, controlledSeconds);
  }, [controlledDevice, controlledSeconds]);
  const controllerState = controllerConnectionUiState(bs?.controllerConnection || {}, controllerFormState);
  const tabs = [
    { id: 'controlled', label: copy.controlledSide, hint: copy.controlledSideHint, icon: Monitor, state: controlledState, statusLabel: controlledCopy[CONTROLLED_STATE_KEYS[controlledState]] },
    { id: 'controller', label: copy.controllerSide, hint: copy.controllerSideHint, icon: Smartphone, state: controllerState, statusLabel: copy[CONTROLLER_STATE_KEYS[controllerState]] },
  ];

  const selectAdjacentTab = (event, index) => {
    let nextIndex = index;
    if (event.key === 'ArrowRight') nextIndex = (index + 1) % tabs.length;
    else if (event.key === 'ArrowLeft') nextIndex = (index - 1 + tabs.length) % tabs.length;
    else if (event.key === 'Home') nextIndex = 0;
    else if (event.key === 'End') nextIndex = tabs.length - 1;
    else return;
    event.preventDefault();
    setRole(tabs[nextIndex].id);
    document.getElementById(`remote-control-tab-${tabs[nextIndex].id}`)?.focus();
  };

  return (
    <div data-testid="remote-control-settings-page">
      <div className="mb-5 rounded-2xl bg-black/[0.035] p-1 dark:bg-white/[0.06]" role="tablist" aria-label={copy.roleSelectorLabel}>
        <div className="grid grid-cols-2 gap-1">
          {tabs.map((tab, index) => {
            const Icon = tab.icon;
            const selected = role === tab.id;
            return (
              <button
                key={tab.id}
                type="button"
                role="tab"
                id={`remote-control-tab-${tab.id}`}
                aria-selected={selected}
                aria-controls={`remote-control-panel-${tab.id}`}
                tabIndex={selected ? 0 : -1}
                onKeyDown={event => selectAdjacentTab(event, index)}
                data-testid={`remote-control-role-${tab.id}`}
                onClick={() => setRole(tab.id)}
                className={`flex min-w-0 items-center gap-3 rounded-xl px-3 py-2.5 text-left transition ${selected ? 'bg-white shadow-sm dark:bg-[#3A3A3C]' : 'hover:bg-black/[0.025] dark:hover:bg-white/[0.04]'}`}
              >
                <span className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-lg ${selected ? 'bg-[#007AFF]/10 text-[#007AFF]' : 'bg-black/[0.04] text-[#8E8E93] dark:bg-white/[0.06]'}`}>
                  <Icon size={17} aria-hidden="true" />
                </span>
                <span className="min-w-0 flex-1">
                  <span className={`block truncate text-[12px] font-semibold ${selected ? 'text-[#1C1C1E] dark:text-[#F2F2F7]' : 'text-[#6E6E73] dark:text-[#98989D]'}`}>{tab.label}</span>
                  <span className="mt-0.5 block truncate text-[10px] text-[#8E8E93]">{tab.hint}</span>
                </span>
                <span className={`h-2 w-2 shrink-0 rounded-full ${dotClass(tab.state)}`} aria-hidden="true" />
                <span className="sr-only">{copy.roleStatusLabel}: {tab.statusLabel}</span>
              </button>
            );
          })}
        </div>
      </div>

      <div id="remote-control-panel-controlled" role="tabpanel" aria-labelledby="remote-control-tab-controlled" hidden={role !== 'controlled'}>
        <ControlledDevicePanel
          t={t}
          bs={bs}
          supported={supported}
          effectiveState={controlledState}
          secondsRemaining={controlledSeconds}
          nowMs={controlledNow}
        />
      </div>
      <div id="remote-control-panel-controller" role="tabpanel" aria-labelledby="remote-control-tab-controller" hidden={role !== 'controller'}>
        <RemoteControllerPanel
          t={t}
          bs={bs}
          conversationOpen={conversationOpen}
          onConversationOpen={onConversationOpen}
          onConnected={onConnected}
          onConversationClose={onConversationClose}
          formValue={controllerFormValue}
          formState={controllerFormState}
          onFormValueChange={setControllerFormValue}
          onFormStateChange={setControllerFormState}
        />
      </div>
    </div>
  );
}
