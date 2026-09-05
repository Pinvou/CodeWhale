const fingerprintPattern = /^[0-9a-f]{64}$/;
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const safeNoWrite = new Set([
  '400:INVALID_REQUEST', '400:INVALID_COMMAND', '400:INVALID_ROUTE_COMMAND', '400:INVALID_REASON', '400:IDEMPOTENCY_KEY_REQUIRED',
  '401:LOGIN_REQUIRED', '401:RECENT_AUTHENTICATION_REQUIRED',
  '403:CSRF_REJECTED', '403:PERMISSION_DENIED', '403:TOKEN_FORBIDDEN',
  '404:NOT_FOUND', '404:TOKEN_ACCOUNT_NOT_FOUND',
  '409:CATALOG_VERSION_CONFLICT', '409:COMMAND_PRECHECK_CHANGED', '409:TOKEN_ROUTE_VERSION_CONFLICT', '409:IDEMPOTENCY_KEY_REUSED',
  '412:DRAFT_ETAG_MISMATCH', '422:COMMAND_PRECHECK_FAILED', '422:TOKEN_ROUTE_REFERENCE_INVALID',
  '503:TOKEN_COMMAND_GOVERNANCE_INACTIVE', '503:IMPACT_PRECHECK_DISABLED',
]);
const precheckVersionCommands = new Set([
  'TOKEN_MODEL_DRAFT_SAVE', 'TOKEN_MODEL_PUBLISH', 'TOKEN_ACCOUNT_CREATE', 'TOKEN_ACCOUNT_STATUS_CHANGE',
  'TOKEN_ROUTE_UPSERT', 'TOKEN_ROUTE_DELETE', 'TOKEN_CREDENTIAL_VALIDATE',
  'TOKEN_ROUTE_PROVISIONING_REPAIR', 'TOKEN_USAGE_RECONCILIATION_RETRY',
]);

function stable(value) {
  if (Array.isArray(value)) return value.map(stable);
  if (!value || typeof value !== 'object') return value;
  return Object.fromEntries(Object.keys(value).sort().map((key) => [key, stable(value[key])]));
}

export function highRiskPayloadKey(value) {
  return JSON.stringify(stable(value));
}

function canonicalTokenCommand(commandType, command) {
  const normalized = commandType === 'TOKEN_MODEL_PUBLISH' && !command.publish_mode
    ? { ...command, publish_mode: 'IMMEDIATE' }
    : { ...command };
  if (commandType === 'TOKEN_MODEL_PUBLISH' && normalized.publish_mode === 'SCHEDULED') {
    const instant = Date.parse(String(normalized.effective_at || ''));
    if (Number.isFinite(instant)) normalized.effective_at = new Date(Math.floor(instant / 1000) * 1000).toISOString().replace('.000Z', 'Z');
  }
  return normalized;
}

async function sha256(value) {
  const bytes = new TextEncoder().encode(value);
  const digest = await globalThis.crypto.subtle.digest('SHA-256', bytes);
  return [...new Uint8Array(digest)].map((item) => item.toString(16).padStart(2, '0')).join('');
}

export async function governedTokenEnvelope({ commandType, targetResourceType, targetResourceID, reason, impact, expectedVersions = {}, command = {} }) {
  const normalizedCommand = canonicalTokenCommand(commandType, command);
  const frozenVersions = impact?.expected_versions && typeof impact.expected_versions === 'object' && !Array.isArray(impact.expected_versions) ? impact.expected_versions : expectedVersions;
  const requestFingerprint = await sha256(highRiskPayloadKey({ commandType, targetResourceType, targetResourceID, expectedVersions: frozenVersions, command: normalizedCommand }));
  const versionEvidence = { ...frozenVersions, request_fingerprint: requestFingerprint };
  if (precheckVersionCommands.has(commandType)) versionEvidence.precheck_fingerprint = impact.precheck_fingerprint;
  return {
    command_type: commandType,
    reason,
    target_resource_type: targetResourceType,
    target_resource_id: targetResourceID,
    expected_versions: versionEvidence,
    precheck_fingerprint: impact.precheck_fingerprint,
    command: normalizedCommand,
  };
}

export function validImpactPrecheck(value, expectedAction = '') {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  if (expectedAction && value.action !== expectedAction) return false;
  if (!fingerprintPattern.test(String(value.precheck_fingerprint || '')) || value.requires_cas_revalidation !== true) return false;
  if (value.snapshot_semantics !== 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS') return false;
  if (!Array.isArray(value.blockers) || value.blockers.some((item) => !/^[A-Z][A-Z0-9_]{0,119}$/.test(String(item)))) return false;
  const evidence = value.evidence;
  const version = evidence?.current_version;
  const dependencies = evidence?.dependencies;
  const expectedResourceType = expectedAction.startsWith('MODEL_LIFECYCLE_') ? 'MODEL'
    : expectedAction === 'TOKEN_MODEL_PUBLISH' ? 'MODEL'
    : expectedAction.startsWith('TOKEN_ACCOUNT_STATUS_') ? 'TOKEN_ACCOUNT'
				: expectedAction === 'MODEL_CATALOG_ROLLBACK' || expectedAction === 'TOKEN_MODEL_CATALOG_ROLLBACK' || expectedAction === 'TOKEN_MODEL_CATALOG_SYNC' ? 'MODEL_CATALOG'
        : expectedAction.startsWith('TOKEN_PROTOCOL_') ? 'PROTOCOL'
          : expectedAction.startsWith('TOKEN_FORMULA_') ? 'FORMULA'
            : expectedAction.startsWith('TOKEN_CREDENTIAL_') ? 'PROVIDER_CREDENTIAL'
            : expectedAction === 'TOKEN_ROUTE_PROVISIONING_REPAIR' ? 'PROVISIONING'
              : expectedAction === 'TOKEN_USAGE_RECONCILIATION_RETRY' ? 'USAGE_REQUEST'
                : expectedAction.startsWith('TOKEN_ROUTE_') ? 'TOKEN_ROUTE' : '';
  if (!evidence || !version || !dependencies || evidence.resource_type !== expectedResourceType || typeof evidence.resource_id !== 'string' || !evidence.resource_id || typeof evidence.current_status !== 'string') return false;
  if (!/^"impact-[0-9a-f]{64}"$/.test(String(version.evidence_etag || '')) || !fingerprintPattern.test(String(version.evidence_set_sha256 || ''))) return false;
  const counts = ['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'];
  if (counts.some((key) => !Number.isInteger(dependencies[key]) || dependencies[key] < 0)) return false;
  return !Number.isNaN(Date.parse(String(value.token_observed_at || '')));
}

export function requireGovernedTokenPrecheck(value, expected) {
  if (!validImpactPrecheck(value, expected.commandType)
    || value.evidence.resource_type !== expected.targetResourceType
    || value.evidence.resource_id !== expected.targetResourceID
    || !value.expected_versions || typeof value.expected_versions !== 'object' || Array.isArray(value.expected_versions)) {
    throw Object.assign(new Error('invalid governed command precheck'), { code: 'INVALID_IMPACT_RESPONSE' });
  }
  return value;
}

const approvalSnapshotFields = Object.freeze({
	TOKEN_MODEL_LIFECYCLE_CHANGE: ['model_id', 'catalog_version', 'current_lifecycle', 'target_lifecycle', 'lifecycle_plan_version', 'effective_at', 'existing_session_cutoff_at', 'replacement_model_id', 'billing_price_version', 'route_set_fingerprint', 'provisioning_set_fingerprint', 'inflight_request_set_fingerprint', 'policy_version'],
	TOKEN_MODEL_CATALOG_ROLLBACK: ['current_catalog_version', 'source_catalog_version', 'catalog_diff_fingerprint', 'billing_price_version', 'route_set_fingerprint', 'provisioning_set_fingerprint', 'inflight_request_set_fingerprint', 'policy_version'],
	TOKEN_CREDENTIAL_REVOKE: ['token_account_id', 'token_account_version', 'credential_id', 'credential_version', 'credential_status', 'credential_fingerprint', 'active_route_set_fingerprint', 'provisioning_lease_set_fingerprint', 'inflight_request_set_fingerprint', 'replacement_credential_version', 'policy_version'],
});
const financialSnapshotFields = Object.freeze(['billing_price_revision', 'price_set_fingerprint', 'models_checked', 'models_covered', 'fully_covered', 'billing_as_of']);

function hasExactFields(value, fields) {
	return value && typeof value === 'object' && !Array.isArray(value)
		&& Object.keys(value).length === fields.length && fields.every((field) => Object.hasOwn(value, field));
}

export function requireTokenApprovalImpact(value, expected) {
	if (!validImpactPrecheck(value, expected.action)) throw Object.assign(new Error('invalid approval impact'), { code: 'INVALID_IMPACT_RESPONSE' });
	const snapshot = value.critical_resource_versions;
	const financial = value.financial_snapshot;
	const fields = approvalSnapshotFields[expected.commandType] || [];
	const credentialRevoke = expected.commandType === 'TOKEN_CREDENTIAL_REVOKE';
	if (!hasExactFields(snapshot, fields) || (credentialRevoke ? !hasExactFields(financial, []) : !hasExactFields(financial, financialSnapshotFields))) throw Object.assign(new Error('incomplete approval snapshot'), { code: 'INVALID_IMPACT_RESPONSE' });
	const hashes = fields.filter((field) => field.endsWith('_fingerprint'));
	if (hashes.some((field) => !fingerprintPattern.test(String(snapshot[field] || '')))
		|| !Number.isInteger(snapshot.policy_version) || snapshot.policy_version !== (credentialRevoke ? 1 : 2)
		|| credentialRevoke && (!uuidPattern.test(String(snapshot.token_account_id || '').toLowerCase())
			|| !uuidPattern.test(String(snapshot.credential_id || '').toLowerCase()) || snapshot.credential_id !== expected.targetResourceID
			|| !Number.isInteger(snapshot.token_account_version) || snapshot.token_account_version < 1
			|| !Number.isInteger(snapshot.credential_version) || snapshot.credential_version < 1
			|| !['PENDING', 'CANDIDATE', 'ACTIVE', 'RETIRING', 'RETIRED', 'REVOKED'].includes(snapshot.credential_status)
			|| snapshot.replacement_credential_version != null && (!Number.isInteger(snapshot.replacement_credential_version) || snapshot.replacement_credential_version < 1))
		|| !credentialRevoke && (!Number.isInteger(snapshot.billing_price_version) || snapshot.billing_price_version < 1
		|| !Number.isInteger(financial.billing_price_revision) || financial.billing_price_revision !== snapshot.billing_price_version
		|| !fingerprintPattern.test(String(financial.price_set_fingerprint || ''))
		|| !Number.isInteger(financial.models_checked) || financial.models_checked < 1
		|| !Number.isInteger(financial.models_covered) || financial.models_covered < 0 || financial.models_covered > financial.models_checked
		|| financial.fully_covered !== true || financial.models_covered !== financial.models_checked
		|| Number.isNaN(Date.parse(String(financial.billing_as_of || ''))))) throw Object.assign(new Error('invalid approval snapshot'), { code: 'INVALID_IMPACT_RESPONSE' });
	if (expected.commandType === 'TOKEN_MODEL_LIFECYCLE_CHANGE') {
		if (!uuidPattern.test(String(snapshot.model_id || '').toLowerCase()) || snapshot.model_id !== expected.targetResourceID || snapshot.target_lifecycle !== expected.targetStatus
			|| !['DRAFT', 'PUBLISHED', 'DEPRECATED', 'REMOVED'].includes(snapshot.current_lifecycle)
			|| !Number.isInteger(snapshot.catalog_version) || snapshot.catalog_version < 1
			|| !Number.isInteger(snapshot.lifecycle_plan_version) || snapshot.lifecycle_plan_version < 0
			|| Number.isNaN(Date.parse(String(snapshot.effective_at || '')))
			|| snapshot.existing_session_cutoff_at != null && Number.isNaN(Date.parse(String(snapshot.existing_session_cutoff_at)))
			|| snapshot.replacement_model_id != null && !uuidPattern.test(String(snapshot.replacement_model_id).toLowerCase())) throw Object.assign(new Error('mismatched lifecycle snapshot'), { code: 'INVALID_IMPACT_RESPONSE' });
	} else if (expected.commandType === 'TOKEN_MODEL_CATALOG_ROLLBACK' && (!Number.isInteger(snapshot.current_catalog_version) || snapshot.current_catalog_version < 1
		|| !Number.isInteger(snapshot.source_catalog_version) || snapshot.source_catalog_version !== expected.sourceCatalogVersion
		|| !fingerprintPattern.test(String(snapshot.catalog_diff_fingerprint || '')))) {
		throw Object.assign(new Error('mismatched rollback snapshot'), { code: 'INVALID_IMPACT_RESPONSE' });
	}
	return value;
}

export function validGovernedTokenCommandResult(value, expected) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const allowed = new Set(['command_reference', 'command_type', 'policy_version', 'actor_user_id', 'actor_permission', 'authorization_version', 'idempotency_fingerprint', 'target_resource_type', 'target_resource_id', 'status', 'redacted_result', 'stable_error_code', 'approval_request_id', 'audit_request_id', 'created_at', 'completed_at']);
  if (Object.keys(value).some((key) => !allowed.has(key))) return false;
  if (!uuidPattern.test(String(value.command_reference || '').toLowerCase()) || !uuidPattern.test(String(value.actor_user_id || '').toLowerCase())) return false;
  if (value.command_type !== expected.commandType || value.target_resource_type !== expected.targetResourceType || value.target_resource_id !== expected.targetResourceID) return false;
  if (!Number.isInteger(value.policy_version) || value.policy_version < 1 || !Number.isInteger(value.authorization_version) || value.authorization_version < 1) return false;
  if (typeof value.actor_permission !== 'string' || !value.actor_permission || !fingerprintPattern.test(String(value.idempotency_fingerprint || ''))) return false;
  if (!['PENDING', 'SUCCEEDED', 'REJECTED', 'FAILED', 'UNKNOWN'].includes(value.status) || typeof value.audit_request_id !== 'string' || !value.audit_request_id || Number.isNaN(Date.parse(String(value.created_at || '')))) return false;
  if (value.approval_request_id != null && !uuidPattern.test(String(value.approval_request_id).toLowerCase())) return false;
  if (value.completed_at != null && Number.isNaN(Date.parse(String(value.completed_at)))) return false;
  return value.stable_error_code == null || /^[A-Z][A-Z0-9_]{0,119}$/.test(String(value.stable_error_code));
}

export function requireGovernedTokenCommandResult(value, expected) {
  if (!validGovernedTokenCommandResult(value, expected)) throw Object.assign(new Error('invalid Token command response'), { code: 'INVALID_COMMAND_RESPONSE' });
  if (['PENDING', 'UNKNOWN'].includes(value.status)) throw Object.assign(new Error('Token command outcome is not final'), { status: 202, code: 'COMMAND_RESULT_UNKNOWN', requestID: value.audit_request_id || '', commandReference: value.command_reference });
  if (value.status !== 'SUCCEEDED') throw Object.assign(new Error('Token command completed without success'), { status: 200, code: value.stable_error_code || 'COMMAND_REJECTED', requestID: value.audit_request_id || '', commandReference: value.command_reference, finalResult: true });
  return value;
}

export function validStableRouteWriteResult(value, expected) {
  const keys = ['route_id', 'token_account_id', 'model_id', 'protocol_version_id', 'protocol_code', 'priority', 'default_scope', 'health_status', 'health_diagnostic_code', 'health_observed_at', 'health_expires_at', 'route_version', 'route_generation', 'account_version', 'updated_at'];
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).length !== keys.length || keys.some((key) => !Object.hasOwn(value, key))) return false;
  if (![value.route_id, value.token_account_id, value.model_id, value.protocol_version_id].every((item) => uuidPattern.test(String(item || '').toLowerCase()))) return false;
  if (value.token_account_id !== expected.tokenAccountID || value.model_id !== expected.modelID || value.protocol_version_id !== expected.protocolVersionID) return false;
  if (typeof value.protocol_code !== 'string' || !value.protocol_code || !Number.isInteger(value.priority) || value.priority !== expected.priority || !['NONE', 'UNIT', 'ORG'].includes(value.default_scope) || value.default_scope !== expected.defaultScope) return false;
  if (!['UNKNOWN', 'PROBING', 'HEALTHY', 'DEGRADED', 'UNHEALTHY', 'STALE'].includes(value.health_status) || !/^[A-Z][A-Z0-9_]{0,63}$/.test(String(value.health_diagnostic_code || ''))) return false;
  if ((value.health_observed_at != null && Number.isNaN(Date.parse(String(value.health_observed_at))))
    || (value.health_expires_at != null && Number.isNaN(Date.parse(String(value.health_expires_at))))) return false;
  return Number.isInteger(value.route_version) && value.route_version >= 1 && Number.isInteger(value.route_generation) && value.route_generation >= 1 && value.account_version === expected.expectedAccountVersion + 1 && !Number.isNaN(Date.parse(String(value.updated_at || '')));
}

export function validApprovalRequest(value, expected, statuses) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  if (!uuidPattern.test(String(value.approval_request_id || '').toLowerCase()) || !uuidPattern.test(String(value.requester_user_id || '').toLowerCase())) return false;
  if (value.command_type !== expected.command_type || value.target_resource_type !== expected.target_resource_type || value.target_resource_id !== expected.target_resource_id || value.reason !== expected.reason) return false;
	const expectedPolicyVersion = expected.command_type === 'TOKEN_CREDENTIAL_REVOKE' ? 1 : 2;
	if (value.policy_version !== expectedPolicyVersion || value.approval_mode !== 'APPROVAL_REQUIRED' || !statuses.includes(value.status) || !Number.isInteger(value.version) || value.version < 1) return false;
  if (!fingerprintPattern.test(String(value.command_fingerprint || '')) || Number.isNaN(Date.parse(String(value.expires_at || ''))) || Number.isNaN(Date.parse(String(value.created_at || ''))) || Number.isNaN(Date.parse(String(value.updated_at || '')))) return false;
  if (!Array.isArray(value.allowed_actions) || value.allowed_actions.some((item) => !['EDIT', 'SUBMIT', 'WITHDRAW', 'APPROVE', 'REJECT'].includes(item))) return false;
  return highRiskPayloadKey(value.normalized_command) === highRiskPayloadKey(expected.normalized_command)
    && highRiskPayloadKey(value.critical_resource_versions) === highRiskPayloadKey(expected.critical_resource_versions)
    && highRiskPayloadKey(value.financial_snapshot) === highRiskPayloadKey(expected.financial_snapshot);
}

export function prepareHighRiskCommand(previous, payload, randomUUID = () => globalThis.crypto.randomUUID()) {
  const payloadKey = highRiskPayloadKey(payload);
  if (previous?.payloadKey === payloadKey) return previous;
  if (previous?.unknown || previous?.dispatched && !previous?.provenNoWrite) return { ...previous, locked: true, payloadMismatch: true, error: { status: 0, code: 'COMMAND_PAYLOAD_LOCKED', requestID: previous?.error?.requestID || '' } };
  return { idempotencyKey: `admin-token-action-${randomUUID()}`, payload, payloadKey, dispatched: false, provenNoWrite: false, unknown: false, locked: false };
}

export function classifyHighRiskFailure(command, error) {
  const status = Number.isInteger(error?.status) ? error.status : 0;
  const code = String(error?.code || 'REQUEST_FAILED');
  const finalResult = error?.finalResult === true;
  const provenNoWrite = status !== 504 && safeNoWrite.has(`${status}:${code}`);
  return {
    ...command,
    dispatched: true,
    provenNoWrite,
    unknown: !provenNoWrite && !finalResult,
    terminal: finalResult,
    locked: !provenNoWrite,
    error: { status, code: provenNoWrite || finalResult ? code : 'COMMAND_RESULT_UNKNOWN', requestID: String(error?.requestID || ''), commandReference: uuidPattern.test(String(error?.commandReference || error?.details?.command_reference || '').toLowerCase()) ? String(error?.commandReference || error?.details?.command_reference) : '' },
  };
}

export async function executeHighRiskCommand(holder, payload, send, randomUUID = () => globalThis.crypto.randomUUID(), { recovery = false } = {}) {
  if (holder.current?.inflight) return { duplicate: true };
  const prepared = prepareHighRiskCommand(holder.current?.command, payload, randomUUID);
  if (prepared.unknown && !recovery) return { ok: false, command: prepared, error: prepared.error, recoveryRequired: true };
  if (prepared.locked && (prepared.payloadMismatch || !prepared.unknown)) return { ok: false, command: prepared, error: prepared.error };
  const command = { ...prepared, dispatched: true };
  holder.current = { command, inflight: true };
  try {
    const value = await send(payload, command.idempotencyKey);
    holder.current = { command: { ...command, completed: true, locked: true }, inflight: false };
    return { ok: true, value, command: holder.current.command };
  } catch (error) {
    const recoveryError = recovery && error?.finalResult !== true ? Object.assign(new Error('command result lookup remains unknown'), {
      status: 202,
      code: 'COMMAND_RESULT_UNKNOWN',
      requestID: error?.requestID || '',
      commandReference: command.error?.commandReference || error?.commandReference || '',
    }) : error;
    const failed = classifyHighRiskFailure(command, recoveryError);
    holder.current = { command: failed, inflight: false };
    return { ok: false, command: failed, error: failed.error };
  }
}

export function resetHighRiskCommandAfterSafeFailure(holder) {
  if (holder.current?.command?.provenNoWrite) holder.current = { command: null, inflight: false };
}
