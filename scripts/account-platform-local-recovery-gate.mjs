// Explicitly selected local-host exception; never substitutes for the default policy.
const sha256 = /^[a-f0-9]{64}$/;
const identifier = /^[a-zA-Z0-9][a-zA-Z0-9_-]{7,127}$/;
const databases = ['account', 'billing', 'token', 'keycloak', 'newapi'];

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function finite(value, maximum, name) {
  requireValue(typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= maximum, `${name} is invalid or exceeds limit`);
}

function timestamp(value, name) {
  requireValue(typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z$/.test(value), `${name} must be a UTC timestamp`);
  const parsed = Date.parse(value);
  requireValue(Number.isFinite(parsed), `${name} timestamp is invalid`);
  // Date.parse normalizes impossible dates (e.g. February 30); reject those.
  requireValue(new Date(parsed).toISOString().slice(0, 19) === value.slice(0, 19), `${name} timestamp is invalid`);
  return parsed;
}

function fresh(value, anchor, maximumSeconds, name) {
  const parsed = timestamp(value, name);
  finite((anchor - parsed) / 1000, maximumSeconds, `${name} age`);
  return parsed;
}

function exactDatabases(value, name) {
  requireValue(Array.isArray(value) && value.length === databases.length && new Set(value).size === databases.length && databases.every((db) => value.includes(db)), `${name} must contain exactly five databases`);
}

function digest(value, name) {
  requireValue(typeof value === 'string' && sha256.test(value), `${name} digest is invalid`);
}

function records(value, names, key) {
  requireValue(Array.isArray(value) && value.length === names.length && new Set(value.map((item) => item?.[key])).size === names.length, `${key} records are missing or duplicated`);
  const result = new Map(value.map((item) => [item[key], item]));
  requireValue(names.every((name) => result.has(name)), `${key} records are incomplete`);
  return result;
}

// FORMAT_ONLY: returned status describes claims, not independently verified restoration.
export function validateLocalEvidence(policy, evidence, { deploymentId, bootstrap = false, now = Date.now() } = {}) {
  requireValue(typeof deploymentId === 'string' && identifier.test(deploymentId), 'expected deployment ID is required');
  requireValue(evidence?.schema_version === 1 && evidence.environment === 'production-new' && evidence.profile === 'local-only' && evidence.deployment_id === deploymentId, 'local evidence does not match expected production deployment');
  const generated = fresh(evidence.generated_at, now, 86400, 'generated_at');
  const acceptance = evidence.risk_acceptance;
  requireValue(acceptance?.deployment_id === deploymentId && typeof acceptance.owner === 'string' && acceptance.owner.trim().length > 0 && acceptance.owner.length <= 128 && acceptance.same_host_loss_accepted === true, 'explicit deployment-bound local risk acceptance is required');
  fresh(acceptance.accepted_at, generated, Number.MAX_SAFE_INTEGER, 'accepted_at');
  digest(acceptance.approval_receipt_sha256, 'risk acceptance');
  const storage = evidence.storage;
  requireValue(storage?.offsite === false && storage.immutable === false && storage.host_loss_recoverable === false, 'local mode must not claim offsite, immutable, or host-loss recovery');
  requireValue(storage.at_rest_encryption === 'AES-256' && ['LOCAL_ONLY', 'TLS1.2+'].includes(storage.transport_encryption), 'local backup encryption is invalid');
  requireValue(storage.key_access_separated === true && storage.application_identity_can_access === false && storage.host_privileged_identity_can_delete === true, 'local access boundary is invalid');
  const cluster = evidence.cluster;
  requireValue(typeof cluster?.system_identifier === 'string' && /^\d{16,20}$/.test(cluster.system_identifier), 'cluster identity is invalid');
  exactDatabases(cluster.databases, 'cluster');
  requireValue(typeof cluster.backup_id === 'string' && identifier.test(cluster.backup_id), 'backup identity is invalid');
  digest(cluster.checksum_sha256, 'backup');
  fresh(cluster.last_wal_archived_at, generated, policy.objectives.rpo_seconds.financial, 'WAL');
  fresh(cluster.last_daily_backup_at, generated, 26 * 3600, 'daily backup');
  fresh(cluster.last_full_at, generated, 8 * 86400, 'full backup');
  const automation = evidence.automation;
  for (const field of ['continuous_wal', 'daily_incremental', 'weekly_full', 'pre_migration_backup', 'archive_check']) {
    requireValue(automation?.[field] === true, `${field} must remain enabled`);
  }
  requireValue(automation.expire_auto === false && automation.drop_wal_on_queue_full === false && automation.daily_retention_days === 35 && automation.monthly_retention_months === 13, 'local retention or WAL protection is invalid');
  const monitor = evidence.monitor;
  fresh(monitor?.checked_at, generated, 600, 'monitor');
  requireValue(monitor.archive_ok === true && monitor.scheduler_ok === true && monitor.notification_delivery === 'local_status_only', 'local backup monitoring is incomplete');
  finite(monitor.free_fraction, 1, 'free fraction');
  finite(monitor.inode_free_fraction, 1, 'free inode fraction');
  finite(monitor.free_bytes, Number.MAX_SAFE_INTEGER, 'free bytes');
  requireValue(monitor.free_fraction >= 0.10 && monitor.free_bytes >= 2 * 1024 ** 3 && monitor.inode_free_fraction >= 0.10, 'critical local backup capacity');
  const warning = monitor.free_fraction < 0.20 || monitor.free_bytes < 5 * 1024 ** 3;
  requireValue(monitor.capacity_warning === warning, 'capacity warning must match measurements');
  const restore = evidence.restore;
  fresh(restore?.completed_at, generated, 86400, 'restore');
  requireValue(restore.backup_id === cluster.backup_id && restore.system_identifier === cluster.system_identifier, 'restore does not match source backup');
  exactDatabases(restore.databases, 'restored cluster');
  for (const field of ['isolated', 'source_untouched', 'roles_verified', 'wal_replay_verified']) {
    requireValue(restore[field] === true, `${field} restoration proof is required`);
  }
  requireValue(restore.real_provider_side_effects === 0, 'restore must not cause real Provider side effects');
  digest(restore.receipt_sha256, 'restore');
  finite(restore.elapsed_seconds, policy.objectives.rto_seconds.core, 'infrastructure restore elapsed');
  finite(evidence.measured?.financial_rpo_seconds, policy.objectives.rpo_seconds.financial, 'financial RPO');
  finite(evidence.measured?.configuration_rpo_seconds, policy.objectives.rpo_seconds.configuration, 'configuration RPO');
  if (bootstrap) {
    requireValue(evidence.status === 'LOCAL_BOOTSTRAP_VERIFIED', 'bootstrap mode requires its distinct status');
    return 'LOCAL_BOOTSTRAP_VERIFIED';
  }
  requireValue(evidence.status === 'LOCAL_ONLY_RECOVERABLE', 'complete local business recovery is required for cutover');
  finite(evidence.measured.core_rto_seconds, policy.objectives.rto_seconds.core, 'core RTO');
  finite(evidence.measured.noncritical_rto_seconds, policy.objectives.rto_seconds.noncritical, 'noncritical RTO');
  const recoverySet = evidence.recovery_set;
  requireValue(recoverySet?.deployment_id === deploymentId && recoverySet.postgres_backup_id === cluster.backup_id, 'recovery set binding is invalid');
  digest(recoverySet.manifest_sha256, 'recovery set');
  const components = records(recoverySet.components, ['postgres', 'nats', 'newapi_files', 'site_configuration', 'edge_configuration', 'release_artifacts', 'kms_references'], 'component');
  for (const component of components.values()) {
    requireValue(component.restored_and_verified === true, 'recovery component is not verified');
    digest(component.receipt_sha256, 'component');
  }
  requireValue(recoverySet.redis_sessions_invalidated === true && recoverySet.domain_audit_sql_passed === true && recoverySet.cross_domain_reconciled === true && recoverySet.real_provider_side_effects === 0, 'full local domain recovery is incomplete');
  const exercises = records(evidence.exercises, ['monthly_sample', 'quarterly_end_to_end'], 'kind');
  for (const [kind, days] of [['monthly_sample', 35], ['quarterly_end_to_end', 100]]) {
    const exercise = exercises.get(kind);
    fresh(exercise.completed_at, generated, days * 86400, kind);
    requireValue(exercise.audit_sql_passed === true && exercise.real_provider_side_effects === 0 && exercise.open_remediations === 0, `${kind} validation is incomplete`);
    digest(exercise.receipt_sha256, kind);
  }
  const scans = records(evidence.retention_deletion_scans, policy.retention.map(({ classification }) => classification), 'classification');
  for (const retention of policy.retention) {
    const scan = scans.get(retention.classification);
    fresh(scan.scanned_at, generated, 35 * 86400, 'retention scan');
    requireValue(Number.isSafeInteger(scan.expired_eligible_count) && scan.expired_eligible_count >= 0 && scan.expired_eligible_count === scan.deleted_count, 'retention deletion counts are invalid');
    finite(scan.maximum_deletion_delay_days, retention.deletion_sla_days, 'retention deletion delay');
    digest(scan.receipt_sha256, 'retention');
  }
  return 'LOCAL_ONLY_RECOVERABLE';
}
