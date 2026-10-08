import test from 'node:test';
import assert from 'node:assert/strict';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { parseUsageJSON, encodeUsageJSON, requireUsageDetail, requireUsageReadiness, requireUsagePrecheck, requireUsageCommandResult } from '../src/features/account-platform/admin-usage-governance.js';
import { tokenManagementCopy } from '../src/shared/i18n.js';

const id = '11111111-1111-4111-8111-111111111111';
const version = '2026-10-08T00:00:00.123456Z';
const command = { usage_request_id: id, expected_usage_version: version, evidence_high_watermark: '9007199254740993', replay_model_request: false };
const detail = { usage_request_id: id, updated_at: version, status: 'REVIEW', execution_status: 'REVIEW', settlement_status: 'REVIEW', last_persisted_sequence: command.evidence_high_watermark, maximum_microcredits: '9007199254740993' };

test('Usage int64 source tokens and typed command digits remain exact', () => {
  const value = parseUsageJSON('{"last_persisted_sequence":9007199254740993,"actual_microcredits":9007199254740993,"input_tokens":9007199254740993}');
  assert.equal(value.last_persisted_sequence, '9007199254740993');
  assert.equal(value.actual_microcredits, '9007199254740993');
  assert.equal(value.input_tokens, '9007199254740993');
  assert.ok(encodeUsageJSON({ command, expected_versions: command }).includes('"evidence_high_watermark":9007199254740993'));
  assert.ok(!encodeUsageJSON({ command }).includes('9007199254740992'));
});

test('Usage codec rejects unsafe and out-of-range integers instead of rounding', () => {
  assert.throws(() => encodeUsageJSON({ evidence_high_watermark: 9007199254740992 }));
  for (const text of ['{"last_persisted_sequence":-1}', '{"last_persisted_sequence":1.5}', '{"last_persisted_sequence":9223372036854775808}', '{"maximum_microcredits":1e4}']) assert.throws(() => parseUsageJSON(text));
});

test('Usage detail rejects a foreign target or missing authoritative version', () => {
  assert.equal(requireUsageDetail(detail, id), detail);
  assert.throws(() => requireUsageDetail({ ...detail, usage_request_id: '22222222-2222-4222-8222-222222222222' }, id));
  assert.throws(() => requireUsageDetail({ ...detail, updated_at: null }, id));
});

test('Readiness requires actual active epoch, command identity and typed boolean', () => {
  const ready = { schema_version: 1, command_type: 'TOKEN_USAGE_RECONCILIATION_RETRY', governance_state: 'ACTIVE', governance_epoch: 1, commands_enabled: true };
  assert.equal(requireUsageReadiness(ready), ready);
  assert.throws(() => requireUsageReadiness({ ...ready, governance_epoch: 0 }));
  assert.throws(() => requireUsageReadiness({ ...ready, commands_enabled: 'true' }));
  assert.equal(requireUsageReadiness({ ...ready, governance_state: 'PRE_ACTIVATION', governance_epoch: 0, commands_enabled: false, unavailable_reason: 'GOVERNANCE_INACTIVE' }).commands_enabled, false);
});

test('Usage precheck binds exact usage, CAS timestamp and high-water mark', () => {
  const counts = Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map(key => [key, 0]));
  const impact = { action: 'TOKEN_USAGE_RECONCILIATION_RETRY', precheck_fingerprint: 'a'.repeat(64), requires_cas_revalidation: true, snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', token_observed_at: version, blockers: [], expected_versions: command, evidence: { resource_type: 'USAGE_REQUEST', resource_id: id, current_status: 'REVIEW', current_version: { evidence_etag: `"impact-${'b'.repeat(64)}"`, evidence_set_sha256: 'b'.repeat(64) }, dependencies: counts } };
  const expected = { commandType: impact.action, targetResourceType: 'USAGE_REQUEST', targetResourceID: id, command };
  assert.equal(requireUsagePrecheck(impact, expected), impact);
  for (const changed of [{ evidence_high_watermark: '9007199254740992' }, { expected_usage_version: '2026-10-09T00:00:00Z' }, { usage_request_id: '22222222-2222-4222-8222-222222222222' }]) assert.throws(() => requireUsagePrecheck({ ...impact, expected_versions: { ...command, ...changed } }, expected));
});

test('Actual Admin client selects only five current paths and preserves headers/body', async () => {
  const original = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return new Response('{"items":[],"last_persisted_sequence":9007199254740993}', { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AdminPlatformClient(); client.csrfToken = 'CONTROLLED_SYNTHETIC_CSRF';
    await client.usageRequests({ organization_id: id, q: 'controlled', cursor: 'opaque' });
    assert.ok(!calls[0].url.includes('organization_id'));
    assert.ok(calls[0].url.includes('/usage-requests?limit=50'));
    assert.equal((await client.usageRequest(id)).last_persisted_sequence, '9007199254740993');
    await client.usageEvidence(id, { evidence_type: 'PROVIDER_USAGE' });
    await client.usageReconciliationCases({ case_type: 'USAGE_RECONCILIATION_REVIEW' });
    await client.usageReconciliationCommandReadiness();
    assert.ok(calls[2].url.includes(`/usage-requests/${id}/evidence?`));
    assert.ok(calls[3].url.includes('/reconciliation-cases?'));
    assert.ok(calls[4].url.endsWith('/commands/TOKEN_USAGE_RECONCILIATION_RETRY/readiness'));
    await client.usageReconciliationPrecheck({ command_type: 'TOKEN_USAGE_RECONCILIATION_RETRY', command });
    await client.createUsageReconciliationCommand({ command_type: 'TOKEN_USAGE_RECONCILIATION_RETRY', command }, 'controlled-original-key');
    assert.equal(calls[6].options.headers['Idempotency-Key'], 'controlled-original-key');
    assert.equal(calls[6].options.headers['X-CSRF-Token'], client.csrfToken);
    assert.ok(calls[6].options.body.includes('"evidence_high_watermark":9007199254740993'));
    assert.equal(calls[6].options.cache, 'no-store');
    await assert.rejects(async () => client.usageRequest('../foreign'));
    await client.reconciliationCases({ run_id: id });
    assert.ok(calls[7].url.startsWith('/api/admin/billing/admin/v1/reconciliation-cases?'));
    assert.equal(calls.length, 8);
  } finally { globalThis.fetch = original; }
});

test('Successful governance result must retain the exact queued usage and prohibit replay', () => {
  const value = { command_reference: id, command_type: 'TOKEN_USAGE_RECONCILIATION_RETRY', policy_version: 1, actor_user_id: id, actor_permission: 'platform.usage.reconcile.retry', authorization_version: 1, idempotency_fingerprint: 'c'.repeat(64), target_resource_type: 'USAGE_REQUEST', target_resource_id: id, status: 'SUCCEEDED', redacted_result: { usage_request_id: id, status: 'QUEUED', evidence_high_watermark: command.evidence_high_watermark, replay_model_request: false }, audit_request_id: 'CONTROLLED_AUDIT', created_at: version, completed_at: version };
  assert.equal(requireUsageCommandResult(value, id, command.evidence_high_watermark), value);
  for (const change of [{ usage_request_id: '22222222-2222-4222-8222-222222222222' }, { evidence_high_watermark: '9007199254740992' }, { replay_model_request: true }, { status: 'SETTLED' }]) assert.throws(() => requireUsageCommandResult({ ...value, redacted_result: { ...value.redacted_result, ...change } }, id, command.evidence_high_watermark));
});

test('All three languages retain complete usage governance labels', () => {
  for (const language of ['zh', 'en', 'ja']) for (const key of ['usageRequests', 'usageDetail', 'evidenceHistory', 'reconciliationQueue', 'retryReconciliation', 'actionRecoverSameKey', 'commandReadinessUnavailable', 'operationsViews']) assert.equal(typeof tokenManagementCopy[language].modelToken[key], 'string', `${language}:${key}`);
});
