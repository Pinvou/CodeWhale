import React from 'react';
import { createRoot } from 'react-dom/client';
import { AccountPlatformView } from '../../src/features/account-platform/AccountPlatformView.jsx';
import { AccountPlatformDesktopClient } from '../../src/features/account-platform/desktop-client.js';
import { recoverPersonalRechargeReauthentication, personalRechargeOwner } from '../../src/features/account-platform/personal-recharge-reauthentication.js';
import { readClosureIntent } from '../../src/features/account-platform/closure-intent.js';
import { accountPlatformCopy, accountClosureCopy, financeCopy } from '../../src/shared/i18n.js';
const query = new URLSearchParams(location.search);
const id = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const state = window.nativeReauth = { language: query.get('language') || 'zh', user: id(1), originalUser: id(1), calls: [], loginCalls: 0, logoutCalls: 0, recent: false, authenticated: true, unknown: query.has('unknown'), delayed: query.has('delay'), wrongActor: query.has('wrong-actor'), recharge: query.has('recharge'), contextVersion: 1, permissionsChanged: false, rechargeUnknown: true, failed: query.has('failure'), copy: accountPlatformCopy, closureCopy: accountClosureCopy, financeCopy };
const receipt = { closure_request_id: id(9), status: 'IN_PROGRESS', stage: 'CLOSURE_REQUESTED', blockers: [], sla_expires_at: query.has('invalid-closure-ack') ? undefined : '2026-10-14T00:00:00Z' };
const profile = () => ({ user_id: state.user, status: 'ACTIVE', personal_permissions: ['personal.account.close.read', 'personal.account.close.request', ...(state.recharge ? ['personal.billing.read', ...(!state.permissionsChanged ? ['personal.recharge.create'] : [])] : [])], platform_permissions: [], personal_authorization_version: 1, active_context: { context_type: 'PERSONAL', context_version: state.contextVersion, personal_authorization_version: 1 } });
const transport = {
  session: async () => ({ authenticated: state.authenticated }),
  registrationStatus: async () => null,
  openPortal: async () => { throw new Error('Unexpected Portal reauthentication'); },
  logout: async () => { state.logoutCalls++; state.authenticated = false; },
  login: async () => {
    state.loginCalls++; state.authenticated = false;
    if (state.delayed) await new Promise(resolve => { state.resolveLogin = resolve; });
    if (state.failed) throw new Error('oidc_credentials_invalid');
    state.user = state.wrongActor ? id(2) : state.originalUser;
    state.authenticated = true; state.recent = true;
    if (query.has('context-change')) state.contextVersion = 2;
    if (query.has('permissions-change')) state.permissionsChanged = true;
  },
  request: async input => {
    state.calls.push(input);
    if (input.resource === '/v1/account/bootstrap') return { status: 200, body: profile() };
    if (input.resource === '/v1/me/balance?consistency=strong') return { status: 200, body: { available_microcredits: 0, reserved_microcredits: 0 } };
    if (input.resource === '/v1/recharge/products') return { status: 200, body: { items: [{ product_id: id(5), amount_fen: 100, channels: ['UNIONPAY'] }] } };
    if (input.resource === '/v1/payment-capabilities') return { status: 200, body: { channels: ['UNIONPAY'] } };
    if (input.resource === '/v1/recharge/orders' && input.method === 'GET') return { status: 200, body: { items: [] } };
    if (input.resource === '/v1/me/entitlement-packages') return { status: 200, body: { items: [] } };
    if (input.resource === '/v1/recharge/orders' && input.method === 'POST') {
      if (query.has('recharge-delay') && !state.rechargeDelayed) { state.rechargeDelayed = true; return new Promise(resolve => { state.resolveRecharge = () => resolve({ status: 201, body: { order_id: id(7), status: 'CREATED', amount_fen: 100 } }); }); }
      if (state.rechargeUnknown) { state.rechargeUnknown = false; return { status: 503, body: { code: 'COMMAND_RESULT_UNKNOWN' } }; }
      if (query.has('recharge-denied')) return { status: 403, body: { code: 'FORBIDDEN' } };
      return { status: 201, body: { order_id: id(7), status: 'CREATED', amount_fen: 100 } };
    }
    if (input.resource === '/v1/me/organizations') return { status: 200, body: { items: [] } };
    if (input.resource === '/v1/me/account-closure-request') return state.saved ? { status: 200, body: receipt } : { status: 404, body: { code: 'NOT_FOUND' } };
    if (input.resource === '/v1/me/account-closure-requests') {
      if (state.unknown) { state.unknown = false; return { status: 503, body: { code: 'COMMAND_RESULT_UNKNOWN' } }; }
      if (!state.recent) return { status: 403, body: { code: 'RECENT_AUTHENTICATION_REQUIRED' } };
      state.saved = true; return { status: 202, body: receipt };
    }
    throw new Error(`Unexpected controlled desktop route: ${input.resource}`);
  },
};
const root = createRoot(document.getElementById('root'));
state.client = new AccountPlatformDesktopClient(transport);
state.rechargeProof = () => recoverPersonalRechargeReauthentication(state.client, personalRechargeOwner(profile()));
state.proof = () => readClosureIntent(state.originalUser, 'account-closure');
state.render = () => root.render(<AccountPlatformView client={state.client} copy={accountPlatformCopy[state.language]} language={state.language} onLanguageChange={value => { state.language = value; state.render(); }} />);
state.replaceClient = () => { state.user = id(2); state.client = new AccountPlatformDesktopClient(transport); state.authenticated = true; state.render(); };
state.render();
