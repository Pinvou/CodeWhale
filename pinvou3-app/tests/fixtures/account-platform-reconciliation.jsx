import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminReconciliationWorkspace } from '../../src/features/account-platform/AdminReconciliationWorkspace.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { reconciliationCopy } from '../../src/shared/i18n.js';
const root = createRoot(document.querySelector('#root'));
const id = '11111111-1111-4111-8111-111111111111';
const other = '22222222-2222-4222-8222-222222222222';
const client = new AdminPlatformClient(); client.csrfToken = 'PUBLIC_SYNTHETIC_CSRF';
const control = { id, other, user: id, language: 'en', readonly: false, calls: [], unknown: false, unknownRun: false, wrong: false, delayed: false };
const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const row = () => ({ case_id: id, reconciliation_run_id: id, status: 'OPEN', merchant_order_no: 'controlled-merchant', case_type: 'AMOUNT_MISMATCH', version: 1, provider_snapshot: { amount_fen: '9007199254740993' } });
globalThis.fetch = async (url, options = {}) => {
  control.calls.push({ url: String(url), method: options.method || 'GET', body: options.body, key: options.headers?.['Idempotency-Key'] });
  if (String(url).includes('/reconciliation-runs?')) {
    const query = new URL(String(url), 'http://fixture.invalid').searchParams;
    const from = query.get('business_date_from'); const to = query.get('business_date_to');
    const next = new Date(`${from}T00:00:00Z`); next.setUTCDate(next.getUTCDate() + 1);
    if (!/^\d{4}-\d{2}-\d{2}$/.test(from || '') || !Number.isFinite(next.getTime()) || to !== next.toISOString().slice(0, 10)) return json({ code: 'INVALID_FILTER' }, 400);
    return json({ items: [{ reconciliation_run_id: id, business_date: from, status: 'COMPLETED', case_count: 1 }] });
  }
  if (String(url).includes('/reconciliation-cases?')) return json({ items: [row()] });
  if (String(url).endsWith('/commands')) {
    if (control.delayed) return new Promise(resolve => { control.release = () => resolve(json({ ...row(), status: 'IN_REVIEW' })); });
    return control.unknown ? json({ code: 'CONTROLLED_UNAVAILABLE' }, 503) : json({ ...row(), case_id: control.wrong ? other : id, status: 'IN_REVIEW' });
  }
  if (String(url).endsWith('/reconciliation-runs') || String(url).endsWith('/reconciliation-imports')) return control.unknownRun ? json({ code: 'CONTROLLED_UNAVAILABLE' }, 503) : json({ reconciliation_run_id: id, ...JSON.parse(options.body), status: 'COMPLETED' }, String(url).endsWith('/reconciliation-imports') ? 201 : 200);
  return json({ code: 'UNEXPECTED_FIXTURE_ROUTE' }, 404);
};
function render() { root.render(<AdminReconciliationWorkspace client={client} me={{ user_id: control.user, platform_authorization_version: 1, platform_permissions: ['platform.usage.read', ...(control.readonly ? [] : ['platform.usage.reconcile.retry'])] }} language={control.language} />); }
window.f03 = Object.assign(control, { render, copy: () => reconciliationCopy[control.language], switchIdentity: () => { control.user = other; client.financeSessionEpoch++; render(); } });
render();
