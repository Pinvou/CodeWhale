import React from 'react';
import { createRoot } from 'react-dom/client';
import { AccountClosureWorkspace } from './src/features/account-platform/AccountClosureWorkspace.jsx';
import { AdminAccountClosureWorkspace } from './src/features/account-platform/AdminAccountClosureWorkspace.jsx';
import { AccountPlatformBffClient } from './src/features/account-platform/bff-client.js';
import { AdminPlatformClient } from './src/features/account-platform/admin-client.js';
import { accountClosureCopy, financeCopy } from './src/shared/i18n.js';
const receipt = { closure_request_id: '11111111-1111-4111-8111-111111111111', status: 'IN_PROGRESS', stage: 'CLOSURE_REQUESTED', blockers: [], sla_expires_at: '2030-01-01T00:00:00Z' };
const ui = createRoot(document.getElementById('root'));
const c = window.closure = { language: 'en', owner: '22222222-2222-4222-8222-222222222222', admin: false, deniedRead: false, readonly: false, queryError: 0, unknown: false, delay: false, calls: [], receipt: null };
c.client = new AccountPlatformBffClient(); c.client.csrfToken = 'controlled-csrf';
c.adminClient = new AdminPlatformClient(); c.adminClient.csrfToken = 'controlled-admin-csrf';
c.copy = () => accountClosureCopy[c.language]; c.finance = () => financeCopy[c.language];
globalThis.fetch = async (url, input = {}) => {
  const method = input.method || 'GET'; c.calls.push({ url, method, headers: input.headers || {}, body: input.body });
  if (method === 'GET') {
    if (c.queryError) return new Response(JSON.stringify({ code: 'CONTROLLED_READ_UNAVAILABLE' }), { status: c.queryError });
    if (String(url).includes('/api/admin/')) return Response.json({ items: [{ ...receipt, status: 'REPAIR_REQUIRED' }] });
    return c.receipt ? Response.json(c.receipt) : new Response('{}', { status: 404 });
  }
  if (c.delay) await new Promise(resolve => { c.resolve = resolve; });
  if (c.unknown) { c.unknown = false; throw new TypeError('Controlled ACK lost'); }
  return Response.json(receipt, { status: String(url).includes('/actions/retry') ? 200 : 202 });
};
window.prompt = () => 'CLOSE_MY_ACCOUNT';
c.render = () => {
  const me = { user_id: c.owner, personal_permissions: c.deniedRead ? [] : c.readonly ? ['personal.account.close.read'] : ['personal.account.close.read', 'personal.account.close.request'], platform_permissions: c.readonly ? ['platform.account-closures.read'] : ['platform.account-closures.read', 'platform.account-closures.repair'] };
  ui.render(c.admin ? <AdminAccountClosureWorkspace key={c.owner} client={c.adminClient} me={me} language={c.language} /> : <AccountClosureWorkspace key={c.owner} client={c.client} me={me} language={c.language} />);
};
c.render();
