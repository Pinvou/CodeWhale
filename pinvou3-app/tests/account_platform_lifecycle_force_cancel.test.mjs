import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { requireTokenApprovalImpact } from '../src/features/account-platform/admin-high-risk-action-state.js';
import { tokenManagementCopy, financeCopy } from '../src/shared/i18n.js';
import { currentApprovalEditDraft } from '../src/features/account-platform/admin-finance.js';

const id = '11111111-1111-4111-8111-111111111111';
const replacement = '22222222-2222-4222-8222-222222222222';
const hash = 'a'.repeat(64);
const require = createRequire(import.meta.url);
const Ajv = require('ajv');

test('published lifecycle input validates the supported conditional subset and legacy defaults', () => {
  const specification = JSON.parse(readFileSync(new URL('../../account-platform/contracts/openapi/token-management.yaml', import.meta.url), 'utf8'));
  assert.equal(specification.openapi, '3.1.0');
  const route = specification.paths['/admin/v1/models/{model_id}/lifecycle-plans'].post;
  assert.equal(route.requestBody.content['application/json'].schema.$ref, '#/components/schemas/LifecyclePlanInput');
  const schema = specification.components.schemas.LifecyclePlanInput;
  // AJV6 validates this common Draft7/2020-12 subset (if/then, const, types).
  // This is not a claim to validate every OpenAPI3.1 keyword in the document.
  const ajv = new Ajv({ allErrors: true, unknownFormats: 'ignore' });
  const validate = ajv.compile(schema);
  const request = { expected_catalog_version: 1, target_lifecycle: 'REMOVED', effective_at: '2026-11-01T12:00:00Z', reason: 'reviewed removal' };
  assert.equal(schema.properties.force_cancel_inflight.default, false);
  for (const target of ['DEPRECATED', 'REMOVED']) {
    assert.equal(validate({ ...request, target_lifecycle: target }), true, `${target}: omitted flag`);
    assert.equal(validate({ ...request, target_lifecycle: target, force_cancel_inflight: false }), true, `${target}: explicit false`);
  }
  assert.equal(validate({ ...request, force_cancel_inflight: true }), true);
  for (const input of [
    { ...request, target_lifecycle: 'DEPRECATED', force_cancel_inflight: true },
    { ...request, target_lifecycle: 'UNKNOWN', force_cancel_inflight: true },
    { ...request, force_cancel_inflight: 'true' },
    { ...request, force_cancel_inflight: null },
    { ...request, force_cancel_inflight: 1 },
    { ...request, force_cancel_inflight: true, bypass: true },
  ]) assert.equal(validate(input), false);
  const baseline = structuredClone(schema);
  baseline.properties.force_cancel_inflight.enum = [false];
  assert.equal(ajv.compile(baseline)({ ...request, force_cancel_inflight: true }), false, 'original false-only schema must reject the valid new input');
});
function impact(version = 3, flag = true) {
  const dependencies = Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map(key => [key, 0]));
  dependencies.inflight_requests = 1;
  return { action: 'MODEL_LIFECYCLE_REMOVED', precheck_fingerprint: hash, requires_cas_revalidation: true, snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', blockers: [], token_observed_at: '2026-11-01T11:58:00Z', evidence: { resource_type: 'MODEL', resource_id: id, current_status: 'PUBLISHED', current_version: { evidence_etag: `"impact-${hash}"`, evidence_set_sha256: hash }, dependencies }, critical_resource_versions: { model_id: id, catalog_version: 1, current_lifecycle: 'PUBLISHED', target_lifecycle: 'REMOVED', lifecycle_plan_version: 1, effective_at: '2026-11-01T12:00:00Z', existing_session_cutoff_at: '2026-11-01T12:00:00Z', replacement_model_id: replacement, billing_price_version: 5, route_set_fingerprint: hash, provisioning_set_fingerprint: hash, inflight_request_set_fingerprint: hash, policy_version: version, ...(version === 3 ? { force_cancel_inflight: flag } : {}) }, financial_snapshot: { billing_price_revision: 5, price_set_fingerprint: hash, models_checked: 2, models_covered: 2, fully_covered: true, billing_as_of: '2026-11-01T11:58:00Z' } };
}
const expected = { action: 'MODEL_LIFECYCLE_REMOVED', commandType: 'TOKEN_MODEL_LIFECYCLE_CHANGE', targetResourceID: id, targetStatus: 'REMOVED', forceCancelInflight: true };

test('v3 impact freezes an explicit boolean and the persisted removal choice', () => {
  assert.equal(requireTokenApprovalImpact(impact(), expected).critical_resource_versions.force_cancel_inflight, true);
  for (const flag of [null, 'true', 1, false]) assert.throws(() => requireTokenApprovalImpact(impact(3, flag), expected));
  assert.throws(() => requireTokenApprovalImpact(impact(2), expected));
  const missing = impact(); delete missing.critical_resource_versions.force_cancel_inflight;
  assert.throws(() => requireTokenApprovalImpact(missing, expected));
  const unknown = impact(); unknown.critical_resource_versions.bypass = true;
  assert.throws(() => requireTokenApprovalImpact(unknown, expected));
  const old = impact(2); old.critical_resource_versions.force_cancel_inflight = true;
  assert.throws(() => requireTokenApprovalImpact(old, expected));
  assert.equal(requireTokenApprovalImpact(impact(2), { ...expected, forceCancelInflight: false }).critical_resource_versions.policy_version, 2);
});

test('editing an approval preserves its frozen true intent and dependency facts', async () => {
  const facts = impact();
  const command = { model_id: id, lifecycle_plan_id: replacement, lifecycle_plan_version: 1, target_lifecycle: 'REMOVED', force_cancel_inflight: true };
  const detail = { approval_request_id: id, status: 'DRAFT', allowed_actions: ['EDIT'], command_type: expected.commandType, command, target: { resource_type: 'MODEL', resource_id: id }, critical_resource_versions: facts.critical_resource_versions, financial_snapshot: facts.financial_snapshot };
  const draft = await currentApprovalEditDraft({}, detail, 'reviewed removal cancellation reason');
  assert.deepEqual(draft.normalized_command, command);
  assert.deepEqual(draft.critical_resource_versions, facts.critical_resource_versions);
});

test('both management and final approval communicate cancellation and billing in three languages', () => {
  for (const language of ['zh', 'en', 'ja']) {
    for (const copy of [tokenManagementCopy[language].modelToken, financeCopy[language]]) {
      for (const key of ['forceCancelInflight', 'forceCancelInflightHelp', 'forceCancelEnabled', 'forceCancelDisabled']) assert.equal(typeof copy[key], 'string', `${language}:${key}`);
      assert.notEqual(copy.forceCancelEnabled, copy.forceCancelDisabled);
    }
  }
});
