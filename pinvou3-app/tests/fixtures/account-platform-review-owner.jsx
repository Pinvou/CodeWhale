import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminPaymentReviewQueue } from '../../src/features/account-platform/AdminPaymentReviewQueue.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { financeCopy } from '../../src/shared/i18n.js';

const params = new URLSearchParams(location.search);
const id = '11111111-1111-4111-8111-111111111111';
const user = '22222222-2222-4222-8222-222222222222';
const other = '33333333-3333-4333-8333-333333333333';
const language = params.get('language') || 'en';
const anomaly = params.get('anomaly') === 'true';
const state = { calls: [], owner: params.get('owner') === 'self' ? user : params.get('owner') === 'none' ? null : other };
const item = () => ({ case_id: id, payment_order_id: id, owner_id: state.owner, status: 'OPEN',
  anomaly_type: 'PAYMENT_FACT_CONFLICT', review_status: 'REVIEW_REQUIRED_LATE_PAYMENT', severity: 'P2' });
globalThis.fetch = async (url, options = {}) => {
  const method = options.method || 'GET'; state.calls.push({ url, method });
  const respond = (value, status = 200) => new Response(JSON.stringify(value), { status });
  if (url.endsWith('/claim')) {
    if (params.get('claim') === 'transport') throw new TypeError('Controlled lost claim response');
    if (params.get('claim') === 'conflict') { state.owner = other; return respond({ code: 'BILLING_CONFLICT' }, 409); }
    state.owner = user; return respond(item());
  }
  if (url.includes('?')) return respond({ items: [item()] });
  return respond({ case: item(), events: [] });
};
const client = new AdminPlatformClient(); client.csrfToken = 'PUBLIC_SYNTHETIC_CSRF';
window.reviewOwnerProbe = { ...state, copy: financeCopy[language], calls: state.calls };
createRoot(document.querySelector('#root')).render(<AdminPaymentReviewQueue client={client} copy={financeCopy[language]}
  language={language} anomaly={anomaly} me={{ user_id: user, platform_permissions: ['platform.billing.wallets.read', 'platform.billing.orders.sync', 'platform.billing.repairs.request'] }} />);
