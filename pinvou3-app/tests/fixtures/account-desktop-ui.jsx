import React from 'react';
import { createRoot } from 'react-dom/client';
import '../../src/styles/tailwind.css';
import { AccountPlatformPopover } from '../../src/features/account-platform/AccountPlatformPopover.jsx';
import { accountPlatformCopy } from '../../src/shared/i18n.js';

const query = new URLSearchParams(location.search);
const id = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const state = window.accountFixture = { language: query.get('language') || 'zh', open: true, authenticated: !query.has('guest'), organization: query.has('organization'), calls: [], contextVersion: 1, order: null, portalFail: false, paid: false, pending: query.has('pending') };
const personal = ['personal.context.switch', 'personal.profile.update', 'personal.invitations.read', 'personal.invitations.accept', 'personal.account.close.read', 'personal.billing.read', 'personal.recharge.create', 'personal.recharge.manage'];
const orgPermissions = ['org.entitlements.purchase', 'org.wallet.read', 'org.quota.read.self', 'org.quota.allocate'];
const profile = () => ({ user_id: id(1), username: 'sample-user', mobile: '+8613800000000', status: 'ACTIVE', personal_permissions: query.has('no-permission') ? [] : personal, platform_permissions: [], active_context: { context_type: state.organization ? 'ORGANIZATION' : 'PERSONAL', organization_id: state.organization ? id(2) : null, context_version: state.contextVersion, membership_version: 1, organization_authorization_version: 1, permissions: state.organization && !query.has('no-permission') ? orgPermissions : [] } });
const product = { product_id: id(3), plan_name: 'Sample top-up', amount_fen: 1234, channels: ['ALIPAY'], availability_status: 'AVAILABLE' };
const order = () => ({ order_id: id(4), status: state.paid ? 'PAID' : 'PENDING', fulfillment_status: state.paid ? 'CREDITED' : 'NONE', amount_fen: 1234, channel: 'ALIPAY', close_status: 'OPEN', expires_at: new Date(Date.now() + 120000).toISOString(), created_at: new Date().toISOString(), qr_data: 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aRHcAAAAASUVORK5CYII=', qr_scan_apps: ['ALIPAY'] });
const client = {
  financeSessionEpoch: 0,
  session: async () => ({ authenticated: state.authenticated }),
  bootstrap: async () => profile(),
  balance: async () => { state.calls.push('balance'); return { available_microcredits: state.paid ? 250000 : 100000, reserved_microcredits: 0 }; },
  registrationStatus: async () => null,
  login: async () => { state.calls.push('login'); state.authenticated = true; },
  logout: async () => { state.calls.push('logout'); state.authenticated = false; },
  openPortal: async language => { state.calls.push(`portal:${language}`); if (state.portalFail) throw new Error('UNAVAILABLE'); },
  products: async () => { state.calls.push('products'); return { items: query.has('empty-products') ? [] : [product] }; },
  paymentCapabilities: async () => ({ channels: ['ALIPAY'] }),
  rechargeOrders: async () => { state.calls.push('orders'); return { items: state.order || state.pending ? [order()] : [] }; },
  rechargeOrder: async () => order(),
  createRecharge: async (...args) => { state.calls.push(['create', ...args]); state.order = order(); return state.order; },
  request: async (area, resource, options) => {
    state.calls.push(resource);
    if (resource === '/v1/me/organizations') return { items: [{ organization_id: id(2), name: 'Sample organization', version: 1, status: 'ACTIVE' }] };
    if (resource === '/v1/sessions/current/active-context') { state.organization = Boolean(options.body.organization_id); state.contextVersion++; return profile().active_context; }
    if (resource.endsWith('/balance')) return { owner_type: 'ORGANIZATION', owner_id: id(2), wallet_id: id(5), posted_microcredits: 100000, available_microcredits: 100000, reserved_microcredits: 0 };
    if (resource.endsWith('/recharge/products')) return { items: [product] };
    if (resource.endsWith('/payment-capabilities')) return { channels: ['ALIPAY'], test_only: true, formal_collection_admitted: false };
    if (resource.endsWith('/recharge/orders') && options.method === 'POST') { state.calls.push(['org-create', options.body]); return { ...order(), product_id: id(3), beneficiary_organization_id: id(2) }; }
    if (resource.endsWith(`/recharge/orders/${id(4)}`)) return { ...order(), product_id: id(3), beneficiary_organization_id: id(2) };
    throw new Error(`Unexpected fixture request: ${area} ${resource}`);
  },
};
const root = createRoot(document.getElementById('root'));
state.render = () => root.render(<><button id="background" onClick={() => { state.open = true; state.render(); }}>Open account</button><AccountPlatformPopover open={state.open} client={client} copy={accountPlatformCopy[state.language]} language={state.language} onLanguageChange={value => { state.language = value; state.render(); }} theme={query.get('theme') || 'light'} presentation={query.get('presentation')} onClose={() => { state.open = false; state.render(); }} /></>);
state.render();
