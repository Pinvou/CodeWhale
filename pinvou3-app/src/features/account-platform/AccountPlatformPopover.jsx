import React, { useLayoutEffect, useRef } from 'react';
import { createPortal } from 'react-dom';
import { AccountPlatformView } from './AccountPlatformView.jsx';
import './desktop-account.css';

export function AccountPlatformPopover({ open, onClose, theme = 'light', presentation, ...props }) {
  const dialog = useRef(null);
  useLayoutEffect(() => {
    if (!open) return undefined;
    const previous = document.activeElement;
    const isolated = [];
    // Isolate the existing app branch, while later body portals (captcha and
    // payment dialogs) remain interactive. Restore each previous host state.
    let branch = dialog.current?.parentElement;
    while (branch && branch !== document.body) {
      for (const sibling of branch.parentElement.children) {
        if (sibling === branch || !(sibling instanceof HTMLElement)) continue;
        isolated.push({ element: sibling, inert: sibling.inert, hadInert: sibling.hasAttribute('inert'), ariaHidden: sibling.getAttribute('aria-hidden') });
        sibling.inert = true; sibling.setAttribute('aria-hidden', 'true');
      }
      branch = branch.parentElement;
    }
    (dialog.current?.querySelector('button') || dialog.current)?.focus();
    return () => {
      for (const { element, inert, hadInert, ariaHidden } of isolated) {
        element.inert = inert; if (!hadInert) element.removeAttribute('inert');
        if (ariaHidden === null) element.removeAttribute('aria-hidden'); else element.setAttribute('aria-hidden', ariaHidden);
      }
      if (previous?.isConnected && !previous.closest('[hidden]') && !previous.inert) previous.focus();
    };
  }, [open]);
  const keydown = event => {
    if (event.isComposing || event.keyCode === 229 || !dialog.current?.contains(event.target)) return;
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); onClose?.(); return; }
    if (event.key !== 'Tab') return;
    const controls = [...dialog.current.querySelectorAll('button:not(:disabled), input:not(:disabled), select:not(:disabled), a[href], [tabindex="0"]')].filter(node => !node.closest('[hidden]') && node.getClientRects().length);
    const first = controls[0], last = controls[controls.length - 1];
    if (!first) { event.preventDefault(); dialog.current.focus(); }
    else if (event.shiftKey && (document.activeElement === first || document.activeElement === dialog.current)) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
  };
  if (!open) return null;
  const content = <div className={`ap-popover-backdrop ap-desktop-backdrop${presentation === 'screen' ? ' ap-account-screen' : ''}`} data-theme={theme}>
    <div className="ap-account-dialog" ref={dialog} role="dialog" aria-modal="true" aria-label={props.copy.title} tabIndex={-1} onKeyDown={keydown}>
      {onClose && <button type="button" className="ap-account-close" aria-label={props.copy.close} onClick={onClose}>×</button>}
      <AccountPlatformView {...props} onClose={onClose} />
    </div>
  </div>;
  return presentation === 'screen' ? content : createPortal(content, document.body);
}
