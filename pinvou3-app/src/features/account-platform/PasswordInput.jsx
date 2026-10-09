import React, { useEffect, useLayoutEffect, useRef, useState } from 'react';
import {
  beginPasswordEdit,
  completePasswordEdit,
  concealPassword,
  expirePasswordEdit,
  isPasswordEditKey,
  passwordCanReveal,
  protectedPasswordState,
  togglePasswordReveal,
} from './password-reveal-state.js';

/** @param {{ visible: boolean }} props */
function PasswordVisibilityIcon({ visible }) {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M2.5 12s3.4-6 9.5-6 9.5 6 9.5 6-3.4 6-9.5 6-9.5-6-9.5-6Z" />
      <circle cx="12" cy="12" r="2.6" />
      {visible && <path d="m4 4 16 16" />}
    </svg>
  );
}

/**
 * @param {{
 *   value: string,
 *   onValueChange: (value: string) => void,
 *   onBlur?: (event: React.FocusEvent<HTMLDivElement>) => void,
 *   resetKey: string | number,
 *   showLabel: string,
 *   hideLabel: string,
 *   disabled?: boolean,
 *   placeholder?: string,
 *   ariaLabel: string,
 *   autoComplete: string,
 *   controlTheme: string,
 * }} props
 */
export function PasswordInput({ value, onValueChange, onBlur, resetKey, showLabel, hideLabel, disabled, placeholder, ariaLabel, autoComplete, controlTheme }) {
  const inputRef = useRef(/** @type {HTMLInputElement | null} */ (null));
  const transferIntentRef = useRef(/** @type {{ inputType: string, data: string, selectionStart: number | null, selectionEnd: number | null } | null} */ (null));
  const editIntentRef = useRef(/** @type {{ intent: import('./password-reveal-state.js').PasswordEditIntent } | null} */ (null));
  const resetMarkerRef = useRef(resetKey);
  const revealRef = useRef(protectedPasswordState(value));
  const [revealState, setRevealState] = useState(revealRef.current);
  /** @param {import('./password-reveal-state.js').PasswordRevealState} next */
  const updateRevealState = (next) => {
    revealRef.current = next;
    setRevealState(next);
  };
  const protectCurrentValue = () => updateRevealState(protectedPasswordState(inputRef.current?.value ?? value));

  const resetChanged = resetMarkerRef.current !== resetKey;
  const renderedRevealState = resetChanged ? protectedPasswordState(value) : revealState;
  const renderedInputValue = resetChanged ? value : inputRef.current?.value ?? value;
  const revealed = !resetChanged
    && renderedRevealState.revealed
    && renderedRevealState.manualValue === value
    && passwordCanReveal(renderedRevealState, renderedInputValue);

  useLayoutEffect(() => {
    if (resetMarkerRef.current === resetKey) return;
    resetMarkerRef.current = resetKey;
    transferIntentRef.current = null;
    editIntentRef.current = null;
    updateRevealState(protectedPasswordState(inputRef.current?.value ?? value));
  }, [resetKey, value]);

  useLayoutEffect(() => {
    const current = revealRef.current;
    if (current.source === 'manual' && current.manualValue === value) return;
    if (current.source === 'empty' && !value) return;
    updateRevealState(protectedPasswordState(value));
  }, [value]);

  useEffect(() => {
    if (!revealed) return undefined;
    let frame = 0;
    const verify = () => {
      const inputValue = inputRef.current?.value ?? '';
      if (!passwordCanReveal(revealRef.current, inputValue)) {
        updateRevealState(protectedPasswordState(inputValue));
        return;
      }
      frame = window.requestAnimationFrame(verify);
    };
    frame = window.requestAnimationFrame(verify);
    return () => window.cancelAnimationFrame(frame);
  }, [revealed]);

  /** @param {import('./password-reveal-state.js').PasswordRevealState} next */
  const expirePending = (next) => {
    if (!next.pending) return;
    const pending = next.pending;
    window.setTimeout(() => {
      const current = revealRef.current;
      const expired = expirePasswordEdit(current, pending, inputRef.current?.value ?? value);
      if (expired !== current) updateRevealState(expired);
    }, 0);
  };

  useLayoutEffect(() => {
    const input = inputRef.current;
    if (!input) return undefined;
    /** @param {Event} event */
    const handleBeforeInput = (event) => {
      const nativeEvent = /** @type {InputEvent} */ (event);
      const transfer = transferIntentRef.current;
      const editIntent = editIntentRef.current;
      const next = beginPasswordEdit(revealRef.current, {
        trusted: nativeEvent.isTrusted === true,
        inputType: nativeEvent.inputType,
        intent: editIntent?.intent,
        data: nativeEvent.data,
        transferData: transfer
          && transfer.inputType === nativeEvent.inputType
          && transfer.selectionStart === input.selectionStart
          && transfer.selectionEnd === input.selectionEnd ? transfer.data : null,
        value: input.value,
        selectionStart: input.selectionStart,
        selectionEnd: input.selectionEnd,
      });
      if (editIntent?.intent !== 'composition') editIntentRef.current = null;
      transferIntentRef.current = null;
      updateRevealState(next);
      expirePending(next);
    };
    /** @param {Event} event */
    const handleInput = (event) => {
      const nativeEvent = /** @type {InputEvent} */ (event);
      const next = completePasswordEdit(revealRef.current, {
        trusted: nativeEvent.isTrusted === true,
        inputType: nativeEvent.inputType,
        data: nativeEvent.data,
        value: input.value,
        selectionStart: input.selectionStart,
        selectionEnd: input.selectionEnd,
      });
      onValueChange(input.value);
      // Do not render the mirror from the target-phase native listener: the
      // parent must first publish the same value to the controlled input.
      revealRef.current = next;
      window.queueMicrotask(() => {
        if (revealRef.current === next) setRevealState(next);
      });
    };
    input.addEventListener('beforeinput', handleBeforeInput);
    input.addEventListener('input', handleInput);
    return () => {
      input.removeEventListener('beforeinput', handleBeforeInput);
      input.removeEventListener('input', handleInput);
    };
  }, [value]);

  const revokePending = () => {
    if (revealRef.current.pending) protectCurrentValue();
  };
  /**
   * @param {import('./password-reveal-state.js').PasswordEditIntent} intent
   * @param {React.SyntheticEvent} event
   * @param {boolean} [persistent]
   */
  const armEditIntent = (intent, event, persistent = false) => {
    if (event.nativeEvent.isTrusted !== true) {
      editIntentRef.current = null;
      return;
    }
    const token = { intent };
    editIntentRef.current = token;
    if (!persistent) {
      window.setTimeout(() => {
        if (editIntentRef.current === token) editIntentRef.current = null;
      }, 0);
    }
  };

  return (
    <div
      className="ap-password-input relative"
      data-password-input
      data-password-revealed={revealed ? 'true' : 'false'}
      onBlur={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget)) {
          updateRevealState(concealPassword(revealRef.current, inputRef.current?.value ?? value, true));
          onBlur?.(event);
        }
      }}
    >
      <span
        data-password-reveal-output
        role="status"
        aria-live="polite"
        aria-atomic="true"
        aria-hidden={!revealed}
        hidden={!revealed}
        className={`pointer-events-none absolute inset-0 z-0 flex h-9 select-none items-center overflow-hidden whitespace-pre rounded-xl border px-3 pr-10 font-mono text-[13px] ${controlTheme}`}
      >
        {revealed ? renderedRevealState.manualValue : ''}
      </span>
      <input
        ref={inputRef}
        value={value}
        onChange={() => {}}
        onKeyDown={(event) => {
          revokePending();
          if (!isPasswordEditKey(event.key, event.ctrlKey, event.metaKey, event.altKey, event.getModifierState('AltGraph'))) {
            editIntentRef.current = null;
            return;
          }
          armEditIntent('keyboard', event);
        }}
        onKeyUp={() => {
          revokePending();
          if (editIntentRef.current?.intent === 'keyboard') editIntentRef.current = null;
        }}
        onCompositionStart={(event) => {
          revokePending();
          armEditIntent('composition', event, true);
        }}
        onCompositionEnd={() => {
          revokePending();
          if (editIntentRef.current?.intent === 'composition') editIntentRef.current = null;
        }}
        onPaste={(event) => {
          revokePending();
          armEditIntent('paste', event);
          const input = inputRef.current;
          if (!input) return;
          const intent = {
            inputType: 'insertFromPaste',
            data: event.clipboardData.getData('text/plain'),
            selectionStart: input.selectionStart,
            selectionEnd: input.selectionEnd,
          };
          transferIntentRef.current = intent;
          window.setTimeout(() => {
            if (transferIntentRef.current === intent) transferIntentRef.current = null;
          }, 0);
        }}
        onDrop={(event) => {
          revokePending();
          armEditIntent('drop', event);
          const input = inputRef.current;
          if (!input) return;
          const intent = {
            inputType: 'insertFromDrop',
            data: event.dataTransfer.getData('text/plain'),
            selectionStart: input.selectionStart,
            selectionEnd: input.selectionEnd,
          };
          transferIntentRef.current = intent;
          window.setTimeout(() => {
            if (transferIntentRef.current === intent) transferIntentRef.current = null;
          }, 0);
        }}
        onCut={(event) => {
          revokePending();
          armEditIntent('cut', event);
        }}
        disabled={disabled}
        placeholder={placeholder}
        aria-label={ariaLabel}
        type="password"
        autoComplete={autoComplete}
        style={revealed ? {
          backgroundColor: 'transparent',
          caretColor: 'currentColor',
          WebkitTextFillColor: 'transparent',
        } : undefined}
        className={`relative z-10 h-9 w-full rounded-xl border px-3 pr-10 font-mono text-[13px] outline-none ${controlTheme}`}
      />
      <button
        type="button"
        disabled={disabled || !passwordCanReveal(renderedRevealState, renderedInputValue)}
        hidden={!passwordCanReveal(renderedRevealState, renderedInputValue)}
        onMouseDown={(event) => event.preventDefault()}
        onClick={() => updateRevealState(togglePasswordReveal(revealRef.current, inputRef.current?.value ?? value))}
        aria-label={revealed ? hideLabel : showLabel}
        aria-pressed={revealed}
        title={revealed ? hideLabel : showLabel}
        className="absolute right-1 top-1/2 z-20 flex h-7 w-7 -translate-y-1/2 items-center justify-center rounded-lg text-[#6F7378] transition-colors hover:bg-black/5 disabled:cursor-not-allowed disabled:opacity-35 dark:hover:bg-white/10"
      >
        <PasswordVisibilityIcon visible={revealed} />
      </button>
    </div>
  );
}
