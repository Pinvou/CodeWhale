import { requireFinanceID } from './finance-contract.js';

export function requireAccountClosure(item) {
  const statuses = ['IN_PROGRESS', 'BLOCKED', 'RETRY', 'REPAIR_REQUIRED', 'COMPLETED'];
  const stages = ['CLOSURE_REQUESTED', 'QUIESCING', 'FINANCIAL_CLEARANCE', 'IDENTITY_CLEANUP', 'ANONYMIZED', 'COMPLETED'];
  if (!item || typeof item !== 'object' || !statuses.includes(item.status) || !stages.includes(item.stage) || !Array.isArray(item.blockers)) throw unknownClosureACK();
  try { requireFinanceID(item.closure_request_id); } catch { throw unknownClosureACK(); }
  for (const blocker of item.blockers) {
    if (!blocker || typeof blocker !== 'object' || typeof blocker.code !== 'string' || typeof blocker.domain !== 'string' || (blocker.user_action != null && typeof blocker.user_action !== 'string')) throw unknownClosureACK();
  }
  return item;
}

function unknownClosureACK() {
  const error = new Error('COMMAND_RESULT_UNKNOWN');
  error.code = 'COMMAND_RESULT_UNKNOWN';
  return error;
}
