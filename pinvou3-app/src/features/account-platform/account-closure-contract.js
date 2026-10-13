import { requireFinanceID } from './finance-contract.js';

function validSLA(value) {
  if (typeof value !== 'string') return false;
  const match = /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:[Zz]|([+-])(\d{2}):(\d{2}))$/.exec(value);
  if (!match) return false;
  const [year, month, day, hour, minute, second] = match.slice(1, 7).map(Number);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  if (month < 1 || month > 12 || day < 1 || day > days[month - 1] || hour > 23 || minute > 59 || second > 60 || Number(match[8] || 0) > 23 || Number(match[9] || 0) > 59) return false;
  if (second < 60) return Number.isFinite(Date.parse(value));
  // RFC3339 leap-second notation is a UTC month-end minute. This validates
  // its representation, not an SLA duration, a future deadline or a policy.
  const preceding = new Date(value.slice(0, 17) + '59' + value.slice(19));
  return Number.isFinite(preceding.getTime()) && preceding.getUTCHours() === 23 && preceding.getUTCMinutes() === 59 &&
    new Date(preceding.getTime() + 1000).getUTCDate() === 1;
}

export function requireAccountClosure(item) {
  const statuses = ['IN_PROGRESS', 'BLOCKED', 'RETRY', 'REPAIR_REQUIRED', 'COMPLETED'];
  const stages = ['CLOSURE_REQUESTED', 'QUIESCING', 'FINANCIAL_CLEARANCE', 'IDENTITY_CLEANUP', 'ANONYMIZED', 'COMPLETED'];
  if (!item || typeof item !== 'object' || !statuses.includes(item.status) || !stages.includes(item.stage) || !Array.isArray(item.blockers) || !validSLA(item.sla_expires_at)) throw unknownClosureACK();
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
