import { useEffect, useMemo, useRef, useState } from 'react';
import { AlertTriangle, CheckCircle2, Copy, Link } from '../../components/icons.jsx';
import { bridge } from '../../hooks/useBridge.js';
import { readClipboardText } from '../conversation/message-clipboard.js';
import {
  connectionErrorKey,
  connectionErrorState,
  createAsyncOperationGuard,
  groupPairingCodeInput,
  normalizePairingCode,
  pairingCodeInputState,
} from './remote-control-logic.mjs';

export default function RemoteConnectForm({
  t,
  value = '',
  onValueChange,
  onConnected,
  onUiStateChange,
  connectCancellation,
}) {
  const copy = (t && t.controllerConnection) || {};
  const [errorKey, setErrorKey] = useState(null);
  const [errorCode, setErrorCode] = useState(null);
  const [busy, setBusy] = useState(false);
  const operationGuard = useRef(createAsyncOperationGuard());
  const inputState = useMemo(() => pairingCodeInputState(value), [value]);
  const uiState = errorCode ? connectionErrorState(errorCode) : inputState;

  useEffect(() => {
    const guard = createAsyncOperationGuard();
    operationGuard.current = guard;
    return () => guard.dispose();
  }, []);

  const updateValue = (raw) => {
    const compact = String(raw || '').toUpperCase().replace(/[\s-]/g, '');
    const formatted = pairingCodeInputState(raw) !== 'format_invalid'
      ? groupPairingCodeInput(compact)
      : String(raw || '').toUpperCase();
    onValueChange?.(formatted);
    setErrorKey(null);
    setErrorCode(null);
    onUiStateChange?.(pairingCodeInputState(formatted));
  };

  const paste = async () => {
    const text = await readClipboardText();
    if (text) updateValue(text);
  };

  const submit = async (event) => {
    event.preventDefault();
    const code = normalizePairingCode(value);
    if (!code || inputState !== 'ready') {
      setErrorKey('errorLocalFormat');
      onUiStateChange?.('format_invalid');
      return;
    }
    setBusy(true);
    setErrorKey(null);
    setErrorCode(null);
    onUiStateChange?.('connecting');
    const attempt = operationGuard.current.start();
    const cancellationGeneration = connectCancellation?.current;
    const isAttemptCurrent = () => operationGuard.current.isCurrent(attempt)
      && connectCancellation?.current === cancellationGeneration;
    try {
      if (!bridge.available || !bridge.desktopRemoteControl) {
        if (isAttemptCurrent()) {
          setErrorKey('errorGeneric');
          setErrorCode('bridge_unavailable');
          onUiStateChange?.('failed');
        }
        return;
      }
      await bridge.desktopRemoteControl.controllerConnect(code);
      if (!isAttemptCurrent()) return;
      const opened = await bridge.desktopRemoteControl.openLiveRemoteProxy(null);
      if (!isAttemptCurrent()) return;
      if (await onConnected?.(opened, isAttemptCurrent) === false && isAttemptCurrent()) {
        throw new Error('remote_proxy_binding_mismatch');
      }
      if (!isAttemptCurrent()) return;
      onValueChange?.('');
      onUiStateChange?.('empty');
    } catch (failure) {
      if (!isAttemptCurrent()) return;
      const rawCode = failure?.code || '';
      setErrorKey(connectionErrorKey(rawCode));
      setErrorCode(rawCode || 'desktop_remote_control_failed');
      onUiStateChange?.(connectionErrorState(rawCode));
    } finally {
      if (isAttemptCurrent()) setBusy(false);
    }
  };

  const invalid = inputState === 'format_invalid' || !!errorKey;
  const ready = inputState === 'ready' && !busy;

  return (
    <form
      data-testid="remote-controller-connect-form"
      data-ui-state={uiState}
      aria-labelledby="remote-controller-connect-title"
      onSubmit={submit}
      className="rounded-2xl border border-black/[0.08] bg-white p-5 shadow-[0_1px_2px_rgba(0,0,0,0.03)] dark:border-white/[0.1] dark:bg-white/[0.035]"
    >
      <div className="flex items-start gap-3">
        <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[#007AFF]/10 text-[#007AFF]">
          <Link size={18} aria-hidden="true" />
        </div>
        <div>
          <h3 id="remote-controller-connect-title" className="text-[15px] font-semibold">{copy.title}</h3>
          <p className="mt-1 text-[12px] leading-5 text-[#6E6E73] dark:text-[#98989D]">{copy.desc}</p>
        </div>
      </div>

      <label className="mt-5 block text-[12px] font-semibold" htmlFor="connect-remote-code">{copy.codeLabel}</label>
      <div className="relative mt-2">
        <input
          id="connect-remote-code"
          data-testid="remote-controller-code-input"
          value={value}
          onChange={event => updateValue(event.target.value)}
          disabled={busy}
          placeholder={copy.codePlaceholder}
          autoComplete="off"
          spellCheck={false}
          inputMode="text"
          maxLength={9}
          aria-invalid={invalid}
          aria-describedby={invalid ? 'connect-remote-error' : 'connect-remote-hint'}
          className={`h-12 w-full rounded-xl border bg-transparent px-4 pr-20 font-mono text-[19px] font-semibold tracking-[0.16em] outline-none transition ${invalid ? 'border-[#FF3B30] ring-2 ring-[#FF3B30]/10' : ready ? 'border-[#34C759] ring-2 ring-[#34C759]/10' : 'border-black/10 focus:border-[#007AFF] focus:ring-2 focus:ring-[#007AFF]/10 dark:border-white/15'}`}
        />
        <button type="button" onClick={paste} className="absolute right-2 top-1/2 inline-flex -translate-y-1/2 items-center gap-1 rounded-lg px-2 py-1.5 text-[11px] font-medium text-[#007AFF] hover:bg-[#007AFF]/[0.06]">
          <Copy size={13} aria-hidden="true" /> {copy.paste}
        </button>
      </div>

      {!invalid && (
        <p id="connect-remote-hint" className={`mt-2 flex items-center gap-1.5 text-[11px] leading-4 ${ready ? 'text-[#248A3D] dark:text-[#5DD879]' : 'text-[#6E6E73] dark:text-[#98989D]'}`}>
          {ready && <CheckCircle2 size={13} aria-hidden="true" />}
          {inputState === 'empty' ? copy.inputEmpty : inputState === 'typing' ? copy.inputTyping : copy.inputReady}
        </p>
      )}
      {invalid && (
        <p id="connect-remote-error" role="alert" className="mt-2 flex gap-1.5 text-[11px] leading-4 text-[#C9342C] dark:text-[#FF6961]">
          <AlertTriangle size={13} className="mt-0.5 shrink-0" aria-hidden="true" />
          <span>{errorKey ? copy[errorKey] : copy.errorLocalFormat}{errorCode && <span className="settings-error-code ml-1 font-mono opacity-70">({String(errorCode)})</span>}</span>
        </p>
      )}

      <button
        type="submit"
        data-testid="remote-controller-connect-submit"
        disabled={!ready}
        className="mt-5 flex h-10 w-full items-center justify-center rounded-xl bg-[#007AFF] text-[13px] font-semibold text-white transition hover:bg-[#006FE6] disabled:cursor-not-allowed disabled:bg-black/[0.08] disabled:text-black/30 dark:disabled:bg-white/[0.08] dark:disabled:text-white/30"
      >
        {busy ? copy.connecting : copy.connect}
      </button>
      <p className="mt-3 text-center text-[10px] leading-4 text-[#8E8E93]">{copy.codeHint}</p>
    </form>
  );
}
