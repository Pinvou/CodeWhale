import { secureRequestID } from './request-id.js';
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

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
