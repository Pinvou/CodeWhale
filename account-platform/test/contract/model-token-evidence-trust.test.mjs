import assert from 'node:assert/strict';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { mkdtemp, mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { closedModelTokenGates } from '../../../scripts/account-platform-model-token-release-plan.mjs';
import { evidenceKinds, requiredEvidenceCases, evidenceSignedBytes } from '../../../scripts/account-platform-model-token-evidence-policy.mjs';
import { verifyModelTokenEvidence } from '../../../scripts/account-platform-model-token-evidence.mjs';
const hash = 'a'.repeat(64);
const now = Date.parse('2026-09-06T12:00:00Z');
const sha = bytes => createHash('sha256').update(bytes).digest('hex');
// Same closed plan fixture as model-token-release-plan.test.mjs; this is an
// ephemeral cryptographic fixture, never evidence of a target deployment.
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

async function exportFixture(t) {
  const base = await mkdtemp(path.join(tmpdir(), 'pinvou-signed-evidence-'));
  t.after(() => rm(base, { recursive: true, force: true }));
  const rootPath = path.join(base, 'export'); await mkdir(path.join(rootPath, 'reports'), { recursive: true }); await mkdir(path.join(rootPath, 'artifacts'));
  const plan = fixture(); const planPath = path.join(base, 'plan.json'); const planRaw = Buffer.from(JSON.stringify(plan)); await writeFile(planPath, planRaw);
  const receipt = Buffer.from('synthetic finite crypto fixture, not deployment evidence\n'); await writeFile(path.join(rootPath, 'artifacts/receipt.txt'), receipt);
  const { publicKey, privateKey } = generateKeyPairSync('ed25519'); const planHash = sha(planRaw);
  const reports = new Map(); const bundle = { schema_version: 1, stage: 'INTERNAL', entries: [] };
  function signedEntry(kind, report) {
    const { schema_version, candidate, environment, status, started_at, ended_at, executor_reference, reviewer_reference, approval_reference } = report;
    const entry = { source: 'synthetic', key_id: 'key.test', envelope: { schema_version, candidate, environment, kind, status, started_at, ended_at, executor_reference, reviewer_reference, approval_reference, report_sha256: sha(Buffer.from(JSON.stringify(report))), report_path: `reports/${kind.toLowerCase()}.json` } };
    entry.signature_base64url = sign(null, evidenceSignedBytes(entry, planHash), privateKey).toString('base64url'); return entry;
  }
  for (const kind of evidenceKinds) {
    const report = { schema_version: 1, candidate: structuredClone(plan.candidate), environment: plan.environment, kind, status: 'PASS', started_at: '2026-09-06T10:00:00Z', ended_at: '2026-09-06T11:00:00Z', executor_reference: 'operator.test', reviewer_reference: 'reviewer.test', approval_reference: 'approval.test', validation_level: 'TARGET_ENVIRONMENT', exit_code: 0, artifacts: [{ path: 'artifacts/receipt.txt', sha256: sha(receipt) }], cases: requiredEvidenceCases[kind].map(id => ({ id, status: 'PASS', artifact_paths: ['artifacts/receipt.txt'] })), observation: null, legacy_links: null };
    if (kind === 'OBSERVATION') { const stage = plan.stages[0]; report.observation = { stages: [{ stage: stage.stage, cohort: stage.cohort, traffic_percent: stage.traffic_percent, approval_reference: stage.approval_reference, started_at: report.started_at, ended_at: report.ended_at, samples: stage.minimum_samples, metrics: Object.fromEntries(Object.keys(stage.thresholds).map(key => [key, 0])) }] }; }
    if (kind === 'LEGACY_LINKS') report.legacy_links = { compatibility_retained: true, stable_releases: [] };
    reports.set(kind, report); const entry = signedEntry(kind, report); bundle.entries.push(entry); await writeFile(path.join(rootPath, entry.envelope.report_path), JSON.stringify(report));
  }
  const bundlePath = path.join(rootPath, 'bundle.json'); await writeFile(bundlePath, JSON.stringify(bundle));
  const trust = { schema_version: 1, expected_plan_sha256: planHash, max_age_seconds: 86400, keys: [{ key_id: 'key.test', public_key_base64url: publicKey.export({ format: 'der', type: 'spki' }).subarray(-32).toString('base64url'), source: 'synthetic', environment: plan.environment, kinds: [...evidenceKinds], executor_references: ['operator.test'], reviewer_references: ['reviewer.test'], approval_references: ['approval.test'], valid_from: '2026-09-06T09:00:00Z', valid_until: '2026-09-07T00:00:00Z' }] };
  const trustPath = path.join(base, 'trust.json'); await writeFile(trustPath, JSON.stringify(trust));
  async function saveReport(kind, report) { const entry = signedEntry(kind, report); bundle.entries[bundle.entries.findIndex(item => item.envelope.kind === kind)] = entry; await writeFile(path.join(rootPath, entry.envelope.report_path), JSON.stringify(report)); await writeFile(bundlePath, JSON.stringify(bundle)); }
  return { plan, reports, bundle, trust, rootPath, bundlePath, planPath, trustPath, saveReport, options: { planPath, bundlePath, rootPath, trustPath, now } };
}
test('real Ed25519 signatures bind all seventeen reports and actual artifacts, without deploying', async t => {
  const f = await exportFixture(t); const result = await verifyModelTokenEvidence(f.options);
  assert.equal(result.decision, 'EVIDENCE_VERIFIED'); assert.equal(result.reports_verified, 17); assert.equal(result.artifacts_verified, 1); assert.equal(result.deployment_status, 'NOT_DEPLOYED'); assert.equal(result.legacy_retention, 'RETAIN_REQUIRED');
});
test('external trust, exact source, key window and signature reject unauthorized envelopes', async t => {
  const f = await exportFixture(t);
  for (const mutate of [v => { v.keys[0].source = 'other'; }, v => { v.keys[0].reviewer_references = ['other']; }, v => { v.keys[0].valid_until = '2026-09-06T10:30:00Z'; }, v => { v.expected_plan_sha256 = 'b'.repeat(64); }]) {
    const trust = structuredClone(f.trust); mutate(trust); await writeFile(f.trustPath, JSON.stringify(trust)); await assert.rejects(verifyModelTokenEvidence(f.options), /model_token_evidence_/);
  }
  await writeFile(f.trustPath, JSON.stringify(f.trust)); const inside = path.join(f.rootPath, 'trust.json'); await writeFile(inside, JSON.stringify(f.trust)); await assert.rejects(verifyModelTokenEvidence({ ...f.options, trustPath: inside }), /external_trust_required/);
  f.bundle.entries[0].signature_base64url = Buffer.alloc(64).toString('base64url'); await writeFile(f.bundlePath, JSON.stringify(f.bundle)); await assert.rejects(verifyModelTokenEvidence(f.options), /signature_invalid/);
});
test('plan, report and artifact bytes cannot be replaced by claimed digests', async t => {
  const f = await exportFixture(t);
  const planRaw = await readFile(f.planPath); await writeFile(f.planPath, Buffer.concat([planRaw, Buffer.from('\n')])); await assert.rejects(verifyModelTokenEvidence(f.options), /plan_hash_invalid/); await writeFile(f.planPath, planRaw);
  const reportPath = path.join(f.rootPath, f.bundle.entries[0].envelope.report_path); const reportRaw = await readFile(reportPath); await writeFile(reportPath, Buffer.concat([reportRaw, Buffer.from('\n')])); await assert.rejects(verifyModelTokenEvidence(f.options), /report_hash_invalid/); await writeFile(reportPath, reportRaw);
  await writeFile(path.join(f.rootPath, 'artifacts/receipt.txt'), 'tampered'); await assert.rejects(verifyModelTokenEvidence(f.options), /artifact_hash_invalid/);
});
test('signed reports still enforce complete cases, target evidence, skip and failed observations', async t => {
  const f = await exportFixture(t); const original = f.reports.get('WRITER_FENCE');
  for (const mutate of [v => { v.cases.pop(); }, v => { v.cases[0].artifact_paths = ['artifacts/missing.txt']; }, v => { v.validation_level = 'LOCAL_FIXTURE'; }]) { const report = structuredClone(original); mutate(report); await f.saveReport('WRITER_FENCE', report); await assert.rejects(verifyModelTokenEvidence(f.options), /model_token_evidence_/); }
  const skipped = structuredClone(original); skipped.cases[0].status = 'SKIP'; await f.saveReport('WRITER_FENCE', skipped); assert.equal((await verifyModelTokenEvidence(f.options)).decision, 'HOLD');
  await f.saveReport('WRITER_FENCE', original); const observation = structuredClone(f.reports.get('OBSERVATION')); observation.observation.stages[0].metrics.unauthorized_effects = 1; await f.saveReport('OBSERVATION', observation); assert.equal((await verifyModelTokenEvidence(f.options)).decision, 'NO_GO');
});
