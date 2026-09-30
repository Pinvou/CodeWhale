// Controller-side operation zone. It keeps pairing, connection status and the
// remote conversation in the settings page, visually separate from the B panel.
import { useEffect, useState } from 'react';
import { bridge } from '../../hooks/useBridge.js';
import { connectionErrorKey } from './remote-control-logic.mjs';
import RemoteConnectForm from './RemoteConnectForm.jsx';

const TERMINAL_STATUSES = ['idle', 'disconnected', 'error'];
const ACTIVE_STATUSES = ['connecting', 'safety_handshake', 'connected', 'reconnecting'];

export default function RemoteControllerPanel({
  t,
  bs,
  conversationOpen = false,
  onConversationOpen,
  onConnected,
  onConversationClose,
}) {
  const copy = (t && t.controllerConnection) || {};
  const connection = (bs && bs.controllerConnection) || {};
  const connected = connection.status === 'connected';
  const active = ACTIVE_STATUSES.includes(connection.status);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  // 窗口重载后 Rust 状态机是权威；桥接初始化时能力可能还没就绪，
  // 进入主控端面板时必须再同步一次，避免后端 connected 而前端显示表单。
  useEffect(() => {
    if (!bridge.available || !bridge.desktopRemoteControl) return undefined;
    bridge.desktopRemoteControl.controllerStatus().catch(() => {});
    return undefined;
  }, []);

  useEffect(() => {
    if (TERMINAL_STATUSES.includes(connection.status)) setBusy(false);
  }, [connection.status]);

  const disconnect = async () => {
    setBusy(true);
    setError(null);
    try {
      if (bridge.available && bridge.desktopRemoteControl) {
        await bridge.desktopRemoteControl.controllerDisconnect();
      }
      if (onConversationClose) onConversationClose();
    } catch (failure) {
      setError(connectionErrorKey(failure && failure.code));
    } finally {
      setBusy(false);
    }
  };

  const status = connected
    ? copy.connectedTo(connection.deviceName || copy.connected)
    : connection.status === 'connecting'
      ? copy.connecting
      : (connection.lastError || connection.status === 'error')
        ? (copy[connectionErrorKey(connection.lastError)] || copy.errorGeneric)
        : copy.connecting;

  return (
    <div data-testid="remote-controller-panel" className="space-y-3">
      {connected && !conversationOpen && (
        <div className="rounded-xl border border-black/10 p-4 dark:border-white/10">
          <div className="text-sm font-medium" role="status" aria-live="polite">{status}</div>
          <div className="mt-3 flex justify-end gap-2">
            <button
              type="button"
              data-testid="remote-conversation-open"
              onClick={onConversationOpen}
              className="rounded-full bg-[#1A73E8] px-4 py-1.5 text-[13px] text-white"
            >
              {copy.open}
            </button>
            <button
              type="button"
              onClick={disconnect}
              disabled={busy}
              className="rounded-full px-4 py-1.5 text-[13px] disabled:opacity-60"
            >
              {busy ? copy.connecting : copy.disconnect}
            </button>
          </div>
        </div>
      )}

      {!connected && !active && (
        <>
          {connection.status !== 'idle' && (
            <div
              className="rounded-xl border border-black/10 p-3 text-[12px] opacity-70 dark:border-white/10"
              role="status"
              aria-live="polite"
            >
              {status}
            </div>
          )}
          <RemoteConnectForm t={t} onConnected={onConnected || onConversationOpen} />
          {error && (
            <p role="alert" className="text-[12px] text-[#EA4335]">{copy[error]}</p>
          )}
        </>
      )}

      {!connected && active && (
        <div className="rounded-xl border border-black/10 p-4 dark:border-white/10">
          <div className="text-sm font-medium" role="status" aria-live="polite">{status}</div>
          <div className="mt-3 flex justify-end">
            <button
              type="button"
              onClick={disconnect}
              disabled={busy}
              className="rounded-full px-4 py-1.5 text-[13px] disabled:opacity-60"
            >
              {busy ? copy.connecting : copy.disconnect}
            </button>
          </div>
        </div>
      )}

    </div>
  );
}
