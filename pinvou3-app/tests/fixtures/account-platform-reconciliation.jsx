/* global __PINVOU_ORPHAN_CASE__ */
import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminReconciliationWorkspace } from '../../src/features/account-platform/AdminReconciliationWorkspace.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { reconciliationCopy } from '../../src/shared/i18n.js';
import recordedOrphan from './reconciliation-unknown-date-case.json';
const root = createRoot(document.querySelector('#root'));
const id = '11111111-1111-4111-8111-111111111111';
const other = '22222222-2222-4222-8222-222222222222';
const client = new AdminPlatformClient(); client.csrfToken = 'PUBLIC_SYNTHETIC_CSRF';
const control = { id, other, user: id, language: 'en', readonly: false, calls: [], unknown: false, unknownRun: false, wrong: false, delayed: false };
const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const producedOrphan = typeof __PINVOU_ORPHAN_CASE__ === 'undefined' ? recordedOrphan.case : __PINVOU_ORPHAN_CASE__;
const row = () => control.orphanCase || ({ case_id: id, reconciliation_run_id: id, status: 'OPEN', merchant_order_no: 'controlled-merchant', case_type: control.caseType || 'AMOUNT_MISMATCH', version: 1, provider_snapshot: { amount_fen: '9007199254740993' } });
globalThis.fetch = async (url, options = {}) => {
  control.calls.push({ url: String(url), method: options.method || 'GET', body: options.body, key: options.headers?.['Idempotency-Key'] });
  if (String(url).includes('/reconciliation-runs?')) {
    if (control.readStatus) return json({ code: control.readCode || 'CONTROLLED_READ_FAILURE' }, control.readStatus);
    const query = new URL(String(url), 'http://fixture.invalid').searchParams;
    const from = query.get('business_date_from'); const to = query.get('business_date_to');
    const next = new Date(`${from}T00:00:00Z`); next.setUTCDate(next.getUTCDate() + 1);
    if (!/^\d{4}-\d{2}-\d{2}$/.test(from || '') || !Number.isFinite(next.getTime()) || to !== next.toISOString().slice(0, 10)) return json({ code: 'INVALID_FILTER' }, 400);
    return json({ items: [{ reconciliation_run_id: id, business_date: from, status: control.running ? 'RUNNING' : 'COMPLETED', completed_at: control.running ? null : '2026-10-07T12:00:00Z', case_count: 1, provider_row_count: 5, source_mode: 'PROVIDER_QUERY' }], next_cursor: control.paginated && !query.get('cursor') ? 'controlled-page-2' : '' });
  }
  if (String(url).includes('/reconciliation-cases?')) return control.readStatus ? json({code:control.readCode || 'CONTROLLED_READ_FAILURE'},control.readStatus) : json({ items: [row()] });
  if (String(url).endsWith('/commands')) {
    if (control.orphanCase && JSON.parse(options.body).action === 'IGNORE' && JSON.parse(options.body).observed_observation_id !== row().latest_observation.observation_id) return json({ code: 'BILLING_CONFLICT' }, 409);
    if (control.commandStatus) return json({ code: control.commandCode }, control.commandStatus);
    if (control.delayed) return new Promise(resolve => { control.release = () => resolve(json({ ...row(), status: 'IN_REVIEW' })); });
    return control.unknown ? json({ code: 'CONTROLLED_UNAVAILABLE' }, 503) : json({ ...row(), case_id: control.wrong ? other : id, status: 'IN_REVIEW' });
  }
  if (String(url).endsWith('/reconciliation-runs') || String(url).endsWith('/reconciliation-imports')) { if (control.writeStatus) return json({ code: control.writeCode }, control.writeStatus); return control.unknownRun ? json({ code: 'CONTROLLED_UNAVAILABLE' }, 503) : json({ reconciliation_run_id: id, ...JSON.parse(options.body), status: 'COMPLETED' }, String(url).endsWith('/reconciliation-imports') ? 201 : 200); }
  return json({ code: 'UNEXPECTED_FIXTURE_ROUTE' }, 404);
};
function render() { root.render(<AdminReconciliationWorkspace client={client} me={{ user_id: control.user, platform_authorization_version: 1, platform_permissions: ['platform.billing.reconciliation.read', ...(control.readonly ? [] : ['platform.billing.reconciliation.run', 'platform.billing.reconciliation.manage'])] }} language={control.language} />); }
window.f03 = Object.assign(control, { render, copy: () => reconciliationCopy[control.language], useOrphanFixture: () => { control.orphanCase = structuredClone(producedOrphan); }, switchIdentity: () => { control.user = other; client.financeSessionEpoch++; render(); } });
render();
