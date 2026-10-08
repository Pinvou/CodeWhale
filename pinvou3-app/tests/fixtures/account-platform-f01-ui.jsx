import React from 'react';
import { createRoot } from 'react-dom/client';
import { AccountPlatformView } from '../../src/features/account-platform/AccountPlatformView.jsx';
import { AdminPlatformView } from '../../src/features/account-platform/AdminPlatformView.jsx';
import { AccountPlatformBffClient } from '../../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../../src/features/account-platform/desktop-client.js';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { accountPlatformCopy, adminPlatformCopy } from '../../src/shared/i18n.js';
const root = createRoot(document.querySelector('#root'));
const id = 'a7f21df2-d2ef-4d55-99c8-a5470f9968b9';
const other = 'b7f21df2-d2ef-4d55-99c8-a5470f9968b9';
const json = (value, status = 200) => new Response(status === 204 ? null : JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
let language = 'en'; let kind = 'account'; let client;
const image = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aWTsAAAAASUVORK5CYII=';
const control = { id, other, calls: [], authenticated: true, balanceAllowed: true, balanceFail: false, logoutFail: false, user: id, delayedProfile: false, badRetry: false, smsFail: false, smsBad: false, smsRevision: 2, adminError: false, smsCalls: [], nativeCalls: [], authenticatedCallbacks: [], registration: null };
const configuration = () => ({ revision: control.smsRevision, enabled: false, provider_id: 'http-generated', credential_profile: 'http-default', templates: { REGISTER: '', LOGIN: '', PROFILE_MOBILE: '', PASSWORD_RESET: '' }, source: 'DATABASE', readiness: 'READY', available_profiles: [{ provider_id: 'http-generated', profile_id: 'http-default', configured: true }] });
globalThis.fetch = async (url, options = {}) => {
  control.calls.push({ url, method: options.method || 'GET', body: options.body, headers: options.headers });
  if (url === '/api/session') return control.authenticated ? json({ authenticated: true, csrf_token: 'control-csrf' }) : json({}, 401);
  if (url === '/api/logout') { if (control.logoutFail) return json({ code: 'UPSTREAM_UNAVAILABLE' }, 503); control.authenticated = false; return json(null, 204); }
  if (url.endsWith('/account/bootstrap')) return json({ user_id: control.user, username: control.user === id ? 'account-a' : 'account-b', status: 'ACTIVE', personal_permissions: ['personal.profile.update', ...(control.balanceAllowed ? ['personal.billing.read'] : [])], platform_permissions: ['platform.users.read', 'platform.users.status.manage', 'platform.registrations.repair', 'platform.profile-changes.repair', 'platform.sms-config.read', 'platform.sms-config.manage'] });
  if (url.includes('/v1/me/balance')) {
    const response = () => control.balanceFail ? json({ code: 'UPSTREAM_UNAVAILABLE' }, 503) : new Response('{"available_microcredits":9223372036854775807,"reserved_microcredits":-10001}', { status: 200 });
    if (control.delayNextBalance) { control.delayNextBalance = false; return new Promise(resolve => { control.releaseBalance = () => resolve(response()); }); }
    return response();
  }
  if (url.endsWith('/v1/me/profile-change-requests')) { if (control.delayedProfile) return await new Promise(resolve => { control.releaseProfile = () => resolve(json({ profile_change_request_id: id, field: JSON.parse(options.body).field, status: 'PENDING' })); }); return json({ profile_change_request_id: id, field: JSON.parse(options.body).field, status: 'PENDING' }); }
  if (url.includes('/v1/me/profile-change-requests/') && url.endsWith('/actions/complete')) return json({ profile_change_request_id: id, field: 'USERNAME', status: control.completedProfile ? 'COMPLETED' : 'RETRY' });
  if (url.includes('/admin/v1/users?')) return json({ items: [{ user_id: id, username: 'account-a', status: control.status || 'ACTIVE' }], next_cursor: null });
  if (url.endsWith('/status')) { control.status = JSON.parse(options.body).status; return json(null, 204); }
  if (url.includes('/admin/v1/registration-operations?')) return json({ items: [{ registration_operation_id: id, status: 'REPAIR_REQUIRED', stage: 'ACCOUNT_COMMITTING', compensation_allowed: true }] });
  if (url.endsWith('/admin/v1/profile-change-requests')) return control.adminError ? json({ code: 'UPSTREAM_UNAVAILABLE' }, 503) : json({ items: [{ profile_change_request_id: id, status: 'REPAIR_REQUIRED' }] });
  if (url.includes('/admin/v1/profile-change-requests/') && url.endsWith('/actions/retry')) return json({ request_id: id, resource_type: 'profile_change', action: 'RETRY_SCHEDULED', status: 'RETRY' }, control.badRetry ? 200 : 202);
  if (url.includes('/registration-operations/') && url.endsWith('/actions/retry')) return json({ registration_operation_id: id, status: 'IDENTITY_CREATING' }, 202);
  if (url.endsWith('/actions/compensate')) return json(null, 204);
  if (url.endsWith('/admin/v1/sms-configuration')) {
    if (options.method !== 'PUT') return json(configuration());
    const command = JSON.parse(options.body); control.smsCalls.push({ key: options.headers['Idempotency-Key'], command });
    if (control.smsFail) return json({ code: 'UPSTREAM_UNAVAILABLE' }, 503);
    control.smsRevision = command.expected_revision + 1;
    return json({ ...configuration(), ...command, revision: control.smsBad ? command.expected_revision : control.smsRevision });
  }
  throw new Error(`unexpected fixture route ${options.method || 'GET'} ${url}`);
};
const native = {
  session: async () => ({ authenticated: control.authenticated }),
  registrationStatus: async () => control.registration,
  login: async body => { control.nativeCalls.push(['login', { account: body.account }]); if (control.loginError) throw control.loginError; if (control.loginFail) throw Object.assign(new Error('AUTHENTICATION_FAILED'), { code: 'AUTHENTICATION_FAILED' }); control.authenticated = true; return { authenticated: true }; },
  createSliderCaptcha: async body => { control.nativeCalls.push(['captcha', body]); return { captcha_id: id, width: 280, height: 120, piece_width: 44, piece_height: 44, background_image: image, piece_image: image }; },
  precheckMobileRegistration: async body => { control.nativeCalls.push(['precheck', { username: body.username, mobile: body.mobile }]); return { username: body.username, password_handle: 'control-password-handle', password_policy_version: 'control-policy-v1' }; },
  sendSms: async body => { control.nativeCalls.push(['sms', body]); if (control.smsError) throw control.smsError; if (body.purpose === 'REGISTER') control.registration = { registration_operation_id: id, username: body.username, status: 'SMS_PENDING' }; return { challenge_id: id, registration_operation_id: id, registration_query_token: 'control-query-proof', retry_after_seconds: 1 }; },
  registerMobile: async body => { control.nativeCalls.push(['register', body]); if (control.registerUnknown) { control.registration = { registration_operation_id: id, username: body.username, status: 'REPAIR_REQUIRED' }; throw new Error('UPSTREAM_UNAVAILABLE'); } control.registration = { registration_operation_id: id, username: body.username, status: 'ACTIVE', user_id: id }; return control.registration; },
  loginSms: async body => { control.nativeCalls.push(['sms-login', body]); control.authenticated = true; return { authenticated: true }; },
  resetPassword: async body => { control.nativeCalls.push(['reset', { mobile: body.mobile, challenge_id: body.challenge_id, code: body.code }]); return { status: 'COMPLETED' }; },
  logout: async () => { control.authenticated = false; return {}; },
  openPortal: async body => { control.nativeCalls.push(['open-portal', body]); },
  request: async body => { const response = await fetch(`/api/${body.area}${body.resource}`, { method: body.method, body: JSON.stringify(body.body) }); const bodyText = await response.text(); return { status: response.status, body: JSON.parse(bodyText), bodyText }; },
};
function render() {
  root.render(kind === 'admin' ? <AdminPlatformView client={client} copy={adminPlatformCopy[language]} language={language} onLanguageChange={value => { language = value; render(); }} /> : <AccountPlatformView client={client} copy={accountPlatformCopy[language]} language={language} onLanguageChange={value => { language = value; render(); }} portal={kind !== 'desktop'} onAuthenticated={profile => control.authenticatedCallbacks.push(profile.user_id)} />);
}
control.render = value => { kind = value; client = value === 'admin' ? new AdminPlatformClient() : value === 'desktop' ? new AccountPlatformDesktopClient(native) : new AccountPlatformBffClient(); render(); };
control.switchIdentity = () => { control.user = other; client = new AccountPlatformBffClient(); render(); };
control.copy = () => ({ account: accountPlatformCopy[language], admin: adminPlatformCopy[language] });
control.render('account');
window.f01 = control;
