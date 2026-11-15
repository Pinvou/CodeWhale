import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminPlatformView } from '../src/features/account-platform/AdminPlatformView.jsx';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { adminPlatformCopy } from '../src/shared/i18n.js';
const params = new URLSearchParams(location.search);
const language = params.get('language') || 'en';
const state = window.statusFixture = { calls: [], language, copy: adminPlatformCopy[language], mode: params.get('mode') || 'healthy', resolve: null };
const original = window.fetch.bind(window);
window.fetch = async (url, options = {}) => {
  if (!String(url).startsWith('/api/')) return original(url, options);
  state.calls.push({ url: String(url), method: options.method || 'GET' });
  if (url === '/api/session') return new Response('{"authenticated":true}');
  if (url === '/api/account/v1/account/bootstrap') return new Response(JSON.stringify({ user_id: '00000000-0000-4000-8000-000000000018', status: 'ACTIVE', active_context: { context_type: 'PERSONAL', organization_id: null, membership_version: 1, context_version: 1, authorization_version: 1, personal_authorization_version: 1, platform_authorization_version: 1, organization_authorization_version: 1, permissions: [] }, personal_permissions: [], platform_permissions: state.mode === 'deny' ? [] : ['platform.system.status.read'] }));
  if (url === '/api/admin/system/status') {
    const mode = state.mode;
    if (mode === 'delay') await new Promise(resolve => { state.resolve = resolve; });
    if (mode === 'error') return new Response('{"code":"ADMIN_AUTH_UNAVAILABLE"}', { status: 503 });
    if (mode === 'missing') return new Response('{}');
    if (mode === 'reconciliation-unavailable') return new Response('{"services":[],"relays":[],"reconciliation":{"pending":null,"stopped":null},"recent_errors":[{"source":"token-reconciliation","code":"RECONCILIATION_STATUS_UNAVAILABLE","message":"reconciliation status unavailable"},{"source":"token-reconciliation","code":"RECONCILIATION_ERRORS_UNAVAILABLE","message":"reconciliation errors unavailable"}]}');
    if (mode === 'built-in-diagnostics') return new Response(JSON.stringify({services:[{key:'portal',status:'UNHEALTHY',latency_ms:12,code:'PROBE_CONNECTION_FAILED',message:'connection failed'},{key:'billing',status:'UNHEALTHY',latency_ms:12,code:'PROBE_HTTP_ERROR',message:'HTTP 503'}],relays:[{key:'billing-event-relay',status:'DEGRADED',pending_events:2,dead_lettered_events:3,oldest_pending_seconds:1.25,code:'RELAY_METRICS_INVALID',message:'relay metrics are invalid'},{key:'account-event-relay',status:'DEGRADED',pending_events:0,dead_lettered_events:0,oldest_pending_seconds:0,code:'RELAY_METRICS_HTTP_ERROR',message:'metrics HTTP 504'}],reconciliation:{pending:0,stopped:0},recent_errors:[{source:'portal',code:'PROBE_CONNECTION_FAILED',message:'connection failed'},{source:'billing-event-relay',code:'RELAY_METRICS_INVALID',message:'relay metrics are invalid'},{source:'billing',code:'PROBE_HTTP_ERROR',message:'HTTP 503'},{source:'account-event-relay',code:'RELAY_METRICS_HTTP_ERROR',message:'metrics HTTP 504'},{source:'dead-relay',code:'RELAY_DEAD_LETTERED_EVENTS',message:'3 dead-lettered events'},{source:'external-provider',code:'EXTERNAL_PROVIDER_NOTE',message:'controlled external diagnostic'}]}));
    const unhealthy = mode === 'unhealthy';
    return new Response(`{"checked_at":"2026-10-09T01:02:03Z","services":[{"key":"portal","status":"${unhealthy ? 'UNHEALTHY' : 'HEALTHY'}","latency_ms":12,"message":"${unhealthy ? 'connection failed' : ''}"}],"relays":[{"key":"billing-event-relay","status":"${unhealthy ? 'DEGRADED' : 'HEALTHY'}","pending_events":9007199254740993,"dead_lettered_events":${unhealthy ? 2 : 0},"oldest_pending_seconds":1.25}],"reconciliation":{"pending":9223372036854775807,"stopped":${unhealthy ? 3 : 0}},"recent_errors":${unhealthy ? '[{"source":"token-reconciliation","resource":"controlled-resource","message":"controlled pending error","updated_at":"2026-10-09T00:00:00Z"}]' : '[]'}}`);
  }
  throw Error(`Unexpected controlled request: ${url}`);
};
const root = createRoot(document.getElementById('root'));
state.render = (client = new AdminPlatformClient()) => root.render(<AdminPlatformView client={client} copy={adminPlatformCopy[language]} language={language} onLanguageChange={() => {}} />);
state.render();
