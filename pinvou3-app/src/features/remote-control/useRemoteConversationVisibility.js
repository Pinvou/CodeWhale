import { useCallback, useState } from 'react';

// Every opening owns a distinct dialog. A disconnect started by an older
// render can close only that opening, even after Escape and a reopen.
export function useRemoteConversationVisibility() {
  const [owner, setOwner] = useState(null);
  const setOpen = useCallback(value => setOwner(value ? {} : null), []);
  const close = useCallback(() => {
    setOwner(current => current === owner ? null : current);
  }, [owner]);
  return { open: owner !== null, setOpen, close };
}
