import { createContext, useContext, useEffect, useMemo, useState } from 'react';
import { createAdminMutationOwner } from './admin-operation-scope.js';

export const AdminMutationContext = createContext(null);

export function useAdminMutationOwner(area) {
  const shared = useContext(AdminMutationContext);
  const fallback = useMemo(() => createAdminMutationOwner(), []);
  const owner = shared || fallback;
  const [busy, setBusy] = useState(() => owner.isBusy(area));
  const [completed, setCompleted] = useState(0);
  useEffect(() => {
    setBusy(owner.isBusy(area));
    return owner.subscribe(event => {
      if (event.area !== area) return;
      setBusy(owner.isBusy(area));
      if (event.completed) setCompleted(value => value + 1);
    });
  }, [owner, area]);
  return { owner, busy, completed, begin: () => owner.begin(area), finish: ticket => owner.finish(area, ticket) };
}
