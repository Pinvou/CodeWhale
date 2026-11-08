import { financeNotDispatched } from './finance-operation-state.js';
import { requireFinanceID } from './finance-contract.js';

const authorizationKeys = ['context_version', 'personal_authorization_version',
  'platform_authorization_version', 'organization_authorization_version'];

// This is a local authority snapshot; the receiver still gets its original
// five-field protocol and performs current authorization and owner checks.
export function latePaymentQueryAuthority(me) {
  const snapshot = { actorID: me.user_id };
  for (const key of authorizationKeys) snapshot[key] = Number(me[key] ?? me.active_context?.[key] ?? 0);
  return Object.freeze(snapshot);
}

export function isCurrentLatePaymentQueryAuthority(snapshot, observed = {}) {
  try { requireFinanceID(snapshot?.actorID); } catch { return false; }
  return authorizationKeys.every(key => Number.isSafeInteger(snapshot[key]) && snapshot[key] >= 0 &&
    snapshot[key] >= Number(observed[key] || 0)) && snapshot.platform_authorization_version > 0;
}

export function freshLatePaymentQuery(item, actorID, authorization) {
  return Object.freeze({ action: 'query', commandProtocol: 1, id: item.case_id,
    actorID, authorization: Object.freeze({ ...authorization }), paymentOrderID: item.payment_order_id, providerID: item.provider_id,
    providerVersion: item.provider_version, merchantOrderNo: item.merchant_order_no });
}

export function latePaymentQueryBody(caseID, facts, key) {
  try {
    requireFinanceID(caseID);
    requireFinanceID(facts?.actorID);
    requireFinanceID(facts?.paymentOrderID);
    if (!isCurrentLatePaymentQueryAuthority(facts?.authorization) || facts.authorization.actorID !== facts.actorID ||
      facts?.commandProtocol !== 1 || facts?.action !== 'query' || facts?.id !== caseID ||
      !/^[A-Za-z0-9][A-Za-z0-9._:-]{7,119}$/.test(key || '') ||
      ![facts.providerID, facts.providerVersion, facts.merchantOrderNo].every(value => typeof value === 'string' && value.length > 0)) {
      throw new Error('invalid original payment review query');
    }
    return { command_protocol: 1, payment_order_id: facts.paymentOrderID,
      provider_id: facts.providerID, provider_version: facts.providerVersion, merchant_order_no: facts.merchantOrderNo };
  } catch (error) {
    throw financeNotDispatched(Object.assign(error, { code: 'QUERY_RESULT_UNKNOWN' }), true);
  }
}

export function requireLatePaymentQueryReceipt(receipt, facts, key) {
  if (receipt?.command_protocol !== 1 || receipt?.query_reference !== key || receipt?.case_id !== facts.id ||
    receipt?.payment_order_id !== facts.paymentOrderID || receipt?.actor_id !== facts.actorID ||
    receipt?.state !== 'COMPLETED' || receipt?.result?.case_id !== facts.id ||
    receipt?.result?.payment_order_id !== facts.paymentOrderID || !receipt?.result?.last_query_at || !receipt?.result?.last_query_result) {
    throw Object.assign(new Error('original payment review query remains unconfirmed'), { code: 'COMMAND_RESULT_UNKNOWN', queryReference: key });
  }
  return receipt.result;
}
