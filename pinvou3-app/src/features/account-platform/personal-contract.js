const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const nonzeroUUID = value => typeof value === 'string' && UUID.test(value) && value !== '00000000-0000-0000-0000-000000000000';
export function requireSession(payload) {
  if (!payload || typeof payload.authenticated !== 'boolean') throw new Error('INVALID_SESSION');
  return payload;
}
export function requirePersonalAccount(payload) {
  const context = payload?.active_context;
  const validVersion = value => Number.isSafeInteger(value) && value > 0;
  const validContext = context && !Array.isArray(context) && ['PERSONAL', 'ORGANIZATION'].includes(context.context_type) &&
    validVersion(context.context_version) && Array.isArray(context.permissions) && context.permissions.every(value => typeof value === 'string') &&
    (context.context_type === 'PERSONAL' ? context.organization_id == null :
      nonzeroUUID(context.organization_id) && validVersion(context.membership_version) && validVersion(context.organization_authorization_version));
  if (!validContext || !nonzeroUUID(payload?.user_id) || typeof payload.status !== 'string' || !payload.status || !['personal_permissions', 'platform_permissions'].every(key => Array.isArray(payload[key]) && payload[key].every(value => typeof value === 'string'))) throw new Error('INVALID_ACCOUNT_ACK');
  return payload;
}
export function requireProfileChange(payload, expectedID = '', expectedField = '') {
  if (!nonzeroUUID(payload?.profile_change_request_id) || expectedID && payload.profile_change_request_id !== expectedID || expectedField && payload.field !== expectedField || typeof payload.status !== 'string' || !payload.status) throw new Error('INVALID_PROFILE_ACK');
  return payload;
}

// A notice selects a destination; only the current authenticated snapshot grants access.
export function organizationRechargeTargetAllowed(profile, target) {
  const context = profile?.active_context;
  return target?.contextType === 'ORGANIZATION' && target.view === 'organizationRecharge'
    && nonzeroUUID(target.organizationId) && target.organizationId === target.organizationId.toLowerCase()
    && context?.context_type === 'ORGANIZATION' && context.organization_id === target.organizationId
    && Array.isArray(context.permissions) && context.permissions.includes('org.entitlements.purchase');
}
