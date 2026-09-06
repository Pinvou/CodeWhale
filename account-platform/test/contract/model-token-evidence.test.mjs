import assert from 'node:assert/strict';
import test from 'node:test';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { mkdtemp, mkdir, writeFile, readFile, rm, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import { verifyModelTokenEvidence } from '../../../scripts/account-platform-model-token-evidence.mjs';
import { evidenceCommandTypes, evidenceKinds, requiredEvidenceCases, evidenceSignedBytes, observationStages } from '../../../scripts/account-platform-model-token-evidence-policy.mjs';
import { evaluateObservation, validateLegacyRetention, validateReportSemantics } from '../../../scripts/account-platform-model-token-evidence-report.mjs';
import { closedModelTokenGates } from '../../../scripts/account-platform-model-token-release-plan.mjs';

// Synthetic signed reports test the verifier only, never represent deployment evidence.
const now = Date.parse('2026-09-06T12:00:00Z');
const sha = bytes => createHash('sha256').update(bytes).digest('hex');
const stamp = offset => new Date(now + offset).toISOString();
const pair = generateKeyPairSync('ed25519');
const publicKey = pair.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64url');
function planFixture() {
  const hash = 'a'.repeat(64);
  const limits = { unauthorized_effects: [0, 0], error_rate: [0.01, 0.02], latency_p95_ms: [100, 200], queue_depth: [10, 20], capacity_utilization: [0.7, 0.85], resource_recovery_failures: [0, 0] };
  return {
    schema_version: 1, candidate: { commit: 'b'.repeat(40), image_sha256: hash, config_sha256: hash, migration_manifest_sha256: hash, policy_set_digest: hash, handler_set_digest: hash },
    environment: 'PRODUCTION', app_env: 'production-new', release_reference: 'release.fixture', approval_reference: 'approval.fixture', operator_reference: 'operator.fixture',
    activation: { state: 'ACTIVE', epoch: 1, installed_policy_set_digest: hash, installed_handler_set_digest: hash },
    gates: closedModelTokenGates(),
    runtime_roles: { gateway: 'token_release_fixture', credential_portal: 'credential_release_fixture', permission_group: 'token_gateway', retired_roles: ['token_retired_fixture'] },
    operations: { token_migrator: 'token_migrator', account_policy_role: 'account_policy_operator', account_policy_lock: 'SHARE_NOWAIT', role_evidence_reference: 'roles.fixture' },
    credential_origin: 'https://credentials.pinvou.com',
    legacy_links: { ga_at: null, ga_version: null, minimum_days: 90, minimum_stable_versions: 2 },
    stages: observationStages.map((stage, index) => ({ stage, cohort: `cohort.${index}`, traffic_percent: [1, 10, 100][index], observation_seconds: 60, minimum_samples: 10, insufficient_samples: 'PAUSE', approval_reference: 'approval.fixture', thresholds: Object.fromEntries(Object.entries(limits).map(([metric, [advance_max, pause_above]]) => [metric, { advance_max, pause_above, slo_reference: `slo.${metric}` }])) })),
  };
}
function signedFixture() {
  const plan = planFixture();
  const reports = new Map(); const artifacts = new Map();
  const bundle = { schema_version: 1, stage: 'FULL', entries: [] };
  const planBytes = Buffer.from(JSON.stringify(plan));
  const trust = { schema_version: 1, expected_plan_sha256: sha(planBytes), max_age_seconds: 86400, keys: [{ key_id: 'test-only', public_key_base64url: publicKey, source: 'fixture.not-production', environment: 'PRODUCTION', kinds: [...evidenceKinds], executor_references: ['executor.fixture'], reviewer_references: ['reviewer.fixture'], approval_references: ['approval.fixture'], valid_from: stamp(-86400000), valid_until: stamp(86400000) }] };
  for (const kind of evidenceKinds) {
    const artifactPath = `artifacts/${kind}.txt`;
    const raw = Buffer.from(`Synthetic fixture only: ${kind}\n`);
    artifacts.set(artifactPath, raw);
    const envelope = { schema_version: 1, candidate: structuredClone(plan.candidate), environment: plan.environment, kind, status: 'PASS', started_at: stamp(-600000), ended_at: stamp(-60000), executor_reference: 'executor.fixture', reviewer_reference: 'reviewer.fixture', approval_reference: 'approval.fixture', report_sha256: 'a'.repeat(64), report_path: `reports/${kind}.json` };
    const { report_path, report_sha256, ...common } = structuredClone(envelope);
    const report = { ...common, validation_level: 'TARGET_ENVIRONMENT', exit_code: 0, artifacts: [{ path: artifactPath, sha256: sha(raw) }], cases: requiredEvidenceCases[kind].map(id => ({ id, status: 'PASS', artifact_paths: [artifactPath] })), observation: kind === 'OBSERVATION' ? { stages: plan.stages.map((stage, index) => ({ stage: stage.stage, cohort: stage.cohort, traffic_percent: stage.traffic_percent, approval_reference: stage.approval_reference, started_at: stamp(-240000 + index * 60000), ended_at: stamp(-180000 + index * 60000), samples: 10, metrics: { unauthorized_effects: 0, error_rate: 0.01, latency_p95_ms: 100, queue_depth: 10, capacity_utilization: 0.7, resource_recovery_failures: 0 } })) } : null, legacy_links: kind === 'LEGACY_LINKS' ? { compatibility_retained: true, stable_releases: [] } : null };
    reports.set(report_path, report);
    bundle.entries.push({ source: 'fixture.not-production', key_id: 'test-only', envelope, signature_base64url: '' });
  }
  return { plan, trust, bundle, reports, artifacts };
}
function resign(fixture) {
  fixture.trust.expected_plan_sha256 = sha(JSON.stringify(fixture.plan));
  for (const entry of fixture.bundle.entries) {
    entry.envelope.report_sha256 = sha(JSON.stringify(fixture.reports.get(entry.envelope.report_path)));
    entry.signature_base64url = sign(null, evidenceSignedBytes(entry, fixture.trust.expected_plan_sha256), pair.privateKey).toString('base64url');
  }
}
async function onDisk(t, fixture = signedFixture()) {
  const directory = await mkdtemp(path.join(tmpdir(), 'pinvou-evidence-test-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const rootPath = path.join(directory, 'bundle');
  await mkdir(path.join(rootPath, 'reports'), { recursive: true });
  await mkdir(path.join(rootPath, 'artifacts'));
  const options = { planPath: path.join(rootPath, 'plan.json'), bundlePath: path.join(rootPath, 'bundle.json'), rootPath, trustPath: path.join(directory, 'trust.json'), now };
  async function save({ signReports = true } = {}) {
    if (signReports) resign(fixture);
    await Promise.all([
      writeFile(options.planPath, JSON.stringify(fixture.plan)), writeFile(options.bundlePath, JSON.stringify(fixture.bundle)), writeFile(options.trustPath, JSON.stringify(fixture.trust)),
      ...[...fixture.reports].map(([relative, value]) => writeFile(path.join(rootPath, relative), JSON.stringify(value))),
      ...[...fixture.artifacts].map(([relative, value]) => writeFile(path.join(rootPath, relative), value)),
    ]);
  }
  await save(); return { fixture, options, save, directory };
}

test('full signed fixture verifies actual report/artifact bytes without claiming deployed or invoking tools', async t => {
  const { options } = await onDisk(t);
  const result = await verifyModelTokenEvidence(options);
  assert.equal(result.decision, 'EVIDENCE_VERIFIED');
  assert.equal(result.environment_status, 'VERIFIED');
  assert.equal(result.deployment_status, 'NOT_DEPLOYED');
  assert.equal(result.reports_verified, 17); assert.equal(result.artifacts_verified, 17);
  assert.equal(result.legacy_retention, 'RETAIN_REQUIRED');
  assert.equal(result.stage, 'FULL');
});

test('coverage policy is the frozen seventeen kinds, nine sources and actual nineteen Token commands', async () => {
  const schema = JSON.parse(await readFile(new URL('../../contracts/deployment/model-token-release-evidence.schema.json', import.meta.url), 'utf8'));
  assert.deepEqual(evidenceKinds, schema.properties.kind.enum);
  const manifest = await readFile(new URL('../../internal/platform/approvalpolicy/manifest.go', import.meta.url), 'utf8');
  const declaration = manifest.match(/var governancePolicyCommands = \[\]string\{([\s\S]*?)\n\}/)[1];
  assert.deepEqual(evidenceCommandTypes, [...declaration.matchAll(/"(TOKEN_[A-Z_]+)"/g)].map(match => match[1]));
  assert.equal(requiredEvidenceCases.CURSOR_ISOLATION.length, 45);
  const fixture = signedFixture();
  for (const entry of fixture.bundle.entries) {
    const report = fixture.reports.get(entry.envelope.report_path);
    for (const item of report.cases) {
      const missing = structuredClone(report); missing.cases = missing.cases.filter(value => value.id !== item.id);
      assert.throws(() => validateReportSemantics(missing, entry.envelope, fixture.plan, 'FULL', now), /case_coverage_invalid/);
    }
    for (const mutate of [r => { r.cases.push(structuredClone(r.cases[0])); }, r => { r.cases[0].id = 'invented'; }, r => { r.validation_level = 'LOCAL_FIXTURE'; }, r => { r.validation_level = 'VALIDATED_HTTPS_REPLAY'; }, r => { r.cases[0].artifact_paths = ['absent.txt']; }]) {
      const changed = structuredClone(report); mutate(changed);
      assert.throws(() => validateReportSemantics(changed, entry.envelope, fixture.plan, 'FULL', now), /model_token_evidence_/);
    }
  }
});

test('external trust rejects unknown source, role, environment, scope, key and time', async t => {
  const { fixture, options, save } = await onDisk(t);
  const original = structuredClone(fixture.trust);
  for (const mutate of [
    k => { k.source = 'other'; }, k => { k.environment = 'CLOUD_UAT'; }, k => { k.kinds = ['OBSERVATION']; },
    k => { k.executor_references = ['wrong']; }, k => { k.reviewer_references = ['wrong']; }, k => { k.approval_references = ['wrong']; },
    k => { k.key_id = 'unknown'; }, k => { k.public_key_base64url = 'A'.repeat(43); },
    k => { k.valid_from = stamp(-1000); }, k => { k.valid_until = stamp(-1); },
  ]) {
    fixture.trust = structuredClone(original); mutate(fixture.trust.keys[0]); await save();
    await assert.rejects(verifyModelTokenEvidence(options), /model_token_evidence_/);
  }
  fixture.trust = structuredClone(original); fixture.trust.keys.push(structuredClone(original.keys[0])); await save();
  await assert.rejects(verifyModelTokenEvidence(options), /schema_invalid|key_duplicate/);
  fixture.trust = structuredClone(original); fixture.trust.max_age_seconds = 59; await save();
  await assert.rejects(verifyModelTokenEvidence(options), /trust_window_invalid/);
  fixture.trust.max_age_seconds = 86401; await save();
  await assert.rejects(verifyModelTokenEvidence(options), /schema_invalid/);
  fixture.trust = structuredClone(original); await save();
  const internalTrust = path.join(options.rootPath, 'trust.json'); await writeFile(internalTrust, JSON.stringify(fixture.trust));
  await assert.rejects(verifyModelTokenEvidence({ ...options, trustPath: internalTrust }), /external_trust_required/);
});

test('signature domain, canonical encoding and all bound bytes fail closed', async t => {
  const { fixture, options, save } = await onDisk(t);
  const entry = fixture.bundle.entries[0];
  const signature = entry.signature_base64url;
  entry.signature_base64url = sign(null, Buffer.from('wrong domain'), pair.privateKey).toString('base64url'); await save({ signReports: false });
  await assert.rejects(verifyModelTokenEvidence(options), /signature_invalid/);
  entry.signature_base64url = `${signature.slice(0, -1)}B`; await save({ signReports: false });
  await assert.rejects(verifyModelTokenEvidence(options), /signature_/);
  await save();
  await writeFile(options.planPath, `${JSON.stringify(fixture.plan)}\n`);
  await assert.rejects(verifyModelTokenEvidence(options), /plan_hash_invalid/);
  await save();
  await writeFile(path.join(options.rootPath, entry.envelope.report_path), '{}');
  await assert.rejects(verifyModelTokenEvidence(options), /report_hash_invalid/);
  await save();
  await writeFile(path.join(options.rootPath, 'artifacts/GATE_MATRIX.txt'), 'tampered');
  await assert.rejects(verifyModelTokenEvidence(options), /artifact_hash_invalid/);
  await save();
  const report = fixture.reports.get(entry.envelope.report_path); report.candidate.commit = 'c'.repeat(40); await save();
  await assert.rejects(verifyModelTokenEvidence(options), /report_binding_invalid/);
});

test('missing and duplicate kinds, malformed schema, duplicate JSON keys and unsafe integers are rejected', async t => {
  const { fixture, options, save } = await onDisk(t);
  const original = structuredClone(fixture.bundle);
  fixture.bundle.entries.pop(); await save();
  await assert.rejects(verifyModelTokenEvidence(options), /schema_invalid/);
  fixture.bundle = structuredClone(original); fixture.bundle.entries[16] = structuredClone(fixture.bundle.entries[0]); await save();
  await assert.rejects(verifyModelTokenEvidence(options), /schema_invalid|kind_coverage_invalid/);
  fixture.bundle = structuredClone(original); await save();
  await writeFile(options.bundlePath, JSON.stringify(fixture.bundle).replace('"schema_version":1', '"schema_version":1,"schema_version":1'));
  await assert.rejects(verifyModelTokenEvidence(options), /json_invalid/);
  await save();
  const observation = fixture.reports.get('reports/OBSERVATION.json');
  for (const value of [9007199254740992, -1, null, '10', 1.1]) {
    observation.observation.stages[0].samples = value; await save();
    await assert.rejects(verifyModelTokenEvidence(options), value === 9007199254740992 ? /json_invalid/ : /report_schema_invalid/);
  }
});

test('raw signed decimal tokens cannot round across counts or thresholds or underflow unauthorized effects', async t => {
  const { fixture, options, save } = await onDisk(t);
  const entry = fixture.bundle.entries.find(item => item.envelope.kind === 'OBSERVATION');
  const original = JSON.stringify(fixture.reports.get(entry.envelope.report_path));
  async function rawReport(bytes) {
    entry.envelope.report_sha256 = sha(bytes);
    entry.signature_base64url = sign(null, evidenceSignedBytes(entry, fixture.trust.expected_plan_sha256), pair.privateKey).toString('base64url');
    await writeFile(path.join(options.rootPath, entry.envelope.report_path), bytes);
    await writeFile(options.bundlePath, JSON.stringify(fixture.bundle));
  }
  for (const [source, replacement] of [
    ['"samples":10', '"samples":9.999999999999999999'],
    ['"unauthorized_effects":0', '"unauthorized_effects":1e-400'],
    ['"error_rate":0.01', '"error_rate":0.0100000000000000001'],
    ['"samples":10', '"samples":1e100000000'],
    ['"samples":10', `"samples":0.${'0'.repeat(128)}1`],
  ]) {
    const raw = original.replace(source, replacement); assert.notEqual(raw, original);
    await rawReport(raw);
    await assert.rejects(verifyModelTokenEvidence(options), /json_invalid/);
  }
  await rawReport(original.replaceAll('"samples":10', '"samples":1e1').replaceAll('"error_rate":0.01', '"error_rate":0.0100').replaceAll('"capacity_utilization":0.7', '"capacity_utilization":0.70'));
  assert.equal((await verifyModelTokenEvidence(options)).decision, 'EVIDENCE_VERIFIED');
  await save();
});

test('bundle/root aliases cannot move a same-directory trust root outside the claimed collection', async t => {
  const { options, directory } = await onDisk(t);
  const outsideBundle = path.join(directory, 'bundle.json');
  await writeFile(outsideBundle, await readFile(options.bundlePath));
  await assert.rejects(verifyModelTokenEvidence({ ...options, bundlePath: outsideBundle }), /bundle_outside_root/);
  const alias = path.join(directory, 'alias');
  await symlink(directory, alias, process.platform === 'win32' ? 'junction' : 'dir');
  await assert.rejects(verifyModelTokenEvidence({ ...options, bundlePath: path.join(alias, 'bundle.json') }), /bundle_outside_root/);
});

test('recently signed reports cannot refresh seven-day-old observed metrics', async t => {
  const { fixture, options, save } = await onDisk(t);
  const report = fixture.reports.get('reports/OBSERVATION.json');
  const entry = fixture.bundle.entries.find(item => item.envelope.kind === 'OBSERVATION');
  fixture.trust.keys[0].valid_from = stamp(-8 * 86400000);
  report.started_at = stamp(-8 * 86400000); entry.envelope.started_at = report.started_at;
  for (const stage of report.observation.stages) {
    stage.started_at = new Date(Date.parse(stage.started_at) - 7 * 86400000).toISOString();
    stage.ended_at = new Date(Date.parse(stage.ended_at) - 7 * 86400000).toISOString();
  }
  await save();
  await assert.rejects(verifyModelTokenEvidence(options), /observation_freshness_invalid/);
  report.ended_at = report.observation.stages.at(-1).ended_at; entry.envelope.ended_at = report.ended_at;
  await save();
  await assert.rejects(verifyModelTokenEvidence(options), /trust_window_invalid/);
});

test('all real artifact bytes are required, bounded and cannot traverse links or alias reports', async t => {
  const { fixture, options, save, directory } = await onDisk(t);
  const report = fixture.reports.get('reports/GATE_MATRIX.json');
  const original = structuredClone(report);
  const artifactPath = path.join(options.rootPath, report.artifacts[0].path);
  await rm(artifactPath);
  await assert.rejects(verifyModelTokenEvidence(options), /file_unavailable/);
  await save();
  await writeFile(artifactPath, Buffer.alloc(4 * 1024 * 1024 + 1));
  await assert.rejects(verifyModelTokenEvidence(options), /file_invalid/);
  for (const relative of ['../outside.txt', '/outside.txt', 'reports/GATE_MATRIX.json']) {
    report.artifacts[0].path = relative; for (const item of report.cases) item.artifact_paths = [relative]; await save();
    await assert.rejects(verifyModelTokenEvidence(options), /report_schema_invalid|artifact_report_alias/);
  }
  fixture.reports.set('reports/GATE_MATRIX.json', original); await save();
  const outside = path.join(directory, 'outside'); await mkdir(outside);
  await writeFile(path.join(outside, 'raw.txt'), 'outside');
  await symlink(outside, path.join(options.rootPath, 'linked'), process.platform === 'win32' ? 'junction' : 'dir');
  original.artifacts[0] = { path: 'linked/raw.txt', sha256: sha('outside') };
  for (const item of original.cases) item.artifact_paths = ['linked/raw.txt']; await save();
  await assert.rejects(verifyModelTokenEvidence(options), /path_link|path_outside_root/);
});

test('observation boundaries use lower sample limits, upper metric limits and NO_GO before HOLD', () => {
  const fixture = signedFixture(); const original = fixture.reports.get('reports/OBSERVATION.json');
  const evaluate = mutate => { const report = structuredClone(original); mutate(report); return evaluateObservation(fixture.plan, report, 'FULL', now); };
  assert.equal(evaluate(() => {}), 'EVIDENCE_VERIFIED');
  assert.equal(evaluate(r => { r.observation.stages[0].samples = 11; }), 'EVIDENCE_VERIFIED');
  assert.equal(evaluate(r => { r.observation.stages[0].samples = 9; }), 'HOLD');
  assert.equal(evaluate(r => { r.observation.stages[0].started_at = stamp(-239999); }), 'HOLD');
  for (const [metric, threshold] of Object.entries(fixture.plan.stages[0].thresholds)) {
    assert.equal(evaluate(r => { r.observation.stages[0].metrics[metric] = threshold.advance_max; }), 'EVIDENCE_VERIFIED');
    if (threshold.advance_max < threshold.pause_above) assert.equal(evaluate(r => { r.observation.stages[0].metrics[metric] = threshold.pause_above; }), 'HOLD');
    assert.equal(evaluate(r => { r.observation.stages[0].samples = 1; r.observation.stages[0].metrics[metric] = threshold.pause_above + 1; }), 'NO_GO');
  }
  for (const mutate of [r => { r.observation.stages.reverse(); }, r => { r.observation.stages.pop(); }, r => { r.observation.stages[0].cohort = 'wrong'; }, r => { r.observation.stages[0].traffic_percent = 2; }, r => { r.observation.stages[0].approval_reference = 'wrong'; }, r => { r.observation.stages[1].started_at = stamp(-181000); }, r => { r.observation.stages[2].ended_at = stamp(1); }]) assert.throws(() => evaluate(mutate), /model_token_evidence_/);
});

test('signed FAIL, nonzero exit and SKIP never become verified and NO_GO wins over insufficient samples', async t => {
  const { fixture, options, save } = await onDisk(t);
  const report = fixture.reports.get('reports/GATE_MATRIX.json');
  report.cases[0].status = 'SKIP'; await save();
  assert.equal((await verifyModelTokenEvidence(options)).decision, 'HOLD');
  report.cases[0].status = 'FAIL'; await save();
  assert.equal((await verifyModelTokenEvidence(options)).decision, 'NO_GO');
  report.cases[0].status = 'PASS'; report.exit_code = 1; await save();
  assert.equal((await verifyModelTokenEvidence(options)).decision, 'NO_GO');
  report.exit_code = 0; report.status = 'FAIL'; fixture.bundle.entries[0].envelope.status = 'FAIL';
  fixture.reports.get('reports/OBSERVATION.json').observation.stages[0].samples = 0; await save();
  assert.equal((await verifyModelTokenEvidence(options)).decision, 'NO_GO');
});

test('legacy links stay compatible until both ninety days and two later stable production versions pass', () => {
  const plan = planFixture(); const legacy = { compatibility_retained: true, stable_releases: [] };
  assert.equal(validateLegacyRetention(plan, legacy, now), 'RETAIN_REQUIRED');
  plan.legacy_links.ga_at = new Date(now - 90 * 86400000).toISOString(); plan.legacy_links.ga_version = '1.0.0';
  const one = { version: '1.1.0', released_at: new Date(now - 10 * 86400000).toISOString(), environment: 'PRODUCTION', channel: 'STABLE' };
  const two = { ...one, version: '1.2.0', released_at: new Date(now - 86400000).toISOString() };
  for (const releases of [[], [one]]) assert.equal(validateLegacyRetention(plan, { ...legacy, stable_releases: releases }, now), 'RETAIN_REQUIRED');
  legacy.stable_releases = [one, two];
  assert.equal(validateLegacyRetention(plan, legacy, now), 'MINIMUM_MET_COMPATIBILITY_RETAINED');
  plan.legacy_links.ga_at = new Date(now - 89 * 86400000).toISOString();
  assert.equal(validateLegacyRetention(plan, legacy, now), 'RETAIN_REQUIRED');
  for (const releases of [[one, one], [two, one], [{ ...one, environment: 'CLOUD_UAT' }], [{ ...one, version: '1.1.0-beta.1' }], [{ ...one, version: '9007199254740992.1.0' }]]) assert.throws(() => validateLegacyRetention(plan, { ...legacy, stable_releases: releases }, now), /model_token_evidence_/);
  assert.throws(() => validateLegacyRetention(plan, { ...legacy, compatibility_retained: false }, now), /compatibility_required/);
});

test('default CLI cannot verify without external trust and emits only fixed safe diagnostics', async t => {
  const { options } = await onDisk(t);
  const script = new URL('../../../scripts/account-platform-model-token-evidence.mjs', import.meta.url);
  const result = spawnSync(process.execPath, [fileURLToPath(script), '--plan', options.planPath, '--bundle', options.bundlePath, '--root', options.rootPath], { encoding: 'utf8', timeout: 15000, env: { PATH: process.env.PATH, SystemRoot: process.env.SystemRoot } });
  assert.equal(result.status, 1); assert.equal(result.stdout, '');
  assert.equal(result.stderr.trim(), 'model_token_evidence_trust_configuration_missing');
  assert.ok(!result.stderr.includes(options.rootPath));
});

test('complete CLI accepts only actual byte-verified synthetic fixtures and the collection has a total bound', async t => {
  const fixture = signedFixture(); const delta = Date.now() - now;
  const shift = value => {
    if (Array.isArray(value)) return value.map(shift);
    if (value !== null && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, shift(item)]));
    return typeof value === 'string' && /^2026-09-0[567]T/.test(value) ? new Date(Date.parse(value) + delta).toISOString() : value;
  };
  fixture.bundle = shift(fixture.bundle); fixture.trust = shift(fixture.trust);
  fixture.reports = new Map([...fixture.reports].map(([name, report]) => [name, shift(report)]));
  const { options, save } = await onDisk(t, fixture);
  const result = spawnSync(process.execPath, [fileURLToPath(new URL('../../../scripts/account-platform-model-token-evidence.mjs', import.meta.url)), '--plan', options.planPath, '--bundle', options.bundlePath, '--root', options.rootPath], { encoding: 'utf8', timeout: 15000, env: { PATH: process.env.PATH, SystemRoot: process.env.SystemRoot, PINVOU_MODEL_TOKEN_EVIDENCE_TRUST_FILE: options.trustPath } });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(JSON.parse(result.stdout).decision, 'EVIDENCE_VERIFIED');
  assert.equal(JSON.parse(result.stdout).deployment_status, 'NOT_DEPLOYED');
  const big = Buffer.alloc(2 * 1024 * 1024, 'x');
  for (const [relative] of fixture.artifacts) fixture.artifacts.set(relative, big);
  for (const report of fixture.reports.values()) report.artifacts[0].sha256 = sha(big);
  await save();
  await assert.rejects(verifyModelTokenEvidence({ ...options, now: Date.now() }), /total_size_exceeded/);
});
