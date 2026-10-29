import { usePortalDialogScope } from './portal-dialog-scope.jsx';
import { isImeComposing } from '../../shared/ime-guard.mjs';
import React, { useId, useLayoutEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';

// The account's typed consent does not depend on a native JavaScript prompt.
export function AccountClosureConfirmation({ copy, mismatch, onCancel, onConfirm, canRestoreFocus }) {
  const visible = usePortalDialogScope();
  const titleID = useId(), descriptionID = useId(), inputID = useId();
  const dialog = useRef(null), input = useRef(null), restoreAllowed = useRef(canRestoreFocus);
  const [text, setText] = useState('');
  restoreAllowed.current = canRestoreFocus;
  useLayoutEffect(() => {
    if (!visible) return undefined;
    const previous = document.activeElement, element = dialog.current;
    input.current?.focus();
    return () => { if (previous?.isConnected && element?.contains(document.activeElement) && !previous.closest('[hidden]') && restoreAllowed.current()) previous.focus(); };
  }, [visible]);
  const keyDown = event => {
    if ((event.key === 'Enter' || event.key === 'Escape') && isImeComposing(event)) {
      if (event.key === 'Enter') event.preventDefault();
      return;
    }
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); onCancel(); return; }
    if (event.key !== 'Tab') return;
    const controls = [...dialog.current.querySelectorAll('input:not(:disabled), button:not(:disabled)')], first = controls[0], last = controls[controls.length - 1];
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
  };
  if (!visible) return null;
  return createPortal(<div className="ap-closure-confirm-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) onCancel(); }}>
    <form className="ap-closure-confirm-dialog" ref={dialog} role="alertdialog" aria-modal="true" aria-labelledby={titleID} aria-describedby={descriptionID} onKeyDown={keyDown} onSubmit={event => { event.preventDefault(); onConfirm(text); }}>
      <h3 id={titleID}>{copy.title}</h3><p id={descriptionID}>{copy.confirmPrompt}</p>
      {mismatch && <p role="alert">{copy.confirmMismatch}</p>}
      <label htmlFor={inputID}>{copy.confirmInput}</label><input id={inputID} ref={input} value={text} onChange={event => setText(event.target.value)} autoComplete="off" autoCapitalize="off" spellCheck={false} />
      <footer><button type="button" onClick={onCancel}>{copy.cancelConfirmation}</button><button type="submit">{copy.confirmAction}</button></footer>
    </form>
  </div>, document.body);
}
