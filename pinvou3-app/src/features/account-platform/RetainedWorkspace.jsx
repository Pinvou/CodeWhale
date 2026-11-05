import React, { useEffect, useState } from 'react';
import { PortalDialogScope } from './portal-dialog-scope.jsx';

// A finite, permission-filtered set of visited views shares the existing owners.
// Hidden views keep accepted commands alive; actor/session changes reset the parent.
export function RetainedWorkspace({ active, available, children }) {
  const [visited, setVisited] = useState([]);
  const signature = available.join(',');
  const current = visited.filter(key => available.includes(key));
  if (available.includes(active) && !current.includes(active)) current.push(active);
  useEffect(() => {
    setVisited(previous => {
      const next = previous.filter(key => available.includes(key));
      if (available.includes(active) && !next.includes(active)) next.push(active);
      return next.join(',') === previous.join(',') ? previous : next;
    });
  }, [active, signature]);
  return current.map(key => <div key={key} hidden={active !== key} data-workspace={key}><PortalDialogScope active={active === key}>{children(key)}</PortalDialogScope></div>);
}

export function ActiveWorkspace({ active, children }) {
  return <>{children(active)}</>;
}
