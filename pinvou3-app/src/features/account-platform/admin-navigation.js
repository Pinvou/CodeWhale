
const adminModelTokenQueryParameters = Object.freeze({
  models: Object.freeze({ q: 'model_q', lifecycle_status: 'model_lifecycle_status', protocol_code: 'model_protocol_code', source_adapter: 'model_source_adapter', source_missing: 'model_source_missing', cursor: 'model_cursor' }),
  protocols: Object.freeze({ q: 'protocol_q', status: 'protocol_status', protocol_code: 'protocol_code', published: 'protocol_published', created_from: 'protocol_created_from', created_to: 'protocol_created_to', cursor: 'protocol_cursor' }),
  formulas: Object.freeze({ q: 'formula_q', status: 'formula_status', protocol_definition_id: 'formula_protocol_id', metering_unit_code: 'formula_metering_unit_code', published: 'formula_published', created_from: 'formula_created_from', created_to: 'formula_created_to', cursor: 'formula_cursor' }),
  tokenAccounts: Object.freeze({ q: 'token_q', owner_type: 'token_owner_type', owner_id: 'token_owner_id', status: 'token_status', health_status: 'token_health_status', credential_status: 'token_credential_status', cursor: 'token_cursor' }),
  provisionings: Object.freeze({ q: 'provisioning_q', status: 'provisioning_status', model_id: 'provisioning_model_id', token_account_id: 'provisioning_token_account_id', scope_type: 'provisioning_scope_type', scope_id: 'provisioning_scope_id', sla_overdue: 'provisioning_sla_overdue', cursor: 'provisioning_cursor' }),
  usage: Object.freeze({ q: 'usage_q', execution_status: 'usage_execution_status', settlement_status: 'usage_settlement_status', model: 'usage_model', token_account_id: 'usage_token_account_id', organization_id: 'usage_organization_id', started_from: 'usage_started_from', started_to: 'usage_started_to', cursor: 'usage_cursor' }),
  reconciliation: Object.freeze({ q: 'reconciliation_q', status: 'reconciliation_status', severity: 'reconciliation_severity', case_type: 'reconciliation_case_type', owner_id: 'reconciliation_owner_id', sla_overdue: 'reconciliation_sla_overdue', cursor: 'reconciliation_cursor' }),
  ownerCandidates: Object.freeze({ q: 'owner_candidate_q', owner_type: 'owner_candidate_type', status: 'owner_candidate_status', organization_id: 'owner_candidate_organization_id', cursor: 'owner_candidate_cursor' }),
  accountRecovery: Object.freeze({ q: 'account_recovery_q', cursor: 'account_recovery_cursor' }),
  modelAudit: Object.freeze({ action: 'model_audit_action', actor_id: 'model_audit_actor_id', occurred_from: 'model_audit_occurred_from', occurred_to: 'model_audit_occurred_to', cursor: 'model_audit_cursor' }),
  modelHistory: Object.freeze({ cursor: 'model_history_cursor' }),
	modelLifecyclePlans: Object.freeze({ status: 'model_lifecycle_plan_status', target_lifecycle: 'model_lifecycle_plan_target', cursor: 'model_lifecycle_plan_cursor' }),
	modelLifecycleCandidates: Object.freeze({ q: 'model_lifecycle_candidate_q', cursor: 'model_lifecycle_candidate_cursor' }),
  tokenAudit: Object.freeze({ action: 'token_audit_action', actor_id: 'token_audit_actor_id', occurred_from: 'token_audit_occurred_from', occurred_to: 'token_audit_occurred_to', cursor: 'token_audit_cursor' }),
  modelProvisionings: Object.freeze({ q: 'model_provisioning_q', status: 'model_provisioning_status', cursor: 'model_provisioning_cursor' }),
  tokenProvisionings: Object.freeze({ q: 'token_provisioning_q', status: 'token_provisioning_status', cursor: 'token_provisioning_cursor' }),
  tokenCredentials: Object.freeze({ status: 'token_credential_metadata_status', cursor: 'token_credential_cursor' }),
  tokenRoutes: Object.freeze({ health_status: 'token_route_health_status', cursor: 'token_route_cursor' }),
  provisioningAttempts: Object.freeze({ status: 'provisioning_attempt_status', cursor: 'provisioning_attempt_cursor' }),
  usageEvidence: Object.freeze({ evidence_type: 'usage_evidence_type', cursor: 'usage_evidence_cursor' }),
});

export function readAdminModelTokenQuery(source, locationLike = globalThis.location) {
  const parameters = adminModelTokenQueryParameters[source];
  if (!parameters) return {};
  const search = new URLSearchParams(String(locationLike?.search || ''));
  const result = {};
  for (const [key, parameter] of Object.entries(parameters)) result[key] = String(search.get(parameter) || '').trim();
  for (const key of ['source_missing', 'published', 'sla_overdue']) {
    if (Object.prototype.hasOwnProperty.call(result, key) && result[key] !== 'true' && result[key] !== 'false') result[key] = '';
  }
  return result;
}

export function adminModelTokenQueryUpdates(source, input = {}) {
  const parameters = adminModelTokenQueryParameters[source];
  if (!parameters) return {};
  return Object.fromEntries(Object.entries(parameters).map(([key, parameter]) => [parameter, String(input[key] || '').trim() || null]));
}

export function synchronizeAdminModelTokenCursorHistory(previous = {}, query = {}) {
  const signature = JSON.stringify(Object.entries(query).filter(([key]) => key !== 'cursor').sort(([left], [right]) => left.localeCompare(right)));
  const currentCursor = String(query.cursor || '');
  if (previous.signature !== signature) return { signature, currentCursor, previousByCursor: {} };
  return { ...previous, currentCursor, previousByCursor: { ...(previous.previousByCursor || {}) } };
}

export function rememberAdminModelTokenNextCursor(history, nextCursor) {
  const cursor = String(nextCursor || '');
  if (!cursor) return history;
  return { ...history, currentCursor: cursor, previousByCursor: { ...history.previousByCursor, [cursor]: history.currentCursor || '' } };
}

export function previousAdminModelTokenCursor(history) {
  const current = String(history?.currentCursor || '');
  if (!current || !Object.prototype.hasOwnProperty.call(history?.previousByCursor || {}, current)) return { available: false, cursor: '' };
  return { available: true, cursor: history.previousByCursor[current] || '' };
}

export function readAdminModelTokenModelTab(permissions = [], locationLike = globalThis.location) {
  const granted = new Set(permissions);
  const visible = [
    granted.has('platform.models.read') ? 'directory' : '',
    granted.has('platform.pricing.read') ? 'protocols' : '',
  ].filter(Boolean);
  const requested = new URLSearchParams(String(locationLike?.search || '')).get('model_tab') || '';
  return { tab: visible.includes(requested) ? requested : visible[0] || '', replace: Boolean(visible.length && !visible.includes(requested)), visible };
}
