import React, { useLayoutEffect, useId, useRef } from 'react';
import { createPortal } from 'react-dom';

// Feature-local confirmation does not depend on native dialog commands.
export function OrganizationActionConfirmation({ copy, title, target, details, onCancel, onConfirm, canRestoreFocus }) {
 const titleID = useId(), descriptionID = useId(), dialog = useRef(null), cancelButton = useRef(null), restoreAllowed = useRef(canRestoreFocus);
 restoreAllowed.current = canRestoreFocus;
 useLayoutEffect(() => {
  const previous = document.activeElement, element = dialog.current;
  cancelButton.current?.focus();
  return () => { if (previous?.isConnected && element.contains(document.activeElement) && restoreAllowed.current()) previous.focus(); };
 }, []);
 const keyDown = event => {
  if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); onCancel(); return; }
  if (event.key !== 'Tab') return;
  const buttons = [...dialog.current.querySelectorAll('button:not(:disabled)')], first = buttons[0], last = buttons[buttons.length - 1];
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
 };
 return createPortal(<div className="ap-org-confirm-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) onCancel(); }}>
  <div className="ap-org-confirm-dialog" ref={dialog} role="alertdialog" aria-modal="true" aria-labelledby={titleID} aria-describedby={descriptionID} onKeyDown={keyDown}>
   <h3 id={titleID}>{title}</h3><p id={descriptionID}>{copy.confirm}</p>
   <dl>{target && <><dt>{copy.confirmationTarget}</dt><dd>{target}</dd></>}{details.map(item => <React.Fragment key={item.name}><dt>{item.label}</dt><dd>{item.value}</dd></React.Fragment>)}</dl>
   <footer><button type="button" ref={cancelButton} onClick={onCancel}>{copy.cancelConfirmation}</button><button type="button" onClick={onConfirm}>{copy.confirmAction}</button></footer>
  </div>
 </div>, document.body);
}
