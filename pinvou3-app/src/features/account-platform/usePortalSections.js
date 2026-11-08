import { useEffect, useState } from 'react';
import { availablePortalSection, personalPortalSections, portalSectionLocation } from './portal-sections.js';

// Resolve only after the current transport's authoritative identity is loaded.
// URL changes do not retire the mounted owners of pending financial operations.
export function usePortalSections({ enabled, ready, me }) {
  const [search, setSearch] = useState(() => globalThis.location?.search || '');
  const sections = ready ? personalPortalSections(me) : [];
  const signature = sections.join(',');
  const section = enabled && ready ? availablePortalSection(search, sections) : '';
  useEffect(() => {
    if (!enabled) return undefined;
    const changed = () => {
      const incoming = globalThis.location?.search || '';
      const modal = globalThis.document?.querySelector('[role=dialog], [role=alertdialog]');
      if (ready && modal && availablePortalSection(incoming, sections) !== section) {
        const target = modal.getAttribute('data-portal-location') || portalSectionLocation(section, incoming, globalThis.location.pathname, true);
        globalThis.history.replaceState({}, '', target);
        return;
      }
      setSearch(incoming);
    };
    globalThis.addEventListener('popstate', changed);
    return () => globalThis.removeEventListener('popstate', changed);
  }, [enabled, ready, section, signature]);
  useEffect(() => {
    if (!enabled || !ready || !section) return;
    const url = portalSectionLocation(section, globalThis.location.search, globalThis.location.pathname, true);
    if (url !== globalThis.location.pathname + globalThis.location.search) globalThis.history.replaceState({}, '', url);
    setSearch(globalThis.location.search);
  }, [enabled, ready, section, signature, search]);
  const navigate = target => {
    if (!enabled || !ready || !sections.includes(target) || target !== section && globalThis.document?.querySelector('[role=dialog], [role=alertdialog]')) return;
    globalThis.history.pushState({}, '', portalSectionLocation(target, globalThis.location.search, globalThis.location.pathname));
    setSearch(globalThis.location.search);
  };
  return { section, sections, navigate };
}
