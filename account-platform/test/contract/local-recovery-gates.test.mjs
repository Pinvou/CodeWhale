import assert from 'node:assert/strict';
import { readFileSync, mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { validateLocalEvidence } from '../../../scripts/account-platform-local-recovery-gate.mjs';
import { validateEvidence, validatePolicy } from '../../../scripts/account-platform-recovery-gate.mjs';

const policy = JSON.parse(readFileSync(new URL('../../deploy/recovery-policy.json', import.meta.url), 'utf8'));
const hash = 'a'.repeat(64);
const deploymentId = 'local-rollout-test';
const now = Date.parse('2026-09-08T10:00:00Z');
const recent = new Date(now - 30_000).toISOString();

function evidence(bootstrap = false) {
  const cluster = {
    system_identifier: '1234567890123456789', databases: ['account', 'billing', 'token', 'keycloak', 'newapi'],
    backup_id: '20260908-090000F', checksum_sha256: hash,
    last_wal_archived_at: recent, last_daily_backup_at: recent, last_full_at: recent,
  };
  return {
    schema_version: 1, environment: 'production-new', profile: 'local-only', deployment_id: deploymentId,
    status: bootstrap ? 'LOCAL_BOOTSTRAP_VERIFIED' : 'LOCAL_ONLY_RECOVERABLE', generated_at: new Date(now).toISOString(),
    risk_acceptance: { deployment_id: deploymentId, owner: 'test-custodian', accepted_at: recent, same_host_loss_accepted: true, approval_receipt_sha256: hash },
    storage: { offsite: false, immutable: false, host_loss_recoverable: false, at_rest_encryption: 'AES-256', transport_encryption: 'LOCAL_ONLY', key_access_separated: true, application_identity_can_access: false, host_privileged_identity_can_delete: true },
    cluster,
    automation: { continuous_wal: true, daily_incremental: true, weekly_full: true, pre_migration_backup: true, archive_check: true, expire_auto: false, drop_wal_on_queue_full: false, daily_retention_days: 35, monthly_retention_months: 13 },
    monitor: { checked_at: recent, archive_ok: true, scheduler_ok: true, notification_delivery: 'local_status_only', free_fraction: 0.3, inode_free_fraction: 0.8, free_bytes: 10 * 1024 ** 3, capacity_warning: false },
    restore: { backup_id: cluster.backup_id, system_identifier: cluster.system_identifier, databases: [...cluster.databases], completed_at: recent, isolated: true, source_untouched: true, roles_verified: true, wal_replay_verified: true, real_provider_side_effects: 0, elapsed_seconds: 30, receipt_sha256: hash },
    measured: { financial_rpo_seconds: 20, configuration_rpo_seconds: 20, core_rto_seconds: 500, noncritical_rto_seconds: 800 },
    recovery_set: {
      deployment_id: deploymentId, postgres_backup_id: cluster.backup_id, manifest_sha256: hash,
      components: ['postgres', 'nats', 'newapi_files', 'site_configuration', 'edge_configuration', 'release_artifacts', 'kms_references'].map((component) => ({ component, restored_and_verified: true, receipt_sha256: hash })),
      redis_sessions_invalidated: true, domain_audit_sql_passed: true, cross_domain_reconciled: true, real_provider_side_effects: 0,
    },
    exercises: ['monthly_sample', 'quarterly_end_to_end'].map((kind) => ({ kind, completed_at: recent, audit_sql_passed: true, real_provider_side_effects: 0, open_remediations: 0, receipt_sha256: hash })),
    retention_deletion_scans: policy.retention.map(({ classification }) => ({ classification, scanned_at: recent, expired_eligible_count: 0, deleted_count: 0, maximum_deletion_delay_days: 0, receipt_sha256: hash })),
  };
}

test('explicit local mode preserves default offsite policy and separates bootstrap from public cutover', () => {
  assert.doesNotThrow(() => validatePolicy(policy));
  const data = evidence();
  assert.equal(validateLocalEvidence(policy, data, { deploymentId, now }), 'LOCAL_ONLY_RECOVERABLE');
  assert.throws(() => validateEvidence(policy, data, now));
  assert.throws(() => validateLocalEvidence(policy, data, { now }), /expected deployment/);
  assert.throws(() => validateLocalEvidence(policy, data, { deploymentId: 'other-rollout', now }), /does not match/);
  const initial = evidence(true);
  delete initial.recovery_set;
  delete initial.exercises;
  delete initial.retention_deletion_scans;
  delete initial.measured.core_rto_seconds;
  delete initial.measured.noncritical_rto_seconds;
  assert.equal(validateLocalEvidence(policy, initial, { deploymentId, now, bootstrap: true }), 'LOCAL_BOOTSTRAP_VERIFIED');
  assert.throws(() => validateLocalEvidence(policy, initial, { deploymentId, now }), /complete local business/);
  assert.throws(() => validateLocalEvidence(policy, data, { deploymentId, now, bootstrap: true }), /distinct status/);
});

const rejectedMutations = [
  ['environment', d => { d.environment = 'development'; }],
  ['risk binding', d => { d.risk_acceptance.deployment_id = 'other-rollout'; }],
  ['risk denied', d => { d.risk_acceptance.same_host_loss_accepted = false; }],
  ['risk receipt', d => { d.risk_acceptance.approval_receipt_sha256 = ''; }],
  ['offsite claim', d => { d.storage.offsite = true; }],
  ['immutable claim', d => { d.storage.immutable = true; }],
  ['host loss claim', d => { d.storage.host_loss_recoverable = true; }],
  ['privileged deletion claim', d => { d.storage.host_privileged_identity_can_delete = false; }],
  ['application backup access', d => { d.storage.application_identity_can_access = true; }],
  ['no encryption', d => { d.storage.at_rest_encryption = 'none'; }],
  ['plain network', d => { d.storage.transport_encryption = 'HTTP'; }],
  ['duplicate database', d => { d.cluster.databases[4] = 'account'; }],
  ['missing newapi', d => { d.cluster.databases.pop(); }],
  ['missing restored newapi', d => { d.restore.databases.pop(); }],
  ['wrong restored cluster', d => { d.restore.system_identifier = '9876543210123456789'; }],
  ['wrong backup', d => { d.restore.backup_id = 'other-backup'; }],
  ['WAL stale', d => { d.cluster.last_wal_archived_at = new Date(now - 61_000).toISOString(); }],
  ['future WAL', d => { d.cluster.last_wal_archived_at = new Date(now + 1000).toISOString(); }],
  ['future evidence', d => { d.generated_at = new Date(now + 1000).toISOString(); }],
  ['impossible date', d => { d.risk_acceptance.accepted_at = '2026-02-30T10:00:00Z'; }],
  ['stale full', d => { d.cluster.last_full_at = new Date(now - 9 * 86400_000).toISOString(); }],
  ['stale daily', d => { d.cluster.last_daily_backup_at = new Date(now - 27 * 3600_000).toISOString(); }],
  ['automatic expiration', d => { d.automation.expire_auto = true; }],
  ['WAL discard', d => { d.automation.drop_wal_on_queue_full = true; }],
  ['archive check disabled', d => { d.automation.archive_check = false; }],
  ['short retention', d => { d.automation.monthly_retention_months = 1; }],
  ['failed archive', d => { d.monitor.archive_ok = false; }],
  ['stale monitor', d => { d.monitor.checked_at = new Date(now - 601_000).toISOString(); }],
  ['critical disk', d => { d.monitor.free_bytes = 1024 ** 3; d.monitor.capacity_warning = true; }],
  ['concealed warning', d => { d.monitor.free_fraction = 0.15; }],
  ['no restore', d => { d.restore.wal_replay_verified = false; }],
  ['Provider effect', d => { d.restore.real_provider_side_effects = 1; }],
  ['negative RPO', d => { d.measured.financial_rpo_seconds = -1; }],
  ['string RPO', d => { d.measured.financial_rpo_seconds = '0'; }],
  ['boolean RTO', d => { d.measured.core_rto_seconds = false; }],
  ['NaN RTO', d => { d.measured.core_rto_seconds = NaN; }],
  ['infinite RTO', d => { d.measured.core_rto_seconds = Infinity; }],
  ['RTO exceeded', d => { d.measured.core_rto_seconds = 3601; }],
  ['missing NATS', d => { d.recovery_set.components.splice(1, 1); }],
  ['duplicate component', d => { d.recovery_set.components[1].component = 'postgres'; }],
  ['different recovery set', d => { d.recovery_set.postgres_backup_id = 'other-backup'; }],
  ['unaudited domain', d => { d.recovery_set.domain_audit_sql_passed = false; }],
  ['restored old sessions', d => { d.recovery_set.redis_sessions_invalidated = false; }],
  ['missing quarterly', d => { d.exercises.pop(); }],
  ['future exercise', d => { d.exercises[0].completed_at = new Date(now + 1000).toISOString(); }],
  ['open restoration issue', d => { d.exercises[0].open_remediations = 1; }],
  ['missing deletion scan', d => { d.retention_deletion_scans.pop(); }],
  ['negative deletion count', d => { d.retention_deletion_scans[0].expired_eligible_count = -1; d.retention_deletion_scans[0].deleted_count = -1; }],
];
for (const [name, mutate] of rejectedMutations) {
  test(`local recovery rejects ${name}`, () => {
    const data = evidence();
    mutate(data);
    assert.throws(() => validateLocalEvidence(policy, data, { deploymentId, now }));
  });
}

test('capacity warning remains visible but only critical capacity blocks', () => {
  const data = evidence();
  data.monitor.free_fraction = 0.15;
  data.monitor.capacity_warning = true;
  assert.equal(validateLocalEvidence(policy, data, { deploymentId, now }), 'LOCAL_ONLY_RECOVERABLE');
});

test('CLI requires explicit local mode, trusted deployment target and distinct bootstrap invocation', () => {
  const folder = mkdtempSync(path.join(os.tmpdir(), 'pinvou-local-recovery-test-'));
  const script = new URL('../../../scripts/account-platform-recovery-gate.mjs', import.meta.url);
  try {
    const file = path.join(folder, 'fixture.json');
    const data = evidence(true);
    const realNow = Date.now();
    const serialized = JSON.stringify(data).replaceAll(new Date(now).toISOString(), new Date(realNow).toISOString()).replaceAll(recent, new Date(realNow - 30_000).toISOString());
    writeFileSync(file, serialized);
    const run = (...args) => spawnSync(process.execPath, [fileURLToPath(script), ...args], { encoding: 'utf8' });
    assert.equal(run().status, 0);
    assert.notEqual(run(file).status, 0);
    assert.notEqual(run('--local-only', file).status, 0);
    assert.notEqual(run('--local-only', '--deployment-id', 'wrong-rollout', '--bootstrap', file).status, 0);
    assert.notEqual(run('--local-only', '--deployment-id', deploymentId, file).status, 0);
    const result = run('--local-only', '--deployment-id', deploymentId, '--bootstrap', file);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), 'LOCAL_BOOTSTRAP_VERIFIED');
    assert.notEqual(run('--local-only', '--deployment-id', deploymentId, '--bootstrap', file, 'ignored').status, 0);
  } finally {
    rmSync(folder, { recursive: true }); // Exact mkdtemp-owned fixture, never a caller path.
  }
});
