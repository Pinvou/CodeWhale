#!/usr/bin/env node

import { readFile } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const policyPath = path.join(repositoryRoot, 'account-platform', 'deploy', 'recovery-policy.json');
const sha256 = /^[a-f0-9]{64}$/;

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function timestamp(value, name) {
  const parsed = Date.parse(value);
  requireValue(Number.isFinite(parsed), `${name} must be an RFC3339 timestamp`);
  return parsed;
}

function ageSeconds(now, value, name) {
  return (now - timestamp(value, name)) / 1000;
}

export function validatePolicy(policy) {
  requireValue(policy.schema_version === 1, 'unsupported recovery policy schema');
  requireValue(policy.objectives?.rpo_seconds?.financial === 60, 'financial RPO must be 60 seconds');
  requireValue(policy.objectives?.rpo_seconds?.configuration === 300, 'configuration RPO must be 300 seconds');
  requireValue(policy.objectives?.rto_seconds?.core === 3600, 'core RTO must be 3600 seconds');
  requireValue(policy.objectives?.rto_seconds?.noncritical === 14400, 'noncritical RTO must be 14400 seconds');
  requireValue(policy.backups?.continuous_wal === true, 'continuous WAL must be required');
  requireValue(policy.backups?.incremental_interval_hours === 24, 'daily incremental backup must be required');
  requireValue(policy.backups?.full_interval_days === 7, 'weekly full backup must be required');
  requireValue(policy.backups?.pre_migration_snapshot === true, 'pre-migration snapshot must be required');
  requireValue(policy.backups?.transport_encryption_minimum === 'TLS1.2', 'backup transport must require TLS1.2+');
  requireValue(policy.backups?.at_rest_encryption_minimum === 'AES-256', 'backup storage must require AES-256 or equivalent');
  requireValue(policy.backups?.key_separation === true && policy.backups?.immutable_offsite_copy === true, 'backup keys and immutable offsite copy must be separated');
  requireValue(policy.exercises?.monthly_sample_days === 35, 'monthly exercise freshness must be 35 days');
  requireValue(policy.exercises?.quarterly_end_to_end_days === 100, 'quarterly exercise freshness must be 100 days');
  requireValue(policy.exercises?.annual_loss_days === 400, 'annual exercise freshness must be 400 days');
  const retention = new Map(policy.retention.map((entry) => [entry.classification, entry]));
  for (const classification of ['payment_evidence', 'ledger', 'audit', 'billing_usage']) {
    requireValue(retention.get(classification)?.retention_days === 1095, `${classification} retention must be three years`);
    requireValue(retention.get(classification)?.deletion_sla_days === 30, `${classification} deletion SLA must be 30 days`);
  }
  requireValue(retention.get('ordinary_runtime_logs')?.retention_days === 90, 'ordinary runtime logs must be retained for 90 days total');
  requireValue(retention.get('ordinary_runtime_logs')?.deletion_sla_days === 30, 'ordinary runtime log deletion SLA must be 30 days');
}

export function validateEvidence(policy, evidence, now = Date.now()) {
  requireValue(evidence.schema_version === 1 && evidence.environment === 'production-new', 'production recovery evidence is required');
  const generatedAt = timestamp(evidence.generated_at, 'generated_at');
  requireValue(Math.abs(now - generatedAt) <= 24 * 3600 * 1000, 'recovery evidence must be generated within 24 hours');
  const requiredDatabases = new Map([['billing', 60], ['token', 60], ['account', 300], ['keycloak', 300]]);
  const backups = new Map(evidence.backups.map((entry) => [entry.database, entry]));
  for (const [database, walLimit] of requiredDatabases) {
    const backup = backups.get(database);
    requireValue(backup, `backup evidence is missing for ${database}`);
    requireValue(ageSeconds(generatedAt, backup.last_wal_archived_at, `${database}.last_wal_archived_at`) <= walLimit, `${database} WAL archive exceeds RPO`);
    requireValue(ageSeconds(generatedAt, backup.last_incremental_at, `${database}.last_incremental_at`) <= 26 * 3600, `${database} daily incremental is stale`);
    requireValue(ageSeconds(generatedAt, backup.last_full_at, `${database}.last_full_at`) <= 8 * 86400, `${database} weekly full is stale`);
    requireValue(backup.transport_encryption === 'TLS1.2+' && backup.at_rest_encryption === 'AES-256', `${database} backup encryption evidence is invalid`);
    requireValue(backup.key_separated === true && backup.immutable_offsite === true && backup.production_identity_can_delete === false, `${database} backup separation or immutability evidence is invalid`);
    requireValue(typeof backup.backup_id === 'string' && backup.backup_id.length >= 8 && sha256.test(backup.checksum_sha256), `${database} backup identity/checksum is invalid`);
  }
  requireValue(evidence.measured?.financial_rpo_seconds <= policy.objectives.rpo_seconds.financial, 'measured financial RPO exceeds 60 seconds');
  requireValue(evidence.measured?.configuration_rpo_seconds <= policy.objectives.rpo_seconds.configuration, 'measured configuration RPO exceeds 300 seconds');
  requireValue(evidence.measured?.core_rto_seconds <= policy.objectives.rto_seconds.core, 'measured core RTO exceeds 60 minutes');
  requireValue(evidence.measured?.noncritical_rto_seconds <= policy.objectives.rto_seconds.noncritical, 'measured noncritical RTO exceeds 4 hours');
  const exerciseLimits = new Map([['monthly_sample', 35], ['quarterly_end_to_end', 100], ['annual_loss', 400]]);
  for (const [kind, limitDays] of exerciseLimits) {
    const exercise = evidence.exercises.filter((entry) => entry.kind === kind).sort((a, b) => timestamp(b.completed_at, `${kind}.completed_at`) - timestamp(a.completed_at, `${kind}.completed_at`))[0];
    requireValue(exercise, `${kind} exercise evidence is missing`);
    requireValue(ageSeconds(generatedAt, exercise.completed_at, `${kind}.completed_at`) <= limitDays * 86400, `${kind} exercise is stale`);
    requireValue(exercise.audit_sql_passed === true && exercise.real_provider_side_effects === 0, `${kind} exercise validation is incomplete`);
    requireValue(exercise.open_remediations === 0 || exercise.remediation_retest_due_within_days <= 30, `${kind} remediation has no 30-day retest`);
  }
  const scans = new Map(evidence.retention_deletion_scans.map((entry) => [entry.classification, entry]));
  for (const retention of policy.retention) {
    const scan = scans.get(retention.classification);
    requireValue(scan, `deletion scan is missing for ${retention.classification}`);
    requireValue(ageSeconds(generatedAt, scan.scanned_at, `${retention.classification}.scanned_at`) <= 35 * 86400, `${retention.classification} deletion scan is stale`);
    requireValue(scan.expired_eligible_count === scan.deleted_count, `${retention.classification} has expired eligible records not deleted`);
    requireValue(scan.maximum_deletion_delay_days <= retention.deletion_sla_days, `${retention.classification} deletion exceeded SLA`);
    requireValue(sha256.test(scan.receipt_sha256), `${retention.classification} deletion receipt is invalid`);
  }
}

async function main() {
  const policy = JSON.parse(await readFile(policyPath, 'utf8'));
  validatePolicy(policy);
  const evidencePath = process.argv[2];
  if (evidencePath) validateEvidence(policy, JSON.parse(await readFile(path.resolve(evidencePath), 'utf8')));
  console.log(evidencePath ? 'production recovery evidence passed' : 'recovery policy passed');
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(`[recovery-gate] ${error.message}`);
    process.exitCode = 1;
  });
}
