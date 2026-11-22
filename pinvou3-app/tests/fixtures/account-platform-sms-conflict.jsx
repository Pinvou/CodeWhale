import React from 'react';
import { createRoot } from 'react-dom/client';
import { SettingsWorkspace } from '../../src/features/account-platform/SettingsWorkspace.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { adminPlatformCopy } from '../../src/shared/i18n.js';

const root = createRoot(document.querySelector('#root'));
let language = new URLSearchParams(location.search).get('language') || 'en';
const aliyun = new URLSearchParams(location.search).get('provider') === 'aliyun-managed';
const templates = { REGISTER: aliyun ? 'SMS_REGISTER' : '', LOGIN: aliyun ? 'SMS_LOGIN' : '', PROFILE_MOBILE: aliyun ? 'SMS_PROFILE' : '', PASSWORD_RESET: aliyun ? 'SMS_RESET' : '' };
const control = { revision: 0, enabled: false, signName: aliyun ? 'Pinvou' : '', templates, receipts: new Map(), gets: [], writes: [], replies: [], readReplies: [], heldReads: [], heldWrites: [], holdRead: false, holdWrite: false, version: 1, permissions: ['platform.sms-config.read', 'platform.sms-config.manage'] };
const configuration = () => ({ revision: control.revision, enabled: control.enabled, provider_id: aliyun ? 'aliyun-managed' : 'http-generated', credential_profile: aliyun ? 'aliyun-default' : 'http-default', sign_name: control.signName, templates: control.templates, source: 'ADMIN', readiness: 'READY', available_profiles: [{ provider_id: aliyun ? 'aliyun-managed' : 'http-generated', profile_id: aliyun ? 'aliyun-default' : 'http-default', configured: true }] });
// HTTP stand-in for normalizeSMSConfiguration: literal expectations in the
// browser cases also check the outgoing command rather than trusting this echo.
const serverTrim = value => value.replace(/^[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g, '');
const response = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const client = new AdminPlatformClient(); client.csrfToken = 'synthetic-csrf'; client.authorizationVersions = { context_version: 1, platform_authorization_version: 1 };
control.client = client;
globalThis.fetch = async (url, options = {}) => {
 if (url !== '/api/admin/account/admin/v1/sms-configuration') throw new Error(`Unexpected SMS fixture route ${url}`);
 if (!options.method || options.method === 'GET') {
  const snapshot = configuration(); control.gets.push(snapshot.revision);
  const readReply = control.readReplies.shift();
  if (control.holdRead) { control.holdRead = false; await new Promise(resolve => control.heldReads.push(resolve)); }
  return readReply ? response({ code: 'UPSTREAM_UNAVAILABLE' }, readReply) : response(snapshot);
 }
 if (options.method !== 'PUT') throw new Error(`Unexpected SMS fixture method ${options.method}`);
 const command = JSON.parse(options.body), key = options.headers['Idempotency-Key'];
 control.writes.push({ command, key }); const reply = control.replies.shift();
 if (control.holdWrite) { control.holdWrite = false; await new Promise(resolve => control.heldWrites.push(resolve)); }
 if (reply === 'abort') throw new DOMException('Synthetic lost response', 'AbortError');
 if (reply === 'network') throw new TypeError('Synthetic network loss');
 if (reply === 'bad-ack') return response({ ...configuration(), ...command, revision: command.expected_revision });
 if (reply?.status) return response({ code: reply.code }, reply.status);
 const accepted = { ...command, sign_name: serverTrim(command.sign_name), templates: Object.fromEntries(Object.entries(command.templates).map(([purpose, value]) => [purpose, serverTrim(value)])) };
 if (control.receipts.has(key)) return response(control.receipts.get(key));
 if (reply === 'replay') return response({ ...configuration(), ...accepted, revision: command.expected_revision + 1 });
 if (command.expected_revision !== control.revision) return response({ code: 'SMS_CONFIGURATION_CONFLICT' }, 409);
 control.revision++; control.enabled = accepted.enabled; control.signName = accepted.sign_name; control.templates = accepted.templates;
 const receipt = { ...configuration(), ...accepted, revision: control.revision };
 control.receipts.set(key, receipt);
 if (reply === 'lost-ack') throw new TypeError('Synthetic lost response after accepted canonical write');
 return response(receipt);
};
const render = () => root.render(<SettingsWorkspace client={client} language={language} copy={adminPlatformCopy[language]} permissions={control.permissions} onLanguageChange={value => { language = value; render(); }} />);
Object.assign(control, {
 render, copy: () => adminPlatformCopy[language], configuration,
 versionChange() { control.version++; client.observeAuthorizationVersions(new Headers({ 'X-Context-Version': String(control.version), 'X-Platform-Authorization-Version': String(control.version) })); },
 retire() { client.retireSession('synthetic-next-session'); render(); },
});
globalThis.smsConflict = control; render();
