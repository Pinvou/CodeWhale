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
  if (url === '/api/account/v1/account/bootstrap') return new Response(JSON.stringify({ user_id: '00000000-0000-4000-8000-000000000018', status: 'ACTIVE', personal_permissions: [], platform_permissions: state.mode === 'deny' ? [] : ['platform.system.status.read'] }));
  if (url === '/api/admin/system/status') {
    const mode = state.mode;
    if (mode === 'delay') await new Promise(resolve => { state.resolve = resolve; });
    if (mode === 'error') return new Response('{"code":"ADMIN_AUTH_UNAVAILABLE"}', { status: 503 });
    if (mode === 'missing') return new Response('{}');
    const unhealthy = mode === 'unhealthy';
    return new Response(`{"services":[{"key":"portal","status":"${unhealthy ? 'UNHEALTHY' : 'HEALTHY'}","latency_ms":12,"message":"${unhealthy ? 'connection failed' : ''}"}],"relays":[{"key":"token-event-relay","status":"${unhealthy ? 'DEGRADED' : 'HEALTHY'}","pending_events":9007199254740993,"dead_lettered_events":${unhealthy ? 2 : 0},"oldest_pending_seconds":1.25}],"reconciliation":{"pending":9223372036854775807,"stopped":${unhealthy ? 3 : 0}},"recent_errors":${unhealthy ? '[{"source":"token-reconciliation","resource":"controlled-resource","message":"controlled pending error","updated_at":"2026-10-09T00:00:00Z"}]' : '[]'}}`);
  }
  throw Error(`Unexpected controlled request: ${url}`);
};
const root = createRoot(document.getElementById('root'));
state.render = (client = new AdminPlatformClient()) => root.render(<AdminPlatformView client={client} copy={adminPlatformCopy[language]} language={language} onLanguageChange={() => {}} />);
state.render();
