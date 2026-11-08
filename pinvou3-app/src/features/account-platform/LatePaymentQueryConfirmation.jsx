import React, { useId, useLayoutEffect, useRef } from 'react';
import { createPortal } from 'react-dom';

export function LatePaymentQueryConfirmation({ copy, caseID, newReview, onCancel, onConfirm }) {
  const titleID = useId();
  const descriptionID = useId();
  const dialog = useRef(null);
  const cancelButton = useRef(null);
  useLayoutEffect(() => {
    const previous = document.activeElement;
    const element = dialog.current;
    cancelButton.current?.focus();
    return () => {
      if (previous?.isConnected && element.contains(document.activeElement)) previous.focus();
    };
  }, []);
  const keyDown = event => {
    if (event.key === 'Escape') { event.preventDefault(); onCancel(); return; }
    if (event.key !== 'Tab') return;
    const buttons = [...dialog.current.querySelectorAll('button')];
    const last = buttons[buttons.length - 1];
    if (event.shiftKey && document.activeElement === buttons[0]) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); buttons[0].focus(); }
  };
  return createPortal(<div className="ap-order-close-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) onCancel(); }}>
    <div ref={dialog} className="ap-order-close-dialog" role="alertdialog" aria-modal="true" aria-labelledby={titleID} aria-describedby={descriptionID} onKeyDown={keyDown}>
      <h3 id={titleID}>{newReview ? copy.confirmNewQuery : copy.confirmQuery}</h3>
      <p id={descriptionID}>{copy.newQueryHelp}</p><code>{caseID}</code>
      <footer><button ref={cancelButton} onClick={onCancel}>{copy.cancelQuery}</button><button onClick={onConfirm}>{copy.confirmQueryAction}</button></footer>
    </div>
  </div>, document.body);
}
