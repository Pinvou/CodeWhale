import assert from 'node:assert/strict';
import test from 'node:test';

import { assertExistingEntriesImmutable } from '../../../scripts/account-platform-migration-manifest.mjs';
import { validateEvidence, validatePolicy } from '../../../scripts/account-platform-recovery-gate.mjs';

const hash = 'a'.repeat(64);
const manifest = {
  schema_version: 1,
  algorithm: 'sha256-normalized-lf',
  domains: { account: [{ file: '000001_foundation.up.sql', version: 1, sha256: hash }] },
};

test('migration manifest rejects mutation, deletion, and insertion behind the latest version', () => {
  assert.doesNotThrow(() => assertExistingEntriesImmutable(manifest, structuredClone(manifest.domains), false));
  const changed = structuredClone(manifest.domains);
  changed.account[0].sha256 = 'b'.repeat(64);
  assert.throws(() => assertExistingEntriesImmutable(manifest, changed, false), /modified/);
  assert.throws(() => assertExistingEntriesImmutable(manifest, { account: [] }, false), /removed/);
  const inserted = structuredClone(manifest.domains);
  inserted.account.push({ file: '000000_old.up.sql', version: 0, sha256: hash });
  assert.throws(() => assertExistingEntriesImmutable(manifest, inserted, true), /must advance/);
});

const policy = {
  schema_version: 1,
  objectives: { rpo_seconds: { financial: 60, configuration: 300 }, rto_seconds: { core: 3600, noncritical: 14400 } },
  backups: {
    continuous_wal: true,
    incremental_interval_hours: 24,
    full_interval_days: 7,
    pre_migration_snapshot: true,
    transport_encryption_minimum: 'TLS1.2',
    at_rest_encryption_minimum: 'AES-256',
    key_separation: true,
    immutable_offsite_copy: true,
  },
  exercises: { monthly_sample_days: 35, quarterly_end_to_end_days: 100, annual_loss_days: 400 },
  retention: ['payment_evidence', 'ledger', 'audit', 'billing_usage'].map((classification) => ({ classification, retention_days: 1095, deletion_sla_days: 30 })).concat({ classification: 'ordinary_runtime_logs', retention_days: 90, deletion_sla_days: 30 }),
};

function evidence(now) {
  const recent = new Date(now - 30_000).toISOString();
  const backups = ['billing', 'token', 'account', 'keycloak'].map((database) => ({
    database,
    last_wal_archived_at: recent,
    last_incremental_at: recent,
    last_full_at: recent,
    transport_encryption: 'TLS1.2+',
    at_rest_encryption: 'AES-256',
    key_separated: true,
    immutable_offsite: true,
    production_identity_can_delete: false,
    backup_id: `${database}-backup`,
    checksum_sha256: hash,
  }));
  const exercises = ['monthly_sample', 'quarterly_end_to_end', 'annual_loss'].map((kind) => ({
    kind,
    completed_at: recent,
    audit_sql_passed: true,
    real_provider_side_effects: 0,
    open_remediations: 0,
  }));
  const retention_deletion_scans = policy.retention.map(({ classification }) => ({
    classification,
    scanned_at: recent,
    expired_eligible_count: 2,
    deleted_count: 2,
    maximum_deletion_delay_days: 1,
    receipt_sha256: hash,
  }));
  return {
    schema_version: 1,
    environment: 'production-new',
    generated_at: new Date(now).toISOString(),
    backups,
    measured: { financial_rpo_seconds: 30, configuration_rpo_seconds: 120, core_rto_seconds: 1800, noncritical_rto_seconds: 7200 },
    exercises,
    retention_deletion_scans,
  };
}

test('recovery evidence gate enforces tiered objectives, cadence, encryption, and deletion receipts', () => {
  const now = Date.parse('2026-08-24T00:00:00Z');
  assert.doesNotThrow(() => validatePolicy(policy));
  assert.doesNotThrow(() => validateEvidence(policy, evidence(now), now));
  const staleWal = evidence(now);
  staleWal.backups.find(({ database }) => database === 'billing').last_wal_archived_at = new Date(now - 61_000).toISOString();
  assert.throws(() => validateEvidence(policy, staleWal, now), /exceeds RPO/);
  const missingReceipt = evidence(now);
  missingReceipt.retention_deletion_scans.pop();
  assert.throws(() => validateEvidence(policy, missingReceipt, now), /deletion scan is missing/);
});

test('recovery rejects invalid measurements, future completed evidence and negative counts', () => {
  const now = Date.parse('2026-08-24T00:00:00Z');
  for (const invalid of [-1, null, '30']) {
    const measured = evidence(now); measured.measured.financial_rpo_seconds = invalid;
    assert.throws(() => validateEvidence(policy, measured, now), /financial RPO/);
  }
  const future = evidence(now); future.backups[0].last_wal_archived_at = new Date(now + 1_000).toISOString();
  assert.throws(() => validateEvidence(policy, future, now), /last_wal_archived_at|WAL/);
  const futureExercise = evidence(now); futureExercise.exercises[0].completed_at = new Date(now + 1_000).toISOString();
  assert.throws(() => validateEvidence(policy, futureExercise, now), /completed_at|exercise/);
  const negativeCounts = evidence(now); negativeCounts.retention_deletion_scans[0].expired_eligible_count = -1; negativeCounts.retention_deletion_scans[0].deleted_count = -1;
  assert.throws(() => validateEvidence(policy, negativeCounts, now), /eligible records/);
  const negativeDelay = evidence(now); negativeDelay.retention_deletion_scans[0].maximum_deletion_delay_days = -1;
  assert.throws(() => validateEvidence(policy, negativeDelay, now), /deletion/);
});
