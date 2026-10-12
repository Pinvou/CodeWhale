import { validApprovalRequest } from './admin-high-risk-action-state.js';
import { beforeFinanceDispatch, financeNotDispatched, financeWriteObserved } from './finance-operation-state.js';

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
    throw withReference(financeWriteObserved(error), requestID);
  }
}

export function supersedesEligibleApproval(page, commandType, targetID) {
  if (!page || !Array.isArray(page.items) || page.has_more === true || page.next_cursor || page.items.length >= 200) {
    throw financeNotDispatched(Object.assign(new Error('approval history is incomplete'), { code: 'APPROVAL_HISTORY_INCOMPLETE' }));
  }
  const matching = page.items.filter((item) => item.command_type === commandType &&
    (item.target?.resource_id || item.target_resource_id) === targetID);
  if (matching.some((item) => ['DRAFT', 'SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTION_FAILED'].includes(item.status))) {
    throw financeNotDispatched(Object.assign(new Error('an approval is already open or awaiting recovery'), { code: 'APPROVAL_ALREADY_ACTIVE' }));
  }
  const latest = matching[0];
  if (!latest || latest.status === 'EXECUTED') return '';
  if (terminalStatuses.has(latest.status) && latest.approval_request_id) return latest.approval_request_id;
  throw financeNotDispatched(Object.assign(new Error('approval state is not eligible for replacement'), { code: 'APPROVAL_REPLACEMENT_UNAVAILABLE' }));
}

export async function createAndSubmitFreshGovernedApproval(client, draft, signal) {
  const page = await beforeFinanceDispatch(() => client.governedApprovalRequests({ command_type: draft.command_type,
    target: draft.target_resource_id, limit: 200 }, signal));
  if (!page || !Array.isArray(page.items) || page.has_more === true || page.next_cursor || page.items.length >= 200) {
    throw financeNotDispatched(Object.assign(new Error('approval history is incomplete'), { code: 'APPROVAL_HISTORY_INCOMPLETE' }));
  }
  const matching = page.items.filter((item) => item.command_type === draft.command_type &&
    (item.target?.resource_id || item.target_resource_id) === draft.target_resource_id);
  // Re-enter the authoritative create replay with the same frozen identity.
  // An active target alone cannot identify our invocation; Account either
  // returns this exact idempotent request or rejects a different invocation.
  if (matching.some(item => activeStatuses.has(item.status))) {
    return createAndSubmitGovernedApproval(client, draft, signal);
  }
  if (matching[0]?.status === 'DRAFT') {
    if (!matching[0].approval_request_id || matching.slice(1).some((item) =>
      !terminalStatuses.has(item.status) && item.status !== 'EXECUTED')) {
      throw financeNotDispatched(Object.assign(new Error('approval history cannot identify a single editable draft'),
        { code: 'APPROVAL_ALREADY_ACTIVE' }));
    }
    return updateAndSubmitGovernedApproval(client, matching[0], draft, signal);
  }
  const supersedesID = supersedesEligibleApproval(page, draft.command_type, draft.target_resource_id);
  return createAndSubmitGovernedApproval(client, supersedesID
    ? { ...draft, supersedes_request_id: supersedesID } : draft, signal);
}

export async function updateAndSubmitGovernedApproval(client, existing, draft, signal) {
  const requestID = existing?.approval_request_id;
  if (typeof requestID !== 'string' || !requestID || !draft?.command_type || !draft?.target_resource_type ||
    !draft?.target_resource_id || existing?.command_type !== draft.command_type ||
    (existing.target?.resource_type || existing.target_resource_type) !== draft.target_resource_type ||
    (existing.target?.resource_id || existing.target_resource_id) !== draft.target_resource_id) {
    throw financeNotDispatched(Object.assign(new Error('approval reference does not match the edited command'), { code: 'INVALID_APPROVAL_RESPONSE' }));
  }
  let writeObserved = false;
  try {
    const reference = { ...existing, target_resource_type: draft.target_resource_type,
      target_resource_id: draft.target_resource_id };
    const current = await beforeFinanceDispatch(async () => requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, reference));
    if (current.version !== existing.version) {
      throw financeNotDispatched(Object.assign(new Error('approval version changed; reload before editing'), { status: 409, code: 'APPROVAL_FACTS_CHANGED' }));
    }
    if (current.status !== 'DRAFT' || !current.allowed_actions.includes('EDIT')) {
      throw financeNotDispatched(Object.assign(new Error('approval draft is no longer editable'), { code: 'APPROVAL_DRAFT_NOT_EDITABLE' }));
    }
    await client.updateGovernedApprovalDraft(requestID, current, draft, signal);
    writeObserved = true;
    const updated = requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, draft);
    if (updated.status !== 'DRAFT' || updated.version !== current.version + 1 ||
      !updated.allowed_actions.includes('SUBMIT')) {
      throw Object.assign(new Error('approval update result remains unconfirmed'),
        { status: 202, code: 'COMMAND_RESULT_UNKNOWN' });
    }
    await client.submitGovernedApprovalRequest(requestID, updated, signal);
    const submitted = requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, draft);
    if (!activeStatuses.has(submitted.status)) {
      throw Object.assign(new Error('approval submit result remains unconfirmed'),
        { status: 202, code: 'COMMAND_RESULT_UNKNOWN' });
    }
    return submitted;
  } catch (error) {
    throw withReference(writeObserved ? financeWriteObserved(error) : error, requestID);
  }
}
