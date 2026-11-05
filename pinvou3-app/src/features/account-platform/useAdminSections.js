import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { adminRouteLocation, resolveAdminRoute, visibleAdminSections } from './admin-sections.js';

function modalOpen() {
  return [...document.querySelectorAll('[role="dialog"][aria-modal="true"], [role="alertdialog"][aria-modal="true"]')].some(node => !node.closest('[hidden]') && !node.querySelector('.adm-inline-message.success'));
}
export function useAdminSections({ me, flags, ready }) {
  const [search, setSearch] = useState(() => globalThis.location?.search || '');
  const accepted = useRef(globalThis.location?.href || '');
  const available = ready ? visibleAdminSections(me, flags) : [];
  const route = resolveAdminRoute(search, flags, available);
  const section = available.includes(route.section) ? route.section : route.section === 'overview' && !new URLSearchParams(search).has('section') ? available[0] : route.section;
  // A new workspace starts below the fixed top bar. Long pages otherwise retain
  // the previous page's scroll anchor and can place the new heading behind it.
  useLayoutEffect(() => { if (ready) globalThis.scrollTo?.(0, 0); }, [ready, section]);
  useEffect(() => {
    const changed = () => {
      if (modalOpen()) { globalThis.history.replaceState({}, '', accepted.current); return; }
      accepted.current = globalThis.location.href;
      setSearch(globalThis.location.search);
    };
    globalThis.addEventListener('popstate', changed);
    return () => globalThis.removeEventListener('popstate', changed);
  }, []);
  useEffect(() => {
    if (!ready || !section || !available.includes(section)) return;
    const target = adminRouteLocation(search, section, route.tab ? { finance_tab: route.tab } : {} , globalThis.location.pathname);
    if (target !== globalThis.location.pathname + globalThis.location.search) {
      globalThis.history.replaceState({}, '', target);
      accepted.current = globalThis.location.href; setSearch(globalThis.location.search);
    }
  }, [ready, section, search, route.tab]); // availability is checked against the current authorization above.
  const navigate = (target, updates = {}, mode = 'push') => {
    if (!available.includes(target) || modalOpen()) return;
    const url = adminRouteLocation(globalThis.location.search, target, updates, globalThis.location.pathname);
    globalThis.history[mode === 'replace' ? 'replaceState' : 'pushState']({}, '', url);
    accepted.current = globalThis.location.href; setSearch(globalThis.location.search);
  };
  return { ...route, section, search, available, navigate };
}
