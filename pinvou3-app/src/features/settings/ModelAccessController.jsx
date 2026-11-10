import { useEffect, useMemo } from 'react';
import { useModelAccess } from './useModelAccess.js';
import { AccountPlatformDesktopClient } from '../account-platform/desktop-client.js';

// Mount after the shell's first render. Identity/model reads and their refresh
// owner have one feature boundary shared by all account entry points.
export function ModelAccessController({ onChange, onClient, accountTransport, ...options }) {
  const client = useMemo(() => accountTransport ? new AccountPlatformDesktopClient(accountTransport) : null, [accountTransport]);
  useEffect(() => { onClient(client); }, [client, onClient]);
  const access = useModelAccess({ ...options, accountClient: client });
  useEffect(() => { onChange(access); }, [access, onChange]);
  return null;
}
