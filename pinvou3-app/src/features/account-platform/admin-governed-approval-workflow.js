import { validApprovalRequest } from './admin-high-risk-action-state.js';

const activeStatuses = new Set(['SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTED', 'EXECUTION_FAILED']);
const terminalStatuses = new Set(['REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED']);
const knownStatuses = [...activeStatuses, ...terminalStatuses, 'DRAFT'];

function withReference(error, requestID) {
  return Object.assign(error instanceof Error ? error : new Error('approval result unavailable'), { commandReference: requestID });
}

function requireDetail(value, requestID, draft) {
  const target = value?.target || { resource_type: value?.target_resource_type, resource_id: value?.target_resource_id };
  if (value?.approval_request_id !== requestID || value.command_type !== draft.command_type ||
    target.resource_type !== draft.target_resource_type || target.resource_id !== draft.target_resource_id ||
    !Number.isSafeInteger(value.version) || value.version < 1 ||
    !Number.isSafeInteger(value.action_control_version) || value.action_control_version < 1 ||
    ![undefined, 'AVAILABLE'].includes(value.detail_availability) ||
    !Array.isArray(value.allowed_actions)) {
    throw withReference(Object.assign(new Error('approval detail does not match the frozen request'),
      { code: 'INVALID_APPROVAL_RESPONSE' }), requestID);
  }
  if (value.detail_availability === undefined && !validApprovalRequest(value, draft, knownStatuses)) {
    throw withReference(Object.assign(new Error('legacy approval detail does not match the frozen draft'),
      { code: 'INVALID_APPROVAL_RESPONSE' }), requestID);
  }
  return value;
}

export async function createAndSubmitGovernedApproval(client, draft, signal) {
  const requestID = await client.createGovernedApprovalDraft(draft, signal);
  try {
    const created = requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, draft);
    if (activeStatuses.has(created.status)) return created;
    if (created.status !== 'DRAFT' || created.version !== 1 || !created.allowed_actions?.includes('SUBMIT')) {
      throw Object.assign(new Error('approval draft requires business review before submission'),
        { status: 202, code: 'COMMAND_RESULT_UNKNOWN' });
    }
    await client.submitGovernedApprovalRequest(requestID, created, signal);
    const submitted = requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, draft);
    if (!activeStatuses.has(submitted.status)) {
      throw Object.assign(new Error('approval submit result remains unconfirmed'),
        { status: 202, code: 'COMMAND_RESULT_UNKNOWN' });
    }
    return submitted;
  } catch (error) {
    throw withReference(error, requestID);
  }
}

export async function submitExistingGovernedApproval(client, existing, signal) {
  const requestID = existing?.approval_request_id;
  const draft = {
    command_type: existing?.command_type,
    target_resource_type: existing?.target?.resource_type || existing?.target_resource_type,
    target_resource_id: existing?.target?.resource_id || existing?.target_resource_id,
    reason: existing?.reason, normalized_command: existing?.normalized_command,
    critical_resource_versions: existing?.critical_resource_versions,
    financial_snapshot: existing?.financial_snapshot,
  };
  if (typeof requestID !== 'string' || !draft.command_type || !draft.target_resource_type || !draft.target_resource_id) {
    throw Object.assign(new Error('approval reference is incomplete'), { code: 'INVALID_APPROVAL_RESPONSE' });
  }
  try {
    const current = requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, draft);
    if (activeStatuses.has(current.status)) return current;
    if (current.status !== 'DRAFT' || current.version !== 1 || !current.allowed_actions.includes('SUBMIT')) {
      throw Object.assign(new Error('approval draft requires business review before submission'),
        { status: 202, code: 'COMMAND_RESULT_UNKNOWN' });
    }
    await client.submitGovernedApprovalRequest(requestID, current, signal);
    const submitted = requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, draft);
    if (!activeStatuses.has(submitted.status)) {
      throw Object.assign(new Error('approval submit result remains unconfirmed'),
        { status: 202, code: 'COMMAND_RESULT_UNKNOWN' });
    }
    return submitted;
  } catch (error) {
    throw withReference(error, requestID);
  }
}

export async function recoverGovernedApproval(client, requestID, draft, signal) {
  let value;
  try {
    value = requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, draft);
  } catch (error) {
    throw withReference(error, requestID);
  }
  if (activeStatuses.has(value.status)) return value;
  if (terminalStatuses.has(value.status)) {
    throw Object.assign(new Error('approval reached a terminal non-success state'),
      { code: `APPROVAL_${value.status}`, commandReference: requestID, finalResult: true });
  }
  throw Object.assign(new Error('approval result remains unconfirmed'),
    { status: 202, code: 'COMMAND_RESULT_UNKNOWN', commandReference: requestID });
}
