import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminPlatformView } from '../../src/features/account-platform/AdminPlatformView.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { adminPlatformCopy, financeCopy } from '../../src/shared/i18n.js';
const root = createRoot(document.querySelector('#root'));
const id = '11111111-1111-4111-8111-111111111111';
const other = '22222222-2222-4222-8222-222222222222';
const api = new AdminPlatformClient();
const control = { id, other, language: new URLSearchParams(location.search).get('language') || 'en', calls: [], permission: true, unknown: false, expired: false, submitted: false, brokenDirectory: false };
const permissions = ['platform.billing.wallets.read', 'platform.billing.repairs.request', 'platform.billing.repairs.approve', 'platform.billing.adjustments.approve', 'platform.billing.payment-controls.resume.request', 'platform.billing.payment-controls.resume.approve'];
const detail = () => ({ approval_request_id: id, command_type: 'WALLET_ADJUSTMENT', status: control.submitted ? 'SUBMITTED' : 'DRAFT', version: control.submitted ? 2 : 1, action_control_version: 1, detail_availability: 'AVAILABLE', allowed_actions: control.submitted ? ['WITHDRAW'] : ['EDIT', 'SUBMIT'], requester: { user_id: id }, target: { resource_type: 'MANUAL_ADJUSTMENT', resource_id: id }, command: { adjustment_id: id, wallet_id: id, amount_microcredits: '9007199254740993' }, execution: { attempt_status: 'PENDING' } });
const json = (value, status=200, headers={}) => new Response(status === 204 ? null : JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json', ...headers } });
globalThis.fetch = async (url, options={}) => {
 const method=options.method || 'GET'; control.calls.push({ url, method, body: options.body, headers: options.headers });
 if (url.endsWith('/api/session')) return json({ authenticated: true, csrf_token: 'SYNTHETIC_ONLY_CSRF' });
 if (url.endsWith('/v1/account/bootstrap')) return json({ user_id: id, status: 'ACTIVE', personal_permissions: [], platform_permissions: control.permission ? permissions : [], platform_authorization_version: 1 });
 if (url.endsWith('/approvals/capability')) return json({ control: { version: 1, epoch: 1, authorization_relay_epoch: 1 }, session_approval_wire_version: 2 });
 if (url.endsWith('/approvals/directory')) return control.brokenDirectory ? json({ code: 'UNAVAILABLE' }, 503) : json({ schema_version: 1, platform_authorization_version: 1, items: [ { command_type:'WALLET_ADJUSTMENT', policy_version:1, risk_class:'FINANCIAL_FACT_CHANGE', requestable:false }, { command_type:'PAYMENT_PROVIDER_RESUME', policy_version:2, risk_class:'FINANCIAL_FACT_CHANGE', requestable:true } ] }, 200, { 'X-Platform-Authorization-Version': '1' });
 if (url.includes('/approvals/requests?')) {
  const params=new URL(url, location.origin).searchParams;
  if (control.expired && params.get('cursor')) return json({code:'PAGINATION_CURSOR_EXPIRED'},410);
  const own={...detail(), allowed_actions:[]};
  const foreign={...own, approval_request_id:other, requester:{user_id:other},status:params.get('status') || 'SUBMITTED'};
  return json({items:params.get('requester_user_id') ? [own] : [own,foreign],next_cursor:params.get('cursor') ? '' : 'controlled-next'});
 }
 if (url.endsWith(`/approvals/requests/${id}`)) return json(detail());
 if (url.endsWith(`/approvals/requests/${other}`)) return json({...detail(),approval_request_id:other,requester:{user_id:other},allowed_actions:[]});
 if (url.endsWith('/actions/submit')) { control.submitted=true; return control.unknown ? json({code:'UPSTREAM_UNAVAILABLE'},503) : json(null,204); }
 if (url.includes('/admin/v1/wallets?')) return json({items:[]});
 if (url.includes('/admin/v1/adjustments?')) return json({items:[]});
 if (url.endsWith('/api/logout')) return json(null,204);
 throw new Error(`Uncontrolled HTTP ${method} ${url}`);
};
function render() { root.render(<AdminPlatformView key={control.mountVersion || 0} client={api} copy={adminPlatformCopy[control.language]} language={control.language} onLanguageChange={language=>{control.language=language;render();}} />); }
control.copy=()=>adminPlatformCopy[control.language];control.financeCopy=()=>financeCopy[control.language];control.render=render;
control.mount=()=>{control.mountVersion=(control.mountVersion||0)+1;render();};
window.approvalCenter=control;render();
