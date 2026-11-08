import { classifyHighRiskFailure, requireGovernedTokenCommandResult } from './admin-high-risk-action-state.js';

// A missing acknowledgement does not prove absence. This lookup never creates
// another command, and never turns a 404 into permission to change the key.
export async function recoverTokenCommandByKey(client, expected, idempotencyKey, signal) {
  if (typeof idempotencyKey !== 'string' || !idempotencyKey) {
    throw Object.assign(new Error('command identity unavailable'), { code: 'COMMAND_RESULT_UNKNOWN', status: 202 });
  }
  try {
    return requireGovernedTokenCommandResult(
      await client.governedTokenCommandByIdempotencyKey(expected.commandType, idempotencyKey, signal), expected,
    );
  } catch (error) {
    throw unknownCommandRecoveryError(error);
  }
}

export function unknownCommandRecoveryError(error, commandReference = '') {
  if (error?.finalResult === true) return error;
  return Object.assign(new Error('command recovery remains unknown'), {
    status: 202, code: 'COMMAND_RESULT_UNKNOWN', requestID: error?.requestID || '',
    commandReference: commandReference || error?.commandReference || '',
  });
}

export function classifyTokenAccountRecoveryFailure(error, recovering = false) {
  return classifyHighRiskFailure({ commandType: 'TOKEN_ACCOUNT_RECOVERY_ACTION' }, recovering ? unknownCommandRecoveryError(error) : error);
}
