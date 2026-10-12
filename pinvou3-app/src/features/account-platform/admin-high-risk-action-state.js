import { secureRequestID } from './request-id.js';
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const fingerprintPattern = /^[0-9a-f]{64}$/;
function wireString(value) {
  // Go's JSON decoder replaces isolated UTF-16 surrogates before encoding.
  return [...value].map(char => {
    const point = char.codePointAt(0);
    return point >= 0xd800 && point <= 0xdfff ? '\ufffd' : char;
  }).join('');
}
function encodedValue(value) {
  return JSON.stringify(typeof value === 'string' ? wireString(value) : value)
    ?.replace(/[<>&\u2028\u2029]/g, char => `\\u${char.charCodeAt(0).toString(16).padStart(4, '0')}`);
}
function wireKeyOrder(left, right) {
  const a = new TextEncoder().encode(left), b = new TextEncoder().encode(right);
  for (let index = 0; index < Math.min(a.length, b.length); index += 1) {
    if (a[index] !== b[index]) return a[index] - b[index];
  }
  return a.length - b.length;
}
export function highRiskPayloadKey(value) {
  if (Array.isArray(value)) return `[${Array.from(value, item => highRiskPayloadKey(item) ?? 'null').join(',')}]`;
  if (!value || typeof value !== 'object') return encodedValue(value);
  // Encode directly: rebuilding an object would reorder integer-like keys.
  // Match the existing Go JSON contract without changing persisted fingerprints.
  const entries = new Map();
  for (const key of Object.keys(value)) {
    const encoded = highRiskPayloadKey(value[key]);
    if (encoded !== undefined) entries.set(wireString(key), encoded);
  }
  return `{${[...entries.keys()].sort(wireKeyOrder).map(key => `${encodedValue(key)}:${entries.get(key)}`).join(',')}}`;
}
export function validApprovalRequest(value, expected, statuses) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  if (!uuidPattern.test(String(value.approval_request_id || '').toLowerCase()) || !uuidPattern.test(String(value.requester_user_id || '').toLowerCase())) return false;
  if (value.command_type !== expected.command_type || value.target_resource_type !== expected.target_resource_type || value.target_resource_id !== expected.target_resource_id || value.reason !== expected.reason) return false;
	const expectedPolicyVersion = ['PAYMENT_PROVIDER_RESUME', 'TOKEN_MODEL_LIFECYCLE_CHANGE', 'TOKEN_MODEL_CATALOG_ROLLBACK'].includes(expected.command_type) ? 2 : 1;
  if (!['WALLET_ADJUSTMENT', 'PAYMENT_PROVIDER_RESUME', 'PAYMENT_PRODUCT_RESUME', 'TOKEN_MODEL_LIFECYCLE_CHANGE', 'TOKEN_MODEL_CATALOG_ROLLBACK', 'TOKEN_CREDENTIAL_REVOKE'].includes(expected.command_type)) return false;
	if (value.policy_version !== expectedPolicyVersion || value.approval_mode !== 'APPROVAL_REQUIRED' || !statuses.includes(value.status) || !Number.isInteger(value.version) || value.version < 1) return false;
  if (!fingerprintPattern.test(String(value.command_fingerprint || '')) || Number.isNaN(Date.parse(String(value.expires_at || ''))) || Number.isNaN(Date.parse(String(value.created_at || ''))) || Number.isNaN(Date.parse(String(value.updated_at || '')))) return false;
  if (!Array.isArray(value.allowed_actions) || value.allowed_actions.some((item) => !['EDIT', 'SUBMIT', 'WITHDRAW', 'APPROVE', 'REJECT'].includes(item))) return false;
  return highRiskPayloadKey(value.normalized_command) === highRiskPayloadKey(expected.normalized_command)
    && highRiskPayloadKey(value.critical_resource_versions) === highRiskPayloadKey(expected.critical_resource_versions)
    && highRiskPayloadKey(value.financial_snapshot) === highRiskPayloadKey(expected.financial_snapshot);
}


const rfc3339Pattern = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|[+-](\d{2}):(\d{2}))$/;

const safeNoWrite = new Set([
  '400:INVALID_REQUEST', '400:INVALID_COMMAND', '400:INVALID_ROUTE_COMMAND', '400:INVALID_REASON', '400:IDEMPOTENCY_KEY_REQUIRED',
  '401:LOGIN_REQUIRED', '401:RECENT_AUTHENTICATION_REQUIRED',
  '403:CSRF_REJECTED', '403:PERMISSION_DENIED', '403:TOKEN_FORBIDDEN',
  '404:NOT_FOUND', '404:TOKEN_ACCOUNT_NOT_FOUND', '404:TOKEN_NOT_FOUND',
  '409:CATALOG_VERSION_CONFLICT', '409:COMMAND_PRECHECK_CHANGED', '409:TOKEN_ROUTE_VERSION_CONFLICT', '409:IDEMPOTENCY_KEY_REUSED',
  '409:TOKEN_ACCOUNT_CAP_REACHED', '503:TOKEN_ACCOUNT_POLICY_UNAVAILABLE', '503:TOKEN_ACCOUNT_POLICY_CONFIG_MISMATCH',
  '412:DRAFT_ETAG_MISMATCH', '422:COMMAND_PRECHECK_FAILED', '422:TOKEN_ROUTE_REFERENCE_INVALID',
  '503:TOKEN_COMMAND_GOVERNANCE_INACTIVE', '503:IMPACT_PRECHECK_DISABLED',
  '503:CREDENTIAL_REQUEST_DEADLINE_EXCEEDED',
]);

const protocolFormulaSafeNoWrite = new Set([
  '422:FORMULA_UNIT_VALIDATION_REJECTED',
  '503:COMMAND_RETRYABLE',
]);

const protocolFormulaCommands = new Set([
  'TOKEN_PROTOCOL_DRAFT_CREATE', 'TOKEN_PROTOCOL_PUBLISH',
  'TOKEN_FORMULA_DRAFT_CREATE', 'TOKEN_FORMULA_PUBLISH',
]);

const tokenAccountCreateSafeNoWrite = new Set([
  '503:TOKEN_ACCOUNT_CREATE_NOT_ACTIVE',
]);

const precheckVersionCommands = new Set([
  'TOKEN_MODEL_DRAFT_SAVE', 'TOKEN_MODEL_PUBLISH', 'TOKEN_ACCOUNT_CREATE', 'TOKEN_ACCOUNT_STATUS_CHANGE',
  'TOKEN_ROUTE_UPSERT', 'TOKEN_ROUTE_DELETE', 'TOKEN_CREDENTIAL_VALIDATE',
  'TOKEN_ROUTE_PROVISIONING_REPAIR', 'TOKEN_USAGE_RECONCILIATION_RETRY',
]);

function validDateTime(value) {
  if (typeof value !== 'string') return false;
  const match = value.match(rfc3339Pattern);
  if (!match) return false;
  const [, yearText, monthText, dayText, hourText, minuteText, secondText, zone, offsetHourText = '0', offsetMinuteText = '0'] = match;
  const [year, month, day, hour, minute, second, offsetHour, offsetMinute] = [yearText, monthText, dayText, hourText, minuteText, secondText, offsetHourText, offsetMinuteText].map(Number);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const monthDays = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return month >= 1 && month <= 12 && day >= 1 && day <= monthDays[month - 1]
    && hour <= 23 && minute <= 59 && second <= 59
    && (zone === 'Z' || (offsetHour <= 23 && offsetMinute <= 59))
    && Number.isFinite(Date.parse(value));
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
		: ['TOKEN_MODEL_DRAFT_SAVE', 'TOKEN_MODEL_PUBLISH'].includes(expectedAction) ? 'MODEL'
		: expectedAction === 'TOKEN_ACCOUNT_CREATE' ? 'TOKEN_ACCOUNT'
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
		&& Object.keys(value).length === fields.length && fields.every((field) => Object.prototype.hasOwnProperty.call(value, field));
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
  const allowed = new Set(['command_reference', 'command_type', 'policy_version', 'actor_user_id', 'actor_permission', 'authorization_version', 'idempotency_fingerprint', 'target_resource_type', 'target_resource_id', 'status', 'redacted_result', 'stable_error_code', 'approval_request_id', 'audit_request_id', 'created_at', 'completed_at', 'recovery_state', 'recovery_diagnostic_code', 'recovery_epoch']);
  if (Object.keys(value).some((key) => !allowed.has(key))) return false;
  if (!uuidPattern.test(String(value.command_reference || '').toLowerCase()) || !uuidPattern.test(String(value.actor_user_id || '').toLowerCase())) return false;
  if (value.command_type !== expected.commandType || value.target_resource_type !== expected.targetResourceType || value.target_resource_id !== expected.targetResourceID) return false;
  if (!Number.isInteger(value.policy_version) || value.policy_version < 1 || !Number.isInteger(value.authorization_version) || value.authorization_version < 1) return false;
  if (typeof value.actor_permission !== 'string' || !value.actor_permission || !fingerprintPattern.test(String(value.idempotency_fingerprint || ''))) return false;
  if (!['PENDING', 'SUCCEEDED', 'REJECTED', 'FAILED', 'UNKNOWN'].includes(value.status) || typeof value.audit_request_id !== 'string' || !value.audit_request_id || !validDateTime(value.created_at)) return false;
  if (value.approval_request_id != null && !uuidPattern.test(String(value.approval_request_id).toLowerCase())) return false;
  const completed = value.completed_at != null && validDateTime(value.completed_at);
  const stableError = value.stable_error_code != null
    && /^[A-Z][A-Z0-9_]{0,119}$/.test(String(value.stable_error_code))
    && (value.stable_error_code === 'CREDENTIAL_PROBE_OUTCOME_UNKNOWN'
      || !/(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)/.test(String(value.stable_error_code)));
  if (value.completed_at != null && !completed) return false;
  if (value.stable_error_code != null && !stableError) return false;
  const recoveryFields = ['recovery_state', 'recovery_diagnostic_code', 'recovery_epoch'].some((field) => Object.prototype.hasOwnProperty.call(value, field));
  if (value.command_type === 'TOKEN_ACCOUNT_CREATE' && value.status === 'PENDING') {
    if (!['PENDING', 'MANUAL_REVIEW'].includes(value.recovery_state) || !Number.isInteger(value.recovery_epoch) || value.recovery_epoch < 1) return false;
    if (value.recovery_state === 'MANUAL_REVIEW' ? value.recovery_diagnostic_code !== 'ACCOUNT_PROVISIONING_MANUAL_REVIEW' : value.recovery_diagnostic_code != null) return false;
  } else if (recoveryFields) return false;
  if (value.status === 'PENDING') return !completed && value.stable_error_code == null;
  if (value.status === 'SUCCEEDED') return completed && value.stable_error_code == null;
  return completed && stableError;
}

export function requireGovernedTokenCommandResult(value, expected) {
  if (!validGovernedTokenCommandResult(value, expected)) throw Object.assign(new Error('invalid Token command response'), {
    code: 'INVALID_COMMAND_RESPONSE',
    commandReference: uuidPattern.test(String(value?.command_reference || '').toLowerCase()) ? String(value.command_reference) : '',
  });
  if (['PENDING', 'UNKNOWN'].includes(value.status)) throw Object.assign(new Error('Token command outcome is not final'), { status: 202, code: 'COMMAND_RESULT_UNKNOWN', requestID: value.audit_request_id || '', commandReference: value.command_reference, commandResult: value });
  if (value.status !== 'SUCCEEDED') throw Object.assign(new Error('Token command completed without success'), { status: 200, code: value.stable_error_code || 'COMMAND_REJECTED', requestID: value.audit_request_id || '', commandReference: value.command_reference, finalResult: true });
  return value;
}

export function validTokenAccountRecoveryActionResult(value, expected = {}) {
  const keys = ['action_id', 'command_reference', 'command_type', 'action', 'status', 'recovery_state', 'recovery_epoch', 'stable_error_code', 'audit_reference', 'created_at', 'completed_at'];
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).length !== keys.length || keys.some((key) => !Object.prototype.hasOwnProperty.call(value, key))) return false;
  if (![value.action_id, value.command_reference, value.audit_reference].every((item) => uuidPattern.test(String(item || '').toLowerCase())) || value.command_type !== 'TOKEN_ACCOUNT_CREATE') return false;
  if ((expected.commandReference && value.command_reference !== expected.commandReference)
    || (expected.action && value.action !== expected.action)) return false;
  if (!['REQUEUE', 'CANCEL'].includes(value.action) || !['PENDING', 'SUCCEEDED', 'FAILED'].includes(value.status) || !validDateTime(value.created_at)) return false;
  const completed = value.completed_at != null && validDateTime(value.completed_at);
  const stableError = value.stable_error_code != null && /^[A-Z][A-Z0-9_]{0,119}$/.test(String(value.stable_error_code)) && !/(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)/.test(String(value.stable_error_code));
  if (value.status === 'PENDING') return value.recovery_state === 'MANUAL_REVIEW' && value.recovery_epoch == null && value.stable_error_code == null && value.completed_at == null;
  if (value.status === 'FAILED') return value.recovery_state === 'MANUAL_REVIEW' && value.recovery_epoch == null && stableError && completed;
  return completed && value.stable_error_code == null && Number.isInteger(value.recovery_epoch) && value.recovery_epoch >= 1
    && (value.action === 'REQUEUE' ? value.recovery_state === 'PENDING' : value.recovery_state === 'FAILED');
}

export function validStableRouteWriteResult(value, expected) {
  const keys = ['route_id', 'token_account_id', 'model_id', 'protocol_version_id', 'protocol_code', 'priority', 'default_scope', 'health_status', 'health_diagnostic_code', 'health_observed_at', 'health_expires_at', 'route_version', 'route_generation', 'account_version', 'updated_at'];
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).length !== keys.length || keys.some((key) => !Object.prototype.hasOwnProperty.call(value, key))) return false;
  if (![value.route_id, value.token_account_id, value.model_id, value.protocol_version_id].every((item) => uuidPattern.test(String(item || '').toLowerCase()))) return false;
  if (value.token_account_id !== expected.tokenAccountID || value.model_id !== expected.modelID || value.protocol_version_id !== expected.protocolVersionID) return false;
  if (typeof value.protocol_code !== 'string' || !value.protocol_code || !Number.isInteger(value.priority) || value.priority !== expected.priority || !['NONE', 'UNIT', 'ORG'].includes(value.default_scope) || value.default_scope !== expected.defaultScope) return false;
  if (!['UNKNOWN', 'PROBING', 'HEALTHY', 'DEGRADED', 'UNHEALTHY', 'STALE'].includes(value.health_status) || !/^[A-Z][A-Z0-9_]{0,63}$/.test(String(value.health_diagnostic_code || ''))) return false;
  if ((value.health_observed_at != null && Number.isNaN(Date.parse(String(value.health_observed_at))))
    || (value.health_expires_at != null && Number.isNaN(Date.parse(String(value.health_expires_at))))) return false;
  return Number.isInteger(value.route_version) && value.route_version >= 1 && Number.isInteger(value.route_generation) && value.route_generation >= 1 && value.account_version === expected.expectedAccountVersion + 1 && !Number.isNaN(Date.parse(String(value.updated_at || '')));
}

export function requireGovernedRouteCommandResult(value, expected) {
  const result = requireGovernedTokenCommandResult(value, expected);
  const route = result.redacted_result;
  let valid = false;
  if (expected.commandType === 'TOKEN_ROUTE_UPSERT') {
    valid = validStableRouteWriteResult(route, expected) && route.route_version === expected.expectedRouteVersion + 1
      && validDateTime(route.updated_at) && (route.health_observed_at == null || validDateTime(route.health_observed_at))
      && (route.health_expires_at == null || validDateTime(route.health_expires_at));
  } else if (expected.commandType === 'TOKEN_ROUTE_DELETE') {
    const keys = ['route_id', 'token_account_id', 'model_id', 'protocol_version_id', 'deleted', 'deleted_route_version', 'route_version', 'route_generation', 'account_version', 'deleted_at'];
    valid = route && typeof route === 'object' && !Array.isArray(route) && Object.keys(route).length === keys.length && keys.every((key) => Object.prototype.hasOwnProperty.call(route, key))
      && route.route_id === expected.targetResourceID && route.token_account_id === expected.tokenAccountID
      && route.model_id === expected.modelID && route.protocol_version_id === expected.protocolVersionID
      && [route.route_id, route.token_account_id, route.model_id, route.protocol_version_id].every((id) => uuidPattern.test(String(id)))
      && route.deleted === true && route.deleted_route_version === expected.expectedRouteVersion
      && route.route_version === expected.expectedRouteVersion + 1 && route.account_version === expected.expectedAccountVersion + 1
      && Number.isInteger(route.route_generation) && route.route_generation >= 1 && validDateTime(route.deleted_at);
  }
  if (!valid) throw Object.assign(new Error('invalid governed route result'), { code: 'INVALID_COMMAND_RESPONSE', commandReference: result.command_reference });
  return result;
}

export function prepareHighRiskCommand(previous, payload, randomUUID = () => secureRequestID()) {
  const payloadKey = highRiskPayloadKey(payload);
  if (previous?.payloadKey === payloadKey) return previous;
  if (previous?.unknown || previous?.dispatched && !previous?.provenNoWrite) return { ...previous, locked: true, payloadMismatch: true, error: { status: 0, code: 'COMMAND_PAYLOAD_LOCKED', requestID: previous?.error?.requestID || '' } };
  return { idempotencyKey: `admin-token-action-${randomUUID()}`, payload, payloadKey, dispatched: false, provenNoWrite: false, unknown: false, locked: false };
}

export function classifyHighRiskFailure(command, error) {
  const status = Number.isInteger(error?.status) ? error.status : 0;
  const code = String(error?.code || 'REQUEST_FAILED');
  const finalResult = error?.finalResult === true;
  const pair = `${status}:${code}`;
  const commandScopedNoWrite = protocolFormulaCommands.has(command?.commandType) && protocolFormulaSafeNoWrite.has(pair);
  const accountCreateNoWrite = command?.commandType === 'TOKEN_ACCOUNT_CREATE' && tokenAccountCreateSafeNoWrite.has(pair);
  const provenNoWrite = status !== 504 && (safeNoWrite.has(pair) || commandScopedNoWrite || accountCreateNoWrite);
  const invalidCommandResponse = code === 'INVALID_COMMAND_RESPONSE';
  return {
    ...command,
    dispatched: true,
    provenNoWrite,
    unknown: !provenNoWrite && !finalResult,
    terminal: finalResult,
    locked: !provenNoWrite,
    error: {
      status,
      code: provenNoWrite || finalResult || invalidCommandResponse ? code : 'COMMAND_RESULT_UNKNOWN',
      requestID: String(error?.requestID || ''),
      commandReference: uuidPattern.test(String(error?.commandReference || error?.details?.command_reference || '').toLowerCase()) ? String(error?.commandReference || error?.details?.command_reference) : '',
      commandResult: validGovernedTokenCommandResult(error?.commandResult, {
        commandType: command?.payload?.precheck?.action || command?.commandType,
        targetResourceType: command?.payload?.precheck?.evidence?.resource_type || error?.commandResult?.target_resource_type,
        targetResourceID: command?.payload?.precheck?.evidence?.resource_id || error?.commandResult?.target_resource_id,
      }) ? error.commandResult : null,
    },
  };
}

export async function executeHighRiskCommand(holder, payload, send, randomUUID = () => secureRequestID(), { recovery = false } = {}) {
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

export function resetHighRiskCommandAfterSafeFailure(holder, { preserveKey = false } = {}) {
  if (!holder.current?.command?.provenNoWrite) return;
  holder.current = preserveKey
    ? { command: { ...holder.current.command, dispatched: false, provenNoWrite: false, unknown: false, terminal: false, locked: false, error: null }, inflight: false }
    : { command: null, inflight: false };
}
