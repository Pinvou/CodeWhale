import React, { useEffect } from 'react';
import { createRoot } from 'react-dom/client';
import { AdminFinanceWorkspace } from '../../src/features/account-platform/AdminFinanceWorkspace.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { useFinanceOperation } from '../../src/features/account-platform/useFinanceOperation.js';
import { financeCopy } from '../../src/shared/i18n.js';

export function beginLegacyDirectRepairProbe() {
  const id = window.f02.id;
  const container = document.createElement('div'); container.dataset.legacyDirectRepair = ''; document.body.append(container);
  const root = createRoot(container); const client = new AdminPlatformClient('');
  const copy = financeCopy.en;
  const me = { user_id: id, active_context: { platform_authorization_version: 1 }, platform_permissions: ['platform.billing.wallets.read', 'platform.billing.repairs.request'] };
  const legacy = Object.freeze({ action: 'repair', orderID: id, reason: 'controlled original legacy repair' });
  const probe = { originalKey: '', recoveryArguments: [], writes: [], legacyHasMarker: Object.hasOwn(legacy, 'commandProtocol') };
  window.legacyDirectRepairProbe = probe;
  const originalRepair = client.repairPaymentOrder.bind(client);
  client.repairPaymentOrder = (...args) => { probe.recoveryArguments.push(args); return originalRepair(...args); };
  const originalFetch = window.fetch;
  window.fetch = async (url, options = {}) => {
    if (options.method === 'POST' && String(url).endsWith('/repair')) probe.writes.push(String(url));
    return originalFetch(url, options);
  };
  function SeedLegacyUnknown() {
    const operation = useFinanceOperation(`${id}:1`, copy, client, 'payments');
    useEffect(() => {
      void operation.mutate(legacy, async (_facts, key) => {
        probe.originalKey = key;
        throw new TypeError('controlled pre-protocol ambiguous repair');
      }).then(() => root.render(<AdminFinanceWorkspace client={client} me={me} language="en" />));
    }, []); // eslint-disable-line react-hooks/exhaustive-deps
    return <p>controlled legacy invocation</p>;
  }
  root.render(<SeedLegacyUnknown />);
}
