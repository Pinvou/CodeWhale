// Windows A 主控端连接入口（DEV-22）。
//
// 只做本地格式归一化（粘贴、大小写、分隔符）；服务端失效原因统一文案，
// 不提供最近连接码历史，也不保存连接码。所有文案走三语词条。

import { useEffect, useRef, useState } from 'react';
import { connectionErrorKey, groupPairingCode, normalizePairingCode } from './remote-control-logic.mjs';

export default function ConnectRemoteDeviceDialog({ t, open, onClose, onConnected }) {
  const copy = (t && t.controllerConnection) || {};
  const [value, setValue] = useState('');
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const inputRef = useRef(null);

  useEffect(() => {
    if (!open) return;
    setValue('');
    setError(null);
    setBusy(false);
    const timer = setTimeout(() => inputRef.current && inputRef.current.focus(), 0);
    return () => clearTimeout(timer);
  }, [open]);

  if (!open) return null;

  const submit = async () => {
    const code = normalizePairingCode(value);
    if (!code) {
      setError('errorLocalFormat');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const bridgeApi = (await import('../../hooks/useBridge.js')).bridge;
      if (!bridgeApi || !bridgeApi.desktopRemoteControl) {
        setError('errorGeneric');
        return;
      }
      const result = await bridgeApi.desktopRemoteControl.controllerConnect(code);
      if (onConnected) onConnected(result);
      if (onClose) onClose();
    } catch (failure) {
      setError(connectionErrorKey(failure && failure.code));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40" role="presentation">
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="connect-remote-title"
        className="w-[420px] max-w-[92vw] rounded-2xl bg-white p-5 shadow-xl dark:bg-[#1F2023]"
      >
        <h2 id="connect-remote-title" className="text-[16px] font-medium">{copy.title}</h2>
        <p className="mt-2 text-[12px] opacity-70">{copy.desc}</p>
        <label className="mt-4 block text-[12px]" htmlFor="connect-remote-code">{copy.codeLabel}</label>
        <input
          id="connect-remote-code"
          ref={inputRef}
          value={value}
          onChange={(event) => setValue(groupPairingCode(event.target.value))}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && !busy) submit();
          }}
          placeholder={copy.codePlaceholder}
          autoComplete="off"
          spellCheck={false}
          inputMode="text"
          aria-invalid={!!error}
          aria-describedby={error ? 'connect-remote-error' : 'connect-remote-hint'}
          className="mt-1 w-full rounded-lg border border-black/10 px-3 py-2 font-mono text-[18px] tracking-[0.2em] dark:border-white/15 dark:bg-transparent"
        />
        <p id="connect-remote-hint" className="mt-2 text-[11px] opacity-60">{copy.codeHint}</p>
        {error && <p id="connect-remote-error" role="alert" className="mt-2 text-[12px] text-[#EA4335]">{copy[error]}</p>}
        <div className="mt-4 flex justify-end gap-2">
          <button type="button" onClick={onClose} disabled={busy} className="rounded-full px-4 py-1.5 text-[13px]">
            {copy.close}
          </button>
          <button
            type="button"
            onClick={submit}
            disabled={busy}
            className="rounded-full bg-[#1A73E8] px-4 py-1.5 text-[13px] text-white disabled:opacity-60"
          >
            {busy ? copy.connecting : copy.connect}
          </button>
        </div>
      </div>
    </div>
  );
}
