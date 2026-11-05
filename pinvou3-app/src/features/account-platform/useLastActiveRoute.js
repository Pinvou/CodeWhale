import { useLayoutEffect, useState } from 'react';

// A retained owner continues to receive current authorization and navigation,
// while another page's query parameters cannot switch its hidden child views.
export function useLastActiveRoute(route, active) {
  const { search, section, tab, operationsTab } = route;
  const [saved, setSaved] = useState({ search, section, tab, operationsTab });
  useLayoutEffect(() => {
    if (active) setSaved(previous => previous.search === search && previous.section === section && previous.tab === tab && previous.operationsTab === operationsTab ? previous : { search, section, tab, operationsTab });
  }, [active, search, section, tab, operationsTab]);
  return active ? route : { ...route, ...saved };
}
