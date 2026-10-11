import { useEffect, useRef, useState } from 'react';
import { AlertTriangle, CheckCircle2, Clock, Lock, RefreshCw } from '../../components/icons.jsx';
import { bridge } from '../../hooks/useBridge.js';
import { connectionErrorKey, controllerConnectionUiState } from './remote-control-logic.mjs';
import RemoteConnectForm from './RemoteConnectForm.jsx';
import RemoteProxyArchivePanel from './RemoteProxyArchivePanel.jsx';

const ACTIVE_STATES = ['connecting', 'safety_handshake', 'connected', 'reconnecting'];
const ERROR_STATES = ['invalid', 'busy', 'offline', 'version', 'protocol', 'unsupported', 'local_busy', 'failed'];

const STATE_META = {
  empty: { key: 'stateEmpty', tone: 'neutral' },
  typing: { key: 'stateTyping', tone: 'neutral' },
  ready: { key: 'stateReady', tone: 'ready' },
  format_invalid: { key: 'stateFormatInvalid', tone: 'danger' },
  invalid: { key: 'stateInvalid', tone: 'danger' },
  connecting: { key: 'stateConnecting', tone: 'progress' },
  safety_handshake: { key: 'stateSafetyHandshake', tone: 'progress' },
  connected: { key: 'stateConnected', tone: 'success' },
  reconnecting: { key: 'stateReconnecting', tone: 'warning' },
  disconnected: { key: 'stateDisconnected', tone: 'neutral' },
  busy: { key: 'stateBusy', tone: 'warning' },
  offline: { key: 'stateOffline', tone: 'danger' },
  version: { key: 'stateVersion', tone: 'danger' },
  protocol: { key: 'stateProtocol', tone: 'danger' },
  unsupported: { key: 'stateUnsupported', tone: 'danger' },
  local_busy: { key: 'stateLocalBusy', tone: 'warning' },
  failed: { key: 'stateFailed', tone: 'danger' },
};

const TONES = {
  neutral: 'bg-black/[0.04] text-[#6E6E73] dark:bg-white/[0.06] dark:text-[#AEAEB2]',
  ready: 'bg-[#007AFF]/10 text-[#0066D6] dark:text-[#64A8FF]',
  progress: 'bg-[#5856D6]/10 text-[#4D4BC4] dark:text-[#9B99FF]',
  success: 'bg-[#34C759]/10 text-[#248A3D] dark:text-[#5DD879]',
  warning: 'bg-[#FF9F0A]/12 text-[#A65F00] dark:text-[#FFB340]',
  danger: 'bg-[#FF3B30]/10 text-[#C9342C] dark:text-[#FF6961]',
};

function ConnectionCard({ copy, state, deviceName, busy, onDisconnect, onOpen }) {
  const meta = STATE_META[state] || STATE_META.failed;
  const detailKey = `detail${state.charAt(0).toUpperCase()}${state.slice(1)}`;
  const Icon = state === 'connected' ? CheckCircle2 : ERROR_STATES.includes(state) ? AlertTriangle : state === 'safety_handshake' ? Lock : state === 'reconnecting' ? RefreshCw : Clock;
  const spinning = ['connecting', 'safety_handshake', 'reconnecting'].includes(state);
  return (
    <div className="rounded-2xl border border-black/[0.08] bg-white p-5 shadow-[0_1px_2px_rgba(0,0,0,0.03)] dark:border-white/[0.1] dark:bg-white/[0.035]">
      <div className={`flex gap-3 rounded-xl p-4 ${TONES[meta.tone]}`} role={ERROR_STATES.includes(state) ? 'alert' : 'status'} aria-live="polite">
        <Icon size={19} className={`mt-0.5 shrink-0 ${spinning && state !== 'safety_handshake' ? 'animate-spin motion-reduce:animate-none' : ''}`} aria-hidden="true" />
        <div className="min-w-0">
          <div className="text-[13px] font-semibold">{copy[meta.key]}</div>
          <div className="mt-1 text-[12px] leading-5 opacity-80">
            {state === 'invalid' ? copy.errorInvalidCode : copy[detailKey]}
          </div>
          {['connected', 'reconnecting'].includes(state) && (
            <div className="mt-2 text-[12px] font-medium">{copy.deviceLabel}: {deviceName || copy.deviceUnknown}</div>
          )}
        </div>
      </div>
      {ACTIVE_STATES.includes(state) && (
        <div className="mt-4 flex justify-end gap-2 border-t border-black/[0.06] pt-4 dark:border-white/[0.08]">
          {state === 'connected' && (
            <button type="button" data-testid="remote-conversation-open" onClick={onOpen} disabled={busy || !onOpen} className="rounded-lg bg-[#007AFF] px-4 py-2 text-[12px] font-semibold text-white hover:bg-[#006FE6] disabled:opacity-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] forced-colors:border forced-colors:border-[ButtonText]">
              {copy.open}
            </button>
          )}
          <button type="button" onClick={onDisconnect} disabled={busy} className="rounded-lg border border-black/10 px-4 py-2 text-[12px] font-medium hover:bg-black/[0.03] disabled:opacity-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] forced-colors:border forced-colors:border-[ButtonText] dark:border-white/10 dark:hover:bg-white/[0.06]">
            {busy ? copy.disconnecting : state === 'connected' ? copy.disconnect : copy.cancelConnection}
          </button>
        </div>
      )}
      {ACTIVE_STATES.includes(state) && <p className="mt-2 text-[11px] leading-4 text-[#6E6E73] dark:text-[#AEAEB2]">{copy.disconnectSafetyHint}</p>}
    </div>
  );
}

export default function RemoteControllerPanel({
  t,
  bs,
  conversationOpen = false,
  onConversationOpen,
  onConnected,
  onConversationClose,
  formValue = '',
  formState = 'empty',
  onFormValueChange,
  onFormStateChange,
}) {
  const copy = (t && t.controllerConnection) || {};
  const connection = (bs && bs.controllerConnection) || {};
  const [busy, setBusy] = useState(false);
  const [disconnectError, setDisconnectError] = useState(null);
  const [cancelEpoch, setCancelEpoch] = useState(0);
  const connectCancellation = useRef(0);
  const state = controllerConnectionUiState(connection, formState);
  const showForm = !ACTIVE_STATES.includes(state);
  const showStatusCard = ACTIVE_STATES.includes(state) || state === 'disconnected' || ERROR_STATES.includes(state);

  useEffect(() => {
    if (!bridge.available || !bridge.desktopRemoteControl) return undefined;
    bridge.desktopRemoteControl.controllerStatus().catch(() => {});
    return undefined;
  }, []);

  useEffect(() => {
    if (!ACTIVE_STATES.includes(state)) setBusy(false);
  }, [state]);

  const disconnect = async () => {
    connectCancellation.current += 1;
    onFormValueChange?.('');
    onFormStateChange?.('empty');
    setCancelEpoch(epoch => epoch + 1);
    setBusy(true);
    setDisconnectError(null);
    try {
      if (!bridge.available || !bridge.desktopRemoteControl) {
        setDisconnectError('errorUnsupported');
        return;
      }
      await bridge.desktopRemoteControl.controllerDisconnect();
      onConversationClose?.();
    } catch (failure) {
      setDisconnectError(connectionErrorKey(failure?.code));
    } finally {
      setBusy(false);
    }
  };

  const openConversation = async () => {
    if (busy) return;
    setBusy(true);
    setDisconnectError(null);
    try { await onConversationOpen?.(); }
    catch (failure) { setDisconnectError(connectionErrorKey(failure?.code)); }
    finally { setBusy(false); }
  };

  return (
    <section data-testid="remote-controller-panel" data-ui-state={state} aria-labelledby="remote-controller-panel-title" className="space-y-4">
      <div>
        <h3 id="remote-controller-panel-title" className="text-[16px] font-semibold text-[#1C1C1E] dark:text-[#F2F2F7]">{copy.controllerTitle}</h3>
        <p className="mt-1 text-[12px] leading-5 text-[#6E6E73] dark:text-[#98989D]">{copy.controllerDesc}</p>
      </div>

      {showStatusCard && (
        <ConnectionCard
          copy={copy}
          state={state}
          deviceName={connection.deviceName}
          busy={busy}
          onDisconnect={disconnect}
          onOpen={onConversationOpen ? openConversation : undefined}
        />
      )}

      <div hidden={!showForm} aria-hidden={!showForm}>
        <RemoteConnectForm
          key={cancelEpoch}
          t={t}
          value={formValue}
          onValueChange={onFormValueChange}
          onConnected={onConnected || onConversationOpen}
          onUiStateChange={onFormStateChange}
          connectCancellation={connectCancellation}
        />
      </div>

      {disconnectError && (
        <div role="alert" className="flex gap-2 rounded-xl bg-[#FF3B30]/10 px-3 py-2.5 text-[11px] leading-4 text-[#C9342C] dark:text-[#FF6961]">
          <AlertTriangle size={14} className="mt-0.5 shrink-0" aria-hidden="true" />
          <span>{copy[disconnectError] || copy.errorGeneric}</span>
        </div>
      )}

      {connection.status === 'connected' && conversationOpen && (
        <div className="flex items-center gap-2 rounded-xl bg-[#34C759]/[0.08] px-3 py-2.5 text-[11px] text-[#248A3D] dark:text-[#5DD879]">
          <CheckCircle2 size={14} aria-hidden="true" /> {copy.conversationOpen}
        </div>
      )}
      <div className="flex gap-2 rounded-xl bg-black/[0.025] px-3 py-2.5 text-[11px] leading-4 text-[#6E6E73] dark:bg-white/[0.035] dark:text-[#98989D]">
        <Lock size={14} className="mt-0.5 shrink-0" aria-hidden="true" />
        <span>{copy.securityNotice}</span>
      </div>
      <RemoteProxyArchivePanel t={t} />
    </section>
  );
}
