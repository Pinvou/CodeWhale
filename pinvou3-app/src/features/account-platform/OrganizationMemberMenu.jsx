import React, { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { usePortalDialogScope } from './portal-dialog-scope.jsx';

const dialogOpen = () => Boolean(document.querySelector('[role=dialog], [role=alertdialog]'));
const controls = 'button:not(:disabled), a[href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), summary, [tabindex="0"]';
const focusable = node => node.tabIndex >= 0 && !node.closest('[hidden]') && node.getClientRects().length && getComputedStyle(node).visibility !== 'hidden';

// The body portal avoids transformed/clipped table ancestors. Action instances
// remain mounted while hidden, retaining accepted operations and their dialogs.
export function OrganizationMemberMenu({ label, targetLabel, children }) {
  const details = useRef(null), summary = useRef(null), menu = useRef(null), focusOnOpen = useRef(false);
  const [open, setOpen] = useState(false), [placement, setPlacement] = useState(null);
  const visible = usePortalDialogScope(), menuID = useId();
  const close = useCallback(() => {
    if (details.current) details.current.open = false;
    focusOnOpen.current = false; setOpen(false); setPlacement(null);
  }, []);
  const place = useCallback(() => {
    if (!details.current?.open || !summary.current || !menu.current) return;
    const anchor = summary.current.getBoundingClientRect();
    const width = Math.min(224, Math.max(0, window.innerWidth - 16));
    menu.current.style.width = `${width}px`;
    const height = menu.current.getBoundingClientRect().height;
    const left = Math.max(8, Math.min(anchor.right - width, window.innerWidth - width - 8));
    const below = anchor.bottom + 6;
    const proposedTop = below + height <= window.innerHeight - 8 ? below : anchor.top - height - 6;
    const top = Math.max(8, Math.min(proposedTop, window.innerHeight - height - 8));
    setPlacement({ left, top, width });
  }, []);
  const closeAndRestore = useCallback(() => {
    const focused = menu.current?.contains(document.activeElement);
    close();
    if (focused) summary.current?.focus({ preventScroll: true });
  }, [close]);
  const toggle = () => {
    if (details.current?.open) { focusOnOpen.current = true; setOpen(true); place(); }
    else close();
  };
  useLayoutEffect(() => { if (open) place(); }, [open, label, targetLabel, children, place]);
  useLayoutEffect(() => {
    if (visible && open && placement && focusOnOpen.current) {
      focusOnOpen.current = false;
      [...menu.current.querySelectorAll(controls)].find(focusable)?.focus({ preventScroll: true });
    }
  }, [visible, open, placement]);
  useEffect(() => { if (!visible) close(); }, [visible, close]);
  useEffect(() => {
    if (!open) return undefined;
    const outside = event => {
      if (!dialogOpen() && !details.current?.contains(event.target) && !menu.current?.contains(event.target)) closeAndRestore();
    };
    const key = event => {
      if (event.key !== 'Escape' || event.isComposing || event.keyCode === 229 || dialogOpen()) return;
      event.preventDefault(); event.stopPropagation(); close(); summary.current?.focus({ preventScroll: true });
    };
    const moved = event => {
      if (dialogOpen()) { place(); return; }
      if (event.target instanceof Node && menu.current?.contains(event.target)) return;
      closeAndRestore();
    };
    document.addEventListener('pointerdown', outside);
    window.addEventListener('keydown', key);
    window.addEventListener('resize', moved);
    window.addEventListener('scroll', moved, true);
    return () => {
      document.removeEventListener('pointerdown', outside);
      window.removeEventListener('keydown', key);
      window.removeEventListener('resize', moved);
      window.removeEventListener('scroll', moved, true);
    };
  }, [open, close, closeAndRestore, place]);
  const menuKey = event => {
    if (event.key !== 'Tab' || dialogOpen() || !menu.current?.contains(event.target)) return;
    const items = [...menu.current.querySelectorAll(controls)].filter(focusable);
    if (event.shiftKey && document.activeElement === items[0]) {
      event.preventDefault(); close(); summary.current?.focus({ preventScroll: true });
    } else if (!event.shiftKey && document.activeElement === items[items.length - 1]) {
      const pageItems = [...document.querySelectorAll(controls)].filter(node => !node.closest('.adm-member-menu-portal') && focusable(node));
      const next = pageItems[pageItems.indexOf(summary.current) + 1];
      event.preventDefault(); close(); (next || summary.current)?.focus();
    }
  };
  return <details ref={details} className="adm-member-actions" onToggle={toggle}>
    <summary ref={summary} aria-controls={menuID} aria-label={`${targetLabel} · ${label}`} onKeyDown={event => {
      if (event.key === 'Tab' && !event.shiftKey && open) {
        event.preventDefault(); [...menu.current.querySelectorAll(controls)].find(focusable)?.focus();
      }
    }}>{label}</summary>
    {createPortal(<div className="adm-shell adm-restored-shell adm-member-menu-portal" data-open={visible && open}>
      <div ref={menu} id={menuID} className="adm-member-action-menu" role="group" aria-label={`${targetLabel} · ${label}`} style={visible && open && placement ? placement : { visibility: 'hidden' }} onKeyDown={menuKey}>{children}</div>
    </div>, document.body)}
  </details>;
}
