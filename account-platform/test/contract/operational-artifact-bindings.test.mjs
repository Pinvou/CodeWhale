import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtemp, writeFile, readFile, rm, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { artifactGateArguments, verifyArtifactBindings } from '../../../scripts/account-platform-artifact-bindings.mjs';
const root = fileURLToPath(new URL('../../../', import.meta.url));
const sha = bytes => createHash('sha256').update(bytes).digest('hex');
const release = 'a'.repeat(64); const deployment = 'synthetic-deployment';
async function fixture(t, kind = 'HA', environment = 'production-new') {
  const base = await mkdtemp(path.join(tmpdir(), 'pinvou-artifact-boundary-')); t.after(() => rm(base, { recursive: true, force: true }));
  const artifactRoot = path.join(base, 'export'); await import('node:fs/promises').then(fs => fs.mkdir(artifactRoot));
  const artifact = Buffer.from('synthetic receipt, not a deployment fact\n'); await writeFile(path.join(artifactRoot, 'receipt.txt'), artifact);
  const evidence = { environment, scenario: { receipt_sha256: sha(artifact) } }; const evidencePath = path.join(artifactRoot, 'evidence.json'); const evidenceBytes = Buffer.from(JSON.stringify(evidence)); await writeFile(evidencePath, evidenceBytes);
  const trust = { schema_version: 1, kind, environment, deployment_id: deployment, release_sha256: release, evidence_sha256: sha(evidenceBytes), artifacts: [{ claim: '/scenario/receipt_sha256', path: 'receipt.txt', sha256: sha(artifact) }] };
  const trustPath = path.join(base, 'independent-review.json'); await writeFile(trustPath, JSON.stringify(trust));
  return { base, artifactRoot, evidencePath, trustPath, evidence, trust, options: { rootPath: artifactRoot, evidencePath, trustPath, kind, environment, deploymentId: deployment, expectedReleaseSHA256: release } };
}
test('external reviewed bytes bind exactly, without claiming an exercise or release approval', async t => {
  const f = await fixture(t); const { result } = await verifyArtifactBindings(f.options);
  assert.equal(result.status, 'ARTIFACT_BINDINGS_VERIFIED'); assert.equal(result.artifacts_verified, 1); assert.equal(result.deployment_status, 'NOT_DEPLOYED'); assert.equal(result.exercise_verified, false); assert.equal(result.release_authorized, false);
});
test('self-provided JSON/digests cannot replace external trust or original artifacts', async t => {
  const f = await fixture(t);
  for (const missing of ['trustPath', 'rootPath', 'deploymentId', 'expectedReleaseSHA256']) await assert.rejects(verifyArtifactBindings({ ...f.options, [missing]: undefined }), /artifact_bindings_invalid/);
  const internalTrust = path.join(f.artifactRoot, 'trust.json'); await writeFile(internalTrust, JSON.stringify(f.trust)); await assert.rejects(verifyArtifactBindings({ ...f.options, trustPath: internalTrust }), /artifact_bindings_invalid/);
  await writeFile(path.join(f.artifactRoot, 'receipt.txt'), 'tampered'); await assert.rejects(verifyArtifactBindings(f.options), /artifact_bindings_invalid/);
});
test('trust rejects mismatched context, evidence bytes, missing/duplicate claims and traversal', async t => {
  const f = await fixture(t);
  const mutations = [v => { v.kind = 'RECOVERY'; }, v => { v.environment = 'production-equivalent'; }, v => { v.deployment_id = 'other-deployment'; }, v => { v.release_sha256 = 'b'.repeat(64); }, v => { v.evidence_sha256 = 'b'.repeat(64); }, v => { v.unknown = true; }, v => { v.artifacts = []; }, v => { v.artifacts.push(v.artifacts[0]); }, v => { v.artifacts[0].claim = '/unknown'; }, v => { v.artifacts[0].path = '../receipt.txt'; }, v => { v.artifacts[0].path = f.evidencePath; }, v => { v.artifacts[0].sha256 = 'b'.repeat(64); }];
  for (const mutate of mutations) { const trust = structuredClone(f.trust); mutate(trust); await writeFile(f.trustPath, JSON.stringify(trust)); await assert.rejects(verifyArtifactBindings(f.options), /artifact_bindings_invalid/); }
  await writeFile(f.trustPath, JSON.stringify(f.trust)); await writeFile(f.evidencePath, JSON.stringify({ ...f.evidence, extra: true })); await assert.rejects(verifyArtifactBindings(f.options), /artifact_bindings_invalid/);
});
test('duplicate JSON keys and symlinked evidence roots fail closed', async t => {
  const f = await fixture(t);
  const raw = JSON.stringify(f.trust).replace('"schema_version":1', '"schema_version":0,"schema_version":1'); await writeFile(f.trustPath, raw); await assert.rejects(verifyArtifactBindings(f.options), /artifact_bindings_invalid/);
  await writeFile(f.trustPath, JSON.stringify(f.trust)); const link = path.join(f.base, 'linked-export');
  try { await symlink(f.artifactRoot, link, process.platform === 'win32' ? 'junction' : 'dir'); } catch (error) { assert.equal(error.code, 'EPERM'); t.skip('host cannot create a test link'); return; }
  await assert.rejects(verifyArtifactBindings({ ...f.options, rootPath: link }), /artifact_bindings_invalid/);
});

test('bound raw JSON numbers cannot underflow or round onto metric thresholds', async t => {
  const f = await fixture(t);
  for (const literal of ['1e-999', '0.0000000000000000000001e-999', '0.100000000000000005', '9007199254740993']) {
    const raw = Buffer.from(`{"environment":"production-new","scenario":{"receipt_sha256":"${f.trust.artifacts[0].sha256}","metric":${literal}}}`);
    await writeFile(f.evidencePath, raw); await writeFile(f.trustPath, JSON.stringify({ ...f.trust, evidence_sha256: sha(raw) }));
    await assert.rejects(verifyArtifactBindings(f.options), /artifact_bindings_invalid/);
  }
  const raw = Buffer.from(`{"environment":"production-new","scenario":{"receipt_sha256":"${f.trust.artifacts[0].sha256}","metric":0.10}}`);
  await writeFile(f.evidencePath, raw); await writeFile(f.trustPath, JSON.stringify({ ...f.trust, evidence_sha256: sha(raw) }));
  assert.equal((await verifyArtifactBindings(f.options)).evidence.scenario.metric, 0.1);
});
test('operational CLI without original artifacts never emits production evidence passed', async t => {
  const f = await fixture(t);
  for (const script of ['ha', 'recovery']) { const result = spawnSync(process.execPath, [path.join(root, `scripts/account-platform-${script}-gate.mjs`), f.evidencePath], { encoding: 'utf8' }); assert.equal(result.status, 1); assert.match(result.stderr, /artifact_bindings_invalid/); assert.equal(result.stdout.includes('production'), false); }
});
test('production evidence mode fails closed while plain policy mode remains available', () => {
  for (const name of ['ha', 'recovery']) {
    const script = path.join(root, `scripts/account-platform-${name}-gate.mjs`);
    const policy = spawnSync(process.execPath, [script], { encoding: 'utf8' });
    assert.equal(policy.status, 0); assert.match(policy.stdout, /policy passed/);
    for (const args of [['--require-evidence'], ['--require-evidence', '--require-evidence']]) {
      const result = spawnSync(process.execPath, [script, ...args], { encoding: 'utf8' });
      assert.equal(result.status, 1); assert.equal(result.stdout, '');
      assert.match(result.stderr, /evidence file is required|duplicate --require-evidence/);
    }
  }
});
test('binding options preserve strict defaults and reject duplicate options', () => {
  assert.deepEqual(artifactGateArguments([]).remaining, []);
  const options = artifactGateArguments(['evidence.json', '--deployment-id=synthetic-deployment', '--trust-file=review.json', '--artifact-root=export', `--expected-release-sha256=${release}`]); assert.deepEqual(options.remaining, ['evidence.json']); assert.equal(options.deploymentId, deployment);
  assert.throws(() => artifactGateArguments(['--trust-file=a', '--trust-file=b']), /artifact_bindings_invalid/);
});

test('all four canonical CLI modes bind real files and preserve metric validation', async t => {
  const recent = new Date(Date.now() - 30_000).toISOString(); const now = new Date().toISOString();
  for (const mode of ['ha', 'recovery', 'local', 'performance']) {
    const f = await fixture(t); const receiptHash = sha(await readFile(path.join(f.artifactRoot, 'receipt.txt')));
    const policyName = mode === 'local' ? 'recovery' : mode; const policy = JSON.parse(await readFile(path.join(root, `account-platform/deploy/${policyName}-policy.json`), 'utf8')); let evidence;
    if (mode === 'ha') evidence = { schema_version: 1, environment: 'production-new', generated_at: now, failure_domains: 2, post_failure_headroom_percent: 40, workloads: policy.required_replicated_workloads.map(name => ({ name, desired_replicas: 2, ready_replicas: 2, failure_domains: 2, pdb_enforced: true, max_unavailable: 1 })), dependency_failovers: policy.required_dependency_failovers.map(name => ({ name, passed: true, committed_record_loss: 0, duplicate_financial_effects: 0 })), scenarios: policy.required_scenarios.map(name => ({ name, passed: true, core_unavailable_seconds: 30, noncritical_unavailable_seconds: 60, real_provider_side_effects: 0, evidence_sha256: receiptHash })), identity_adapter_replacement_seconds: 60 };
    if (mode === 'recovery') evidence = { schema_version: 1, environment: 'production-new', generated_at: now, backups: ['billing', 'token', 'account', 'keycloak'].map(database => ({ database, last_wal_archived_at: recent, last_incremental_at: recent, last_full_at: recent, transport_encryption: 'TLS1.2+', at_rest_encryption: 'AES-256', key_separated: true, immutable_offsite: true, production_identity_can_delete: false, backup_id: `${database}-backup`, checksum_sha256: receiptHash })), measured: { financial_rpo_seconds: 30, configuration_rpo_seconds: 120, core_rto_seconds: 1800, noncritical_rto_seconds: 7200 }, exercises: ['monthly_sample', 'quarterly_end_to_end', 'annual_loss'].map(kind => ({ kind, completed_at: recent, audit_sql_passed: true, real_provider_side_effects: 0, open_remediations: 0 })), retention_deletion_scans: policy.retention.map(({ classification }) => ({ classification, scanned_at: recent, expired_eligible_count: 2, deleted_count: 2, maximum_deletion_delay_days: 1, receipt_sha256: receiptHash })) };
    if (mode === 'local') evidence = { schema_version: 1, environment: 'production-new', profile: 'local-only', deployment_id: deployment, generated_at: now, status: 'LOCAL_BOOTSTRAP_VERIFIED', risk_acceptance: { deployment_id: deployment, owner: 'synthetic reviewer', accepted_at: recent, same_host_loss_accepted: true, approval_receipt_sha256: receiptHash }, storage: { offsite: false, immutable: false, host_loss_recoverable: false, at_rest_encryption: 'AES-256', transport_encryption: 'LOCAL_ONLY', key_access_separated: true, application_identity_can_access: false, host_privileged_identity_can_delete: true }, cluster: { system_identifier: '1234567890123456789', databases: ['account', 'billing', 'token', 'keycloak', 'newapi'], backup_id: 'synthetic-backup', checksum_sha256: receiptHash, last_wal_archived_at: recent, last_daily_backup_at: recent, last_full_at: recent }, automation: { continuous_wal: true, daily_incremental: true, weekly_full: true, pre_migration_backup: true, archive_check: true, expire_auto: false, drop_wal_on_queue_full: false, daily_retention_days: 35, monthly_retention_months: 13 }, monitor: { checked_at: recent, archive_ok: true, scheduler_ok: true, notification_delivery: 'local_status_only', free_fraction: 0.4, inode_free_fraction: 0.4, free_bytes: 10 * 1024 ** 3, capacity_warning: false }, restore: { completed_at: recent, backup_id: 'synthetic-backup', system_identifier: '1234567890123456789', databases: ['account', 'billing', 'token', 'keycloak', 'newapi'], isolated: true, source_untouched: true, roles_verified: true, wal_replay_verified: true, real_provider_side_effects: 0, receipt_sha256: receiptHash, elapsed_seconds: 60 }, measured: { financial_rpo_seconds: 30, configuration_rpo_seconds: 120 } };
    if (mode === 'performance') evidence = { schema_version: 1, environment: 'production-equivalent', generated_at: now, release_sha256: release, dataset_sha256: receiptHash, report_sha256: receiptHash, steady_minutes: 30, maximum_declared_capacity_percent: 70, api: { samples: 10000, ...policy.api }, external: { ...policy.external }, stream: { samples: 1000, ...policy.stream }, holds: { ...policy.holds, conservation_violations: 0, duplicate_effects: 0 }, payments: { ...policy.payments, duplicate_financial_effects: 0 }, admin_pagination: { payment_orders: 1000000, pages: 5000, members_seen: 1000000, concurrent_creates: 1, concurrent_updates: 1, first_snapshot_p99_ms: 10000, duplicate_members: 0, omitted_members: 0, page_trace_sha256: receiptHash, mutation_trace_sha256: receiptHash }, events: { ...policy.events, lost_events: 0, high_risk_gate_triggered: true }, timing_segments: { platform_ms: 1, provider_ms: 1, queue_ms: 1, client_network_ms: 1 } };
    const artifacts = [];
    function collect(value, pointer = '') { for (const [key, item] of Object.entries(value)) { const location = `${pointer}/${key}`; if (key.endsWith('_sha256') && key !== 'release_sha256') artifacts.push({ claim: location, path: 'receipt.txt', sha256: item }); else if (item && typeof item === 'object') collect(item, location); } }
    collect(evidence); const raw = Buffer.from(JSON.stringify(evidence)); await writeFile(f.evidencePath, raw); const kind = { ha: 'HA', recovery: 'RECOVERY', local: 'LOCAL_RECOVERY', performance: 'PERFORMANCE' }[mode];
    await writeFile(f.trustPath, JSON.stringify({ ...f.trust, kind, environment: evidence.environment, evidence_sha256: sha(raw), artifacts }));
    const args = [path.join(root, `scripts/account-platform-${mode === 'local' ? 'recovery' : mode}-gate.mjs`), '--require-evidence', ...(mode === 'local' ? ['--local-only', '--bootstrap'] : []), f.evidencePath, `--trust-file=${f.trustPath}`, `--artifact-root=${f.artifactRoot}`, `--deployment-id=${deployment}`, `--expected-release-sha256=${release}`, ...(mode === 'performance' ? [`--page-trace=${path.join(f.artifactRoot, 'receipt.txt')}`, `--mutation-trace=${path.join(f.artifactRoot, 'receipt.txt')}`] : [])];
    const actual = spawnSync(process.execPath, args, { encoding: 'utf8' }); assert.equal(actual.status, 0, `${mode}: ${actual.stderr}`); const result = JSON.parse(actual.stdout); assert.equal(result.status, 'ARTIFACT_BINDINGS_VERIFIED'); assert.equal(result.release_authorized, false); assert.equal(result.exercise_verified, false); if (mode === 'local') assert.equal(result.declared_status, 'LOCAL_BOOTSTRAP_VERIFIED');
    await writeFile(path.join(f.artifactRoot, 'receipt.txt'), 'changed'); const tampered = spawnSync(process.execPath, args, { encoding: 'utf8' }); assert.equal(tampered.status, 1, mode); assert.match(tampered.stderr, /artifact_bindings_invalid/);
  }
});
