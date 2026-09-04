import assert from 'node:assert/strict';
import test from 'node:test';
import {
  classifyHighRiskFailure,
  executeHighRiskCommand,
  governedTokenEnvelope,
  highRiskPayloadKey,
  prepareHighRiskCommand,
  requireGovernedTokenCommandResult,
  requireTokenApprovalImpact,
  validApprovalRequest,
  validGovernedTokenCommandResult,
  validImpactPrecheck,
  validStableRouteWriteResult,
} from '../../../pinvou3-app/src/features/account-platform/admin-high-risk-action-state.js';
import { chromiumUnsafePorts, listenOnChromiumSafePort } from '../../../pinvou3-app/tests/chromium-safe-port.mjs';

const fingerprint = 'a'.repeat(64);
const impact = {
  action: 'TOKEN_ROUTE_DELETE',
  precheck_fingerprint: fingerprint,
  requires_cas_revalidation: true,
  snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS',
  token_observed_at: '2026-09-04T12:00:00Z',
  blockers: [],
  evidence: {
    resource_type: 'TOKEN_ROUTE',
    resource_id: '0199a300-0001-7000-8000-000000000001',
    current_status: 'ACTIVE',
    current_version: { evidence_etag: `"impact-${'b'.repeat(64)}"`, evidence_set_sha256: 'c'.repeat(64) },
    dependencies: Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map((key) => [key, 0])),
  },
};

test('impact precheck is action-bound and fails closed on incomplete evidence', () => {
  assert.equal(validImpactPrecheck(impact, 'TOKEN_ROUTE_DELETE'), true);
  assert.equal(validImpactPrecheck({ ...impact, action: 'TOKEN_ROUTE_UPSERT' }, 'TOKEN_ROUTE_DELETE'), false);
  assert.equal(validImpactPrecheck({ ...impact, evidence: { ...impact.evidence, resource_type: 'TOKEN_ACCOUNT' } }, 'TOKEN_ROUTE_DELETE'), false);
  assert.equal(validImpactPrecheck({ ...impact, evidence: { ...impact.evidence, dependencies: { ...impact.evidence.dependencies, routes: -1 } } }, 'TOKEN_ROUTE_DELETE'), false);
  assert.equal(validImpactPrecheck({ ...impact, precheck_fingerprint: 'not-a-digest' }, 'TOKEN_ROUTE_DELETE'), false);
});

test('governed envelope binds stable versions and precheck without serializing secrets', async () => {
  const envelope = await governedTokenEnvelope({
    commandType: 'TOKEN_ROUTE_DELETE', targetResourceType: 'TOKEN_ROUTE', targetResourceID: impact.evidence.resource_id,
    reason: 'remove a superseded route after review', impact,
    expectedVersions: { route_id: impact.evidence.resource_id, expected_route_version: 3 }, command: { route_id: impact.evidence.resource_id },
  });
  assert.equal(envelope.precheck_fingerprint, fingerprint);
  assert.match(envelope.expected_versions.request_fingerprint, /^[0-9a-f]{64}$/u);
  assert.equal(envelope.expected_versions.precheck_fingerprint, fingerprint);
  assert.doesNotMatch(JSON.stringify(envelope), /secret|api[_-]?key|password/iu);

  const protocol = await governedTokenEnvelope({
    commandType: 'TOKEN_PROTOCOL_DRAFT_CREATE', targetResourceType: 'PROTOCOL', targetResourceID: 'CHAT',
    reason: 'create a reviewed protocol draft', impact: { ...impact, expected_versions: { protocol_code: 'CHAT', expected_protocol_head_version: 0 } },
    command: { protocol_code: 'CHAT', request_schema: { type: 'object' } },
  });
  assert.deepEqual(Object.keys(protocol.expected_versions).sort(), ['expected_protocol_head_version', 'protocol_code', 'request_fingerprint']);
  assert.equal(protocol.precheck_fingerprint, fingerprint);
});

test('double submit dispatches once and unknown outcomes never repeat a write', async () => {
  const holder = { current: { command: null, inflight: false } };
  const payload = { reason: 'retry existing evidence only', precheck: impact };
  let release;
  const pending = new Promise((resolve) => { release = resolve; });
  const calls = [];
  const send = async (_body, key) => { calls.push(key); await pending; return { status: 'OK' }; };
  const first = executeHighRiskCommand(holder, payload, send, () => '00000000-0000-4000-8000-000000000001');
  const duplicate = await executeHighRiskCommand(holder, payload, send);
  assert.equal(duplicate.duplicate, true);
  release();
  assert.equal((await first).ok, true);
  assert.equal(calls.length, 1);

  const unknownHolder = { current: { command: null, inflight: false } };
  const failure = Object.assign(new Error('lost response'), { status: 500, code: 'INTERNAL_ERROR', requestID: 'request-1' });
  const failed = await executeHighRiskCommand(unknownHolder, payload, async () => { throw failure; }, () => '00000000-0000-4000-8000-000000000002');
  assert.equal(failed.command.unknown, true);
  const blocked = await executeHighRiskCommand(unknownHolder, payload, async () => assert.fail('unknown command must not be written again'));
  assert.equal(blocked.recoveryRequired, true);
  const recovered = await executeHighRiskCommand(unknownHolder, payload, async (_body, key) => ({ key }), undefined, { recovery: true });
  assert.equal(recovered.value.key, 'admin-token-action-00000000-0000-4000-8000-000000000002');
  const changed = await executeHighRiskCommand(unknownHolder, { ...payload, reason: 'a changed operation must remain locked' }, async () => assert.fail('must not dispatch'));
  assert.equal(changed.error.code, 'COMMAND_PAYLOAD_LOCKED');
});

test('only exact status and code pairs prove zero write; 504 is always unknown', () => {
  const command = prepareHighRiskCommand(null, { reason: 'validated request' }, () => '00000000-0000-4000-8000-000000000003');
  assert.equal(classifyHighRiskFailure(command, { status: 400, code: 'INVALID_REASON' }).provenNoWrite, true);
  assert.equal(classifyHighRiskFailure(command, { status: 409, code: 'INTERNAL_ERROR' }).unknown, true);
  assert.equal(classifyHighRiskFailure(command, { status: 504, code: 'PERMISSION_DENIED' }).unknown, true);
  assert.equal(classifyHighRiskFailure(command, new TypeError('fetch failed')).unknown, true);
});

test('result lookup failures keep the frozen reference unknown without another write', async () => {
  const payload = { reason: 'query the frozen command result only', precheck: impact };
  const reference = '0199a300-0001-7000-8000-000000000099';
  for (const failure of [
    { status: 404, code: 'NOT_FOUND' },
    { status: 403, code: 'PERMISSION_DENIED' },
    new TypeError('network unavailable'),
  ]) {
    const holder = { current: { command: { idempotencyKey: 'frozen-key', payload, payloadKey: highRiskPayloadKey(payload), dispatched: true, unknown: true, locked: true, error: { code: 'COMMAND_RESULT_UNKNOWN', commandReference: reference } }, inflight: false } };
    let resultReads = 0;
    const result = await executeHighRiskCommand(holder, payload, async () => {
      resultReads += 1;
      throw Object.assign(failure instanceof Error ? failure : new Error('lookup failed'), failure, { commandReference: reference });
    }, undefined, { recovery: true });
    assert.equal(resultReads, 1);
    assert.equal(result.command.unknown, true);
    assert.equal(result.command.error.commandReference, reference);
  }
});

test('result recovery preserves trusted terminal success, rejection, and failure states', async () => {
  const payload = { reason: 'query only the frozen command result', precheck: impact };
  const reference = '0199a300-0001-7000-8000-000000000098';
  const frozen = () => ({ current: { command: { idempotencyKey: 'frozen-terminal-key', payload, payloadKey: highRiskPayloadKey(payload), dispatched: true, unknown: true, locked: true, error: { code: 'COMMAND_RESULT_UNKNOWN', commandReference: reference } }, inflight: false } });
  const succeeded = await executeHighRiskCommand(frozen(), payload, async () => ({ status: 'SUCCEEDED' }), undefined, { recovery: true });
  assert.equal(succeeded.ok, true);
  for (const code of ['COMMAND_REJECTED', 'COMMAND_EXECUTION_FAILED']) {
    const terminal = await executeHighRiskCommand(frozen(), payload, async () => {
      throw Object.assign(new Error('trusted terminal result'), { status: 200, code, commandReference: reference, finalResult: true });
    }, undefined, { recovery: true });
    assert.equal(terminal.ok, false);
    assert.equal(terminal.command.terminal, true);
    assert.equal(terminal.command.unknown, false);
    assert.equal(terminal.error.code, code);
    assert.equal(terminal.error.commandReference, reference);
  }
});

test('successful command and route responses are action-bound closed projections', () => {
  const command = {
    command_reference: '0199a300-0001-7000-8000-000000000011', command_type: 'TOKEN_ROUTE_DELETE', policy_version: 2,
    actor_user_id: '0199a300-0001-7000-8000-000000000012', actor_permission: 'platform.tokens.manage', authorization_version: 3,
    idempotency_fingerprint: 'd'.repeat(64), target_resource_type: 'TOKEN_ROUTE', target_resource_id: impact.evidence.resource_id,
    status: 'PENDING', audit_request_id: 'request-safe', created_at: '2026-09-04T12:00:00Z',
  };
  assert.equal(validGovernedTokenCommandResult(command, { commandType: 'TOKEN_ROUTE_DELETE', targetResourceType: 'TOKEN_ROUTE', targetResourceID: impact.evidence.resource_id }), true);
  assert.equal(validGovernedTokenCommandResult({ ...command, command_type: 'TOKEN_ACCOUNT_STATUS_CHANGE' }, { commandType: 'TOKEN_ROUTE_DELETE', targetResourceType: 'TOKEN_ROUTE', targetResourceID: impact.evidence.resource_id }), false);
  assert.equal(validGovernedTokenCommandResult({ ...command, secret: 'must-not-pass' }, { commandType: 'TOKEN_ROUTE_DELETE', targetResourceType: 'TOKEN_ROUTE', targetResourceID: impact.evidence.resource_id }), false);
  const expectedCommand = { commandType: 'TOKEN_ROUTE_DELETE', targetResourceType: 'TOKEN_ROUTE', targetResourceID: impact.evidence.resource_id };
  assert.throws(() => requireGovernedTokenCommandResult({ ...command, status: 'PENDING' }, expectedCommand), (error) => error.code === 'COMMAND_RESULT_UNKNOWN' && error.commandReference === command.command_reference);
  assert.throws(() => requireGovernedTokenCommandResult({ ...command, status: 'REJECTED', stable_error_code: 'COMMAND_PRECHECK_FAILED' }, expectedCommand), (error) => error.finalResult === true && error.code === 'COMMAND_PRECHECK_FAILED');
  assert.equal(requireGovernedTokenCommandResult({ ...command, status: 'SUCCEEDED', completed_at: '2026-09-04T12:00:01Z' }, expectedCommand).status, 'SUCCEEDED');

  const route = {
    route_id: '0199a300-0001-7000-8000-000000000013', token_account_id: '0199a300-0001-7000-8000-000000000014', model_id: '0199a300-0001-7000-8000-000000000015', protocol_version_id: '0199a300-0001-7000-8000-000000000016',
    protocol_code: 'CHAT', priority: 100, default_scope: 'NONE', health_status: 'UNKNOWN', health_diagnostic_code: 'ROUTE_CONFIGURATION_CHANGED', health_observed_at: null, health_expires_at: null,
    route_version: 2, route_generation: 2, account_version: 5, updated_at: '2026-09-04T12:00:00Z',
  };
  const expectedRoute = { tokenAccountID: route.token_account_id, modelID: route.model_id, protocolVersionID: route.protocol_version_id, priority: 100, defaultScope: 'NONE', expectedAccountVersion: 4 };
  assert.equal(validStableRouteWriteResult(route, expectedRoute), true);
  assert.equal(validStableRouteWriteResult({ ...route, upstream: 'sensitive' }, expectedRoute), false);
});

test('approval create and recovery responses preserve the complete frozen command and snapshots', () => {
  const critical_resource_versions = {
    current_catalog_version: 7, source_catalog_version: 3, catalog_diff_fingerprint: '4'.repeat(64), billing_price_version: 9,
    route_set_fingerprint: '1'.repeat(64), provisioning_set_fingerprint: '2'.repeat(64), inflight_request_set_fingerprint: '3'.repeat(64), policy_version: 2,
  };
  const financial_snapshot = { billing_price_revision: 9, price_set_fingerprint: 'f'.repeat(64), models_checked: 2, models_covered: 2, fully_covered: true, billing_as_of: '2026-09-04T12:00:00Z' };
  const input = { command_type: 'TOKEN_MODEL_CATALOG_ROLLBACK', target_resource_type: 'MODEL_CATALOG', target_resource_id: '7', reason: 'rollback after complete impact review', normalized_command: { source_catalog_version: 3 }, critical_resource_versions, financial_snapshot };
  const approval = { ...input, approval_request_id: '0199a300-0001-7000-8000-000000000017', requester_user_id: '0199a300-0001-7000-8000-000000000018', policy_version: 2, approval_mode: 'APPROVAL_REQUIRED', command_fingerprint: 'e'.repeat(64), status: 'DRAFT', version: 1, expires_at: '2026-09-05T12:00:00Z', created_at: '2026-09-04T12:00:00Z', updated_at: '2026-09-04T12:00:00Z', allowed_actions: ['EDIT', 'SUBMIT'] };
  assert.equal(validApprovalRequest(approval, input, ['DRAFT']), true);
  assert.deepEqual(approval.critical_resource_versions, input.critical_resource_versions);
  assert.deepEqual(approval.financial_snapshot, input.financial_snapshot);
  const recovery = { ...approval, status: 'EXECUTED', version: 3, allowed_actions: [] };
  assert.equal(validApprovalRequest(recovery, input, ['EXECUTED']), true);
  assert.deepEqual(recovery.critical_resource_versions, input.critical_resource_versions);
  assert.deepEqual(recovery.financial_snapshot, input.financial_snapshot);
  assert.equal(validApprovalRequest({ ...approval, normalized_command: { source_catalog_version: 4 } }, input, ['DRAFT']), false);
  assert.equal(validApprovalRequest({ ...approval, critical_resource_versions: { ...approval.critical_resource_versions, route_set_fingerprint: '8'.repeat(64) } }, input, ['DRAFT']), false);
  assert.equal(validApprovalRequest({ ...approval, financial_snapshot: { ...approval.financial_snapshot, billing_price_revision: 10 } }, input, ['DRAFT']), false);
  assert.equal(validApprovalRequest({ ...recovery, financial_snapshot: { ...recovery.financial_snapshot, models_checked: 3 } }, input, ['EXECUTED']), false);
  assert.equal(validApprovalRequest({ ...approval, requester_user_id: approval.approval_request_id, status: 'APPROVED' }, input, ['DRAFT']), false);
});

test('approval impact consumes only the authoritative 13/8 critical snapshot and closed six-field financial snapshot', () => {
  const financial_snapshot = { billing_price_revision: 9, price_set_fingerprint: 'f'.repeat(64), models_checked: 2, models_covered: 2, fully_covered: true, billing_as_of: '2026-09-04T12:00:00Z' };
  const lifecycle = {
    ...impact, action: 'MODEL_LIFECYCLE_DEPRECATED',
    evidence: { ...impact.evidence, resource_type: 'MODEL' },
    critical_resource_versions: {
      model_id: impact.evidence.resource_id, catalog_version: 7, current_lifecycle: 'PUBLISHED', target_lifecycle: 'DEPRECATED', lifecycle_plan_version: 0,
      effective_at: '2026-09-04T12:05:00Z', existing_session_cutoff_at: null, replacement_model_id: null, billing_price_version: 9,
      route_set_fingerprint: '1'.repeat(64), provisioning_set_fingerprint: '2'.repeat(64), inflight_request_set_fingerprint: '3'.repeat(64), policy_version: 2,
    },
    financial_snapshot,
  };
  assert.equal(requireTokenApprovalImpact(lifecycle, { action: lifecycle.action, commandType: 'TOKEN_MODEL_LIFECYCLE_CHANGE', targetResourceID: impact.evidence.resource_id, targetStatus: 'DEPRECATED' }), lifecycle);
  for (const invalid of [
    { ...lifecycle, approval_snapshot: lifecycle.critical_resource_versions, critical_resource_versions: undefined },
    { ...lifecycle, price_coverage: financial_snapshot, financial_snapshot: undefined },
    { ...lifecycle, critical_resource_versions: { ...lifecycle.critical_resource_versions, extra: true } },
    { ...lifecycle, financial_snapshot: { ...financial_snapshot, extra: true } },
    { ...lifecycle, financial_snapshot: { ...financial_snapshot, billing_price_revision: 10 } },
    { ...lifecycle, financial_snapshot: { ...financial_snapshot, models_covered: 1 } },
  ]) assert.throws(() => requireTokenApprovalImpact(invalid, { action: lifecycle.action, commandType: 'TOKEN_MODEL_LIFECYCLE_CHANGE', targetResourceID: impact.evidence.resource_id, targetStatus: 'DEPRECATED' }), /approval snapshot/u);

  const rollback = {
    ...impact, action: 'MODEL_CATALOG_ROLLBACK',
    evidence: { ...impact.evidence, resource_type: 'MODEL_CATALOG', resource_id: '3' },
    critical_resource_versions: {
      current_catalog_version: 7, source_catalog_version: 3, catalog_diff_fingerprint: '4'.repeat(64), billing_price_version: 9,
      route_set_fingerprint: '1'.repeat(64), provisioning_set_fingerprint: '2'.repeat(64), inflight_request_set_fingerprint: '3'.repeat(64), policy_version: 2,
    },
    financial_snapshot,
  };
  assert.equal(requireTokenApprovalImpact(rollback, { action: rollback.action, commandType: 'TOKEN_MODEL_CATALOG_ROLLBACK', sourceCatalogVersion: 3 }), rollback);
  assert.throws(() => requireTokenApprovalImpact({ ...rollback, critical_resource_versions: { ...rollback.critical_resource_versions, source_catalog_version: 4 } }, { action: rollback.action, commandType: 'TOKEN_MODEL_CATALOG_ROLLBACK', sourceCatalogVersion: 3 }), /rollback snapshot/u);
});

test('credential revoke consumes the authoritative 11-field Token snapshot and no Billing facts', () => {
  const credentialID = '0199a300-0001-7000-8000-000000000031';
  const accountID = '0199a300-0001-7000-8000-000000000032';
  const revoke = {
    ...impact, action: 'TOKEN_CREDENTIAL_REVOKE', billing_observed_at: undefined,
    evidence: { ...impact.evidence, resource_type: 'PROVIDER_CREDENTIAL', resource_id: credentialID, current_status: 'ACTIVE' },
    critical_resource_versions: {
      token_account_id: accountID, token_account_version: 7, credential_id: credentialID, credential_version: 3,
      credential_status: 'ACTIVE', credential_fingerprint: '1'.repeat(64), active_route_set_fingerprint: '2'.repeat(64),
      provisioning_lease_set_fingerprint: '3'.repeat(64), inflight_request_set_fingerprint: '4'.repeat(64),
      replacement_credential_version: null, policy_version: 1,
    },
    financial_snapshot: {},
  };
  assert.equal(requireTokenApprovalImpact(revoke, { action: revoke.action, commandType: 'TOKEN_CREDENTIAL_REVOKE', targetResourceID: credentialID }), revoke);
  for (const invalid of [
    { ...revoke, financial_snapshot: null },
    { ...revoke, financial_snapshot: [] },
    { ...revoke, financial_snapshot: { extra: true } },
    { ...revoke, financial_snapshot: { billing_price_revision: 1 } },
    { ...revoke, critical_resource_versions: { ...revoke.critical_resource_versions, credential_id: accountID } },
    { ...revoke, critical_resource_versions: { ...revoke.critical_resource_versions, credential_fingerprint: 'unsafe' } },
    { ...revoke, critical_resource_versions: { ...revoke.critical_resource_versions, extra: true } },
  ]) assert.throws(() => requireTokenApprovalImpact(invalid, { action: revoke.action, commandType: 'TOKEN_CREDENTIAL_REVOKE', targetResourceID: credentialID }), /approval snapshot/u);

  const input = { command_type: 'TOKEN_CREDENTIAL_REVOKE', target_resource_type: 'PROVIDER_CREDENTIAL', target_resource_id: credentialID, reason: 'revoke after complete impact review', normalized_command: { token_account_id: accountID, expected_account_version: 7, credential_id: credentialID, credential_version: 3 }, critical_resource_versions: revoke.critical_resource_versions, financial_snapshot: {} };
  const approval = { ...input, approval_request_id: '0199a300-0001-7000-8000-000000000033', requester_user_id: '0199a300-0001-7000-8000-000000000034', policy_version: 1, approval_mode: 'APPROVAL_REQUIRED', command_fingerprint: 'e'.repeat(64), status: 'DRAFT', version: 1, expires_at: '2026-09-06T12:00:00Z', created_at: '2026-09-05T12:00:00Z', updated_at: '2026-09-05T12:00:00Z', allowed_actions: ['EDIT', 'SUBMIT'] };
  assert.equal(validApprovalRequest(approval, input, ['DRAFT']), true);
  assert.equal(validApprovalRequest({ ...approval, policy_version: 2 }, input, ['DRAFT']), false);
});

test('Chromium smoke server binding rejects the complete unsafe-port set and retries port 3659', async () => {
  const expectedUnsafe = [
    1, 7, 9, 11, 13, 15, 17, 19, 20, 21, 22, 23, 25, 37, 42, 43, 53, 69, 77, 79, 87, 95,
    101, 102, 103, 104, 109, 110, 111, 113, 115, 117, 119, 123, 135, 137, 139, 143, 161, 179,
    389, 427, 465, 512, 513, 514, 515, 526, 530, 531, 532, 540, 548, 554, 556, 563, 587, 601,
    636, 989, 990, 993, 995, 1719, 1720, 1723, 2049, 3659, 4045, 4190, 5060, 5061, 6000, 6566,
    6665, 6666, 6667, 6668, 6669, 6697, 10080,
  ];
  assert.deepEqual([...chromiumUnsafePorts], expectedUnsafe);
  const ports = [3659, 49152];
  let closes = 0;
  const result = await listenOnChromiumSafePort({}, {
    bind: async () => ({ address: '127.0.0.1', family: 'IPv4', port: ports.shift() }),
    close: async () => { closes += 1; },
  });
  assert.equal(closes, 1);
  assert.equal(result.port, 49152);
  assert.equal(result.origin, 'http://127.0.0.1:49152');
});
