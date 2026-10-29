import React, { useId, useLayoutEffect, useRef } from 'react';
import { createPortal } from 'react-dom';
import './portal-dialog.css';
import { usePortalDialogScope } from './portal-dialog-scope.jsx';

export function PortalDialog({ title, closeLabel, onClose, children, busy = false, active = true, drawer = false }) {
  const scopeActive = usePortalDialogScope();
  const visible = scopeActive && active;
  const titleID = useId(), dialog = useRef(null), close = useRef(null);
  useLayoutEffect(() => {
    if (!visible) return undefined;
    const previous = document.activeElement;
    (close.current?.disabled ? dialog.current : close.current)?.focus();
    return () => {
      if (previous?.isConnected && !previous.closest('[hidden]')) previous.focus();
    };
  }, [visible]);
  useLayoutEffect(() => { if (visible && busy && dialog.current && (!dialog.current.contains(document.activeElement) || document.activeElement?.disabled)) dialog.current.focus(); });
  const keydown = event => {
    if (event.isComposing || event.keyCode === 229) return;
    if (event.key === 'Escape') {
      event.preventDefault(); event.stopPropagation();
      if (!busy) onClose();
    }
    if (event.key !== 'Tab') return;
    const controls = [...dialog.current.querySelectorAll('button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), a[href], [tabindex="0"]')].filter(element => !element.closest('[hidden]') && element.getClientRects().length);
    const first = controls[0], last = controls[controls.length - 1];
    if (!first) { event.preventDefault(); dialog.current.focus(); }
    else if (document.activeElement === dialog.current) { event.preventDefault(); (event.shiftKey ? last : first).focus(); }
    else if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
  };
  if (!visible) return null;
  return createPortal(<div className={`ap-restored-dialog-backdrop${drawer ? ' ap-restored-drawer-backdrop' : ''}`} onMouseDown={event => { if (!busy && event.currentTarget === event.target) onClose(); }}>
    <div ref={dialog} className={`ap-restored-dialog${drawer ? ' ap-restored-drawer' : ''}`} role="dialog" data-portal-location={`${globalThis.location.pathname}${globalThis.location.search}${globalThis.location.hash}`} aria-modal="true" aria-labelledby={titleID} tabIndex={-1} onKeyDown={keydown}>
      <header><h2 id={titleID}>{title}</h2><button type="button" ref={close} disabled={busy} onClick={onClose} aria-label={closeLabel}>×</button></header>{children}
    </div>
  </div>, document.body);
}
