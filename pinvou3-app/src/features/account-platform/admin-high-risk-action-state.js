const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const fingerprintPattern = /^[0-9a-f]{64}$/;
function stable(value) {
 if (Array.isArray(value)) return value.map(stable);
 if (!value || typeof value !== 'object') return value;
 return Object.fromEntries(Object.keys(value).sort().map(key => [key, stable(value[key])]));
}
function highRiskPayloadKey(value) { return JSON.stringify(stable(value)); }
export function validApprovalRequest(value, expected, statuses) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  if (!uuidPattern.test(String(value.approval_request_id || '').toLowerCase()) || !uuidPattern.test(String(value.requester_user_id || '').toLowerCase())) return false;
  if (value.command_type !== expected.command_type || value.target_resource_type !== expected.target_resource_type || value.target_resource_id !== expected.target_resource_id || value.reason !== expected.reason) return false;
	const expectedPolicyVersion = expected.command_type === 'PAYMENT_PROVIDER_RESUME' ? 2 : 1;
  if (!['WALLET_ADJUSTMENT', 'PAYMENT_PROVIDER_RESUME', 'PAYMENT_PRODUCT_RESUME'].includes(expected.command_type)) return false;
	if (value.policy_version !== expectedPolicyVersion || value.approval_mode !== 'APPROVAL_REQUIRED' || !statuses.includes(value.status) || !Number.isInteger(value.version) || value.version < 1) return false;
  if (!fingerprintPattern.test(String(value.command_fingerprint || '')) || Number.isNaN(Date.parse(String(value.expires_at || ''))) || Number.isNaN(Date.parse(String(value.created_at || ''))) || Number.isNaN(Date.parse(String(value.updated_at || '')))) return false;
  if (!Array.isArray(value.allowed_actions) || value.allowed_actions.some((item) => !['EDIT', 'SUBMIT', 'WITHDRAW', 'APPROVE', 'REJECT'].includes(item))) return false;
  return highRiskPayloadKey(value.normalized_command) === highRiskPayloadKey(expected.normalized_command)
    && highRiskPayloadKey(value.critical_resource_versions) === highRiskPayloadKey(expected.critical_resource_versions)
    && highRiskPayloadKey(value.financial_snapshot) === highRiskPayloadKey(expected.financial_snapshot);
}
