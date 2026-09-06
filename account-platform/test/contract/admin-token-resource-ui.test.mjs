import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { AdminPlatformClient } from '../../../pinvou3-app/src/features/account-platform/admin-client.js';
import { adminTokenResourceFormsGateEnabled, normalizeAdminModelTokenEntry, readAdminModelTokenQuery } from '../../../pinvou3-app/src/features/account-platform/admin-navigation.js';
import { executeStableRouteCommand, ownerCandidateQuery, routeEditorVisible, serverRouteHealth, stableRouteChange, stableRouteChoices, tokenAccountCreateInput, tokenResourceAccess } from '../../../pinvou3-app/src/features/account-platform/admin-token-resource-state.js';
import { governedTokenEnvelope, requireGovernedTokenPrecheck } from '../../../pinvou3-app/src/features/account-platform/admin-high-risk-action-state.js';

const modelID = '0199a100-0001-7000-8000-000000000101';
const protocolID = '0199a100-0001-7000-8000-000000000102';
const organizationID = '0199a100-0001-7000-8000-000000000103';
const unitID = '0199a100-0001-7000-8000-000000000104';
const repositoryRoot = path.resolve(import.meta.dirname, '../../..');

test('APT-129 creation permission matrix fails closed without directory fallback', () => {
  assert.deepEqual(tokenResourceAccess(['platform.tokens.manage'], true), {
    manage: true, ownerCandidates: false, models: false, showCreate: false, explainCreateBlocked: true, requestOwnerCandidates: false, showRouteWrite: false, explainRouteBlocked: true,
  });
  assert.equal(tokenResourceAccess(['platform.tokens.owner-candidates.read'], true).showCreate, false);
  assert.equal(tokenResourceAccess(['platform.tokens.owner-candidates.read'], true).requestOwnerCandidates, false);
  assert.equal(tokenResourceAccess(['platform.tokens.manage', 'platform.tokens.owner-candidates.read'], false).showCreate, false);
  assert.equal(tokenResourceAccess(['platform.tokens.manage', 'platform.tokens.owner-candidates.read'], false).requestOwnerCandidates, false);
  const complete = tokenResourceAccess(['platform.tokens.manage', 'platform.tokens.owner-candidates.read', 'platform.models.read'], true);
  assert.equal(complete.showCreate, true);
  assert.equal(complete.requestOwnerCandidates, true);
  assert.equal(complete.showRouteWrite, true);
  assert.equal(routeEditorVisible(complete, true), true);
  assert.equal(routeEditorVisible(tokenResourceAccess(['platform.tokens.manage'], true), true), false);
  assert.equal(routeEditorVisible(tokenResourceAccess(['platform.tokens.manage', 'platform.models.read'], false), true), false);
  assert.equal(adminTokenResourceFormsGateEnabled({ feature_flags: { admin_token_resource_forms: true } }), true);
  assert.equal(adminTokenResourceFormsGateEnabled({ feature_flags: { admin_token_resource_forms: false } }), false);
});

test('owner selection pages user, organization, and unit authorities and builds closed create payloads', () => {
  assert.deepEqual(ownerCandidateQuery('USER', { q: ' Alice ', status: 'ACTIVE', cursor: 'signed' }), { q: 'Alice', owner_type: 'USER', status: 'ACTIVE', organization_id: '', cursor: 'signed' });
  assert.equal(ownerCandidateQuery('ORGANIZATION', {}, '').owner_type, 'ORGANIZATION');
  assert.deepEqual(ownerCandidateQuery('UNIT', { q: '研发' }, ''), { q: '研发', owner_type: 'ORGANIZATION', status: 'ACTIVE', organization_id: '', cursor: '' });
  assert.deepEqual(ownerCandidateQuery('UNIT', { q: '研发' }, organizationID), { q: '研发', owner_type: 'UNIT', status: 'ACTIVE', organization_id: organizationID, cursor: '' });
  const unit = { id: unitID, type: 'UNIT', display_label: '研发一部', status: 'ACTIVE' };
  assert.deepEqual(tokenAccountCreateInput({ candidate: unit, organization: { id: organizationID, type: 'ORGANIZATION' }, name: '生产路由', priority: 120 }), {
    owner_type: 'ORG_UNIT', owner_id: unitID, owner_organization_id: organizationID, owner_unit_id: unitID, name: '生产路由', share_scope: 'OWNER_SUBTREE', default_priority: 120,
  });
  assert.equal(tokenAccountCreateInput({ candidate: unit, organization: null, name: '生产路由' }), null);
  assert.equal(tokenAccountCreateInput({ candidate: { ...unit, status: 'DISABLED' }, organization: { id: organizationID, type: 'ORGANIZATION' }, name: '生产路由' }), null);
});

test('account create and status changes use governed prechecks and stable command envelopes', async () => {
  const dependencies = Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map((key) => [key, 0]));
  const precheck = {
    action: 'TOKEN_ACCOUNT_CREATE', blockers: [], precheck_fingerprint: 'a'.repeat(64), requires_cas_revalidation: true,
    snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', token_observed_at: '2026-09-05T12:00:00Z',
    evidence: { resource_type: 'TOKEN_ACCOUNT', resource_id: modelID, current_status: 'CREATABLE', current_version: { evidence_etag: `"impact-${'b'.repeat(64)}"`, evidence_set_sha256: 'b'.repeat(64) }, dependencies, observed_at: '2026-09-05T12:00:00Z' },
    expected_versions: { owner_type: 'USER', owner_id: modelID, owner_authorization_version: 9, owner_account_count: 19, owner_account_cap: 20, owner_account_cap_policy_version: 2, creation_version: 0 },
  };
  assert.equal(requireGovernedTokenPrecheck(precheck, { commandType: 'TOKEN_ACCOUNT_CREATE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: modelID }), precheck);
  const envelope = await governedTokenEnvelope({ commandType: 'TOKEN_ACCOUNT_CREATE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: modelID, reason: 'create the reviewed account', impact: precheck, command: { owner_type: 'USER', owner_id: modelID, owner_organization_id: null, owner_unit_id: null, name: 'Primary', share_scope: 'OWNER_UNIT_ONLY', default_priority: 100 } });
  assert.equal(envelope.command_type, 'TOKEN_ACCOUNT_CREATE');
  assert.equal(envelope.expected_versions.precheck_fingerprint, precheck.precheck_fingerprint);
  assert.match(envelope.expected_versions.request_fingerprint, /^[0-9a-f]{64}$/);
  assert.equal(Object.hasOwn(envelope.command, 'authorization_version'), false);
});

test('stable route selector emits stable IDs and never accepts health, upstream, or display text', () => {
  const choices = stableRouteChoices([
    { model_id: modelID, protocol_definition_id: protocolID, display_name: 'Flash', lifecycle_status: 'PUBLISHED', source_available: true, protocol_code: 'CHAT', protocol_version: 2, upstream_model_ref: 'private-ref' },
    { model_id: '0199a100-0001-7000-8000-000000000105', protocol_definition_id: '0199a100-0001-7000-8000-000000000106', display_name: 'Removed', lifecycle_status: 'REMOVED', source_available: true },
  ]);
  assert.deepEqual(choices, [{ modelID, displayName: 'Flash', protocolVersionID: protocolID, protocolCode: 'CHAT', protocolVersion: 2 }]);
  const body = stableRouteChange({ choice: choices[0], accountVersion: 7, existingRoute: { route_version: 3 }, priority: 90, defaultScope: 'ORG', reason: 'route the published model safely' });
  assert.deepEqual(body, { model_id: modelID, protocol_version_id: protocolID, priority: 90, default_scope: 'ORG', expected_account_version: 7, expected_route_version: 3, reason: 'route the published model safely' });
  for (const forbidden of ['model', 'display_name', 'health_status', 'provider', 'upstream', 'upstream_model_ref']) assert.equal(Object.hasOwn(body, forbidden), false);
  assert.equal(stableRouteChange({ choice: choices[0], accountVersion: 7, priority: 90, defaultScope: 'ORG', reason: 'short' }), null);
});

test('health projection expires HEALTHY evidence locally and never invents routability', () => {
  const asOf = '2026-09-04T12:00:00Z';
  assert.deepEqual(serverRouteHealth({ health_status: 'HEALTHY', health_diagnostic_code: 'PROBE_SUCCEEDED', health_observed_at: '2026-09-04T11:58:00Z', health_expires_at: '2026-09-04T12:03:00Z' }, asOf), {
    status: 'HEALTHY', diagnostic: 'PROBE_SUCCEEDED', observedAt: '2026-09-04T11:58:00Z', expiresAt: '2026-09-04T12:03:00Z', routable: true,
  });
  assert.equal(serverRouteHealth({ health_status: 'HEALTHY', health_observed_at: '2026-09-04T11:58:00Z', health_expires_at: '2026-09-04T11:59:59Z' }, asOf).status, 'STALE');
  assert.equal(serverRouteHealth({ health_status: 'UNKNOWN', health_observed_at: '2026-09-04T11:58:00Z', health_expires_at: '2026-09-04T12:03:00Z' }, asOf).routable, false);
  assert.equal(serverRouteHealth({ health_status: 'made-up', health_diagnostic_code: '<secret>' }, asOf).status, 'UNKNOWN');
  for (const invalid of [
    { health_status: 'HEALTHY' },
    { health_status: 'HEALTHY', health_observed_at: 'invalid', health_expires_at: '2026-09-04T12:03:00Z' },
    { health_status: 'HEALTHY', health_observed_at: '2026-09-04T12:03:00Z', health_expires_at: '2026-09-04T12:02:00Z' },
    { health_status: 'HEALTHY', health_observed_at: '2026-09-04T11:55:00Z', health_expires_at: '2026-09-04T12:01:00Z' },
    { health_status: 'HEALTHY', health_observed_at: '2026-09-04T12:00:00.001Z', health_expires_at: '2026-09-04T12:03:00Z' },
  ]) {
    const projected = serverRouteHealth(invalid, asOf);
    assert.equal(projected.status, 'UNKNOWN');
    assert.equal(projected.routable, false);
    assert.equal(projected.observedAt, null);
    assert.equal(projected.expiresAt, null);
  }
  assert.equal(serverRouteHealth({ health_status: 'HEALTHY', health_observed_at: asOf, health_expires_at: '2026-09-04T12:05:00Z' }, asOf).routable, true, 'the exact database boundary is accepted');
  assert.equal(serverRouteHealth({ health_status: 'HEALTHY', health_observed_at: '2026-09-04T11:59:00Z', health_expires_at: '2026-09-04T12:01:00Z' }).status, 'UNKNOWN', 'missing authoritative as_of fails closed');
});

test('owner and route deep links remain stable while revoked detail state is normalized away', () => {
  const location = { pathname: '/admin.html', search: `?section=tokenResources&owner_candidate_type=UNIT&owner_candidate_organization_id=${organizationID}&owner_candidate_q=研发&owner_candidate_cursor=signed&token_account_id=${modelID}&token_detail_tab=routes`, hash: '' };
  assert.deepEqual(readAdminModelTokenQuery('ownerCandidates', location), { q: '研发', owner_type: 'UNIT', status: '', organization_id: organizationID, cursor: 'signed' });
  const allowed = normalizeAdminModelTokenEntry('tokenResources', ['tokenResources'], true, location, ['platform.tokens.read', 'platform.tokens.manage', 'platform.tokens.owner-candidates.read'], { tokenResourceFormsEnabled: true, tokenDetailsEnabled: true });
  assert.equal(allowed.updates.token_account_id, undefined);
  assert.equal(allowed.updates.owner_candidate_q, undefined);
  const fineGrainedRevoked = normalizeAdminModelTokenEntry('tokenResources', ['tokenResources'], true, location, ['platform.tokens.read', 'platform.tokens.manage'], { tokenResourceFormsEnabled: true, tokenDetailsEnabled: true });
  assert.equal(fineGrainedRevoked.section, 'tokenResources');
  assert.equal(fineGrainedRevoked.updates.owner_candidate_q, null);
  assert.equal(fineGrainedRevoked.updates.owner_candidate_cursor, null);
  assert.equal(fineGrainedRevoked.updates.token_account_id, undefined);
  const gateOff = normalizeAdminModelTokenEntry('tokenResources', ['tokenResources'], true, location, ['platform.tokens.read', 'platform.tokens.manage', 'platform.tokens.owner-candidates.read'], { tokenResourceFormsEnabled: false, tokenDetailsEnabled: true });
  assert.equal(gateOff.updates.owner_candidate_type, null);
  const clean = normalizeAdminModelTokenEntry('tokenResources', ['tokenResources'], true, { pathname: '/admin.html', search: '?section=tokenResources', hash: '' }, ['platform.tokens.read', 'platform.tokens.manage'], { tokenResourceFormsEnabled: true, tokenDetailsEnabled: true });
  assert.equal(clean.replace, false);
  assert.deepEqual(clean.updates, {});
  const revoked = normalizeAdminModelTokenEntry('tokenResources', ['models'], true, location, ['platform.models.read']);
  assert.equal(revoked.updates.token_account_id, null);
	const legacyRouteHealth = normalizeAdminModelTokenEntry('tokenResources', ['tokenResources'], true, location, ['platform.tokens.read', 'platform.tokens.manage'], { tokenResourceFormsEnabled: true, tokenDetailsEnabled: false });
	assert.equal(legacyRouteHealth.updates.token_account_id, undefined);
	assert.equal(legacyRouteHealth.updates.token_detail_tab, 'routes');
  assert.equal(revoked.updates.owner_candidate_q, null);
});

test('stable route submission freezes idempotency, blocks double dispatch, and fails closed on unknown outcomes', async () => {
  const payload = { model_id: modelID, protocol_version_id: protocolID, expected_account_version: 2, expected_route_version: 0, priority: 100, default_scope: 'NONE', reason: 'configure a stable route safely' };
  const holder = { current: { command: null, inflight: false } };
  let release;
  let sends = 0;
  const first = executeStableRouteCommand(holder, payload, async () => { sends += 1; return new Promise((resolve) => { release = resolve; }); }, () => '0199a100-0001-7000-8000-000000000201');
  const duplicate = await executeStableRouteCommand(holder, payload, async () => { sends += 1; }, () => '0199a100-0001-7000-8000-000000000202');
  assert.equal(duplicate.duplicate, true);
  assert.equal(sends, 1);
  release({});
  assert.equal((await first).ok, true);

  const unknownHolder = { current: { command: null, inflight: false } };
  const lost = await executeStableRouteCommand(unknownHolder, payload, async () => { throw Object.assign(new Error('network'), { status: 0 }); }, () => '0199a100-0001-7000-8000-000000000203');
  assert.equal(lost.error.code, 'COMMAND_RESULT_UNKNOWN');
  const retainedKey = unknownHolder.current.command.idempotencyKey;
  const locked = await executeStableRouteCommand(unknownHolder, payload, async () => { throw new Error('must not send'); }, () => '0199a100-0001-7000-8000-000000000204');
  assert.equal(locked.error.code, 'COMMAND_RESULT_UNKNOWN');
  assert.equal(unknownHolder.current.command.idempotencyKey, retainedKey);
  const serverFailureHolder = { current: { command: null, inflight: false } };
  const serverFailure = await executeStableRouteCommand(serverFailureHolder, payload, async () => { throw Object.assign(new Error('server'), { status: 500, code: 'INTERNAL_ERROR' }); }, () => '0199a100-0001-7000-8000-000000000208');
  assert.equal(serverFailure.error.code, 'COMMAND_RESULT_UNKNOWN');
  assert.equal(serverFailureHolder.current.command.locked, true);
  const mismatchedPairHolder = { current: { command: null, inflight: false } };
  const mismatchedPair = await executeStableRouteCommand(mismatchedPairHolder, payload, async () => { throw Object.assign(new Error('mismatch'), { status: 409, code: 'INTERNAL_ERROR' }); }, () => '0199a100-0001-7000-8000-000000000209');
  assert.equal(mismatchedPair.error.code, 'COMMAND_RESULT_UNKNOWN', 'an allowlisted status with the wrong code cannot prove zero writes');
  assert.equal(mismatchedPairHolder.current.command.locked, true);

  const safeHolder = { current: { command: null, inflight: false } };
  const keys = [];
  await executeStableRouteCommand(safeHolder, payload, async (_body, key) => { keys.push(key); throw Object.assign(new Error('conflict'), { status: 409, code: 'TOKEN_ROUTE_VERSION_CONFLICT' }); }, () => '0199a100-0001-7000-8000-000000000205');
  await executeStableRouteCommand(safeHolder, payload, async (_body, key) => { keys.push(key); return {}; }, () => '0199a100-0001-7000-8000-000000000206');
  assert.equal(keys[0], keys[1]);
  const changedHolder = { current: { command: null, inflight: false } };
  await executeStableRouteCommand(changedHolder, payload, async () => { throw Object.assign(new Error('conflict'), { status: 409, code: 'TOKEN_ROUTE_VERSION_CONFLICT' }); }, () => '0199a100-0001-7000-8000-000000000206');
  const changed = { ...payload, priority: 101 };
  const changedResult = await executeStableRouteCommand(changedHolder, changed, async (_body, key) => { keys.push(key); return {}; }, () => '0199a100-0001-7000-8000-000000000207');
  assert.equal(changedResult.ok, true);
  assert.notEqual(changedHolder.current.command.idempotencyKey, 'admin-route-0199a100-0001-7000-8000-000000000206');
});

test('Admin client maps owner paging and stable route writes to exact BFF paths', async () => {
  const previousFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = async (url, init = {}) => { requests.push({ url: String(url), init }); return { ok: true, status: 200, headers: new Headers(), json: async () => ({ items: [] }) }; };
  try {
    const client = new AdminPlatformClient('https://portal.example');
    client.csrfToken = 'csrf';
    await client.tokenOwnerCandidates({ q: '研发', owner_type: 'UNIT', status: 'ACTIVE', organization_id: organizationID, cursor: 'signed' });
    await client.upsertStableTokenRoute(modelID, { model_id: modelID, protocol_version_id: protocolID }, 'admin-route-fixed-key');
  } finally { globalThis.fetch = previousFetch; }
  assert.equal(requests[0].url, `https://portal.example/api/admin/token/admin/v1/owner-candidates?limit=50&q=%E7%A0%94%E5%8F%91&owner_type=UNIT&status=ACTIVE&organization_id=${organizationID}&cursor=signed`);
  assert.equal(requests[1].url, `https://portal.example/api/admin/token/admin/v2/token-accounts/${modelID}/routes`);
  assert.equal(requests[1].init.method, 'PUT');
  assert.equal(requests[1].init.headers['Idempotency-Key'], 'admin-route-fixed-key');
});

test('Token resource dialogs are keyboard-contained, translated, and collapse at the narrow layout boundary', async () => {
  const [workspace, styles, catalogs, runtimeSmoke, packageJSON, smokeSelector] = await Promise.all([
    readFile(path.join(repositoryRoot, 'pinvou3-app/src/features/account-platform/AdminTokenResourcesWorkspace.jsx'), 'utf8'),
    readFile(path.join(repositoryRoot, 'pinvou3-app/src/features/account-platform/admin-platform.css'), 'utf8'),
    readFile(path.join(repositoryRoot, 'pinvou3-app/src/shared/admin-platform-i18n.js'), 'utf8'),
    readFile(path.join(repositoryRoot, 'pinvou3-app/tests/admin_token_resources_ui_smoke.mjs'), 'utf8'),
    readFile(path.join(repositoryRoot, 'pinvou3-app/package.json'), 'utf8'),
    readFile(path.join(repositoryRoot, 'scripts/select-frontend-smokes.mjs'), 'utf8'),
  ]);
  assert.match(workspace, /role="dialog" aria-modal="true" aria-labelledby=/);
  assert.match(workspace, /event\.key === 'Escape'/);
  assert.match(workspace, /event\.key !== 'Tab'/);
  assert.match(workspace, /previousFocus\.current\?\.focus/);
  assert.match(workspace, /access\.requestOwnerCandidates && ownerQuery\.owner_type/);
  assert.match(workspace, /showRouteDialog && state\.item && <RouteDialog/);
  assert.match(workspace, /if \(!access\.showRouteWrite\) setRouteDialog\(false\)/);
	assert.match(workspace, /command_type: 'TOKEN_ACCOUNT_CREATE'/);
	assert.match(workspace, /command_type: 'TOKEN_ACCOUNT_STATUS_CHANGE'/);
	assert.doesNotMatch(workspace, /client\.createTokenAccount\(input\)/);
	assert.doesNotMatch(workspace, /client\.setTokenAccountStatus\(/);
  assert.doesNotMatch(workspace, /route\.(?:upstream|provider|credential_secret)/);
  assert.match(styles, /@media\(max-width:760px\)[\s\S]*\.adm-token-dialog-grid,[^\n]*grid-template-columns:1fr/);
  for (const marker of ['candidateAborted', 'modelAborted', 'replaceCount', 'routeCalls', 'Admin Token resources UI smoke passed']) assert.ok(runtimeSmoke.includes(marker));
  assert.equal(JSON.parse(packageJSON).scripts['test:admin-token-resources-ui'], 'node tests/admin_token_resources_ui_smoke.mjs');
  assert.match(smokeSelector, /command\("npm", "test:admin-token-resources-ui"\)/);
  for (const phrase of ['创建 Token Account', 'Create Token Account', 'Token Account を作成']) assert.ok(catalogs.includes(phrase), `missing translated resource text: ${phrase}`);
});
