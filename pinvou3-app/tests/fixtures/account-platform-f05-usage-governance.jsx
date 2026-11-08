import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminManagementWorkspace } from '../../src/features/account-platform/AdminManagementWorkspace.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { encodeUsageJSON, parseUsageJSON } from '../../src/features/account-platform/admin-usage-governance.js';
import { adminPlatformCopy, tokenManagementCopy } from '../../src/shared/i18n.js';

const id = '11111111-1111-4111-8111-111111111111';
const other = '22222222-2222-4222-8222-222222222222';
const version = '2026-10-08T00:00:00.123456Z';
const type = 'TOKEN_USAGE_RECONCILIATION_RETRY';
const root = createRoot(document.querySelector('#root'));
const client = new AdminPlatformClient(); client.csrfToken = 'CONTROLLED_SYNTHETIC_CSRF';
const control = { id, other, language: 'en', calls: [], canRead: true, canRetry: true, ready: true, blocker: false, foreignPrecheck: false, unknown: false, receipt: null, listFailure: 0, owner: id, canTokens: false, delivery: 'COMPLETED' };
const detail = { usage_request_id: id, updated_at: version, started_at: version, status: 'REVIEW', execution_status: 'REVIEW', settlement_status: 'REVIEW', delivery_status: 'COMPLETED', model: 'CONTROLLED_MODEL', maximum_microcredits: '9007199254740993', actual_microcredits: '9007199254740993', last_persisted_sequence: '9007199254740993', input_tokens: '9007199254740993', cached_input_tokens: '0', output_tokens: '1', reconciliation_attempts: 1, token_account_id: other, reconciliation_last_error: 'MANUAL_RECONCILIATION_REQUIRED' };
const json = (value, status = 200) => new Response(encodeUsageJSON(value), { status, headers: { 'Content-Type': 'application/json' } });
const collection = items => ({ items, next_cursor: null, has_more: false, limit: 50, as_of: version });
const counts = Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map(key => [key, 0]));
globalThis.fetch = async (target, options = {}) => {
  const url = new URL(target, location.href); const path = url.pathname; const method = options.method || 'GET';
  control.calls.push({ path, query: url.search, method, body: options.body, key: options.headers?.['Idempotency-Key'], csrf: options.headers?.['X-CSRF-Token'] });
  const base = '/api/admin/token/admin/v1';
  if (method === 'GET') {
    if (path === `${base}/provisionings`) return json(collection([]));
    if (path === `${base}/usage-requests`) {
      if (control.listFailure) return json({ code: 'SNAPSHOT_EXPIRED', request_id: 'CONTROLLED_REQUEST' }, control.listFailure);
      return json(collection([detail]));
    }
    if (path === `${base}/usage-requests/${id}`) return json({ ...detail, delivery_status: control.delivery });
    if (path === `${base}/usage-requests/${id}/evidence`) return json(collection([{ source_kind: 'PROVIDER_USAGE', source_id: id, observed_at: version, evidence_type: 'TRUSTED_USAGE', evidence: { actual_microcredits: '9007199254740993' } }]));
    if (path === `${base}/reconciliation-cases`) return json(collection([{ case_id: other, resource_id: id, case_type: 'USAGE_RECONCILIATION_REVIEW', severity: 'P1', status: 'OPEN', resolution_due_at: version }]));
    if (path === `${base}/commands/${type}/readiness`) return json({ schema_version: 1, command_type: type, governance_state: control.ready ? 'ACTIVE' : 'PRE_ACTIVATION', governance_epoch: control.ready ? 1 : 0, commands_enabled: control.ready, ...(control.ready ? {} : { unavailable_reason: 'GOVERNANCE_INACTIVE' }) });
    if (path === `${base}/commands/by-idempotency-key`) {
      if (!control.receipt || options.headers?.['Idempotency-Key'] !== control.receipt.key || url.searchParams.get('command_type') !== type) return json({ code: 'NOT_FOUND' }, 404);
      return json(control.receipt.value);
    }
    if (path === `${base}/commands/${other}` && control.receipt) return json(control.receipt.value);
    throw new Error('UNEXPECTED_CONTROLLED_GET');
  }
  if (method !== 'POST' || options.headers?.['X-CSRF-Token'] !== client.csrfToken) throw new Error('INVALID_CONTROLLED_WRITE');
  const input = parseUsageJSON(options.body);
  if (path === `${base}/commands:precheck`) {
    if (input.command_type !== type || input.target_resource_type !== 'USAGE_REQUEST' || input.target_resource_id !== id || input.command.replay_model_request !== false || input.command.evidence_high_watermark !== detail.last_persisted_sequence) return json({ code: 'INVALID_COMMAND' }, 400);
    return json({ action: type, precheck_fingerprint: 'a'.repeat(64), requires_cas_revalidation: true, snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', token_observed_at: version, blockers: control.blocker ? ['USAGE_RECONCILIATION_DEBIT_PROHIBITED'] : [], expected_versions: { usage_request_id: id, expected_usage_version: version, evidence_high_watermark: detail.last_persisted_sequence }, evidence: { resource_type: 'USAGE_REQUEST', resource_id: control.foreignPrecheck ? other : id, current_status: 'REVIEW', current_version: { evidence_etag: `"impact-${'b'.repeat(64)}"`, evidence_set_sha256: 'b'.repeat(64) }, dependencies: counts } });
  }
  if (path === `${base}/commands`) {
    if (input.command_type !== type || input.command.replay_model_request !== false || input.target_resource_id !== id || input.expected_versions.evidence_high_watermark !== detail.last_persisted_sequence || !options.headers?.['Idempotency-Key']) return json({ code: 'INVALID_COMMAND' }, 400);
    control.receipt = { key: options.headers['Idempotency-Key'], value: { command_reference: other, command_type: type, policy_version: 1, actor_user_id: id, actor_permission: 'platform.usage.reconcile.retry', authorization_version: 1, idempotency_fingerprint: 'c'.repeat(64), target_resource_type: 'USAGE_REQUEST', target_resource_id: id, status: 'SUCCEEDED', redacted_result: { usage_request_id: id, status: 'QUEUED', evidence_high_watermark: detail.last_persisted_sequence, replay_model_request: false }, audit_request_id: 'CONTROLLED_AUDIT', created_at: version, completed_at: version } };
    if (control.unknown) throw new Error('CONTROLLED_LOST_ACK');
    return json(control.receipt.value);
  }
  throw new Error('UNEXPECTED_CONTROLLED_POST');
};
control.labels = () => tokenManagementCopy[control.language].modelToken;
control.render = () => root.render(<AdminManagementWorkspace key={control.owner} client={client} copy={adminPlatformCopy[control.language]} language={control.language} permissions={[...(control.canTokens ? ['platform.tokens.read'] : []), ...(control.canRead ? ['platform.usage.read'] : []), ...(control.canRetry ? ['platform.usage.reconcile.retry'] : [])]} owner={control.owner} credentialOrigin="https://controlled.invalid" />);
globalThis.f05Usage = control;
control.render();
