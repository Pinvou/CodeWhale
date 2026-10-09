const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const nonzeroUUID = value => typeof value === 'string' && UUID.test(value) && value !== '00000000-0000-0000-0000-000000000000';
export function requireSession(payload) {
  if (!payload || typeof payload.authenticated !== 'boolean') throw new Error('INVALID_SESSION');
  return payload;
}
export function requirePersonalAccount(payload) {
  if (!nonzeroUUID(payload?.user_id) || typeof payload.status !== 'string' || !payload.status || !['personal_permissions', 'platform_permissions'].every(key => Array.isArray(payload[key]) && payload[key].every(value => typeof value === 'string'))) throw new Error('INVALID_ACCOUNT_ACK');
  return payload;
}
export function requireProfileChange(payload, expectedID = '', expectedField = '') {
  if (!nonzeroUUID(payload?.profile_change_request_id) || expectedID && payload.profile_change_request_id !== expectedID || expectedField && payload.field !== expectedField || typeof payload.status !== 'string' || !payload.status) throw new Error('INVALID_PROFILE_ACK');
  return payload;
}
