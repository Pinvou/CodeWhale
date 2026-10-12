import React, { useLayoutEffect, useId, useRef } from 'react';
import { createPortal } from 'react-dom';

// Feature confirmation is independent of the native async window.confirm shim.
export function PersonalOrderCloseConfirmation({ copy, orderID, amount, canRestoreFocus, onCancel, onConfirm }) {
  const titleID = useId();
  const descriptionID = useId();
  const dialog = useRef(null);
  const cancelButton = useRef(null);
  const restoreAllowed = useRef(canRestoreFocus);
  restoreAllowed.current = canRestoreFocus;
  useLayoutEffect(() => {
    const previous = document.activeElement;
    const element = dialog.current;
    cancelButton.current?.focus();
    return () => {
      if (previous?.isConnected && element.contains(document.activeElement) && restoreAllowed.current()) previous.focus();
    };
  }, []);
  const keyDown = event => {
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); onCancel(); return; }
    if (event.key !== 'Tab') return;
    const buttons = [...dialog.current.querySelectorAll('button:not(:disabled)')];
    const first = buttons[0]; const last = buttons[buttons.length - 1];
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
  };
  return createPortal(<div className="ap-order-close-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) onCancel(); }}>
    <div className="ap-order-close-dialog" ref={dialog} role="alertdialog" aria-modal="true" aria-labelledby={titleID} aria-describedby={descriptionID} onKeyDown={keyDown}>
      <h3 id={titleID}>{copy.close}</h3><p id={descriptionID}>{copy.confirmClose}</p>
      <dl><dt>{copy.orderID}</dt><dd>{orderID}</dd><dt>{copy.amount}</dt><dd>{amount}</dd></dl>
      <footer><button ref={cancelButton} onClick={onCancel}>{copy.cancelClose}</button><button onClick={onConfirm}>{copy.close}</button></footer>
    </div>
  </div>, document.body);
}
