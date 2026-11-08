import { beforeFinanceDispatch, financeNotDispatched, financeWriteObserved } from './finance-operation-state.js';
import { requireFinanceID } from './finance-contract.js';

const resultFields = ['approval_request_id', 'command_protocol', 'operation', 'expected_version', 'result_version',
  'origin_action_control_version', 'action_control_version', 'receipt_event_id', 'status', 'recorded_at'];
function unknown() { return Object.assign(new Error('the original withdrawal remains unconfirmed'), { code: 'COMMAND_RESULT_UNKNOWN' }); }

function requireOriginal(requestID, detail, protocol) {
  if (protocol !== 1 || detail?.approval_request_id !== requestID || !Number.isSafeInteger(detail?.version) ||
    detail.version < 1 || detail.version >= Number.MAX_SAFE_INTEGER ||
    !Number.isSafeInteger(detail?.action_control_version) || detail.action_control_version < 1) {
    throw financeNotDispatched(unknown());
  }
  try { requireFinanceID(requestID); } catch (error) { throw financeNotDispatched(error, true); }
}

function requireReceipt(receipt, requestID, detail, currentControl) {
  if (!receipt || Object.keys(receipt).length !== resultFields.length || resultFields.some(key => !(key in receipt)) ||
    receipt.approval_request_id !== requestID || receipt.command_protocol !== 1 || receipt.operation !== 'WITHDRAW' ||
    receipt.expected_version !== detail.version || receipt.result_version !== detail.version + 1 ||
    receipt.origin_action_control_version !== detail.action_control_version || receipt.action_control_version !== currentControl ||
    receipt.status !== 'WITHDRAWN' || typeof receipt.recorded_at !== 'string' || !Number.isFinite(Date.parse(receipt.recorded_at))) throw unknown();
  requireFinanceID(receipt.receipt_event_id);
  return receipt;
}

async function readOriginalWithdrawal(client, requestID, detail, signal, currentIdentity) {
  currentIdentity();
  const capability = await beforeFinanceDispatch(() => client.governedApprovalCapability(signal));
  currentIdentity();
  const lineage = client.approvalLineage();
  if (lineage.control !== capability) throw unknown();
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal?.addEventListener('abort', abort, { once: true });
  if (signal?.aborted) controller.abort();
  client.approvalControllers.add(controller);
  try {
    const query = new URLSearchParams({ command_protocol: '1', expected_version: String(detail.version),
      origin_action_control_version: String(detail.action_control_version) });
    const response = await fetch(`${client.baseURL}/api/admin/approvals/requests/${encodeURIComponent(requestID)}/actions/withdraw-result?${query}`, {
      headers: { Accept: 'application/json', 'X-Pinvou-Approval-Action-Control-Version': String(capability.control.version) },
      credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
    });
    const payload = await response.json().catch(() => null);
    currentIdentity();
    if (!client.approvalLineageMatches(lineage)) throw unknown();
    if (response.status !== 200) throw Object.assign(unknown(), { status: response.status });
    return requireReceipt(payload, requestID, detail, capability.control.version);
  } finally {
    client.approvalControllers.delete(controller);
    signal?.removeEventListener('abort', abort);
  }
}

// The write always retains its original CAS/control. A changed control may
// authorize only this separate read of an already committed original event.
export async function withdrawWithOriginalReceipt(client, requestID, detail, signal, protocol, write) {
  requireOriginal(requestID, detail, protocol);
  const epoch = client.financeSessionEpoch;
  const csrf = client.csrfToken;
  const authorization = JSON.stringify(client.authorizationVersions);
  const currentIdentity = () => {
    if (signal?.aborted || epoch !== client.financeSessionEpoch || csrf !== client.csrfToken ||
      authorization !== JSON.stringify(client.authorizationVersions)) throw unknown();
  };
  try { currentIdentity(); } catch (error) { throw financeNotDispatched(error); }
  try {
    const value = await write();
    currentIdentity();
    return value;
  } catch (error) {
    const original = error?.financeRequestState === 'NOT_DISPATCHED' ? error : financeWriteObserved(error);
    try {
      await readOriginalWithdrawal(client, requestID, detail, signal, currentIdentity);
      return null;
    } catch {
      // Missing/forbidden/retired confirmation is never proof of zero effects.
      // In particular there is no fallback write with the newer control/CAS.
      throw original;
    }
  }
}
