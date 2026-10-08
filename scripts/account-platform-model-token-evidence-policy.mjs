// APT-137 evidence coverage, not an alternate registry of executable domain handlers.
import { modelTokenGateNames } from './account-platform-model-token-release-plan.mjs';

export const evidenceDomain = 'PINVOU_MODEL_TOKEN_RELEASE_EVIDENCE_V1';
export const observationStages = Object.freeze(['INTERNAL', 'LIMITED', 'FULL']);
export const observationMetrics = Object.freeze(['unauthorized_effects', 'error_rate', 'latency_p95_ms', 'queue_depth', 'capacity_utilization', 'resource_recovery_failures']);
export const cursorSources = Object.freeze(['models', 'protocols', 'formulas', 'tokenAccounts', 'provisionings', 'usage', 'reconciliation', 'ownerCandidates', 'resourceAudit']);
export const evidenceCommandTypes = Object.freeze([
  'TOKEN_PROTOCOL_DRAFT_CREATE', 'TOKEN_PROTOCOL_PUBLISH', 'TOKEN_FORMULA_DRAFT_CREATE', 'TOKEN_FORMULA_PUBLISH',
  'TOKEN_MODEL_DRAFT_SAVE', 'TOKEN_MODEL_PUBLISH', 'TOKEN_MODEL_LIFECYCLE_CHANGE', 'TOKEN_MODEL_CATALOG_SYNC', 'TOKEN_MODEL_CATALOG_ROLLBACK',
  'TOKEN_ACCOUNT_CREATE', 'TOKEN_ACCOUNT_STATUS_CHANGE', 'TOKEN_ROUTE_UPSERT', 'TOKEN_ROUTE_DELETE',
  'TOKEN_CREDENTIAL_ENROLL', 'TOKEN_CREDENTIAL_ROTATE', 'TOKEN_CREDENTIAL_REVOKE', 'TOKEN_CREDENTIAL_VALIDATE',
  'TOKEN_ROUTE_PROVISIONING_REPAIR', 'TOKEN_USAGE_RECONCILIATION_RETRY',
]);
const cases = {
  GATE_MATRIX: [...modelTokenGateNames.flatMap(name => [`${name}.on`, `${name}.off`]), 'dependencies.reject', 'authorization.cancel', 'parent.readonly', 'unknown.get_only', 'direct.no_fallback'],
  CURSOR_ISOLATION: cursorSources.flatMap(name => ['source_binding', 'actor_binding', 'filter_binding', 'expired_410', 'gate_off_410'].map(boundary => `${name}.${boundary}`)),
  LEGACY_LINKS: ['legacy.models', 'legacy.resources', 'legacy.operations', 'legacy.clear_private_state', 'legacy.permission_fallback', 'legacy.history_restore', 'legacy.retention'],
  AUTHORIZATION: ['pages.permissions', 'actions.permissions', 'recent_auth', 'workload.isolation', 'revocation.zero_effects'],
  THREE_SERVICE_COMMANDS: [...evidenceCommandTypes.map(type => `command.${type}`), 'approval.lifecycle', 'approval.catalog_rollback', 'approval.credential_revoke', 'billing.authority', 'replay.single_effect', 'unknown.get_only'],
  WORKER_READINESS_SAFE: ['lifecycle.registered', 'account.registered', 'account.not_ready_closed', 'lease.unique', 'safe.stop', 'old_epoch.zero_effects'],
  NATS_CONSUMERS: ['cluster.authenticated_same', 'durable.lifecycle', 'durable.catalog_rollback', 'durable.credential_revoke', 'delivery.first', 'delivery.replay', 'unknown.retained', 'safe.stop', 'billing.unchanged'],
  GOVERNANCE_POLICY_HANDLER: ['database.policy_22', 'installed.handler_19', 'migration.31', 'cas.state_epoch', 'request_id.read_only'],
  WRITER_FENCE: ['binary.n', 'binary.n_minus_one', 'role.gateway', 'role.credential', 'role.relay', 'retired.nologin', 'old_sessions.zero', 'cross_database.isolation'],
  ACCOUNT_POLICY_LOCK: ['identity.non_superuser', 'share.acquired', 'share.nowait_conflict', 'share.held_until_token_cas'],
  CAP_BOOTSTRAP: ['approved.explicit_cap', 'bootstrap.real', 'replay.single_revision', 'existing.grandfather', 'headroom', 'readiness'],
  DEADLINES_CAPACITY: ['budget.10_12_14', 'budget.3_5_7', 'budget.5_7_9', 'capacity.70_percent', 'capacity.85_percent', 'timeout.resource_recovery', 'recovery.alert'],
  DUAL_ORIGIN_BROWSER: ['dns.real_pair', 'tls.real_pair', 'oidc.dedicated', 'handoff.fixed_no_id', 'handoff.user_gesture', 'handoff.exact_csp', 'handoff.no_service_worker', 'handoff.message_channel', 'handoff.self_body_post', 'handoff.origin', 'handoff.303_headers_opener', 'handoff.no_leak', 'handoff.failure_matrix', 'handoff.unknown_refresh_cross_browser'],
  PROXY_LOG_SAFETY: ['logs.all_layers_canary_zero', 'headers.trusted', 'paths.exact', 'errors.safe', 'upstream.secure', 'kms.legacy_compatible'],
  WORKLOAD_ROTATION: ['issuer.actual', 'lifetime.15_minutes', 'rotation.hot', 'expired.reject', 'audience.reject', 'subject.reject', 'permission.reject'],
  SAFE_ROLLBACK: ['cas.linearized', 'old_epoch.zero_effects', 'cursor.410', 'session.invalidated', 'history.read_only', 'recovery.new_identity'],
  OBSERVATION: ['stage.cohort', 'window.measured', 'samples.measured', 'metrics.measured', 'pause.alert', 'recovery.alert'],
};
export const requiredEvidenceCases = Object.freeze(Object.fromEntries(Object.entries(cases).map(([kind, ids]) => [kind, Object.freeze(ids)])));
export const evidenceKinds = Object.freeze(Object.keys(requiredEvidenceCases));

export function evidenceRejected(code) { throw new Error(`model_token_evidence_${code}`); }

// All claims are closed-schema, finite, typed values before canonicalization.
export function canonicalEvidenceJSON(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalEvidenceJSON).join(',')}]`;
  if (value !== null && typeof value === 'object') return `{${Object.keys(value).sort().map(key => `${JSON.stringify(key)}:${canonicalEvidenceJSON(value[key])}`).join(',')}}`;
  return JSON.stringify(value);
}

export function evidenceSignedBytes(entry, planSHA256) {
  return Buffer.from(canonicalEvidenceJSON({ domain: evidenceDomain, plan_sha256: planSHA256, source: entry.source, key_id: entry.key_id, envelope: entry.envelope }));
}
