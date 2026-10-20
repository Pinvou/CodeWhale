import { validApprovalRequest } from './admin-high-risk-action-state.js';
import { beforeFinanceDispatch, financeNotDispatched, financeWriteObserved } from './finance-operation-state.js';

const activeStatuses = new Set(['SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTED', 'EXECUTION_FAILED']);
const terminalStatuses = new Set(['REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED']);
const knownStatuses = [...activeStatuses, ...terminalStatuses, 'DRAFT'];
const draftEditReferences = new WeakMap();

function editReferences(client) {
  let references = draftEditReferences.get(client);
  if (!references) { references = new Map(); draftEditReferences.set(client, references); }
  return references;
}

function frozenDraftKey(draft) {
  const canonical = value => Array.isArray(value) ? value.map(canonical)
    : value && typeof value === 'object' ? Object.fromEntries(Object.keys(value).sort().map(key => [key, canonical(value[key])])) : value;
  return JSON.stringify(canonical(draft));
}


function withReference(error, requestID) {
  return Object.assign(error instanceof Error ? error : new Error('approval result unavailable'), { commandReference: requestID });
}

function requireDetail(value, requestID, draft, verifyLegacyFacts = true) {
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
  if (verifyLegacyFacts && value.detail_availability === undefined && !validApprovalRequest(value, draft, knownStatuses)) {
    throw withReference(Object.assign(new Error('legacy approval detail does not match the frozen draft'),
      { code: 'INVALID_APPROVAL_RESPONSE' }), requestID);
  }
  return value;
}

export async function createAndSubmitGovernedApproval(client, draft, signal, expectedReference = '') {
  const requestID = await client.createGovernedApprovalDraft(draft, signal);
  if (expectedReference && requestID !== expectedReference) throw withReference(Object.assign(new Error('approval replay returned another invocation'), { status: 202, code: 'COMMAND_RESULT_UNKNOWN' }), expectedReference);
  try {
    const created = requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, draft);
    if (activeStatuses.has(created.status)) {
      const replayedID = await client.createGovernedApprovalDraft(draft, signal);
      if (replayedID !== requestID) throw Object.assign(new Error('approval replay returned another invocation'), { status: 202, code: 'COMMAND_RESULT_UNKNOWN' });
      return created;
    }
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

export async function createAndSubmitFreshGovernedApproval(client, draft, signal, expectedReference = '', referenceStorage) {
  const references = referenceStorage || editReferences(client);
  const identity = JSON.stringify([client.financeSessionEpoch || 0, draft?.command_type, draft?.target_resource_id, draft?.idempotency_key]);
  const frozenInput = frozenDraftKey(draft);
  const previous = references.get(identity);
  if (previous) {
    if (expectedReference && previous.existing.approval_request_id !== expectedReference) throw Object.assign(new Error('approval invocation reference changed'), { status: 202, code: 'COMMAND_RESULT_UNKNOWN', commandReference: expectedReference });
    if (previous.input !== frozenInput) throw Object.assign(new Error('approval invocation facts changed'), { status: 409, code: 'APPROVAL_FACTS_CHANGED' });
    const result = await updateAndSubmitGovernedApproval(client, previous.existing, draft, signal);
    references.delete(identity);
    return result;
  }
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
    return createAndSubmitGovernedApproval(client, draft, signal, expectedReference);
  }
  if (matching[0]?.status === 'DRAFT') {
    if (expectedReference && matching[0].approval_request_id !== expectedReference) throw Object.assign(new Error('approval history returned another invocation'), { status: 202, code: 'COMMAND_RESULT_UNKNOWN', commandReference: expectedReference });
    if (!matching[0].approval_request_id || matching.slice(1).some((item) =>
      !terminalStatuses.has(item.status) && item.status !== 'EXECUTED')) {
      throw financeNotDispatched(Object.assign(new Error('approval history cannot identify a single editable draft'),
        { code: 'APPROVAL_ALREADY_ACTIVE' }));
    }
    // Preserve the first authoritative CAS for this logical invocation, even
    // when its PUT or subsequent submit commits but the response is lost.
    const existing = JSON.parse(JSON.stringify(matching[0])); // Plain wire DTO; compatible with the Safari 14 baseline.
    references.set(identity, { existing, input: frozenInput });
    const result = await updateAndSubmitGovernedApproval(client, existing, draft, signal);
    references.delete(identity);
    return result;
  }
  const supersedesID = supersedesEligibleApproval(page, draft.command_type, draft.target_resource_id);
  return createAndSubmitGovernedApproval(client, supersedesID
    ? { ...draft, supersedes_request_id: supersedesID } : draft, signal, expectedReference);
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
    const current = await beforeFinanceDispatch(async () => requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, reference, false));
    const sameVersion = current.version === existing.version;
    if (sameVersion && (current.status !== 'DRAFT' || !current.allowed_actions.includes('EDIT'))) {
      throw financeNotDispatched(Object.assign(new Error('approval draft is no longer editable'), { code: 'APPROVAL_DRAFT_NOT_EDITABLE' }));
    }
    // On a lost acknowledgement, replay the original CAS and frozen input.
    // Account's durable edit receipt must prove it; the UI never infers success
    // from the newer version. Use the fresh action control, preserving that CAS.
    await client.updateGovernedApprovalDraft(requestID, { ...current, version: existing.version }, draft, signal);
    writeObserved = true;
    const updated = requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, draft);
    if (activeStatuses.has(updated.status)) {
      // The typed detail omits the original caller fingerprint. Confirm the
      // same frozen edit against Account's durable receipt AFTER this read.
      // Active approval intent is immutable; a foreign edit cannot be adopted.
      await client.updateGovernedApprovalDraft(requestID, { ...updated, version: existing.version }, draft, signal);
      return updated;
    }
    if (updated.status !== 'DRAFT' || updated.version !== existing.version + 1 ||
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

export async function recoverGovernedApproval(client, requestID, draft, signal) {
  if (!requestID) {
    if (typeof draft?.idempotency_key !== 'string' || !draft.idempotency_key) throw Object.assign(new Error('approval invocation identity is unavailable'), { status: 202, code: 'COMMAND_RESULT_UNKNOWN' });
    return createAndSubmitFreshGovernedApproval(client, draft, signal);
  }
  let value;
  try {
    value = requireDetail(await client.governedApprovalRequest(requestID, signal), requestID, draft);
  } catch (error) {
    throw withReference(error, requestID);
  }

  if (terminalStatuses.has(value.status)) {
    throw Object.assign(new Error('approval reached a terminal non-success state'),
      { code: `APPROVAL_${value.status}`, commandReference: requestID, finalResult: true });
  }
  if (typeof draft?.idempotency_key !== 'string' || !draft.idempotency_key) {
    throw Object.assign(new Error('approval invocation identity is unavailable'),
      { status: 202, code: 'COMMAND_RESULT_UNKNOWN', commandReference: requestID });
  }
  try {
    const recovered = await createAndSubmitFreshGovernedApproval(client, draft, signal, requestID);
    if (recovered.approval_request_id !== requestID) {
      throw Object.assign(new Error('approval replay returned another invocation'),
        { status: 202, code: 'COMMAND_RESULT_UNKNOWN' });
    }
    return recovered;
  } catch (error) {
    throw withReference(error, requestID);
  }
}
