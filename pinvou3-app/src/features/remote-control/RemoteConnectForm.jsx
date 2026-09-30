// A-side inline connect form: the code lives in the remote-control settings
// page instead of a floating dialog behind the settings modal.
import { useState } from 'react';
import { bridge } from '../../hooks/useBridge.js';
import { connectionErrorKey, groupPairingCode, normalizePairingCode } from './remote-control-logic.mjs';

export default function RemoteConnectForm({ t, onConnected }) {
  const copy = (t && t.controllerConnection) || {};
  const [value, setValue] = useState('');
  const [error, setError] = useState(null);
  const [errorCode, setErrorCode] = useState(null);
  const [busy, setBusy] = useState(false);

  const submit = async (event) => {
    event.preventDefault();
    const code = normalizePairingCode(value);
    if (!code) {
      setError('errorLocalFormat');
      return;
    }
    setBusy(true);
    setError(null);
    setErrorCode(null);
    try {
      if (!bridge.available || !bridge.desktopRemoteControl) {
        setError('errorGeneric');
        setErrorCode('bridge_unavailable');
        return;
      }
      const result = await bridge.desktopRemoteControl.controllerConnect(code);
      setValue('');
      if (onConnected) onConnected(result);
    } catch (failure) {
      const rawCode = (failure && failure.code) || '';
      setError(connectionErrorKey(rawCode));
      setErrorCode(rawCode || null);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form
      data-testid="remote-controller-connect-form"
      aria-labelledby="remote-controller-connect-title"
      onSubmit={submit}
      className="rounded-xl border border-black/10 p-4 dark:border-white/10"
    >
      <h3 id="remote-controller-connect-title" className="text-[15px] font-medium">{copy.title}</h3>
      <p className="mt-2 text-[12px] leading-5 opacity-70">{copy.desc}</p>
      <label className="mt-4 block text-[12px]" htmlFor="connect-remote-code">{copy.codeLabel}</label>
      <input
        id="connect-remote-code"
        data-testid="remote-controller-code-input"
        value={value}
        onChange={(event) => setValue(groupPairingCode(event.target.value))}
        placeholder={copy.codePlaceholder}
        autoComplete="off"
        spellCheck={false}
        inputMode="text"
        aria-invalid={!!error}
        aria-describedby={error ? 'connect-remote-error' : 'connect-remote-hint'}
        className="mt-1 w-full rounded-lg border border-black/10 px-3 py-2 font-mono text-[18px] tracking-[0.2em] dark:border-white/15 dark:bg-transparent"
      />
      <p id="connect-remote-hint" className="mt-2 text-[11px] leading-4 opacity-60">{copy.codeHint}</p>
      {error && (
        <p id="connect-remote-error" role="alert" className="mt-2 text-[12px] text-[#EA4335]">
          {copy[error]}
          {errorCode && <span className="settings-error-code"> ({String(errorCode)})</span>}
        </p>
      )}
      <div className="mt-4 flex justify-end">
        <button
          type="submit"
          data-testid="remote-controller-connect-submit"
          disabled={busy}
          className="rounded-full bg-[#1A73E8] px-4 py-1.5 text-[13px] text-white disabled:opacity-60"
        >
          {busy ? copy.connecting : copy.connect}
        </button>
      </div>
    </form>
  );
}
