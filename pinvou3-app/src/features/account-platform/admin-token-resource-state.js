const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const routeNoWritePairs = new Set([
  '400:INVALID_ROUTE_COMMAND',
  '400:IDEMPOTENCY_KEY_REQUIRED',
  '401:LOGIN_REQUIRED',
  '403:CSRF_REJECTED',
  '403:TOKEN_FORBIDDEN',
  '404:TOKEN_ACCOUNT_NOT_FOUND',
  '409:TOKEN_ROUTE_VERSION_CONFLICT',
  '422:TOKEN_ROUTE_REFERENCE_INVALID',
  '503:TOKEN_COMMAND_GOVERNANCE_INACTIVE',
]);

function stableValue(value) {
  if (Array.isArray(value)) return value.map(stableValue);
  if (!value || typeof value !== 'object') return value;
  return Object.fromEntries(Object.keys(value).sort().map((key) => [key, stableValue(value[key])]));
}

function routePayloadKey(payload) {
  return JSON.stringify(stableValue(payload));
}

export function tokenResourceAccess(permissions = [], formsEnabled = false) {
  const granted = new Set(permissions);
  const manage = granted.has('platform.tokens.manage');
  const ownerCandidates = granted.has('platform.tokens.owner-candidates.read');
  const models = granted.has('platform.models.read');
  return Object.freeze({
    manage,
    ownerCandidates,
    models,
    showCreate: formsEnabled && manage && ownerCandidates,
    explainCreateBlocked: formsEnabled && manage && !ownerCandidates,
    requestOwnerCandidates: formsEnabled && manage && ownerCandidates,
    showRouteWrite: formsEnabled && manage && models,
    explainRouteBlocked: formsEnabled && manage && !models,
  });
}

export function routeEditorVisible(access, requested) {
  return Boolean(requested && access?.showRouteWrite);
}

export function prepareStableRouteCommand(previous, payload, randomUUID = () => globalThis.crypto.randomUUID()) {
  const payloadKey = routePayloadKey(payload);
  if (previous?.payloadKey === payloadKey) return previous.unknown || previous.completed ? { ...previous, locked: true } : previous;
  if (previous?.completed || previous?.dispatched && !previous.provenNoWrite) return { ...previous, locked: true };
  return { idempotencyKey: `admin-route-${randomUUID()}`, payloadKey, payload, dispatched: false, provenNoWrite: false, unknown: false, locked: false };
}

export function dispatchStableRouteCommand(command) {
  return command?.locked ? command : { ...command, dispatched: true };
}

export function failStableRouteCommand(command, error) {
  const status = Number.isInteger(error?.status) ? error.status : 0;
  const code = String(error?.code || 'REQUEST_FAILED');
  const provenNoWrite = status !== 504 && routeNoWritePairs.has(`${status}:${code}`);
  return { ...command, dispatched: true, provenNoWrite, unknown: !provenNoWrite, locked: !provenNoWrite, error: { status, code, requestID: String(error?.requestID || '') } };
}

export async function executeStableRouteCommand(holder, payload, send, randomUUID = () => globalThis.crypto.randomUUID()) {
  if (holder.current?.inflight) return { ok: false, duplicate: true, command: holder.current.command };
  const prepared = prepareStableRouteCommand(holder.current?.command, payload, randomUUID);
  if (prepared.locked) return { ok: false, duplicate: false, command: prepared, error: { code: 'COMMAND_RESULT_UNKNOWN', status: 0, requestID: prepared.error?.requestID || '' } };
  const dispatched = dispatchStableRouteCommand(prepared);
  holder.current = { command: dispatched, inflight: true };
  try {
    const value = await send(payload, prepared.idempotencyKey);
    const completed = { ...dispatched, provenNoWrite: false, completed: true, locked: true };
    holder.current = { command: completed, inflight: false };
    return { ok: true, value, command: completed };
  } catch (error) {
    const failed = failStableRouteCommand(dispatched, error);
    holder.current = { command: failed, inflight: false };
    return { ok: false, duplicate: false, command: failed, error: failed.unknown ? { ...failed.error, code: 'COMMAND_RESULT_UNKNOWN' } : failed.error };
  }
}

export function ownerCandidateQuery(ownerType, query = {}, organizationID = '') {
  const type = String(ownerType || '').toUpperCase();
  const mapped = type === 'UNIT' && !String(organizationID || '').trim() ? 'ORGANIZATION' : ({ USER: 'USER', ORGANIZATION: 'ORGANIZATION', UNIT: 'UNIT' }[type] || 'USER');
  return {
    q: String(query.q || '').trim(),
    owner_type: mapped,
    status: String(query.status || 'ACTIVE').trim().toUpperCase(),
    organization_id: mapped === 'UNIT' ? String(organizationID || '').trim() : '',
    cursor: String(query.cursor || '').trim(),
  };
}

export function tokenAccountCreateInput({ candidate, organization, name, priority = 100 }) {
  if (!candidate || !uuidPattern.test(String(candidate.id || '')) || candidate.status !== 'ACTIVE') return null;
  const displayName = String(name || '').trim();
  const defaultPriority = Number(priority);
  if (!displayName || displayName.length > 120 || !Number.isInteger(defaultPriority) || defaultPriority < 0 || defaultPriority > 10000) return null;
  if (candidate.type === 'USER') return { owner_type: 'USER', owner_id: candidate.id, owner_organization_id: null, owner_unit_id: null, name: displayName, share_scope: 'OWNER_UNIT_ONLY', default_priority: defaultPriority };
  if (candidate.type === 'ORGANIZATION') return { owner_type: 'ORG', owner_id: candidate.id, owner_organization_id: candidate.id, owner_unit_id: null, name: displayName, share_scope: 'ORG_ALL', default_priority: defaultPriority };
  if (candidate.type !== 'UNIT' || organization?.type !== 'ORGANIZATION' || !uuidPattern.test(String(organization.id || ''))) return null;
  return { owner_type: 'ORG_UNIT', owner_id: candidate.id, owner_organization_id: organization.id, owner_unit_id: candidate.id, name: displayName, share_scope: 'OWNER_SUBTREE', default_priority: defaultPriority };
}

export function stableRouteChoices(models = []) {
  const seen = new Set();
  return models.flatMap((model) => {
    const modelID = String(model?.model_id || '');
    const protocolVersionID = String(model?.protocol_definition_id || '');
    if (model?.lifecycle_status !== 'PUBLISHED' || model?.source_available !== true || !uuidPattern.test(modelID) || !uuidPattern.test(protocolVersionID)) return [];
    const key = `${modelID}:${protocolVersionID}`.toLowerCase();
    if (seen.has(key)) return [];
    seen.add(key);
    return [{
      modelID,
      displayName: String(model.display_name || modelID),
      protocolVersionID,
      protocolCode: String(model.protocol_code || ''),
      protocolVersion: Number(model.protocol_version || 0),
    }];
  });
}

export function stableRouteChange({ choice, accountVersion, existingRoute, priority, defaultScope, reason }) {
  const expectedAccountVersion = Number(accountVersion);
  const expectedRouteVersion = existingRoute ? Number(existingRoute.route_version) : 0;
  const normalizedPriority = Number(priority);
  const normalizedScope = String(defaultScope || '').toUpperCase();
  const normalizedReason = String(reason || '').trim();
  if (!choice || !uuidPattern.test(String(choice.modelID || '')) || !uuidPattern.test(String(choice.protocolVersionID || '')) || !Number.isInteger(expectedAccountVersion) || expectedAccountVersion < 1 || !Number.isInteger(expectedRouteVersion) || expectedRouteVersion < 0 || !Number.isInteger(normalizedPriority) || normalizedPriority < 0 || normalizedPriority > 10000 || !['NONE', 'UNIT', 'ORG'].includes(normalizedScope) || normalizedReason.length < 10 || normalizedReason.length > 2000) return null;
  return {
    model_id: choice.modelID,
    protocol_version_id: choice.protocolVersionID,
    priority: normalizedPriority,
    default_scope: normalizedScope,
    expected_account_version: expectedAccountVersion,
    expected_route_version: expectedRouteVersion,
    reason: normalizedReason,
  };
}

export function serverRouteHealth(route = {}, authoritativeAsOf) {
  const allowed = new Set(['UNKNOWN', 'PROBING', 'HEALTHY', 'DEGRADED', 'UNHEALTHY', 'STALE']);
  let status = allowed.has(route.health_status) ? route.health_status : 'UNKNOWN';
  const observed = Date.parse(String(route.health_observed_at || ''));
  const expiry = Date.parse(String(route.health_expires_at || ''));
  const asOf = Date.parse(String(authoritativeAsOf || ''));
  const evidenceValid = Number.isFinite(asOf) && Number.isFinite(observed) && Number.isFinite(expiry)
    && observed <= expiry && expiry - observed <= 5 * 60 * 1000
    && observed <= asOf;
  if (!evidenceValid) status = 'UNKNOWN';
  else if (expiry <= asOf) status = 'STALE';
  const diagnostic = /^[A-Z][A-Z0-9_]{0,63}$/.test(String(route.health_diagnostic_code || '')) ? route.health_diagnostic_code : '';
  return Object.freeze({
    status,
    diagnostic,
    observedAt: evidenceValid ? route.health_observed_at : null,
    expiresAt: evidenceValid ? route.health_expires_at : null,
    routable: evidenceValid && status === 'HEALTHY' && expiry > asOf,
  });
}
