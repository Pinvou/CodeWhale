import React, { createContext, useContext } from 'react';

const Visibility = createContext(true);

export function PortalDialogScope({ active, children }) {
  const parentActive = useContext(Visibility);
  return <Visibility.Provider value={parentActive && active}>{children}</Visibility.Provider>;
}

export function usePortalDialogScope() { return useContext(Visibility); }
