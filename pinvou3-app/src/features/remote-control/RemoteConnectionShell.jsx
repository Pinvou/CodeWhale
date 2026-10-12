// Windows A 主控端连接状态壳（DEV-22）。
//
// 关闭窗口只改变可见性：shell 明确区分「隐藏窗口（连接保持）」与
// 「断开连接（显式断开并安全收尾）」。断开后壳层不可发送/审批。

import { useEffect, useRef, useState } from 'react';
import { bridge } from '../../hooks/useBridge.js';
import { connectionErrorKey, controllerConnectionUiState } from './remote-control-logic.mjs';
import { dialogTabTarget } from './remote-connection-shell-focus.mjs';

const TERMINAL_STATUSES = ['idle', 'disconnected', 'error'];
const DISCONNECTABLE_STATUSES = ['connecting', 'safety_handshake', 'connected', 'reconnecting'];
const STATUS_COPY_KEYS = {
  empty: 'stateEmpty', typing: 'stateTyping', ready: 'stateReady', format_invalid: 'stateFormatInvalid',
  invalid: 'stateInvalid',
  connecting: 'stateConnecting', safety_handshake: 'stateSafetyHandshake',
  reconnecting: 'stateReconnecting', disconnected: 'stateDisconnected',
  busy: 'stateBusy', offline: 'stateOffline', version: 'stateVersion',
  protocol: 'stateProtocol', unsupported: 'stateUnsupported',
  local_busy: 'stateLocalBusy', failed: 'stateFailed',
};

export default function RemoteConnectionShell({ t, bs, onClose, onOpenChat, onRequestDisconnect }) {
  const copy = (t && t.controllerConnection) || {};
  const connection = (bs && bs.controllerConnection) || {};
  const connected = connection.status === 'connected';
  const reconnecting = connection.status === 'reconnecting';
  const disconnectable = DISCONNECTABLE_STATUSES.includes(connection.status);
  const status = controllerConnectionUiState(connection);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const dialogRef = useRef(null);
  const closeRef = useRef(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

  useEffect(() => {
    const previousFocus = document.activeElement;
    closeRef.current?.focus();
    const onKeyDown = event => {
      if (event.key === 'Escape') {
        event.preventDefault();
        onCloseRef.current?.();
        return;
      }
      if (event.key !== 'Tab') return;
      const target = dialogTabTarget(dialogRef.current, document.activeElement, event.shiftKey);
      if (target) {
        event.preventDefault();
        target.focus();
      }
    };
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('keydown', onKeyDown);
      if (previousFocus?.isConnected) previousFocus.focus();
    };
  }, []);

  useEffect(() => {
    if (TERMINAL_STATUSES.includes(connection.status)) {
      setBusy(false);
    }
  }, [connection.status]);

  const disconnect = async () => {
    setBusy(true);
    setError(null);
    try {
      if (!bridge?.available || !bridge.desktopRemoteControl || !onRequestDisconnect) {
        setError('errorUnsupported');
        return;
      }
      await onRequestDisconnect();
    } catch (failure) {
      setError(connectionErrorKey(failure?.code));
    } finally {
      setBusy(false);
    }
  };

  const openChat = async () => {
    setBusy(true);
    setError(null);
    try {
      if (await onOpenChat?.() === false) throw new Error('remote_proxy_binding_mismatch');
    } catch (failure) {
      setError(connectionErrorKey(failure?.code));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section ref={dialogRef} tabIndex={-1} className="remote-connection-shell p-4 sm:p-5 forced-colors:border forced-colors:border-[CanvasText]" aria-labelledby="remote-connection-title">
      <header className="flex items-center justify-between gap-2">
        <div>
          <h2 id="remote-connection-title" className="text-[15px] font-medium">{copy.title}</h2>
          <p className="text-[12px] opacity-70" role="status" aria-live="polite">
            {connected ? copy.stateConnected : copy[STATUS_COPY_KEYS[status]] || copy.stateFailed}
          </p>
          {(connected || reconnecting) && <p className="mt-1 text-[12px]">{copy.deviceLabel}: {connection.deviceName || copy.deviceUnknown}</p>}
        </div>
        <div className="flex flex-wrap items-center justify-end gap-2">
          <button ref={closeRef} type="button" onClick={onClose} className="rounded-lg px-3 py-2 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] forced-colors:border forced-colors:border-[ButtonText]">{copy.close}</button>
          <button type="button" onClick={disconnect} disabled={busy || !disconnectable} className="rounded-lg border border-red-500 px-3 py-2 text-red-700 disabled:opacity-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-red-600 forced-colors:border-[ButtonText] forced-colors:text-[ButtonText] dark:text-red-300 motion-reduce:transition-none">
            {busy ? copy.disconnecting : copy.disconnect}
          </button>
        </div>
      </header>
      <p id="remote-connection-background-hint" className="mt-2 text-[11px] opacity-70">{copy.backgroundHint}</p>
      {disconnectable && <p className="mt-1 text-[11px] opacity-70">{copy.disconnectSafetyHint}</p>}
      {connected && <button type="button" onClick={openChat} disabled={!onOpenChat || busy} className="mt-3 rounded-lg bg-[#007AFF] px-3 py-2 text-white focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] forced-colors:border forced-colors:border-[ButtonText]">{copy.openChat}</button>}
      {error && <p role="alert" className="text-[12px] text-[#EA4335]">{copy[error] || copy.errorGeneric}</p>}
      {!connected && <p className="mt-3 text-[12px] opacity-70">{copy.codeHint}</p>}
    </section>
  );
}
