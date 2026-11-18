import { useCallback, useState } from 'react';

// Every opening owns a distinct dialog. A disconnect started by an older
// render can close only that opening, even after Escape and a reopen.
export function useRemoteConversationVisibility() {
  const [owner, setOwner] = useState(false);
  const setOpen = useCallback(value => setOwner(value && {}), []);
  return [Boolean(owner), setOpen,
    () => setOwner(current => current === owner ? false : current)];
}
