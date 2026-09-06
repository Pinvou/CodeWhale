import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, writeFile, rm, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import {
  closedModelTokenGates, modelTokenGateNames, parseReleaseJSON, activationFromGovernanceSnapshot,
  validateModelTokenReleasePlan, validateModelTokenEvidenceEnvelope,
} from '../../../scripts/account-platform-model-token-release-plan.mjs';

const hash = 'a'.repeat(64);
const now = Date.parse('2026-09-06T12:00:00Z');
function fixture() {
  const thresholds = {
    unauthorized_effects: { advance_max: 0, pause_above: 0, slo_reference: 'slo.authorization' },
    error_rate: { advance_max: 0.01, pause_above: 0.02, slo_reference: 'slo.error' },
    latency_p95_ms: { advance_max: 100, pause_above: 200, slo_reference: 'slo.latency' },
    queue_depth: { advance_max: 10, pause_above: 20, slo_reference: 'slo.queue' },
    capacity_utilization: { advance_max: 0.7, pause_above: 0.85, slo_reference: 'slo.capacity' },
    resource_recovery_failures: { advance_max: 0, pause_above: 0, slo_reference: 'slo.recovery' },
  };
  return {
    schema_version: 1,
    candidate: { commit: 'b'.repeat(40), image_sha256: hash, config_sha256: hash, migration_manifest_sha256: hash, policy_set_digest: hash, handler_set_digest: hash },
    environment: 'CLOUD_UAT', app_env: 'staging', release_reference: 'release.test', approval_reference: 'approval.test', operator_reference: 'operator.test',
    activation: { state: 'ACTIVE', epoch: 1, installed_policy_set_digest: hash, installed_handler_set_digest: hash },
    gates: closedModelTokenGates(),
    runtime_roles: { gateway: 'token_gateway_release_test', credential_portal: 'token_credential_release_test', permission_group: 'token_gateway', retired_roles: ['token_retired_test'] },
    operations: { token_migrator: 'token_migrator', account_policy_role: 'account_policy_operator', account_policy_lock: 'SHARE_NOWAIT', role_evidence_reference: 'roles.test' },
    credential_origin: null,
    legacy_links: { ga_at: null, ga_version: null, minimum_days: 90, minimum_stable_versions: 2 },
    stages: ['INTERNAL', 'LIMITED', 'FULL'].map((stage, index) => ({ stage, cohort: `cohort.${index}`, traffic_percent: [1, 10, 100][index], observation_seconds: 60, minimum_samples: 10, insufficient_samples: 'PAUSE', approval_reference: 'approval.observation', thresholds: structuredClone(thresholds) })),
  };
}
function evidence(plan) {
  return { schema_version: 1, candidate: structuredClone(plan.candidate), environment: plan.environment,
    kind: 'WRITER_FENCE', status: 'PASS', started_at: '2026-09-06T10:00:00Z', ended_at: '2026-09-06T11:00:00Z',
    executor_reference: 'operator.test', reviewer_reference: 'reviewer.test', approval_reference: 'approval.test', report_sha256: hash, report_path: 'reports/writer-fence.json' };
}
function rejected(mutate, expected = /model_token_release_/) {
  const plan = fixture(); mutate(plan);
  assert.throws(() => validateModelTokenReleasePlan(plan), expected);
}

test('plan freezes thirteen existing gates with closed defaults and exact schema fields', async () => {
  const sourceGates = {
    'token-gateway': ['TOKEN_ADMIN_PAGINATION_ENABLED', 'TOKEN_ADMIN_IMPACT_PRECHECKS_ENABLED', 'TOKEN_CREDENTIAL_ISOLATION_ENABLED', 'TOKEN_ADMIN_OPERATIONS_OVERVIEW_ENABLED'],
    'billing-service': ['BILLING_ADMIN_MODEL_PRICE_PROJECTION_ENABLED'],
    'token-portal': ['PORTAL_ADMIN_MODEL_TOKEN_IA_ENABLED', 'PORTAL_ADMIN_TOKEN_RESOURCE_FORMS_ENABLED', 'PORTAL_ADMIN_MODEL_OPERATIONS_DETAILS_ENABLED', 'PORTAL_ADMIN_MODEL_TOKEN_ACTIONS_ENABLED', 'PORTAL_ADMIN_MODEL_DETAILS_ENABLED', 'PORTAL_ADMIN_TOKEN_DETAILS_ENABLED', 'PORTAL_ADMIN_MODEL_PRICING_EDIT_ENABLED', 'PORTAL_ADMIN_MODEL_OPERATIONS_OVERVIEW_ENABLED'],
  };
  assert.deepEqual([...modelTokenGateNames].sort(), Object.values(sourceGates).flat().sort());
  for (const [service, names] of Object.entries(sourceGates)) {
    const source = await readFile(new URL(`../../cmd/${service}/main.go`, import.meta.url), 'utf8');
    for (const name of names) assert.ok(source.includes(`"${name}"`), `existing source gate ${name}`);
  }
  assert.equal(modelTokenGateNames.length, 13);
  assert.equal(Object.values(closedModelTokenGates()).every(value => value === false), true);
  assert.deepEqual(validateModelTokenReleasePlan(fixture()), { decision: 'PLAN_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' });
  for (const mutate of [p => { p.force = true; }, p => { p.gates.UNKNOWN_ENABLED = false; },
    p => { delete p.gates.TOKEN_ADMIN_PAGINATION_ENABLED; }, p => { p.gates.TOKEN_ADMIN_PAGINATION_ENABLED = 'false'; },
    p => { p.candidate.commit = 'main'; }, p => { p.operations.password = 'not-a-real-secret'; }]) rejected(mutate, /schema_invalid/);
});

test('every declared gate dependency fails closed without auto-enabling prerequisites', async () => {
  const schema = JSON.parse(await readFile(new URL('../../contracts/deployment/model-token-release-plan.schema.json', import.meta.url), 'utf8'));
  const enabled = fixture(); Object.keys(enabled.gates).forEach(name => { enabled.gates[name] = true; });
  enabled.credential_origin = 'https://credentials-uat.pinvou.com';
  assert.doesNotThrow(() => validateModelTokenReleasePlan(enabled));
  for (const rule of schema.properties.gates.allOf) {
    for (const prerequisite of Object.keys(rule.then.properties)) {
      const plan = structuredClone(enabled); plan.gates[prerequisite] = false;
      const before = JSON.stringify(plan);
      assert.throws(() => validateModelTokenReleasePlan(plan), /schema_invalid/);
      assert.equal(JSON.stringify(plan), before);
    }
  }
  const readOnly = fixture(); readOnly.activation.state = 'SAFE_ROLLBACK';
  readOnly.gates.TOKEN_ADMIN_PAGINATION_ENABLED = true;
  readOnly.gates.PORTAL_ADMIN_MODEL_TOKEN_IA_ENABLED = true;
  readOnly.gates.PORTAL_ADMIN_MODEL_DETAILS_ENABLED = true;
  readOnly.gates.BILLING_ADMIN_MODEL_PRICE_PROJECTION_ENABLED = true;
  assert.doesNotThrow(() => validateModelTokenReleasePlan(readOnly));
});

test('environment, origin, state and digest declarations cannot create an activation bypass', () => {
  for (const mutate of [p => { p.app_env = 'development'; }, p => { p.app_env = 'production'; },
    p => { p.credential_origin = 'https://credentials.example.com'; }, p => { p.credential_origin = 'http://credentials-uat.pinvou.com'; },
    p => { p.activation.epoch = 9007199254740992; }, p => { p.activation.state = 'PRE_ACTIVATION'; },
    p => { p.activation.installed_handler_set_digest = 'c'.repeat(64); }]) rejected(mutate);
  for (const state of ['NOT_INSTALLED', 'PRE_ACTIVATION', 'SAFE_ROLLBACK']) {
    const plan = fixture(); plan.activation.state = state;
    if (state === 'NOT_INSTALLED') Object.assign(plan.activation, { epoch: null, installed_handler_set_digest: null, installed_policy_set_digest: null });
    if (state === 'PRE_ACTIVATION') Object.assign(plan.activation, { epoch: 0, installed_handler_set_digest: null, installed_policy_set_digest: null });
    assert.doesNotThrow(() => validateModelTokenReleasePlan(plan));
    plan.gates.TOKEN_ADMIN_PAGINATION_ENABLED = true; plan.gates.TOKEN_ADMIN_IMPACT_PRECHECKS_ENABLED = true;
    plan.gates.PORTAL_ADMIN_MODEL_TOKEN_IA_ENABLED = true; plan.gates.PORTAL_ADMIN_MODEL_TOKEN_ACTIONS_ENABLED = true;
    assert.throws(() => validateModelTokenReleasePlan(plan), /write_requires_active/);
  }
});

test('new writer identity separation and Account lock requirement are mandatory planning inputs', () => {
  for (const mutate of [p => { p.runtime_roles.gateway = 'token_gateway'; },
    p => { p.runtime_roles.gateway = p.runtime_roles.credential_portal; },
    p => { p.runtime_roles.retired_roles.push(p.runtime_roles.gateway); },
    p => { p.operations.token_migrator = p.runtime_roles.gateway; },
    p => { p.operations.account_policy_lock = 'SELECT'; }, p => { delete p.operations.role_evidence_reference; },
    p => { p.runtime_roles.gateway = 'a'.repeat(64); }, p => { p.runtime_roles.gateway = 'quoted.role'; },
    p => { p.runtime_roles.retired_roles = ['非ASCII']; }]) rejected(mutate);
  const longest = fixture(); longest.runtime_roles.gateway = 'a'.repeat(63);
  assert.doesNotThrow(() => validateModelTokenReleasePlan(longest));
});

test('real PRE SQL null/CLI empty digests normalize once, and existing production-new remains supported', () => {
  for (const empty of [null, '']) {
    const plan = fixture(); plan.activation = activationFromGovernanceSnapshot({ state: 'PRE_ACTIVATION', epoch: 0, policy_set_digest: empty, handler_set_digest: empty });
    assert.equal(plan.activation.installed_policy_set_digest, null);
    assert.doesNotThrow(() => validateModelTokenReleasePlan(plan));
  }
  for (const invalid of [null, {}, { state: 'PRE_ACTIVATION', epoch: 0 },
    { state: 'PRE_ACTIVATION', epoch: 1, policy_set_digest: hash, handler_set_digest: hash },
    { state: 'PRE_ACTIVATION', epoch: -1, policy_set_digest: null, handler_set_digest: null },
    { state: 'ACTIVE', epoch: 9007199254740992, policy_set_digest: hash, handler_set_digest: hash },
    { state: 'ACTIVE', epoch: 1, policy_set_digest: hash },
    { state: 'SAFE_ROLLBACK', epoch: 0, policy_set_digest: hash, handler_set_digest: hash },
    { state: 'PRE_ACTIVATION', epoch: 0, policy_set_digest: hash, handler_set_digest: hash },
    { state: 'ACTIVE', epoch: 1, policy_set_digest: '', handler_set_digest: '' }]) {
    assert.throws(() => activationFromGovernanceSnapshot(invalid), /snapshot_invalid/);
  }
  for (const appEnv of ['production', 'production-new']) {
    const plan = fixture(); plan.environment = 'PRODUCTION'; plan.app_env = appEnv;
    plan.credential_origin = 'https://credentials.pinvou.com';
    assert.doesNotThrow(() => validateModelTokenReleasePlan(plan));
  }
  for (const state of ['ACTIVE', 'SAFE_ROLLBACK']) {
    const snapshot = { state, epoch: 1, policy_set_digest: hash, handler_set_digest: hash };
    assert.deepEqual(activationFromGovernanceSnapshot(snapshot), { state, epoch: 1, installed_policy_set_digest: hash, installed_handler_set_digest: hash });
    for (const key of ['policy_set_digest', 'handler_set_digest']) {
      for (const invalid of [[hash], { toString: () => hash }, null, 1, false]) {
        assert.throws(() => activationFromGovernanceSnapshot({ ...snapshot, [key]: invalid }), /activation_snapshot_invalid/);
      }
    }
  }
});

test('observation plans reject absent, zero, unordered, unapproved and weakened thresholds', () => {
  for (const mutate of [p => { p.stages[0].observation_seconds = 0; }, p => { p.stages[0].minimum_samples = 0; },
    p => { p.stages[0].approval_reference = ''; }, p => { p.stages[0].cohort = ''; }, p => { p.stages.reverse(); },
    p => { p.stages[2].traffic_percent = 90; }, p => { p.stages[1].traffic_percent = 1; },
    p => { p.stages[0].insufficient_samples = 'GO'; }, p => { delete p.stages[0].thresholds.queue_depth; },
    p => { p.stages[0].thresholds.error_rate.pause_above = 2; },
    p => { p.stages[0].thresholds.latency_p95_ms.advance_max = 201; },
    p => { p.stages[0].thresholds.unauthorized_effects.pause_above = 1; },
    p => { p.stages[0].thresholds.capacity_utilization.pause_above = 0.86; },
    p => { p.legacy_links.minimum_days = 89; }, p => { p.legacy_links.minimum_stable_versions = 1; },
    p => { p.legacy_links.ga_at = '2026-09-06T00:00:00Z'; }]) rejected(mutate);
});

test('evidence envelope binds every candidate digest but does not attest report truth or release GO', () => {
  const plan = fixture(); const good = evidence(plan);
  for (const status of ['PASS', 'FAIL']) {
    const result = validateModelTokenEvidenceEnvelope({ ...good, status }, plan, now);
    assert.deepEqual(result, { decision: 'ENVELOPE_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' });
  }
  for (const mutate of [e => { e.passed = true; }, e => { e.report_sha256 = ''; }, e => { e.report_path = '../secret.txt'; },
    e => { e.report_path = 'C:/secret.txt'; }, e => { e.kind = 'MOCK_PRODUCTION'; },
    e => { e.executor_reference = e.reviewer_reference; }, e => { e.ended_at = e.started_at; },
    e => { e.ended_at = '2026-09-07T00:00:00Z'; }, e => { e.environment = 'PRODUCTION'; }]) {
    const item = structuredClone(good); mutate(item);
    assert.throws(() => validateModelTokenEvidenceEnvelope(item, plan, now), /model_token_release_/);
  }
  for (const key of Object.keys(good.candidate)) {
    const item = structuredClone(good); item.candidate[key] = 'c'.repeat(key === 'commit' ? 40 : 64);
    assert.throws(() => validateModelTokenEvidenceEnvelope(item, plan, now), /binding_invalid/);
  }
});

test('strict JSON rejects duplicate and escaped duplicate keys, malformed and oversized input', () => {
  for (const raw of ['{"first":false,"first":true}', '{"gates":{"a":false,"\\u0061":true}}',
    '{"a":1} {}', '{"password":"private-marker",', ' '.repeat(1024 * 1024 + 1)]) {
    assert.throws(() => parseReleaseJSON(raw), error => /^model_token_release_json_/.test(error.message) && !error.message.includes('private-marker'));
  }
  assert.deepEqual(parseReleaseJSON('{"a":[{"b":1},{"b":2}]}'), { a: [{ b: 1 }, { b: 2 }] });
});

test('planning CLI has no GO/apply mode and never prints raw input or environment credentials', async () => {
  const directory = await mkdtemp(path.join(tmpdir(), 'pinvou-release-plan-'));
  try {
    const input = path.join(directory, 'plan.json');
    await writeFile(input, JSON.stringify(fixture()));
    const script = new URL('../../../scripts/account-platform-model-token-release-plan.mjs', import.meta.url);
    const run = args => spawnSync(process.execPath, [fileURLToPath(script), ...args], { encoding: 'utf8', env: { ...process.env, TOKEN_SECRET: 'private-marker' } });
    const success = run(['--plan', input]); assert.equal(success.status, 0, success.stderr);
    assert.equal(JSON.parse(success.stdout).environment_status, 'ENV_PENDING');
    const apply = run(['--plan', input, '--apply']); assert.equal(apply.status, 1);
    await writeFile(input, '{"password":"private-marker",');
    const invalid = run(['--plan', input]); assert.equal(invalid.status, 1);
    assert.equal(`${invalid.stdout}${invalid.stderr}${success.stdout}${apply.stderr}`.includes('private-marker'), false);
  } finally { await rm(directory, { recursive: true, force: true }); }
});
