import React, { useEffect } from 'react';
import { createRoot } from 'react-dom/client';
import { AccountClosureWorkspace } from './src/features/account-platform/AccountClosureWorkspace.jsx';
import { AdminAccountClosureWorkspace } from './src/features/account-platform/AdminAccountClosureWorkspace.jsx';
import { AccountPlatformBffClient } from './src/features/account-platform/bff-client.js';
import { AdminPlatformClient } from './src/features/account-platform/admin-client.js';
import { AccountPlatformDesktopClient } from './src/features/account-platform/desktop-client.js';
import { AccountPortalPanel } from './src/features/account-platform/AccountPortalLayout.jsx';
import { accountClosureCopy, financeCopy } from './src/shared/i18n.js';
const receipt = { closure_request_id: '11111111-1111-4111-8111-111111111111', status: 'IN_PROGRESS', stage: 'CLOSURE_REQUESTED', blockers: [], sla_expires_at: '2030-01-01T00:00:00Z' };
const ui = createRoot(document.getElementById('root'));
const c = window.closure = { language: 'en', owner: '22222222-2222-4222-8222-222222222222', admin: false, deniedRead: false, readonly: false, queryError: 0, unknown: false, delay: false, calls: [], receipt: null };
c.client = new AccountPlatformBffClient(); c.client.csrfToken = 'controlled-csrf';
c.adminClient = new AdminPlatformClient(); c.adminClient.csrfToken = 'controlled-admin-csrf';
c.nativeClient = new AccountPlatformDesktopClient({
  async request(input) {
    // The real desktop client's transport has no AbortSignal parameter.
    const response = await globalThis.fetch(`/api/${input.area}${input.resource}`, { method: input.method, headers: input.idempotencyKey ? { 'Idempotency-Key': input.idempotencyKey } : {}, body: input.body === undefined ? undefined : JSON.stringify(input.body) });
    return { status: response.status, body: await response.json() };
  },
});
c.copy = () => accountClosureCopy[c.language]; c.finance = () => financeCopy[c.language];
c.receiptTemplate = receipt;
c.readHolds = [];
c.releaseReads = () => { const held = c.readHolds.splice(0); for (const item of held) item.release(); };
c.unmount = () => ui.render(null);
globalThis.fetch = async (url, input = {}) => {
  const method = input.method || 'GET'; c.calls.push({ url, method, headers: input.headers || {}, body: input.body });
  if (method === 'GET') {
    const response = c.queryError ? new Response(JSON.stringify({ code: 'CONTROLLED_READ_UNAVAILABLE' }), { status: c.queryError })
      : String(url).includes('/api/admin/') ? Response.json({ items: [{ ...receipt, status: 'REPAIR_REQUIRED' }] })
        : c.receipt ? Response.json(c.receipt) : new Response('{}', { status: 404 });
    // Capture the old server result and deliberately ignore abort, so a late
    // response tests the real component's fence rather than a cooperative fake.
    if (c.holdRead) await new Promise(resolve => { c.readHolds.push({ signal: input.signal, release: resolve }); });
    return response;
  }
  if (c.persistRequests && !String(url).includes('/api/admin/')) c.receipt = { ...receipt };
  if (c.delay) await new Promise(resolve => { c.resolve = resolve; });
  if (c.repair401 && String(url).includes('/actions/retry')) return new Response(JSON.stringify({ code: 'RECENT_AUTHENTICATION_REQUIRED' }), { status: 401 });
  if (c.unknown) { c.unknown = false; throw new TypeError('Controlled ACK lost'); }
  if (c.serializationConflict) { c.serializationConflict = false; return new Response(JSON.stringify({ code: 'ACCOUNT_CLOSURE_SERIALIZATION_CONFLICT' }), { status: 409 }); }
  return Response.json(c.invalidSLA ? { ...receipt, sla_expires_at: c.invalidSLA === 'missing' ? undefined : '2026-02-31T12:00:00Z' } : receipt, { status: String(url).includes('/actions/retry') ? 200 : 202 });
};
window.prompt = () => 'CLOSE_MY_ACCOUNT';
function AfterEffectProbe({ client, owner, epoch }) {
  useEffect(() => {
    if (c.afterEffectSubmit) {
      c.afterEffectSubmit = false;
      c.afterEffectCalls = (c.afterEffectCalls || 0) + 1;
      [...document.querySelectorAll('.ap-danger-section button')].find(button => [c.copy().start, c.finance().retry].includes(button.textContent))?.click();
    }
  }, [client, owner, epoch]);
  return null;
}
c.render = () => {
  const me = { user_id: c.owner, personal_permissions: c.deniedRead ? [] : c.readonly ? ['personal.account.close.read'] : ['personal.account.close.read', 'personal.account.close.request'], platform_permissions: c.readonly ? ['platform.account-closures.read'] : ['platform.account-closures.read', 'platform.account-closures.repair'] };
  ui.render(c.admin ? <AdminAccountClosureWorkspace key={c.owner} client={c.adminClient} me={me} language={c.language} /> : <AccountPortalPanel section="accountSettings" active={c.sectionActive !== false}><AccountClosureWorkspace key={c.keepMounted ? 'controlled-owned-view' : c.owner} client={c.client} me={me} language={c.language} /><AfterEffectProbe client={c.client} owner={c.owner} epoch={c.client.financeSessionEpoch} /></AccountPortalPanel>);
};
c.render();
