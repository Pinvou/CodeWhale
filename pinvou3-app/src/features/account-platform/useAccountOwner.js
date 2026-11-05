import { useEffect, useMemo, useRef } from 'react';

// A client/identity change retires both outstanding reads and mutation replies.
export function useAccountOwner(identity) {
  const owner = useRef({ identity, active: true, generation: 0 });
  if (owner.current.identity !== identity) owner.current = { identity, active: true, generation: owner.current.generation + 1 };
  useEffect(() => {
    const current = owner.current;
    current.active = true;
    return () => { current.active = false; current.generation++; };
  }, [identity]);
  return useMemo(() => ({
    capture: () => ({ owner: owner.current, generation: owner.current.generation }),
    current: token => token.owner === owner.current && token.owner.active && token.generation === token.owner.generation,
    invalidate: () => { owner.current.generation++; },
  }), []);
}
