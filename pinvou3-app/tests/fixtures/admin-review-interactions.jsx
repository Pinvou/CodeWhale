import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminProtocolFormulaWorkspace } from '../../src/features/account-platform/AdminProtocolFormulaWorkspace.jsx';
import { SettingsWorkspace } from '../../src/features/account-platform/SettingsWorkspace.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { adminPlatformCopy, tokenManagementCopy } from '../../src/shared/i18n.js';
import '../../src/features/account-platform/admin-platform.css';
import '../../src/features/account-platform/admin-portal-layout.css';
import '../../src/features/account-platform/admin-token-management.css';

// Controlled public-shaped DTOs; these endpoints never contact a live service.
const id = number => '00000000-0000-4000-8000-' + String(number).padStart(12, '0');
const formula = { formula_version_id: id(301), protocol_definition_id: id(201), protocol_code: 'SYNTHETIC_PROTOCOL', version: 1, status: 'PUBLISHED', required_unit_versions: { INPUT: id(401) }, expression: { INPUT: 'estimated_input_tokens' }, evaluator_version: 'SAFE_PARAMETER_MAP_V1', etag: 'a'.repeat(64) };
const protocol = { protocol_definition_id: id(201), protocol_code: formula.protocol_code, version: 1, status: 'PUBLISHED', request_schema: { type: 'object', properties: {} }, etag: 'b'.repeat(64) };
const configuration = { revision: 3, enabled: true, provider_id: 'aliyun-managed', credential_profile: 'aliyun-default', sign_name: 'Synthetic sign', templates: { REGISTER: 'Synthetic register', LOGIN: 'Synthetic login', PROFILE_MOBILE: 'Synthetic mobile', PASSWORD_RESET: 'Synthetic reset' }, available_profiles: [{ provider_id: 'aliyun-managed', profile_id: 'aliyun-default', configured: true }] };
const control = window.adminReview = { calls: [], release: null, smsWrites: [] };
const original = window.fetch.bind(window);
const response = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const page = items => ({ items, has_more: false, next_cursor: null, limit: 50, as_of: '2026-11-10T00:00:00Z' });
window.fetch = async (url, options = {}) => {
  if (!String(url).startsWith('/api/')) return original(url, options);
  const path = new URL(url, location.href).pathname;
  const method = options.method || 'GET';
  control.calls.push({ path, method });
  if (path.endsWith('/protocols')) return response(page([protocol]));
  if (path.endsWith('/formulas')) return response(page([formula]));
  if (path.endsWith('/preview')) return new Promise(resolve => {
    control.release = failure => resolve(failure ? response({ code: 'UPSTREAM_UNAVAILABLE' }, 503) : response({ formula, deterministic: true, external_access: false, sample_missing_optional_parameters: { INPUT: 0 } }));
  });
  if (path.endsWith('/sms-configuration')) {
    if (method === 'GET') return response(configuration);
    control.smsWrites.push({ body: options.body, key: options.headers['Idempotency-Key'] });
    if (control.smsWrites.length === 1) return response({ code: 'UPSTREAM_UNAVAILABLE' }, 503);
    return response({ ...JSON.parse(options.body), revision: 4 });
  }
  return response({ code: 'SYNTHETIC_UNEXPECTED_ROUTE' }, 404);
};
const client = new AdminPlatformClient();
client.csrfToken = 'SYNTHETIC_CSRF';
const sms = new URLSearchParams(location.search).has('sms');
createRoot(document.querySelector('#root')).render(sms
  ? <SettingsWorkspace copy={adminPlatformCopy.en} language="en" client={client} permissions={['platform.sms-config.read', 'platform.sms-config.manage']} onLanguageChange={() => {}} />
  : <AdminProtocolFormulaWorkspace client={client} copy={tokenManagementCopy.en.modelToken} canManage={false} routeVersion={0} cursorHistory={{ current: {} }} onRouteChange={() => {}} />);
