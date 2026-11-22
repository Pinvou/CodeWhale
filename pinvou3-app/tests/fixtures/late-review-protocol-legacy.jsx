import React, { useEffect } from 'react';
import { createRoot } from 'react-dom/client';
import { AdminPaymentReviewQueue } from '../../src/features/account-platform/AdminPaymentReviewQueue.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { useFinanceOperation } from '../../src/features/account-platform/useFinanceOperation.js';
import { financeCopy } from '../../src/shared/i18n.js';

// A real hook retains a controlled, pre-protocol unknown invocation. Its first
// ambiguous action is controlled; recovery uses the shipped queue and client.
export function beginLegacyReviewProtocolProbe(action) {
  const id = window.f02.id;
  const container = document.createElement('div'); container.dataset.legacyReviewProtocol = action; document.body.append(container);
  const root = createRoot(container); const client = new AdminPlatformClient('');
  const copy = financeCopy.en;
  const me = { user_id: id, platform_authorization_version: 1, platform_permissions: ['platform.billing.orders.sync'] };
  const legacy = { action, id, reason: 'controlled original legacy reason', nextReview: '2099-01-02T12:34:00.000Z', resolution: '' };
  const probe = { originalKey: '', recoveryArguments: [], writes: [], legacyHasMarker: Object.hasOwn(legacy, 'commandProtocol') };
  window.legacyReviewProtocolProbe = probe;
  const method = action === 'schedule' ? 'scheduleLatePaymentReview' : 'archiveLatePaymentReview';
  const originalMethod = client[method].bind(client);
  client[method] = (...args) => { probe.recoveryArguments.push(args); return originalMethod(...args); };
  const originalFetch = window.fetch;
  window.fetch = async (url, options = {}) => {
    if (options.method === 'POST' && /\/(schedule|archive|archive-command)$/.test(String(url))) probe.writes.push(String(url));
    return originalFetch(url, options);
  };
  function SeedLegacyUnknown() {
    const operation = useFinanceOperation(`${id}:1`, copy, client, 'late-review');
    useEffect(() => {
      void operation.mutate(legacy, async (_facts, key) => {
        probe.originalKey = key;
        throw new TypeError('controlled pre-protocol ambiguous dispatch');
      }).then(() => {
        root.render(<AdminPaymentReviewQueue client={client} me={me} copy={copy} kind="late" />);
      });
    }, []); // eslint-disable-line react-hooks/exhaustive-deps
    return <p>controlled legacy invocation</p>;
  }
  root.render(<SeedLegacyUnknown />);
}
