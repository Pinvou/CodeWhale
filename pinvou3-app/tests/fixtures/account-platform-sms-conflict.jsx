import React from 'react';
import { createRoot } from 'react-dom/client';
import { SettingsWorkspace } from '../../src/features/account-platform/SettingsWorkspace.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { adminPlatformCopy } from '../../src/shared/i18n.js';

const root = createRoot(document.querySelector('#root'));
let language = new URLSearchParams(location.search).get('language') || 'en';
const templates = { REGISTER: '', LOGIN: '', PROFILE_MOBILE: '', PASSWORD_RESET: '' };
const control = { revision: 0, enabled: false, gets: [], writes: [], replies: [], readReplies: [], heldReads: [], heldWrites: [], holdRead: false, holdWrite: false, version: 1, permissions: ['platform.sms-config.read', 'platform.sms-config.manage'] };
const configuration = () => ({ revision: control.revision, enabled: control.enabled, provider_id: 'http-generated', credential_profile: 'http-default', sign_name: '', templates, source: 'ADMIN', readiness: 'READY', available_profiles: [{ provider_id: 'http-generated', profile_id: 'http-default', configured: true }] });
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
 if (reply === 'replay') return response({ ...configuration(), ...command, revision: command.expected_revision + 1 });
 if (command.expected_revision !== control.revision) return response({ code: 'SMS_CONFIGURATION_CONFLICT' }, 409);
 control.revision++; control.enabled = command.enabled;
 return response({ ...configuration(), ...command, revision: control.revision });
};
const render = () => root.render(<SettingsWorkspace client={client} language={language} copy={adminPlatformCopy[language]} permissions={control.permissions} onLanguageChange={value => { language = value; render(); }} />);
Object.assign(control, {
 render, copy: () => adminPlatformCopy[language], configuration,
 versionChange() { control.version++; client.observeAuthorizationVersions(new Headers({ 'X-Context-Version': String(control.version), 'X-Platform-Authorization-Version': String(control.version) })); },
 retire() { client.retireSession('synthetic-next-session'); render(); },
});
globalThis.smsConflict = control; render();
