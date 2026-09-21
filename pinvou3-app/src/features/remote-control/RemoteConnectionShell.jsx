// Windows A 主控端连接状态壳（DEV-22）。
//
// 关闭窗口只改变可见性：shell 明确区分「隐藏窗口（连接保持）」与
// 「断开连接（显式断开并安全收尾）」。断开后壳层不可发送/审批。

import { useEffect, useState } from 'react';
import { bridge } from '../../hooks/useBridge.js';
import RemoteConversationView from './RemoteConversationView.jsx';

const TERMINAL_STATUSES = ['idle', 'disconnected', 'error'];

export default function RemoteConnectionShell({ t, bs, onClose, onRequestDisconnect, modelSource, onModelSourceChange }) {
  const copy = (t && t.controllerConnection) || {};
  const connection = (bs && bs.controllerConnection) || {};
  const connected = connection.status === 'connected';
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  useEffect(() => {
    if (TERMINAL_STATUSES.includes(connection.status)) {
      setBusy(false);
    }
  }, [connection.status]);

  const disconnect = async () => {
    setBusy(true);
    setError(null);
    try {
      if (bridge && bridge.available && bridge.desktopRemoteControl && onRequestDisconnect) {
        await onRequestDisconnect();
      }
    } catch (failure) {
      setError((failure && failure.code) || 'errorGeneric');
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="remote-connection-shell" aria-labelledby="remote-connection-title">
      <header className="flex items-center justify-between gap-2">
        <div>
          <h2 id="remote-connection-title" className="text-[15px] font-medium">{copy.title}</h2>
          <p className="text-[12px] opacity-70" role="status" aria-live="polite">
            {connected ? copy.connectedTo(connection.deviceName || copy.connected) : copy[`error${connection.status}`] || copy.connecting}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <button type="button" onClick={onClose}>{copy.close}</button>
          <button type="button" onClick={disconnect} disabled={busy || !connected}>
            {busy ? copy.connecting : copy.disconnect}
          </button>
        </div>
      </header>
      <p className="mt-1 text-[11px] opacity-60">{copy.backgroundHint}</p>
      {error && <p role="alert" className="text-[12px] text-[#EA4335]">{copy.errorGeneric}</p>}
      {connected ? (
        // 远程会话窗口：模型/能力/审批/附件/成果/历史全部经 DEV-20 bridge。
        <RemoteConversationView t={t} bs={bs} modelSource={modelSource} onModelSourceChange={onModelSourceChange} />
      ) : (
        <p className="mt-3 text-[12px] opacity-70">{copy.codeHint}</p>
      )}
    </section>
  );
}
