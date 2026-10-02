import { useEffect, useMemo, useRef, useState } from 'react';
import { AlertTriangle, CheckCircle2, Copy, Link } from '../../components/icons.jsx';
import { bridge } from '../../hooks/useBridge.js';
import { readClipboardText } from '../conversation/message-clipboard.js';
import {
  connectionErrorKey,
  connectionErrorState,
  createAsyncOperationGuard,
  createPairingInputGuard,
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
  const [failureState, setFailureState] = useState(null);
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const operationGuard = useRef(createAsyncOperationGuard());
  const inputGuard = useRef(createPairingInputGuard());
  const inputRef = useRef(null);
  const inputState = useMemo(() => pairingCodeInputState(value), [value]);
  const uiState = failureState || inputState;

  useEffect(() => {
    const guard = createAsyncOperationGuard();
    operationGuard.current = guard;
    return () => guard.dispose();
  }, []);

  const updateValue = (raw) => {
    inputGuard.current.changed();
    if (String(raw || '').length > 64) {
      onValueChange?.('');
      setErrorKey('errorLocalFormat');
      setFailureState('format_invalid');
      onUiStateChange?.('format_invalid');
      return;
    }
    const compact = String(raw || '').toUpperCase().replace(/[\s-]/g, '');
    const formatted = pairingCodeInputState(raw) !== 'format_invalid'
      ? groupPairingCodeInput(compact)
      : String(raw || '').toUpperCase();
    onValueChange?.(formatted);
    setErrorKey(null);
    setFailureState(null);
    onUiStateChange?.(pairingCodeInputState(formatted));
  };

  const paste = async () => {
    if (busyRef.current || inputGuard.current.isComposing()) return;
    const revision = inputGuard.current.revision();
    const attempt = operationGuard.current.start();
    try {
      const text = await readClipboardText();
      if (text && inputGuard.current.canApplyPaste(revision) && operationGuard.current.isCurrent(attempt)) updateValue(text);
    } catch {
      if (inputGuard.current.canApplyPaste(revision) && operationGuard.current.isCurrent(attempt)) {
        setErrorKey('errorPaste');
        setFailureState('format_invalid');
        onValueChange?.('');
        onUiStateChange?.('format_invalid');
        inputRef.current?.focus();
      }
    }
  };

  const submit = async (event) => {
    event.preventDefault();
    if (inputGuard.current.isComposing() || event.nativeEvent?.isComposing) return;
    const code = normalizePairingCode(value);
    if (!code || inputState !== 'ready') {
      setErrorKey('errorLocalFormat');
      setFailureState('format_invalid');
      onValueChange?.('');
      onUiStateChange?.('format_invalid');
      inputRef.current?.focus();
      return;
    }
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setErrorKey(null);
    setFailureState(null);
    onUiStateChange?.('connecting');
    const attempt = operationGuard.current.start();
    const cancellationGeneration = connectCancellation?.current;
    const isAttemptCurrent = () => operationGuard.current.isCurrent(attempt)
      && connectCancellation?.current === cancellationGeneration;
    try {
      if (!bridge.available || !bridge.desktopRemoteControl) {
        if (isAttemptCurrent()) {
          setErrorKey('errorUnsupported');
          setFailureState('unsupported');
          onValueChange?.('');
          onUiStateChange?.('unsupported');
          inputRef.current?.focus();
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
      const state = connectionErrorState(rawCode);
      setFailureState(state);
      onValueChange?.('');
      onUiStateChange?.(state);
      inputRef.current?.focus();
    } finally {
      if (isAttemptCurrent()) {
        busyRef.current = false;
        setBusy(false);
      }
    }
  };

  const invalid = inputState === 'format_invalid' || !!errorKey;
  const ready = inputState === 'ready' && !busy && !errorKey;

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
          ref={inputRef}
          id="connect-remote-code"
          data-testid="remote-controller-code-input"
          value={value}
          onChange={event => {
            if (inputGuard.current.isComposing() || event.nativeEvent?.isComposing) {
              inputGuard.current.changed();
              onValueChange?.(event.target.value);
            } else updateValue(event.target.value);
          }}
          onCompositionStart={() => inputGuard.current.compositionStart()}
          onCompositionEnd={event => { inputGuard.current.compositionEnd(); updateValue(event.currentTarget.value); }}
          onPaste={event => {
            if (inputGuard.current.isComposing() || event.nativeEvent?.isComposing) {
              event.preventDefault();
              return;
            }
            const text = event.clipboardData?.getData('text');
            if (text == null) return;
            event.preventDefault();
            updateValue(text);
          }}
          disabled={busy}
          placeholder={copy.codePlaceholder}
          autoComplete="off"
          spellCheck={false}
          inputMode="text"
          maxLength={64}
          aria-invalid={invalid}
          aria-describedby={invalid ? 'connect-remote-error' : 'connect-remote-hint'}
          className={`h-12 w-full rounded-xl border bg-transparent px-4 pr-20 font-mono text-[19px] font-semibold tracking-[0.16em] outline-none transition motion-reduce:transition-none focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] forced-colors:border-[ButtonText] ${invalid ? 'border-[#FF3B30] ring-2 ring-[#FF3B30]/10' : ready ? 'border-[#34C759] ring-2 ring-[#34C759]/10' : 'border-black/10 focus:border-[#007AFF] focus:ring-2 focus:ring-[#007AFF]/10 dark:border-white/15'}`}
        />
        <button type="button" onClick={paste} disabled={busy} className="absolute right-2 top-1/2 inline-flex -translate-y-1/2 items-center gap-1 rounded-lg px-2 py-1.5 text-[11px] font-medium text-[#007AFF] hover:bg-[#007AFF]/[0.06] disabled:opacity-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] forced-colors:border forced-colors:border-[ButtonText]">
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
          <span>{errorKey ? copy[errorKey] : copy.errorLocalFormat}</span>
        </p>
      )}

      <button
        type="submit"
        data-testid="remote-controller-connect-submit"
        disabled={!ready}
        className="mt-5 flex h-10 w-full items-center justify-center rounded-xl bg-[#007AFF] text-[13px] font-semibold text-white transition motion-reduce:transition-none hover:bg-[#006FE6] disabled:cursor-not-allowed disabled:bg-black/[0.08] disabled:text-black/30 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] forced-colors:border forced-colors:border-[ButtonText] dark:disabled:bg-white/[0.08] dark:disabled:text-white/30"
      >
        {busy ? copy.connecting : copy.connect}
      </button>
      <p className="mt-3 text-center text-[10px] leading-4 text-[#8E8E93]">{copy.codeHint}</p>
    </form>
  );
}
