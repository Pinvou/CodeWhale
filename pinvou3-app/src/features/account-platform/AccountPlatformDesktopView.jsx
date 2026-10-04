import React, { useMemo } from 'react';
import { AccountPlatformDesktopClient } from './desktop-client.js';
import { AccountPlatformView } from './AccountPlatformView.jsx';

/** Keep native client setup in the same lazy boundary as its account view. */
export function AccountPlatformDesktopView({ transport, ...props }) {
  const client = useMemo(() => new AccountPlatformDesktopClient(transport), [transport]);
  return <AccountPlatformView {...props} client={client} />;
}
