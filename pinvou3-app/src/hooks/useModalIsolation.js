import { useEffect } from 'react';

const focusableSelector = [
  '[data-modal-initial-focus="true"]',
  'button:not([disabled])',
  'a[href]',
  'input:not([disabled]):not([type="hidden"])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',');

function focusableElements(container) {
  return Array.from(container.querySelectorAll(focusableSelector)).filter((element) => {
    if (!(element instanceof HTMLElement) || element.hidden) return false;
    const style = window.getComputedStyle(element);
    return style.display !== 'none' && style.visibility !== 'hidden';
  });
}

function focusFirst(container) {
  const elements = focusableElements(container);
  const preferred = elements.find((element) => element.dataset.modalInitialFocus === 'true');
  (preferred || elements[0] || container).focus({ preventScroll: true });
}

export function isolateModalBackground(container) {
  const isolated = new Map();

  const isolate = (element) => {
    if (!(element instanceof HTMLElement) || element.contains(container) || isolated.has(element)) return;
    isolated.set(element, {
      ariaHidden: element.getAttribute('aria-hidden'),
      hadInert: element.hasAttribute('inert'),
      inert: element.inert,
    });
    element.inert = true;
    element.setAttribute('inert', '');
    element.setAttribute('aria-hidden', 'true');
  };

  const isolateBackground = () => {
    // Walk from the modal to body and isolate the siblings of the active
    // branch at every level. Looking only at the modal's direct siblings (or
    // only at body children) misses app-shell peers such as TitleBar, sidebar,
    // and RightDock when the React root itself contains the gate.
    let branch = container;
    while (branch && branch !== document.body) {
      const parent = branch.parentElement;
      if (!parent) break;
      Array.from(parent.children).forEach((sibling) => {
        if (sibling !== branch) isolate(sibling);
      });
      branch = parent;
    }
  };

  isolateBackground();
  const observer = new MutationObserver(isolateBackground);
  observer.observe(document.body, { childList: true, subtree: true });

  return () => {
    observer.disconnect();
    isolated.forEach((previous, element) => {
      if (previous.ariaHidden === null) element.removeAttribute('aria-hidden');
      else element.setAttribute('aria-hidden', previous.ariaHidden);
      element.inert = previous.inert;
      if (!previous.hadInert) element.removeAttribute('inert');
    });
  };
}

// A full-window gate is still a modal from the accessibility tree's point of
// view. Isolate every sibling (including body portals), trap keyboard focus,
// and restore the invoking focus when the gate leaves the tree.
export function useModalIsolation(open, containerRef) {
  useEffect(() => {
    if (!open || !containerRef.current) return undefined;

    const container = containerRef.current;
    const previouslyFocused = document.activeElement instanceof HTMLElement
      ? document.activeElement
      : null;

    const handleKeyDown = (event) => {
      if (event.key !== 'Tab') return;
      const elements = focusableElements(container);
      if (elements.length === 0) {
        event.preventDefault();
        container.focus({ preventScroll: true });
        return;
      }
      const first = elements[0];
      const last = elements[elements.length - 1];
      const active = document.activeElement;
      if (event.shiftKey && (active === first || !container.contains(active))) {
        event.preventDefault();
        last.focus({ preventScroll: true });
      } else if (!event.shiftKey && (active === last || !container.contains(active))) {
        event.preventDefault();
        first.focus({ preventScroll: true });
      }
    };

    const restoreBackground = isolateModalBackground(container);

    const focusNewContent = () => {
      if (!container.contains(document.activeElement)) focusFirst(container);
    };
    const contentObserver = new MutationObserver(focusNewContent);
    contentObserver.observe(container, { childList: true, subtree: true });

    document.addEventListener('keydown', handleKeyDown, true);
    const frame = window.requestAnimationFrame(() => focusFirst(container));

    return () => {
      window.cancelAnimationFrame(frame);
      restoreBackground();
      contentObserver.disconnect();
      document.removeEventListener('keydown', handleKeyDown, true);
      if (previouslyFocused?.isConnected && !previouslyFocused.inert) {
        previouslyFocused.focus({ preventScroll: true });
      }
    };
  }, [containerRef, open]);
}
