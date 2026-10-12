import React from 'react';
import { createRoot } from 'react-dom/client';
import { PersonalUsageWorkspace } from '../../src/features/account-platform/PersonalUsageWorkspace.jsx';
import { AccountPlatformBffClient } from '../../src/features/account-platform/bff-client.js';
import { modelUsageCopy } from '../../src/shared/i18n.js';
const root = createRoot(document.querySelector('#root'));
const control = { language: 'en', user: 'public-user-a', version: 1, read: true, cancel: true, active: false, invalid: false, unknownCancel: false, delayed: false, calls: [], pending: [], execution: null, settlement: null };
const client = new AccountPlatformBffClient(); client.csrfToken = 'public-controlled-csrf';
const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const payload = user => ({ items: [{ usage_request_id: 'public-request-001', model: user, logical_turn_id: 'pinvou-turn-public-group', charged_microcredits: control.active ? null : '9223372036854775807', input_tokens: 10, output_tokens: 2, cached_input_tokens: 90, status: control.active ? 'HOLD_RESERVED' : 'COMMITTED', execution_status: control.execution || (control.active ? 'STREAMING' : 'SUCCEEDED'), settlement_status: control.settlement || (control.active ? 'HOLD_ACTIVE' : 'SETTLED'), display_status: control.active ? 'IN_PROGRESS' : 'SETTLED', started_at: '2026-10-01T00:00:00Z' }], next_cursor: 'turn-v1.page2' });
globalThis.fetch = async (url, options) => {
  control.calls.push({ method: options.method, resource: url.replace('/api/token', ''), key: options.headers['Idempotency-Key'] });
  if (url.endsWith('/cancel')) {
    if (control.unknownCancel) { control.unknownCancel = false; return json({ code: 'CONTROLLED_UNKNOWN_ACK' }, 503); }
    control.active = false; return json({ status: 'DISPLAY_STOPPED_EXECUTION_CONTINUES' });
  }
  if (control.delayed) { control.delayed = false; const value = payload(control.user); return new Promise(resolve => control.pending.push(() => resolve(json(value)))); }
  if (control.invalid) return json({ items: null });
  return json(payload(control.user));
};
control.render = () => root.render(<PersonalUsageWorkspace client={client} me={{ user_id: control.user, active_context: { context_version: control.version, personal_authorization_version: control.version }, personal_permissions: [...(control.read ? ['personal.usage.read'] : []), ...(control.cancel ? ['personal.usage.cancel'] : [])] }} language={control.language} />);
control.copy = () => modelUsageCopy[control.language]; control.resolve = () => control.pending.splice(0).forEach(resolve => resolve());
globalThis.f05 = control; control.render();
