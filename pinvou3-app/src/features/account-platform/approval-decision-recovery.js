import { beforeFinanceDispatch, financeNotDispatched, financeWriteObserved } from './finance-operation-state.js';
import { requireFinanceID } from './finance-contract.js';
import { canonicalApprovalDecision } from './approval-decision-contract.js';

export { canonicalApprovalDecision } from './approval-decision-contract.js';

const receiptFields = [
  'approval_request_id', 'command_protocol', 'operation', 'decision', 'expected_version', 'result_version',
  'origin_action_control_version', 'action_control_version', 'reason_digest', 'receipt_event_id', 'decision_id', 'recorded_at',
];

function unconfirmed() {
  return Object.assign(new Error('the original approval decision remains unconfirmed'), { code: 'COMMAND_RESULT_UNKNOWN' });
}

// The digest binds the JSON-decoded wire reason, before domain normalization.
export async function approvalDecisionReasonDigest(reason) {
  if (typeof reason !== 'string' || !globalThis.crypto?.subtle) throw financeNotDispatched(unconfirmed(), true);
  const bytes = await globalThis.crypto.subtle.digest('SHA-256', new TextEncoder().encode(reason));
  return [...new Uint8Array(bytes)].map(value => value.toString(16).padStart(2, '0')).join('');
}

function requireReceipt(value, id, detail, decision, digest, current) {
  if (!value || Object.keys(value).length !== receiptFields.length || receiptFields.some(key => !(key in value)) ||
    value.approval_request_id !== id || value.command_protocol !== 1 || value.operation !== 'DECIDE' || value.decision !== decision ||
    value.expected_version !== detail.version || value.result_version !== detail.version + 1 ||
    value.origin_action_control_version !== detail.action_control_version || value.action_control_version !== current ||
    value.reason_digest !== digest || typeof value.recorded_at !== 'string' || !Number.isFinite(Date.parse(value.recorded_at))) {
    throw unconfirmed();
  }
  requireFinanceID(value.receipt_event_id);
  requireFinanceID(value.decision_id);
}

async function readOriginal(client, id, detail, decision, digest, signal, currentIdentity) {
  currentIdentity();
  // Reuse an already validated live tuple: a late original response must not
  // refetch an older server observation and retire newer concurrent readers.
  // The result endpoint still checks this current tuple and current authority.
  const capability = client.approvalControl || await beforeFinanceDispatch(() => client.governedApprovalCapability(signal));
  currentIdentity();
  const lineage = client.approvalLineage();
  if (lineage.control !== capability) throw unconfirmed();
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal?.addEventListener('abort', abort, { once: true });
  if (signal?.aborted) controller.abort();
  client.approvalControllers.add(controller);
  try {
    const query = new URLSearchParams({
      command_protocol: '1', expected_version: String(detail.version),
      origin_action_control_version: String(detail.action_control_version), decision, reason_digest: digest,
    });
    const response = await fetch(`${client.baseURL}/api/admin/approvals/requests/${encodeURIComponent(id)}/actions/decision-result?${query}`, {
      headers: { Accept: 'application/json', 'X-Pinvou-Approval-Action-Control-Version': String(capability.control.version) },
      credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
    });
    const value = await response.json().catch(() => null);
    currentIdentity();
    if (!client.approvalLineageMatches(lineage) || response.status !== 200) throw unconfirmed();
    requireReceipt(value, id, detail, decision, digest, capability.control.version);
  } finally {
    client.approvalControllers.delete(controller);
    signal?.removeEventListener('abort', abort);
  }
}

export async function decideWithOriginalReceipt(client, id, detail, action, reason, signal, protocol, write) {
  const decision = canonicalApprovalDecision(action);
  if (protocol !== 1 || detail?.approval_request_id !== id || !Number.isSafeInteger(detail?.version) ||
    detail.version < 1 || detail.version >= Number.MAX_SAFE_INTEGER ||
    !Number.isSafeInteger(detail?.action_control_version) || detail.action_control_version < 1) {
    throw financeNotDispatched(unconfirmed());
  }
  try { requireFinanceID(id); } catch (error) { throw financeNotDispatched(error, true); }
  const epoch = client.financeSessionEpoch;
  const csrf = client.csrfToken;
  const authorization = JSON.stringify(client.authorizationVersions);
  const currentIdentity = () => {
    if (signal?.aborted || epoch !== client.financeSessionEpoch || csrf !== client.csrfToken ||
      authorization !== JSON.stringify(client.authorizationVersions)) throw unconfirmed();
  };
  let digest;
  try {
    currentIdentity();
    digest = await approvalDecisionReasonDigest(reason);
    currentIdentity();
  } catch (error) {
    throw financeNotDispatched(error);
  }
  let dispatchedLineage;
  try {
    const value = await write(decision, lineage => { dispatchedLineage = lineage; });
    currentIdentity();
    return value;
  } catch (error) {
    // A typed closed-decoder denial proves this fresh attempt was not dispatched.
    // The operation hook still retains any previously unresolved original intent.
    const zeroDispatch = error?.status === 400 && [
      'INVALID_APPROVAL_REQUEST', 'INVALID_APPROVAL_DECISION', 'INVALID_APPROVAL_ACTION_CONTROL_VERSION',
    ].includes(error.code);
    const original = error?.financeRequestState === 'NOT_DISPATCHED' ? error
      : zeroDispatch ? financeNotDispatched(error) : financeWriteObserved(error);
    // A concurrent live control belongs to its newer operation. Keep this
    // original unknown; an explicit retry can confirm it without a POST.
    if (dispatchedLineage && client.approvalControl && client.approvalControl !== dispatchedLineage.control) throw original;
    try {
      await readOriginal(client, id, detail, decision, digest, signal, currentIdentity);
      return null;
    } catch {
      throw original;
    }
  }
}
